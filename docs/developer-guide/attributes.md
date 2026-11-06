# Attributes and derived properties

"Attribute" means two related things in Mono:

1. **Observable attributes:** the key/value pairs on an entity view
   (`energy.stored`, `combat.health`, ...), produced by components (see
   [entities.md](entities.md#attributes-how-component-data-becomes-visible)).
2. **Unit attributes:** the configurable, upgradable stats managed by the
   **attributes plugin** ([`internal/plugins/attributes`](../../internal/plugins/attributes/attributes.go)),
   such as armor and propulsion. Their levels appear as observable
   attributes `attributes.<name>`.

This page is about the second kind.

## Model

The attributes plugin keeps a registry of attribute definitions from
config:

- default, min, max
- `mass`, cumulative `cost` and cumulative `duration` formulas of `v`

Each unit stores its current levels in the `attributes.unit` component
(`attributes.Unit{Values [16]int64, Invested int64}`), attached to every
new unit by an `OnCreate` hook with every attribute at its default.

At registration the plugin evaluates `cost` and `duration` over
`[min, max]` into tables. That validates that they're non-decreasing and
makes lookups O(1). It publishes the tables (`cost_table`,
`duration_table`) in its rules description so clients can show exact
prices.

| Concept | Definition |
|---|---|
| Upgrade price `a → b` | `cost(b) − cost(a)` energy and `max(1, duration(b) − duration(a))` ticks (`Plugin.UpgradeCost`). |
| Embodied | `Σ cost(level) − cost(default)` over attributes (`Plugin.Embodied`). |
| Invested | Energy actually paid: completed upgrades plus the embodied investment a clone inherits. Admin overrides don't add to it. |
| `mass` | Built in: `base_mass + Σ mass(level)`. |
| Derived | Config formulas over attributes, `mass` and other derived properties, evaluated in dependency order. Cycles and unknown names are rejected at startup. |

Derived properties are computed on demand, never stored, so there's
nothing to invalidate.

## Go API for other plugins

Plugins that build on attributes receive the **shared instance** through
their constructor. `game.Plugins()` creates one `attributes.New()` and
passes it to energy, movement, combat, salvage, visibility, logistics,
upgrades and cloning: explicit composition, not a global lookup.

| Method | Use |
|---|---|
| `Has(name) bool` | Validate a config binding at registration time. |
| `Lookup(w, id, name) (int64, bool)` | Current value of an attribute or derived property of a unit (the usual way to read a stat). |
| `Names()`, `Index(name)`, `Bounds(i)` | Introspection. |
| `Get(w, id)`, `Property(u, name)` | Read a unit's whole record or one property. |
| `OnChange(fn)` | Be notified when a unit's levels change. Energy uses it to keep capacity in sync. |
| `Apply(w, id, i, value, invested)` | Set a level and add to invested (used by upgrades and admin overrides). |
| `Inherit(w, child, parent)` | Copy levels to a clone. |
| `Reserve(w, id, kind)`, `Release`, `Busy` | Exclusive "project" slot. One upgrade or clone per unit. |
| `UpgradeCost(i, from, to)`, `Embodied(u)` | Pricing. |

The plugin also registers every attribute and derived property as a
kernel derived property named `attributes.<name>`, so code that only has
a `*sim.World` can read them with `w.Derived("attributes.armor", id)`.

## Binding pattern

Mechanics read attributes **only where configured to**, so a config
without attributes behaves exactly as before:

| Binding | Plugin |
|---|---|
| `energy.capacity_attribute`, `energy.extraction_attribute` | energy |
| `movement.pace_attribute` | movement |
| `combat.{max_health,attack,range,cooldown}.property` | combat |
| `salvage.salvage_rate.property` | salvage |
| `visibility.{vision,sensor,signature,identification}.property` | visibility |
| `logistics.range.property` | logistics |

A stat is typically declared as `{ "property": "...", "default": N }`.
When `property` is set it must name an existing attribute or derived
property (checked in `Register` with `attrs.Has`); otherwise the default
is used. The [plugin guide](plugins/attributes-and-formulas.md) shows how
to add such a binding to your own plugin.

## Formulas

All formulas use [`internal/expr`](../../internal/expr/expr.go):

- integers only
- `+ - * / %`, `min`, `max`, `clamp`, `abs`, `pow`, `sqrt`
- total semantics: division by zero yields 0, and results saturate

See the [formula reference](../reference/formulas.md).

## Upgrades and cloning

The upgrade and cloning plugins are thin layers over this model: they
reserve the project slot, pay from the unit's store up front, count ticks,
then `Apply` or `Inherit`. Rules are in
[mechanics.md](mechanics.md#unit-evolution-milestone-3).
