# Installation

## Requirements

| Requirement | Notes |
|---|---|
| Go **1.26.2** or newer | `go.mod` declares `go 1.26.2`. The Makefile sets `GOTOOLCHAIN=local`, so your installed Go is used and nothing is downloaded behind your back. |
| A modern browser | Only for the bundled web clients. |

Nothing else needs installing. Code generation and lint tools (`buf`,
`protoc-gen-go`, `protoc-gen-go-grpc`, `staticcheck`) are pinned in
[`tools/go.mod`](../../tools/go.mod) and run through
`go tool -modfile=tools/go.mod ...`.

Generated protobuf code is committed under [`gen/`](../../gen), so a
plain build needs no generator at all.

## Get the code and build

```bash
git clone git@github.com:dakotak/mono-rts-game.git
cd mono-rts-game
make build        # writes bin/rts-server, bin/rts-gateway, bin/rts-sim, bin/rts-bench
make test         # optional: run the full test suite
```

`make build` runs `go build -o bin/ ./cmd/...`. You can also build a single
binary:

```bash
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server
```

## The binaries

| Binary | Source | What it does |
|---|---|---|
| `rts-server` | [`cmd/rts-server`](../../cmd/rts-server/main.go) | Loads a config, builds the world, runs the simulation, and serves `GameService` and `AdminService` over gRPC (with gRPC reflection enabled). |
| `rts-gateway` | [`cmd/rts-gateway`](../../cmd/rts-gateway/main.go) | Browser bridge: serves the web clients and translates HTTP/JSON to gRPC. One process, two listeners: player and admin. |
| `rts-sim` | [`cmd/rts-sim`](../../cmd/rts-sim/main.go) | Headless harness: many units, random orders, no network. Prints throughput and the final state hash. |
| `rts-bench` | [`cmd/rts-bench`](../../cmd/rts-bench/main.go) | Scale scenarios (1k–25k units × 7 workloads) and the tick-time report. |
| `rts-replay` | [`cmd/rts-replay`](../../cmd/rts-replay/main.go) | `info` / `verify` for exported match replays. |
| `mono-bot` | [`cmd/mono-bot`](../../cmd/mono-bot/main.go) | The computer player: joins with a join token and plays ([guide](../computer-player/README.md)). |
| `mono-bothost` | [`cmd/mono-bothost`](../../cmd/mono-bothost/main.go) | Starts/stops computer players in configured slots over HTTP, for the web client's "Add computer opponent" panel (`rts-gateway -bothost`). |
| `mono-arena` | [`cmd/mono-arena`](../../cmd/mono-arena/main.go) | Bot-vs-bot evaluation in lockstep ([evaluation](../computer-player/evaluation.md)). |

### `rts-server` flags

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `configs/dev.json` | Game configuration file ([reference](../reference/configuration.md)). |
| `-addr` | `127.0.0.1:50051` | gRPC listen address. |
| `-check` | false | Validate the configuration (world generation included), print a summary, and exit. |
| `-replay-hash-interval` | 100 | Ticks between full state hashes in recorded replays. |

### `rts-gateway` flags

| Flag | Default | Meaning |
|---|---|---|
| `-server` | `127.0.0.1:50051` | Address of `rts-server`. |
| `-addr` | `127.0.0.1:8080` | Player HTTP listener (player client and `GameService` bridge). |
| `-admin-addr` | `127.0.0.1:8081` | Admin HTTP listener (admin client and `AdminService` bridge). Empty disables it. |
| `-web` | *(embedded)* | Serve the clients from this directory instead of the copy embedded in the binary. `make dev` passes `web/static` so client edits apply on reload. |

### `rts-sim` and `rts-bench`

```bash
go run ./cmd/rts-sim -units 5000 -ticks 2000 -size 200 -players 4 -seed 1 -order-every 20
go run ./cmd/rts-bench -units 1000,5000 -scenarios sensors,mixed -ticks 40
```

`rts-bench` also accepts `-seed`, `-cpuprofile FILE` and `-memprofile FILE`.

## Running

The development script starts everything:

```bash
make dev          # = ./scripts/dev.sh
```

[`scripts/dev.sh`](../../scripts/dev.sh) builds both binaries and starts
them. It reads these environment variables:

| Variable | Default |
|---|---|
| `CONFIG` | `configs/dev.json` |
| `GRPC_ADDR` | `127.0.0.1:50051` |
| `HTTP_ADDR` | `127.0.0.1:8080` |
| `ADMIN_ADDR` | `127.0.0.1:8081` |

These are the only environment variables anything in the repository reads.
The server and gateway themselves are configured by flags and the config
file.

You can also run the processes separately:

```bash
make run-server      # go run ./cmd/rts-server -config configs/dev.json
make run-gateway     # go run ./cmd/rts-gateway -web web/static
```

Production-style deployment (separate hosts, hardening) is covered in
[operations/deployment.md](../operations/deployment.md).

### `rts-gateway -bothost`

`-bothost URL` exposes a computer-player host (`mono-bothost`) at
`/api/bots` on the player listener, which turns on the player client's
"Computer opponents" panel. See [the computer player](../computer-player/README.md#adding-a-computer-opponent-from-the-web-client).
