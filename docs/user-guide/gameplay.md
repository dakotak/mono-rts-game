# Gameplay and the web client

The bundled player client is at `http://<gateway>:8080/`
([`web/static/player`](../../web/static/player)). It is a plain
HTML/JS/canvas page that uses the same public API as any bot. This page
covers its controls, then walks through a first match and some strategy.

## Joining

Enter your **join token** (`red` or `blue` in the dev config) and press
**Connect**. Each browser tab is an independent session, and reloading the
tab resumes it. Several sessions may play the same player at once.

## Controls

| Input | Action |
|---|---|
| Click own unit | Select (Shift adds to the selection) |
| Drag | Box-select own units |
| Right-click empty tile | Move the selection, spread over nearby free tiles |
| Right-click a well | Selected units walk next to it and mine |
| Right-click own unit | Adjacent selected units send it the "Transfer amount" of energy |
| Right-click enemy unit | Attack it. There's no pursuit: get in range yourself, or use *Guard* |
| Right-click wreckage | Selected units walk next to it and salvage |
| **S** | Stop moving, mining, attacking and salvaging |
| **Esc** | Deselect, or cancel an automation target pick |

### Sidebar

**With one unit selected:**

- **Attributes:** each attribute with the exact cost and duration of +1
  and an **Upgrade** button. Costs come straight from the server's
  published tables, so they're exact.
- **Derived properties:** mass, pace, and the invested and embodied
  energy.
- **Clone:** the exact price, and progress bars for upgrades and cloning.
- **Combat:** integrity, attack, range, cooldown and energy per attack.
- **Sensors:** vision, sensor strength, signature (stealth) and
  identification.
- **Automation & supply:** behavior, state, status, objective and
  supply links.
- **Metadata:** tags (key=value), with remove buttons.

**Panels for many units:**

- **Batch actions:** upgrade or clone every selected unit; the total cost
  is shown first.
- **Metadata:** set or remove a tag on the selection (for example
  `role=miner`).
- **Filter:** `key=value`, `key` (present) or `!key` (absent). You can
  select matching units, or upgrade them through a **server-side
  selector**, which the server evaluates when the command runs.
- **Automation:** pick a behavior and parameters, press **Assign…**, then
  click the targets it asks for (well, receiver, guard position, leader).
  Tick "Apply to units matching the filter" to assign it to a metadata
  group server-side instead of the selection.
- **Supply:** set reserve, fill level and resume level, press **Supply a
  unit…**, then click the receiver.
- **Automation status:** counts per behavior and a list of blocked units
  with the reason. Click one to select it.

### On the board

| Mark | Meaning |
|---|---|
| Bar under a unit | Its energy |
| Health bar | Integrity |
| Cyan / magenta ring | Upgrading / cloning (dashed while waiting for a free tile) |
| Floating "+N" / "−N" | Energy mined or transferred, damage taken |
| White, gold, green, red, brown lines | Movement destinations, mining, transfers, attacks, salvage |
| Cyan lines | Supply links: bright while energy flows, dim while resting |
| Dark / dim tiles (fog of war) | Unexplored / explored but not currently visible |
| Outlined "ghosts" | Last known positions of things you no longer see, with the tick they were seen |

The message log reports command results (including per-unit batch
results), automation blocked/failed reports, destroyed units and other
events addressed to you.

## A first match (dev config)

1. **Scout.** The dev map has fog, three wells and rock walls. Send two or
   three units out in different directions. Every step costs 1 energy, and
   units start with 30.
2. **Mine.** When you find a well, right-click it with a few units. A
   well releases at most its extraction rate per tick, shared fairly among
   adjacent miners. In the dev config a finite well releases 4 per tick
   and a unit mines 2, so a third miner adds nothing.
3. **Automate the loop.** Select the miners, choose *Mine & deliver*, then
   click the well and a "depot" unit nearby. Miners fill up, walk to the
   depot, unload and go back.
4. **Move energy forward.** Supply links (*Supply a unit…*) move energy
   between units within transfer range every tick, respecting a reserve.
   Chain depot → relay → front line.
5. **Specialize.** Upgrade a few units' `energy_capacity` first: big
   upgrades and clones are paid up front from a single unit's store. Then
   go for attack, structure and armor for fighters, or sensor, vision and
   optics for scouts.
6. **Clone.** A clone copies its parent's attributes. Cloning a fully
   upgraded unit is how you mass-produce a specialization, at a price of
   at least its embodied investment.
7. **Fight.** Attack within range. Damage is resolved simultaneously, so
   trades can be mutual. Destroyed units leave wreckage holding part of
   their energy and upgrades, worth salvaging.

## Strategy notes

- **Mass is speed.** Every attribute adds mass, and in the dev config pace
  is `clamp(mass / (propulsion * 8), 1, 20)`. A heavily armored unit is
  slow unless you also buy propulsion.
- **Capacity gates everything.** You can't start an upgrade or clone you
  can't pay for in full from one unit, so a few high-capacity units are
  your "banks".
- **Concentrate miners carefully.** Splits are max-min fair. Extra miners
  on a well shorten nobody's wait once the well's rate is saturated.
- **Information is a weapon.**
  - Stealth (signature) lowers how far enemies detect you.
  - Optics raises how precisely you identify them.
  - Enemies can't see your energy, upgrades, orders or tags unless the
    server's disclosure rules allow it, and by default they don't.
- **Ghosts lie.** A ghost is where something *was*. The server never tells
  you whether a lost contact moved, hid or died.
- **Budget your commands.** With `unit_command_budget` set (500 in dev),
  re-ordering hundreds of units every tick fails. Use persistent orders,
  metadata groups and automation.
- **Wreckage is energy.** Embodied upgrades are partly recoverable
  (`embodied_recovery_permille`). A battlefield full of wrecks favors
  whoever can salvage it.

Exact rules for every mechanic are in the
[mechanics reference](../developer-guide/mechanics.md).
