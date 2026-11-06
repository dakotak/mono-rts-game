// Package regeneration is an example Mono plugin: idle units slowly
// regenerate energy (think solar collectors). It exists to demonstrate the
// plugin API end to end and is NOT registered in the default server build;
// docs/developer-guide/plugins/getting-started.md walks through it and
// shows how to register it.
//
// # Rules
//
//   - A unit regenerates on ticks where (tick + id) % interval == 0, so the
//     work is spread evenly over ticks instead of every unit at once.
//   - It gains `amount` energy (or the value of `amount_attribute`, an
//     attributes-plugin property, when configured), but never beyond
//     limit_percent of its capacity.
//   - With idle_only, a unit with a movement order does not regenerate.
//   - Players can switch regeneration off per unit (SetRegenerationCommand).
//
// Regeneration creates energy: unlike mining or transfers it does not
// conserve the world's total. That is a deliberate rule of this example.
package regeneration

import (
	"fmt"

	regenerationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/examples/regeneration/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name. It prefixes every component, system, derived
// property and event the plugin registers, and keys its config section.
const Name = "regeneration"

// Identifiers registered by the plugin.
const (
	Phase          = "regeneration"
	AttrDisabled   = "regeneration.disabled"    // owner-visible, present only when switched off
	DerivedAmount  = "regeneration.amount"      // energy per regeneration step
	EventRegen     = "regeneration.regenerated" // owner; fields amount, stored
	MaxAmount      = 1000
	MaxIntervalTks = 1000
)

// CommandSet is the command type: the payload's protobuf full name.
var CommandSet = string((&regenerationv1.SetRegenerationCommand{}).ProtoReflect().Descriptor().FullName())

// Config is the "regeneration" configuration section.
type Config struct {
	// Enabled turns the mechanic on. Off by default, so registering the
	// plugin changes nothing until a config enables it.
	Enabled bool `json:"enabled"`
	// Amount is the energy gained per regeneration step (0..1000).
	Amount int64 `json:"amount"`
	// AmountAttribute, if set, names an attributes-plugin attribute or
	// derived property whose value replaces Amount per unit (upgradable).
	AmountAttribute string `json:"amount_attribute,omitempty"`
	// Interval is the number of ticks between steps for a unit (1..1000).
	Interval int64 `json:"interval"`
	// LimitPercent caps regeneration at this share of capacity (1..100).
	LimitPercent int64 `json:"limit_percent"`
	// IdleOnly stops regeneration while a unit has a movement order.
	IdleOnly bool `json:"idle_only"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{Amount: 1, Interval: 10, LimitPercent: 50, IdleOnly: true}
}

func (p *Plugin) validate() error {
	c := p.cfg
	switch {
	case c.Amount < 0 || c.Amount > MaxAmount:
		return fmt.Errorf("amount must be in 0..%d, got %d", MaxAmount, c.Amount)
	case c.Interval < 1 || c.Interval > MaxIntervalTks:
		return fmt.Errorf("interval must be in 1..%d, got %d", MaxIntervalTks, c.Interval)
	case c.LimitPercent < 1 || c.LimitPercent > 100:
		return fmt.Errorf("limit_percent must be in 1..100, got %d", c.LimitPercent)
	case c.AmountAttribute != "" && !p.attrs.Has(c.AmountAttribute):
		return fmt.Errorf("amount_attribute: %q is not an attribute or derived property", c.AmountAttribute)
	}
	return nil
}

// optOut marks a unit whose owner switched regeneration off. Components
// must be comparable value types; this one carries no data beyond its
// presence, but a struct leaves room to grow.
type optOut struct{ Since sim.Tick }

var optOuts = sim.NewComponent[optOut]("regeneration.opt_out")

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg   Config
	attrs *attributes.Plugin
}

// New returns the plugin. attrs is the shared attributes plugin instance
// (the one in game.Plugins()); it is needed only for amount_attribute.
func New(attrs *attributes.Plugin) *Plugin { return &Plugin{attrs: attrs} }

// Name implements sim.Plugin.
func (*Plugin) Name() string { return Name }

// DependsOn implements sim.Plugin: these register first, and their Go APIs
// (energy stores, movement orders, attribute lookups) are used here.
func (*Plugin) DependsOn() []string {
	return []string{energy.Name, movement.Name, attributes.Name}
}

// Register implements sim.Plugin. Everything the plugin contributes is
// declared here; nothing happens outside the Registrar.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil { // strict: unknown fields fail
		return err
	}
	if err := p.validate(); err != nil {
		return fmt.Errorf("regeneration: %w", err)
	}
	if err := r.Component(optOuts.Def(sim.VisibleOwner, func(optOut) []sim.Field {
		return []sim.Field{{Key: AttrDisabled, Value: sim.BoolValue(true)}}
	})); err != nil {
		return err
	}
	if err := r.Derived(sim.DerivedDef{Name: DerivedAmount, Compute: p.amount}); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{energy.Phase}, Before: []string{movement.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "regeneration.step", Phase: Phase, Run: p.step}); err != nil {
		return err
	}
	if err := r.Command(sim.CommandDef{Type: CommandSet, Execute: p.execSet, ApplyToUnit: p.applySet}); err != nil {
		return err
	}
	return r.Describe(p.cfg) // published to clients as WorldInfo.rules_json["regeneration"]
}

// amount is the derived property: the energy a unit gains per step.
func (p *Plugin) amount(w *sim.World, id sim.EntityID) (int64, bool) {
	if _, ok := energy.GetStore(w, id); !ok {
		return 0, false // not a unit with a store
	}
	if p.cfg.AmountAttribute != "" {
		v, ok := p.attrs.Lookup(w, id, p.cfg.AmountAttribute)
		return min(max(v, 0), MaxAmount), ok
	}
	return p.cfg.Amount, true
}

// step is the system. It runs once per tick on the simulation goroutine.
func (p *Plugin) step(ctx *sim.TickContext) {
	if !p.cfg.Enabled {
		return
	}
	w := ctx.World
	interval := uint64(p.cfg.Interval)
	// EntityIDs is sorted: iteration order is deterministic. Never range
	// over a Go map to decide outcomes.
	for _, id := range w.EntityIDs() {
		if (uint64(ctx.Tick)+uint64(id))%interval != 0 {
			continue
		}
		e, _ := w.Entity(id)
		if e.Kind != sim.KindUnit || optOuts.Has(w, id) {
			continue
		}
		if _, moving := movement.Destination(w, id); moving && p.cfg.IdleOnly {
			continue
		}
		s, ok := energy.GetStore(w, id)
		if !ok {
			continue
		}
		amt, _ := w.Derived(DerivedAmount, id)
		limit := s.Capacity * p.cfg.LimitPercent / 100 // integer math only
		gain := min(amt, limit-s.Energy)
		if gain <= 0 {
			continue
		}
		energy.SetStored(w, id, s.Energy+gain)
		ctx.Emit(sim.Event{Type: EventRegen, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
			{Key: "amount", Value: sim.IntValue(gain)},
			{Key: "stored", Value: sim.IntValue(s.Energy + gain)},
		}})
	}
}

// check validates one unit for the issuer. Missing and foreign units get
// the same error (sim.Unavailable) so commands cannot probe for hidden
// entities.
func check(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit || (!issuer.Admin && e.Owner != issuer.Player) {
		return sim.Unavailable("unit", id)
	}
	return nil
}

func set(w *sim.World, id sim.EntityID, enabled bool) {
	if enabled {
		optOuts.Remove(w, id)
	} else if !optOuts.Has(w, id) {
		optOuts.Set(w, id, optOut{Since: w.Tick()})
	}
}

// execSet handles SetRegenerationCommand: validate everything, charge the
// budget, then mutate — so a failed command changes nothing.
func (p *Plugin) execSet(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*regenerationv1.SetRegenerationCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetRegenerationCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > energy.MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", energy.MaxUnitsPerCommand, len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		if err := check(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil {
		return err
	}
	for _, id := range ids {
		set(ctx.World, sim.EntityID(id), m.GetEnabled())
	}
	return nil
}

// applySet applies the command template to one unit (batch commands).
func (p *Plugin) applySet(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*regenerationv1.SetRegenerationCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetRegenerationCommand", cmd.Payload)
	}
	if err := check(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	set(ctx.World, unit, m.GetEnabled())
	return nil
}
