# Architecture

Mono is one Go module (`github.com/dakotaknutson/unit-strategy-game`) that
builds a few binaries. The core idea: a **deterministic simulation kernel**
that knows nothing about game rules or networking, **plugins** that
implement every mechanic, and a **server** that turns the kernel's output
into per-player views.

## System overview

```mermaid
flowchart TB
  subgraph Clients
    BR[Browser<br/>web/static/player, admin]
    BOT[Bots / gRPC clients]
    OPS[Operator tools<br/>grpcurl, scripts]
  end
  subgraph GW[rts-gateway · internal/gateway]
    PH[Player handler :8080<br/>GameService only]
    AH[Admin handler :8081<br/>AdminService only]
  end
  subgraph SRV[rts-server]
    GS[gameService<br/>internal/server/server.go]
    AS[adminService<br/>internal/server/admin.go]
    HUB[Hub<br/>internal/server/hub.go<br/>read-only copy of views,<br/>per-player observations]
    ENG[sim.Engine<br/>internal/sim/engine.go]
    W[(sim.World<br/>entities, components)]
    RS[sim.Ruleset<br/>plugins, phases, systems,<br/>commands, derived props]
  end
  BR -- HTTP/protojson, NDJSON streams --> PH & AH
  PH -- gRPC --> GS
  AH -- gRPC --> AS
  BOT -- gRPC --> GS
  OPS -- gRPC --> AS
  GS & AS -- Engine.Submit --> ENG
  ENG -- owns --> W
  ENG -- uses --> RS
  ENG -- Frame per tick --> HUB
  GS & AS -- snapshots, streams --> HUB
```

## Packages

| Package | Responsibility | Imports |
|---|---|---|
| [`internal/sim`](../../internal/sim) | The kernel: `World` (entities, occupancy, components, change tracking), `Engine` (tick loop, command queue and ordering, budget), `Ruleset` (plugin registry, phase and system ordering), events, frames, `StateHash`, seeded `Rand`, opaque IDs. **No game rules, no protobuf, no networking.** | stdlib |
| [`internal/expr`](../../internal/expr) | Deterministic integer formula language. | stdlib |
| [`internal/fairshare`](../../internal/fairshare) | Max-min fair integer split, used by mining, salvage and logistics. | stdlib |
| [`internal/plugins/*`](../../internal/plugins) | One package per mechanic: `terrain`, `wells`, `attributes`, `energy`, `upgrades`, `cloning`, `movement`, `metadata`, `combat`, `salvage`, `automation`, `logistics`, `visibility`. | `sim`, each other (declared with `DependsOn`), their own `gen/` packages |
| [`internal/game`](../../internal/game) | Config loading and validation, plugin composition (`game.Plugins()`), building the engine, deriving player IDs, placing initial units. The **only** package that knows which plugins exist. | `sim`, plugins |
| [`internal/protoreg`](../../internal/protoreg) | Imports every plugin's generated proto package so `Any` payloads can be decoded by name. | `gen` |
| [`internal/server`](../../internal/server) | gRPC `GameService` and `AdminService`, sessions, command dedupe, the `Hub`, per-viewer filtering. | `sim`, `game`, `gen`, `protoreg` |
| [`internal/gateway`](../../internal/gateway) | HTTP/NDJSON ↔ gRPC bridge for browsers. Just another API client. | `gen`, `protoreg` |
| [`internal/plugintest`](../../internal/plugintest) | Test harness over the real plugin composition. | `game`, plugins |
| [`internal/scale`](../../internal/scale) | Repeatable large-world scenarios and measurement. | `game`, plugins |
| [`proto/`](../../proto) → [`gen/`](../../gen) | Public API (`rts.v1`) and one package per plugin for its command messages. Generated code is committed. | — |
| [`web/static`](../../web/static) | Player and admin clients (plain JS, no build step) and shared rendering code. | gateway HTTP API |
| [`examples/`](../../examples) | Example plugin (`examples/plugins/regeneration`) and bot (`examples/bot`). Not part of the server build. | — |

Dependency direction: `sim` ← plugins ← `game` ← `server` ← binaries.
Plugins never import `server`; the kernel never imports anything
game-specific.

## Ownership of authoritative state

- **All authoritative state lives in one `sim.World`**: entities
  (`ID`, `Kind`, `Owner`, `Pos`, `Passable`) and typed component values.
  There are no package-level mutable game variables.
- **Only the simulation goroutine touches the `World`**: the one calling
  `Engine.Run` or `Engine.Step`. Setup code may touch it before the engine
  starts.
- **Everything else reads frames.** The server never reads the `World`
  while the engine runs; it reads the `Hub`, which is built purely from
  frames.
- **Plugins mutate state only** inside command handlers, systems, `Init`
  and `OnCreate` hooks, all of which run on the simulation goroutine.

## Tick lifecycle

```mermaid
sequenceDiagram
  autonumber
  participant Q as Command queue (mutex)
  participant E as Engine.Step
  participant C as Command handlers
  participant P as Phases → systems
  participant O as Observation authority
  participant H as Hub / server
  E->>Q: take pending commands, nextTick++
  E->>E: sort: admin first, then player ID, then per-issuer seq
  loop each command
    E->>C: CommandDef.Execute(ctx, cmd)
    C-->>E: nil or CommandError → CommandResult
  end
  loop each phase in resolved order
    E->>P: SystemDef.Run(ctx) for each system in order
  end
  E->>O: Observations(ctx) (only if registered: fog of war)
  E->>E: takeChanges(): changed views + removed IDs
  E->>H: Frame{tick, changed, removed, events, results, commands, observations}
  H->>H: update copy, filter per viewer, fan out to streams
```

With every built-in plugin the resolved order is (`Ruleset.Schedule()`):

```
automation: automation.evaluate
energy:     energy.mine, energy.transfer
logistics:  logistics.supply
salvage:    salvage.collect
movement:   movement.step
combat:     combat.resolve
upgrades:   upgrades.progress
cloning:    cloning.progress
(then the visibility plugin's Observations)
```

Details in [simulation.md](simulation.md).

## Command lifecycle

```mermaid
sequenceDiagram
  autonumber
  participant Cl as Client
  participant S as gameService.SubmitCommand
  participant E as Engine
  participant H as Handler (plugin)
  participant St as Observe stream
  Cl->>S: SubmitCommandRequest{command_id, Any payload}
  S->>S: authenticate session → player
  S->>S: dedupe by command_id (per player, last 4096)
  S->>S: decode Any via protoreg → Go message, type = full name
  S->>E: Submit(Command{ID, Issuer, Type, Payload})
  E->>E: envelope check: known type? admin-only?
  E-->>S: scheduled tick (= next tick to execute)
  S-->>Cl: SubmitCommandResponse{accepted, scheduled_tick}
  Note over E: …at the scheduled tick…
  E->>H: Execute(ctx, cmd): validate all → Charge budget → mutate
  H-->>E: ok / CommandError(code)
  E->>St: Frame.Results → CommandResult (issuer only)
  St-->>Cl: TickUpdate.command_results[]
```

**Acceptance is not execution.** The acknowledgment only says the
envelope was valid and names the tick. World-state validation (ownership,
range, energy) happens at execution and is reported in the
`CommandResult`. See [commands.md](commands.md).

## Plugin execution pipeline

```mermaid
flowchart LR
  subgraph Startup[Startup: sim.NewRuleset]
    A[game.Plugins] --> B[validate names,<br/>config sections,<br/>dependencies]
    B --> C[topological sort<br/>DependsOn, ties by name]
    C --> D[Register each plugin<br/>Registrar: config, components,<br/>derived, phases, systems,<br/>commands, Init, OnCreate,<br/>Describe, ObservationAuthority]
    D --> E[order phases After/Before<br/>order systems After]
  end
  subgraph Init[game.New]
    E --> F[NewEngine] --> G[place config units<br/>OnCreate hooks fire] --> H[Engine.Init<br/>plugin Init funcs]
  end
  subgraph Tick[each tick]
    H --> I[commands] --> J[phases/systems] --> K[observation] --> L[Frame]
  end
```

See the [plugin guide](plugins/overview.md).

## Client synchronization

Each viewer receives a **snapshot**, then **one `TickUpdate` per tick**.
Updates contain full replacement entities for everything that changed,
removed IDs, events and command results, all filtered for that viewer.
Applying update *N* to snapshot *N−1* gives snapshot *N*, and the server
tests check exactly that. A stream that lags 128 frames gets a fresh
snapshot instead. See [events.md](events.md) and
[api/streaming.md](../api/streaming.md).

## Design decisions

The reasoning behind the main choices (proto-free kernel, typed generic
components, all-fail tile contention, full-entity updates, admin
mutations as scheduled commands, cumulative cost formulas, total integer
formula semantics, ...) is collected in
[design-notes.md](design-notes.md#key-decisions-and-tradeoffs).
