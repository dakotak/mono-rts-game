# Replay format and reproducibility

Source: [`internal/replay`](../../internal/replay/replay.go). CLI:
[`cmd/rts-replay`](../../cmd/rts-replay/main.go).

## What is recorded

The server attaches a `replay.Recorder` to every match's frame sink
(`internal/server`, `newMatch`). On every tick, on the simulation
goroutine, it records:

- each executed command (`Frame.Commands`): ID, issuer (player ID or
  admin), per-issuer sequence number, type, and payload. Protobuf payloads
  are stored as protojson; the kernel's Go-struct admin commands
  (`sim.CreateEntity`, `sim.RemoveEntity`, `sim.MoveEntity`) as JSON.
- a **frame digest** (`replay.Digest`): FNV-64 over command results,
  events, changed entities (ID, position, attribute count) and removals
- a **full state hash** (`World.StateHash`) every
  `-replay-hash-interval` ticks (default 100), at tick 0, and when the
  match finishes

Because everything that changes the world is a scheduled command (player
and admin alike, including lifecycle and readiness), the configuration
plus this log fully determines the match. Wall-clock timing never matters:
ticks that execute no command are replayed as empty ticks.

## Format `mono-replay/1`

```json
{
  "format": "mono-replay/1",
  "engine_version": "mono-m7",
  "ruleset_hash": "308ddeb979ef590b",
  "match_id": "3eaf5cdf9cb9551c",
  "seed": 20240,
  "config": { "world": { "seed": 20240, "id_key": "…", "random_seed": false, … }, "players": [ { "slot": 1, "join_token": "replay-slot-1", … } ], "admin_token": "", "plugins": { … } },
  "players": [ { "slot": 1, "name": "Red", "id": 3899826582 } ],
  "final_tick": 755,
  "ticks": [ { "tick": 312, "commands": [ { "id": "bot-…", "player": 3899826582, "seq": 1, "type": "rts.match.v1.SetReadyCommand", "payload": { "ready": true } } ] } ],
  "digests": [ "1f0c…", "…" ],
  "checkpoints": [ { "tick": 0, "hash": "…" }, { "tick": 100, "hash": "…" } ],
  "result": { "matchId": "…", "winnerId": 0, "draw": true, "reason": "e2e-test", … }
}
```

| Field | Notes |
|---|---|
| `config` | The **resolved** configuration: the seed actually used (`random_seed` off) and the ID key (hex). Join tokens are replaced by placeholders, and the admin token is empty. |
| `ticks` | Only ticks with commands, in tick order, commands in execution order. |
| `digests` | One per tick, `1..final_tick` (hex). |
| `result` | The public `MatchResult` (protojson) once the match finished. |

## Verification

`replay.Verify` (used by `rts-replay verify` and `AdminService.VerifyReplay`):

1. Strictly decodes the config and builds the match with `game.New`. This
   reruns world generation from the recorded seed.
2. Compares the tick-0 state hash. A mismatch there means the
   configuration or generation code differs.
3. For each tick, runs `Engine.StepWith` with exactly the recorded commands
   (sequence numbers preserved), then compares the frame digest and any
   checkpoint hash. It stops at the **first divergent tick** and reports
   what differed.

It also reports whether `engine_version` and `ruleset_hash` match the
running build.

## Compatibility

| Change | Effect on old replays |
|---|---|
| Same build, same plugins and configuration | Reproduces exactly (tested: complete matches, tampered commands caught within a few ticks, matches replayed after several resets). |
| Different `game.EngineVersion` | Reported as a mismatch. It may still reproduce if nothing relevant changed, but there's no guarantee. Bump the version for any change that can alter simulation results. |
| Different ruleset (plugins added or removed, phase order, any effective config value) | Reported. Usually diverges: the component and global list is part of `StateHash`. |
| Custom plugins | Verification uses `game.New`, so plugins must be registered in `game.Plugins()` of the verifying build. |

There's no cross-version migration, replay database or replay service.
Replays are files you keep.

## Guarantees and limits

- **Resets don't contaminate replays.** Each match has its own engine and
  recorder, and the previous match's recorder is archived unchanged.
- **The log is the source of truth, not the timing.** When a command was
  *submitted* doesn't matter; the tick it *executed* in is recorded.
- **Divergence is located per tick**, through frame digests. Full state
  hashes at checkpoints confirm the state itself.
- **Memory.** Recording costs one digest per tick plus the commands, held
  in memory. The server keeps the current match and the last 16.
- **Size limits.** gRPC and the gateway accept replay documents up to
  64 MiB.
