package game_test

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"testing"
	"time"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// fogWorld: 4,000 units of two players interleaved on a 120x120 grid with
// vision 5, so every unit's vision overlaps dozens of friends and enemies.
func fogWorld(t testing.TB) *game.Game {
	t.Helper()
	cfg := &game.Config{
		World:   game.WorldConfig{Width: 120, Height: 120, Seed: 21, TickRateHz: 10, IDKey: "aabbccddeeff00112233445566778899"},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "a"}, {Slot: 2, JoinToken: "b"}},
		Plugins: map[string]json.RawMessage{
			"visibility": json.RawMessage(`{
				"enabled": true, "vision": {"default": 5}, "sensor": {"default": 6}, "identification": {"default": 3},
				"detailed_threshold": 2,
				"disclosure": [{"key": "combat.health", "basic": 50, "estimated": 10, "detailed": 1}, {"key": "energy.stored", "estimated": 25}]
			}`),
		},
	}
	r := rand.New(rand.NewPCG(4, 4))
	taken := map[[2]int32]bool{}
	for i := 0; i < 4000; {
		x, y := int32(r.IntN(120)), int32(r.IntN(120))
		if taken[[2]int32{x, y}] {
			continue
		}
		taken[[2]int32{x, y}] = true
		cfg.Units = append(cfg.Units, game.UnitBlock{Owner: 1 + i%2, X: x, Y: y})
		i++
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// hashFrame covers everything players can observe: observations and
// events, in order.
func hashFrame(f *sim.Frame) uint64 {
	h := fnv.New64a()
	for _, o := range f.Observations {
		fmt.Fprintf(h, "p%d e%v x%v l%v|", o.Player, o.TilesEntered, o.TilesExited, o.Lost)
		for _, u := range o.Upserts {
			fmt.Fprintf(h, "u%d:%d:%v:%v|", u.View.ID, u.Level, u.View.Pos, u.View.Attrs)
		}
	}
	for _, e := range f.Events {
		fmt.Fprintf(h, "%s:%d:%v:%v|", e.Type, e.Entity, e.Audience, e.Fields)
	}
	return h.Sum64()
}

func runFog(t testing.TB, ticks int, log [][]sim.Command) (states, observations []uint64, out [][]sim.Command, perTick time.Duration) {
	g := fogWorld(t)
	e := g.Engine
	ids := e.World().EntityIDs()
	r := rand.New(rand.NewPCG(8, 8))
	start := time.Now()
	for tick := range ticks {
		var f *sim.Frame
		if log != nil {
			f = e.StepWith(log[tick])
		} else {
			// Each tick ~200 random units get new destinations.
			for range 200 {
				id := ids[r.IntN(len(ids))]
				ent, _ := e.World().Entity(id)
				_, _ = e.Submit(sim.Command{ID: "m", Issuer: sim.Principal{Player: ent.Owner}, Type: movement.CommandMove,
					Payload: &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(id), Destination: &rtsv1.Position{X: int32(r.IntN(120)), Y: int32(r.IntN(120))}}}}})
			}
			f = e.Step()
			out = append(out, f.Commands)
		}
		states = append(states, e.World().StateHash())
		observations = append(observations, hashFrame(f))
	}
	return states, observations, out, time.Since(start) / time.Duration(ticks)
}

func TestFogDeterminismAndReplayAtScale(t *testing.T) {
	const ticks = 40
	s1, o1, log, perTick := runFog(t, ticks, nil)
	s2, o2, _, _ := runFog(t, ticks, nil)
	s3, o3, _, _ := runFog(t, ticks, log)
	for i := range s1 {
		if s1[i] != s2[i] || o1[i] != o2[i] {
			t.Fatalf("tick %d differs between runs (state %v, observations %v)", i+1, s1[i] == s2[i], o1[i] == o2[i])
		}
		if s1[i] != s3[i] || o1[i] != o3[i] {
			t.Fatalf("tick %d differs on replay (state %v, observations %v)", i+1, s1[i] == s3[i], o1[i] == o3[i])
		}
	}
	t.Logf("4000 units, vision 5, 200 reorders/tick: %v per tick (including movement and observation)", perTick.Round(10*time.Microsecond))
}

func TestFogObservationInvariantsAtScale(t *testing.T) {
	g := fogWorld(t)
	e := g.Engine
	f := e.Step()
	w := e.World()
	for _, o := range f.Observations {
		for _, u := range o.Upserts {
			ent, ok := w.Entity(u.View.ID)
			if !ok || ent.Owner == o.Player {
				t.Fatalf("player %d observed own or missing entity %d", o.Player, u.View.ID)
			}
			if ent.Pos != u.View.Pos {
				t.Fatal("observed position differs from the real one")
			}
			for _, a := range u.View.Attrs {
				if a.Key != "combat.health" && a.Key != "energy.stored" {
					t.Fatalf("undisclosed attribute %s", a.Key)
				}
			}
		}
	}
	if len(f.Observations) != 2 {
		t.Fatalf("observations for %d players", len(f.Observations))
	}
}

func BenchmarkFogTick(b *testing.B) {
	g := fogWorld(b)
	e := g.Engine
	e.Step()
	for b.Loop() {
		e.Step()
	}
}
