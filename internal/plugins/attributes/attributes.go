// Package attributes is the data-driven attribute registry. Operators
// define attributes (armor, propulsion, ...) and derived properties in
// configuration; every unit carries a value for every attribute. There are
// no unit classes: units differ only by their attribute values.
//
// # Attribute definitions
//
// Each attribute has an integer default, min and max (max - min <=
// MaxLevels) and three formulas in the expr language, each a function of
// the attribute value v:
//
//   - mass: the attribute's contribution to the unit's mass.
//   - cost: the cumulative energy invested to reach value v. Upgrading
//     from a to b costs cost(b) - cost(a), so the price does not depend on
//     how an upgrade is split into steps, and a convex cost function makes
//     upgrades nonlinear. cost must be non-decreasing over [min, max].
//   - duration: the cumulative ticks to reach value v. An upgrade from a
//     to b takes max(1, duration(b) - duration(a)) ticks. Non-decreasing.
//
// cost and duration are tabulated at load time over [min, max], which
// validates them and makes lookups O(1).
//
// # Derived properties
//
// "mass" is built in: base_mass + the sum of every attribute's mass
// contribution. Further derived properties are formulas over attribute
// values, mass and other derived properties; they are evaluated in
// dependency order and cycles are rejected at load time. Because they
// depend only on a unit's own attribute values, they are deterministic and
// never stored.
//
// # Embodied investment
//
// A unit's embodied investment is sum(cost(value) - cost(default)) over its
// attributes: the energy its upgrades represent. The cloning plugin charges
// at least this much, so copying a unit can never be cheaper than building
// its upgrades.
package attributes

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	attributesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/attributes/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/expr"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "attributes"

// Limits.
const (
	MaxAttributes = 16
	MaxDerived    = 32
	MaxLevels     = 1000
)

// Built-in property names.
const (
	PropMass     = "mass"
	PropInvested = "invested"
	PropEmbodied = "embodied"
)

// AttrPrefix prefixes observable attribute keys: "attributes.armor".
const AttrPrefix = "attributes."

// CommandSet is the admin-only SetAttributeCommand.
var CommandSet = string((&attributesv1.SetAttributeCommand{}).ProtoReflect().Descriptor().FullName())

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reserved = map[string]bool{
		PropMass: true, PropInvested: true, PropEmbodied: true, "v": true,
		"min": true, "max": true, "clamp": true, "abs": true, "pow": true, "sqrt": true,
	}
)

// AttrConfig configures one attribute.
type AttrConfig struct {
	Default  int64  `json:"default"`
	Min      int64  `json:"min"`
	Max      int64  `json:"max"`
	Mass     string `json:"mass,omitempty"`
	Cost     string `json:"cost,omitempty"`
	Duration string `json:"duration,omitempty"`
	// Description is shown to players and admins.
	Description string `json:"description,omitempty"`
}

// Config is the plugin's configuration schema (the "attributes" section).
type Config struct {
	BaseMass   int64                 `json:"base_mass"`
	Attributes map[string]AttrConfig `json:"attributes"`
	Derived    map[string]string     `json:"derived"`
}

type attrDef struct {
	name   string
	cfg    AttrConfig
	mass   *expr.Program
	invest []int64 // cumulative cost at v = min + i
	time   []int64 // cumulative ticks at v = min + i
}

type derivedDef struct {
	name string
	src  string
	prog *expr.Program
}

// Unit is a unit's attribute state.
type Unit struct {
	// Values is indexed like Plugin.Names().
	Values [MaxAttributes]int64
	// Invested is the energy actually spent on this unit's upgrades,
	// including the embodied investment inherited by clones.
	Invested int64
}

// project marks a unit as busy with a long-running activity (an upgrade or
// cloning), which are mutually exclusive.
type project struct {
	Kind string
}

var (
	units    = sim.NewComponent[Unit]("attributes.unit")
	projects = sim.NewComponent[project]("attributes.project")
)

// Plugin implements sim.Plugin and is the query API other plugins use.
type Plugin struct {
	baseMass int64
	attrs    []attrDef    // sorted by name
	derived  []derivedDef // dependency order
	slots    map[string]int
	subs     []func(w *sim.World, id sim.EntityID)
}

// New returns the attributes plugin.
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return nil }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	var cfg Config
	if err := r.DecodeConfig(&cfg); err != nil {
		return err
	}
	if err := p.load(cfg); err != nil {
		return fmt.Errorf("attributes config: %w", err)
	}
	if err := r.Component(units.Def(sim.VisibleOwner, p.observe)); err != nil {
		return err
	}
	if err := r.Component(projects.Def(sim.VisibleOwner, func(pr project) []sim.Field {
		return []sim.Field{{Key: "attributes.busy", Value: sim.StringValue(pr.Kind)}}
	})); err != nil {
		return err
	}
	// Every property is also a kernel derived property, e.g.
	// "attributes.mass", so generic tools can read them by name.
	for name := range p.slots {
		if err := r.Derived(sim.DerivedDef{Name: AttrPrefix + name, Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
			return p.Lookup(w, id, name)
		}}); err != nil {
			return err
		}
	}
	r.OnCreate(func(w *sim.World, e sim.Entity) {
		if e.Kind == sim.KindUnit {
			units.Set(w, e.ID, p.Defaults())
		}
	})
	if err := r.Command(sim.CommandDef{Type: CommandSet, AdminOnly: true, Execute: p.execSet}); err != nil {
		return err
	}
	return r.Describe(p.describe())
}

func (p *Plugin) load(cfg Config) error {
	if len(cfg.Attributes) > MaxAttributes {
		return fmt.Errorf("at most %d attributes, got %d", MaxAttributes, len(cfg.Attributes))
	}
	if len(cfg.Derived) > MaxDerived {
		return fmt.Errorf("at most %d derived properties, got %d", MaxDerived, len(cfg.Derived))
	}
	p.baseMass = cfg.BaseMass
	p.slots = map[string]int{}
	var names []string
	for name := range cfg.Attributes {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		if !nameRE.MatchString(name) || reserved[name] {
			return fmt.Errorf("invalid attribute name %q", name)
		}
		c := cfg.Attributes[name]
		def, err := compileAttr(name, c)
		if err != nil {
			return err
		}
		p.attrs = append(p.attrs, def)
		p.slots[name] = i
	}
	p.slots[PropMass] = len(p.attrs)

	// Derived properties: validate names, parse, then order by dependency.
	parsed := map[string]*expr.Expr{}
	var dnames []string
	for name, src := range cfg.Derived {
		if !nameRE.MatchString(name) || reserved[name] {
			return fmt.Errorf("invalid derived property name %q", name)
		}
		if _, clash := p.slots[name]; clash {
			return fmt.Errorf("derived property %q has the same name as an attribute", name)
		}
		e, err := expr.Parse(src)
		if err != nil {
			return fmt.Errorf("derived %q: %w", name, err)
		}
		parsed[name] = e
		dnames = append(dnames, name)
	}
	slices.Sort(dnames)
	deps := map[string][]string{}
	for _, name := range dnames {
		for _, v := range parsed[name].Vars() {
			_, isAttr := p.slots[v]
			_, isDerived := parsed[v]
			switch {
			case isDerived:
				deps[name] = append(deps[name], v)
			case !isAttr:
				return fmt.Errorf("derived %q: unknown variable %q (attributes: %s; built-in: mass)", name, v, strings.Join(names, ", "))
			}
		}
	}
	order, err := topo(dnames, deps)
	if err != nil {
		return err
	}
	for i, name := range order {
		p.slots[name] = len(p.attrs) + 1 + i
	}
	for _, name := range order {
		prog, err := parsed[name].Compile(func(v string) (int, bool) {
			i, ok := p.slots[v]
			return i, ok
		})
		if err != nil {
			return fmt.Errorf("derived %q: %w", name, err)
		}
		p.derived = append(p.derived, derivedDef{name: name, src: cfg.Derived[name], prog: prog})
	}
	return nil
}

func compileAttr(name string, c AttrConfig) (attrDef, error) {
	d := attrDef{name: name, cfg: c}
	if c.Min < 0 || c.Max < c.Min || c.Default < c.Min || c.Default > c.Max {
		return d, fmt.Errorf("attribute %q: need 0 <= min <= default <= max, got min %d default %d max %d", name, c.Min, c.Default, c.Max)
	}
	if c.Max-c.Min > MaxLevels {
		return d, fmt.Errorf("attribute %q: max - min must be <= %d", name, MaxLevels)
	}
	prog := func(field, src string) (*expr.Program, error) {
		if src == "" {
			src = "0"
		}
		e, err := expr.Parse(src)
		if err != nil {
			return nil, fmt.Errorf("attribute %q %s: %w", name, field, err)
		}
		pr, err := e.CompileVars("v")
		if err != nil {
			return nil, fmt.Errorf("attribute %q %s: %w (only v is available)", name, field, err)
		}
		return pr, nil
	}
	var err error
	if d.mass, err = prog("mass", c.Mass); err != nil {
		return d, err
	}
	cost, err := prog("cost", c.Cost)
	if err != nil {
		return d, err
	}
	dur, err := prog("duration", c.Duration)
	if err != nil {
		return d, err
	}
	n := c.Max - c.Min + 1
	d.invest = make([]int64, n)
	d.time = make([]int64, n)
	for i := range n {
		env := []int64{c.Min + i}
		d.invest[i] = cost.Eval(env)
		d.time[i] = dur.Eval(env)
		if i > 0 && d.invest[i] < d.invest[i-1] {
			return d, fmt.Errorf("attribute %q: cost must be non-decreasing, but cost(%d)=%d < cost(%d)=%d", name, c.Min+i, d.invest[i], c.Min+i-1, d.invest[i-1])
		}
		if i > 0 && d.time[i] < d.time[i-1] {
			return d, fmt.Errorf("attribute %q: duration must be non-decreasing, but duration(%d)=%d < duration(%d)=%d", name, c.Min+i, d.time[i], c.Min+i-1, d.time[i-1])
		}
	}
	return d, nil
}

// topo orders derived properties so dependencies come first (ties by name).
func topo(names []string, deps map[string][]string) ([]string, error) {
	var out []string
	state := map[string]int{} // 0 new, 1 visiting, 2 done
	var visit func(n string, path []string) error
	visit = func(n string, path []string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("derived properties form a cycle: %s", strings.Join(append(path, n), " -> "))
		case 2:
			return nil
		}
		state[n] = 1
		ds := slices.Clone(deps[n])
		slices.Sort(ds)
		for _, d := range ds {
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, n)
		return nil
	}
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- queries ----------------------------------------------------------------

// Names returns the attribute names in index order.
func (p *Plugin) Names() []string {
	out := make([]string, len(p.attrs))
	for i, a := range p.attrs {
		out[i] = a.name
	}
	return out
}

// Index returns an attribute's index in Unit.Values.
func (p *Plugin) Index(attr string) (int, bool) {
	i, ok := p.slots[attr]
	return i, ok && i < len(p.attrs)
}

// Has reports whether name is an attribute or a derived property
// (including mass).
func (p *Plugin) Has(name string) bool {
	_, ok := p.slots[name]
	return ok
}

// Bounds returns an attribute's min and max.
func (p *Plugin) Bounds(i int) (lo, hi int64) { return p.attrs[i].cfg.Min, p.attrs[i].cfg.Max }

// Defaults returns the attribute state of a fresh unit.
func (p *Plugin) Defaults() Unit {
	var u Unit
	for i, a := range p.attrs {
		u.Values[i] = a.cfg.Default
	}
	return u
}

// Get returns a unit's attribute state.
func (p *Plugin) Get(w *sim.World, id sim.EntityID) (Unit, bool) { return units.Get(w, id) }

// env evaluates every property of u into a fixed-size array.
func (p *Plugin) env(u Unit) [MaxAttributes + 1 + MaxDerived]int64 {
	var env [MaxAttributes + 1 + MaxDerived]int64
	n := len(p.attrs)
	copy(env[:n], u.Values[:n])
	mass := p.baseMass
	for i, a := range p.attrs {
		mass = satAdd(mass, a.mass.Eval([]int64{u.Values[i]}))
	}
	env[n] = mass
	for i, d := range p.derived {
		env[n+1+i] = d.prog.Eval(env[:])
	}
	return env
}

// Property evaluates an attribute, mass or derived property for a state.
func (p *Plugin) Property(u Unit, name string) (int64, bool) {
	switch name {
	case PropInvested:
		return u.Invested, true
	case PropEmbodied:
		return p.Embodied(u), true
	}
	slot, ok := p.slots[name]
	if !ok {
		return 0, false
	}
	if slot < len(p.attrs) {
		return u.Values[slot], true
	}
	env := p.env(u)
	return env[slot], true
}

// Lookup evaluates a property for a unit.
func (p *Plugin) Lookup(w *sim.World, id sim.EntityID, name string) (int64, bool) {
	u, ok := units.Get(w, id)
	if !ok {
		return 0, false
	}
	return p.Property(u, name)
}

// Embodied returns the energy embodied in a unit's upgrades.
func (p *Plugin) Embodied(u Unit) int64 {
	var sum int64
	for i, a := range p.attrs {
		sum = satAdd(sum, a.investAt(u.Values[i])-a.investAt(a.cfg.Default))
	}
	return sum
}

func (a attrDef) investAt(v int64) int64 { return a.invest[v-a.cfg.Min] }
func (a attrDef) timeAt(v int64) int64   { return a.time[v-a.cfg.Min] }

// UpgradeCost returns the energy and ticks to raise attribute i from one
// value to a higher one. Both values must be within the attribute's bounds.
func (p *Plugin) UpgradeCost(i int, from, to int64) (cost, ticks int64) {
	a := p.attrs[i]
	return a.investAt(to) - a.investAt(from), max(1, a.timeAt(to)-a.timeAt(from))
}

// OnChange registers a callback run after a unit's attribute values change
// (upgrade completion, cloning inheritance, admin edits). Plugins whose
// state depends on attributes (e.g. energy capacity) use it to stay in
// sync. Register callbacks during plugin registration only.
func (p *Plugin) OnChange(fn func(w *sim.World, id sim.EntityID)) { p.subs = append(p.subs, fn) }

// Apply sets attribute i of a unit to value and adds invested energy. It is
// how upgrades complete. It notifies OnChange subscribers.
func (p *Plugin) Apply(w *sim.World, id sim.EntityID, i int, value, invested int64) {
	u, _ := units.Get(w, id)
	u.Values[i] = value
	u.Invested = satAdd(u.Invested, invested)
	units.Set(w, id, u)
	p.notify(w, id)
}

// Inherit gives a new unit a copy of a parent's attribute values. The
// child's Invested is the parent's embodied investment, which the cloning
// plugin charged for.
func (p *Plugin) Inherit(w *sim.World, child sim.EntityID, parent Unit) {
	units.Set(w, child, Unit{Values: parent.Values, Invested: p.Embodied(parent)})
	p.notify(w, child)
}

func (p *Plugin) notify(w *sim.World, id sim.EntityID) {
	for _, fn := range p.subs {
		fn(w, id)
	}
}

// Reserve marks a unit busy with an activity. It fails if the unit is
// already busy.
func (p *Plugin) Reserve(w *sim.World, id sim.EntityID, kind string) bool {
	if projects.Has(w, id) {
		return false
	}
	projects.Set(w, id, project{Kind: kind})
	return true
}

// Release clears a unit's busy mark.
func (p *Plugin) Release(w *sim.World, id sim.EntityID) { projects.Remove(w, id) }

// Busy returns the activity a unit is busy with.
func (p *Plugin) Busy(w *sim.World, id sim.EntityID) (string, bool) {
	pr, ok := projects.Get(w, id)
	return pr.Kind, ok
}

// observe exposes attribute values and every derived property.
func (p *Plugin) observe(u Unit) []sim.Field {
	env := p.env(u)
	fields := make([]sim.Field, 0, len(p.slots)+2)
	for i, a := range p.attrs {
		fields = append(fields, sim.Field{Key: AttrPrefix + a.name, Value: sim.IntValue(u.Values[i])})
	}
	fields = append(fields, sim.Field{Key: AttrPrefix + PropMass, Value: sim.IntValue(env[len(p.attrs)])})
	for i, d := range p.derived {
		fields = append(fields, sim.Field{Key: AttrPrefix + d.name, Value: sim.IntValue(env[len(p.attrs)+1+i])})
	}
	fields = append(fields,
		sim.Field{Key: AttrPrefix + PropInvested, Value: sim.IntValue(u.Invested)},
		sim.Field{Key: AttrPrefix + PropEmbodied, Value: sim.IntValue(p.Embodied(u))},
	)
	return fields
}

func (p *Plugin) execSet(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*attributesv1.SetAttributeCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetAttributeCommand", cmd.Payload)
	}
	id := sim.EntityID(m.GetUnitId())
	if !units.Has(ctx.World, id) {
		return sim.Fail(sim.CodeNotFound, "unit %d not found", m.GetUnitId())
	}
	i, ok := p.Index(m.GetAttribute())
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "unknown attribute %q", m.GetAttribute())
	}
	lo, hi := p.Bounds(i)
	if m.GetValue() < lo || m.GetValue() > hi {
		return sim.Fail(sim.CodeInvalidArgument, "%s must be in %d..%d", m.GetAttribute(), lo, hi)
	}
	// Admin override: may lower values; Invested is left untouched.
	p.Apply(ctx.World, id, i, m.GetValue(), 0)
	return nil
}

// ---- description ------------------------------------------------------------

// AttrDescription is the public description of an attribute.
type AttrDescription struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Default     int64   `json:"default"`
	Min         int64   `json:"min"`
	Max         int64   `json:"max"`
	Mass        string  `json:"mass"`
	Cost        string  `json:"cost"`
	Duration    string  `json:"duration"`
	CostTable   []int64 `json:"cost_table"`     // cumulative cost at min..max
	TicksTable  []int64 `json:"duration_table"` // cumulative ticks at min..max
}

// Description is the plugin's published rule description.
type Description struct {
	BaseMass     int64             `json:"base_mass"`
	Attributes   []AttrDescription `json:"attributes"`
	Derived      map[string]string `json:"derived"`
	DerivedOrder []string          `json:"derived_order"`
	Notes        string            `json:"notes"`
}

func (p *Plugin) describe() Description {
	d := Description{
		BaseMass: p.baseMass,
		Derived:  map[string]string{PropMass: "base_mass + sum of attribute mass contributions"},
		Notes:    "Upgrade cost from a to b = cost_table[b-min] - cost_table[a-min]; ticks = max(1, duration_table[b-min] - duration_table[a-min]).",
	}
	for _, a := range p.attrs {
		or0 := func(s string) string {
			if s == "" {
				return "0"
			}
			return s
		}
		d.Attributes = append(d.Attributes, AttrDescription{
			Name: a.name, Description: a.cfg.Description,
			Default: a.cfg.Default, Min: a.cfg.Min, Max: a.cfg.Max,
			Mass: or0(a.cfg.Mass), Cost: or0(a.cfg.Cost), Duration: or0(a.cfg.Duration),
			CostTable: a.invest, TicksTable: a.time,
		})
	}
	for _, dd := range p.derived {
		d.Derived[dd.name] = dd.src
		d.DerivedOrder = append(d.DerivedOrder, dd.name)
	}
	return d
}

func satAdd(a, b int64) int64 {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		if a > 0 {
			return 1<<63 - 1
		}
		return -1 << 63
	}
	return s
}
