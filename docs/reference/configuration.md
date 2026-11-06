# Configuration reference

Every option the server reads, with defaults and valid ranges, taken from
the parsing and validation code. The guide is
[getting-started/configuration.md](../getting-started/configuration.md).

General rules:

- JSON, decoded with unknown fields rejected
  ([`game.LoadConfig`](../../internal/game/game.go),
  [`sim.Registrar.DecodeConfig`](../../internal/sim/plugin.go)).
- A plugin section is decoded **over the plugin's defaults**: fields you
  omit keep their defaults.
- All quantities are integers (`int64` unless noted). Formulas are strings
  in the [formula language](formulas.md).
- "Property" means an attributes-plugin attribute or derived property name
  (for example `"armor"` or `"ticks_per_move"`). Binding a stat to a
  property that doesn't exist is a startup error.

## Top level

| Field | Type | Default | Rules |
|---|---|---|---|
| `world` | object | required | See below. |
| `players` | array | `[]` | Player slots. |
| `admin_token` | string | `""` | Bearer token for `AdminService`. Empty disables the admin API (`PermissionDenied`). Must differ from every join token. |
| `units` | array | `[]` | Initial unit blocks. |
| `plugins` | object | `{}` | Plugin name → section. A section for an unknown plugin is an error. |

### `world`

| Field | Type | Default | Rules |
|---|---|---|---|
| `width`, `height` | int32 | — | > 0. |
| `seed` | uint64 | 0 | Seeds `World.Rand()` (gameplay randomness, e.g. movement contention backoff). |
| `tick_rate_hz` | uint32 | — | 1..1000. The tick interval is `1s / tick_rate_hz`. |
| `id_key` | hex string | random | ≥ 16 bytes (32 hex characters). Secret: derives entity and player IDs. Set it only for reproducible runs. |
| `unit_command_budget` | int | 0 | ≥ 0. Unit-commands each player may execute per tick; 0 = unlimited. Admin commands are never charged. |

### `players[]`

| Field | Type | Rules |
|---|---|---|
| `slot` | int | > 0, unique. Config-local handle only, never exposed. |
| `name` | string | Display name returned by `Connect`. |
| `join_token` | string | Non-empty, unique. Presenting it to `Connect` plays this slot. |

### `units[]`

| Field | Type | Default | Rules |
|---|---|---|---|
| `owner` | int | 0 | A configured slot, or 0 for unowned. |
| `x`, `y` | int32 | — | Top-left tile. Every tile must be in bounds and free. |
| `w`, `h` | int32 | 1 | Block size; values < 1 count as 1. |

## Plugin sections

Plugins in registration order (the order printed at startup):
`attributes`, `metadata`, `terrain`, `visibility`, `wells`, `energy`,
`logistics`, `movement`, `upgrades`, `cloning`, `combat`, `salvage`,
`automation`. The order comes from `DependsOn` with ties broken by name.

### `attributes` ([source](../../internal/plugins/attributes/attributes.go))

| Field | Type | Default | Rules |
|---|---|---|---|
| `base_mass` | int64 | 0 | Mass of a unit before attribute contributions. |
| `attributes` | map name → attribute | `{}` | At most 16. Name: `^[a-z][a-z0-9_]{0,31}$`. |
| `derived` | map name → formula | `{}` | At most 32. Variables: attribute names, `mass`, other derived names. Cycles and unknown names are rejected. A derived name may not equal an attribute name. |

Each attribute:

| Field | Type | Default | Rules |
|---|---|---|---|
| `default`, `min`, `max` | int64 | 0 | `0 ≤ min ≤ default ≤ max`, `max − min ≤ 1000`. |
| `mass` | formula of `v` | `0` | Contribution to mass at level `v`. |
| `cost` | formula of `v` | `0` | **Cumulative** energy to reach `v`. Non-decreasing over `[min, max]`. |
| `duration` | formula of `v` | `0` | **Cumulative** ticks to reach `v`. Non-decreasing. |
| `description` | string | — | Shown to clients. |

### `metadata` ([source](../../internal/plugins/metadata/metadata.go))

| Field | Default | Rules |
|---|---|---|
| `max_entries` | 8 | 1..8 entries per unit. |
| `max_key_length` | 32 | 1..64. Keys match `^[a-z0-9][a-z0-9_.-]*$`. |
| `max_value_length` | 64 | 0..256, printable ASCII. |
| `max_batch_units` | 4096 | ≥ 1. Upper bound on units a selector yields. |

### `terrain` ([source](../../internal/plugins/terrain/terrain.go))

| Field | Rules |
|---|---|
| `types` | Map name → `{ "passable": bool, "cost": int }`. Name: `^[a-z][a-z0-9_]{0,31}$`. Passable types: `cost` 0..20 (0 means 1), multiplies the step cooldown. Impassable types must not set `cost`. |
| `tiles` | Array of `{ "type", "x", "y", "w", "h" }` rectangles (`w`, `h` default 1). The type must exist. Impassable tiles must be free; they block like units. |

### `visibility` ([source](../../internal/plugins/visibility/visibility.go))

| Field | Default | Rules |
|---|---|---|
| `enabled` | `false` | Turns fog of war on. Off: every player sees everything. |
| `vision` | `{ "default": 5 }` | Stat (`property` and/or `default`); default 0..32 tiles. |
| `sensor` | `{ "default": 5 }` | Stat. |
| `signature` | `{ "default": 0 }` | Stat (stealth: higher is harder to detect). |
| `identification` | `{ "default": 2 }` | Stat. |
| `resource_signature` | 0 | Signature of wells and wreckage. |
| `detection_formula` | `observer_sensor - target_signature - distance` | Variables: `observer_<p>`, `target_<p>`, `distance`; `<p>` = `vision`, `sensor`, `signature`, `identification` or a property. |
| `detection_threshold` | 0 | Detected when the formula is ≥ this. |
| `identification_formula` | `observer_identification - distance` | Same variables. |
| `estimated_threshold` | 0 | ≤ `detailed_threshold`. |
| `detailed_threshold` | 3 | |
| `distance_metric` | `chebyshev` | `chebyshev`, `manhattan` or `euclidean` (rounded to the nearest integer). |
| `occlusion` | `none` | Only `none` is supported. |
| `disclosure` | `[]` | Rules `{ "key", "basic", "estimated", "detailed" }`. The key is an attribute key or ends in `.*`. Precision per level: 0 hidden, 1 exact, N → a range of width N. Higher levels must be at least as precise, and at least one level must disclose. |

### `wells` ([source](../../internal/plugins/wells/wells.go))

| Field | Default | Rules |
|---|---|---|
| `defaults` | `{ "energy": 1000, "capacity": 1000, "extraction_rate": 5, "unlimited": false }` | `0 ≤ energy ≤ capacity`, `extraction_rate ≥ 0`. |
| `wells` | `[]` | `{ "x", "y" }` plus optional overrides of any default field. Placed in `Init`, after units. |

### `energy` ([source](../../internal/plugins/energy/energy.go))

| Field | Default | Rules |
|---|---|---|
| `unit_capacity` | 100 | ≥ 0. Storage of new units (unless `capacity_attribute`). |
| `initial_energy` | 50 | 0..`unit_capacity`. |
| `extraction_rate` | 2 | ≥ 0. Mining per tick per unit (unless `extraction_attribute`). |
| `transfer_rate` | 5 | ≥ 0. Per tick per sender; also used by logistics. |
| `capacity_attribute` | — | Property that sets capacity; kept in sync as it changes. |
| `extraction_attribute` | — | Property that sets the mining rate. |

### `logistics` ([source](../../internal/plugins/logistics/logistics.go))

| Field | Default | Rules |
|---|---|---|
| `range` | `{ "default": 1 }` | Supply range (Chebyshev). `default` 1..16. A bound `property` is clamped to 1..16. |

### `movement` ([source](../../internal/plugins/movement/movement.go))

| Field | Default | Rules |
|---|---|---|
| `ticks_per_move` | 1 | ≥ 1. Cooldown after a step (unless `pace_attribute`). |
| `max_blocked_ticks` | 20 | ≥ 0. Consecutive failed steps before the order is cancelled. |
| `contention_backoff_ticks` | 2 | ≥ 0. Random extra wait after losing a contested tile. |
| `energy_per_step` | 0 | ≥ 0. Energy per successful step. |
| `pace_attribute` | — | Property for ticks per step (`max(1, value)`). |
| `pathfinding` | `auto` | `auto` (greedy steps, A* when blocked) or `always`. |
| `max_path_nodes` | 4096 | ≥ 16. Node expansions per plan. |
| `plans_per_tick` | 128 | ≥ 1. Plans per tick across all units. |
| `replan_after_blocked` | 3 | ≥ 1. Blocked steps before replanning around nearby units. |

### `upgrades` ([source](../../internal/plugins/upgrades/upgrades.go))

| Field | Default | Rules |
|---|---|---|
| `progress_event_interval` | 10 | ≥ 0. Ticks between `upgrades.progress` events (0 = none). |

### `cloning` ([source](../../internal/plugins/cloning/cloning.go))

| Field | Default | Rules |
|---|---|---|
| `base_cost` | 50 | Variable `base` in the formulas. |
| `cost` | `base + embodied` | Variables `base`, `embodied`, `mass`. The effective cost is `max(cost, embodied)`. |
| `duration` | `20 + embodied / 20` | Same variables. The effective duration is `max(1, duration)`. |
| `offspring_energy` | 10 | Energy moved from parent to child. |
| `progress_event_interval` | 10 | Ticks between `cloning.progress` events. |

### `combat` ([source](../../internal/plugins/combat/combat.go))

| Field | Default | Rules |
|---|---|---|
| `max_health` | `{ "default": 100 }` | Stat; default ≥ 1. |
| `attack` | `{ "default": 10 }` | Stat; default ≥ 0. |
| `range` | `{ "default": 1 }` | Stat; default ≥ 0 (Chebyshev). |
| `cooldown` | `{ "default": 5 }` | Stat; default ≥ 1 tick. |
| `damage` | `attacker_attack` | Variables `attacker_<p>`, `target_<p>`, `distance`. Built-ins `<p>`: `attack`, `range`, `cooldown`, `max_health`, `health`, `energy`; anything else is a property. |
| `energy_cost` | `0` | Energy per attack; only `attacker_<p>` variables. |

### `salvage` ([source](../../internal/plugins/salvage/salvage.go))

| Field | Default | Rules |
|---|---|---|
| `embodied_recovery_permille` | 500 | 0..1000. Share of `min(embodied, invested)` left in the wreck. |
| `stored_recovery_permille` | 500 | 0..1000. Share of stored energy left in the wreck. |
| `salvage_rate` | `{ "default": 5 }` | Stat; ≥ 0 per tick. |

### `automation` ([source](../../internal/plugins/automation/automation.go))

| Field | Default | Rules |
|---|---|---|
| `eval_interval` | 5 | 1..100. A unit is evaluated when `(id + tick) % eval_interval == 0`. |
| `max_evaluations_per_tick` | 2000 | ≥ 1. The rest are deferred to their next slot. |

## Operational limits (compiled in)

| Limit | Value | Where |
|---|---|---|
| Units per command | 4096 | `energy.MaxUnitsPerCommand`, `combat.MaxUnitsPerCommand`, automation and logistics |
| Command ID length | 1..128 bytes | `internal/server` (`maxCommandIDLen`) |
| Remembered acknowledgments per player (idempotency window) | 4096 | `internal/server` (`ackHistory`) |
| Frames an observer may lag before resync | 128 | `internal/server/hub.go` (`subscriberBuffer`) |
| Admin step request | 1..1000 ticks | `sim.MaxStepRequest` |
| HTTP request body (gateway) | 1 MiB | `internal/gateway` (`maxBody`) |
| Formula size | 512 chars, 128 nodes, depth 32 | `internal/expr` |
| Attributes / derived / levels | 16 / 32 / 1000 | `internal/plugins/attributes` |
| Vision range | 32 | `visibility.MaxVision` |
| Automation radius / follow distance | 16 / 8 | `automation.MaxRadius`, `MaxFollowDist` |
| Supply links per supplier / supply range | 4 / 16 | `logistics.MaxLinks`, `MaxRange` |
| Path waypoints stored per order | 32 | `movement.PathLen` |
| Terrain cost | 20 | `terrain.MaxCost` |
