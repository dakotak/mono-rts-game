# Concepts

What a Mono world is, how time works, and what you can know.

## The world

- A world is a rectangular **grid of tiles**. `(0,0)` is the top-left
  tile, `x` grows to the right and `y` grows downward.
- Every **entity** sits on one tile. There are three kinds:

  | Kind | Examples |
  |---|---|
  | **unit** | Your units and your opponents' units |
  | **resource** | Energy wells, wreckage |
  | **terrain** | Rock, marsh |

- Most entities **block** their tile: at most one blocking entity per
  tile. Units, wells and impassable terrain block. **Passable** entities
  (wreckage, passable terrain such as marsh) don't: units walk over them,
  and several can share a tile.
- Every entity has an opaque 64-bit **ID**. IDs look random on purpose:
  they reveal neither how many entities exist nor which was made first.

## Time: ticks

The world advances in fixed **ticks**. The dev config runs 10 per second;
the server's `tick_rate_hz` sets the rate. Within a tick:

1. All commands submitted since the previous tick execute, in a fixed
   order.
2. Every mechanic advances one step, in a fixed phase order: automation,
   energy, logistics, salvage, movement, combat, upgrades, cloning.
3. Each player receives one update describing what they can now observe.

Everything is integer arithmetic with deterministic tie-breaking, so how
fast your connection is doesn't change what happens. It only changes when
you hear about it.

## Commands vs. orders

You **command** units; the server decides. A command is:

1. **Accepted** right away if it's well-formed. The acknowledgment says
   which tick it will run in.
2. **Executed** at that tick, when it either succeeds or fails with a
   reason (not your unit, not enough energy, out of range, ...).

Most commands set a persistent **order** that keeps working over many
ticks until it completes, fails or is replaced:

- "move to (12,5)"
- "mine this well"
- "attack that unit"
- "upgrade armor to 3"
- "supply that unit"

That's why a unit keeps mining while you look elsewhere.

Each player may execute a limited number of **unit-commands per tick**
(the command budget, `unit_command_budget`). A command over budget fails
and changes nothing. Persistent orders, batch commands over metadata
groups and automation behaviors let you control many units without
spending a command on each unit each tick.

## One unit type

There are **no unit classes**. Every unit has:

- a **position** and an **energy store** (stored energy and capacity)
- a set of **attributes** defined by the server configuration, all
  starting at their defaults. In the dev config these are armor,
  propulsion, energy capacity, mining efficiency, attack, attack range,
  structure, vision, sensor, stealth, optics and transfer range.
- **derived properties** computed from those attributes. The best known is
  **mass**, and heavier units are slower.
- **integrity** (health)

A unit becomes a miner, a scout, a tank or a carrier by **upgrading**,
which costs energy and time. Upgrades are irreversible. Cloning copies a
unit's upgrades into a new unit, so specializations can be mass-produced.
See [upgrades and cloning](upgrades.md).

## Energy is local

There is no global "money". Energy is stored **in units**, comes out of
**wells** (mined by adjacent units) and **wreckage** (salvaged), and moves
between units by **transfer** or **supply links**. A unit can only spend
its own energy, on moving, upgrading, cloning or attacking. Getting energy
to where it's needed is half the game. See [energy](energy.md) and
[automation and logistics](automation.md).

## Information

When the server enables **fog of war**:

- You see only tiles within your units' **vision**.
- You see foreign entities only if your sensors **detect** them.
- What you learn about a detected entity depends on your
  **identification** level (basic, estimated, detailed). Its integrity
  might show as "between 40 and 49" or as exact.
- Losing sight of a unit and that unit being destroyed look the same to
  you.
- Commands can't be used to probe. "Not yours", "doesn't exist" and "you
  can't see it" all produce the same `not_found` error.

See [visibility](visibility.md). With fog off, every player sees every
entity, but other players' private attributes (energy, upgrades, orders,
metadata) are always hidden.

## Where to go next

- [Gameplay and the web client](gameplay.md): controls and a first match
- [Units, movement and groups](units.md)
- [Glossary](../reference/glossary.md)
