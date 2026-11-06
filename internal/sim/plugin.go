package sim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Plugin is a compiled-in package that contributes game mechanics.
//
// Register is called exactly once, in dependency order, while the Ruleset is
// built. Everything a plugin contributes is declared through the Registrar;
// plugins never reach into the kernel otherwise.
type Plugin interface {
	// Name is a unique, lowercase identifier, e.g. "movement". It prefixes
	// the plugin's component, system, derived-property and event names and
	// keys its configuration section.
	Name() string
	// DependsOn lists plugins that must be registered first.
	DependsOn() []string
	Register(r *Registrar) error
}

// PhaseDef declares a simulation phase. Phases run every tick after
// commands execute, in an order satisfying every After constraint (ties
// broken by name).
type PhaseDef struct {
	Name  string
	After []string
}

// SystemDef declares a system: a function run once per tick within a phase.
// Systems within a phase run in an order satisfying After (names of other
// systems in the same phase), ties broken by name.
type SystemDef struct {
	Name  string // "<plugin>.<name>"
	Phase string
	After []string
	Run   func(ctx *TickContext)
}

// DerivedDef declares a derived property: a value computed from an entity's
// state rather than stored. This is the hook future configurable formulas
// will plug into.
type DerivedDef struct {
	Name    string // "<plugin>.<name>"
	Compute func(w *World, id EntityID) (int64, bool)
}

// Registrar collects a plugin's contributions.
type Registrar struct {
	plugin string
	config json.RawMessage
	b      *rulesetBuilder
}

// PluginName returns the name of the plugin being registered.
func (r *Registrar) PluginName() string { return r.plugin }

// DecodeConfig strictly decodes the plugin's configuration section into v.
// v should be pre-populated with defaults; absent sections leave it
// untouched. Unknown fields are an error. This typed struct (plus the
// plugin's own validation) is the plugin's configuration schema.
func (r *Registrar) DecodeConfig(v any) error {
	if len(r.config) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(r.config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("plugin %q config: %w", r.plugin, err)
	}
	return nil
}

func (r *Registrar) requirePrefix(what, name string) error {
	if !strings.HasPrefix(name, r.plugin+".") || len(name) == len(r.plugin)+1 {
		return fmt.Errorf("plugin %q: %s %q must be prefixed with %q", r.plugin, what, name, r.plugin+".")
	}
	return nil
}

// Phase declares a phase.
func (r *Registrar) Phase(p PhaseDef) error {
	if p.Name == "" {
		return fmt.Errorf("plugin %q: phase name is empty", r.plugin)
	}
	if _, dup := r.b.phases[p.Name]; dup {
		return fmt.Errorf("plugin %q: phase %q already declared", r.plugin, p.Name)
	}
	r.b.phases[p.Name] = p
	return nil
}

// System declares a system.
func (r *Registrar) System(s SystemDef) error {
	if err := r.requirePrefix("system", s.Name); err != nil {
		return err
	}
	if s.Run == nil {
		return fmt.Errorf("plugin %q: system %q has no Run func", r.plugin, s.Name)
	}
	if _, dup := r.b.systems[s.Name]; dup {
		return fmt.Errorf("plugin %q: system %q already declared", r.plugin, s.Name)
	}
	r.b.systems[s.Name] = s
	return nil
}

// Command declares a command handler.
func (r *Registrar) Command(c CommandDef) error {
	if c.Type == "" || c.Execute == nil {
		return fmt.Errorf("plugin %q: command needs a Type and Execute func", r.plugin)
	}
	if _, dup := r.b.commands[c.Type]; dup {
		return fmt.Errorf("plugin %q: command %q already declared", r.plugin, c.Type)
	}
	r.b.commands[c.Type] = c
	return nil
}

// Component declares a component.
func (r *Registrar) Component(c ComponentDef) error {
	if err := r.requirePrefix("component", c.ID); err != nil {
		return err
	}
	if _, dup := r.b.components[c.ID]; dup {
		return fmt.Errorf("plugin %q: component %q already declared", r.plugin, c.ID)
	}
	r.b.components[c.ID] = c
	return nil
}

// Init registers a function run once by Engine.Init, after the initial
// world from configuration has been created and before the first tick.
// Plugins use it to place their own initial entities. Init functions run in
// plugin registration order.
func (r *Registrar) Init(fn func(w *World) error) {
	r.b.inits = append(r.b.inits, namedInit{plugin: r.plugin, fn: fn})
}

// OnCreate registers a function called whenever an entity is created, right
// after it exists (in setup code, commands or systems). Plugins use it to
// attach default components, e.g. an energy store to every new unit. Hooks
// run in plugin registration order and must not create or remove entities.
func (r *Registrar) OnCreate(fn func(w *World, e Entity)) {
	r.b.onCreate = append(r.b.onCreate, fn)
}

// Describe publishes a JSON-serializable description of the plugin's
// effective rules (configuration after defaults, formulas, tables). The
// description is public: servers expose it to players and admins so they
// can display costs and inspect how mechanics are configured.
func (r *Registrar) Describe(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("plugin %q: describe: %w", r.plugin, err)
	}
	r.b.rules[r.plugin] = b
	return nil
}

// Derived declares a derived property.
func (r *Registrar) Derived(d DerivedDef) error {
	if err := r.requirePrefix("derived property", d.Name); err != nil {
		return err
	}
	if d.Compute == nil {
		return fmt.Errorf("plugin %q: derived property %q has no Compute func", r.plugin, d.Name)
	}
	if _, dup := r.b.derived[d.Name]; dup {
		return fmt.Errorf("plugin %q: derived property %q already declared", r.plugin, d.Name)
	}
	r.b.derived[d.Name] = d
	return nil
}

// Ruleset is the immutable, validated combination of plugins that defines a
// game. It is built once at startup.
type Ruleset struct {
	plugins        []string
	phases         []phase
	commands       map[string]CommandDef
	components     map[string]ComponentDef
	componentOrder []string // sorted
	derived        map[string]DerivedDef
	inits          []namedInit
	onCreate       []func(w *World, e Entity)
	rules          map[string]json.RawMessage
}

// Rules returns each plugin's published rule description (see
// Registrar.Describe), keyed by plugin name.
func (rs *Ruleset) Rules() map[string]json.RawMessage { return rs.rules }

type namedInit struct {
	plugin string
	fn     func(w *World) error
}

type phase struct {
	name    string
	systems []SystemDef
}

// Plugins returns plugin names in registration order.
func (rs *Ruleset) Plugins() []string { return slices.Clone(rs.plugins) }

// CommandTypes returns all registered command types, sorted.
func (rs *Ruleset) CommandTypes(includeAdmin bool) []string {
	var out []string
	for t, c := range rs.commands {
		if includeAdmin || !c.AdminOnly {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// Schedule describes the per-tick execution order as "phase: system, ..."
// lines. Useful for documentation and debugging.
func (rs *Ruleset) Schedule() []string {
	var out []string
	for _, p := range rs.phases {
		names := make([]string, len(p.systems))
		for i, s := range p.systems {
			names[i] = s.Name
		}
		out = append(out, p.name+": "+strings.Join(names, ", "))
	}
	return out
}

type rulesetBuilder struct {
	phases     map[string]PhaseDef
	systems    map[string]SystemDef
	commands   map[string]CommandDef
	components map[string]ComponentDef
	derived    map[string]DerivedDef
	inits      []namedInit
	onCreate   []func(w *World, e Entity)
	rules      map[string]json.RawMessage
}

// NewRuleset validates and registers plugins. config maps plugin names to
// their raw JSON configuration sections; sections for unknown plugins are an
// error.
func NewRuleset(plugins []Plugin, config map[string]json.RawMessage) (*Ruleset, error) {
	byName := make(map[string]Plugin, len(plugins))
	for _, p := range plugins {
		name := p.Name()
		if name == "" || strings.ContainsAny(name, ". ") {
			return nil, fmt.Errorf("invalid plugin name %q", name)
		}
		if name == "sim" {
			return nil, errors.New(`plugin name "sim" is reserved`)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("plugin %q registered twice", name)
		}
		byName[name] = p
	}
	for name := range config {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("configuration for unknown plugin %q", name)
		}
	}

	deps := make(map[string][]string, len(byName))
	for name, p := range byName {
		for _, d := range p.DependsOn() {
			if _, ok := byName[d]; !ok {
				return nil, fmt.Errorf("plugin %q depends on missing plugin %q", name, d)
			}
		}
		deps[name] = p.DependsOn()
	}
	order, err := topoSort(deps)
	if err != nil {
		return nil, fmt.Errorf("plugin dependencies: %w", err)
	}

	b := &rulesetBuilder{
		phases:     map[string]PhaseDef{},
		systems:    map[string]SystemDef{},
		commands:   map[string]CommandDef{},
		components: map[string]ComponentDef{},
		derived:    map[string]DerivedDef{},
		rules:      map[string]json.RawMessage{},
	}
	for _, c := range builtinCommands() {
		b.commands[c.Type] = c
	}
	for _, name := range order {
		r := &Registrar{plugin: name, config: config[name], b: b}
		if err := byName[name].Register(r); err != nil {
			return nil, fmt.Errorf("register plugin %q: %w", name, err)
		}
	}

	rs := &Ruleset{
		plugins:    order,
		commands:   b.commands,
		components: b.components,
		derived:    b.derived,
		inits:      b.inits,
		onCreate:   b.onCreate,
		rules:      b.rules,
	}
	for id := range b.components {
		rs.componentOrder = append(rs.componentOrder, id)
	}
	slices.Sort(rs.componentOrder)

	// Order phases.
	phaseDeps := make(map[string][]string, len(b.phases))
	for name, p := range b.phases {
		for _, a := range p.After {
			if _, ok := b.phases[a]; !ok {
				return nil, fmt.Errorf("phase %q must run after unknown phase %q", name, a)
			}
		}
		phaseDeps[name] = p.After
	}
	phaseOrder, err := topoSort(phaseDeps)
	if err != nil {
		return nil, fmt.Errorf("phase ordering: %w", err)
	}

	// Order systems within each phase.
	sysDeps := make(map[string]map[string][]string)
	for name, s := range b.systems {
		if _, ok := b.phases[s.Phase]; !ok {
			return nil, fmt.Errorf("system %q uses unknown phase %q", name, s.Phase)
		}
		for _, a := range s.After {
			other, ok := b.systems[a]
			if !ok {
				return nil, fmt.Errorf("system %q must run after unknown system %q", name, a)
			}
			if other.Phase != s.Phase {
				return nil, fmt.Errorf("system %q (phase %q) cannot order after system %q in another phase (%q); order the phases instead", name, s.Phase, a, other.Phase)
			}
		}
		if sysDeps[s.Phase] == nil {
			sysDeps[s.Phase] = map[string][]string{}
		}
		sysDeps[s.Phase][name] = s.After
	}
	for _, pn := range phaseOrder {
		ordered, err := topoSort(sysDeps[pn])
		if err != nil {
			return nil, fmt.Errorf("system ordering in phase %q: %w", pn, err)
		}
		ph := phase{name: pn}
		for _, sn := range ordered {
			ph.systems = append(ph.systems, b.systems[sn])
		}
		rs.phases = append(rs.phases, ph)
	}
	return rs, nil
}

// topoSort orders nodes so each comes after its dependencies, breaking ties
// by name so the result is independent of map iteration order.
func topoSort(deps map[string][]string) ([]string, error) {
	indegree := make(map[string]int, len(deps))
	dependents := make(map[string][]string, len(deps))
	for n, ds := range deps {
		indegree[n] += 0
		for _, d := range ds {
			indegree[n]++
			dependents[d] = append(dependents[d], n)
		}
	}
	var ready []string
	for n, deg := range indegree {
		if deg == 0 {
			ready = append(ready, n)
		}
	}
	slices.Sort(ready)
	var out []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, m := range dependents[n] {
			indegree[m]--
			if indegree[m] == 0 {
				i, _ := slices.BinarySearch(ready, m)
				ready = slices.Insert(ready, i, m)
			}
		}
	}
	if len(out) != len(indegree) {
		var cyclic []string
		for n, deg := range indegree {
			if deg > 0 {
				cyclic = append(cyclic, n)
			}
		}
		slices.Sort(cyclic)
		return nil, fmt.Errorf("dependency cycle among %v", cyclic)
	}
	return out, nil
}
