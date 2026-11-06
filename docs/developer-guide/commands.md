# Commands

Commands are the **only** way anything outside the simulation changes
the world, whether it comes from a player or an admin. Every command goes
through the same queue, ordering and handlers, so every change is
deterministic and replayable.

Source: [`internal/sim/command.go`](../../internal/sim/command.go),
[`engine.go`](../../internal/sim/engine.go),
[`internal/server/server.go`](../../internal/server/server.go).

## Anatomy

```go
type Command struct {
    ID      string        // chosen by the issuer, echoed in the result
    Issuer  sim.Principal // {Player PlayerID; Admin bool}
    Type    string        // handler key: the payload's protobuf full name
    Payload any           // the decoded message, e.g. *movementv1.MoveCommand
    Tick    sim.Tick      // assigned by Submit
    Seq     uint64        // per-issuer submission sequence, assigned by Submit
}
```

- **Type.** For network commands, `Type` is the fully qualified protobuf
  message name, e.g. `rts.movement.v1.MoveCommand`. The server derives it
  from the `Any` type URL. The kernel's built-in admin commands
  (`sim.CreateEntity`, `sim.RemoveEntity`, `sim.MoveEntity`) use Go
  structs as payloads and are only reachable through `AdminService`'s
  dedicated RPCs.
- **Payload.** The kernel itself is protobuf-free: payloads are `any`, and
  handlers type-assert them.

## Lifecycle

1. **Submit** (any goroutine): `Engine.Submit` checks the envelope (known
   type, admin-only), assigns `Seq` and `Tick`, and queues it. The gRPC
   acknowledgment carries `scheduled_tick`.
2. **Order** (start of the tick): admin first, then player ID, then `Seq`.
3. **Execute:** `CommandDef.Execute(ctx, cmd)` returns `nil` or an error.
   - `sim.Fail(code, format, ...)` gives a coded `*sim.CommandError`.
   - Any other error becomes `internal`.
4. **Report:** one `CommandResult{OK, Code, Message, Units, Partial}` per
   command, delivered only to the issuer (admins see admin results).

## Handler contract

```go
type CommandDef struct {
    Type        string
    AdminOnly   bool
    Execute     func(ctx *sim.TickContext, cmd sim.Command) error
    ApplyToUnit func(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error // optional
}
```

Every handler in the repository follows the same contract:

1. **Type-assert the payload**, or fail with `invalid_argument`.
2. **Validate everything** before touching the world: unit lists (size
   1..4096, no duplicates), ownership, referenced entities, parameter
   ranges, preconditions.
3. **Charge the budget**: `ctx.Charge(cmd.Issuer, n)` with `n` units
   affected. That's the last step that may fail.
4. **Mutate.**

That makes commands **atomic**: a failed command leaves the world
unchanged. Batch-style commands that allow partial success report per
unit with `ctx.ReportUnit(unit, err)`. The result is then `Partial` if the
command succeeded but some units failed, and `FAILED` if none succeeded.

### Authorization

- **Admin-only commands** are rejected for players at `Submit` time.
- **Ownership** is checked in the handler.
- **Unavailable references.** For anything a player may not act on or
  refer to (missing, not theirs, not currently detected), handlers return
  **`sim.Unavailable(what, id)`**: always `not_found` with the same
  message shape. Distinguishing those cases would let players probe for
  hidden units.
- **Admins** have `Issuer.Admin == true`. Handlers usually let admins act
  on any unit.

### Per-unit application (batches)

When `ApplyToUnit` is set, the command can be a template in
`rts.metadata.v1.BatchCommand`. The batch handler resolves the selector
**at execution time** and calls `ctx.ApplyToUnit(template, unit)` for each
selected unit. `ApplyToUnit` must validate that one unit, charge 1 and
mutate, atomically for that unit. The template's own unit list is
ignored.

## Error codes

| Code | Meaning |
|---|---|
| `invalid_argument` | Malformed payload or parameters out of range. |
| `not_found` | The entity doesn't exist, isn't yours, or isn't currently observable (deliberately indistinguishable). |
| `permission_denied` | The request targets something it may never target: a friendly unit for an attack, a foreign receiver for a transfer, a selector `owner_id` from a player, or an admin-only template in a batch. |
| `failed_precondition` | A valid request the world state doesn't allow right now (occupied tile, busy unit, cycle, ...). |
| `resource_exhausted` | Per-tick unit-command budget exceeded. |
| `insufficient_energy` | The unit can't pay up front (upgrades, cloning). Defined by energy as `energy.CodeInsufficientEnergy`. |
| `internal` | A non-coded error escaped a handler (a bug). |

Plugins may define more codes. The [command reference](../reference/commands.md)
lists every command.

## Commands vs. orders

Most player commands don't act once. They install an **order**, a
component that a system advances every tick until it ends: movement,
mining, transfers, attacks, salvage, upgrades, cloning, automation tasks,
supply links. Order components are owner-visible, so clients can display
them, and every order end is reported with an event and a reason.

This split keeps per-tick command traffic low (see the
[budget](simulation.md#command-budget)) and makes the world's evolution a
pure function of state plus the command log.
