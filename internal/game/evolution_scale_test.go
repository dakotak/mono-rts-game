package game_test

import (
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/metadata"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// scaleWorld places `units` units for two players on every other column of
// a 200x200 grid (so each has free neighbours for offspring) with plenty of
// energy.
func scaleWorld(t testing.TB, units int) (*game.Game, []sim.EntityID) {
	t.Helper()
	cfg := &game.Config{
		World:   game.WorldConfig{Width: 200, Height: 200, Seed: 3, TickRateHz: 10, IDKey: "00112233445566778899aabbccddeeff"},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "a"}, {Slot: 2, JoinToken: "b"}},
		Plugins: map[string]json.RawMessage{
			"attributes": json.RawMessage(plugintest.Rules),
			"energy":     json.RawMessage(`{"capacity_attribute": "energy_capacity", "extraction_attribute": "mining_efficiency", "initial_energy": 100}`),
			"cloning":    json.RawMessage(`{"base_cost": 20, "cost": "base + embodied", "duration": "15", "offspring_energy": 5}`),
			"upgrades":   json.RawMessage(`{"progress_event_interval": 0}`),
		},
	}
	for i := range units {
		cfg.Units = append(cfg.Units, game.UnitBlock{Owner: 1 + i%2, X: int32(i%100) * 2, Y: int32(i / 100 * 2)})
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, g.Engine.World().EntityIDs()
}

// runEvolution has half the units upgrade (explicit commands), tags the
// other half by metadata and clones them through batch selectors, then runs
// until everything has finished. It returns per-tick hashes and the log.
func runEvolution(t testing.TB, units int, log [][]sim.Command) ([]uint64, [][]sim.Command, *game.Game) {
	g, ids := scaleWorld(t, units)
	e := g.Engine
	var hashes []uint64
	var out [][]sim.Command
	step := func() {
		f := e.Step()
		out = append(out, f.Commands)
		hashes = append(hashes, e.World().StateHash())
	}
	if log != nil {
		for _, cmds := range log {
			e.StepWith(cmds)
			hashes = append(hashes, e.World().StateHash())
		}
		return hashes, log, g
	}
	byOwner := map[sim.PlayerID][]uint64{}
	for _, id := range ids {
		ent, _ := e.World().Entity(id)
		byOwner[ent.Owner] = append(byOwner[ent.Owner], uint64(id))
	}
	submit := func(p sim.PlayerID, typ string, payload any) {
		if _, err := e.Submit(sim.Command{ID: typ, Issuer: sim.Principal{Player: p}, Type: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	for _, pl := range g.Players {
		mine := byOwner[pl.ID]
		half := len(mine) / 2
		submit(pl.ID, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: mine[:half], Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 2}})
		submit(pl.ID, metadata.CommandSet, &metadatav1.SetMetadataCommand{UnitIds: mine[half:], Key: "role", Value: "breeder"})
		tmpl, _ := anypb.New(&cloningv1.CloneCommand{})
		submit(pl.ID, metadata.CommandBatch, &metadatav1.BatchCommand{
			Selector: &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{{Key: "role", Value: "breeder", Match: metadatav1.MatchKind_MATCH_KIND_EQUALS}}},
			Command:  tmpl,
		})
	}
	for range 20 {
		step()
	}
	return hashes, out, g
}

func TestThousandsUpgradingAndCloningConcurrently(t *testing.T) {
	const units = 4000
	start := time.Now()
	h1, log, g := runEvolution(t, units, nil)
	elapsed := time.Since(start)
	w := g.Engine.World()

	// Everyone finished: 2000 upgraded to armor 2, 2000 offspring created.
	if got := w.EntityCount(); got != units+units/2 {
		t.Fatalf("entities = %d, want %d", got, units+units/2)
	}
	attrs := attrsOf(g)
	upgraded, busy := 0, 0
	seen := map[sim.Pos]bool{}
	for _, id := range w.EntityIDs() {
		ent, _ := w.Entity(id)
		if seen[ent.Pos] {
			t.Fatalf("two entities at %v", ent.Pos)
		}
		seen[ent.Pos] = true
		if v, _ := attrs.Lookup(w, id, "armor"); v == 2 {
			upgraded++
		}
		if _, b := attrs.Busy(w, id); b {
			busy++
		}
		if s, _ := energy.GetStore(w, id); s.Energy < 0 || s.Energy > s.Capacity {
			t.Fatalf("store out of range: %+v", s)
		}
	}
	if upgraded != units/2 || busy != 0 {
		t.Fatalf("upgraded %d busy %d", upgraded, busy)
	}
	t.Logf("%d units: %d upgrades + %d clones in 20 ticks, %v total", units, units/2, units/2, elapsed.Round(time.Millisecond))

	// Deterministic, and the recorded log (including the batch selectors)
	// replays to identical state.
	h2, _, _ := runEvolution(t, units, nil)
	h3, _, _ := runEvolution(t, units, log)
	for i := range h1 {
		if h1[i] != h2[i] || h1[i] != h3[i] {
			t.Fatalf("diverged at tick %d (rerun %v, replay %v)", i+1, h1[i] == h2[i], h1[i] == h3[i])
		}
	}
}

func attrsOf(g *game.Game) *attributes.Plugin {
	for _, p := range game.PluginsOf(g) {
		if a, ok := p.(*attributes.Plugin); ok {
			return a
		}
	}
	return nil
}

// BenchmarkEvolutionTick measures ticks while thousands of upgrades and
// clones are in progress.
func BenchmarkEvolutionTick(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		g, ids := scaleWorld(b, 4000)
		e := g.Engine
		for _, pl := range g.Players {
			var mine []uint64
			for _, id := range ids {
				if ent, _ := e.World().Entity(id); ent.Owner == pl.ID {
					mine = append(mine, uint64(id))
				}
			}
			half := len(mine) / 2
			_, _ = e.Submit(sim.Command{Issuer: sim.Principal{Player: pl.ID}, Type: upgrades.CommandUpgrade, Payload: &upgradesv1.UpgradeCommand{UnitIds: mine[:half], Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 2}}})
			_, _ = e.Submit(sim.Command{Issuer: sim.Principal{Player: pl.ID}, Type: cloning.CommandClone, Payload: &cloningv1.CloneCommand{UnitIds: mine[half:]}})
		}
		b.StartTimer()
		for range 20 {
			e.Step()
		}
	}
}
