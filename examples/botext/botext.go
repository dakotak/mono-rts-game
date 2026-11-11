// Package botext is a worked example of extending Mono's computer player
// without modifying it: a custom operational objective and a custom
// strategic heuristic, plugged in through bot.Extensions.
//
//   - Watchtower (objective): claims one unit, walks it to the middle of the
//     map and keeps it there as a lookout, replacing it if it is lost. It
//     uses an extension role ("ext:watch"), so the built-in planners never
//     reassign or command that unit.
//   - Expansionist (heuristic): while most of the map is unexplored and the
//     match is young, it favors the economic_expansion strategy.
//
// cmd (main.go) runs a bot with both, like mono-bot; the test plays it in
// the arena against a stock bot.
package botext

import (
	"fmt"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// WatchRole is the extension role of the lookout.
const WatchRole ai.Role = "ext:watch"

// Watchtower keeps one unit posted at the center of the map.
type Watchtower struct {
	// After is the first tick the watchtower is manned (let the economy
	// start first).
	After uint64
	unit  uint64
}

// Name implements ai.Objective.
func (w *Watchtower) Name() string { return "watchtower" }

// Plan implements ai.Objective: it runs every operational cycle.
func (w *Watchtower) Plan(b *ai.Bot) (bool, string) {
	if b.Tick() < w.After {
		return false, ""
	}
	k := b.K
	center := state.Pos{X: k.Width / 2, Y: k.Height / 2}
	u := k.Own[w.unit]
	if u == nil {
		// Claim the fighter with the most energy (it must walk there).
		var best *state.Entity
		for _, f := range b.UnitsByRole(ai.RoleFighter) {
			if best == nil || f.IntOr("energy.stored", 0) > best.IntOr("energy.stored", 0) {
				best = f
			}
		}
		if best == nil {
			return false, "no fighter to post"
		}
		w.unit, u = best.ID, best
		b.SetRole(u, WatchRole)
	}
	if state.Cheb(u.Pos, center) > 1 {
		b.Issue(ai.Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: ai.PrioRoles, Desc: "watchtower post",
			Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: &rtsv1.Position{X: center.X, Y: center.Y}}})
		return true, "manning " + center.String()
	}
	return true, "watching " + center.String()
}

// Expansionist favors economic expansion early on a mostly unknown map.
var Expansionist = ai.HeuristicFunc{N: "expansionist", F: func(f *ai.Features, strategy string) int {
	if strategy == "economic_expansion" && f.ExploredPm < 300 && f.Tick < 2000 {
		return 200
	}
	return 0
}}

// Extensions returns the example's extensions (a fresh objective per bot:
// objectives keep state).
func Extensions() bot.Extensions {
	return bot.Extensions{Heuristics: []ai.Heuristic{Expansionist}, Objectives: []ai.Objective{&Watchtower{After: 300}}}
}
