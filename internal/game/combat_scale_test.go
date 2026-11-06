package game_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	salvagev1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/salvage/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const battlePairs = 2000

// battle builds 2,000 red/blue pairs on a 200x200 grid. Even pairs fight a
// mutual duel; odd pairs are one-sided (red attacks, blue does not). Units
// carry different amounts of energy so wrecks differ.
func battle(t testing.TB) (*game.Game, [][2]sim.EntityID) {
	t.Helper()
	cfg := &game.Config{
		World:   game.WorldConfig{Width: 200, Height: 200, Seed: 11, TickRateHz: 10, IDKey: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "r"}, {Slot: 2, JoinToken: "b"}},
		Plugins: map[string]json.RawMessage{
			"energy":  json.RawMessage(`{"unit_capacity": 1000, "initial_energy": 0}`),
			"combat":  json.RawMessage(`{"attack": {"default": 10}, "max_health": {"default": 100}, "cooldown": {"default": 2}, "energy_cost": "3"}`),
			"salvage": json.RawMessage(`{"stored_recovery_permille": 700, "salvage_rate": {"default": 9}}`),
		},
	}
	for i := range battlePairs {
		x, y := int32(i%200), int32(i/200)*4
		cfg.Units = append(cfg.Units, game.UnitBlock{Owner: 1, X: x, Y: y}, game.UnitBlock{Owner: 2, X: x, Y: y + 1})
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := g.Engine.World()
	var pairs [][2]sim.EntityID
	for i := range battlePairs {
		x, y := int32(i%200), int32(i/200)*4
		r, _ := w.EntityAt(sim.Pos{X: x, Y: y})
		b, _ := w.EntityAt(sim.Pos{X: x, Y: y + 1})
		pairs = append(pairs, [2]sim.EntityID{r, b})
	}
	// Fund units deterministically (direct setup before tick 1).
	for i, pr := range pairs {
		for k, u := range pr {
			setEnergyDirect(g, u, int64(60+(i*7+k*13)%140))
		}
	}
	return g, pairs
}

func setEnergyDirect(g *game.Game, u sim.EntityID, v int64) {
	// Same path the plugins use; this is setup, not simulation.
	energySet(g.Engine.World(), u, v)
}

// runBattle drives the scenario (or replays a log) and returns per-tick
// hashes and the command log.
func runBattle(t testing.TB, log [][]sim.Command) ([]uint64, [][]sim.Command, *game.Game, [][2]sim.EntityID) {
	g, pairs := battle(t)
	e := g.Engine
	var hashes []uint64
	if log != nil {
		for _, cmds := range log {
			e.StepWith(cmds)
			hashes = append(hashes, e.World().StateHash())
		}
		return hashes, log, g, pairs
	}
	red, blue := g.Players[0].ID, g.Players[1].ID
	submit := func(p sim.Principal, id, typ string, payload any) {
		if _, err := e.Submit(sim.Command{ID: id, Issuer: p, Type: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	for i, pr := range pairs {
		if i%2 == 0 {
			submit(sim.Principal{Player: blue}, fmt.Sprint("b", i), combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(pr[1])}, TargetId: uint64(pr[0])})
		}
		submit(sim.Principal{Player: red}, fmt.Sprint("r", i), combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(pr[0])}, TargetId: uint64(pr[1])})
	}
	var out [][]sim.Command
	step := func() {
		f := e.Step()
		out = append(out, f.Commands)
		hashes = append(hashes, e.World().StateHash())
	}
	for tick := range 30 {
		if tick == 5 {
			// Administrative mutations mid-battle: heal one unit, finish
			// another, drop a wreck. All are scheduled and replayed.
			admin := sim.Principal{Admin: true}
			submit(admin, "heal", combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(pairs[2][0]), Health: 100})
			submit(admin, "kill", combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(pairs[3][1]), Health: 0})
			submit(admin, "wreck", salvage.CommandCreate, &salvagev1.CreateWreckCommand{Position: &rtsv1.Position{X: 3, Y: 2}, Energy: 33})
		}
		if tick == 22 {
			// Survivors salvage the wreck below them (on their blue partner's
			// tile), if any.
			wrecks := map[sim.Pos]sim.EntityID{}
			for _, id := range e.World().EntityIDs() {
				if _, ok := salvage.GetWreck(e.World(), id); ok {
					ent, _ := e.World().Entity(id)
					wrecks[ent.Pos] = id
				}
			}
			for i, pr := range pairs {
				r, ok := e.World().Entity(pr[0])
				if !ok {
					continue
				}
				if wk, ok := wrecks[sim.Pos{X: r.Pos.X, Y: r.Pos.Y + 1}]; ok {
					submit(sim.Principal{Player: red}, fmt.Sprint("s", i), salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(pr[0])}, WreckId: uint64(wk)})
				}
			}
		}
		step()
	}
	return hashes, out, g, pairs
}

func TestLargeScaleCombat(t *testing.T) {
	start := time.Now()
	h1, log, g, pairs := runBattle(t, nil)
	elapsed := time.Since(start)
	w := g.Engine.World()

	// Outcome: every blue unit died (attackers outnumber or match them, and
	// each red fires every 2 ticks for 10 damage: 20 ticks to kill). In
	// mutual duels both die simultaneously; in one-sided pairs red lives.
	redAlive, blueAlive := 0, 0
	for i, pr := range pairs {
		_, r := w.Entity(pr[0])
		_, b := w.Entity(pr[1])
		if b {
			blueAlive++
		}
		if r {
			redAlive++
			if i%2 == 0 && i != 2 {
				t.Fatalf("pair %d: red survived a mutual duel", i)
			}
		}
	}
	if blueAlive != 0 || redAlive != battlePairs/2+1 { // + the healed red in pair 2
		t.Fatalf("red alive %d, blue alive %d", redAlive, blueAlive)
	}
	// Invariants: integrity within bounds, at most one blocking entity per
	// tile, wrecks never above their initial energy.
	seen := map[sim.Pos]bool{}
	for _, id := range w.EntityIDs() {
		e, _ := w.Entity(id)
		if hh, ok := combat.GetHealth(w, id); ok && (hh.HP <= 0 || hh.HP > hh.Max) {
			t.Fatalf("unit %d health %+v", id, hh)
		}
		if wk, ok := salvage.GetWreck(w, id); ok && (wk.Energy <= 0 || wk.Energy > wk.Initial) {
			t.Fatalf("wreck %d = %+v", id, wk)
		}
		if !e.Passable {
			if seen[e.Pos] {
				t.Fatalf("two blocking entities at %v", e.Pos)
			}
			seen[e.Pos] = true
		}
	}
	t.Logf("%d units, 30 ticks of combat and salvage: %v", 2*battlePairs, elapsed.Round(time.Millisecond))

	// Determinism and replay equivalence.
	h2, _, _, _ := runBattle(t, nil)
	h3, _, _, _ := runBattle(t, log)
	for i := range h1 {
		if h1[i] != h2[i] || h1[i] != h3[i] {
			t.Fatalf("diverged at tick %d (rerun %v, replay %v)", i+1, h1[i] == h2[i], h1[i] == h3[i])
		}
	}
}

func TestSalvageConservesEnergyAtScale(t *testing.T) {
	_, _, g, _ := runBattle(t, nil)
	e := g.Engine
	total := func() int64 {
		var sum int64
		for _, id := range e.World().EntityIDs() {
			sum += energyOf(e.World(), id)
			if wk, ok := salvage.GetWreck(e.World(), id); ok {
				sum += wk.Energy
			}
		}
		return sum
	}
	// Combat is over (no living blues): only salvage moves energy now.
	before := total()
	for range 20 {
		e.Step()
		if got := total(); got != before {
			t.Fatalf("energy changed during salvage: %d -> %d", before, got)
		}
	}
}
