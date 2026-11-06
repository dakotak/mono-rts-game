# Troubleshooting

## Startup

| Symptom | Cause and fix |
|---|---|
| `parse configs/x.json: json: unknown field "..."` | A typo or unsupported field at the top level. Check the [configuration reference](../reference/configuration.md). |
| `plugin "movement" config: json: unknown field "..."` | A typo inside a plugin section. Plugin sections are strict too. |
| `configuration for unknown plugin "..."` | A section for a plugin that isn't compiled in (or a misspelled plugin name). |
| `register plugin "X": ...` | That plugin rejected its config: ranges, formulas, a binding to a property that doesn't exist (`... is not an attribute or derived property`). |
| `attribute "x": cost must be non-decreasing ...` | `cost` and `duration` are **cumulative** and must not decrease over `[min, max]`. |
| `units[3]: ...` / `terrain tiles[0] at (x,y): ...` | Overlapping blocking entities or out of bounds. Units, wells and impassable terrain need distinct tiles. |
| `id_key must be at least 16 bytes` | Use 32+ hex characters, or leave it empty. |
| `listen tcp ...: address already in use` | Another server is still running. With `make dev`, a previous run may still be shutting down; wait or stop it. |

## Connecting

| Symptom | Cause and fix |
|---|---|
| `Unauthenticated: unknown join token` | The token isn't in `players[].join_token` for the config the server was started with. |
| `Unauthenticated: missing or invalid session token` | Missing `authorization: Bearer <session>` header, or the server restarted (sessions are in memory). Call `Connect` again. |
| Admin calls fail with `PermissionDenied: admin API disabled` | The config has no `admin_token`. |
| Admin calls fail with `Unauthenticated: invalid admin token` | Wrong token, or a player session token sent to AdminService. |
| Browser client loads but can't stream | The gateway can't reach `rts-server` (`-server` address), or a proxy is buffering the NDJSON response. |
| The gateway returns 400 `InvalidArgument` ("unable to resolve ... type") for a new plugin's command | The gateway binary predates the plugin, or the proto isn't in `internal/protoreg`. Rebuild the gateway: it embeds the registry. |

## Commands

| Symptom | Cause |
|---|---|
| `accepted: false`, "unknown command type" | Wrong `@type` (it must be `type.googleapis.com/<full message name>`), or the type isn't in `WorldInfo.command_types`. |
| `accepted: false`, "requires admin privileges" | An admin-only command sent through GameService. |
| Accepted, then `FAILED not_found` | The unit isn't yours, doesn't exist, or (for targets, wells, wrecks) isn't currently detected. Under fog these are intentionally indistinguishable. |
| `FAILED resource_exhausted` | Per-tick unit-command budget exceeded. Use batches, automation or persistent orders, or spread commands over ticks. |
| `FAILED insufficient_energy` | Upgrades and clones are paid up front from the unit's own store. Check the price (`rules_json`, `cloning.quote_*`). |
| Resubmitted command never runs | Same `command_id` as an earlier command: you get the old acknowledgment back. Use unique IDs, with a per-run nonce. |
| `PARTIAL` result | Batch-style command where some units failed. See `unit_results`. |

## Gameplay surprises

| Symptom | Explanation |
|---|---|
| Units refuse to move | Out of energy (`movement.stalled`; `energy_per_step > 0`), or blocked: units contesting one tile all fail and back off. Orders are cancelled after `max_blocked_ticks`, or with `unreachable` if there's no path. |
| Miners don't mine | They must be **adjacent** to the well. The order waits otherwise. A full unit also waits. |
| Attack never fires | Out of range (no pursuit), short of energy for the attack cost, on cooldown, or the target is no longer detected. |
| An enemy vanished | Under fog, leaving detection and destruction look the same. |
| Automation unit "blocked" | See `automation.reason`: `receiver_full`, `no_wreckage`, `battery_full`, `*_crowded`, `cannot_attack`. |
| Supply link not flowing | See `logistics.supply.<n>.status`: `out_of_range`, `reserve`, `receiver_full`, `satisfied` (resting until the receiver drops below `resume_below`). |

## Performance

| Symptom | What to check |
|---|---|
| The game runs slower than `tick_rate_hz` | Tick time exceeds the interval. Measure with `go run ./cmd/rts-bench` for your unit count. Lower `tick_rate_hz`, vision radii or unit counts, or tune the bounded-work knobs ([deployment](deployment.md#performance-and-tuning)). |
| A client keeps getting snapshots mid-stream | It's falling 128+ ticks behind and being resynced. Process updates faster, or check the network path. |
| High bandwidth | Every changed entity is sent in full each tick, and mining changes every miner every tick. This is a known limitation ([design notes](../developer-guide/design-notes.md#performance-milestone-6)). |

## Development

| Symptom | Fix |
|---|---|
| `make lint` complains about formatting | `make fmt`. |
| `TestEveryCommandTypeIsRegisteredForGateways` fails | Add your plugin's proto file to `internal/protoreg.Files`. |
| Generated code out of date | `make generate` (uses the pinned `buf`). |
| `go` downloads a toolchain | The Makefile sets `GOTOOLCHAIN=local`. Set it yourself when running `go` directly, and use Go ≥ 1.26.2. |
