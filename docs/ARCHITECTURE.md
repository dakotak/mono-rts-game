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
| `internal/plugins/movement` | Movement orders, occupancy-respecting steps, conflict resolution. | `sim`, its own proto messages |
| `internal/game` | Reads config, picks the compiled-in plugins, builds ruleset and engine, places initial units. The only package that knows which plugins exist. | `sim`, plugins |
| `internal/server` | gRPC `GameService` and `AdminService`, sessions, command dedupe, observation hub, per-viewer filtering. | `sim`, `game`, `gen` |
| `internal/gateway` | Browser bridge. Translates HTTP+protojson to gRPC. It's just another API client, with no game logic and no simulation access. | `gen` |
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
| Derived properties | `r.Derived(DerivedDef{Name, Compute})` | Read with `World.Derived(name, id)`. Movement pace uses one so a future formula can replace the constant. |

`sim.NewRuleset` validates everything up front: unique plugin names,
dependencies that exist and don't cycle (registration follows dependency
order), config sections only for known plugins, unique and correctly
prefixed IDs, known phases, and acyclic phase and system ordering. The
error names the offender. `Ruleset.Schedule()` prints the resolved
per-tick order, and the server logs it at startup.

The kernel has one built-in command, `sim.CreateEntity` (admin only), which
`AdminService.SpawnUnit` uses.

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

## Public API

Protobuf files live in `proto/`, are linted by `buf` (STANDARD rules) and
checked for breaking changes with the FILE rules.

**`rts.v1.GameService`** (players, humans and AI alike):

- `Connect(join_token)` returns the player's own opaque ID and name, a
  session token and `WorldInfo` (size, tick rate, accepted command types,
  plugins).
  - There is deliberately no player roster, since it would disclose the
    player count. Clients learn about other players only from what they
    observe (unit `owner_id`s). The admin snapshot lists all players.
  Calling it again with the same join token returns another session for
  the same player.
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
admin token is refused by `GameService`):

- `GetWorldSnapshot` returns everything, unfiltered.
- `SpawnUnit` is scheduled through the engine like any other command.
- `SetPaused` pauses and resumes the clock.

**Player filtering (Milestone 1):**

- Every entity's core data is visible to every player. There is no fog
  of war yet.
- Attributes are shown according to their visibility: owner-only
  attributes go only to the owner.
- Events go only to their audience, and command results go only to their
  issuer.

### Browser gateway

`rts-gateway` serves the web client and exposes these endpoints:

| Endpoint | Request → response |
|---|---|
| `POST /api/connect` | `ConnectRequest` → `ConnectResponse` |
| `POST /api/command` | `SubmitCommandRequest` → `SubmitCommandResponse` |
| `GET /api/snapshot` | → `GetSnapshotResponse` |
| `GET /api/observe` | → `ObserveResponse` stream, one per line |

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
- Execution results of admin commands aren't streamed anywhere in
  Milestone 1. Inspect state with `GetWorldSnapshot`.
- Initial entities appear as "changed" in tick 1's update. That's harmless,
  because clients start from a snapshot.

## Deferred (not in Milestone 1)

- Fog of war and sensors: per-entity visibility, sensor strength,
  approximate observations.
- Energy (local storage, mining, transfers, action costs), reproduction,
  upgrades, combat, salvage.
- Resource and terrain entities with their own occupancy semantics.
- Configurable deterministic expression language for derived properties.
- Pathfinding and smarter collision handling.
- Persistent replay files, save and load of world state.
- Admin: entity inspection and modification, resource placement, config
  changes, an admin event stream, single-step.
- Runtime-loaded plugins (WASM or similar), with the `Plugin`/`Registrar`
  boundary designed for it.
- TLS, real authentication, rate limiting, per-session command quotas.

## Proposed next milestones

1. **Visibility.** A per-player visibility system plugin plus a kernel hook
   for per-entity observation filtering. Observation updates that add and
   remove entities as they enter and leave sight.
2. **Energy and resources.** Resource well entities, local unit energy,
   mining and transfer commands, energy costs on actions. Energy
   conservation tests.
3. **Attributes and upgrades.** A generic numeric attribute component with
   a restricted, deterministic, integer formula language for derived
   properties (mass and propulsion → pace), plus nonlinear upgrade costs.
4. **Reproduction.** Energy-conserving unit creation with inherited
   upgrades.
5. **Combat.** Weapons, cooldowns, armor, simultaneous damage resolution
   in its own phase, salvage.
6. **Operations.** Replay files and a replay CLI, save and load, a richer
   AdminService, metrics.
