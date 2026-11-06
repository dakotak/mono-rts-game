# Formulas and attributes reference

Every configurable formula is written in one small language,
implemented in [`internal/expr`](../../internal/expr/expr.go).

## Language

```
expr     = sum
sum      = product { ("+" | "-") product }
product  = unary { ("*" | "/" | "%") unary }
unary    = "-" unary | primary
primary  = integer | identifier | call | "(" expr ")"
call     = identifier "(" expr { "," expr } ")"
```

- **Identifiers:** `[a-z_][a-z0-9_]*`. Which identifiers are allowed
  depends on where the formula is used (tables below). Unknown names are
  rejected at startup.
- **Integers only** (int64). No floating point, strings or booleans.

| Function | Meaning |
|---|---|
| `min(a, b, ...)`, `max(a, b, ...)` | Two or more arguments |
| `clamp(x, lo, hi)` | `min(max(x, lo), hi)` |
| `abs(x)` | Absolute value |
| `pow(x, n)` | Power; `n < 0` → 0 |
| `sqrt(x)` | Floor square root; `x < 0` → 0 |

### Semantics: total, so evaluation never fails

- `/` and `%` truncate toward zero (like Go). **Division or modulo by
  zero gives 0.**
- Every operation **saturates** at the int64 range instead of
  overflowing.
- Results are identical on every platform.

Limits: 512 characters, 128 nodes and nesting depth 32, checked at
parse time.

## Where formulas are used

| Config field | Variables | Notes |
|---|---|---|
| `attributes.attributes.<a>.mass` | `v` (the level) | Contribution to `mass`. |
| `attributes.attributes.<a>.cost` | `v` | **Cumulative** energy to reach level `v`; must not decrease over `[min, max]`. |
| `attributes.attributes.<a>.duration` | `v` | **Cumulative** ticks to reach `v`; must not decrease. |
| `attributes.derived.<d>` | attribute names, `mass`, other derived names | Evaluated in dependency order; cycles are rejected. |
| `cloning.cost`, `cloning.duration` | `base`, `embodied`, `mass` | Effective cost `max(cost, embodied)`; duration `max(1, duration)`. |
| `combat.damage` | `attacker_<p>`, `target_<p>`, `distance` | `<p>`: `attack`, `range`, `cooldown`, `max_health`, `health`, `energy`, or any attribute or derived property. |
| `combat.energy_cost` | `attacker_<p>` | Static per attacker, so each unit carries an exact quote. |
| `visibility.detection_formula`, `identification_formula` | `observer_<p>`, `target_<p>`, `distance` | `<p>`: `vision`, `sensor`, `signature`, `identification`, or any attribute or derived property. |

## Cumulative cost, worked example

```json
"armor": { "default": 0, "min": 0, "max": 20, "mass": "v * 3",
           "cost": "v * v * 5 + v * 10", "duration": "v * 8" }
```

| Level `v` | `cost(v)` | `duration(v)` |
|---|---|---|
| 0 | 0 | 0 |
| 1 | 15 | 8 |
| 2 | 40 | 16 |
| 3 | 75 | 24 |

- Upgrading 0 → 3 costs `75 − 0 = 75` energy and takes 24 ticks.
- Upgrading 1 → 3 costs `75 − 15 = 60` and takes 16 ticks.
- Stepping 0→1→2→3 costs 15 + 25 + 35 = 75: **the price doesn't depend on
  the path.**
- A convex `cost` makes high levels progressively expensive.
- **Embodied investment** at level 3 is 75 (relative to the default of 0).

The attributes plugin publishes per-level `cost_table` and
`duration_table` arrays in `WorldInfo.rules_json.attributes`, so clients
read exact prices without evaluating formulas.

## Built-in derived values

| Name | Defined by | Meaning |
|---|---|---|
| `mass` / `attributes.mass` | attributes | `base_mass + Σ mass(level)` |
| `attributes.<name>` | attributes | Each attribute level and each config-derived property |
| `movement.ticks_per_move` | movement | `pace_attribute` value (min 1) or `ticks_per_move` |
| `energy.extraction_rate` | energy | `extraction_attribute` value or `extraction_rate` |
| `energy.transfer_rate` | energy | `transfer_rate` |
| `logistics.range` | logistics | Bound property clamped to 1..16, or `range.default` |
| `regeneration.amount` | example plugin | `amount_attribute` value or `amount` |

## Common observable attribute keys

| Prefix | Examples | Visibility |
|---|---|---|
| `energy.` | `stored`, `capacity`, `mining_well`, `transfer_to`, `transfer_remaining` | Owner |
| `attributes.` | each attribute, `mass`, derived properties, `invested`, `embodied`, `busy` | Owner |
| `movement.` | `destination` | Owner |
| `combat.` | `health`, `max_health` (public, or disclosed under fog); `attack`, `range`, `cooldown`, `attack_cost`, `ready_at`, `target`, `out_of_range`, `stalled` (owner) | Mixed |
| `wells.` | `energy`, `capacity`, `extraction_rate`, `unlimited` | Public (disclosed under fog) |
| `salvage.` | `energy`, `initial` (wrecks); `target` (collector) | Public / owner |
| `cloning.` | `quote_cost`, `quote_ticks`, `quote_offspring_energy`; while cloning `cost`, `done`, `duration`, `offspring_energy`, `blocked` | Owner |
| `upgrades.` | while upgrading `attribute`, `from`, `target`, `cost`, `done`, `duration` | Owner |
| `metadata.` | `<key>` | Owner |
| `visibility.` | `vision`, `sensor`, `signature`, `identification` | Owner |
| `automation.` | `behavior`, `state`, `status`, `reason`, `target`, `well`, `receiver`, `leader`, `position`, `radius` | Owner |
| `logistics.` | `reserve`, `supply.<n>.to`, `supply.<n>.status` | Owner |
| `terrain.` | `type`, `passable`, `cost` | Public |

Derived properties such as `logistics.range` are **not** attributes. Read them in code with `World.Derived`; clients see only what a component exposes.

Admins see everything. With fog of war on, foreign entities' attributes
reach players only through `visibility.disclosure` rules.
