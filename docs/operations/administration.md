# Administration

Everything an operator can do, through the browser admin client or the
`AdminService` API. Every world change is a **scheduled command**, so it
runs in the next tick in deterministic order and is part of the replay
log. Admins can't act "between" ticks.

## The admin client

Open the admin gateway (default http://127.0.0.1:8081) and sign in with
the `admin_token`. You see the whole world without fog, every attribute
and every event. A launcher can sign you in with a link,
`http://127.0.0.1:8081/#token=<admin token>` (the part after `#` never
reaches the server and is removed from the address bar immediately; `make
watch-bots` uses this).

### Workspace

The client is a dockable workspace, like an IDE: every tool is a panel
you can move, dock beside or above/below another, stack as tabs, resize,
close, reopen and maximize.

```text
┌───────────────────────── Simulation ─────────────────────────┐
├─ Players │ Tools ──┬──────── Game board ────────┬─ Inspector │ Match │ … ─┤
│                    │                            │                         │
├──────────────────────────── Logs ───────────────────────────────────────┤
└─────────────────────────────────────────────────────────────────────────┘
```

| Panel | Contents |
|---|---|
| **Game board** | The world. Wheel zooms about the cursor, middle-drag or Alt+drag pans, **Fit** (or F) shows everything. Click or drag to select; right-click repositions the selected entity. |
| **Simulation** | Simulation clock (Pause/Resume, Step N) and match lifecycle (Start, Pause match, Resume match, Restart match). |
| **Players** | Per-player statistics; select a player to highlight, center on or filter by them ([below](#players-panel)). |
| **Inspector** | The selected entity, entities or tile ([below](#inspector)). |
| **Logs** | Every admin action, command result and simulation event, filterable ([below](#logs)). |
| **Tools** | Board tools: Select, Spawn unit for a player, Create well (with optional energy, capacity, rate, unlimited), Reposition, Create wreck. |
| **Match** | Match summary; End with a winner or none (and a reason); Eliminate a player (optionally removing their units). |
| **New match** | Reset with a seed (or random) and/or an edited configuration; players and the admin token can't change. |
| **Replays** | Export any recent match's replay; verify a replay file. |
| **Automation & logistics** | Per-tick automation work (evaluated, deferred by the cap), logistics transfers and energy moved, behavior/state counts, blocked units (click to select), supply links. |
| **Rules** | The tick schedule, command budget, every attribute with formulas and tables, derived properties, and each plugin's effective configuration. Opened by **Rules** in the header. |

A status bar along the bottom shows the connection state, the current
tick, the simulation clock (running at N Hz or paused) and the entity
count; its values sit in fixed-width slots so nothing shifts as they
change. The header keeps **View as**
(show exactly what one player sees, `GetPlayerView`: unseen tiles
darkened, lines from each detected entity to the unit that detects it),
the **Panels** menu, **Rules** and **Sign out**.

**Arranging panels.** Drag a panel's tab: drop it on the middle of
another panel to add it as a tab there, or on an edge to dock it beside,
above or below; drop it on the outer edge of the workspace to make a full
row or column. Drag the boundary between panels to resize them. The ⤢
button in a group's header maximizes it (⤡ restores). Close a panel with
the × on its tab; reopen it from **Panels** (checked = open), which also
has **Restore default layout**.

**Default layout.** Simulation across the top, Players (with Tools) on
the left, the board in the middle, Inspector (with Match, Automation,
New match and Replays) on the right, Logs across the bottom. On windows
narrower than 1100 px the default stacks everything except the Simulation
bar as tabs beside the board. The arrangement is the constant
`DEFAULT_LAYOUT` in `web/static/admin/logic.js`.

**Persistence.** The layout — positions, sizes, tab groups, active tabs
and which panels are open — is saved in the browser's local storage
(`mono.admin.workspace`) on every change and restored on the next visit;
nothing is stored on the server. A maximized panel is restored to the
normal layout on reload. A saved layout that can't be used (unreadable, a
different `LAYOUT_VERSION`, unknown panels) is ignored with a warning in
the Logs and the default layout is used; panels added by a later version
open in their default place, panels you closed stay closed.

Moving, closing or reopening panels never reloads data: each panel's
contents are created once and only re-attached, and all panels read the
same central state, fed by one observation stream.

### Players panel

One row per player, in configuration order with the player's colour:
connection (green: at least one open observation stream), **Units** and
**Energy** (current), **Lost**, **Kills** and **Mined** (match totals).
Click a row (or Enter on it) to select the player: their units are
outlined on the board and other players' units dimmed, and the row opens
the details below. **Center** zooms the board to their units, **Only
this player** hides other players' units (wells, wreckage and terrain stay
visible), **View as** previews their fog of war. Click the row again to
deselect.

Statistics never estimate. Current values are recomputed from the
authoritative entity state the admin stream delivers (every tick, all
entities, all attributes); match totals are the match plugin's running
counters from `GetMatchDetails`:

| Statistic | Kind | Source |
|---|---|---|
| Units | current | living units owned |
| Stored energy | current | Σ `energy.stored` of those units |
| Energy capacity | current | Σ `energy.capacity` |
| Storage used | current | stored ÷ capacity |
| Embodied investment | current | Σ `attributes.embodied` (what the units' upgrade levels are worth) |
| Invested energy | current | Σ `attributes.invested` (energy actually paid for them; admin-granted levels are embodied but not invested) |
| Mining / Moving / Attacking | current | units with `energy.mining_well` / `movement.destination` / `combat.target` |
| Automated | current | units with `automation.behavior` |
| Upgrading / Cloning | current | units whose `attributes.busy` is `upgrade` / `clone` |
| Supply links | current | `logistics.supply.N.to` links from their units |
| Score | match total | the match score under the configured weights (refreshed at the scoring interval) |
| Energy mined | match total | energy extracted from wells |
| Enemy units destroyed | match total | kills |
| Units lost | match total | own units destroyed |
| Investment destroyed | match total | embodied investment of enemy units destroyed |
| Ready, Eliminated, Observation streams | status | lobby readiness, elimination, open `Observe` streams |

Energy *consumption* (movement, attacks, upgrades, clones) is not
tracked by the server, so it is not shown.

### Inspector

Shows the selection: one entity in sections — *Identity and ownership*,
*Position*, *Energy*, *Current orders* (destination, upgrade in
progress), *Cloning*, *Automation*, *Logistics*, *Combat*, *Attributes*
(with range and next-level cost; hover for the formulas), *Well*,
*Wreckage*, *Sensors*, *Metadata*, then any other plugin's attributes —
and the edits the server supports for it: configure a well, set a unit's
integrity or energy, set a wreck's energy, Reposition, Delete, Cancel
automation, Remove supply links. Several entities show counts by kind and
**Delete all**; an empty tile shows its position (and, under View as,
whether that player can see it). With **View as**, a section at the top
shows what that player knows about the entity. The inspector follows
every authoritative update but does not redraw while you are typing in
one of its fields. Selecting from the board, a log row, or a blocked unit
in the Automation panel all update it.

### Logs

Columns: tick, severity (`error`, `warn`, `ok`, `info`, `debug`),
category, source and message. Categories: `command` (your mutations: the
tick they were scheduled for and their result), `event` (simulation
events; the source is the plugin that emitted them), `match`, `control`
(pause, step), `replay`, `visibility` (View as detections), `system`
(connection, snapshots, workspace notices) and `admin`.

Filter by severity (checkboxes), category, player (events about that
player's entities), the selected entity, and text (matches message,
source and category, case-insensitively). Click a row that names an
entity to select it.

The view follows new entries; scroll up and following pauses (the button
shows **Paused — jump to latest**), so new entries never pull you away;
scroll back to the bottom or press the button to resume. The browser
keeps the newest 5,000 entries and shows up to 600 matching rows at once;
older ones are dropped (the count says how many). **Clear view** empties
this browser's buffer only: the server keeps no log to delete, and the
match replay is unaffected.

### Simulation controls

| Control | Available when |
|---|---|
| **Pause / Resume** (Space) | always: stops or restarts the tick clock |
| **Step N** | the clock is paused: executes N ticks (1..1000) |
| **Start** | the match is waiting in the lobby (skips the ready check) |
| **Pause match / Resume match** | the match is running / paused |
| **Restart match** | always: a new match with the current configuration (confirmation; the old match's replay stays exportable) |

The panel shows what the server reports (polled every second and after
each action), never what was requested; disabled buttons explain why in
their tooltip, and a note explains a paused clock or match. **Pause
match** is a match rule (players' commands are refused) and **Pause** is
the simulation clock ([difference](#pause-the-clock-vs-pause-the-match)).

### Keyboard

Space pauses or resumes, Escape clears the selection, Delete or
Backspace deletes it, F fits the board; panel tabs and player rows are
focusable. Shortcuts are ignored while typing in a field.

### Implementation notes

The workspace uses [dockview](https://dockview.dev) 8.6.2 (MIT), vendored
unmodified in `web/static/admin/vendor/dockview` (no build step, no CDN);
see its `README.md` for upgrading. The client is plain JavaScript:
`store.js` (central state and change topics), `logic.js` (pure logic,
unit-tested with Node), `logs.js`, `players.js`, `admin.js` (session,
stream, board, inspector, controls), `match.js`, `workspace.js`. The
gateway serves client files with `Cache-Control: no-cache`, so browsers
revalidate them and never mix an updated page with stale cached scripts.

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
