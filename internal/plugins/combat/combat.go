// Package combat implements attacks, simultaneous damage resolution and
// unit destruction.
//
// # Stats
//
// Every unit has the same combat stats; specialization comes from
// attributes and upgrades, never from classes. Each stat is either bound to
// an attribute or derived property of the attributes plugin, or a constant:
//
//	max_health  structural integrity when undamaged (>= 1)
//	attack      attack strength (an attacker needs attack > 0)
//	range       maximum Chebyshev distance to the target (>= 0)
//	cooldown    ticks between attacks (>= 1)
//
// Two formulas (package expr) complete the rules:
//
//	damage       variables attacker_<p>, target_<p>, distance
//	energy_cost  variables attacker_<p>
//
// where <p> is a combat stat (attack, range, cooldown, max_health, health,
// energy) or any property of the attributes plugin (armor, mass, ...).
// Negative results count as 0. Attacks hit instantly: there are no
// projectiles and no attack duration.
//
// # Resolution (phase "combat": after movement, before upgrades and cloning)
//
//  1. Eligibility: every attack order is examined against the state at the
//     start of the phase.
//  2. Validation: the target must still exist as a unit, be within range;
//     the attacker's cooldown must have elapsed, its attack be positive,
//     and its stored energy cover the energy cost.
//  3. Energy: the cost of each eligible attack is computed and recorded.
//  4. Damage: each attack's damage is computed from the same start state.
//  5. Aggregation: damage is summed per target.
//  6. Application: attackers pay their energy and start their cooldowns,
//     then every target loses its total damage at once (floored at 0).
//  7. Destruction: every unit at 0 integrity is destroyed.
//
// Steps 1–5 only read the start state, and every write in step 6 is a sum
// or an independent per-unit update, so the outcome does not depend on the
// order attacks are visited in. In particular, a unit destroyed this tick
// still fires its own attack this tick (mutual destruction is possible).
//
// # Order lifecycle
//
// An order persists until its target no longer exists (combat.attack_ended
// "target_destroyed" or "target_lost"), the attacker's attack drops to 0
// ("incapable"), it is replaced by a new order, or cancelled
// (StopAttackCommand). Out of range: the order waits without moving the
// unit (combat.out_of_range, emitted once per episode). Not enough energy:
// it waits (combat.stalled, once per episode).
//
// # Destruction
//
// A destroyed unit is removed from the world, which removes every
// component and therefore cancels all of its orders and activities
// (movement, mining, transfers, upgrades, cloning, attacks); energy paid
// into an unfinished upgrade or clone is lost. Its owner and the owners of
// units that hit it in the destroying tick receive combat.destroyed.
// Plugins observing destruction (salvage) get a snapshot first
// (OnDestroyed). Orders of other units that pointed at it end the next time
// they are evaluated.
package combat

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/expr"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "combat"

// Identifiers.
const (
	Phase = "combat"

	EventAttack     = "combat.attack"       // attacker's owner: target, damage, cost
	EventDamaged    = "combat.damaged"      // target's owner: damage, health, attackers
	EventDestroyed  = "combat.destroyed"    // owner + attacking owners: position, reason
	EventEnded      = "combat.attack_ended" // attacker's owner: target, reason
	EventOutOfRange = "combat.out_of_range" // attacker's owner: target, distance, range
	EventStalled    = "combat.stalled"      // attacker's owner: required, stored
)

// Command types.
var (
	CommandAttack    = string((&combatv1.AttackCommand{}).ProtoReflect().Descriptor().FullName())
	CommandStop      = string((&combatv1.StopAttackCommand{}).ProtoReflect().Descriptor().FullName())
	CommandSetHealth = string((&combatv1.SetHealthCommand{}).ProtoReflect().Descriptor().FullName())
)

// MaxUnitsPerCommand bounds one command.
const MaxUnitsPerCommand = 4096

// Stat binds a combat stat to an attributes property, or a constant.
type Stat struct {
	Property string `json:"property,omitempty"`
	Default  int64  `json:"default"`
}

// Config is the plugin's configuration schema (the "combat" section).
type Config struct {
	MaxHealth  Stat   `json:"max_health"`
	Attack     Stat   `json:"attack"`
	Range      Stat   `json:"range"`
	Cooldown   Stat   `json:"cooldown"`
	Damage     string `json:"damage"`
	EnergyCost string `json:"energy_cost"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		MaxHealth:  Stat{Default: 100},
		Attack:     Stat{Default: 10},
		Range:      Stat{Default: 1},
		Cooldown:   Stat{Default: 5},
		Damage:     "attacker_attack",
		EnergyCost: "0",
	}
}

// Built-in per-unit variables available to formulas (prefixed attacker_ or
// target_). Anything else resolves against the attributes plugin.
var builtins = []string{"attack", "range", "cooldown", "max_health", "health", "energy"}

// Health is a unit's structural integrity.
type Health struct {
	HP, Max int64
}

type order struct {
	Target     sim.EntityID
	OutOfRange bool
	Stalled    bool
}

// stats is the owner-visible summary of a unit's combat capability, kept
// current so clients can display it without evaluating formulas.
type stats struct {
	Attack, Range, Cooldown, Cost int64
}

var (
	healths  = sim.NewComponent[Health]("combat.health")
	orders   = sim.NewComponent[order]("combat.order")
	readyAt  = sim.NewComponent[sim.Tick]("combat.ready_at")
	statsCmp = sim.NewComponent[stats]("combat.stats")
)

// Destroyed describes a unit at the moment it was destroyed.
type Destroyed struct {
	Unit sim.Entity
	// Stored is the energy that was in its store.
	Stored int64
	// Embodied and Invested come from the attributes plugin (0 without it).
	Embodied, Invested int64
	// Audience is the players told about the destruction (the owner and
	// the owners of units that hit it in the destroying tick), sorted.
	Audience []sim.PlayerID
}

// varRef is one formula variable: side 0 attacker, 1 target, 2 distance.
type varRef struct {
	side int
	name string
}

type formula struct {
	prog *expr.Program
	vars []varRef
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg    Config
	attrs  *attributes.Plugin
	damage formula
	cost   formula
	hooks  []func(ctx *sim.TickContext, d Destroyed)

	// CanTarget, if set, restricts which units a player may target. It is
	// the hook for Milestone 5 visibility rules; nil means every unit is
	// targetable.
	CanTarget func(w *sim.World, attacker sim.PlayerID, target sim.EntityID) bool
}

// New returns the combat plugin. attrs may be nil (constant stats only).
func New(attrs *attributes.Plugin) *Plugin { return &Plugin{attrs: attrs} }

func (*Plugin) Name() string { return Name }

// DependsOn includes upgrades and cloning so the combat phase can be placed
// before theirs.
func (p *Plugin) DependsOn() []string {
	deps := []string{energy.Name, movement.Name, upgrades.Name, cloning.Name}
	if p.attrs != nil {
		deps = append(deps, attributes.Name)
	}
	return deps
}

// OnDestroyed registers a callback run for each destroyed unit, after it
// has been removed. Register during plugin registration only.
func (p *Plugin) OnDestroyed(fn func(ctx *sim.TickContext, d Destroyed)) {
	p.hooks = append(p.hooks, fn)
}

func (p *Plugin) hasProperty(name string) bool {
	return slices.Contains(builtins, name) || (p.attrs != nil && (p.attrs.Has(name) || name == attributes.PropInvested || name == attributes.PropEmbodied))
}

func (p *Plugin) compile(field, src string, allowTarget, allowDynamic bool) (formula, error) {
	e, err := expr.Parse(src)
	if err != nil {
		return formula{}, fmt.Errorf("combat %s: %w", field, err)
	}
	var f formula
	slotOf := map[string]int{}
	f.prog, err = e.Compile(func(name string) (int, bool) {
		if i, ok := slotOf[name]; ok {
			return i, true
		}
		ref := varRef{side: -1}
		switch {
		case name == "distance" && allowTarget:
			ref = varRef{side: 2}
		case strings.HasPrefix(name, "attacker_"):
			ref = varRef{side: 0, name: strings.TrimPrefix(name, "attacker_")}
		case strings.HasPrefix(name, "target_") && allowTarget:
			ref = varRef{side: 1, name: strings.TrimPrefix(name, "target_")}
		}
		if ref.side < 0 || (ref.side < 2 && !p.hasProperty(ref.name)) {
			return 0, false
		}
		if !allowDynamic && (ref.name == "health" || ref.name == "energy") {
			return 0, false
		}
		slotOf[name] = len(f.vars)
		f.vars = append(f.vars, ref)
		return slotOf[name], true
	})
	if err != nil {
		hint := "attacker_<property>"
		if allowTarget {
			hint += ", target_<property>, distance"
		}
		if !allowDynamic {
			hint += "; health and energy are not allowed here"
		}
		return formula{}, fmt.Errorf("combat %s: %w (available: %s)", field, err, hint)
	}
	return f, nil
}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	for _, s := range []struct {
		name string
		stat Stat
		min  int64
	}{{"max_health", p.cfg.MaxHealth, 1}, {"attack", p.cfg.Attack, 0}, {"range", p.cfg.Range, 0}, {"cooldown", p.cfg.Cooldown, 1}} {
		if s.stat.Default < s.min {
			return fmt.Errorf("combat %s.default must be >= %d", s.name, s.min)
		}
		if s.stat.Property != "" && (p.attrs == nil || !p.attrs.Has(s.stat.Property)) {
			return fmt.Errorf("combat %s.property: %q is not an attribute or derived property", s.name, s.stat.Property)
		}
	}
	var err error
	if p.damage, err = p.compile("damage", p.cfg.Damage, true, true); err != nil {
		return err
	}
	// The energy cost depends only on the attacker's static properties, so
	// it can be quoted per unit.
	if p.cost, err = p.compile("energy_cost", p.cfg.EnergyCost, false, false); err != nil {
		return err
	}

	if err := r.Component(healths.Def(sim.VisiblePublic, func(h Health) []sim.Field {
		return []sim.Field{
			{Key: "combat.health", Value: sim.IntValue(h.HP)},
			{Key: "combat.max_health", Value: sim.IntValue(h.Max)},
		}
	})); err != nil {
		return err
	}
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		return []sim.Field{
			{Key: "combat.target", Value: sim.StringValue(strconv.FormatUint(uint64(o.Target), 10))},
			{Key: "combat.out_of_range", Value: sim.BoolValue(o.OutOfRange)},
			{Key: "combat.stalled", Value: sim.BoolValue(o.Stalled)},
		}
	})); err != nil {
		return err
	}
	if err := r.Component(readyAt.Def(sim.VisibleOwner, func(t sim.Tick) []sim.Field {
		return []sim.Field{{Key: "combat.ready_at", Value: sim.IntValue(int64(t))}}
	})); err != nil {
		return err
	}
	if err := r.Component(statsCmp.Def(sim.VisibleOwner, func(s stats) []sim.Field {
		return []sim.Field{
			{Key: "combat.attack", Value: sim.IntValue(s.Attack)},
			{Key: "combat.range", Value: sim.IntValue(s.Range)},
			{Key: "combat.cooldown", Value: sim.IntValue(s.Cooldown)},
			{Key: "combat.attack_cost", Value: sim.IntValue(s.Cost)},
		}
	})); err != nil {
		return err
	}

	// Units get full integrity and a stats summary on creation (after the
	// attributes plugin set their defaults); both follow attribute changes.
	r.OnCreate(func(w *sim.World, e sim.Entity) {
		if e.Kind != sim.KindUnit {
			return
		}
		m := p.stat(w, e.ID, p.cfg.MaxHealth, 1)
		healths.Set(w, e.ID, Health{HP: m, Max: m})
		p.refreshStats(w, e.ID)
	})
	if p.attrs != nil {
		p.attrs.OnChange(func(w *sim.World, id sim.EntityID) {
			h, ok := healths.Get(w, id)
			if !ok {
				return
			}
			// Raising max integrity repairs by the same amount; lowering it
			// (admin only) clamps.
			m := p.stat(w, id, p.cfg.MaxHealth, 1)
			if m > h.Max {
				h.HP += m - h.Max
			}
			h.Max = m
			h.HP = min(h.HP, m)
			healths.Set(w, id, h)
			p.refreshStats(w, id)
		})
	}

	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{movement.Phase}, Before: []string{upgrades.Phase, cloning.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "combat.resolve", Phase: Phase, Run: p.resolve}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config":     p.cfg,
		"variables":  "damage: attacker_<p>, target_<p>, distance; energy_cost: attacker_<p> (no health/energy). <p> = attack, range, cooldown, max_health, health, energy, or an attributes property",
		"distance":   "Chebyshev (king-move) distance",
		"resolution": "eligibility, validation, energy, damage, aggregation, simultaneous application, destruction — all computed from the state at the start of the combat phase",
		"order":      "commands, energy, salvage, movement, combat, upgrades, cloning",
	}); err != nil {
		return err
	}
	for _, d := range []sim.CommandDef{
		{Type: CommandAttack, Execute: p.execAttack, ApplyToUnit: p.applyAttack},
		{Type: CommandStop, Execute: p.execStop, ApplyToUnit: p.applyStop},
		{Type: CommandSetHealth, AdminOnly: true, Execute: p.execSetHealth},
	} {
		if err := r.Command(d); err != nil {
			return err
		}
	}
	return nil
}

// stat evaluates a stat for a unit, at least lo.
func (p *Plugin) stat(w *sim.World, id sim.EntityID, s Stat, lo int64) int64 {
	if s.Property != "" {
		v, _ := p.attrs.Lookup(w, id, s.Property)
		return max(v, lo)
	}
	return max(s.Default, lo)
}

// property resolves a formula variable for one unit.
func (p *Plugin) property(w *sim.World, id sim.EntityID, name string) int64 {
	switch name {
	case "attack":
		return p.stat(w, id, p.cfg.Attack, 0)
	case "range":
		return p.stat(w, id, p.cfg.Range, 0)
	case "cooldown":
		return p.stat(w, id, p.cfg.Cooldown, 1)
	case "max_health":
		return p.stat(w, id, p.cfg.MaxHealth, 1)
	case "health":
		h, _ := healths.Get(w, id)
		return h.HP
	case "energy":
		v, _ := energy.Stored(w, id)
		return v
	}
	v, _ := p.attrs.Lookup(w, id, name)
	return v
}

func (p *Plugin) eval(f formula, w *sim.World, attacker, target sim.EntityID, distance int64) int64 {
	env := make([]int64, len(f.vars))
	for i, v := range f.vars {
		switch v.side {
		case 0:
			env[i] = p.property(w, attacker, v.name)
		case 1:
			env[i] = p.property(w, target, v.name)
		default:
			env[i] = distance
		}
	}
	return max(f.prog.Eval(env), 0)
}

func (p *Plugin) refreshStats(w *sim.World, id sim.EntityID) {
	statsCmp.Set(w, id, stats{
		Attack:   p.stat(w, id, p.cfg.Attack, 0),
		Range:    p.stat(w, id, p.cfg.Range, 0),
		Cooldown: p.stat(w, id, p.cfg.Cooldown, 1),
		Cost:     p.eval(p.cost, w, id, 0, 0),
	})
}

// GetHealth returns a unit's integrity.
func GetHealth(w *sim.World, id sim.EntityID) (Health, bool) { return healths.Get(w, id) }

// Distance is the Chebyshev distance used for range checks.
func Distance(a, b sim.Pos) int64 {
	dx, dy := int64(a.X-b.X), int64(a.Y-b.Y)
	return max(dx, -dx, dy, -dy)
}

// ---- commands ---------------------------------------------------------------

func (p *Plugin) checkAttack(w *sim.World, issuer sim.Principal, unit, target sim.EntityID) (sim.Entity, error) {
	a, ok := w.Entity(unit)
	if !ok || a.Kind != sim.KindUnit || !healths.Has(w, unit) {
		return a, sim.Unavailable("unit", sim.EntityID(unit))
	}
	if !issuer.Admin && a.Owner != issuer.Player {
		return a, sim.Unavailable("unit", sim.EntityID(unit))
	}
	// Missing, non-unit and undetected targets get one identical answer so
	// attack commands cannot be used to probe for hidden units.
	t, ok := w.Entity(target)
	if !ok || t.Kind != sim.KindUnit || !healths.Has(w, target) {
		return a, sim.Unavailable("target", target)
	}
	if t.Owner == a.Owner {
		return a, sim.Fail(sim.CodePermissionDenied, "target %d is friendly", target)
	}
	if p.CanTarget != nil && !p.CanTarget(w, a.Owner, target) {
		return a, sim.Unavailable("target", target)
	}
	if p.stat(w, unit, p.cfg.Attack, 0) <= 0 {
		return a, sim.Fail(sim.CodeFailedPrecondition, "unit %d cannot attack (attack strength 0)", unit)
	}
	return a, nil
}

func unitList(ids []uint64) error {
	if len(ids) == 0 || len(ids) > MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", MaxUnitsPerCommand, len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
	}
	return nil
}

func (p *Plugin) execAttack(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*combatv1.AttackCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want AttackCommand", cmd.Payload)
	}
	if err := unitList(m.GetUnitIds()); err != nil {
		return err
	}
	target := sim.EntityID(m.GetTargetId())
	for _, id := range m.GetUnitIds() {
		if _, err := p.checkAttack(ctx.World, cmd.Issuer, sim.EntityID(id), target); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		orders.Set(ctx.World, sim.EntityID(id), order{Target: target})
	}
	return nil
}

func (p *Plugin) applyAttack(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*combatv1.AttackCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want AttackCommand", cmd.Payload)
	}
	target := sim.EntityID(m.GetTargetId())
	if _, err := p.checkAttack(ctx.World, cmd.Issuer, unit, target); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	orders.Set(ctx.World, unit, order{Target: target})
	return nil
}

func ownedUnit(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit {
		return sim.Unavailable("unit", sim.EntityID(id))
	}
	if !issuer.Admin && e.Owner != issuer.Player {
		return sim.Unavailable("unit", sim.EntityID(id))
	}
	return nil
}

func (p *Plugin) execStop(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*combatv1.StopAttackCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want StopAttackCommand", cmd.Payload)
	}
	if err := unitList(m.GetUnitIds()); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		if err := ownedUnit(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		orders.Remove(ctx.World, sim.EntityID(id))
	}
	return nil
}

func (p *Plugin) applyStop(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	if err := ownedUnit(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	orders.Remove(ctx.World, unit)
	return nil
}

func (p *Plugin) execSetHealth(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*combatv1.SetHealthCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetHealthCommand", cmd.Payload)
	}
	id := sim.EntityID(m.GetUnitId())
	h, ok := healths.Get(ctx.World, id)
	if !ok {
		return sim.Unavailable("unit", sim.EntityID(m.GetUnitId()))
	}
	h.HP = max(0, min(m.GetHealth(), h.Max))
	healths.Set(ctx.World, id, h)
	if h.HP == 0 {
		p.destroy(ctx, id, "admin", nil)
	}
	return nil
}

// ---- resolution -------------------------------------------------------------

type attack struct {
	attacker, target sim.EntityID
	attackerOwner    sim.PlayerID
	cost, damage     int64
	cooldown         int64
}

func (p *Plugin) endOrder(ctx *sim.TickContext, id sim.EntityID, o order, reason string) {
	orders.Remove(ctx.World, id)
	e, _ := ctx.World.Entity(id)
	ctx.Emit(sim.Event{Type: EventEnded, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
		{Key: "target", Value: sim.StringValue(strconv.FormatUint(uint64(o.Target), 10))},
		{Key: "reason", Value: sim.StringValue(reason)},
	}})
}

func (p *Plugin) resolve(ctx *sim.TickContext) {
	w := ctx.World
	tick := ctx.Tick

	// Steps 1–4: read-only evaluation of every order against the start
	// state. Only order bookkeeping flags are written here; they do not
	// influence other attacks.
	var attacks []attack
	for _, id := range orders.Entities(w) {
		o, _ := orders.Get(w, id)
		a, _ := w.Entity(id)
		// Under fog of war an attacker's owner must still detect the target
		// (as of the end of the last tick). A target that slipped out of
		// detection and one that was destroyed unseen end the order the
		// same way, so neither reveals anything.
		if p.CanTarget != nil && !p.CanTarget(w, a.Owner, o.Target) {
			p.endOrder(ctx, id, o, "target_unavailable")
			continue
		}
		t, ok := w.Entity(o.Target)
		switch {
		case !ok || t.Kind != sim.KindUnit || !healths.Has(w, o.Target):
			p.endOrder(ctx, id, o, "target_lost")
			continue
		case p.stat(w, id, p.cfg.Attack, 0) <= 0:
			p.endOrder(ctx, id, o, "incapable")
			continue
		}
		rng := p.stat(w, id, p.cfg.Range, 0)
		dist := Distance(a.Pos, t.Pos)
		if dist > rng {
			if !o.OutOfRange {
				o.OutOfRange = true
				orders.Set(w, id, o)
				ctx.Emit(sim.Event{Type: EventOutOfRange, Entity: id, Audience: sim.ToPlayer(a.Owner), Fields: []sim.Field{
					{Key: "target", Value: sim.StringValue(strconv.FormatUint(uint64(o.Target), 10))},
					{Key: "distance", Value: sim.IntValue(dist)},
					{Key: "range", Value: sim.IntValue(rng)},
				}})
			}
			continue
		}
		if r, ok := readyAt.Get(w, id); ok && tick < r {
			if o.OutOfRange {
				o.OutOfRange = false
				orders.Set(w, id, o)
			}
			continue
		}
		cost := p.eval(p.cost, w, id, 0, 0)
		if stored, _ := energy.Stored(w, id); stored < cost {
			if !o.Stalled {
				o.Stalled, o.OutOfRange = true, false
				orders.Set(w, id, o)
				ctx.Emit(sim.Event{Type: EventStalled, Entity: id, Audience: sim.ToPlayer(a.Owner), Fields: []sim.Field{
					{Key: "required", Value: sim.IntValue(cost)},
					{Key: "stored", Value: sim.IntValue(stored)},
				}})
			}
			continue
		}
		if o.OutOfRange || o.Stalled {
			o.OutOfRange, o.Stalled = false, false
			orders.Set(w, id, o)
		}
		attacks = append(attacks, attack{
			attacker: id, target: o.Target, attackerOwner: a.Owner,
			cost:     cost,
			damage:   p.eval(p.damage, w, id, o.Target, dist),
			cooldown: p.stat(w, id, p.cfg.Cooldown, 1),
		})
	}
	if len(attacks) == 0 {
		return
	}

	// Step 5: aggregate damage per target.
	total := map[sim.EntityID]int64{}
	hitBy := map[sim.EntityID][]sim.PlayerID{}
	attackers := map[sim.EntityID]int64{}
	for _, at := range attacks {
		total[at.target] = satAdd(total[at.target], at.damage)
		hitBy[at.target] = append(hitBy[at.target], at.attackerOwner)
		attackers[at.target]++
	}

	// Step 6: apply energy, cooldowns, then damage — all independent
	// per-unit updates, so their order does not matter.
	for _, at := range attacks {
		if !energy.Spend(w, at.attacker, at.cost) {
			panic("combat: validated attack could not pay")
		}
		readyAt.Set(w, at.attacker, tick+sim.Tick(at.cooldown))
		ctx.Emit(sim.Event{Type: EventAttack, Entity: at.attacker, Audience: sim.ToPlayer(at.attackerOwner), Fields: []sim.Field{
			{Key: "target", Value: sim.StringValue(strconv.FormatUint(uint64(at.target), 10))},
			{Key: "damage", Value: sim.IntValue(at.damage)},
			{Key: "cost", Value: sim.IntValue(at.cost)},
		}})
	}
	targets := make([]sim.EntityID, 0, len(total))
	for t := range total {
		targets = append(targets, t)
	}
	slices.Sort(targets)
	var dead []sim.EntityID
	for _, t := range targets {
		h, _ := healths.Get(w, t)
		h.HP = max(0, h.HP-total[t])
		healths.Set(w, t, h)
		e, _ := w.Entity(t)
		ctx.Emit(sim.Event{Type: EventDamaged, Entity: t, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
			{Key: "damage", Value: sim.IntValue(total[t])},
			{Key: "health", Value: sim.IntValue(h.HP)},
			{Key: "attackers", Value: sim.IntValue(attackers[t])},
		}})
		if h.HP == 0 {
			dead = append(dead, t)
		}
	}

	// Step 7: destruction, after all damage is applied.
	for _, t := range dead {
		p.destroy(ctx, t, "destroyed", hitBy[t])
	}
}

// destroy removes a unit, notifies its owner and the given attacking
// owners, and runs OnDestroyed hooks.
func (p *Plugin) destroy(ctx *sim.TickContext, id sim.EntityID, reason string, attackingOwners []sim.PlayerID) {
	w := ctx.World
	e, ok := w.Entity(id)
	if !ok {
		return
	}
	d := Destroyed{Unit: e}
	d.Stored, _ = energy.Stored(w, id)
	if p.attrs != nil {
		if u, ok := p.attrs.Get(w, id); ok {
			d.Embodied, d.Invested = p.attrs.Embodied(u), u.Invested
		}
	}
	if err := w.RemoveEntity(id); err != nil {
		panic(fmt.Sprintf("combat: remove destroyed unit: %v", err))
	}
	audience := []sim.PlayerID{e.Owner}
	for _, o := range attackingOwners {
		if !slices.Contains(audience, o) {
			audience = append(audience, o)
		}
	}
	slices.Sort(audience)
	d.Audience = audience
	for _, o := range audience {
		ctx.Emit(sim.Event{Type: EventDestroyed, Entity: id, Audience: sim.ToPlayer(o), Fields: []sim.Field{
			{Key: "position", Value: sim.PosValue(e.Pos)},
			{Key: "owner", Value: sim.IntValue(int64(e.Owner))},
			{Key: "reason", Value: sim.StringValue(reason)},
		}})
	}
	for _, fn := range p.hooks {
		fn(ctx, d)
	}
}

func satAdd(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}
	return 1<<63 - 1
}
