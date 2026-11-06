# AdminService

`rts.v1.AdminService` ([`proto/rts/v1/admin.proto`](../../proto/rts/v1/admin.proto),
implemented in [`internal/server/admin.go`](../../internal/server/admin.go))
is the privileged API.

**Authentication.**

- Every RPC needs `authorization: Bearer <admin_token>` (the config's
  `admin_token`).
- Player sessions are never accepted here, and the admin token is never
  accepted by `GameService`.
- With no `admin_token` configured, every RPC returns `PermissionDenied`
  ("admin API disabled"). A wrong token returns `Unauthenticated`.

**Visibility.** Every read is the full, unfiltered world: all entities,
all attributes (owner-only and admin-only included), every event.

**Mutations are commands.**

- Every world change is scheduled through the simulation like a player
  command, so it's ordered deterministically and appears in the replay
  log.
- Mutation RPCs return the command ID and the tick it will execute in.
- The outcome arrives as a `CommandResult` (and usually an event) on
  `ObserveWorld`. All admin sessions see admin results.

## Reads

| RPC | Response | Notes |
|---|---|---|
| `GetWorldSnapshot` | `snapshot` (full world), `players` (all configured: `id`, `name`), `status`, `world` (`WorldInfo` including admin-only command types) | |
| `ObserveWorld` | Stream: snapshot, then one `TickUpdate` per tick with every change and event, plus admin command results | Same resync rule as `Observe` |
| `GetStatus` | `SimulationStatus {tick, paused, tick_rate_hz}` | `tick` is the last executed tick. |
| `GetPlayerView(player_id)` | `snapshot` exactly as that player would receive it, plus `detections[]` `{entity_id, level, observer_id, detection_score, identification_score}` | For debugging fog of war. `InvalidArgument` for an unknown player. |

## World mutations

| RPC | Request | Executes as |
|---|---|---|
| `SpawnUnit` | `owner_id` (0 = unowned), `position` | `sim.CreateEntity` (kind unit). Event `sim.entity_created`. |
| `DeleteEntity` | `entity_id` | `sim.RemoveEntity` (any entity: unit, well, wreck, terrain). Event `sim.entity_removed`. |
| `MoveEntity` | `entity_id`, `position` | `sim.MoveEntity`: an instant teleport to an empty tile. Orders are untouched. Event `sim.entity_moved`. |
| `SubmitAdminCommand` | `Any payload` | Any plugin command with admin privileges (below). Returns `{command_id, accepted, scheduled_tick, rejection_reason}`. |

Validation errors in the request itself (missing position, unknown
owner) are gRPC `InvalidArgument`. World-state failures (an occupied
tile, say) arrive as failed `CommandResult`s.

### Admin-only plugin commands (via `SubmitAdminCommand`)

| Type | Effect |
|---|---|
| `rts.wells.v1.CreateWellCommand` | Create a well; unset fields use the configured defaults. |
| `rts.wells.v1.ConfigureWellCommand` | Change set fields of a well. |
| `rts.energy.v1.SetUnitEnergyCommand` | Set a unit's energy (and optionally capacity). |
| `rts.attributes.v1.SetAttributeCommand` | Set an attribute level within bounds (not counted as invested). |
| `rts.combat.v1.SetHealthCommand` | Set integrity; 0 destroys the unit (leaving wreckage). |
| `rts.salvage.v1.CreateWreckCommand` | Create a wreck with energy. |
| `rts.salvage.v1.SetWreckEnergyCommand` | Set a wreck's energy; 0 removes it. |

**Player commands** may be submitted too. Ownership checks then pass for
any unit. Examples: cancel a unit's automation, issue a move for any
player. The issuer is the admin, so results go to admin streams.

## Clock control

| RPC | Behavior |
|---|---|
| `SetPaused(paused)` | Pause or resume the tick clock. Returns `{paused, tick}`. Commands submitted while paused run in the next executed tick. |
| `Step(count)` | While paused, execute `count` ticks (1..1000, default 1) asynchronously. Returns `{first_tick, count}`. Errors: `FailedPrecondition` if not paused, `Unavailable` if a step request is already pending, `InvalidArgument` for a bad count. |

Pausing and stepping change when ticks happen, never what they compute.

## Examples

```bash
# gRPC with grpcurl (reflection is enabled)
grpcurl -plaintext -H 'authorization: Bearer dev-admin' 127.0.0.1:50051 rts.v1.AdminService/GetStatus
grpcurl -plaintext -H 'authorization: Bearer dev-admin' -d '{"paused": true}' 127.0.0.1:50051 rts.v1.AdminService/SetPaused
grpcurl -plaintext -H 'authorization: Bearer dev-admin' -d '{"count": 10}' 127.0.0.1:50051 rts.v1.AdminService/Step

# HTTP admin gateway (protojson)
curl -s localhost:8081/api/status -H 'Authorization: Bearer dev-admin'
curl -s -X POST localhost:8081/api/command -H 'Authorization: Bearer dev-admin' -d '{
  "payload": {"@type": "type.googleapis.com/rts.wells.v1.CreateWellCommand",
              "position": {"x": 5, "y": 5}, "energy": "300", "capacity": "300"}}'
```

`grpcurl` is a third-party tool, not part of this repository.

The browser admin client wraps all of this. See
[administration](../operations/administration.md).
