# Attributes and formulas in plugins

## Showing state as attributes

Any component can be observable:

```go
var stats = sim.NewComponent[myStats]("myplugin.stats")

r.Component(stats.Def(sim.VisibleOwner, func(s myStats) []sim.Field {
    return []sim.Field{
        {Key: "myplugin.charge", Value: sim.IntValue(s.Charge)},
        {Key: "myplugin.mode", Value: sim.StringValue(s.Mode)},
    }
}))
```

- **Keys** should be `<plugin>.<name>`. Clients key their UI on them.
- **The attribute function must be pure** (the kernel memoizes it per
  value) and cheap.
- **Change only what changed.** A `Set` with an equal value is a no-op, so
  stable values cost nothing on the network. A value that changes every
  tick for every unit (a counter, say) makes every unit "changed" every
  tick. Prefer storing the tick when something will happen
  (`combat.ready_at`) over a countdown.

## Adding an upgradable attribute

You don't write code for this. Upgradable attributes are **defined by
configuration** in the attributes plugin, and upgrades and cloning work
for any attribute automatically:

```json
"attributes": {
  "attributes": {
    "solar": { "default": 1, "min": 0, "max": 5, "mass": "v", "cost": "v * v * 20", "duration": "v * 10",
               "description": "Energy regenerated per step (regeneration plugin)." }
  }
}
```

Your plugin then **binds** to it by name. The standard binding shape is
"constant or property":

```go
type Stat struct {
    Property string `json:"property,omitempty"`
    Default  int64  `json:"default"`
}

// Register: validate the binding exists
if s.Property != "" && !p.attrs.Has(s.Property) {
    return fmt.Errorf("myplugin stat.property: %q is not an attribute or derived property", s.Property)
}

// Tick: read it
func (p *Plugin) stat(w *sim.World, id sim.EntityID, s Stat) int64 {
    if s.Property != "" {
        v, _ := p.attrs.Lookup(w, id, s.Property)
        return v
    }
    return s.Default
}
```

The example plugin's `amount_attribute` is a simpler variant (one optional
property name). Clamp bound values to your mechanic's valid range;
attribute values are only bounded by their own min and max.

The attributes plugin must be **the shared instance**: accept
`*attributes.Plugin` in your constructor and pass the one from
`game.Plugins()`. A second `attributes.New()` would be a different
registry, and `NewRuleset` would reject the duplicate plugin name anyway.

## Derived properties

Declare a value that's computed on demand rather than stored:

```go
r.Derived(sim.DerivedDef{
    Name: "myplugin.rate",
    Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
        // return false when the property doesn't apply to this entity
        ...
    },
})
// anywhere in a tick:
rate, ok := w.Derived("myplugin.rate", id)
```

Use derived properties for **rates and stats other code may read**.
Built-in examples: `movement.ticks_per_move`, `energy.extraction_rate`,
`energy.transfer_rate`, `logistics.range`. `Compute` must be
deterministic and depend only on world state.

**Config-defined derived properties** (formulas over attributes and
`mass`) belong in the attributes plugin's `derived` section. The
attributes plugin exposes each as `attributes.<name>`, and your plugin
binds to it like any attribute:

```json
"derived": { "solar_output": "solar * 2 + propulsion / 4" }
```

## Formulas in your own config

To let operators configure a formula, compile it with
[`internal/expr`](../../../internal/expr/expr.go) at registration and
evaluate it in ticks. The pattern used by combat, visibility and cloning:

```go
e, err := expr.Parse(cfg.Formula)                    // *expr.Expr; syntax and size limits
if err != nil { return fmt.Errorf("myplugin formula: %w", err) }
// Compile against a fixed variable layout; unknown variables are errors.
prog, err := e.CompileVars("base", "level")          // or e.Compile(func(name string) (slot int, ok bool))
if err != nil { return fmt.Errorf("myplugin formula: %w (available: base, level)", err) }
...
v := prog.Eval([]int64{base, level})                 // *expr.Program; total: never fails, never panics
```

Read the call sites before copying: `cloning.go` (fixed variables `base`,
`embodied`, `mass`), `combat.go` (`attacker_*`, `target_*`, `distance`)
and `visibility.go` (`observer_*`, `target_*`, `distance`). They show how
to map variable names to slots and validate them with helpful errors. The
language itself is documented in the
[formula reference](../../reference/formulas.md).

## Publishing rules

`r.Describe(v)` makes your effective configuration (and anything
derived from it, like tables) public in `WorldInfo.rules_json`. The
attributes plugin publishes per-level cost and duration tables, so
clients show exact prices without evaluating formulas.
