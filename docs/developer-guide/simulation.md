# The simulation

How a tick executes, how commands are scheduled and ordered, how
conflicts are resolved deterministically, and where concurrency is
allowed. Source: [`internal/sim/engine.go`](../../internal/sim/engine.go),
[`command.go`](../../internal/sim/command.go),
[`plugin.go`](../../internal/sim/plugin.go),
[`world.go`](../../internal/sim/world.go).

## Clock

- **Ticks.** `sim.Tick` 0 is the initial state; the first executed tick is
  1.
- **`Engine.Run(ctx, interval)`** calls `Step` on a wall-clock ticker. The
  interval is `game.Config.TickInterval()` = `1s / tick_rate_hz`. The
  clock only decides **when** a tick runs, never **what** it computes.
- **`Engine.Step()`** executes one tick with everything queued.
  **`Engine.StepWith(cmds)`** executes one tick with exactly the given
  commands, which is how replays work. Tests and `rts-sim` call these
  directly with no clock.
- **If a tick takes longer than the interval**, Go's `time.Ticker` drops
  missed ticks. The simulation then runs slower than real time but stays
  correct.

**Pause and step.**

- `Engine.SetPaused(true)` stops `Run` from stepping.
- `Engine.RequestSteps(n)` (paused only, 1 ≤ n ≤ `sim.MaxStepRequest` =
  1000, one request pending at a time) makes `Run` execute exactly `n`
  ticks.
- Commands submitted while paused execute in the next tick that runs.

## Tick lifecycle (`Engine.execute`)

1. **Drain.** Under the engine mutex, take the pending commands and
   advance `nextTick`.
2. **Order commands.** Sort stably by `commandLess`: **admin commands
   first, then by player ID, then by the issuer's own submission
   sequence** (`Command.Seq`). Network arrival order between issuers never
   matters. Player IDs are opaque and derived from the ID key, so the
   player order is fixed per world but favors no config slot.
3. **Execute commands.** For each command, run `CommandDef.Execute(ctx,
   cmd)`. The returned error becomes a `CommandResult` (`OK` or a code from
   a `*sim.CommandError`; other errors become `internal`). Per-unit
   outcomes from `ctx.ReportUnit` are attached.
4. **Run phases.** In resolved order, and the systems within each phase in
   resolved order.
5. **Observe.** If a plugin registered an `ObservationAuthority`
   (visibility), call `Observations(ctx)` **after all systems**, so players
   only see settled end-of-tick state. Its events join the tick's events.
6. **Collect.** `World.takeChanges()` returns full views of entities
   created or modified this tick (sorted by ID) and removed IDs.
7. **Publish.** Build the immutable `sim.Frame` and hand it to the frame
   sink (`Hub.Publish`).

All of this runs on one goroutine. A `*sim.TickContext` (`ctx`) is shared
by the tick's commands and systems. It carries the `World`, the `Tick`,
the event list and the budget counters.

## Phase and system ordering

- **Phases** are declared by plugins (`sim.PhaseDef{Name, After, Before}`).
- **Systems** are declared per phase (`sim.SystemDef{Name, Phase, After,
  Run}`).
- `sim.NewRuleset` topologically sorts phases by `After` and `Before`, and
  systems within a phase by `After`. Ties are always broken by **name**,
  so the order never depends on map iteration or registration order.
- `Before` lets a plugin place its phase ahead of another plugin's phase
  without that plugin knowing. Combat and salvage use it.
- Systems can only order against systems in the same phase. To order
  across phases, order the phases.

The resolved order is available as `Ruleset.Schedule()`. The server logs
it at startup and publishes it in `WorldInfo.schedule`.

## Command scheduling and acceptance

`Engine.Submit(cmd)` may be called from any goroutine. It checks only the
**envelope**:

- the type is registered (`ErrUnknownCommand`)
- the command isn't admin-only for a player (`ErrAdminOnly`)

It then assigns `Seq` and `Tick = nextTick` under the same mutex `Step`
uses, so the returned scheduled tick is exact. Everything that depends on
world state is checked when the command **executes**.

The gRPC layer adds:

- authentication
- command-ID validation (1..128 bytes)
- per-player idempotency (the last 4096 acknowledgments are remembered;
  resubmitting an ID returns the original acknowledgment)
- `Any` decoding through `protoreg`

## Command budget

`world.unit_command_budget` (`sim.Config.UnitCommandBudget`) limits
**unit-commands per player per tick**:

- Handlers call `ctx.Charge(issuer, n)` **after validation and before
  mutating**, with `n` = units affected. Over budget, the command fails
  with `resource_exhausted` and changes nothing.
- Admin commands are never charged. Invalid commands cost nothing.
  Budgets reset every tick.
- `ctx.BudgetLeft(issuer)` reports the remainder.

## Deterministic conflict resolution

Conflicts happen when several actors want the same thing in the same
tick. The rule throughout is to **compute every claim from the same
snapshot, then resolve with a fixed, ID-agnostic or explicitly tie-broken
rule**:

| Conflict | Resolution | Where |
|---|---|---|
| Several units step onto one tile | All fail, then a seeded random backoff (`World.Rand`) | `movement` |
| Chains and cycles of moving units | Chains move together; cycles and swaps fail | `movement`, `World.MoveEntities` (atomic batch) |
| Miners on one well, collectors on one wreck | `fairshare.Split`: max-min fair, ties by ID, conserving | `energy`, `salvage` |
| Simultaneous attacks | All computed from start-of-phase state; damage summed; destruction last | `combat` |
| Several clones need tiles | Parents in ascending ID order claim tiles one at a time | `cloning` |
| Supply links competing for a receiver or a supplier | Two-stage max-min split from a phase-start snapshot, applied as deltas | `logistics` |
| Equal-cost paths | A* tie-break: h, then squared Euclidean distance, then tile index | `movement/path.go` |
| Guard / auto-salvage target choice | Nearest by distance, ties by ID | `automation` |

## Bounded work

Everything a tick does is bounded:

- **Commands:** at most 4096 units per command, plus the per-player
  budget.
- **Pathfinding:** `max_path_nodes` per plan, `plans_per_tick` per tick.
- **Automation:** `max_evaluations_per_tick` per tick, with a staggered
  evaluation schedule.
- **Scans:** guard scans are bounded by a radius of 16, and vision by 32.

## Concurrency boundaries

| Goroutine | May touch |
|---|---|
| Simulation (`Run`/`Step`) | The `World`, every plugin's state, `TickContext` |
| gRPC handlers | `Engine.Submit`, `SetPaused`, `Paused`, `RequestSteps`, `NextTick`; the `Hub` (its own mutex) |
| Hub subscribers | Their buffered channel of published frames |

There is no concurrency inside a tick. Systems run sequentially, and
plugins must not start goroutines that touch the world. Frames are
immutable once produced, so they can be shared with other goroutines.

## Entity storage and change tracking

See [entities.md](entities.md). In short:

- The kernel tracks a dirty set per tick: created entities, any
  component `Set` or `Remove` that actually changes a value, and moves.
- `takeChanges` turns that set into full views (`EntityView` with all
  attributes tagged by visibility).
- Removed IDs are reported separately.
