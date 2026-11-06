// Package cloning implements reproduction: a unit produces an offspring
// that inherits its attribute values.
//
// # Costs
//
// Cost and duration are formulas (package expr) over:
//
//	base      the configured base_cost
//	embodied  the parent's embodied upgrade investment (package attributes)
//	mass      the parent's mass
//
// The effective cost is max(cost formula, embodied): an offspring carries
// the parent's upgrades, so cloning can never be cheaper than buying those
// upgrades. The default formula is "base + embodied". Duration is
// max(1, duration formula) ticks.
//
// # Energy accounting
//
// When cloning starts the parent pays cost + offspring_energy from its own
// store (rejected with "insufficient_energy" otherwise). The cost is
// consumed. offspring_energy is held by the cloning order and moved into
// the offspring when it is created; the offspring does not receive the
// energy plugin's free initial_energy. Total energy therefore decreases by
// exactly the cost: cloning cannot create energy. Nothing is refunded if
// the parent is destroyed before completion.
//
// # Placement and conflicts
//
// The offspring appears, when the duration has elapsed, on a free tile
// adjacent to the parent: the requested direction, or (unspecified) the
// first free tile in the order N, NE, E, SE, S, SW, W, NW. Parents
// completing in the same tick are resolved in ascending entity-ID order,
// each taking its tile before the next looks, so simultaneous clones never
// collide. A parent with no free tile keeps waiting (paid, emitting
// cloning.blocked once) and completes on the first tick a tile is free.
// A unit clones or upgrades, never both at once.
package cloning

import (
	"errors"
	"fmt"
	"strconv"

	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/expr"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "cloning"

// Identifiers.
const (
	Phase          = "cloning"
	EventStarted   = "cloning.started"
	EventProgress  = "cloning.progress"
	EventBlocked   = "cloning.blocked"
	EventCompleted = "cloning.completed"
	EventFailed    = "cloning.failed"

	BusyKind = "clone"
)

// CommandClone is the CloneCommand type.
var CommandClone = string((&cloningv1.CloneCommand{}).ProtoReflect().Descriptor().FullName())

// MaxUnitsPerCommand bounds one command.
const MaxUnitsPerCommand = 4096

// Config is the plugin's configuration schema (the "cloning" section).
type Config struct {
	BaseCost              int64  `json:"base_cost"`
	Cost                  string `json:"cost"`
	Duration              string `json:"duration"`
	OffspringEnergy       int64  `json:"offspring_energy"`
	ProgressEventInterval int64  `json:"progress_event_interval"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{BaseCost: 50, Cost: "base + embodied", Duration: "20 + embodied / 20", OffspringEnergy: 10, ProgressEventInterval: 10}
}

// offsets in auto-placement order: N, NE, E, SE, S, SW, W, NW.
var offsets = [8]sim.Pos{{X: 0, Y: -1}, {X: 1, Y: -1}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0}, {X: -1, Y: -1}}

type order struct {
	Cost      int64
	Reserved  int64 // offspring energy held for the child
	Duration  int64
	Done      int64
	Direction int8 // 0 auto, 1..8 = offsets[Direction-1]
	Blocked   bool
	Parent    attributes.Unit // attributes to inherit, captured at start
}

var orders = sim.NewComponent[order]("cloning.order")

// quote is the current price of cloning a unit, kept up to date so clients
// can display it exactly without evaluating formulas themselves.
type quote struct {
	Cost, Ticks, OffspringEnergy int64
}

var quotes = sim.NewComponent[quote]("cloning.quote")

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg      Config
	attrs    *attributes.Plugin
	cost     *expr.Program
	duration *expr.Program
}

// New returns the cloning plugin.
func New(attrs *attributes.Plugin) *Plugin { return &Plugin{attrs: attrs} }

func (*Plugin) Name() string { return Name }

// DependsOn includes upgrades so the cloning phase runs after upgrades.
func (*Plugin) DependsOn() []string {
	return []string{attributes.Name, energy.Name, upgrades.Name}
}

var formulaVars = []string{"base", "embodied", "mass"}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.BaseCost < 0 || p.cfg.OffspringEnergy < 0 || p.cfg.ProgressEventInterval < 0 {
		return errors.New("base_cost, offspring_energy and progress_event_interval must be >= 0")
	}
	for _, f := range []struct {
		field, src string
		dst        **expr.Program
	}{{"cost", p.cfg.Cost, &p.cost}, {"duration", p.cfg.Duration, &p.duration}} {
		e, err := expr.Parse(f.src)
		if err != nil {
			return fmt.Errorf("cloning %s: %w", f.field, err)
		}
		if *f.dst, err = e.CompileVars(formulaVars...); err != nil {
			return fmt.Errorf("cloning %s: %w (available: base, embodied, mass)", f.field, err)
		}
	}
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		return []sim.Field{
			{Key: "cloning.cost", Value: sim.IntValue(o.Cost)},
			{Key: "cloning.offspring_energy", Value: sim.IntValue(o.Reserved)},
			{Key: "cloning.done", Value: sim.IntValue(min(o.Done, o.Duration))},
			{Key: "cloning.duration", Value: sim.IntValue(o.Duration)},
			{Key: "cloning.blocked", Value: sim.BoolValue(o.Blocked)},
		}
	})); err != nil {
		return err
	}
	if err := r.Component(quotes.Def(sim.VisibleOwner, func(q quote) []sim.Field {
		return []sim.Field{
			{Key: "cloning.quote_cost", Value: sim.IntValue(q.Cost)},
			{Key: "cloning.quote_ticks", Value: sim.IntValue(q.Ticks)},
			{Key: "cloning.quote_offspring_energy", Value: sim.IntValue(q.OffspringEnergy)},
		}
	})); err != nil {
		return err
	}
	refresh := func(w *sim.World, id sim.EntityID) {
		if st, ok := p.attrs.Get(w, id); ok {
			c, t := p.Quote(st)
			quotes.Set(w, id, quote{Cost: c, Ticks: t, OffspringEnergy: p.cfg.OffspringEnergy})
		}
	}
	// Registered after attributes, so the unit already has its defaults.
	r.OnCreate(func(w *sim.World, e sim.Entity) { refresh(w, e.ID) })
	p.attrs.OnChange(refresh)
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{upgrades.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "cloning.progress", Phase: Phase, Run: p.progress}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config":         p.cfg,
		"variables":      formulaVars,
		"effective_cost": "max(cost, embodied)",
		"payment":        "parent pays cost + offspring_energy when cloning starts; cost is consumed, offspring_energy moves to the child",
		"placement":      "requested direction, or first free of N, NE, E, SE, S, SW, W, NW; simultaneous completions resolve in ascending parent ID order",
	}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandClone, Execute: p.execute, ApplyToUnit: p.applyToUnit})
}

// Quote returns the cost and duration of cloning a unit with this state.
func (p *Plugin) Quote(u attributes.Unit) (cost, ticks int64) {
	embodied := p.attrs.Embodied(u)
	mass, _ := p.attrs.Property(u, attributes.PropMass)
	env := []int64{p.cfg.BaseCost, embodied, mass}
	return max(p.cost.Eval(env), embodied), max(1, p.duration.Eval(env))
}

type plan struct {
	unit  sim.EntityID
	owner sim.PlayerID
	dir   int8
	cost  int64
	ticks int64
	attrs attributes.Unit
}

// tileFor returns the offspring tile for a parent, if one is free.
func tileFor(w *sim.World, at sim.Pos, dir int8) (sim.Pos, bool) {
	try := func(o sim.Pos) (sim.Pos, bool) {
		t := sim.Pos{X: at.X + o.X, Y: at.Y + o.Y}
		if !w.InBounds(t) {
			return t, false
		}
		_, taken := w.EntityAt(t)
		return t, !taken
	}
	if dir > 0 {
		return try(offsets[dir-1])
	}
	for _, o := range offsets {
		if t, ok := try(o); ok {
			return t, true
		}
	}
	return sim.Pos{}, false
}

func (p *Plugin) validate(w *sim.World, issuer sim.Principal, m *cloningv1.CloneCommand, id sim.EntityID) (plan, error) {
	e, ok := w.Entity(id)
	st, hasAttrs := p.attrs.Get(w, id)
	if !ok || e.Kind != sim.KindUnit || !hasAttrs {
		return plan{}, sim.Unavailable("unit", sim.EntityID(id))
	}
	pl := plan{unit: id, owner: e.Owner, attrs: st}
	if !issuer.Admin && e.Owner != issuer.Player {
		// No event to the real owner: the request is not theirs.
		return plan{}, sim.Unavailable("unit", sim.EntityID(id))
	}
	d := m.GetDirection()
	if d < 0 || d > cloningv1.Direction_DIRECTION_NORTH_WEST {
		return pl, sim.Fail(sim.CodeInvalidArgument, "invalid direction %d", d)
	}
	pl.dir = int8(d)
	if kind, busy := p.attrs.Busy(w, id); busy {
		return pl, sim.Fail(sim.CodeFailedPrecondition, "unit %d is busy (%s in progress)", id, kind)
	}
	if _, free := tileFor(w, e.Pos, pl.dir); !free {
		return pl, sim.Fail(sim.CodeFailedPrecondition, "no free adjacent tile for unit %d's offspring", id)
	}
	pl.cost, pl.ticks = p.Quote(st)
	need := pl.cost + p.cfg.OffspringEnergy
	if have, _ := energy.Stored(w, id); have < need {
		return pl, sim.Fail(energy.CodeInsufficientEnergy, "cloning unit %d needs %d energy (%d cost + %d for the offspring), it has %d", id, need, pl.cost, p.cfg.OffspringEnergy, have)
	}
	return pl, nil
}

func (p *Plugin) start(ctx *sim.TickContext, pl plan) {
	if !energy.Spend(ctx.World, pl.unit, pl.cost+p.cfg.OffspringEnergy) || !p.attrs.Reserve(ctx.World, pl.unit, BusyKind) {
		panic("cloning: validated clone could not start")
	}
	orders.Set(ctx.World, pl.unit, order{Cost: pl.cost, Reserved: p.cfg.OffspringEnergy, Duration: pl.ticks, Direction: pl.dir, Parent: pl.attrs})
	ctx.Emit(sim.Event{Type: EventStarted, Entity: pl.unit, Audience: sim.ToPlayer(pl.owner), Fields: []sim.Field{
		{Key: "cost", Value: sim.IntValue(pl.cost)},
		{Key: "offspring_energy", Value: sim.IntValue(p.cfg.OffspringEnergy)},
		{Key: "duration", Value: sim.IntValue(pl.ticks)},
	}})
}

func (p *Plugin) failed(ctx *sim.TickContext, pl plan, err error) {
	if pl.owner == 0 {
		return
	}
	code, msg := sim.CodeInternal, err.Error()
	if ce := (*sim.CommandError)(nil); errors.As(err, &ce) {
		code, msg = ce.Code, ce.Message
	}
	ctx.Emit(sim.Event{Type: EventFailed, Entity: pl.unit, Audience: sim.ToPlayer(pl.owner), Fields: []sim.Field{
		{Key: "reason", Value: sim.StringValue(code)},
		{Key: "message", Value: sim.StringValue(msg)},
	}})
}

func (p *Plugin) execute(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*cloningv1.CloneCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CloneCommand", cmd.Payload)
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
			p.failed(ctx, pl, err)
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
	m, ok := cmd.Payload.(*cloningv1.CloneCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CloneCommand", cmd.Payload)
	}
	pl, err := p.validate(ctx.World, cmd.Issuer, m, unit)
	if err == nil {
		err = ctx.Charge(cmd.Issuer, 1)
	}
	if err != nil {
		p.failed(ctx, pl, err)
		return err
	}
	p.start(ctx, pl)
	return nil
}

func (p *Plugin) progress(ctx *sim.TickContext) {
	w := ctx.World
	// Ascending ID order makes simultaneous completions deterministic.
	for _, id := range orders.Entities(w) {
		o, _ := orders.Get(w, id)
		parent, _ := w.Entity(id)
		if o.Done < o.Duration {
			o.Done++
		}
		if o.Done < o.Duration {
			orders.Set(w, id, o)
			if iv := p.cfg.ProgressEventInterval; iv > 0 && o.Done%iv == 0 {
				ctx.Emit(sim.Event{Type: EventProgress, Entity: id, Audience: sim.ToPlayer(parent.Owner), Fields: []sim.Field{
					{Key: "done", Value: sim.IntValue(o.Done)},
					{Key: "duration", Value: sim.IntValue(o.Duration)},
				}})
			}
			continue
		}
		tile, free := tileFor(w, parent.Pos, o.Direction)
		if !free {
			if !o.Blocked {
				o.Blocked = true
				ctx.Emit(sim.Event{Type: EventBlocked, Entity: id, Audience: sim.ToPlayer(parent.Owner), Fields: []sim.Field{
					{Key: "reason", Value: sim.StringValue("no_free_tile")},
				}})
			}
			orders.Set(w, id, o)
			continue
		}
		child, err := w.CreateEntity(sim.KindUnit, parent.Owner, tile)
		if err != nil {
			panic(fmt.Sprintf("cloning: free tile rejected: %v", err))
		}
		p.attrs.Inherit(w, child, o.Parent)
		energy.SetStored(w, child, o.Reserved)
		orders.Remove(w, id)
		p.attrs.Release(w, id)
		ctx.Emit(sim.Event{Type: EventCompleted, Entity: id, Audience: sim.ToPlayer(parent.Owner), Fields: []sim.Field{
			{Key: "child", Value: sim.StringValue(strconv.FormatUint(uint64(child), 10))},
			{Key: "position", Value: sim.PosValue(tile)},
			{Key: "cost", Value: sim.IntValue(o.Cost)},
		}})
	}
}
