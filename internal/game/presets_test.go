package game_test

import (
	"encoding/json"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
)

// Every shipped preset loads, validates and generates a valid world for
// many seeds (world generation retries are bounded, so this guards against
// presets that fail for some seeds).
func TestPresetsBuildForManySeeds(t *testing.T) {
	for name, want := range map[string]match.State{"sandbox": match.Running, "standard": match.Ready, "bot-arena": match.Ready} {
		cfg, err := game.LoadConfig("../../configs/presets/" + name + ".json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for seed := range uint64(15) {
			c := *cfg
			c.World.Seed, c.World.RandomSeed = seed, false
			g, err := game.New(&c)
			if err != nil {
				t.Fatalf("%s seed %d: %v", name, seed, err)
			}
			if st := match.Get(g.Engine.World()); st.State != want || int(st.N) != len(cfg.Players) {
				t.Fatalf("%s: state %s with %d players", name, st.State, st.N)
			}
		}
	}
}

func TestImpossibleMatchRulesRejected(t *testing.T) {
	for name, section := range map[string]string{
		"min_players above players": `{"start": "all_ready", "min_players": 5}`,
		"bad start mode":            `{"start": "whenever"}`,
		"negative time limit":       `{"victory": {"time_limit_ticks": -1}}`,
		"unknown metric":            `{"scoring": {"weights": {"gold": 1}}}`,
	} {
		c := testConfig()
		c.Plugins = map[string]json.RawMessage{"match": json.RawMessage(section)}
		if _, err := game.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := testConfig()
	c.Players = nil
	c.Units = nil
	c.Plugins = map[string]json.RawMessage{"match": json.RawMessage(`{"start": "all_ready"}`)}
	if _, err := game.New(c); err == nil {
		t.Error("all_ready without players accepted")
	}
}
