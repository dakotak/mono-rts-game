# Unit Strategy Game — RTS simulation engine

A deterministic, headless, server-authoritative real-time strategy engine in
Go. One server process hosts one world; any number of players (humans or AI
agents, indistinguishable to the server) connect over a public gRPC API.
Game mechanics are compiled-in plugins.

**Status: Milestone 1** — the kernel, plugin system, a movement plugin, the
gRPC APIs, a browser gateway and a minimal web client. Two players can
connect, select units and move them, and watch the authoritative simulation.
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
| `rts-gateway` | `http://127.0.0.1:8080`  | browser client + HTTP→gRPC bridge            |

Open **http://127.0.0.1:8080** in two browser tabs (or two browsers). Join
as `red` in one and `blue` in the other. Each tab is an independent session.

Controls: click an own unit to select it (Shift adds to the selection), drag
to box-select, right-click (or click an empty tile) to move, **S** to stop,
**Esc** to deselect. Reloading a tab resumes the same session.

To run the processes separately (for example in two terminals):

```bash
go run ./cmd/rts-server -config configs/dev.json -addr 127.0.0.1:50051
```

```bash
go run ./cmd/rts-gateway -server 127.0.0.1:50051 -addr 127.0.0.1:8080 -web web/static
```

`-web web/static` serves the client from disk so edits show on reload.
Without it, the gateway serves the copy embedded at build time.

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
gen/                       generated Go bindings (committed; `make generate`)
internal/sim/              simulation kernel (no game logic, no protobuf)
internal/plugins/movement/ movement plugin
internal/game/             config loading, plugin selection, world setup
internal/server/           gRPC services, sessions, observation hub
internal/gateway/          browser HTTP/NDJSON ↔ gRPC bridge
web/static/                browser client (plain HTML/JS/canvas, no build step)
cmd/rts-server/            server binary
cmd/rts-gateway/           gateway binary
cmd/rts-sim/               headless simulation harness
configs/dev.json           two-player development world
docs/ARCHITECTURE.md       design, extension points, roadmap
```

## Configuration

`configs/dev.json`:

```jsonc
{
  "world":   { "width": 32, "height": 20, "seed": 42, "tick_rate_hz": 10 },
  "players": [ { "id": 1, "name": "Red", "join_token": "red" }, ... ],
  "admin_token": "dev-admin",               // empty disables AdminService
  "units":   [ { "owner": 1, "x": 2, "y": 3, "w": 4, "h": 3 }, ... ],  // w/h: block of units
  "plugins": { "movement": { "ticks_per_move": 2, "max_blocked_ticks": 30, "contention_backoff_ticks": 2 } }
}
```

Unknown fields are rejected, both at the top level and inside each
plugin's section.

## Performance (Apple M-series laptop, `make sim` / `make bench`)

| Units  | World   | Mean tick | Notes                                   |
|--------|---------|-----------|-----------------------------------------|
| 1,000  | 200×200 | ~0.2 ms   | every unit has an order                 |
| 5,000  | 200×200 | ~0.7 ms   | every unit has an order                 |
| 10,000 | 200×200 | ~1.2 ms   | every unit has an order                 |
| 5,000  | 200×200 | p50 1.4 ms, p99 2.4 ms | harness: 4 players, reorders every 20 ticks |

At 10 Hz the budget is 100 ms per tick. Nothing has been optimized yet.
