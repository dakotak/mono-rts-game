# Plugin interfaces

The complete API a plugin uses, all in package `sim`
([`internal/sim`](../../../internal/sim)). Signatures are copied from the
source.

## `sim.Plugin`

```go
type Plugin interface {
    Name() string               // unique; no "." or " "; "sim" is reserved
    DependsOn() []string        // names of plugins that must register first
    Register(r *Registrar) error
}
```

There are no optional interfaces to implement. Every capability is
declared through the `Registrar` inside `Register`.

## `sim.Registrar`

| Method | Purpose | Validation |
|---|---|---|
| `PluginName() string` | The plugin being registered. | — |
| `DecodeConfig(v any) error` | Strictly decode the plugin's config section into `v` (pre-filled with defaults). A missing section leaves `v` unchanged. | Unknown fields → error |
| `Component(c ComponentDef) error` | Register a component (build `c` with `Component[T].Def`). | ID prefixed `<plugin>.`; unique |
| `Derived(d DerivedDef) error` | Register a derived property. | Name prefixed; unique; `Compute` set |
| `Phase(p PhaseDef) error` | Declare a phase. | Unique name (phase names are global and not prefix-checked) |
| `System(s SystemDef) error` | Declare a system in a phase. | Name prefixed; unique; `Run` set |
| `Command(c CommandDef) error` | Register a command handler. | `Type` and `Execute` set; type unique |
| `Init(fn func(w *World) error)` | Run once at `Engine.Init`, in registration order. | — |
| `OnCreate(fn func(w *World, e Entity))` | Run for every created entity, in registration order. It must not create or remove entities. | — |
| `Describe(v any) error` | Publish a JSON rules description (`WorldInfo.rules_json[name]`). | Must marshal |
| `ObservationAuthority(a ObservationAuthority) error` | Become the single authority on what players observe. | Only one per ruleset |
| `Global(g GlobalDef) error` | Register a world-scoped value (`sim.NewGlobal[T](id, initial)`). | ID prefixed; unique |
| `Gate(g Gate) error` | Become the single authority on whether commands run (`AllowCommand`) and systems run (`RunSystems`) each tick. The match plugin uses it. | Only one per ruleset |

Phase and system references (`After`, `Before`, `SystemDef.Phase`) are
resolved after every plugin has registered, so you may reference phases
of plugins you depend on.

## Definitions

```go
type PhaseDef struct {
    Name   string
    After  []string // phases that must run before this one
    Before []string // phases that must run after this one
}

type SystemDef struct {
    Name  string // "<plugin>.<name>"
    Phase string
    After []string // systems in the same phase that must run first
    Run   func(ctx *TickContext)
}

type DerivedDef struct {
    Name    string // "<plugin>.<name>"
    Compute func(w *World, id EntityID) (int64, bool) // false: not defined for this entity
}

type CommandDef struct {
    Type        string // protobuf full name for network commands
    AdminOnly   bool
    Execute     func(ctx *TickContext, cmd Command) error
    ApplyToUnit func(ctx *TickContext, cmd Command, unit EntityID) error // optional; enables batches
}

type ObservationAuthority interface {
    Observations(ctx *TickContext) []Observation
}
```

## Components: `sim.Component[T]`

```go
func NewComponent[T comparable](id string) Component[T]
func (c Component[T]) ID() string
func (c Component[T]) Def(vis Visibility, attributes func(T) []Field) ComponentDef // attributes may be nil
func (c Component[T]) Registered(w *World) bool
func (c Component[T]) Get(w *World, id EntityID) (T, bool)
func (c Component[T]) Has(w *World, id EntityID) bool
func (c Component[T]) Set(w *World, id EntityID, v T)       // marks dirty only if the value changed
func (c Component[T]) Remove(w *World, id EntityID)
func (c Component[T]) Entities(w *World) []EntityID          // sorted; do not modify
```

`Visibility` is one of `sim.VisibleAdmin`, `sim.VisibleOwner` and
`sim.VisiblePublic`.

## `*sim.TickContext`

Passed to systems and command handlers.

```go
ctx.World    *World
ctx.Tick     Tick
ctx.Emit(e Event)
ctx.Charge(issuer Principal, n int) error  // per-tick unit budget; fails with resource_exhausted, consuming nothing
ctx.BudgetLeft(issuer Principal) int       // -1 = unlimited
ctx.ReportUnit(unit EntityID, err error)   // per-unit result for the command being executed
ctx.ApplyToUnit(cmd Command, unit EntityID) error // run another command's ApplyToUnit (used by batches)
ctx.Events() []Event                       // events emitted so far this tick (read-only); later phases can tally them
```

## Globals: `sim.Global[T]`

```go
func NewGlobal[T comparable](id string, initial T) Global[T]
func (g Global[T]) Def() GlobalDef
func (g Global[T]) Get(w *World) T
func (g Global[T]) Set(w *World, v T)   // reported in Frame.Globals when changed
func (w *World) Globals() map[string]any
```

## Composition helpers in `internal/game`

| API | Use |
|---|---|
| `game.Plugins()` | The server's plugin set (edit to register plugins) |
| `game.NewWithPlugins(cfg, plugins)` | Build a game from any plugin set: `game.New` with extra plugins (tests, tools) |
| `game.Find[T](plugins)`, `game.Plugin[T](g)` | Find a plugin instance by type, e.g. the shared `*match.Plugin` |
| `game.PlayerAware` (`SetPlayers([]sim.PlayerID)`) | Implement it to receive the match's players before `Init` (worldgen, match) |
| `game.RulesetHash(rules)`, `game.EngineVersion` | Ruleset fingerprint and engine version (results, replays) |

## `*sim.World`

| Method | Notes |
|---|---|
| `Width()`, `Height()`, `InBounds(p)` | Grid. |
| `Tick()` | Current tick. |
| `Rand() *Rand` | The only randomness source (`Uint64()`, `Intn(n)`). |
| `Entity(id) (Entity, bool)` | Core fields. |
| `EntityAt(p) (EntityID, bool)` | Blocking entity on a tile. |
| `PassableAt(p) []EntityID` | Passable entities on a tile, sorted; don't modify. |
| `EntityIDs() []EntityID`, `EntityCount()` | All entities, sorted. |
| `CreateEntity(kind, owner, pos)`, `CreatePassable(...)` | Create (OnCreate hooks fire). |
| `RemoveEntity(id)` | Remove with all components. |
| `MoveEntities([]Move)` | Atomic, simultaneous batch move. |
| `Derived(name, id) (int64, bool)` | Evaluate a derived property. |
| `View(id)`, `ScratchView(id)`, `FullView()` | Observable views (unfiltered). |
| `Changed(id) bool` | Created or modified so far this tick. |
| `StateHash() uint64` | Determinism checks. |

## Values, events, errors

```go
sim.IntValue(int64), sim.BoolValue(bool), sim.StringValue(string), sim.PosValue(sim.Pos), sim.RangeValue(lo, hi)
sim.Field{Key, Value}
sim.Event{Type, Entity, Audience, Fields}
sim.ToPlayer(p), sim.ToAll(), sim.AdminAudience()
sim.Fail(code, format, args...) error            // *sim.CommandError
sim.Unavailable(what string, id EntityID) error // not_found, uniform message
sim.CodeInvalidArgument, CodeNotFound, CodePermissionDenied, CodeFailedPrecondition, CodeInternal, CodeResourceExhausted
```

## Exported APIs of built-in plugins

Plugins compose by calling each other's exported functions (declare the
dependency in `DependsOn`):

| Plugin | Notable exports |
|---|---|
| `energy` | `GetStore`, `Stored`, `Spend`, `SetStored`, `SetMining`, `StopMining`, `MiningWell`, `SetTransfer`, `Transferring`, `DerivedTransferRate`, `DerivedExtractionRate`, `MaxUnitsPerCommand`, `CodeInsufficientEnergy` |
| `movement` | `SetDestination`, `Destination`, `ClearDestination`, `FreeTileNear`, `Phase` |
| `attributes` | `(*Plugin).Has`, `Lookup`, `OnChange`, `Apply`, `Reserve`, `Release`, `Busy`, `UpgradeCost`, `Embodied`, ... |
| `combat` | `GetHealth`, `SetTarget`, `ClearTarget`, `Target`, `(*Plugin).Capability`, `(*Plugin).OnDestroyed`, `Distance` |
| `salvage` | `GetWreck`, `Wrecks`, `SetSalvage`, `StopSalvage`, `SalvageTarget` |
| `wells` | `Get`, `Set`, `IDs` |
| `terrain` | `StepCost`, `IsTerrain` |
| `metadata` | `Get`, `(*Plugin).Select` |
| `visibility` | `(*Plugin).Detected`, `Enabled`, `Disclose` |
| `match` | `Get(w)` (status), `(*Plugin).AddCondition`, `AddMetric`, `NewMetric`, `Config`, `ConditionNames`; types `Condition`, `Metric`, `View`, `Outcome`, `Status` |
| `worldgen` | `(*Plugin).AddGenerator`, `LastPlan`; `Generator`, `Context`, `Plan`, `PlaceHomeWells`, `SpawnsAtAnchors` |
| `terrain` / `wells` (generation) | `(*terrain.Plugin).Place`, `Type`; `wells.Create`, `(*wells.Plugin).Defaults` |

### Gaps a plugin author may hit

- **Health can't be changed by another plugin.** `combat` exports
  `GetHealth` but no setter. Healing or regenerating health would need a
  new exported function in `combat`, an engine change. The example plugin
  regenerates energy for this reason.
- **No "on removed" hook.** Plugins notice removals by re-checking
  references. `combat.(*Plugin).OnDestroyed` covers combat destruction
  only; salvage uses it to create wrecks.
- **No per-plugin RPCs.** Expose functionality through commands,
  attributes, events and `Describe`.
- **One observation authority.** A second plugin can't add its own fog
  filtering.
