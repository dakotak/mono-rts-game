# Example plugins

## The example: `regeneration`

[`examples/plugins/regeneration`](../../../examples/plugins/regeneration/regeneration.go):
idle units regenerate energy up to a share of their capacity, and players
can opt units out.

| | |
|---|---|
| Config | `enabled` (default `false`), `amount`, `amount_attribute`, `interval`, `limit_percent`, `idle_only` |
| Component | `regeneration.opt_out`, owner-visible as `regeneration.disabled = true` |
| Derived property | `regeneration.amount` |
| Phase and system | `regeneration` (after `energy`, before `movement`): `regeneration.step` |
| Command | `rts.examples.regeneration.v1.SetRegenerationCommand{unit_ids, enabled}`, batch-capable |
| Event | `regeneration.regenerated {amount, stored}` to the owner |
| Depends on | `energy`, `movement`, `attributes` |
| Tests | 10 tests and a benchmark ([`regeneration_test.go`](../../../examples/plugins/regeneration/regeneration_test.go)) |
| Registered by default | **No.** See [tutorial step 12](getting-started.md#12-register-the-plugin-with-the-server). |

Regeneration **creates** energy, unlike every built-in mechanic except
admin grants and the starting energy of spawned units. That's a
deliberate rule of the example. If you register it, worlds no longer
conserve energy.

Measured with the example's benchmark: about 0.13 ms per tick for
10,000 units with `interval: 10` (`BenchmarkStep10k`, Apple M-series),
mostly the kernel's own per-tick work.

### Why energy and not health?

The obvious "regeneration" mechanic restores **health**. That isn't
possible from another plugin today: combat exports `GetHealth` but no
setter, and plugins must not modify another plugin's components except
through its exported API. Health regeneration would need a small addition
to `internal/plugins/combat` (an exported, validated health setter). This
example doesn't make that engine change; it uses the energy plugin's
exported `GetStore` and `SetStored` instead.

## Built-in plugins worth reading

In rough order of complexity:

| Plugin | Read it for |
|---|---|
| [`terrain`](../../../internal/plugins/terrain/terrain.go) | The smallest complete plugin: config, a public component, an `Init` that places entities, passable vs. blocking entities, an exported helper (`StepCost`). |
| [`wells`](../../../internal/plugins/wells/wells.go) | Admin-only commands, config defaults with per-item overrides, `Init`. |
| [`metadata`](../../../internal/plugins/metadata/metadata.go) | Owner-only data, selectors, **batch commands** via `TickContext.ApplyToUnit`. |
| [`energy`](../../../internal/plugins/energy/energy.go) | `OnCreate` hooks, persistent orders, fair splits with `fairshare`, attribute bindings, exported APIs for other plugins. |
| [`upgrades`](../../../internal/plugins/upgrades/upgrades.go), [`cloning`](../../../internal/plugins/cloning/cloning.go) | Projects over many ticks, up-front payment, configurable formulas, progress events. |
| [`logistics`](../../../internal/plugins/logistics/logistics.go) | Snapshot-based simultaneous allocation, cycle rejection at command time, reusable scratch buffers. |
| [`automation`](../../../internal/plugins/automation/automation.go) | State machines that drive other plugins' orders through their exported setters, bounded and staggered evaluation, honoring fog of war through an injected `Detected` hook. |
| [`combat`](../../../internal/plugins/combat/combat.go) | Simultaneous resolution, `PhaseDef.Before`, formulas with per-side variables, destruction callbacks (`OnDestroyed`). |
| [`movement`](../../../internal/plugins/movement/movement.go) | Conflict resolution with all-fail contention, chains and cycles, seeded backoff, bounded A* pathfinding, partial-success group commands. |
| [`visibility`](../../../internal/plugins/visibility/visibility.go) | The observation authority: per-player state, disclosure, the hardest information-hiding problem in the codebase. |

The kernel's own tests
([`internal/sim/sim_test.go`](../../../internal/sim/sim_test.go)) define
tiny throwaway plugins inline. They're useful as minimal templates.

## Plugin ideas that fit the current API

- **Upkeep:** units pay energy per tick for some attribute (via
  `energy.Spend`), and stop or degrade when they can't.
- **Beacons:** a terrain-like passable entity placed in `Init` that boosts
  a derived property for nearby units.
- **Scoring:** an admin-visible component tracking territory, with
  `Describe` publishing the scoring rules.

Ideas that need engine changes first: health regeneration or healing (no
health setter), new entity kinds, new RPCs, runtime-loaded rules.
