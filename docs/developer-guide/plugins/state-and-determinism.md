# State safety and determinism rules for plugins

Every plugin runs inside the authoritative simulation. Break one of these
rules and the server keeps running, but replays diverge, players see
different worlds after reconnecting, or hidden information leaks. The
compiler enforces none of them; reviews and tests must.

## 1. Mutate authoritative state only on the simulation goroutine, through supported APIs

- Mutate only inside command handlers, systems, `Init` and `OnCreate`.
- **Never start goroutines** that touch the `World` or your components,
  and never keep a `*sim.World` for later use outside a tick.
- Mutate through `World` methods, component handles and other plugins'
  exported functions. Never through unexported state you reached by other
  means.
- "Unscheduled" mutation means any change not caused by a scheduled
  command or a system in the current tick, such as a timer, an RPC handler
  or a background job. **It is forbidden.** Admin edits are commands too.

## 2. Don't depend on Go map iteration order

```go
// WRONG: outcome depends on randomized map order
for id, o := range p.pending { apply(id, o) }

// RIGHT: sorted IDs
for _, id := range orders.Entities(w) { ... }       // sorted
for _, id := range w.EntityIDs() { ... }            // sorted
ids := slices.Sorted(maps.Keys(m))                  // if you must use a map
```

Maps are fine for lookups and for accumulating values that are then
applied in a sorted pass (logistics' per-unit deltas).

## 3. No wall-clock time

Don't call `time.Now`, timers or tickers inside a tick. Use `ctx.Tick` or
`w.Tick()`, and store **ticks** in components (`ReadyAt sim.Tick`).

## 4. No uncontrolled randomness

Use `w.Rand()` (`Uint64()`, `Intn(n)`) only. Never `math/rand`,
`math/rand/v2` or `crypto/rand` in simulation code. Every draw advances
shared state that's included in `StateHash`, so draw in a deterministic
order: inside a sorted loop, a fixed number of times.

Prefer deterministic schedules where you can, as the example plugin does:

```go
if (uint64(ctx.Tick)+uint64(id))%interval == 0 { ... } // staggered, no RNG needed
```

## 5. Resolve conflicts deterministically

When several actors compete for one resource in a tick:

1. Compute every claim from **one snapshot** (the state at the start of
   your phase).
2. Resolve with an **explicit** rule, either symmetric (all fail, fair
   split) or with a stated tie-break (ID, then position).
3. Apply the results together.

Reuse [`internal/fairshare`](../../../internal/fairshare/fairshare.go)
(`fairshare.Split`) for dividing a quantity: it's max-min fair, ties go
by ID, and it never over-allocates. Don't let "whoever the loop reached
first" decide outcomes, unless that order is the documented rule (as in
clone placement).

## 6. Integer math, explicit rounding

- All quantities are `int64`. No `float32` or `float64` in simulation
  logic.
- Choose rounding on purpose: Go's `/` truncates toward zero, and
  `x * permille / 1000` floors for non-negative `x`. Watch for overflow
  where products can grow (the formula language saturates; your Go code
  doesn't).
- Write the rounding rule in your plugin's doc comment.

## 7. Bounded per-tick work

- Cap anything players control: units per command (built-ins use 4096),
  radii, list lengths, links per unit.
- Expensive work needs a per-tick budget and a fair, deterministic
  rotation (`movement.plans_per_tick`,
  `automation.max_evaluations_per_tick`).
- Avoid O(n²) scans. Use the occupancy grid (`World.EntityAt`), component
  membership lists, or bounded neighborhood scans.
- Benchmark at scale (see [testing.md](testing.md#benchmarking)).

## 8. Ownership and authorization

- Check ownership **at execution time** (`e.Owner == issuer.Player ||
  issuer.Admin`).
- Systems acting on behalf of a player (automation, standing orders) must
  apply the same checks when they resolve references each tick: a
  receiver must still be the owner's unit.

## 9. Handle destruction and removal

Any entity can vanish between ticks: destroyed, depleted, deleted by an
admin. For each stored reference:

```go
if _, ok := w.Entity(o.Target); !ok {
    // end the order with a reason event; never panic, never use stale data
}
```

Removal drops the removed entity's own components. References **to** it
held by other entities are yours to clean up.

## 10. No unauthorized disclosure

- **Attributes:** `sim.VisibleOwner` unless the information is public.
- **Events:** `sim.ToPlayer`. `sim.ToAll()` bypasses fog of war.
- **Errors:** `sim.Unavailable` for anything not observable.
- **Decisions on a player's behalf** use only what that player observes.

See [commands-and-events.md](commands-and-events.md#preventing-hidden-information-leaks).

## 11. Keep state in components

Plugin struct fields are for configuration and scratch buffers. Anything
that affects future ticks must be in a component, because only
components:

- are included in `StateHash`
- are replayed (the replay starts from the initial state and re-runs
  commands)
- are visible to tooling and the admin client
- vanish correctly when an entity is removed

Component values must be `comparable` plain values: no pointers, slices or
maps. Use fixed-size arrays for small bounded lists (`movement` stores
its route in `[32]sim.Pos`).

## How your plugin affects replays and hashes

- **The component set is part of `StateHash`.** Registering a plugin with
  a component changes every hash, even while the mechanic is disabled. So
  hashes are comparable only between identical builds and
  configurations.
- **Replays reproduce exactly** only if your plugin follows the rules
  above. A replay test (run, re-run, replay, compare hashes each tick)
  catches violations; see [testing.md](testing.md#deterministic-outcomes-and-replay).
- **Changing your plugin's logic invalidates old command logs.** There's
  no versioning of behavior.
