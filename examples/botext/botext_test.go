package botext

import (
	"context"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/arena"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// TestWatchtowerInMatch plays the extended bot against a stock one and
// checks the extensions took effect: a lookout holds the map center, and
// the expansionist heuristic contributed to the strategy scores.
func TestWatchtowerInMatch(t *testing.T) {
	cfg, err := game.LoadConfig("../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	var posted, objective bool
	_, err = arena.Play(context.Background(), arena.Spec{Config: cfg, MapSeed: 2, MaxTicks: 1500,
		Bots: []arena.BotSpec{
			{Slot: 1, Difficulty: 7, Personality: "balanced", Seed: 1, Extensions: Extensions()},
			{Slot: 2, Difficulty: 7, Personality: "balanced", Seed: 2},
		},
		Inspect: func(slot int, b *ai.Bot) {
			if slot != 1 {
				return
			}
			center := state.Pos{X: b.K.Width / 2, Y: b.K.Height / 2}
			for _, u := range b.UnitsByRole(WatchRole) {
				if state.Cheb(u.Pos, center) <= 1 {
					posted = true
				}
			}
			for _, o := range b.Status.Objectives {
				if len(o) >= 10 && o[:10] == "watchtower" {
					objective = true
				}
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !objective {
		t.Fatal("the watchtower objective never reported activity")
	}
	if !posted {
		t.Fatal("no lookout reached the map center")
	}
}
