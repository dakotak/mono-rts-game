# GameService

`rts.v1.GameService` ([`proto/rts/v1/game.proto`](../../proto/rts/v1/game.proto),
implemented in [`internal/server/server.go`](../../internal/server/server.go))
is the player API. Every RPC except `Connect` needs the metadata header
`authorization: Bearer <session_token>`. See
[authentication](authentication.md).

## Connect

```proto
rpc Connect(ConnectRequest) returns (ConnectResponse);
message ConnectRequest  { string join_token = 1; }
message ConnectResponse { uint32 player_id = 1; string session_token = 2; WorldInfo world = 3; string player_name = 4; MatchInfo match = 5; }
```

Exchanges a configured join token for a new session. Calling it again
with the same token returns a **new** session for the same player, and
earlier sessions stay valid. Several sessions per player may be active.

| Field | Meaning |
|---|---|
| `player_id` | Your opaque player ID. Your entities have `owner_id == player_id`. |
| `session_token` | Bearer token for every other call. In memory only: invalid after a server restart. |
| `world` | `WorldInfo` (below). |
| `player_name` | The configured display name. |
| `match` | The current match (see [GetMatch](#getmatch)). |

Sessions are bound to the player **slot**, not to a match. After an admin
reset the same session plays the slot's player in the new match, under a
**new player ID** (IDs are per match): read `MatchInfo.your_player_id`.

**Errors:** `Unauthenticated` "unknown join token".

### WorldInfo

| Field | Meaning |
|---|---|
| `width`, `height` | Grid size. |
| `tick_rate_hz` | Ticks per second. |
| `command_types` | Fully qualified message names accepted as player commands (admin-only types excluded). |
| `plugins` | Registered plugins in registration order. |
| `rules_json` | JSON object: plugin name → that plugin's published rules (effective config, formulas, cost tables). |
| `schedule` | Per-tick execution order, one `"phase: system, ..."` string per phase. |
| `unit_command_budget` | Unit-commands per player per tick (0 = unlimited). |

`WorldInfo` deliberately has **no player list**: that would reveal how
many players exist.

## SubmitCommand

```proto
rpc SubmitCommand(SubmitCommandRequest) returns (SubmitCommandResponse);
message SubmitCommandRequest  { string command_id = 1; google.protobuf.Any payload = 2; }
message SubmitCommandResponse { string command_id = 1; bool accepted = 2; uint64 scheduled_tick = 3; string rejection_reason = 4; }
```

Validates the envelope and schedules the command for the next tick.

| Outcome | Response |
|---|---|
| Accepted | `accepted: true`, `scheduled_tick` = the tick it executes in. The `CommandResult` arrives in that tick's update. |
| Envelope invalid | `accepted: false`, `rejection_reason` (unknown type, admin-only, undecodable `Any`). Not scheduled. |
| Duplicate `command_id` | The original response, unchanged. Not scheduled again (window: the last 4096 IDs per player). |
| Bad session | gRPC `Unauthenticated`. |
| `command_id` empty or over 128 bytes | gRPC `InvalidArgument`. |

World-state checks (ownership, ranges, energy, budget) happen **at
execution**. Their outcome is the `CommandResult`:

```proto
message CommandResult {
  string command_id = 1; uint64 tick = 2; CommandStatus status = 3;   // SUCCEEDED | FAILED | PARTIAL
  string error_code = 4; string error_message = 5;
  repeated UnitResult unit_results = 6;                                // batch-style commands
}
```

Example (Go):

```go
payload, _ := anypb.New(&movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{
    {UnitId: unit, Destination: &rtsv1.Position{X: 12, Y: 5}},
}})
ack, err := game.SubmitCommand(authCtx, &rtsv1.SubmitCommandRequest{CommandId: "mv-1", Payload: payload})
// ack.Accepted, ack.ScheduledTick
```

Example (HTTP gateway, protojson):

```bash
curl -s -X POST localhost:8080/api/command -H "Authorization: Bearer $TOKEN" -d '{
  "commandId": "mv-1",
  "payload": {"@type": "type.googleapis.com/rts.movement.v1.MoveCommand",
              "orders": [{"unitId": "1234567890", "destination": {"x": 12, "y": 5}}]}}'
```

The [command reference](../reference/commands.md) lists every payload
type.

## GetSnapshot

```proto
rpc GetSnapshot(GetSnapshotRequest) returns (GetSnapshotResponse);
message GetSnapshotResponse { WorldSnapshot snapshot = 1; }
```

Returns your current view: the latest tick the server has published.

```proto
message WorldSnapshot {
  uint64 tick = 1;
  repeated Entity entities = 2;        // sorted by id
  bool fog_of_war = 3;
  repeated TileRun visible_tiles = 4;  // fog only: runs (x .. x+length-1, y)
}
```

## Observe

```proto
rpc Observe(ObserveRequest) returns (stream ObserveResponse);
message ObserveResponse { oneof kind { WorldSnapshot snapshot = 1; TickUpdate update = 2; } }
```

Streams your view. The first message is a `WorldSnapshot`; after that,
one `TickUpdate` per tick:

```proto
message TickUpdate {
  uint64 tick = 1;
  repeated Entity changed = 2;          // full replacements (created, changed, or newly observed)
  repeated uint64 removed = 3;          // no longer in your view (under fog: lost OR destroyed)
  repeated Event events = 4;            // events addressed to you
  repeated CommandResult command_results = 5; // your commands executed this tick
  repeated Position tiles_entered = 6;  // fog only
  repeated Position tiles_exited = 7;   // fog only
}
```

- **Resync.** If your client falls more than 128 ticks behind, the server
  sends a **fresh snapshot** instead of the missed updates. Always handle
  a snapshot arriving mid-stream.
- **Reconnecting** is calling `Observe` again; it starts with a snapshot.
- The stream doesn't end on its own; it ends when you cancel it or the
  server shuts down.

See [streaming.md](streaming.md) for how to maintain client state.

### Entity

```proto
message Entity {
  uint64 id = 1; EntityKind kind = 2; uint32 owner_id = 3; Position position = 4;
  map<string, Value> attributes = 5;     // "<plugin>.<name>" → Value
  bool passable = 6;                     // wreckage, passable terrain
  IdentificationLevel identification = 7; // fog only: OWN, or BASIC/ESTIMATED/DETAILED for foreign
}
```

`Value` is a oneof: `int_value`, `bool_value`, `string_value`,
`position_value` or `range_value {low, high}` (an estimate). Entity IDs
inside attribute and event values are **decimal strings**.

## GetMatch

```proto
rpc GetMatch(GetMatchRequest) returns (GetMatchResponse);
message GetMatchResponse { MatchInfo match = 1; repeated MatchResult history = 2; }
```

The current match as you may see it, plus the results of recent finished
matches in this server process (most recent first, at most 16). The same
`MatchInfo` is attached to every snapshot (`WorldSnapshot.match`), and to
a `TickUpdate` (`TickUpdate.match`) whenever the match status changed that
tick.

| `MatchInfo` field | Meaning |
|---|---|
| `match_id` | Changes on every reset. |
| `state` | `MATCH_STATE_SETUP`, `READY`, `RUNNING`, `PAUSED`, `FINISHED`. |
| `tick`, `start_tick`, `run_ticks` | Current tick, the tick the match started, ticks spent running (the match clock; stops while paused). |
| `time_limit_ticks`, `ticks_remaining` | 0 = no time limit. |
| `victory_conditions` | In precedence order, e.g. `["elimination", "time_limit"]`. |
| `start_mode` | `immediate`, `all_ready`, `manual`. |
| `expected_players`, `ready_players`, `eliminated_players` | Counts only: no identities. |
| `you_ready`, `you_eliminated`, `your_score`, `your_player_id` | About you. |
| `scores_public`, `scores` | Everyone's score: only when the rules make scores public, or after the match. |
| `generator`, `width`, `height` | World-generation metadata safe for players. The **seed is not included**, because it would reveal the map; it appears in the result. |
| `result` | When finished: `MatchResult` with match ID, seed, ruleset hash, engine version, winner or draw, reason, final tick, running time, and per-player score, units, embodied, stored energy, energy mined, kills, units lost and investment destroyed. |

Lifecycle effects on commands:

- While the match isn't RUNNING, every gameplay command fails at
  execution with `failed_precondition` ("match is ready", "match is
  paused", ...).
- `rts.match.v1.SetReadyCommand` is the one player command for the READY
  state.
- Eliminated players' commands fail the same way.

## Authorization summary

| You can | You can't |
|---|---|
| Command your own units | Command other players' units (`not_found`) |
| Refer to foreign entities you currently detect (attack, mine, salvage) | Refer to undetected entities (`not_found`, indistinguishable from nonexistent) |
| See your own entities' private attributes | See other players' private attributes, orders, metadata, upgrade progress |
| Receive events addressed to you | Receive other players' events or command results |
| Use player commands | Use admin-only commands (rejected at submission) |
