# Visibility, sensors and intelligence

Fog of war is a server setting (`visibility.enabled`). It's on in the dev
config and off by default. With fog **off**, every player sees every
entity; private attributes (energy, upgrades, orders, tags) are still
owner-only. Everything below is about fog **on**.

## Three questions per entity

| Question | Rule |
|---|---|
| **Is the tile visible?** | A tile within any of your units' **vision** range (default metric: Chebyshev, a square). There's no line-of-sight blocking: terrain doesn't occlude yet. |
| **Is the entity detected?** | It's on a visible tile *and*, for at least one of your units that sees it, the **detection formula** reaches the threshold. Default: `observer_sensor − target_signature − distance ≥ 0`. In dev it's `observer_sensor × 2 − target_signature × 3 − distance ≥ 0`. |
| **How well is it identified?** | The best observer's **identification formula** selects a level: **basic**, **estimated** or **detailed**. Dev uses `observer_identification × 2 − distance`, with detailed at ≥ 4. |

A visible tile doesn't mean you see everything on it. A stealthy unit
(high signature) can stand on a visible tile undetected.

## What you learn at each level

You always get a detected entity's position, kind and owner. Its
attributes reach you only through the server's **disclosure rules**, which
give each attribute a precision per level:

| Precision | You see |
|---|---|
| hidden (0) | Nothing |
| exact (1) | The value |
| N | A range of width N, e.g. "40–49" for health with N = 10 |

The dev config, for example:

- **Integrity:** a range 25 wide at basic, 10 wide at estimated, exact at
  detailed.
- **Enemy energy and attribute levels:** only at estimated or detailed.

Orders, upgrade progress, metadata and targets have no rules, so they're
**never** disclosed to opponents.

A foreign entity is re-sent to you only when what *you* may see of it
changes. Hidden changes produce no traffic you could notice.

## What you are told, and what you aren't

- **Discovery.** `visibility.discovered` when an entity first becomes
  detected. `visibility.identified` when its level changes.
- **Loss.** `visibility.lost` and the entity's removal from your view when
  it leaves detection. Destruction looks **exactly the same**. The client
  keeps a "ghost" of its last known state, marked with the tick it was
  seen.
- **Tile updates.** Tiles entering or leaving vision arrive every tick;
  the client remembers explored tiles itself.
- **No probing.** Commands about entities you don't currently detect fail
  with `not_found`, the same error as for IDs that don't exist or aren't
  yours.
- **Detection lags a tick.** Commands that refer to foreign entities
  (attack, mine, salvage) and continuing attack orders use what you
  detected at the **end of the previous tick**: what you actually knew.

One known leak, by design: if your unit is **blocked** moving into a tile,
something is there. Movement collisions are physics, and they can reveal a
hidden unit.

## Sensor stats

Four stats drive everything. Each is a constant or bound to an attribute;
in dev they're all upgradable:

| Stat | Dev attribute | Effect |
|---|---|---|
| Vision | `vision` | Radius of visible tiles (max 32) |
| Sensor | `sensor` | Detection strength |
| Signature | `stealth` | How hard you are to detect (higher = stealthier) |
| Identification | `optics` | How precisely you identify what you detect |

Scouts and stealth units come purely from upgrades.

## Strategy

- **Spread sensors.** Vision is per unit, so a thin screen of cheap units
  sees more than a clump.
- **Stealth scales hard in dev.** Each point of signature cancels 1.5
  points of sensor. A few stealth upgrades let raiders approach far
  closer than enemies expect.
- **Optics before engaging.** Knowing a target's exact integrity and armor
  (detailed level) tells you whether you can win the trade.
- **Distrust ghosts.** The server never confirms whether a lost contact
  died.

Exact rules: [mechanics: fog of war](../developer-guide/mechanics.md#fog-of-war-sensors-and-intelligence-milestone-5).
