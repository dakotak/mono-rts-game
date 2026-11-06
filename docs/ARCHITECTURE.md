# Architecture

## Overview

```
 browser ──HTTP/NDJSON──▶ rts-gateway ──gRPC (GameService)──┐
 AI agent / any client ──────────────gRPC (GameService)─────┤
 operator tools ─────────────────────gRPC (AdminService)────┤
                                                            ▼
                                   ┌───────────── rts-server ─────────────┐
                                   │ internal/server                      │
                                   │   sessions, auth, per-viewer filters │
                                   │   Submit ──▶ engine queue (mutex)    │
                                   │   Hub ◀── frames (read-only copy)    │
                                   ├──────────────────────────────────────┤
                                   │ internal/sim  (single goroutine)     │
                                   │   Engine: tick loop, command order   │
                                   │   World: entities, occupancy, comps  │
                                   │   Ruleset: plugins, phases, systems  │
                                   ├──────────────────────────────────────┤
                                   │ internal/plugins/movement            │
                                   └──────────────────────────────────────┘
```

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/sim` | Kernel: world lifecycle, ticks, entity identity and storage, command scheduling, plugin registry, phase ordering, events, change tracking, snapshots, seeded RNG. **No game mechanics, no protobuf, no networking.** | stdlib only |
| `internal/plugins/wells` | Energy wells: the well component, admin create/configure commands, initial placement from config. | `sim`, its own proto messages |
| `internal/plugins/energy` | Unit energy stores, mining orders, transfer orders, admin energy setting. Exposes `Stored`/`Spend` to other plugins. | `sim`, `wells`, its own proto messages |
| `internal/plugins/movement` | Movement orders, occupancy-respecting steps, conflict resolution, energy cost per step. | `sim`, `energy`, its own proto messages |
| `internal/game` | Reads config, picks the compiled-in plugins, builds ruleset and engine, places initial units. The only package that knows which plugins exist. | `sim`, plugins |
| `internal/server` | gRPC `GameService` and `AdminService`, sessions, command dedupe, observation hub, per-viewer filtering. | `sim`, `game`, `gen` |
| `internal/gateway` | Browser bridge. Translates HTTP+protojson to gRPC with two separate handlers, one for players (GameService only) and one for admins (AdminService only), meant for separate listeners. It's just another API client, with no game logic and no simulation access. | `gen` |
| `web/static` | `player/` and `admin/` clients plus `shared/` (canvas board, pointer input, attribute helpers, reconnecting stream reader). | gateway HTTP APIs |
| `web/static` | Player client: plain HTML, JS and canvas. | gateway HTTP API |

## Simulation kernel

### Tick

`Engine.Step()` runs one tick:

1. Take the queued commands under the engine mutex and assign them this tick.
2. Sort commands by **(admin first, player ID, per-issuer submission
   sequence)** and run each command's handler. Each command produces one
   `CommandResult`.
3. Run the phases in their resolved order, and the systems within each
   phase in their resolved order.
4. Collect changed and removed entities (dirty tracking), events and
   results into an immutable `Frame`, and hand it to the frame sink (the
   server's `Hub`).

`Engine.Run` calls `Step` on a wall-clock ticker. The clock only decides
*when* a tick runs, never *what* it computes. `Step` and `StepWith` can be
driven directly with no clock, which is how the tests and `rts-sim` work.

### Concurrency

Only the simulation goroutine touches `World`. Other goroutines interact
through two narrow channels:

- **In:** `Engine.Submit` appends to a mutex-protected queue and returns the
  tick the command *will* execute in. That tick is `nextTick`, read under
  the same lock that `Step` uses to drain the queue, so the scheduled tick
  in the acknowledgment is exact.
- **Out:** frames go to `Hub.Publish`, which never blocks. The hub keeps its
  own read-only copy of entity views (built only from frames). gRPC
  handlers read snapshots from the hub, never from the world.

### Determinism

Given the same config, seed and per-tick command log, results are
bit-identical. The rules for kernel and plugin code:

- Map iteration never affects outcomes: entities, component stores,
  phases, systems and plugins are always iterated in sorted order
  (`World.EntityIDs`, `Component.Entities`, topological sorts with name
  tie-breaks).
- Execution order within a tick is independent of network arrival order
  across issuers. Only an issuer's *own* submission order is preserved.
- Randomness comes only from `World.Rand()`, a SplitMix64 implemented in
  the kernel so its sequence never changes with Go releases.
- Integer arithmetic only. There is no wall-clock access inside a tick.
- `World.StateHash()` covers entities, every component value, the RNG
  state and the ID counter.
- Entity IDs come from a keyed function of a counter (next section), so
  they're deterministic for anyone holding the key.

Each `Frame.Commands` lists the commands executed that tick, in execution
order. Replaying them with `Engine.StepWith` from the same initial config
reproduces every intermediate hash. That's what
`internal/game/determinism_test.go` verifies, along with the cases where
the within-tick submission order is reversed.

### Entities and components

- Core entity data is `{ID, Kind, Owner, Pos}`. Kinds are `unit`,
  `resource` and `terrain`; only units are used in Milestone 1.
- **IDs are opaque.** Sequential IDs would tell any player roughly how many
  units exist and in what order they were made. Instead, the n-th entity
  ID is `HMAC-SHA256(id_key, "entity" || n)` truncated to 64 bits
  (`sim.DeriveID`). Zero and IDs of live entities are skipped.
  - Without the secret `id_key`, IDs look random. Seeing any number of them
    reveals nothing about the counter or other IDs.
  - With the key, the sequence is fully deterministic, so replays are exact.
    The key is part of the initial state (`sim.Config.IDKey`, at least 16
    bytes).
  - A plain PRNG would not be enough: SplitMix64 is invertible, so one
    observed ID would expose the counter.
  - A consequence: iterating in ID order is a fixed but scrambled order,
    unrelated to creation order. Determinism doesn't depend on which order
    it is, only that it's stable.
- **Occupancy invariant (Milestone 1):** every entity occupies exactly one
  tile, and a tile holds at most one entity. `World.MoveEntities` applies
  a batch of moves atomically and simultaneously, so a unit may enter a
  tile that another unit in the same batch is leaving.
- Plugins add data through typed components: `sim.NewComponent[T]("plugin.name")`.
  Each `T` must be a comparable value type (no pointers, slices or maps),
  which makes values copy-safe, hashable and cheap to diff, so writing an
  unchanged value doesn't mark the entity dirty. Storage is
  `map[componentID]map[EntityID]any`. There's no ECS framework, and none
  is needed at this scale (see the benchmarks).
- A component definition declares a `Visibility` (admin, owner or public)
  and an optional function that maps its value to named attributes. That's
  how plugin data reaches clients without core API changes.

## Plugin system

A plugin is a Go package implementing:

```go
type Plugin interface {
    Name() string          // "movement"; prefixes everything it registers
    DependsOn() []string
    Register(r *Registrar) error
}
```

Through the `Registrar`, a plugin can declare:

| Capability | API | Notes |
|---|---|---|
| Configuration schema | `r.DecodeConfig(&cfg)` | Strictly decodes the plugin's JSON section into a typed struct with defaults. Plugin-side validation completes the schema. |
| Components / attributes | `r.Component(comp.Def(vis, attrsFn))` | ID must start with `<plugin>.`. |
| Phases | `r.Phase(PhaseDef{Name, After})` | Global names, ordered topologically with name tie-breaks. |
| Systems | `r.System(SystemDef{Name, Phase, After, Run})` | `After` may only reference systems in the same phase. To order across phases, order the phases. |
| Commands | `r.Command(CommandDef{Type, AdminOnly, Execute})` | `Type` is the payload's protobuf full name. Handlers must validate fully before mutating, so commands are atomic. |
| Derived properties | `r.Derived(DerivedDef{Name, Compute})` | Read with `World.Derived(name, id)`. Movement pace and the energy extraction and transfer rates are derived, so future formulas or upgrades can vary them per unit. |
| World initialization | `r.Init(func(w *World) error)` | Runs once (`Engine.Init`) after config units are created, in plugin order. Wells places its initial wells here. |
| Creation hooks | `r.OnCreate(func(w *World, e Entity))` | Runs for every new entity, at setup, from commands or from systems. Energy attaches a store to every new unit here. |

`sim.NewRuleset` validates everything up front: unique plugin names,
dependencies that exist and don't cycle (registration follows dependency
order), config sections only for known plugins, unique and correctly
prefixed IDs, known phases, and acyclic phase and system ordering. The
error names the offender. `Ruleset.Schedule()` prints the resolved
per-tick order, and the server logs it at startup.

The kernel has three built-in admin-only commands. `AdminService` uses
them, and each emits an admin-only event:

| Command | Event |
|---|---|
| `sim.CreateEntity` | `sim.entity_created` |
| `sim.RemoveEntity` | `sim.entity_removed` |
| `sim.MoveEntity` (teleport to an empty tile) | `sim.entity_moved` |

An event with an empty `Audience` is admin-only.

Plugins may depend on each other's Go API, not only on registration order.
`movement` calls `energy.Stored`/`energy.Spend`, and `energy` calls
`wells.Get`/`wells.Set`. The dependency is declared with `DependsOn`, so
the registry enforces it.

**Clock control.** `Engine.SetPaused` stops `Run` from stepping on the
clock. `Engine.RequestSteps(n)` (paused only, `1 ≤ n ≤ 1000`, one pending
request at a time) makes `Run` execute exactly `n` ticks on the simulation
goroutine. Commands submitted while paused are scheduled for the next tick
that executes. Pausing and stepping change when ticks happen, never what
they compute, so replay is unaffected.

**Adding a plugin:** create `internal/plugins/<name>`, put its command
messages in `proto/rts/<name>/v1`, add it to `game.Plugins()`, and add a
blank import of its generated package to the gateway (protojson needs the
types to translate `Any`).

## Movement plugin (rules)

Each tick, in phase `movement`, every unit with an order that is off
cooldown proposes **one step**: one tile toward its destination on each
axis (8-directional, no pathfinding). All proposals are then resolved
together:

| Situation | Outcome |
|---|---|
| Target tile empty | Step succeeds. |
| Two or more units propose the same tile | **All fail.** Each waits a seeded-random `0..contention_backoff_ticks` extra ticks before retrying, which breaks symmetric deadlocks without favoring low IDs. |
| Target occupied by a unit that isn't stepping | Fails. |
| Target occupied by a unit that is stepping | Succeeds only if that unit's step succeeds. Chains move as one; a column follows its leader in the same tick. |
| Cycle, including a two-unit swap | The whole cycle fails. |

A failed step keeps the order. After `max_blocked_ticks` consecutive
failures the order is cancelled with a `movement.cancelled` event. Reaching
the destination emits `movement.arrived`. After a successful step the unit
waits `movement.ticks_per_move` ticks (a derived property) before stepping
again. That cooldown is stored separately from the order, so reissuing
orders can't make a unit move faster.

`MoveCommand` carries an explicit destination per unit, so clients choose
formations themselves. The web client spreads a group over the nearest free
tiles. Validation is atomic: an unknown unit, someone else's unit, an
out-of-bounds or missing destination, or a duplicate unit fails the whole
command with a coded error. A destination attribute is exposed to the owner
only.

## Wells and energy plugins (rules)

All quantities are `int64` energy units. Rates are per tick.

**Wells** (`wells` plugin) are resource entities with
`{energy, capacity, extraction_rate, unlimited}`. All four are public
attributes (`wells.*`), and every player sees every well.

- Config: `defaults` plus a list of initial `wells`, each of which may
  override any default.
- Admin commands: `CreateWellCommand` (unset fields use the defaults) and
  `ConfigureWellCommand` (only set fields change). Both enforce
  `0 ≤ energy ≤ capacity` and `extraction_rate ≥ 0`.
- Reducing `capacity` alone clamps `energy` down to it.
- Wells are deleted or moved with the generic `DeleteEntity` and
  `MoveEntity` admin RPCs.

**Unit storage** (`energy` plugin):

- Every unit gets `{energy: initial_energy, capacity: unit_capacity}` when
  it's created.
- `energy.stored` and `energy.capacity` are owner-only attributes; opponents
  can't see them.
- There is no passive upkeep. Energy changes only through mining,
  transfers, `Spend` (movement) and the admin `SetUnitEnergyCommand`.

The phases run in this order every tick: commands, then `energy`
(`energy.mine`, then `energy.transfer`), then `movement`. Energy settles
before units move and pay.

**Mining.** `MineCommand{unit_ids, well_id}` sets a persistent order. A
unit extracts only while it's adjacent to the well (Chebyshev distance 1,
diagonals included). Otherwise the order waits, so a client typically
sends a `MoveCommand` to a tile next to the well along with the mine
order. Each tick:

1. Each adjacent miner asks for `min(extraction_rate, free capacity)`.
2. The well releases at most `extraction_rate`. A finite well releases at
   most its remaining energy too.
3. If the requests exceed the release, it's split **max-min fairly**:
   requests are visited smallest first (ties by entity ID), and each gets
   `min(request, remaining ÷ claimants left)`.

The grants never exceed the release, and a finite well loses exactly what
units gain. Example: 5 split among three requests of 4 gives 1, 2, 2.

- The miner gets an `energy.mined` event `{amount, well}` (owner only).
- A finite well that reaches 0 emits public `energy.well_depleted`, and
  every order on it ends with `energy.mining_stopped` (`well_depleted`).
- A removed well ends orders with `well_removed`.
- A full unit keeps its order and resumes once it has free capacity.
- `StopMiningCommand` cancels.

**Transfers.** `TransferCommand{from, to, amount}` requires:

- both are units of the issuer (an admin may issue it, but only between
  units of one owner),
- `from ≠ to`,
- `amount > 0`,
- the units are adjacent when the command executes.

It sets a persistent order on the sender, replacing any earlier one. Each
tick, senders are processed in ascending ID order. Each moves
`min(transfer_rate, amount left, sender energy, receiver free)` and emits
`energy.transferred {to, amount}`. The order ends with
`energy.transfer_ended {reason, remaining}`, where the reason is one of:

- `completed`
- `sender_empty`
- `receiver_full`
- `not_adjacent`
- `target_lost`

Energy is only ever moved, never created or destroyed.

**Movement cost.** Each successful step costs `movement.energy_per_step`
(default 0, so Milestone 1 behaviour is unchanged).

- A unit that can't pay doesn't propose a step. It neither moves nor
  counts as blocked, so `max_blocked_ticks` doesn't cancel it.
- It keeps its order and emits one `movement.stalled {required, stored}`
  event when it starts waiting.
- It resumes as soon as it has enough energy, for example after a
  transfer.
- Failed steps cost nothing. Spent energy is consumed (it leaves the
  economy).

**Conservation (tested).** With free movement and finite wells, the energy
in all units plus all finite wells is constant every tick, under random
mining, transfer and move commands.

## Public API

Protobuf files live in `proto/`, are linted by `buf` (STANDARD rules) and
checked for breaking changes with the FILE rules.

**`rts.v1.GameService`** (players, humans and AI alike):

- `Connect(join_token)` returns the player's own opaque ID and name, a
  session token and `WorldInfo` (size, tick rate, accepted command types,
  plugins). Calling it again with the same join token returns another
  session for the same player.
  - There is deliberately no player roster, since it would disclose the
    player count. Clients learn about other players only from what they
    observe (unit `owner_id`s). The admin snapshot lists all players.
- `SubmitCommand(command_id, Any payload)` returns an **acknowledgment**:
  accepted with `scheduled_tick`, or rejected with a reason. Rejection
  covers envelope-level problems: an undecodable payload, an unknown
  command type, or a missing payload.
  - **Execution** happens later. Its `CommandResult` (succeeded, or failed
    with `error_code` and message) arrives in the issuer's observation
    stream.
  - Command IDs are per player and idempotent: resubmitting an ID returns
    the original ack without scheduling the command again. The last 4,096
    IDs are remembered.
- `Observe()` opens a stream. It starts with a full `WorldSnapshot`,
  followed by one `TickUpdate` per tick (changed entities as full
  replacements, removed IDs, events, own command results).
  - If a client falls more than 128 ticks behind, the server drops the
    queued updates and sends a fresh snapshot.
  - Reconnecting means calling `Observe` again. A snapshot plus the updates
    that follow always equals a later snapshot (tested).
- `GetSnapshot()` returns the latest player-visible state.

Authentication uses `authorization: Bearer <session>` metadata.

**Extensibility without core changes:**

- Commands are `google.protobuf.Any`, owned and versioned by their plugin
  (`rts.movement.v1.MoveCommand`).
- Observable plugin state is an attribute map (`movement.destination`).
- Events are typed by name, with a field map.

**`rts.v1.AdminService`** (privileged, separate credential
`authorization: Bearer <admin_token>`; player sessions are refused and the
admin token is refused by `GameService`; every RPC checks it, which is
tested per RPC):

| RPC | Purpose |
|---|---|
| `GetWorldSnapshot` | Everything, unfiltered (owner-only and admin-only attributes), plus all players, `WorldInfo` and `SimulationStatus` |
| `ObserveWorld` | Stream: snapshot, then every tick's full changes, **all** events and admin command results |
| `GetStatus` | Last tick, paused, tick rate |
| `SpawnUnit`, `DeleteEntity`, `MoveEntity` | Kernel built-in commands |
| `SubmitAdminCommand(Any)` | Any plugin command with admin privileges, e.g. `CreateWellCommand`, `ConfigureWellCommand`, `SetUnitEnergyCommand`, or a player command applied to any unit |
| `SetPaused`, `Step(count)` | Clock control (`Step` returns `FailedPrecondition` unless paused) |

Every mutation is a command scheduled through `Engine.Submit` with an
admin principal. Mutations therefore:

- run on the simulation goroutine,
- run first in their tick (admin commands sort before players),
- appear in `Frame.Commands`, so admin activity replays exactly (tested
  with random spawns, moves, deletes and well edits).

Mutation RPCs return `{command_id, scheduled_tick}`, and the outcome
arrives on `ObserveWorld`. Players never receive admin results or
admin-only events, and players can't submit admin-only command types
(rejected at acceptance).

**Player filtering (Milestone 1):**

- Every entity's core data is visible to every player. There is no fog
  of war yet.
- Attributes are shown according to their visibility: owner-only
  attributes go only to the owner.
- Events go only to their audience, and command results go only to their
  issuer.

### Browser gateway

`rts-gateway` runs two listeners with two separate handlers, the player
gateway (`-addr`, default `:8080`) and the admin gateway (`-admin-addr`,
default `:8081`, empty to disable). The player handler only ever calls
`GameService` and serves `web/static/player`. The admin handler only ever
calls `AdminService` and serves `web/static/admin`. Both serve
`web/static/shared`.

Player gateway:

| Endpoint | Request → response |
|---|---|
| `POST /api/connect` | `ConnectRequest` → `ConnectResponse` |
| `POST /api/command` | `SubmitCommandRequest` → `SubmitCommandResponse` |
| `GET /api/snapshot` | → `GetSnapshotResponse` |
| `GET /api/observe` | → `ObserveResponse` stream, one per line |

Admin gateway:

| Endpoint | RPC |
|---|---|
| `GET /api/snapshot` | `GetWorldSnapshot` |
| `GET /api/status` | `GetStatus` |
| `GET /api/observe` | `ObserveWorld` stream |
| `POST /api/spawn` | `SpawnUnit` |
| `POST /api/delete` | `DeleteEntity` |
| `POST /api/move` | `MoveEntity` |
| `POST /api/command` | `SubmitAdminCommand` |
| `POST /api/pause` | `SetPaused` |
| `POST /api/step` | `Step` |

All messages are canonical protojson, streamed as NDJSON over `fetch`, so
headers work without tokens in URLs. It forwards the `Authorization`
header as gRPC metadata.

Why not grpc-web or Connect? NDJSON needs no proxy, no JS codegen and no
frontend build. Protojson keeps the browser contract identical to the
protobuf one.

## Key decisions and tradeoffs

| Decision | Why | Cost / revisit when |
|---|---|---|
| Kernel is proto-free; command payloads are `any` | Kernel stays testable and transport-agnostic | Persisting replay logs to disk needs a payload codec (plugins' payloads are proto already) |
| Typed generic component handles over `map[..]any` | Type safety at call sites, no ECS dependency | Profile if entity counts reach ~10⁵ |
| All contenders fail on a contested tile | Literal spec rule, no ID bias | Needs random backoff to avoid livelock (implemented) |
| Cycles and swaps fail | Simplest well-defined rule | Revisit if formations feel sticky |
| One entity per tile, kernel-enforced | Spec invariant; trivial spatial index | Resources and terrain may need layered occupancy |
| Hub holds a copy of views | Network reads never contend with the sim | Memory is O(entities). Fine. |
| Sessions in memory, issued from config join tokens | Simplest auth that separates player and admin | Real accounts and persistence later |
| Full entity replacement in updates | Simple client merge | Attribute-level deltas if bandwidth matters |
| Admin mutations are ordinary scheduled commands | One code path for ordering, validation and replay | Admin can't act "between" ticks. Fine. |
| Wells and energy are separate plugins | Wells (data and admin) shipped and tested before the economy existed | One more inter-plugin API (`wells.Get`/`Set`) |
| Movement depends on energy | Simplest way to charge for steps | A movement-only game must also load energy (cost 0 keeps it free) |
| Max-min fair split for contested wells | No first-come advantage, deterministic, conserves energy | Small integer remainders favour lower-ID claimants by at most 1 |
| Persistent transfer orders with a per-tick rate | Makes "transfer rate" meaningful | A one-shot transfer can take several ticks |
| Energy levels are owner-only | Hidden information for future sensors | Opponents see wells but not unit energy |

## Assumptions made (unspecified details)

- Tick 0 is the initial state, and the first executed tick is 1. A command
  accepted while tick *N* is being computed runs in tick *N+1*.
- Admin commands run before player commands in a tick. Players run in
  ascending ID order. Because player IDs are derived from the key, that
  order is fixed per world but doesn't favour any particular config slot.
- Player IDs are `DeriveID(id_key, "player", n)` truncated to 32 bits,
  skipping zero and duplicates. Config files refer to players by a `slot`
  number that is never exposed.
- If `id_key` is not configured, a random one is generated at startup. IDs
  then differ between server runs, which is fine because sessions don't
  survive restarts anyway.
- 8-directional movement, with diagonal steps costing the same as straight
  ones.
- Moving to the current tile completes immediately with `movement.arrived`.
- Join tokens are static per config, and anyone with a token plays that
  slot. Multiple concurrent sessions per player are allowed.
- Admin command results and admin-only events reach admins only, through
  `ObserveWorld`. Several admin sessions share one admin identity and see
  each other's results.
- `MoveEntity` (teleport) leaves movement, mining and transfer orders in
  place. Adjacency rules then apply from the new position.
- A mining order may target a distant well and waits until the unit is
  adjacent. Units that never reach an adjacent tile wait indefinitely; the
  player can stop them.
- A full miner keeps its order. Transfers end, rather than wait, when the
  receiver is full or the sender is empty.
- Admins may set any unit's energy and capacity (`SetUnitEnergyCommand`).
  This is the one sanctioned way to create or destroy energy outside
  mining and movement.
- Entity IDs inside attribute and event values (`energy.mining_well`,
  `energy.transfer_to`, `to`, `well`) are decimal strings, because
  `Value.int_value` is signed 64-bit.
- Initial entities appear as "changed" in tick 1's update. That's harmless,
  because clients start from a snapshot.

## Deferred (not in Milestones 1–2)

- Fog of war and sensors: per-entity visibility, sensor strength,
  approximate observations.
- Reproduction, upgrades, combat, salvage.
- Energy costs for actions other than movement.
- Terrain entities, and layered occupancy (wells currently block their
  tile like units).
- Configurable deterministic expression language for derived properties.
- Pathfinding and smarter collision handling.
- Persistent replay files, save and load of world state.
- Admin: runtime world-configuration changes (rates, defaults), editing
  arbitrary unit attributes, multi-admin identities and audit log.
- Runtime-loaded plugins (WASM or similar), with the `Plugin`/`Registrar`
  boundary designed for it.
- TLS, real authentication, rate limiting, per-session command quotas.

## Proposed next milestones

1. **Visibility.** A per-player visibility system plugin plus a kernel hook
   for per-entity observation filtering. Observation updates that add and
   remove entities as they enter and leave sight.
2. ~~Energy and resources~~ (done in Milestone 2).
3. **Attributes and upgrades.** A generic numeric attribute component with
   a restricted, deterministic, integer formula language for derived
   properties (mass and propulsion → pace), plus nonlinear upgrade costs.
4. **Reproduction.** Energy-conserving unit creation with inherited
   upgrades.
5. **Combat.** Weapons, cooldowns, armor, simultaneous damage resolution
   in its own phase, salvage.
6. **Operations.** Replay files and a replay CLI, save and load, a richer
   AdminService, metrics.
