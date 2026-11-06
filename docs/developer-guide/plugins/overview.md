# Plugin development: overview

Every game mechanic in Mono (movement, energy, combat, fog of war,
automation and the rest) is a **plugin**. This guide explains what a
plugin is and which extension points exist, then walks you through
building, registering, testing and shipping one.

| Page | Contents |
|---|---|
| [Tutorial: build a plugin](getting-started.md) | Step by step, from an empty package to a verified running server |
| [Interfaces](interfaces.md) | `sim.Plugin`, `sim.Registrar`, every `*Def`, `TickContext`, `World`, `Component` |
| [Registration](registration.md) | `game.Plugins()`, `sim.NewRuleset`, ordering, validation, failures |
| [Lifecycle](lifecycle.md) | Construction → registration → init → ticks → shutdown |
| [Commands and events](commands-and-events.md) | Adding commands and orders, authorization, budgets, events, information hiding |
| [Attributes and formulas](attributes-and-formulas.md) | Components as attributes, derived properties, binding to upgradable attributes, formulas |
| [State and determinism](state-and-determinism.md) | The rules every plugin must follow |
| [Testing](testing.md) | Unit, integration, determinism and benchmark tests |
| [Examples](examples.md) | The example plugin and built-in plugins worth reading |

## What a plugin is

A plugin is a **Go package compiled into the server** that implements
[`sim.Plugin`](../../../internal/sim/plugin.go):

```go
type Plugin interface {
    Name() string               // unique lowercase name, e.g. "regeneration"
    DependsOn() []string        // plugins that must register first
    Register(r *Registrar) error
}
```

`Register` is called **once at startup**. Through the `Registrar` the
plugin declares everything it contributes:

- its configuration
- components (per-entity state, optionally visible as attributes)
- derived properties
- phases and systems (per-tick logic)
- commands (and per-unit command application)
- init and entity-creation hooks
- a public rules description
- optionally, the observation authority

After registration the plugin never talks to the kernel except through
the callbacks it registered, which the kernel calls on the simulation
goroutine.

## Kernel vs. plugins

| The kernel ([`internal/sim`](../../../internal/sim)) does | Plugins do |
|---|---|
| Tick loop, clock, pause and step | Define what happens each tick (systems) |
| Command queue, deterministic ordering, budgets, results | Define and validate commands, mutate state |
| Entity identity (opaque IDs), positions, occupancy, removal | Give entities meaning through components |
| Component storage, change tracking, `StateHash` | Choose component types and their visibility |
| Phase and system ordering from declared constraints | Declare phases, systems and ordering constraints |
| Seeded RNG (`World.Rand`) | Use it for any randomness |
| Frames: changed views, events, results, commands | Emit events with audiences |
| Observation hook (one authority) | The visibility plugin decides what players observe |

The kernel contains **no game rules**. Even "units have energy" is a
plugin (energy attaches a store to every new unit in an `OnCreate` hook).

## Supported extension points

These `sim` APIs are what the built-in plugins use. They're the supported
way to extend the game:

| Extension point | API |
|---|---|
| Configuration section | `Registrar.DecodeConfig` |
| Per-entity state, optionally visible to admin, owner or public | `sim.NewComponent[T]`, `Registrar.Component` |
| Derived (computed) per-entity values | `Registrar.Derived`, `World.Derived` |
| Per-tick logic, ordered | `Registrar.Phase`, `Registrar.System` |
| Commands (player or admin-only), batch-capable | `Registrar.Command` with `CommandDef{Execute, ApplyToUnit, AdminOnly}` |
| Initial world content | `Registrar.Init` |
| Default components on every new entity | `Registrar.OnCreate` |
| Public rules and cost tables | `Registrar.Describe` |
| Events to players and admins | `TickContext.Emit` |
| Command budget and per-unit results | `TickContext.Charge`, `ReportUnit`, `ApplyToUnit` |
| Using other mechanics | Their exported Go APIs (e.g. `energy.GetStore`, `energy.SetStored`, `movement.Destination`, `attributes.Plugin.Lookup`), declared with `DependsOn` |
| Fog of war (one plugin only) | `Registrar.ObservationAuthority` (used by `visibility`) |

### Internal details, not extension points

- `internal/server`, `internal/gateway` and the `Hub`. Plugins can't add
  RPCs or change filtering; they reach clients only through attributes,
  events, command results and `Describe`.
- Unexported kernel internals (component maps, `World.tick`, `takeChanges`).
- Another plugin's unexported state. Use its exported functions, and only
  from inside a tick.

### Stability

The plugin API is **not versioned or frozen**. Everything lives under
`internal/`, and Go's `internal` rule means only packages **inside this
module** can import it. The API is stable in practice (every milestone
added to it without breaking built-in plugins) but carries no
compatibility promise. Plan on keeping plugins in this repository, or in a
fork, and updating them with the engine.

## Compatibility and limitations

| Question | Answer |
|---|---|
| Go version | Same as the server: Go **1.26.2+** (`go.mod`). Plugins use generics (`sim.NewComponent[T]`). |
| Module and package requirements | The plugin must be a package **inside this module** (for example `internal/plugins/<name>` or `examples/plugins/<name>`), because the plugin API is under `internal/`. A plugin in another Go module can't import it. |
| Recompilation | **Required.** Adding, removing or changing a plugin means rebuilding `rts-server`, and `rts-gateway` too if the plugin adds command protos. |
| Runtime loading | **No.** No `plugin.Open`, no scripting, no dynamic registration. The plugin set is fixed by `game.Plugins()` at build time. |
| Enable or disable per world | **Not by the engine.** Every compiled-in plugin is registered in every world. A plugin can offer its own switch, as `visibility.enabled` and the example's `enabled` do. |
| New entity kinds | **No.** `sim.Kind` is a closed set (unit, resource, terrain). Add meaning with components instead: a well is a resource with a `wells.well` component. |
| New gRPC services or RPCs | **No.** Plugins extend the API only through new command messages (`google.protobuf.Any` payloads), attributes, events and rules descriptions. |
| New per-player filtering | Only by being the single observation authority, which the visibility plugin already is. |
| Changing a built-in rule | Edit that plugin, or replace it in `game.Plugins()`. Other plugins call built-ins' Go APIs, so a replacement must keep the APIs its dependents use. |
| Replay compatibility | Adding a plugin changes `StateHash` (the component list is hashed) and possibly outcomes. Replays only work with the same build and configuration. |

### Security

Plugins are **ordinary Go code running in the server process**, with the
same privileges as the server. **They are not sandboxed.** A plugin can:

- read the whole world, ignoring fog of war
- leak hidden information through attributes or events
- crash the process (a panic in a system takes the server down)
- stall every tick, or access the filesystem and network

Review third-party plugin code like any server code. The determinism and
information-hiding rules in this guide are conventions the code must
follow; the compiler doesn't enforce them.

The repository is licensed under the **GNU AGPL v3**. Distributing or
publicly hosting a server that includes your plugin has license
implications. Read the [LICENSE](../../../LICENSE); this guide isn't legal
advice.

## Where to start

Do the [tutorial](getting-started.md). It builds the example
regeneration plugin in
[`examples/plugins/regeneration`](../../../examples/plugins/regeneration),
which compiles and is tested in this repository, and shows the exact
changes that register it in the server.
