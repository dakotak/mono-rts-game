# Entities and components

Source: [`internal/sim/world.go`](../../internal/sim/world.go),
[`component.go`](../../internal/sim/component.go),
[`types.go`](../../internal/sim/types.go),
[`ids.go`](../../internal/sim/ids.go).

## Entities

```go
type Entity struct { ID EntityID; Kind Kind; Owner PlayerID; Pos Pos; Passable bool }
```

- **Kinds** are a closed kernel enum: `sim.KindUnit`, `sim.KindResource`,
  `sim.KindTerrain`. Plugins can't add kinds. They give meaning through
  components instead: a "well" is a `KindResource` entity with a
  `wells.well` component, and a wreck is a `KindResource` entity with a
  `salvage.wreck` component.
- **Owner** is a `sim.PlayerID`; 0 means unowned.
- **Position and occupancy.** At most one **blocking** entity per tile,
  indexed in a dense grid (`World.EntityAt`). **Passable** entities
  (`World.CreatePassable`: wreckage, passable terrain) don't occupy the
  tile and are listed per tile (`World.PassableAt`).

### Lifecycle

| Step | API | Notes |
|---|---|---|
| Create | `World.CreateEntity(kind, owner, pos)`, `World.CreatePassable(...)` | Fails out of bounds or on an occupied tile (blocking only). Every `OnCreate` hook runs immediately, in plugin order; that's how units get energy stores and attributes. |
| Move | `World.MoveEntities([]Move)` | Applies a batch atomically and simultaneously. A unit may enter a tile another unit in the batch is leaving. Fails as a whole on conflict. |
| Remove | `World.RemoveEntity(id)` | Removes the entity and **all of its components**. Its ID is reported in the frame's `Removed`. |

Destruction in combat, depleted wrecks and admin deletions all go through
`RemoveEntity`. Any plugin that keeps references to other entities (a
target, a receiver, a well) must handle them disappearing. They all
re-check with `World.Entity(id)` before use.

### IDs

Entity IDs are `HMAC-SHA256(id_key, "entity" ‖ n)` truncated to 64 bits
(`sim.DeriveID`), skipping 0 and live IDs.

- Without the key, IDs reveal no counts and no creation order.
- With it, they're reproducible, so replays are exact.
- Player IDs use the domain `"player"`, truncated to 32 bits.
- **Consequence:** ID order is fixed but unrelated to creation order. Code
  must never assume IDs are small, dense or increasing.

## Components

Plugin data is stored in typed components:

```go
var healths = sim.NewComponent[Health]("combat.health") // ID must be "<plugin>.<name>"

r.Component(healths.Def(sim.VisibleOwner, func(h Health) []sim.Field { ... }))

healths.Set(w, id, Health{...})
h, ok := healths.Get(w, id)
healths.Has(w, id)
healths.Remove(w, id)
for _, id := range healths.Entities(w) { ... } // sorted; do not modify the slice
```

Rules (enforced by the type system or by `Registrar`):

- **`T` must be `comparable`**, and should be a plain value type: no
  pointers, slices or maps. Values are copied, hashed with `%#v` in
  `StateHash`, and compared to skip no-op writes. Fixed-size arrays are
  fine; `movement` stores a 32-waypoint route as an array.
- **IDs must be prefixed** with the plugin name.
- **Writing an equal value doesn't mark the entity dirty**, so unchanged
  state causes no network traffic.
- **Using an unregistered component panics.** `Component.Registered(w)`
  checks whether a component exists in this ruleset, which helps for
  optional integrations.

Storage is `map[componentID]map[EntityID]any`, with a cached sorted
membership list per component. There's no ECS framework.

### Attributes: how component data becomes visible

`Component.Def(vis, fn)` declares how a value is shown to clients. `fn`
maps a value to named fields (`sim.Field{Key, Value}`). Each field becomes
an `Attr` tagged with the component's `Visibility`:

| Visibility | Who sees it |
|---|---|
| `sim.VisibleAdmin` | Only `AdminService` |
| `sim.VisibleOwner` | The owner and admins |
| `sim.VisiblePublic` | Everyone (subject to fog-of-war disclosure for foreign entities when fog is on) |

`fn` must be a **pure function of the value**. The kernel memoizes its
output per value (bounded to 4096 entries per component). A component
defined with `fn == nil` is internal: never shown.

Values (`sim.Value`) are a small tagged union: `IntValue`, `BoolValue`,
`StringValue`, `PosValue` and `RangeValue` (an estimate). Entity IDs in
attribute and event values are sent as **decimal strings**, because the
wire's `int_value` is signed.

## Views

- `World.View(id)` builds an `EntityView`: core fields plus every
  attribute from every component, tagged with visibility.
- `World.ScratchView(id)` is the same view in a reused buffer, for callers
  that copy what they keep.
- The kernel never filters. The server filters per viewer, and the
  observation authority filters foreign entities under fog.

## Globals (world-scoped state)

State that belongs to the match rather than to an entity uses a
**global**:

```go
var status = sim.NewGlobal("match.status", Status{}) // plugin-prefixed ID, initial value
r.Global(status.Def())
st := status.Get(w)
status.Set(w, st) // equal values are no-ops
```

Globals follow the same rules as components:

- `T` is a plain comparable value (fixed-size arrays are fine).
- They're part of `StateHash` and replay.
- Changes are reported in `Frame.Globals`; the server reads the match
  status from there.

Examples: `match.status` (lifecycle, players, statistics, scores) and the
`koth.hill` example.

## Derived properties

A derived property is a value computed on demand rather than stored:

```go
r.Derived(sim.DerivedDef{Name: "energy.transfer_rate", Compute: func(w *sim.World, id sim.EntityID) (int64, bool) { ... }})
v, ok := w.Derived("energy.transfer_rate", id)
```

Mechanics read rates through derived properties (`movement.ticks_per_move`,
`energy.extraction_rate`, `energy.transfer_rate`, `logistics.range`,
`attributes.<name>`), so configuration and upgrades can vary them per
unit. See [attributes.md](attributes.md).

## State hash

`World.StateHash()` is an FNV-64a hash over:

- the tick, the ID counter, the RNG state and the world size
- every entity's core fields, in ID order
- every component value, in component-ID order then entity-ID order

It's the basis of every determinism and replay test.
