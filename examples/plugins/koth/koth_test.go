package koth_test

import (
	"encoding/json"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/examples/plugins/koth"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// withKoth is game.Plugins() plus the example, registered exactly as the
// documentation shows for game.Plugins().
func withKoth() []sim.Plugin {
	plugins := game.Plugins()
	m, _ := game.Find[*match.Plugin](plugins)
	return append(plugins, koth.New(m))
}

func newGame(t *testing.T, kothCfg string) *game.Game {
	t.Helper()
	g, err := game.NewWithPlugins(&game.Config{
		World:   game.WorldConfig{Width: 21, Height: 21, TickRateHz: 10, IDKey: "00112233445566778899aabbccddeeff"},
		Players: []game.PlayerConfig{{Slot: 1, Name: "A", JoinToken: "a"}, {Slot: 2, Name: "B", JoinToken: "b"}},
		Units:   []game.UnitBlock{{Owner: 1, X: 10, Y: 10}, {Owner: 2, X: 1, Y: 1}},
		Plugins: map[string]json.RawMessage{
			"koth":  json.RawMessage(kothCfg),
			"match": json.RawMessage(`{"scoring": {"weights": {"hill_ticks": 2}}}`),
		},
	}, withKoth())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestHillHolderWins(t *testing.T) {
	g := newGame(t, `{"enabled": true, "radius": 1, "hold_ticks": 20}`)
	for range 19 {
		g.Engine.Step()
	}
	if match.Get(g.Engine.World()).State != match.Running {
		t.Fatal("ended early")
	}
	g.Engine.Step()
	st := match.Get(g.Engine.World())
	if st.State != match.Finished || st.Winner != g.Players[0].ID || st.Reason != "king_of_the_hill" || st.Scores[0] != 40 {
		t.Fatalf("status = %+v", st)
	}
}

func TestContestedHillCountsForNobody(t *testing.T) {
	g := newGame(t, `{"enabled": true, "radius": 1, "hold_ticks": 5}`)
	if _, err := g.Engine.World().CreateEntity(sim.KindUnit, g.Players[1].ID, sim.Pos{X: 11, Y: 10}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		g.Engine.Step()
	}
	if match.Get(g.Engine.World()).State != match.Running {
		t.Fatal("a contested hill produced a winner")
	}
}

func TestDisabledByDefaultAndValidated(t *testing.T) {
	g := newGame(t, "")
	for range 400 {
		g.Engine.Step()
	}
	if match.Get(g.Engine.World()).State != match.Running {
		t.Fatal("disabled condition ended the match")
	}
	if _, err := game.NewWithPlugins(&game.Config{
		World:   game.WorldConfig{Width: 10, Height: 10, TickRateHz: 10},
		Plugins: map[string]json.RawMessage{"koth": json.RawMessage(`{"hold_ticks": 0}`)},
	}, withKoth()); err == nil {
		t.Fatal("invalid config accepted")
	}
}
