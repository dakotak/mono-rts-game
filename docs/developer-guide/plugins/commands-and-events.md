# Commands, orders and events in plugins

How a plugin adds player-facing gameplay: commands to request things,
orders to keep doing them, events to report them, all without leaking
hidden information. Background: [commands.md](../commands.md) and
[events.md](../events.md).

## Adding a command

1. **Define a protobuf message** in `proto/rts/<plugin>/v1/<plugin>.proto`.
   Conventions used by every built-in plugin:
   - `repeated uint64 unit_ids = 1` for the units acted on
   - entity references as `uint64 <thing>_id`
   - positions as `rts.v1.Position`
   - quantities as `int64`
   - a comment documenting atomicity, cost and batch behavior
2. Run `make generate`.
3. Derive the type string from the descriptor:

   ```go
   var CommandX = string((&xv1.XCommand{}).ProtoReflect().Descriptor().FullName())
   ```

4. Register the handler:
   `r.Command(sim.CommandDef{Type: CommandX, Execute: p.execX, ApplyToUnit: p.applyX})`.
5. Add the proto file to `internal/protoreg.Files`.

Admin-only commands (operator tools such as `rts.wells.v1.CreateWellCommand` and
`rts.combat.v1.SetHealthCommand`) set `AdminOnly: true`. Players get them
rejected at submission.

## Validation and authorization

Follow the three-step contract (validate, charge, mutate) so commands are
atomic:

```go
func (p *Plugin) execX(ctx *sim.TickContext, cmd sim.Command) error {
    m, ok := cmd.Payload.(*xv1.XCommand)
    if !ok { return sim.Fail(sim.CodeInvalidArgument, "payload is %T", cmd.Payload) }
    // 1. validate: sizes, duplicates, ownership, references, ranges
    // 2. ctx.Charge(cmd.Issuer, len(units))
    // 3. mutate
}
```

**Ownership.** `e.Owner == cmd.Issuer.Player`, or `cmd.Issuer.Admin`.
Check it **at execution**; ownership can change between submission and
execution (for example, the unit was destroyed).

**Unavailable is uniform.** Return `sim.Unavailable("unit", id)` for a
missing unit, a foreign unit, and anything the player doesn't currently
observe. Different errors would let players probe for hidden entities.

**Foreign references under fog of war.** If your command refers to a
foreign entity (a target, a well), check that the issuer currently
detects it. Built-in plugins take a function such as
`Detected func(w *sim.World, player sim.PlayerID, id sim.EntityID) bool`,
and `game.Plugins()` wires it to `visibility.(*Plugin).Detected`. Do the
same; don't reach into the visibility plugin from a system.

**Limits.** Bound list sizes (built-ins use 1..4096) and every numeric
parameter. Reject duplicates.

## Budget

- Call `ctx.Charge(cmd.Issuer, n)` once, after validation, with `n` = the
  number of units the command affects.
- Admins and unlimited budgets are free.
- If it fails, return its error (`resource_exhausted`) without mutating.
- Commands that don't act on units (say, a global setting) may charge 0,
  but should still bound their work.

## Per-unit results and batches

- **`ApplyToUnit(ctx, cmd, unit)`** makes the command usable as a
  `BatchCommand` template over a selector. The template's unit list is
  ignored. Validate, `Charge(1)` and mutate for that one unit.
- **Partial success.** When a command over several units allows it,
  call `ctx.ReportUnit(unit, err)` per unit. The result is `PARTIAL` if
  the command returns `nil` while some units failed; return an error if
  none succeeded.
  `rts.movement.v1.GroupMoveCommand` is an example.

## Orders: commands that persist

Most gameplay commands don't do the work themselves. They store an
**order** component and a system advances it each tick:

```go
type order struct { Target sim.EntityID; Since sim.Tick }
var orders = sim.NewComponent[order]("myplugin.order")

// command: validate, charge, then
orders.Set(ctx.World, unit, order{Target: t, Since: ctx.Tick})

// system: advance every order, in sorted order
for _, id := range orders.Entities(w) {
    o, _ := orders.Get(w, id)
    ...
}
```

Good order design, as practiced by every built-in plugin:

- **Expose the order to the owner** as attributes (`myplugin.target`, ...)
  so clients can draw and inspect it.
- **End orders explicitly** with an event and a reason (`completed`,
  `target_lost`, ...). Waiting states (out of range, out of energy) emit
  **one** event when the wait starts, not one per tick.
- **Replacing** an order is just `Set` again; **stopping** is `Remove`,
  usually through a stop command.
- **Re-validate** referenced entities every tick. They can disappear at
  any time.

## Events

```go
ctx.Emit(sim.Event{
    Type:     "myplugin.happened",            // "<plugin>.<name>"
    Entity:   id,                             // primary entity or 0
    Audience: sim.ToPlayer(owner),            // or sim.ToAll(), sim.AdminAudience()
    Fields:   []sim.Field{{Key: "amount", Value: sim.IntValue(n)}},
})
```

- Entity IDs in fields go as **decimal strings**
  (`sim.StringValue(strconv.FormatUint(uint64(id), 10))`), because the
  wire's integer is signed.
- Keep per-tick event volume bounded. At scale, one event per unit per
  tick is the main source of traffic: `energy.mined` at 25k miners is
  25k events per tick.
- Use `sim.AdminAudience()` for diagnostics such as `automation.work`.

### Making events observable through the player API

Nothing to register: any event whose audience includes a player is
delivered in that player's `TickUpdate.events`, with `type`,
`entity_id` and `fields`. Document your events (add them to
[reference/events.md](../../reference/events.md)).

## Preventing hidden-information leaks

| Channel | Rule |
|---|---|
| Component attributes | `sim.VisibleOwner` for anything private. `sim.VisiblePublic` only for what every player may know. Under fog, foreign entities' attributes pass through disclosure rules, which default to hiding them. |
| Events | `sim.ToPlayer(owner)` by default. **`sim.ToAll()` bypasses fog**, so never use it for unit-specific information. If an event concerns another player's unit (an attacker's target), address only the players entitled to know. |
| Command errors | `sim.Unavailable` for anything not observable. Never say "exists but isn't yours". |
| Timing and results | Don't let whether a command succeeds depend on hidden state the issuer couldn't know, beyond physics like blocked movement. |
| Describe | Public. Rules only, never per-player data. |
| Automation-like logic | Decide only on what the owner can observe (wire the `Detected` hook). |

## Integrating with existing mechanics

Call other plugins' exported Go API and declare the dependency:

| Want to | Use |
|---|---|
| Read or change unit energy | `energy.GetStore`, `energy.Spend` (consumes), `energy.SetStored` (clamped) |
| Know whether a unit is moving, or send it somewhere | `movement.Destination`, `movement.SetDestination`, `movement.FreeTileNear` |
| Read an upgradable stat | `(*attributes.Plugin).Lookup` |
| Start mining, transfers, attacks or salvage | `energy.SetMining`, `energy.SetTransfer`, `combat.SetTarget`, `salvage.SetSalvage` (exactly what automation does) |
| Run before or after a built-in phase | `PhaseDef{After: []string{energy.Phase}, Before: []string{movement.Phase}}` |

These setters skip command validation: your plugin must enforce
ownership, visibility and adjacency itself. They also bypass the command
budget, so use them only for behavior the player already authorized
(automation, standing orders).
