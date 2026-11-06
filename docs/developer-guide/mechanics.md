# Mechanics rules reference

The exact rules of every built-in mechanic, as implemented. Player-facing
explanations are in the [user guide](../user-guide/concepts.md); this page
is the precise reference for developers, bot authors and operators.
Configuration fields are listed in
[reference/configuration.md](../reference/configuration.md).

Per-tick phase order with every built-in plugin (logged by the server at
startup as `tick schedule`):

```
commands → automation → energy (energy.mine, energy.transfer) → logistics
→ salvage → movement → combat → upgrades → cloning → [observation]
```

Observation (fog of war) is not a phase: the observation authority runs
after every system (see [simulation.md](simulation.md)).

## Movement plugin (rules)

Each tick, in phase `movement`, every unit with an order that is off
cooldown proposes **one step**: one tile toward its destination on each
axis toward the next waypoint of its route (8-directional; see
*Pathfinding* below). All proposals are then resolved together:

| Situation | Outcome |
|---|---|
| Target tile empty | Step succeeds. |
| Two or more units propose the same tile | **All fail.** Each waits a seeded-random `0..contention_backoff_ticks` extra ticks before retrying, which breaks symmetric deadlocks without favoring low IDs. |
| Target occupied by a unit that isn't stepping | Fails. |
| Target occupied by a unit that is stepping | Succeeds only if that unit's step succeeds. Chains move as one; a column follows its leader in the same tick. |
| Cycle, including a two-unit swap | The whole cycle fails. |

A failed step keeps the order. After `max_blocked_ticks` consecutive
failures the order is cancelled with a `movement.cancelled` event. Reaching
the destination emits `movement.arrived`. After a successful step the unit
waits `movement.ticks_per_move` ticks (a derived property) before stepping
again. That cooldown is stored separately from the order, so reissuing
orders can't make a unit move faster.

`MoveCommand` carries an explicit destination per unit, so clients choose
formations themselves. The web client spreads a group over the nearest free
tiles. Validation is atomic: an unknown unit, someone else's unit, an
out-of-bounds or missing destination, or a duplicate unit fails the whole
command with a coded error. A destination attribute is exposed to the owner
only.

### Pathfinding and group movement (Milestone 6)

Terrain (`internal/plugins/terrain`) places static features from config:
impassable types are blocking entities; passable types multiply the
per-step cooldown by their `cost` (1..20). Units are never permanent
obstacles for planning.

Route choice is hybrid (`pathfinding: "auto"`, the default): while the
straight greedy step is enterable and the unit has not been blocked, it
steps greedily exactly as in Milestone 1. Otherwise it plans an A* route
(`internal/plugins/movement/path.go`) with octile costs × terrain cost, no
corner cutting, and a fixed neighbour order (N, NE, E, SE, S, SW, W, NW).
Ties on equal f are broken by h, then by squared Euclidean distance to the
goal, then by tile index — so equal-cost routes always resolve the same way.
`"always"` plans every route.

Bounded work: at most `max_path_nodes` expansions per plan and
`plans_per_tick` plans per tick (units over the limit keep waiting and plan
on a later tick; the iteration start rotates by tick so no unit starves).
A route stores at most 32 waypoints in the order (comparable, hashed,
replayed) and is replanned only when exhausted or after
`replan_after_blocked` consecutive blocked steps; that replan treats units
within 3 tiles as obstacles, which routes around congestion. If the
destination cannot be reached, the plan walks to the explored tile closest
to it; if no step at all is possible the order is cancelled with reason
`unreachable`. Conflict resolution (all-fail contention, chains, cycles)
is unchanged.

`GroupMoveCommand` assigns a group distinct reachable tiles around one
destination: slots are found breadth-first from the target over enterable
tiles (fixed neighbour order; tiles held by units outside the group are
skipped; at most 64 tiles visited per unit), and units, sorted by distance
to the destination then ID, take slots in that order — the closest unit
gets the target tile. Each unit is charged
against the budget individually and gets a per-unit result: the command
partially succeeds when some units get no slot or the budget runs out.

## Automation (Milestone 6)

`internal/plugins/automation`: four typed behaviors (mine-and-deliver,
auto-salvage, guard, follow), each a small state machine stored as a
comparable component (hashed and replayed like any state). The phase runs
first in the tick, so decisions use end-of-previous-tick state — the same
state the owner observed. A unit is evaluated when
`(id + tick) % eval_interval == 0` (immediately after assignment), at most
`max_evaluations_per_tick` units per tick; the rest are deferred to their
next slot, and admins get an `automation.work` event with the counts.

Behaviors never issue commands. They set the same orders players can give
(destination, mining, transfer, attack target, salvage target) through
exported setters, and the normal mechanics execute them — so automation
cannot do anything a player could not, cannot recurse, and its cost per
evaluation is bounded (guard scans at most (2·16+1)² tiles; salvage scans
the wreck index).

Information: wells, wrecks and enemies must be currently detected by the
owner (visibility plugin); receivers and leaders must be the owner's units.
Command-time validation returns the same not-found error for hidden and
non-existent targets. Blocked (temporary, e.g. `receiver_full`,
`no_wreckage`) and failed (permanent, the behavior is removed:
`well_unavailable`, `receiver_unavailable`, `well_depleted`,
`leader_unavailable`) are reported to the owner as events and as the
`automation.status`/`automation.reason` attributes.

Manual orders on an automated unit are overwritten at its next
evaluation; cancel automation to take manual control.

## Logistics (Milestone 6)

`internal/plugins/logistics`: persistent supply links between a player's
own units (`SupplyCommand`: reserve, fill_to, resume_below; up to 4
receivers per supplier). Range is a stat (`range.property` may bind it to
an upgradable attribute, up to 16); rate is `energy.transfer_rate`.
Links that would form a cycle are rejected at command time, so supply
networks are DAGs and relay chains (well → miners → depot → relay → front)
are explicit.

Allocation per tick (phase `logistics`, after `energy`), all from the
phase-start snapshot and applied at once:

1. A link wants `fill − receiver energy` when in range and not resting.
   It rests once the receiver reaches `fill_to` and resumes only below
   `resume_below` (hysteresis: no top-up oscillation).
2. A supplier offers `min(rate, energy − reserve)`, split max-min fairly
   over its links (ties by receiver ID).
3. A receiver accepts at most its free capacity, split max-min fairly over
   incoming grants (ties by supplier ID).
4. Grants are applied as per-unit deltas.

Because a supplier sends at most `energy − reserve` and a receiver accepts
at most `capacity − energy` (both at phase start), no store goes negative
or over capacity, the global sum is unchanged, and energy advances at
most one link per tick (no teleporting along a chain). Out-of-range links
wait (`out_of_range`); a lost receiver ends the link (`logistics.ended`).

## Wells and energy plugins (rules)

All quantities are `int64` energy units. Rates are per tick.

**Wells** (`wells` plugin) are resource entities with
`{energy, capacity, extraction_rate, unlimited}`. All four are public
attributes (`wells.*`), and every player sees every well.

- Config: `defaults` plus a list of initial `wells`, each of which may
  override any default.
- Admin commands: `CreateWellCommand` (unset fields use the defaults) and
  `ConfigureWellCommand` (only set fields change). Both enforce
  `0 ≤ energy ≤ capacity` and `extraction_rate ≥ 0`.
- Reducing `capacity` alone clamps `energy` down to it.
- Wells are deleted or moved with the generic `DeleteEntity` and
  `MoveEntity` admin RPCs.

**Unit storage** (`energy` plugin):

- Every unit gets `{energy: initial_energy, capacity: unit_capacity}` when
  it's created.
- `energy.stored` and `energy.capacity` are owner-only attributes; opponents
  can't see them.
- There is no passive upkeep. Energy changes only through mining,
  transfers, `Spend` (movement) and the admin `SetUnitEnergyCommand`.

The phases run in this order every tick: commands, then `energy`
(`energy.mine`, then `energy.transfer`), then `movement`. Energy settles
before units move and pay.

**Mining.** `MineCommand{unit_ids, well_id}` sets a persistent order. A
unit extracts only while it's adjacent to the well (Chebyshev distance 1,
diagonals included). Otherwise the order waits, so a client typically
sends a `MoveCommand` to a tile next to the well along with the mine
order. Each tick:

1. Each adjacent miner asks for `min(extraction_rate, free capacity)`.
2. The well releases at most `extraction_rate`. A finite well releases at
   most its remaining energy too.
3. If the requests exceed the release, it's split **max-min fairly**:
   requests are visited smallest first (ties by entity ID), and each gets
   `min(request, remaining ÷ claimants left)`.

The grants never exceed the release, and a finite well loses exactly what
units gain. Example: 5 split among three requests of 4 gives 1, 2, 2.

- The miner gets an `energy.mined` event `{amount, well}` (owner only).
- A finite well that reaches 0 emits public `energy.well_depleted`, and
  every order on it ends with `energy.mining_stopped` (`well_depleted`).
- A removed well ends orders with `well_removed`.
- A full unit keeps its order and resumes once it has free capacity.
- `StopMiningCommand` cancels.

**Transfers.** `TransferCommand{from, to, amount}` requires:

- both are units of the issuer (an admin may issue it, but only between
  units of one owner),
- `from ≠ to`,
- `amount > 0`,
- the units are adjacent when the command executes.

It sets a persistent order on the sender, replacing any earlier one. Each
tick, senders are processed in ascending ID order. Each moves
`min(transfer_rate, amount left, sender energy, receiver free)` and emits
`energy.transferred {to, amount}`. The order ends with
`energy.transfer_ended {reason, remaining}`, where the reason is one of:

- `completed`
- `sender_empty`
- `receiver_full`
- `not_adjacent`
- `target_lost`

Energy is only ever moved, never created or destroyed.

**Movement cost.** Each successful step costs `movement.energy_per_step`
(default 0, so Milestone 1 behaviour is unchanged).

- A unit that can't pay doesn't propose a step. It neither moves nor
  counts as blocked, so `max_blocked_ticks` doesn't cancel it.
- It keeps its order and emits one `movement.stalled {required, stored}`
  event when it starts waiting.
- It resumes as soon as it has enough energy, for example after a
  transfer.
- Failed steps cost nothing. Spent energy is consumed (it leaves the
  economy).

**Conservation (tested).** With free movement and finite wells, the energy
in all units plus all finite wells is constant every tick, under random
mining, transfer and move commands.

## Unit evolution (Milestone 3)

### Formula language (`internal/expr`)

The language has integer literals, variables, `+ - * / %`, unary minus,
parentheses, and `min`, `max`, `clamp`, `abs`, `pow` and `sqrt`.

Evaluation is **total**:

- `/` and `%` truncate toward zero, and dividing by zero yields 0.
- Every operation saturates at the int64 range.
- `pow` is bounded, and `sqrt` is an exact integer floor.

So a formula can never error or panic at runtime, and there's no floating
point in results.

Formulas are parsed once (at most 512 characters, 128 nodes, 32 levels
deep) and compiled against a caller-defined variable layout. Unknown
variables are compile errors, which is how each formula's allowed inputs
are enforced. There are no loops, no assignment, and no access to anything
but the provided integers.

### Attributes

Operators define up to 16 attributes. Each has an integer default, min and
max (at most 1,000 levels), and three formulas of `v`:

| Formula | Meaning |
|---|---|
| `mass` | Contribution to the unit's mass. |
| `cost` | **Cumulative** energy invested to reach `v`. Must be non-decreasing. |
| `duration` | **Cumulative** ticks to reach `v`. Must be non-decreasing. |

Upgrading from `a` to `b` costs `cost(b) − cost(a)` and takes
`max(1, duration(b) − duration(a))` ticks. Because costs are cumulative,
the price doesn't depend on whether you go up in one step or several, and
a convex `cost` gives nonlinear upgrades. The tables are evaluated over
`[min, max]` at load time. That validates monotonicity and makes lookups
O(1), and the tables are published so clients show exact costs without an
evaluator.

**Derived properties:**

- `mass` is built in: `base_mass` plus the sum of the contributions.
- Further derived properties are formulas over attributes, `mass` and other
  derived properties. They are sorted into dependency order, and cycles and
  unknown references are rejected with a readable error.
- They depend only on the unit's own values, so they're computed on demand
  (never stored), are exactly reproducible, and are also exposed as kernel
  derived properties `attributes.<name>`.

**Embodied investment** is `Σ cost(value) − cost(default)`. **Invested** is
the energy actually paid: completed upgrades, plus the embodied investment a
clone inherits.

Attributes are owner-only observations (`attributes.<name>`). Admins can
override any value within its bounds (`SetAttributeCommand`); an override
doesn't count as investment. There are no unit classes: every unit has
every attribute, starting at its default.

**Bindings.** Existing mechanics read attributes only when configured to,
so Milestone 2 configurations behave as before:

| Config setting | Effect |
|---|---|
| `energy.capacity_attribute` | Storage capacity follows the named property, kept in sync through `attributes.OnChange` |
| `energy.extraction_attribute` | Mining rate |
| `movement.pace_attribute` | Ticks per step (`max(1, value)`) |

### Upgrades

`UpgradeCommand{unit_ids, attribute, target | increase}`:

| Rule | Behaviour |
|---|---|
| Authorization | Owner only (or admin). |
| Validity | Target must be higher than the current value (upgrades are irreversible) and at most `max`. |
| Exclusivity | One upgrade **or** cloning per unit at a time, via `attributes.Reserve`. |
| Payment | Full cost from the unit's own store **when the upgrade starts**; otherwise `insufficient_energy` and nothing changes. |
| Completion | The value is applied only after the duration. The start tick counts as tick 1. |
| Interruption | No cancellation and no refunds. A destroyed unit loses its paid energy. If an admin has already raised the value to or past the target, it completes as `upgrades.failed` (`superseded`). |
| Events (owner only) | `upgrades.started`, `upgrades.progress` (every N ticks), `upgrades.completed`, `upgrades.failed` (including validation failures, with the reason code). |

The command is atomic over its unit list. In a batch, each unit stands
alone.

### Cloning

`CloneCommand{unit_ids, direction}`.

**Price:**

| | Formula | Variables |
|---|---|---|
| Effective cost | `max(cost formula, embodied)` | `base`, `embodied`, `mass` |
| Duration | `max(1, duration formula)` | `base`, `embodied`, `mass` |

The default cost is `base + embodied`, so an offspring is never cheaper
than the upgrades it carries.

**Payment:**

- The parent pays `cost + offspring_energy` up front.
- `cost` is consumed. `offspring_energy` is held by the order and moved
  into the child.
- The child does **not** get the energy plugin's free `initial_energy`.
  Total energy therefore drops by exactly `cost`; this is tested by summing
  every store before and after.

**Inheritance:**

- The child copies the parent's attribute values as captured at start
  (the parent can't upgrade while cloning).
- Its `invested` equals the parent's embodied investment.
- It gets a fresh unpredictable ID.
- Metadata is not inherited.

**Placement:**

- The child goes to the requested direction, or else the first free tile in
  the order N, NE, E, SE, S, SW, W, NW.
- Parents completing in the same tick are processed in ascending ID order,
  each claiming its tile before the next looks, so they never collide.
- A parent with no free tile keeps its paid order, emits `cloning.blocked`
  once, and completes on the first tick a tile frees up.
- The command checks that a free tile exists at the start.

**Quote.** Each unit carries an owner-visible `cloning.quote_*` with its
current exact price, refreshed when it's created and whenever its
attributes change.

### Metadata, selectors and batches

**Metadata:**

- Each unit has up to 8 owner-only key/value pairs (`metadata.<key>`).
- Keys use `[a-z0-9][a-z0-9_.-]*`; values are printable ASCII; lengths are
  configurable.
- `SetMetadataCommand` and `RemoveMetadataCommand` are atomic over their
  unit list and owner-only.
- No plugin reads metadata, so it cannot affect gameplay (tested with a
  key that matches an attribute's name).

**Selectors.** A `Selector{unit_ids?, conditions[], limit, owner_id(admin)}`:

- only ever matches the issuer's own units,
- supports `EQUALS`, `PRESENT` and `ABSENT` conditions, all of which must
  hold,
- yields units in ascending ID order, capped by `limit` and
  `max_batch_units`.

It's evaluated **when the batch command executes**: at its scheduled tick,
in the deterministic command order, so it sees the effects of earlier
commands in the same tick.

**`BatchCommand{selector, Any command}`:**

- Applies a unit-applicable template to each selected unit through the
  kernel's `ApplyToUnit`. Supported templates: every command whose
  `CommandDef` sets `ApplyToUnit`: upgrade, clone, stop (movement), mine,
  stop-mining, attack, stop-attack, salvage, stop-salvage, set and remove
  metadata, automate, cancel-automation, supply and cancel-supply.
- Each unit is validated and charged separately, and gets a `UnitResult`.
- The batch is `PARTIAL` if some units fail, and `FAILED` (still listing
  the units) if none succeed or nothing matches.
- Admin-only templates, nested batches and commands without per-unit
  support are rejected.

## Combat, destruction and salvage (Milestone 4)

**Tick order:** commands → energy (mine, transfer) → salvage → movement →
combat → upgrades → cloning. `PhaseDef.Before` lets combat and salvage
place themselves without other plugins knowing about them.

**Combat** (`internal/plugins/combat`):

- **Stats.** `max_health`, `attack`, `range` and `cooldown` are each an
  attributes property or a constant.
- **Formulas.** `damage` uses `attacker_<p>`, `target_<p>` and `distance`.
  `energy_cost` uses only `attacker_<p>` (static properties), so each unit
  carries an exact quote.
- **Range** is Chebyshev distance. Attacks hit instantly: no projectiles and
  no attack duration.
- **Resolution.** Every eligible attack's cost and damage is computed from
  the start-of-phase state. Damage is summed per target, then all energy,
  cooldowns and damage are applied, and destruction comes last. A unit
  destroyed this tick therefore still fires, and visiting order (including
  ID order) cannot change outcomes.
- **Order lifecycle.** An order persists until the target is gone
  (`target_lost`), the attacker's attack drops to 0 (`incapable`), it is
  replaced, or it is stopped. Out of range or short of energy, the order
  waits and emits one event per episode. Friendly targets are rejected.
- **Visibility hook.** `Plugin.CanTarget` (wired to `visibility.Detected`
  in `game.Plugins()`) rejects targets the attacker's owner does not detect.
- **Destruction** removes the entity, which drops all of its
  components, orders and activities (unfinished upgrade or clone energy is
  lost). `combat.destroyed` goes to the owner and the attacking owners.
- **Event scoping.** Health is public; stats and orders are owner-only;
  `combat.attack` goes to the attacker's owner and `combat.damaged` to the
  target's owner.

**Wreckage** is a passable resource entity, made possible by a kernel
addition: passable entities are excluded from the occupancy index, so units
walk over wreckage and several wrecks can share a tile.

- **Energy:** `floor(min(embodied, invested) × e‰) + floor(stored × s‰)`,
  in integer thousandths with floor rounding. Using `min(embodied, invested)`
  means admin attribute grants are never recovered.
- **No wreck** is left when that amount is 0.
- **Distinct from wells:** a wreck has its own component, and mining it is
  rejected.

**Salvage** (phase `salvage`):

- Orders collect while the unit is within distance 1 (adjacent or on the
  wreck), up to `min(rate, free battery)` per tick.
- When several units collect from one wreck, the shared `fairshare.Split`
  (also used by mining) divides it. The wreck loses exactly what collectors
  gain.
- An emptied wreck is removed and its orders end.

**Admin:** set integrity (0 destroys, leaving wreckage), create wrecks, set
wreck energy (0 removes), and attribute edits. All are scheduled commands,
so replays are exact (tested at scale).


## Fog of war, sensors and intelligence (Milestone 5)

**Observation authority.** The `visibility` plugin registers as the
kernel's single *observation authority*
(`Registrar.ObservationAuthority`).

- **When it runs.** After **all** systems each tick (commands → energy →
  salvage → movement → combat → upgrades → cloning → *observation*). Players
  therefore only see settled end-of-tick state: post-move positions,
  post-combat damage and destruction, offspring, executed admin mutations.
- **Its output.** A typed per-player `Observation` in the frame: tiles
  entered and exited, foreign-entity upserts (already filtered), lost
  entities, and admin-only diagnostics.
- **What commands use.** Commands that refer to foreign entities (attacks,
  mining, salvage) and continuing attack orders use the observation from the
  end of the *previous* tick, which is what the player actually knew.

**Model (per player, deterministic, integer).**

- **Visible:** a tile within an own unit's vision range. The metric is
  chebyshev, manhattan or euclidean (rounded); there is no occlusion yet
  (`occlusion: "none"` is validated).
- **Detected:** on a visible tile, and
  `detection_formula(observer_*, target_*, distance) >= detection_threshold`
  for an observer that sees it.
- **Identification level:** the best observer's `identification_formula`
  against `estimated_threshold` and `detailed_threshold`.
- **Resources** use `resource_signature`.
- **Stats.** All four sensor stats (vision, sensor, signature,
  identification) bind to attributes. Scouts and stealth units arise from
  upgrades alone.

**Disclosure.** Foreign attributes are disclosed only through
`disclosure` rules: an exact key or `prefix.*`, with a per-level precision of
0 (hidden), 1 (exact) or N (an estimate range `[⌊v/N⌋N, ⌊v/N⌋N+N−1]`).

- Higher levels must be at least as precise; validation enforces it.
- Metadata, orders, upgrade progress and combat targets have no rule by
  default, so they are never sent.
- A foreign entity is re-sent only when its **filtered** view changes, so
  hidden attribute changes cause no observable traffic.

**Server.**

- **The hub** keeps each player's current observation, built from frames:
  visible tiles plus the disclosed foreign views.
- **Snapshots and updates** are built from:
  - the player's own entities (in full),
  - that observation,
  - `removed` restricted to their own entities plus entities they lost
    (loss and destruction are indistinguishable),
  - events addressed to them.
- **Never per-player world serialization.** The server never serializes the
  whole world per player per tick; per-player work is proportional to what
  that player observes and what changed.
- **Admin.** Admins keep the full world. `AdminService.GetPlayerView`
  previews any player's exact snapshot with the detections behind it.

**Information-security review (fixes made):**

| Leak | Fix |
|---|---|
| Global `removed` list | Only own entities and lost entities |
| `energy.well_depleted` broadcast | Sent only to the owners whose units drained the well |
| "Not found" vs "not yours" vs "not visible" errors | One `not_found` "X is not available" for players (move, stop, mine, transfer, upgrade, clone, metadata, attack, salvage) |
| Failed upgrade/clone events sent to the *target's* owner | Never sent |
| Attack orders revealing destruction | End with `target_unavailable` the moment the target is no longer detected |
| Public health, well and wreck values | Now subject to disclosure rules |

Entity IDs remain opaque, global counts are admin-only, and command timing
does not depend on hidden state.

**Client memory.** The server keeps no explored-map history. The client
keeps explored tiles (per tab) and "last known" ghosts of lost entities,
drawn as stale outlines with the tick last seen.
