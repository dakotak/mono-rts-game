# Energy

Energy is the only resource. It's an integer, it lives **inside units**,
and every action that matters spends it. There is no player-wide pool.

## Where energy comes from

| Source | How |
|---|---|
| **Wells** | Resource entities with `energy`, `capacity`, `extraction_rate` and possibly `unlimited`. Adjacent units mine them. |
| **Wreckage** | Destroyed units leave passable wrecks holding part of their energy and upgrades. Salvage them ([combat](combat.md)). |
| **Starting energy** | New units start with `energy.initial_energy`, except clones, which get exactly what their parent hands over. |
| Admin grants | `SetUnitEnergyCommand`, for operators only. |

## Storage

Each unit has a store (`energy.stored` and `energy.capacity`, visible only
to you). Capacity is either fixed (`unit_capacity`) or bound to an
attribute (the dev config uses `energy_capacity`). A full unit can't take
more: mining waits, and transfers and salvage give less.

## Mining

Give a unit a mining order on a well. It extracts only while
**adjacent** (diagonals count). In the client, right-clicking a well walks
the units next to it.

Each tick:

1. Each adjacent miner asks for `min(its extraction rate, free capacity)`.
   The extraction rate is `energy.extraction_rate` or the bound attribute;
   it's `mining_efficiency` in dev.
2. The well releases at most its `extraction_rate`, and a finite well at
   most what it has left.
3. If miners ask for more than the well releases, it's split **max-min
   fairly**: small requests are filled first, the rest share evenly, and
   ties go by unit ID.

Example: a well releasing 5 to three miners asking 4 each gives 1, 2, 2.
Nothing is lost. A finite well loses exactly what the miners gain.

Events:

| Event | When |
|---|---|
| `energy.mined` | Each tick a unit gains energy (floats "+N" in the client). |
| `energy.well_depleted`, `energy.mining_stopped` | A finite well reaches 0. The depletion event goes to the players whose units drained it. |

A full miner keeps its order and resumes when it has room.

## Transfers

A transfer order moves energy from one of your units to another
**adjacent** one. It moves up to `transfer_rate` per tick until the amount
is sent, the sender is empty, the receiver is full, or they're no longer
adjacent (`energy.transfer_ended` gives the reason). In the client,
select senders next to a unit and right-click it.

For anything recurring, use [supply links](automation.md#logistics-supply-links)
instead. They work over a distance, respect reserves and persist.

## Spending

| Action | Cost |
|---|---|
| Moving | `movement.energy_per_step` per successful step (1 in dev). A unit that can't pay waits. |
| Upgrading | The full upgrade price, paid up front when the upgrade starts. |
| Cloning | Clone cost plus the offspring's starting energy, up front. |
| Attacking | `combat.energy_cost` formula per attack (`1 + attack / 4` in dev). A unit that can't pay waits. |

Spent energy leaves the world. Mining, transfers, supply links and salvage
only move it. Nothing else creates energy, apart from admin grants and the
free `initial_energy` of units spawned by config or the admin. (The
[example regeneration plugin](../developer-guide/plugins/examples.md),
if a server enables it, also creates energy.)

## Strategy

- **Distance is cost.** Every step costs energy, so mine near where
  you'll spend, or carry energy forward with relays.
- **Big purchases need big batteries.** Upgrades and clones are paid in
  full by one unit. Raising `energy_capacity` on a few "bank" units early
  unlocks expensive specializations.
- **Don't over-crowd a well.** Once the miners' combined rate reaches the
  well's `extraction_rate`, more miners add nothing.
- **Unlimited wells** never run out, but still release only their rate per
  tick. Holding one is a lasting advantage.
- **Energy is hidden.** Opponents can't see your stores unless the
  server's disclosure rules allow it. The dev config shows `energy.stored`
  as a range 50 wide at estimated identification, and exactly at detailed.
