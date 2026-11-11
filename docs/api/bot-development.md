# Bot development

A bot is just a player. It uses the same `GameService` as the browser
client, sees exactly what a player in its slot would see, and is bound by
the same rules and command budget. This guide builds one end to end.

For a complete, ready-to-run opponent with ten difficulty levels, see
the [computer player](../computer-player/README.md) (`mono-bot`); its
packages (`internal/bot/state`, `internal/bot/client`) are a fuller
reference implementation of what this guide builds, and it can be
[extended](../computer-player/extending.md) without forking.

Working code in the repository:

| File | What |
|---|---|
| [`examples/bot/main.go`](../../examples/bot/main.go) | A complete Go bot over gRPC: connect, state tracking, fog-aware memory, budget, automation, reconnection |
| [`examples/bot/bot_test.go`](../../examples/bot/bot_test.go) | Testing it against an in-process server |
| [`examples/python/bot.py`](../../examples/python/bot.py) | A standard-library Python client over the HTTP gateway |

```bash
make dev                                              # in one terminal
go run ./examples/bot -token blue -ticks 300          # in another
python3 examples/python/bot.py --token red --ticks 100
```

## 1. Connect and authenticate

```go
conn, _ := grpc.NewClient("127.0.0.1:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
game := rtsv1.NewGameServiceClient(conn)
resp, err := game.Connect(ctx, &rtsv1.ConnectRequest{JoinToken: "blue"})
session := resp.GetSessionToken()
me := resp.GetPlayerId()
w := resp.GetWorld() // size, tick rate, command types, rules_json, schedule, budget
authCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+session)
```

**Read `WorldInfo` before playing.**

- `rules_json` holds every mechanic's actual configuration: attribute cost
  tables, combat formulas, visibility settings.
- `command_types` tells you which commands exist on this server.
- `unit_command_budget` is your per-tick limit.

Bots that adapt to `rules_json` work on any server configuration.

## 2. Receive observations and track state

```go
stream, _ := game.Observe(authCtx, &rtsv1.ObserveRequest{})
for {
    msg, err := stream.Recv()
    if err != nil { /* reconnect, see §7 */ }
    if snap := msg.GetSnapshot(); snap != nil {
        state.applySnapshot(snap) // replace everything
        continue
    }
    state.applyUpdate(msg.GetUpdate()) // changed = full replacements, removed = gone
    think()                            // decide this tick's commands
}
```

The example's `state` keeps:

- `entities map[uint64]*rtsv1.Entity`: your view
- `lastSeen`: when each foreign entity was last observed
- `pending`: command ID → description, until the result arrives

Rules:

- **Replace, don't merge.** A changed entity is complete. A missing
  attribute means it's gone (an order ended).
- **Your units:** `owner_id == player_id` and `kind == UNIT`.
- **Orders are visible on your units** as attributes:
  `movement.destination`, `energy.mining_well`, `combat.target`,
  `automation.behavior`, and so on. A unit with none is idle.
- **Wells** are `RESOURCE` entities that aren't passable; **wreckage** is
  passable. Under fog of war a well's `wells.energy` may be absent, exact
  or a `range_value` ([streaming](streaming.md#fog-of-war)).

## 2b. Follow the match lifecycle

`MatchInfo` arrives in the first snapshot and in every update where the
match changed:

- **READY:** send `rts.match.v1.SetReadyCommand{ready: true}`. With
  `start: all_ready` the match starts when everyone is ready. Retry until
  `you_ready` is true.
- **RUNNING:** play. Gameplay commands sent in other states fail with
  `failed_precondition`.
- **PAUSED / FINISHED:** stop issuing commands. A finished match carries
  `result` (winner, reason, scores, statistics).
- **A new `match_id`:** the server was reset. Drop all state, adopt
  `your_player_id`, and ready up again.

The example bot's `applyMatch` and the start of `think` implement exactly
this. See [matches](../user-guide/matches.md) for the rules.

## 3. Handle fog of war

When `snapshot.fog_of_war` is true:

- You only see detected foreign entities. Their attributes are limited
  by your identification level and may be ranges (`Value.range_value`).
- `removed` means "no longer observed", **not** "destroyed". Keep memory
  (`lastSeen`) and decide how long to trust it.
- Some facts come only from events. The example learns that a well is
  empty from `energy.well_depleted` and from `automation.failed` with
  reason `well_depleted`, because under fog the well's energy may be shown
  only as a broad range. Depleted wells stay on the map.
- Commands referring to things you can't detect fail with `not_found`,
  the same as for nonexistent IDs.

## 4. Issue commands

```go
payload, _ := anypb.New(&automationv1.AutomateCommand{
    UnitIds: miners,
    Behavior: &automationv1.AutomateCommand_MineAndDeliver{MineAndDeliver: &automationv1.MineAndDeliver{
        WellId: well.GetId(), ReceiverId: depot.GetId(),
    }},
})
ack, err := game.SubmitCommand(authCtx, &rtsv1.SubmitCommandRequest{CommandId: id, Payload: payload})
```

- **`ack.Accepted`** only means the envelope was valid. The real outcome
  is a `CommandResult` in the update for `ack.ScheduledTick`. Track
  pending IDs and log failures (`error_code`, `unit_results`).
- **Command IDs must be unique per player.** A repeated ID returns the
  original acknowledgment and **doesn't run again**. That's useful for
  safe retries but a trap for restarts, so the example includes a per-run
  nonce: `bot-<nonce>-<seq>`.
- All command payloads are in the [command reference](../reference/commands.md).

## 5. Respect the command budget

`WorldInfo.unit_command_budget` (500 in the dev config) caps
unit-commands per tick: each unit a command affects counts once. Over
budget, a command fails with `resource_exhausted` and does nothing.
Strategies:

- **Persistent orders, not per-tick micromanagement.** Movement, mining,
  attacks, salvage and supply all continue on their own. The example only
  re-plans every 10 ticks.
- **Batch over metadata.** Tag units once (`SetMetadataCommand`), then send
  `BatchCommand{selector: {conditions: [{key: "role", value: "miner",
  match: MATCH_KIND_EQUALS}]}, command: <template>}`. The server applies
  the template to matching units when the batch executes. It's still
  charged per unit, but it's one command to send, and units tagged earlier
  in the same tick are included.
- **Automation.** One `AutomateCommand` makes units mine, deliver, guard,
  follow or salvage indefinitely, at no per-tick command cost.
- **Supply links.** One `SupplyCommand` moves energy every tick, for good.

## 6. Use persistent orders and selectors

| Goal | Command |
|---|---|
| Move a group without collisions | `rts.movement.v1.GroupMoveCommand` (server picks distinct tiles) |
| Mine | `rts.energy.v1.MineCommand` (unit must be adjacent; pair it with a move), or `AutomateCommand.mine_and_deliver` |
| Fight | `rts.combat.v1.AttackCommand` (no pursuit), or `AutomateCommand.guard` |
| Specialize | `rts.upgrades.v1.UpgradeCommand` (paid up front; check prices in `rules_json.attributes`) |
| Grow | `rts.cloning.v1.CloneCommand` (the price is the owner-visible `cloning.quote_*` attribute) |
| Move energy | `rts.logistics.v1.SupplyCommand` |
| Address groups | `rts.metadata.v1.SetMetadataCommand` + `BatchCommand` |

## 7. Reconnect

```go
for {
    stream, err := game.Observe(authCtx, &rtsv1.ObserveRequest{})
    // ... read until error ...
    if status.Code(err) == codes.Unauthenticated { // server restarted: sessions are in memory
        connect(ctx, joinToken)                     // new session; the world is new as well
    }
    time.Sleep(time.Second)
}
```

Each new `Observe` starts with a snapshot, so state is rebuilt from
scratch. If a `SubmitCommand` call failed in transit, retry with the
**same** command ID.

## 8. Test the bot

`examples/bot/bot_test.go` starts the real game and gRPC server in
process (`game.New`, `server.New`, `bufconn`), runs the engine on a fast
clock, plays 80 ticks, and asserts that the bot put units to work on a
visible well:

```bash
GOTOOLCHAIN=local go test ./examples/bot -v
```

More options:

- **Fully deterministic tests.** Skip `Engine.Run` and call
  `gm.Engine.Step()` yourself after each batch of commands, as the server
  tests in `internal/server` do.
- **Bot vs. bot.** Run two bots with different join tokens against
  `make dev`, and watch from the admin client ("View as" shows what each
  bot sees).
- **Reproducible worlds.** Set `world.id_key` and `world.seed` in a test
  config so entity IDs and outcomes repeat between runs.

## Python

[`examples/python/bot.py`](../../examples/python/bot.py) uses only the
standard library and the HTTP gateway:

```python
c = Client("http://127.0.0.1:8080", "red")          # POST /api/connect
for msg in c.observe():                             # GET /api/observe, NDJSON
    if "snapshot" in msg: ...                       # replace state
    u = msg["update"]                               # apply changed/removed
    c.submit({"@type": "type.googleapis.com/rts.movement.v1.GroupMoveCommand",
              "unitIds": [unit_id], "destination": {"x": 5, "y": 7}})
```

In JSON, 64-bit IDs are strings, and `Any` payloads carry `"@type"`. For
a native gRPC Python client, generate stubs from `proto/` with your
preferred protobuf toolchain. The repository doesn't ship Python stubs.
