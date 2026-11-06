# Combat, destruction and salvage

## Attacking

An attack order targets one enemy unit you can currently detect. Every
unit can attack if its `attack` stat is above 0.

| Stat | Meaning |
|---|---|
| `combat.attack` | Damage input |
| `combat.range` | Reach in tiles (Chebyshev: diagonals count as 1) |
| `combat.cooldown` | Ticks between attacks |
| `combat.attack_cost` | Energy per attack |
| `combat.health` / `combat.max_health` | Integrity |

How an attack plays out:

- **No pursuit.** Out of range, the order waits (`combat.out_of_range`)
  until you move closer or the target comes to you. *Guard* automation
  does close in, within its area.
- **Energy.** Short of energy, the attacker waits (`combat.stalled`).
- **Instant hits.** When off cooldown, in range and paid for, the attack
  lands at once. There are no projectiles.
- **Ending.** The order lasts until the target is gone or undetected
  (`target_unavailable`), your attack drops to 0, you replace it, or you
  stop it.
- **Friendly fire** is rejected.

### Simultaneous resolution

All attacks in a tick are computed from the state at the start of the
combat phase, then applied together:

- **Mutual destruction is possible.** A unit destroyed this tick still
  fires this tick.
- **Damage stacks** from every attacker.
- **No ordering effects.** Nobody wins because of ID order or because
  their command arrived first.

Damage comes from the server's formula. In dev it's
`max(1, attacker_attack * 10 / (10 + target_armor))`, so armor has
diminishing returns but every hit does at least 1.

### Events you see

| Event | Recipient |
|---|---|
| `combat.attack` | You, about your attacks |
| `combat.damaged` | You, when your units are hit |
| `combat.destroyed` | The owner and the attacking players |

## Destruction

A unit at 0 integrity is removed with everything in progress:

- its orders
- an unfinished upgrade or clone, whose paid energy is lost
- its stored energy, except for the part that goes into wreckage

## Wreckage

A destroyed unit leaves a **wreck**: a passable resource entity (units
can stand on it) holding

```
min(embodied, invested) × embodied_recovery_permille / 1000
  + stored × stored_recovery_permille / 1000
```

rounded down. Both rates are 500 (half) by default. Admin-granted levels
aren't recoverable. No wreck is left if that amount is 0.

## Salvage

A salvage order collects from a wreck while the unit is adjacent to it or
on it:

- **Rate.** Up to its salvage rate (`mining_efficiency` in dev) per tick,
  limited by free capacity.
- **Shared wrecks.** Several collectors share a wreck fairly, like miners
  share a well.
- **Emptied wrecks** disappear. You get `salvage.collected`, and
  `salvage.wreck_depleted` when you empty one.

*Auto-salvage* automation finds and collects nearby wrecks on its own.

## Strategy

- **Fight with full batteries.** Attacks cost energy, so a fighter that
  runs dry stops shooting.
- **Focus fire.** Damage sums per target in a tick. Many attackers on one
  target remove it before it can shoot again.
- **Range beats armor** in open ground. Armor wins in close brawls, and
  mass makes armored units slow.
- **Salvage after battles.** Expensive upgraded units leave rich wrecks.
  The side that controls the field afterwards recovers energy.
- **Mind detection.** You can only attack what you detect. Stealthy units
  can close in unseen, and scouts with good sensors expose them.

Exact rules: [mechanics: combat](../developer-guide/mechanics.md#combat-destruction-and-salvage-milestone-4).
