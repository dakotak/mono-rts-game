# Upgrades and cloning

Upgrades turn the universal unit into a specialist. Cloning copies a
specialist.

## Attributes

The server configuration defines the attributes, up to 16 of them. Each
has:

- a **default** (every unit starts here), a **min** and a **max**
- a **mass** contribution formula
- a **cumulative cost** and a **cumulative duration**: the total energy
  and ticks needed to reach a level from the minimum

The dev config defines:

| Attribute | Effect (as bound in the dev config) |
|---|---|
| `armor` | Reduces damage: `max(1, attack * 10 / (10 + armor))`. Heavy. |
| `propulsion` | Divides mass to give pace (faster) |
| `energy_capacity` | Storage |
| `mining_efficiency` | Mining and salvage rate |
| `attack`, `attack_range` | Damage per attack, range |
| `structure` | Maximum integrity |
| `vision`, `sensor`, `stealth`, `optics` | Visibility: sight radius, detection strength, signature, identification |
| `transfer_range` | Supply-link range |

On another server they may differ entirely. The client lists whatever the
server publishes, with exact costs. Attributes **only matter where the
server binds them** to a mechanic, so an attribute nothing reads is purely
cosmetic.

**Derived properties** are computed from attributes:

- `mass` is always there: base mass plus every attribute's contribution.
- The config can add more, like `ticks_per_move` (pace) and
  `attack_cooldown` in dev.
- You see them in the unit's sidebar (`attributes.<name>`).

## Upgrading

1. Pick a unit and an attribute. Raise it to a **target** level, or
   **increase** it by an amount.
2. The **price** is `cost(target) − cost(current)` and the **time** is
   `max(1, duration(target) − duration(current))` ticks. Costs are
   cumulative, so going 1→3 costs the same as 1→2 then 2→3.
3. The unit pays the **full price up front from its own store** when the
   upgrade starts. If it can't, the command fails with
   `insufficient_energy` and nothing happens.
4. The new level applies **when the duration has passed**. Events:
   `upgrades.started`, `upgrades.progress` every few ticks, then
   `upgrades.completed`.

Rules:

- **Irreversible.** Levels never go down. There's no cancel and no refund.
- **One project per unit.** A unit can run one upgrade *or* one clone at a
  time.
- **Destroyed mid-upgrade** means the paid energy is lost.

**Batch upgrades.** Select many units and use *Batch actions*, or a
server-side selector such as "everything tagged `role=miner`". Each unit
is checked and paid separately, so some can succeed while others fail.

## Embodied and invested energy

| Value | Meaning |
|---|---|
| **Embodied** | What the unit's current levels are worth: the sum of `cost(level) − cost(default)`. |
| **Invested** | What was actually paid for them. Admin-granted levels are embodied but not invested. |

Both show in the sidebar. They determine clone prices and how much
wreckage a destroyed unit leaves.

## Cloning

A clone order makes the unit produce an offspring next to itself:

- **Price.** At least the parent's **embodied** investment. The dev config
  uses `base + embodied + mass`. On top of that, the parent hands
  `offspring_energy` to the child. Both are paid up front.
- **Duration.** A formula of `base`, `embodied` and `mass`; slower for
  heavily upgraded parents in dev.
- **Inheritance.** The child copies the parent's attribute levels (as they
  were when cloning started), and its invested energy equals the parent's
  embodied investment. It gets a fresh ID. **Tags aren't inherited.**
- **Placement.** The child appears in the direction you choose, or else
  the first free neighbor (N, NE, E, SE, S, SW, W, NW). If every neighbor
  is occupied, the clone waits (`cloning.blocked`) until one frees up.
  Paid energy isn't lost.

The sidebar shows each unit's exact current clone quote.

Because a clone always costs at least the upgrades it carries, cloning
can't create energy or launder upgrades. What it buys you is **time**:
cloning a finished specialist is faster than upgrading a fresh unit
through every level, and it parallelizes.

## Strategy

- **Upgrade capacity first** on the units that will pay for big things.
- **Prototype, then clone.** Build one specialist, then clone it. Upgrade
  time per level is often superlinear, so cloning saves a lot of waiting.
- **Mind the mass.** Armor and big batteries slow units down unless you
  also buy propulsion.
- **Stealth and optics win scouting.** Higher stealth shrinks the
  distance at which enemies detect you; higher optics gives you exact
  numbers about them.
- Exact formulas: [mechanics: unit evolution](../developer-guide/mechanics.md#unit-evolution-milestone-3),
  [formulas](../reference/formulas.md).
