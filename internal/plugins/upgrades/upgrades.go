// Package upgrades implements persistent, irreversible attribute upgrades.
//
// # Policy
//
//   - Price: an upgrade from a to b costs the attributes plugin's
//     cost(b) - cost(a) energy and takes max(1, duration(b) - duration(a))
//     ticks (see package attributes).
//   - Payment: the full cost is taken from the unit's own energy store when
//     the upgrade starts. A unit that cannot pay is rejected with
//     "insufficient_energy"; nothing is charged. There is no global pool.
//   - Completion: the new value is applied only when the duration has
//     elapsed. The tick an upgrade starts counts as its first tick, so a
//     1-tick upgrade completes in the tick its command executes.
//   - Irreversibility: upgrades only raise values; there is no downgrade
//     and no cancellation.
//   - Interruption: paid energy is never refunded. If the unit is destroyed
//     while upgrading, the energy is lost with it. If an admin raises the
//     attribute to or above the target before completion, the upgrade
//     fails with reason "superseded" (energy already spent).
//   - Exclusivity: a unit runs at most one upgrade or cloning at a time.
//
// Events (owner only): upgrades.started, upgrades.progress (every
// progress_event_interval ticks), upgrades.completed, upgrades.failed.
package upgrades

import (
	"errors"
	"fmt"

	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "upgrades"

// Identifiers.
const (
	Phase          = "upgrades"
	EventStarted   = "upgrades.started"
	EventProgress  = "upgrades.progress"
	EventCompleted = "upgrades.completed"
	EventFailed    = "upgrades.failed"

	// BusyKind marks units with an upgrade in progress.
	BusyKind = "upgrade"
)

// CommandUpgrade is the UpgradeCommand type.
var CommandUpgrade = string((&upgradesv1.UpgradeCommand{}).ProtoReflect().Descriptor().FullName())

// MaxUnitsPerCommand bounds one command.
const MaxUnitsPerCommand = 4096

// Config is the plugin's configuration schema (the "upgrades" section).
type Config struct {
	// ProgressEventInterval emits upgrades.progress every N ticks of an
	// upgrade (0 disables progress events).
	ProgressEventInterval int64 `json:"progress_event_interval"`
}

type order struct {
	Attr     int
	From, To int64
	Cost     int64
	Duration int64
	Done     int64
}

var orders = sim.NewComponent[order]("upgrades.order")

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg   Config
	attrs *attributes.Plugin
}

// New returns the upgrades plugin.
func New(attrs *attributes.Plugin) *Plugin { return &Plugin{attrs: attrs} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return []string{attributes.Name, energy.Name} }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{ProgressEventInterval: 10}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.ProgressEventInterval < 0 {
		return fmt.Errorf("progress_event_interval must be >= 0")
	}
	names := p.attrs.Names()
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		return []sim.Field{
			{Key: "upgrades.attribute", Value: sim.StringValue(names[o.Attr])},
			{Key: "upgrades.from", Value: sim.IntValue(o.From)},
			{Key: "upgrades.target", Value: sim.IntValue(o.To)},
			{Key: "upgrades.cost", Value: sim.IntValue(o.Cost)},
			{Key: "upgrades.done", Value: sim.IntValue(o.Done)},
			{Key: "upgrades.duration", Value: sim.IntValue(o.Duration)},
		}
	})); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{energy.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "upgrades.progress", Phase: Phase, Run: p.progress}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config": p.cfg,
		"policy": "Full cost paid from the unit's energy when the upgrade starts; value applied on completion; no cancellation or refunds; one upgrade or cloning per unit at a time.",
	}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandUpgrade, Execute: p.execute, ApplyToUnit: p.applyToUnit})
}

type plan struct {
	unit     sim.EntityID
	owner    sim.PlayerID
	attr     int
	from, to int64
	cost     int64
	ticks    int64
}

// validate checks one unit against the request without changing anything.
func (p *Plugin) validate(w *sim.World, issuer sim.Principal, m *upgradesv1.UpgradeCommand, id sim.EntityID) (plan, error) {
	e, ok := w.Entity(id)
	st, hasAttrs := p.attrs.Get(w, id)
	if !ok || e.Kind != sim.KindUnit || !hasAttrs {
		return plan{}, sim.Unavailable("unit", sim.EntityID(id))
	}
	pl := plan{unit: id, owner: e.Owner}
	if !issuer.Admin && e.Owner != issuer.Player {
		// No event to the real owner: the request is not theirs.
		return plan{}, sim.Unavailable("unit", sim.EntityID(id))
	}
	i, ok := p.attrs.Index(m.GetAttribute())
	if !ok {
		return pl, sim.Fail(sim.CodeInvalidArgument, "unknown attribute %q", m.GetAttribute())
	}
	pl.attr, pl.from = i, st.Values[i]
	switch a := m.GetAmount().(type) {
	case *upgradesv1.UpgradeCommand_Target:
		pl.to = a.Target
	case *upgradesv1.UpgradeCommand_Increase:
		if a.Increase <= 0 {
			return pl, sim.Fail(sim.CodeInvalidArgument, "increase must be positive")
		}
		pl.to = pl.from + a.Increase
	default:
		return pl, sim.Fail(sim.CodeInvalidArgument, "target or increase required")
	}
	_, hi := p.attrs.Bounds(i)
	if pl.to <= pl.from {
		return pl, sim.Fail(sim.CodeInvalidArgument, "upgrades are irreversible: %s is %d, target %d is not higher", m.GetAttribute(), pl.from, pl.to)
	}
	if pl.to > hi {
		return pl, sim.Fail(sim.CodeInvalidArgument, "%s cannot exceed %d (target %d)", m.GetAttribute(), hi, pl.to)
	}
	if kind, busy := p.attrs.Busy(w, id); busy {
		return pl, sim.Fail(sim.CodeFailedPrecondition, "unit %d is busy (%s in progress)", id, kind)
	}
	pl.cost, pl.ticks = p.attrs.UpgradeCost(i, pl.from, pl.to)
	if have, _ := energy.Stored(w, id); have < pl.cost {
		return pl, sim.Fail(energy.CodeInsufficientEnergy, "upgrading %s from %d to %d costs %d energy, unit %d has %d", m.GetAttribute(), pl.from, pl.to, pl.cost, id, have)
	}
	return pl, nil
}

func (p *Plugin) start(ctx *sim.TickContext, pl plan) {
	if !energy.Spend(ctx.World, pl.unit, pl.cost) || !p.attrs.Reserve(ctx.World, pl.unit, BusyKind) {
		panic("upgrades: validated upgrade could not start")
	}
	orders.Set(ctx.World, pl.unit, order{Attr: pl.attr, From: pl.from, To: pl.to, Cost: pl.cost, Duration: pl.ticks})
	ctx.Emit(sim.Event{Type: EventStarted, Entity: pl.unit, Audience: sim.ToPlayer(pl.owner), Fields: []sim.Field{
		{Key: "attribute", Value: sim.StringValue(p.attrs.Names()[pl.attr])},
		{Key: "from", Value: sim.IntValue(pl.from)},
		{Key: "to", Value: sim.IntValue(pl.to)},
		{Key: "cost", Value: sim.IntValue(pl.cost)},
		{Key: "duration", Value: sim.IntValue(pl.ticks)},
	}})
}

func (p *Plugin) failed(ctx *sim.TickContext, pl plan, attr string, err error) {
	if pl.owner == 0 {
		return // unknown unit: nobody to tell
	}
	code, msg := sim.CodeInternal, err.Error()
	if ce := (*sim.CommandError)(nil); errors.As(err, &ce) {
		code, msg = ce.Code, ce.Message
	}
	ctx.Emit(sim.Event{Type: EventFailed, Entity: pl.unit, Audience: sim.ToPlayer(pl.owner), Fields: []sim.Field{
		{Key: "attribute", Value: sim.StringValue(attr)},
		{Key: "reason", Value: sim.StringValue(code)},
		{Key: "message", Value: sim.StringValue(msg)},
	}})
}

func (p *Plugin) execute(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*upgradesv1.UpgradeCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want UpgradeCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", MaxUnitsPerCommand, len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	plans := make([]plan, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		pl, err := p.validate(ctx.World, cmd.Issuer, m, sim.EntityID(id))
		if err != nil {
			p.failed(ctx, pl, m.GetAttribute(), err)
			return err
		}
		plans = append(plans, pl)
	}
	if err := ctx.Charge(cmd.Issuer, len(plans)); err != nil {
		return err
	}
	for _, pl := range plans {
		p.start(ctx, pl)
	}
	return nil
}

func (p *Plugin) applyToUnit(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*upgradesv1.UpgradeCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want UpgradeCommand", cmd.Payload)
	}
	pl, err := p.validate(ctx.World, cmd.Issuer, m, unit)
	if err == nil {
		err = ctx.Charge(cmd.Issuer, 1)
	}
	if err != nil {
		p.failed(ctx, pl, m.GetAttribute(), err)
		return err
	}
	p.start(ctx, pl)
	return nil
}

func (p *Plugin) progress(ctx *sim.TickContext) {
	w := ctx.World
	names := p.attrs.Names()
	for _, id := range orders.Entities(w) {
		o, _ := orders.Get(w, id)
		e, _ := w.Entity(id)
		o.Done++
		if o.Done < o.Duration {
			orders.Set(w, id, o)
			if iv := p.cfg.ProgressEventInterval; iv > 0 && o.Done%iv == 0 {
				ctx.Emit(sim.Event{Type: EventProgress, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
					{Key: "attribute", Value: sim.StringValue(names[o.Attr])},
					{Key: "done", Value: sim.IntValue(o.Done)},
					{Key: "duration", Value: sim.IntValue(o.Duration)},
				}})
			}
			continue
		}
		orders.Remove(w, id)
		p.attrs.Release(w, id)
		pl := plan{unit: id, owner: e.Owner, attr: o.Attr}
		if cur, _ := p.attrs.Lookup(w, id, names[o.Attr]); cur >= o.To {
			p.failed(ctx, pl, names[o.Attr], sim.Fail("superseded", "%s is already %d (target %d); energy is not refunded", names[o.Attr], cur, o.To))
			continue
		}
		p.attrs.Apply(w, id, o.Attr, o.To, o.Cost)
		ctx.Emit(sim.Event{Type: EventCompleted, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
			{Key: "attribute", Value: sim.StringValue(names[o.Attr])},
			{Key: "value", Value: sim.IntValue(o.To)},
			{Key: "cost", Value: sim.IntValue(o.Cost)},
		}})
	}
}
