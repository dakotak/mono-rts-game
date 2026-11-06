# Automation and logistics

Two built-in systems let units work without a command every tick:

- **behaviors** (automation): mine-and-deliver, auto-salvage, guard,
  follow
- **supply links** (logistics): standing energy deliveries between your
  units

There is no scripting. Behaviors are small fixed state machines with
typed parameters, and they can only do what you could order by hand.

## Automation behaviors

Assign a behavior to selected units, or to a tagged group server-side.
Assigning one replaces any previous behavior and clears the unit's
current orders. Cancel it to take back manual control: a manual order on
an automated unit is overwritten at its next evaluation.

| Behavior | Parameters | What it does |
|---|---|---|
| **Mine & deliver** | well, receiver (your unit), deliver-at level (0 = full), keep | Walk next to the well, mine until holding *deliver-at*, walk next to the receiver (which may move), transfer everything above *keep*, repeat. |
| **Auto-salvage** | radius (1..16) | Pick the nearest detected wreck within the radius (ties by ID), collect it, repeat. |
| **Guard** | position, radius (0..16) | Hold the position. Attack the nearest detected enemy unit within the radius of the position (ties by ID), closing in only while staying inside the area. Return when no threat remains. |
| **Follow** | leader (your unit), distance (1..8) | Stay within the distance of the leader. |

### Status

Each automated unit shows these (owner only):

| Attribute | Values |
|---|---|
| `automation.behavior` | The behavior |
| `automation.state` | E.g. `to_well`, `mining`, `to_receiver`, `delivering`, `searching`, `collecting`, `holding`, `engaging`, `following` |
| `automation.status` | `active` or `blocked` |
| `automation.reason` | Why it's blocked |
| `automation.target` | Its current objective, if any |

Behaviors report problems in two ways:

- **Blocked** (temporary; it waits and retries): `receiver_full`,
  `no_wreckage`, `battery_full`, `well_crowded`, `receiver_crowded`,
  `wreck_crowded`, `leader_crowded`, `cannot_attack`. You get an
  `automation.blocked` event when it starts.
- **Failed** (permanent; the behavior is removed): `well_unavailable`,
  `receiver_unavailable`, `well_depleted`, `leader_unavailable`. You get
  `automation.failed`.

The client's **Automation status** panel lists blocked units with reasons.

### Timing and fairness

- **Evaluation interval.** Units are re-evaluated every few ticks
  (`automation.eval_interval`, 5 in dev), staggered across units. A guard
  may take up to that long to react.
- **Per-tick cap.** The server evaluates at most
  `max_evaluations_per_tick` units per tick and defers the rest to their
  next slot.
- **Same information as you.** Behaviors only use what you could see:
  wells, wrecks and enemies must be currently detected by you.

## Logistics: supply links

A **supply link** is a standing order: "send energy from this unit to that
unit". Each link has:

| Setting | Meaning |
|---|---|
| **Reserve** | The supplier always keeps at least this much. |
| **Fill to** | Stop when the receiver holds this much (0 = its capacity). |
| **Resume below** | After filling, rest until the receiver drops below this (0 = the fill level). This hysteresis stops constant tiny top-ups. |

How links behave:

- **Range and rate.** Links work over distance: within the supplier's
  **transfer range** (`logistics.range`, bound to `transfer_range` in
  dev). They move at most the supplier's transfer rate per tick in total.
- **Shared capacity.** A supplier can feed up to **4** receivers, split
  fairly. A receiver fed by several suppliers splits its free space fairly
  among them.
- **Relays.** A unit can both receive and supply, so chains work:
  well → miners → depot → relay → front line. Energy advances **one link
  per tick**, so it never teleports along a chain.
- **No loops.** A link that would create a cycle (A→B→A) is rejected.
- **Out of range.** If units move apart, the link waits (`out_of_range`)
  and resumes when they're back in range. If the receiver is destroyed,
  the link ends (`logistics.ended`).
- **Conservation.** Energy is conserved exactly. Nothing is created or
  lost in transit.

Link statuses (`logistics.supply.<n>.status`):

| Status | Meaning |
|---|---|
| `supplying` | Energy flowed this tick |
| `satisfied` | Resting: the receiver reached its fill level |
| `reserve` | The supplier is down to its reserve |
| `receiver_full` | The receiver has no free space |
| `out_of_range` | Too far apart |
| `pending` | Waiting to be evaluated |

## Recipes

**Self-running economy.**

1. Tag miners `role=miner`.
2. Assign *Mine & deliver* to the group (well → a depot unit next to it).
3. Supply depot → relay (reserve 20) and relay → front units.

**Defended mine.** Two units on *Guard* at the well (radius 5–6). They
engage raiders that come within the area, and return afterwards.

**Escort.** Fighters *Follow* a valuable carrier at distance 2.

**Battlefield cleanup.** After a fight, *Auto-salvage* radius 8 on a few
high-capacity units.

Exact rules: [mechanics: automation](../developer-guide/mechanics.md#automation-milestone-6)
and [logistics](../developer-guide/mechanics.md#logistics-milestone-6).
