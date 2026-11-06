# Units, movement and groups

## The unit

Every unit is the same kind of thing. What differs is its state:

| State | Who sees it | Attribute keys |
|---|---|---|
| Position, owner | Everyone who observes it | (entity fields) |
| Energy store | Owner | `energy.stored`, `energy.capacity` |
| Attribute levels, derived properties | Owner | `attributes.<name>`, e.g. `attributes.armor`, `attributes.mass` |
| Integrity | Everyone with fog off; under fog, others only as far as disclosure rules allow | `combat.health`, `combat.max_health` |
| Orders | Owner | `movement.destination`, `energy.mining_well`, `combat.target`, ... |
| Tags | Owner | `metadata.<key>` |

Attribute keys are always `<plugin>.<name>`. The full list depends on the
server's configuration, and clients should read whatever attributes
arrive rather than assume a fixed set.

## Movement

Send a unit to a destination with a move order. The order persists until
the unit arrives, is stopped, or gives up.

**Steps.**

- Each tick, a unit whose cooldown has elapsed tries one step toward its
  destination, in any of 8 directions. Diagonals cost the same as
  straight steps.
- After a step it waits `movement.ticks_per_move` ticks. That's a derived
  property; in the dev config it grows with mass and shrinks with
  propulsion.
- Each step may cost energy (`movement.energy_per_step`). A unit that
  can't pay waits (`movement.stalled`) and resumes when it has energy.

**Collisions are resolved for everyone at once.**

| Situation | Result |
|---|---|
| Two units want the same empty tile | **Both** fail and back off a short random time. Nobody wins by ID. |
| A unit steps into a tile another unit is leaving this tick | It succeeds. Columns move as one. |
| Two units try to swap places, or a ring of units rotates | It fails. |
| The path is blocked for `max_blocked_ticks` steps in a row | The order is cancelled (`movement.cancelled`). |

**Routing.**

- Units step straight toward the goal while that works.
- When the straight step is blocked by terrain or a well, or the unit
  keeps getting blocked, it plans a route around obstacles.
- Costly terrain (marsh) slows a unit down; routes avoid it when a cheaper
  way exists.
- If the destination can't be reached at all, the unit walks to the
  closest reachable point. If it can't move at all, the order is
  cancelled with reason `unreachable`.

**Group moves.** Sending a group to one tile makes the units fight over
it. Two ways avoid that:

- The web client spreads the group over the nearest free tiles (one
  destination per unit in a `MoveCommand`).
- A `GroupMoveCommand` lets the server do it: it gives each unit its own
  reachable tile around the destination, closest units first. If there
  aren't enough tiles, some units get a per-unit failure and the rest
  still move.

**Arrival.** You get `movement.arrived` when a unit reaches its
destination.

Exact rules: [mechanics: movement](../developer-guide/mechanics.md#movement-plugin-rules).

## Stopping

**S** in the client, or `StopCommand`, cancels movement. The client's
**S** also stops mining, attacks and salvage. Stopping doesn't cancel an
upgrade or clone in progress; those can't be cancelled.

## Metadata: tagging your units

Each unit can carry up to 8 private key/value tags (`role=miner`,
`squad=alpha`). Tags never affect gameplay, but they're how you address
groups:

- In the client: set tags on a selection, then filter (`role=miner`,
  `squad`, `!tired`) to select matching units.
- **Server-side selectors.** A `BatchCommand` carries a **selector** (tag
  conditions, optional explicit IDs, a limit) and a command **template**.
  The server applies the template to every matching unit **when the
  command executes**, so units tagged a moment ago are included.
  - Each unit is validated and charged separately, with its own result.
  - The batch reports `PARTIAL` when only some units succeed.
- Templates that work per unit:
  - upgrade, clone
  - stop, mine, stop-mining
  - attack, stop-attack
  - salvage, stop-salvage
  - set and remove metadata
  - automate and cancel-automation
  - supply and cancel-supply

Selectors only ever match **your own** units. Opponents never see your
tags.

## Strategy

- **Pace versus everything else.** Each attribute adds mass, so think
  about what mass costs you before stacking armor.
- **Spread your moves.** Moving 50 units onto one tile makes them collide.
  Let the client or `GroupMoveCommand` spread them.
- **Tag early.** `role` and `squad` tags make every later batch, upgrade
  and automation a single command, regardless of the command budget.
