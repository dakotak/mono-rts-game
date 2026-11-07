# Administration

Everything an operator can do, through the browser admin client or the
`AdminService` API. Every world change is a **scheduled command**, so it
runs in the next tick in deterministic order and is part of the replay
log. Admins can't act "between" ticks.

## The admin client

Open the admin gateway (default http://127.0.0.1:8081) and sign in with
the `admin_token`. You see the whole world without fog, every attribute
and every event.

| Control | Action |
|---|---|
| **Pause / Resume** (Space) | Stop or restart the tick clock. |
| **Step N** | Execute N ticks while paused (1..1000). |
| **View as** | Show exactly what one player sees (`GetPlayerView`), with darkened unseen tiles and lines from each detected entity to the unit that detects it. |
| **Select** tool | Click or drag to inspect. The inspector groups an entity's data by plugin and offers actions: configure a well, set a unit's energy, reposition, delete, cancel automation, remove supply links. |
| **Spawn** tool | Click a tile to create a unit for the chosen player. |
| **Well** tool | Click a tile to create a well; blank fields use the configured defaults. |
| **Reposition** tool (or right-click) | Teleport the selected entity to an empty tile. Its orders are kept. |
| **Wreck** tool | Create wreckage with the given energy. |
| **Delete** / Backspace | Delete the selection. |
| **Rules** | The tick schedule, command budget, every attribute with formulas and tables, derived properties in evaluation order, and each plugin's effective config. |
| **Automation & logistics** panel | Per-tick automation work (evaluated and deferred), logistics transfers and energy moved, behavior/state counts, blocked units (click to select), supply links. Supply lines and automation objectives are drawn on the board. |

The log shows each mutation's scheduled tick and its result, plus every
event in the world.

**Match, New match and Replays panels** (Milestone 7):

- **Match:**
  - lifecycle buttons (Start, Pause match, Resume match, End match with a
    winner or none, Eliminate a player)
  - per-player readiness, elimination, open connections and live
    statistics
- **New match:** reset with a seed (or random), or with an edited
  configuration. Players and admin token can't change.
- **Replays:** export any recent match, and verify a replay file.

See [running matches](matches.md).

## World initialization

The world is built at startup from the config file
([configuration](../getting-started/configuration.md)):

1. Units come from `units` blocks.
2. Plugins place their own entities in `Init`: wells from
   `wells.wells`, terrain from `terrain.tiles`.

There's no runtime "reset" or "load". To start over, restart the server.
To change rules (rates, formulas, attributes), edit the config and
restart.

## Editing the world at runtime

| Task | API |
|---|---|
| Spawn a unit | `SpawnUnit{owner_id, position}` |
| Delete any entity | `DeleteEntity{entity_id}` |
| Teleport any entity | `MoveEntity{entity_id, position}` |
| Create or configure a well | `SubmitAdminCommand(rts.wells.v1.CreateWellCommand / ConfigureWellCommand)` |
| Set a unit's energy or capacity | `SubmitAdminCommand(rts.energy.v1.SetUnitEnergyCommand)` |
| Set an attribute level | `SubmitAdminCommand(rts.attributes.v1.SetAttributeCommand)`. Doesn't count as invested, so it isn't recoverable as wreckage. |
| Set integrity (0 destroys) | `SubmitAdminCommand(rts.combat.v1.SetHealthCommand)` |
| Create a wreck or set its energy | `rts.salvage.v1.CreateWreckCommand`, `SetWreckEnergyCommand` |
| Act as any player | `SubmitAdminCommand` with any player command; ownership checks pass |

Full RPC and payload details: [AdminService](../api/admin-service.md),
[command reference](../reference/commands.md).

## Pause, resume and stepping

- **Pausing** stops ticks. Players can still connect and submit commands,
  which wait for the next tick that executes.
- **Stepping** (paused only) executes exactly N ticks as fast as
  possible. One step request may be pending at a time.
- Neither changes simulation results, only timing.

Typical uses: inspect a situation tick by tick, reproduce a bug with
"View as", or freeze a match.

## Debugging fog of war

**View as** and `GetPlayerView` return the exact snapshot a player would
receive, plus `detections`: for each foreign entity, the observing unit,
detection score, identification score and level. Use them to explain
"why can (or can't) this player see that?".

## Pause the clock vs. pause the match

| | Clock pause (`SetPaused`, *Pause* button) | Match pause (`PauseMatchCommand`) |
|---|---|---|
| Ticks | Stop | Continue |
| Systems | Don't run (no ticks) | Skipped |
| Player commands | Queued for the next tick | Rejected (`failed_precondition`) |
| Single-step | Available (`Step`) | — |
| Purpose | Debugging, inspection | A suspended match (the clock stops, players are told) |

## What admins can't do

- Change plugin configuration or formulas at runtime
- Save or load the world, or export a replay
- Kick players or revoke sessions (sessions last until restart; tokens are
  static config)
- See or change anything outside the world (accounts, persistence)
