// Package plugintest provides a test harness over the real, fully wired
// plugin composition (game.Plugins). It is imported only by tests.
package plugintest

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Rules is a representative Milestone 3 attribute configuration.
const Rules = `{
  "base_mass": 10,
  "attributes": {
    "armor":             {"default": 0,   "min": 0,   "max": 20,  "mass": "v * 3", "cost": "v * v * 5 + v * 10", "duration": "v * 4"},
    "propulsion":        {"default": 1,   "min": 1,   "max": 10,  "mass": "v",     "cost": "(v - 1) * (v - 1) * 20", "duration": "(v - 1) * 6"},
    "energy_capacity":   {"default": 100, "min": 100, "max": 400, "mass": "v / 50", "cost": "(v - 100) * (v - 100) / 50 + (v - 100)", "duration": "(v - 100) / 10"},
    "mining_efficiency": {"default": 2,   "min": 1,   "max": 10,  "mass": "v",     "cost": "pow(v, 3)", "duration": "v * 5"}
  },
  "derived": {
    "ticks_per_move": "clamp(mass / (propulsion * 4), 1, 20)"
  }
}`

// Harness drives a game built from game.Plugins with test configuration.
type Harness struct {
	T       testing.TB
	Game    *game.Game
	Engine  *sim.Engine
	Players []sim.PlayerID // index 0 = slot 1
	n       int
}

// New builds a 40x40 world with two players. plugins maps plugin names to
// JSON config sections; "attributes" defaults to Rules and energy binds
// capacity and extraction to attributes unless overridden.
func New(t testing.TB, plugins map[string]string) *Harness {
	t.Helper()
	return NewWithBudget(t, 0, plugins)
}

// NewWithBudget is New with a per-player unit-command budget per tick.
func NewWithBudget(t testing.TB, budget int, plugins map[string]string) *Harness {
	t.Helper()
	cfg := &game.Config{
		World:   game.WorldConfig{Width: 40, Height: 40, Seed: 5, TickRateHz: 10, IDKey: "0123456789abcdef0123456789abcdef", UnitCommandBudget: budget},
		Players: []game.PlayerConfig{{Slot: 1, Name: "A", JoinToken: "a"}, {Slot: 2, Name: "B", JoinToken: "b"}},
		Plugins: map[string]json.RawMessage{
			"attributes": json.RawMessage(Rules),
			"energy":     json.RawMessage(`{"capacity_attribute": "energy_capacity", "extraction_attribute": "mining_efficiency", "initial_energy": 0}`),
		},
	}
	for k, v := range plugins {
		cfg.Plugins[k] = json.RawMessage(v)
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{T: t, Game: g, Engine: g.Engine}
	for _, p := range g.Players {
		h.Players = append(h.Players, p.ID)
	}
	return h
}

// W returns the world.
func (h *Harness) W() *sim.World { return h.Engine.World() }

// Attrs returns the shared attributes plugin.
func (h *Harness) Attrs() *attributes.Plugin {
	for _, p := range game.PluginsOf(h.Game) {
		if a, ok := p.(*attributes.Plugin); ok {
			return a
		}
	}
	h.T.Fatal("no attributes plugin")
	return nil
}

// P returns the player ID for a slot (1-based).
func (h *Harness) P(slot int) sim.PlayerID { return h.Players[slot-1] }

// Unit creates a unit for a player slot and optionally sets its energy.
func (h *Harness) Unit(slot int, x, y int32, energyAmount int64) sim.EntityID {
	h.T.Helper()
	id, err := h.W().CreateEntity(sim.KindUnit, h.P(slot), sim.Pos{X: x, Y: y})
	if err != nil {
		h.T.Fatal(err)
	}
	energy.SetStored(h.W(), id, energyAmount)
	return id
}

// Submit schedules a command for a player slot (0 = admin) and returns its ID.
func (h *Harness) Submit(slot int, typ string, payload any) string {
	h.T.Helper()
	h.n++
	id := fmt.Sprintf("c%d", h.n)
	issuer := sim.Principal{Admin: true}
	if slot > 0 {
		issuer = sim.Principal{Player: h.P(slot)}
	}
	if _, err := h.Engine.Submit(sim.Command{ID: id, Issuer: issuer, Type: typ, Payload: payload}); err != nil {
		h.T.Fatal(err)
	}
	return id
}

// Step executes one tick.
func (h *Harness) Step() *sim.Frame { return h.Engine.Step() }

// Steps executes n ticks and returns their frames.
func (h *Harness) Steps(n int) []*sim.Frame {
	fs := make([]*sim.Frame, 0, n)
	for range n {
		fs = append(fs, h.Engine.Step())
	}
	return fs
}

// Energy returns a unit's stored energy.
func (h *Harness) Energy(id sim.EntityID) int64 {
	v, _ := energy.Stored(h.W(), id)
	return v
}

// Prop returns an attribute or derived property of a unit.
func (h *Harness) Prop(id sim.EntityID, name string) int64 {
	h.T.Helper()
	v, ok := h.Attrs().Lookup(h.W(), id, name)
	if !ok {
		h.T.Fatalf("unit %d has no property %q", id, name)
	}
	return v
}

// Events returns events of a type from frames.
func Events(fs []*sim.Frame, typ string) []sim.Event {
	var out []sim.Event
	for _, f := range fs {
		for _, e := range f.Events {
			if e.Type == typ {
				out = append(out, e)
			}
		}
	}
	return out
}

// Field returns an event field.
func Field(e sim.Event, key string) sim.Value {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return sim.Value{}
}

// Result returns the result for a command ID in a frame.
func Result(f *sim.Frame, id string) (sim.CommandResult, bool) {
	for _, r := range f.Results {
		if r.CommandID == id {
			return r, true
		}
	}
	return sim.CommandResult{}, false
}
