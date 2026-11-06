# Deployment

> **Status.** Mono is development-grade software. It has no persistence,
> TLS, accounts, metrics endpoint, container images or CI. This page
> covers what exists and what you must add yourself before exposing a
> server to untrusted networks.

## Processes and ports

| Process | Default bind | Protocol | Serves |
|---|---|---|---|
| `rts-server` | `127.0.0.1:50051` (`-addr`) | gRPC (HTTP/2, plaintext) | `GameService`, `AdminService`, gRPC reflection |
| `rts-gateway` player listener | `127.0.0.1:8080` (`-addr`) | HTTP/1.1, protojson, NDJSON streams | Player web client and `/api/*` (GameService only) |
| `rts-gateway` admin listener | `127.0.0.1:8081` (`-admin-addr`, `""` disables it) | Same | Admin web client and `/api/*` (AdminService only) |

`rts-gateway` is a stateless gRPC client of `rts-server` (`-server`). It
can run on another host, and you can run several. Clients that speak gRPC
(bots) don't need it. Rebuild the gateway whenever plugin protos change:
it embeds the proto registry and, unless `-web` is given, the web clients.

## Build

```bash
make build                  # bin/rts-server, bin/rts-gateway, bin/rts-sim, bin/rts-bench
```

The binaries are static Go executables with no runtime dependencies. Set
`GOOS`/`GOARCH` to cross-compile.

## Configure

One JSON file per world ([reference](../reference/configuration.md)). For
anything beyond local development:

- Use **long random** `join_token`s and `admin_token`, never the dev
  values (`red`, `blue`, `dev-admin`).
- Leave `world.id_key` **empty**, so a random key is generated at startup.
  With a known key, entity IDs become predictable.
- Set `world.unit_command_budget` (dev: 500) to bound per-player work per
  tick.
- Keep `tick_rate_hz` within what your world size allows. See
  [performance](#performance-and-tuning).

Config errors are fatal at startup, with a message naming the field.

## Run

```bash
bin/rts-server -config /etc/mono/world.json -addr 127.0.0.1:50051
bin/rts-gateway -server 127.0.0.1:50051 -addr 0.0.0.0:8080 -admin-addr 127.0.0.1:8081
```

- `rts-server` stops gracefully on SIGINT or SIGTERM: unary calls get up
  to 2 seconds, then streams are cut.
- **All state is lost on exit.** A restarted server starts a fresh world,
  and clients must reconnect with their join tokens.

No environment variables are read by the binaries. `scripts/dev.sh`
reads `CONFIG`, `GRPC_ADDR`, `HTTP_ADDR` and `ADMIN_ADDR`.

## Logging and observability

| Source | What you get |
|---|---|
| `rts-server` stdout | Startup summary (world size, entity count, tick rate, plugin order, tick schedule), listen address, shutdown. No per-tick or per-request logs. |
| `rts-gateway` stdout | Listen addresses and request errors. |
| `AdminService.GetStatus` | Last tick, paused, tick rate. |
| `ObserveWorld` events | `automation.work` (evaluated and deferred counts) and `logistics.work` (links and energy moved) per tick. Every other event too. |
| `cmd/rts-bench` | Offline tick-time, allocation and event-volume measurements for planning. |

There's no Prometheus or OpenTelemetry endpoint. To measure tick
duration in production you would add instrumentation around
`Engine.Step` (not provided).

## Performance and tuning

Per-tick cost scales with units and active mechanics. From
[`make scale`](../developer-guide/testing.md#benchmarks), single-threaded
on an Apple M-series laptop, p95 per tick:

| Units | Movement | Mining | Fog of war (sensors) | Mixed |
|---|---|---|---|---|
| 1,000 | 2.2 ms | 1.5 ms | 4.2 ms | 12 ms |
| 10,000 | 11 ms | 17 ms | 46 ms | 76 ms |
| 25,000 | 37 ms | 91 ms | 135 ms | 181 ms |

At 10 Hz the budget is 100 ms, and 25k units with fog of war exceeds it.
Measure your own world shape with `go run ./cmd/rts-bench -units N
-scenarios ...`.

Knobs that matter:

| Knob | Effect |
|---|---|
| `tick_rate_hz` | Halving it doubles the time budget per tick. If a tick overruns, the engine simply runs slower than real time; it never skips simulation work. |
| `visibility.enabled`, vision radii | Fog-of-war cost grows with observers × tiles in vision. |
| `movement.plans_per_tick`, `max_path_nodes` | Bound pathfinding work. |
| `automation.max_evaluations_per_tick`, `eval_interval` | Bound automation work. |
| `unit_command_budget` | Bounds command-processing work per player. |
| `energy` / mining | Every miner changes every tick, which costs views and events (25k `energy.mined` events per tick at 25k miners). |
| `GOGC` | Measured with no effect on tick time (see [design notes](../developer-guide/design-notes.md#performance-milestone-6)). |

The simulation uses one core. gRPC serving, the gateway and per-viewer
filtering use others.

## Security

Before exposing Mono beyond localhost:

- **TLS.** Neither binary terminates TLS. Put a TLS-terminating proxy in
  front: one that supports gRPC (HTTP/2) for `:50051`, and HTTP for the
  gateway. NDJSON streaming needs response buffering disabled.
- **Keep the admin listener private.** Bind `-admin-addr` to localhost or
  a private network, or disable it. AdminService on `:50051` shares the
  port with GameService and is protected only by the admin token, so
  firewall `:50051` if bots don't need direct access.
- **Secrets.** The config file contains every token. Restrict its
  permissions.
- **Abuse.** The only built-in limits are the per-tick unit-command
  budget, command-ID length, the 1 MiB gateway body limit and the
  idempotency window. There is no connection or rate limiting.
- **Plugins are trusted code**, compiled into the server and not
  sandboxed ([plugin security](../developer-guide/plugins/overview.md#security)).
- **License.** The project is AGPL-3.0. Running a modified server for
  others has source-availability obligations; read [LICENSE](../../LICENSE).

## Replay and recovery: limitations

- No save or load. Crashes and restarts lose the world.
- No replay files. Determinism makes replay possible (`Engine.StepWith`
  with the recorded command log, the same config and the same `id_key`),
  but recording and replaying is only implemented in tests, not as an
  operational feature.
- Sessions don't survive restarts.

## Development vs. production

| Feature | Development | Production-ready? |
|---|---|---|
| `make dev`, `configs/dev.json`, static dev tokens | Yes | **No.** Replace the tokens. |
| `-web web/static` live client reload | Yes | Use the embedded clients (omit `-web`). |
| Plaintext gRPC and HTTP | Yes | **No.** Add TLS in front. |
| gRPC reflection | On | Always on. Harmless; the API is public anyway. |
| Persistence, accounts, monitoring | — | Not implemented. |
