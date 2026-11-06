# Unit Strategy Game — RTS simulation engine

A deterministic, headless, server-authoritative real-time strategy engine in
Go. One server process hosts one world; any number of players (humans or AI
agents, indistinguishable to the server) connect over a public gRPC API.
Game mechanics are compiled-in plugins.

**Status: Milestone 4** — combat, destruction and salvage:

- **Combat** (`internal/plugins/combat`): persistent attack orders against
  enemy units. Health, attack, range and cooldown are bound to attributes or
  constants, and damage and energy-cost formulas are configurable. Damage is
  resolved simultaneously, so mutual destruction is possible. There is no
  pursuit.
- **Wreckage and salvage** (`internal/plugins/salvage`): a destroyed unit
  leaves passable wreckage holding a configured fraction of its paid
  investment and its stored energy. Salvage orders collect it under a
  fair, conserving split.
- **Clients:** health bars, attack and salvage lines, damage and destruction
  feedback, right-click to attack or salvage, a combat section in unit
  details. Admins can set integrity, create wrecks and edit wreck energy.

Milestone 3 added:

- **Data-driven attributes:** a configurable attribute registry (armor,
  propulsion, energy capacity, mining efficiency, ...) with mass
  contributions, derived properties, and nonlinear upgrade cost and
  duration formulas. Formulas use a small, validated integer expression
  language.
- **Upgrades:** persistent, irreversible upgrade orders, paid from the
  unit's own energy.
- **Cloning:** reproduction whose cost includes the parent's embodied
  upgrade investment.
- **Metadata, selectors and batch commands:** player-owned unit metadata,
  deterministic selectors, batch commands with per-unit results, and a
  per-player unit-command budget per tick.
- **Clients:** player UI for all of the above; admin inspection of
  attributes, orders and investment, and a rules/formulas viewer.

Milestone 2 added:

- **Administration:** an extended `AdminService` and a separate browser
  admin client with full-world visibility. It can inspect, create, delete
  and reposition entities, create and configure energy wells, set unit
  energy, and pause, resume or single-step the simulation.
- **Energy economy:** `wells` and `energy` plugins (unit energy storage,
  mining orders, energy transfers) and an energy cost for movement.

The [architecture document](docs/ARCHITECTURE.md) covers design, extension
points, assumptions, and the deferred features and proposed next milestones.

## Requirements

- Go 1.26.2 or newer. That's the only requirement: code generation and
  linting tools (`buf`, `protoc-gen-go`, `protoc-gen-go-grpc`, `staticcheck`)
  are pinned in `tools/go.mod` and run through `go tool`, so there's nothing
  else to install.
- A modern browser.

## Run it

```bash
make dev
```

That builds and starts:

| Process       | Address                 | Role                                          |
|---------------|-------------------------|-----------------------------------------------|
| `rts-server`  | `127.0.0.1:50051` (gRPC) | simulation + `GameService` + `AdminService`  |
| `rts-gateway` | `http://127.0.0.1:8080`  | player client + player API bridge (GameService only) |
| `rts-gateway` | `http://127.0.0.1:8081`  | admin client + admin API bridge (AdminService only)  |

**Players:** open http://127.0.0.1:8080 in two browser tabs (or two
browsers). Join as `red` in one and `blue` in the other. Each tab is an
independent session, and reloading a tab resumes it.

| Input | Action |
|---|---|
| Click own unit | Select (Shift adds to the selection) |
| Drag | Box-select own units |
| Right-click empty tile | Move the selection (spread over nearby tiles) |
| Right-click a well | Selected units walk next to it and mine |
| Right-click own unit | Adjacent selected units send it the "Transfer amount" of energy |
| **S** | Stop moving and mining |
| **Esc** | Deselect |

With one unit selected, the sidebar shows:

- its attributes, each with an exact "+1" cost and duration and an upgrade
  button
- derived properties (mass, pace), invested and embodied energy
- a Clone button with its exact cost
- progress bars for upgrades and cloning
- its metadata, with remove buttons

Below that:

- **Batch actions** upgrades or clones every selected unit and shows the
  total cost first.
- **Metadata** sets or removes a key on the selection.
- **Filter** takes `key=value`, `key` (present) or `!key` (absent). It can
  select matching units, or upgrade them through a server-side selector
  evaluated at execution time.

Batch results report how many units succeeded and why the rest failed.

On the board, upgrading units show a cyan ring and cloning units a magenta
one (dashed while waiting for a free tile).

Under each own unit, a bar shows its energy. "+N" and "−N" floating
numbers come from the server's `energy.mined` and `energy.transferred`
events. The sidebar shows the selection's energy and orders, and what's
under the cursor, including a well's remaining energy.

**Admin:** open http://127.0.0.1:8081 and sign in with `dev-admin` (the
`admin_token` in `configs/dev.json`). You see every entity with all of its
attributes and orders.

| Control | Action |
|---|---|
| Pause / Resume, **Space** | Stop or restart the simulation clock |
| Step N | Run N ticks while paused |
| Select tool | Click or drag to inspect; the inspector can configure a well, set a unit's energy, reposition or delete |
| Spawn tool | Click a tile to spawn a unit for the chosen player |
| Well tool | Click a tile to create a well; blank fields use server defaults |
| Reposition tool, or right-click | Teleport the selected entity to the tile |
| **Delete** | Delete the selection |

Every mutation is scheduled through the simulation. The log shows the tick
it was scheduled for and its result.

The inspector groups each entity's data by plugin: attributes (with bounds,
next-level cost and formulas on hover), derived properties, invested and
embodied energy, upgrade and cloning progress, metadata and energy.
**Rules** shows the tick schedule, the command budget, every attribute
definition and formula, the derived properties in evaluation order, and
every plugin's effective configuration.

To run the processes separately (for example in two terminals):

```bash
go run ./cmd/rts-server -config configs/dev.json -addr 127.0.0.1:50051
```

```bash
go run ./cmd/rts-gateway -server 127.0.0.1:50051 -addr 127.0.0.1:8080 -admin-addr 127.0.0.1:8081 -web web/static
```

`-web web/static` serves the clients from disk so edits show on reload.
Without it, the gateway serves the copy embedded at build time.
`-admin-addr ""` disables the admin listener entirely.

The server registers gRPC reflection, so tools like `grpcurl` can explore
the API:

```bash
grpcurl -plaintext -H 'authorization: Bearer dev-admin' 127.0.0.1:50051 rts.v1.AdminService/GetWorldSnapshot
```

## Development

```bash
make test       # all tests
make race       # all tests with the race detector
make lint       # gofmt check, go vet, staticcheck, buf lint
make generate   # regenerate gen/ from proto/
make bench      # per-tick benchmark (1k/5k/10k units)
make sim        # headless harness: 5000 units x 2000 ticks, no network
make build      # binaries in bin/
```

## Layout

```
proto/rts/v1/              public API: types, GameService, AdminService
proto/rts/movement/v1/     movement plugin's command messages
proto/rts/wells/v1/        wells plugin's (admin) command messages
proto/rts/energy/v1/       energy plugin's command messages
proto/rts/attributes/v1/   admin attribute override
proto/rts/upgrades/v1/     upgrade command
proto/rts/cloning/v1/      clone command
proto/rts/metadata/v1/     metadata, selectors, batch command
gen/                       generated Go bindings (committed; `make generate`)
internal/sim/              simulation kernel (no game logic, no protobuf)
internal/expr/             deterministic integer formula language
internal/plugins/movement/ movement plugin (+ energy cost)
internal/plugins/wells/    energy wells: data model, admin commands, initial placement
internal/plugins/energy/   unit energy storage, mining, transfers (+ attribute bindings)
internal/plugins/attributes/ attribute registry, derived properties, embodied investment
internal/plugins/upgrades/ upgrade orders
internal/plugins/cloning/  reproduction
internal/plugins/metadata/ unit metadata, selectors, batch commands
internal/protoreg/         registers every plugin's proto types (used by the gateway)
internal/plugintest/       test harness over the real plugin composition
internal/game/             config loading, plugin selection, world setup
internal/server/           gRPC services, sessions, observation hub
internal/gateway/          browser HTTP/NDJSON ↔ gRPC bridge
web/static/player/         player client (plain HTML/JS/canvas, no build step; evolution.js = Milestone 3 UI)
web/static/admin/          admin client
web/static/shared/         rendering, input and stream code shared by both clients
cmd/rts-server/            server binary
cmd/rts-gateway/           gateway binary
cmd/rts-sim/               headless simulation harness
configs/dev.json           two-player development world
docs/ARCHITECTURE.md       design, extension points, roadmap
```

## Configuration

The config file is JSON. Unknown fields are rejected, both at the top level
and inside each plugin's section. All energy quantities are integers
(abstract energy units); rates are per tick.

```jsonc
{
  "world": {
    "width": 32, "height": 20,   // grid size in tiles
    "seed": 42,                  // gameplay randomness (World.Rand)
    "tick_rate_hz": 10,          // 1..1000
    "id_key": "",                // hex secret (>= 32 hex chars) for ID derivation; empty = random per start
    "unit_command_budget": 500   // unit-commands per player per tick (0 = unlimited)
  },
  "players": [ { "slot": 1, "name": "Red", "join_token": "red" } ],  // slot: config-local handle
  "admin_token": "dev-admin",    // empty disables AdminService
  "units": [ { "owner": 1, "x": 2, "y": 3, "w": 4, "h": 3 } ],  // owner: player slot (0 = none); w/h: block size
  "plugins": {
    "wells": {
      "defaults": { "energy": 1000, "capacity": 1000, "extraction_rate": 5, "unlimited": false },
      "wells": [ { "x": 15, "y": 4 }, { "x": 10, "y": 10, "unlimited": true, "extraction_rate": 6 } ]
    },
    "attributes": {              // default: no attributes
      "base_mass": 10,
      "attributes": {            // up to 16; formulas are functions of v
        "armor": { "default": 0, "min": 0, "max": 20,
                   "mass": "v * 3",                    // mass contribution
                   "cost": "v * v * 5 + v * 10",       // cumulative energy to reach v (non-decreasing)
                   "duration": "v * 8",                // cumulative ticks to reach v (non-decreasing)
                   "description": "shown in clients" }
      },
      "derived": { "ticks_per_move": "clamp(mass / (propulsion * 6), 1, 20)" }  // over attributes, mass, other derived
    },
    "energy": {
      "unit_capacity": 100,      // storage of new units (unless capacity_attribute)
      "initial_energy": 50,      // free energy of units created by config/admin (not clones)
      "extraction_rate": 2,      // per tick (unless extraction_attribute)
      "transfer_rate": 5,
      "capacity_attribute": "",  // e.g. "energy_capacity": capacity follows that attribute
      "extraction_attribute": "" // e.g. "mining_efficiency"
    },
    "upgrades": { "progress_event_interval": 10 },
    "cloning": {
      "base_cost": 50,
      "cost": "base + embodied",           // vars: base, embodied, mass; effective = max(cost, embodied)
      "duration": "20 + embodied / 20",    // effective = max(1, duration)
      "offspring_energy": 10,              // moved from parent to offspring
      "progress_event_interval": 10
    },
    "metadata": { "max_entries": 8, "max_key_length": 32, "max_value_length": 64, "max_batch_units": 4096 },
    "movement": {
      "ticks_per_move": 1,       // unless pace_attribute
      "max_blocked_ticks": 20,
      "contention_backoff_ticks": 2,
      "energy_per_step": 0,
      "pace_attribute": ""       // e.g. "ticks_per_move" (a derived property)
    }
  }
}
```

Apart from the `attributes` example, the values shown in `plugins` are the
built-in defaults. `configs/dev.json` uses:

- four attributes (armor, propulsion, energy_capacity, mining_efficiency)
- energy capacity, extraction rate and movement pace bound to attributes
- a 500 unit-command budget per player per tick
- three wells, one of them unlimited

Each entry in `wells.wells` may override any of the `defaults` fields.

**Formulas** use a small integer language: `+ - * / %`, parentheses,
`min`, `max`, `clamp`, `abs`, `pow` and `sqrt`. Division by zero yields 0
and overflow saturates, so a formula can never fail at runtime. Formulas
are compiled and validated when the config loads: names, variables,
monotonic cost and duration tables, and dependency cycles.

Entity and player IDs are opaque random-looking 64-bit/32-bit values
derived from `id_key`, so they don't reveal how many players or units
exist. A slot number is only used inside the config file and never leaves
the server. Set `id_key` only when runs must be reproducible, and keep it
secret.

## Performance (Apple M-series laptop, `make sim` / `make bench`)

| Units  | World   | Mean tick | Notes                                   |
|--------|---------|-----------|-----------------------------------------|
| 1,000  | 200×200 | ~0.6 ms   | every unit has a move order             |
| 5,000  | 200×200 | ~2.2 ms   | every unit has a move order             |
| 10,000 | 200×200 | ~4.7 ms   | every unit has a move order             |
| 5,000  | 200×200 | p50 3.8 ms, p99 6.9 ms | harness: 4 players, reorders every 20 ticks |
| 4,000 → 6,000 | 200×200 | ~4.7 ms | 2,000 concurrent upgrades + 2,000 concurrent clones (`BenchmarkEvolutionTick`) |

These are Milestone 3 numbers. Every unit now carries an energy store,
attributes, a clone quote and metadata, which roughly doubled the
per-unit cost since Milestone 2. At 10 Hz the budget is 100 ms per tick.
Nothing has been optimized yet.
