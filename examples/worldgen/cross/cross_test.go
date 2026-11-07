package cross_test

import (
	"encoding/json"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/examples/worldgen/cross"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/worldgen"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func withCross() []sim.Plugin {
	plugins := game.Plugins()
	wg, _ := game.Find[*worldgen.Plugin](plugins)
	wg.AddGenerator(&cross.Generator{})
	return plugins
}

func build(t *testing.T, seed uint64) *game.Game {
	t.Helper()
	g, err := game.NewWithPlugins(&game.Config{
		World:   game.WorldConfig{Width: 40, Height: 30, Seed: seed, TickRateHz: 10, IDKey: "00112233445566778899aabbccddeeff"},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "a"}, {Slot: 2, JoinToken: "b"}, {Slot: 3, JoinToken: "c"}, {Slot: 4, JoinToken: "d"}},
		Plugins: map[string]json.RawMessage{
			"terrain":  json.RawMessage(`{"types": {"rock": {"passable": false}}}`),
			"worldgen": json.RawMessage(`{"generator": "cross", "params": {"gap": 3}, "spawns": {"layout": "corners", "margin": 4}, "start": {"units": 3}}`),
		},
	}, withCross())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCrossWorld(t *testing.T) {
	g := build(t, 5)
	w := g.Engine.World()
	// The vertical wall blocks the center column except in its passages.
	if id, ok := w.EntityAt(sim.Pos{X: 20, Y: 2}); !ok {
		t.Fatal("no wall on the center column")
	} else if e, _ := w.Entity(id); e.Kind != sim.KindTerrain {
		t.Fatalf("center column holds %v", e.Kind)
	}
	if _, ok := w.EntityAt(sim.Pos{X: 20, Y: 7}); ok {
		t.Fatal("passage at the quarter point is blocked")
	}
	units := 0
	for _, id := range w.EntityIDs() {
		if e, _ := w.Entity(id); e.Kind == sim.KindUnit {
			units++
		}
	}
	if units != 12 {
		t.Fatalf("%d units for 4 players x 3", units)
	}
	// Deterministic: the same seed gives the same world.
	if build(t, 5).Engine.World().StateHash() != w.StateHash() {
		t.Fatal("same seed, different world")
	}
}
