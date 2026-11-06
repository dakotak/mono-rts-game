# Unit Strategy Game — RTS simulation engine

A deterministic, headless, server-authoritative real-time strategy engine in
Go. One server process hosts one world; any number of players (humans or AI
agents, indistinguishable to the server) connect over a public gRPC API.
Game mechanics are compiled-in plugins.

**Status: Milestone 2** — on top of Milestone 1 (kernel, plugin system,
movement, gRPC APIs, browser gateway, player client), Milestone 2 adds:

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
gen/                       generated Go bindings (committed; `make generate`)
internal/sim/              simulation kernel (no game logic, no protobuf)
internal/plugins/movement/ movement plugin (+ energy cost)
internal/plugins/wells/    energy wells: data model, admin commands, initial placement
internal/plugins/energy/   unit energy storage, mining, transfers
internal/game/             config loading, plugin selection, world setup
internal/server/           gRPC services, sessions, observation hub
internal/gateway/          browser HTTP/NDJSON ↔ gRPC bridge
web/static/player/         player client (plain HTML/JS/canvas, no build step)
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
    "id_key": ""                 // hex secret (>= 32 hex chars) for ID derivation; empty = random per start
  },
  "players": [ { "slot": 1, "name": "Red", "join_token": "red" } ],  // slot: config-local handle
  "admin_token": "dev-admin",    // empty disables AdminService
  "units": [ { "owner": 1, "x": 2, "y": 3, "w": 4, "h": 3 } ],  // owner: player slot (0 = none); w/h: block size
  "plugins": {
    "wells": {
      "defaults": { "energy": 1000, "capacity": 1000, "extraction_rate": 5, "unlimited": false },
      "wells": [ { "x": 15, "y": 4 }, { "x": 10, "y": 10, "unlimited": true, "extraction_rate": 6 } ]
    },
    "energy": {
      "unit_capacity": 100,      // storage of new units
      "initial_energy": 50,      // energy of new units (<= unit_capacity)
      "extraction_rate": 2,      // max a unit mines per tick
      "transfer_rate": 5         // max a unit sends per tick
    },
    "movement": {
      "ticks_per_move": 1,       // ticks between a unit's steps
      "max_blocked_ticks": 20,   // cancel an order after this many failed steps (0 = never)
      "contention_backoff_ticks": 2,
      "energy_per_step": 0       // energy per tile moved (0 = free)
    }
  }
}
```

The values shown in `plugins` are the built-in defaults. `configs/dev.json`
overrides some of them: 2 ticks per move, 1 energy per step, 30 initial
energy, and three wells, one of them unlimited. Each entry in `wells.wells`
may override any of the `defaults` fields.

Entity and player IDs are opaque random-looking 64-bit/32-bit values
derived from `id_key`, so they don't reveal how many players or units
exist. A slot number is only used inside the config file and never leaves
the server. Set `id_key` only when runs must be reproducible, and keep it
secret.

## Performance (Apple M-series laptop, `make sim` / `make bench`)

| Units  | World   | Mean tick | Notes                                   |
|--------|---------|-----------|-----------------------------------------|
| 1,000  | 200×200 | ~0.3 ms   | every unit has an order                 |
| 5,000  | 200×200 | ~1.1 ms   | every unit has an order                 |
| 10,000 | 200×200 | ~2.1 ms   | every unit has an order                 |
| 5,000  | 200×200 | p50 2.1 ms, p99 3.6 ms | harness: 4 players, reorders every 20 ticks |

These are Milestone 2 numbers, with every unit carrying an energy store. At
10 Hz the budget is 100 ms per tick. Nothing has been optimized yet.
