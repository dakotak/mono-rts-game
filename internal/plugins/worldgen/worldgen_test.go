package worldgen_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/worldgen"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const terrainTypes = `{"types": {"rock": {"passable": false}, "marsh": {"passable": true, "cost": 3}}}`

func config(seed uint64, players int, worldgenCfg string) *game.Config {
	c := &game.Config{
		World:   game.WorldConfig{Width: 48, Height: 32, Seed: seed, TickRateHz: 10, IDKey: "00112233445566778899aabbccddeeff"},
		Plugins: map[string]json.RawMessage{"terrain": json.RawMessage(terrainTypes), "worldgen": json.RawMessage(worldgenCfg)},
	}
	for i := 1; i <= players; i++ {
		c.Players = append(c.Players, game.PlayerConfig{Slot: i, Name: fmt.Sprint("P", i), JoinToken: fmt.Sprint("t", i)})
	}
	return c
}

const procedural = `{"generator": "procedural",
  "params": {"obstacle_percent": 12, "rough_type": "marsh", "rough_percent": 6},
  "spawns": {"layout": "random", "min_distance": 14},
  "wells": {"per_spawn": 2, "near_distance": 7, "extra": 4, "well": {"energy": 400, "capacity": 400, "regeneration": 1}},
  "start": {"units": 5, "energy": 40, "min_free_tiles": 40}}`

func build(t *testing.T, c *game.Config) *game.Game {
	t.Helper()
	g, err := game.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func plan(g *game.Game) *worldgen.Plan {
	for _, p := range game.PluginsOf(g) {
		if wg, ok := p.(*worldgen.Plugin); ok {
			return wg.LastPlan()
		}
	}
	return nil
}

func TestSameSeedSameWorld(t *testing.T) {
	a := build(t, config(7, 3, procedural))
	b := build(t, config(7, 3, procedural))
	if a.Engine.World().StateHash() != b.Engine.World().StateHash() {
		t.Fatal("same seed produced different worlds")
	}
	c := build(t, config(8, 3, procedural))
	if a.Engine.World().StateHash() == c.Engine.World().StateHash() {
		t.Fatal("different seeds produced the same world")
	}
}

func TestGeneratedWorldsAreValid(t *testing.T) {
	for seed := range uint64(12) {
		g := build(t, config(seed, 3, procedural))
		w := g.Engine.World()
		pl := plan(g)
		if len(pl.Spawns) != 3 {
			t.Fatalf("seed %d: %d spawns", seed, len(pl.Spawns))
		}
		for i, s := range pl.Spawns {
			for j := range i {
				if d := max(abs(s.X-pl.Spawns[j].X), abs(s.Y-pl.Spawns[j].Y)); d < 14 {
					t.Fatalf("seed %d: spawns %d,%d only %d apart", seed, i, j, d)
				}
			}
		}
		units := map[sim.PlayerID]int{}
		var wellCount int
		for _, id := range w.EntityIDs() {
			e, _ := w.Entity(id)
			switch {
			case e.Kind == sim.KindUnit:
				units[e.Owner]++
				if v, _ := energy.Stored(w, id); v != 40 {
					t.Fatalf("seed %d: starting energy %d", seed, v)
				}
			case e.Kind == sim.KindResource:
				if wl, ok := wells.Get(w, id); ok {
					wellCount++
					if wl.Regeneration != 1 || wl.Capacity != 400 {
						t.Fatalf("well template not applied: %+v", wl)
					}
				}
			}
		}
		for _, p := range g.Players {
			if units[p.ID] != 5 {
				t.Fatalf("seed %d: player %d has %d units", seed, p.Slot, units[p.ID])
			}
		}
		if wellCount < 6 {
			t.Fatalf("seed %d: %d wells (want at least 2 per spawn)", seed, wellCount)
		}
	}
}

func abs(v int32) int32 { return max(v, -v) }

func TestArenaWithWallsAndCorners(t *testing.T) {
	g := build(t, config(1, 2, `{"generator": "arena", "params": {"walls": true},
	  "spawns": {"layout": "corners", "margin": 3}, "wells": {"extra": 4}, "start": {"units": 3}}`))
	w := g.Engine.World()
	pl := plan(g)
	if pl.Spawns[0] != (sim.Pos{X: 3, Y: 3}) || pl.Spawns[1] != (sim.Pos{X: 44, Y: 28}) {
		t.Fatalf("corner spawns = %v", pl.Spawns)
	}
	if id, ok := w.EntityAt(sim.Pos{X: 0, Y: 0}); !ok {
		t.Fatal("no wall at the corner")
	} else if e, _ := w.Entity(id); e.Kind != sim.KindTerrain {
		t.Fatalf("corner holds %v", e.Kind)
	}
	if len(pl.Wells) != 2+4 {
		t.Fatalf("%d wells", len(pl.Wells))
	}
}

func TestGenerationFailsAfterBoundedAttempts(t *testing.T) {
	c := config(1, 4, `{"generator": "procedural", "max_attempts": 3, "spawns": {"layout": "random", "min_distance": 40}}`)
	_, err := game.New(c)
	if err == nil || !strings.Contains(err.Error(), "no valid world after 3 attempts") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown generator": `{"generator": "islands"}`,
		"bad layout":        `{"generator": "arena", "spawns": {"layout": "hex"}}`,
		"bad params":        `{"generator": "procedural", "params": {"obstacles": 3}}`,
		"unknown terrain":   `{"generator": "procedural", "params": {"obstacle_type": "lava"}}`,
		"bad well":          `{"generator": "arena", "wells": {"well": {"energy": 10, "capacity": 5}}}`,
	} {
		_, err := game.New(config(1, 2, cfg))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSpawnsAccessibleAndUnitsPlaced(t *testing.T) {
	// A wall splitting the map would make spawns mutually unreachable;
	// validation must reject such attempts, never return one.
	g := build(t, config(3, 2, `{"generator": "procedural", "params": {"obstacle_percent": 30, "cluster_size": 12},
	  "spawns": {"layout": "ring", "min_distance": 10}, "start": {"units": 6, "min_free_tiles": 60}, "max_attempts": 50}`))
	pl := plan(g)
	for i, us := range pl.Units {
		if len(us) != 6 || us[0] != pl.Spawns[i] {
			t.Fatalf("player %d units %v spawn %v", i, us, pl.Spawns[i])
		}
	}
}
