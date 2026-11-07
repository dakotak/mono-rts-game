// Package koth is an example "king of the hill" plugin. It shows the two
// match extension points:
//
//   - a victory condition (match.Condition): the first player to control
//     the hill for hold_ticks running ticks wins;
//   - a scoring metric (match.Metric) "hill_ticks": ticks of control, which
//     the match configuration can weight (match.scoring.weights).
//
// A player controls the hill in a tick when they are the only player with
// units within radius (Chebyshev) of the map center. Control time is
// cumulative and stored in a world global, so it is hashed and replayed.
//
// It is not part of the default server build. Register it in
// game.Plugins() — see docs/developer-guide/plugins/match-extensions.md.
package koth

import (
	"fmt"

	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "koth"

// Config is the "koth" section.
type Config struct {
	Enabled   bool  `json:"enabled"`
	Radius    int32 `json:"radius"`
	HoldTicks int64 `json:"hold_ticks"`
}

// Hill is the plugin's world-scoped state.
type Hill struct {
	Holder sim.PlayerID                   // controlling player this tick (0 = contested or empty)
	Ticks  [match.MaxPlayers]int64        // control ticks per player, by match player index
	Owners [match.MaxPlayers]sim.PlayerID // the player at each index (for lookups)
}

var hill = sim.NewGlobal(Name+".hill", Hill{})

// Plugin implements sim.Plugin, match.Condition and the hill_ticks metric.
type Plugin struct {
	cfg Config
}

// New creates the plugin and registers its condition and metric with the
// match plugin. Call it in game.Plugins(), before the ruleset is built.
func New(m *match.Plugin) *Plugin {
	p := &Plugin{}
	m.AddCondition(p)
	m.AddMetric(match.NewMetric("hill_ticks", true, p.hillTicks))
	return p
}

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return []string{match.Name} }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{Radius: 2, HoldTicks: 300}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.Radius < 0 || p.cfg.HoldTicks < 1 {
		return fmt.Errorf("koth: radius must be >= 0 and hold_ticks >= 1")
	}
	if err := r.Global(hill.Def()); err != nil {
		return err
	}
	// Count control before the match evaluates its victory conditions.
	if err := r.Phase(sim.PhaseDef{Name: Name, Before: []string{match.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "koth.control", Phase: Name, Run: p.control}); err != nil {
		return err
	}
	return r.Describe(p.cfg)
}

// control runs every running tick (the match gate stops systems
// otherwise).
func (p *Plugin) control(ctx *sim.TickContext) {
	if !p.cfg.Enabled {
		return
	}
	w := ctx.World
	st := match.Get(w)
	center := sim.Pos{X: w.Width() / 2, Y: w.Height() / 2}
	present := sim.PlayerID(0)
	contested := false
	for y := center.Y - p.cfg.Radius; y <= center.Y+p.cfg.Radius; y++ {
		for x := center.X - p.cfg.Radius; x <= center.X+p.cfg.Radius; x++ {
			id, ok := w.EntityAt(sim.Pos{X: x, Y: y})
			if !ok {
				continue
			}
			if e, _ := w.Entity(id); e.Kind == sim.KindUnit && e.Owner != 0 {
				if present != 0 && present != e.Owner {
					contested = true
				}
				present = e.Owner
			}
		}
	}
	h := hill.Get(w)
	h.Holder = 0
	copy(h.Owners[:], st.Players[:])
	if i := st.Index(present); i >= 0 && !contested {
		h.Holder = present
		h.Ticks[i]++
	}
	hill.Set(w, h)
}

// Check implements match.Condition.
func (p *Plugin) Check(v *match.View) *match.Outcome {
	if !p.cfg.Enabled {
		return nil
	}
	h := hill.Get(v.World)
	for i := range int(v.Status.N) {
		if h.Ticks[i] >= p.cfg.HoldTicks {
			return &match.Outcome{Winner: v.Status.Players[i], Reason: "king_of_the_hill"}
		}
	}
	return nil
}

func (p *Plugin) hillTicks(v *match.View, pl sim.PlayerID) int64 {
	if i := v.Status.Index(pl); i >= 0 {
		return hill.Get(v.World).Ticks[i]
	}
	return 0
}
