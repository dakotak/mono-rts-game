# Tutorial: build, register and run a plugin

This tutorial builds **`regeneration`**: idle units slowly regenerate
energy, up to a share of their capacity, and players can switch it off
per unit. The finished plugin is in the repository at
[`examples/plugins/regeneration`](../../../examples/plugins/regeneration/regeneration.go),
with its command proto at
[`proto/rts/examples/regeneration/v1`](../../../proto/rts/examples/regeneration/v1/regeneration.proto)
and tests in
[`regeneration_test.go`](../../../examples/plugins/regeneration/regeneration_test.go).
It compiles and its tests run with `make test`. It is **not registered**
in the default server; step 12 shows exactly how to register it.

The plugin touches every extension point you're likely to need:

| Feature | Extension point |
|---|---|
| Configuration with defaults and validation | `DecodeConfig` |
| An owner-visible component | `Component` |
| A derived property bound to an optional upgradable attribute | `Derived`, attributes plugin |
| A phase ordered between two built-in phases, and a system | `Phase`, `System` |
| A player command, usable in batches | `Command`, `ApplyToUnit` |
| An owner-only event | `Emit` |
| Other plugins' state | `energy`, `movement` Go APIs |
| A rules description | `Describe` |

Everything below runs from the repository root with Go 1.26.2+.

## 1. Create the package

Plugins must live **inside this module**, because the plugin API is under
`internal/`. Built-in plugins live in `internal/plugins/<name>`; this
example lives in `examples/plugins/regeneration` to keep it apart from
the shipped game.

```bash
mkdir -p examples/plugins/regeneration
```

Start the file with the package, imports and identifiers. Every name the
plugin registers is prefixed with the plugin name, and the kernel enforces
the prefix:

```go
package regeneration

import (
    "fmt"

    regenerationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/examples/regeneration/v1"
    "github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
    "github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
    "github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
    "github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const Name = "regeneration"

const (
    Phase          = "regeneration"
    AttrDisabled   = "regeneration.disabled"    // owner-visible, present only when switched off
    DerivedAmount  = "regeneration.amount"      // energy per regeneration step
    EventRegen     = "regeneration.regenerated" // owner; fields amount, stored
    MaxAmount      = 1000
    MaxIntervalTks = 1000
)
```

## 2. Define the command message (protobuf)

Player commands arrive over gRPC as `google.protobuf.Any`, so a command
needs a protobuf message. Its **full name is the command type**. Put it
in `proto/` and generate code:

```proto
// proto/rts/examples/regeneration/v1/regeneration.proto
syntax = "proto3";

package rts.examples.regeneration.v1;

option go_package = "github.com/dakotaknutson/unit-strategy-game/gen/rts/examples/regeneration/v1;regenerationv1";

message SetRegenerationCommand {
  repeated uint64 unit_ids = 1;
  bool enabled = 2;
}
```

```bash
make generate          # buf lint + buf generate → gen/rts/examples/regeneration/v1/regeneration.pb.go
```

Built-in plugins use `proto/rts/<plugin>/v1`. Follow the repository's
proto rules: `buf lint` passes, and fields are only ever added.

In Go, the command type string comes from the descriptor, so it can't
drift from the proto:

```go
var CommandSet = string((&regenerationv1.SetRegenerationCommand{}).ProtoReflect().Descriptor().FullName())
// = "rts.examples.regeneration.v1.SetRegenerationCommand"
```

## 3. Implement `sim.Plugin`: identity and dependencies

```go
type Plugin struct {
    cfg   Config
    attrs *attributes.Plugin
}

// New receives the shared attributes plugin instance (explicit composition).
func New(attrs *attributes.Plugin) *Plugin { return &Plugin{attrs: attrs} }

func (*Plugin) Name() string { return Name }

func (*Plugin) DependsOn() []string {
    return []string{energy.Name, movement.Name, attributes.Name}
}
```

`DependsOn` does two jobs:

- It makes those plugins **register first**, so their components and
  phases exist when you reference them.
- It documents that you call their Go APIs. A missing dependency fails
  startup with `plugin "regeneration" depends on missing plugin "energy"`.

The plugin struct holds **configuration only**. Anything that changes
during the game goes in components (step 5).

## 4. Configuration

Define a typed config with JSON tags and defaults, decode it **over the
defaults**, then validate. `DecodeConfig` rejects unknown fields:

```go
type Config struct {
    Enabled         bool   `json:"enabled"`          // off by default
    Amount          int64  `json:"amount"`           // energy per step, 0..1000
    AmountAttribute string `json:"amount_attribute,omitempty"` // optional upgradable source
    Interval        int64  `json:"interval"`         // ticks between steps, 1..1000
    LimitPercent    int64  `json:"limit_percent"`    // cap, share of capacity, 1..100
    IdleOnly        bool   `json:"idle_only"`        // no regeneration while moving
}

func DefaultConfig() Config {
    return Config{Amount: 1, Interval: 10, LimitPercent: 50, IdleOnly: true}
}

func (p *Plugin) validate() error {
    c := p.cfg
    switch {
    case c.Amount < 0 || c.Amount > MaxAmount:
        return fmt.Errorf("amount must be in 0..%d, got %d", MaxAmount, c.Amount)
    case c.Interval < 1 || c.Interval > MaxIntervalTks:
        return fmt.Errorf("interval must be in 1..%d, got %d", MaxIntervalTks, c.Interval)
    case c.LimitPercent < 1 || c.LimitPercent > 100:
        return fmt.Errorf("limit_percent must be in 1..100, got %d", c.LimitPercent)
    case c.AmountAttribute != "" && !p.attrs.Has(c.AmountAttribute):
        return fmt.Errorf("amount_attribute: %q is not an attribute or derived property", c.AmountAttribute)
    }
    return nil
}
```

Making the mechanic **off by default** means registering the plugin
changes nothing for existing configs. It's a good habit for any new
mechanic.

## 5. Components (per-entity state)

State that persists across ticks lives in components. A component type
must be `comparable` and should be a plain value (no pointers, slices or
maps). This one marks units whose owner switched regeneration off:

```go
type optOut struct{ Since sim.Tick }

var optOuts = sim.NewComponent[optOut]("regeneration.opt_out")
```

## 6. `Register`: declare everything

```go
func (p *Plugin) Register(r *sim.Registrar) error {
    p.cfg = DefaultConfig()
    if err := r.DecodeConfig(&p.cfg); err != nil { // strict: unknown fields fail
        return err
    }
    if err := p.validate(); err != nil {
        return fmt.Errorf("regeneration: %w", err)
    }
    // Component, shown to the owner as the attribute "regeneration.disabled".
    if err := r.Component(optOuts.Def(sim.VisibleOwner, func(optOut) []sim.Field {
        return []sim.Field{{Key: AttrDisabled, Value: sim.BoolValue(true)}}
    })); err != nil {
        return err
    }
    // Derived property: computed on demand, never stored.
    if err := r.Derived(sim.DerivedDef{Name: DerivedAmount, Compute: p.amount}); err != nil {
        return err
    }
    // Phase after mining/transfers, before movement; one system in it.
    if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{energy.Phase}, Before: []string{movement.Phase}}); err != nil {
        return err
    }
    if err := r.System(sim.SystemDef{Name: "regeneration.step", Phase: Phase, Run: p.step}); err != nil {
        return err
    }
    // Command, also applicable per unit (batch templates).
    if err := r.Command(sim.CommandDef{Type: CommandSet, Execute: p.execSet, ApplyToUnit: p.applySet}); err != nil {
        return err
    }
    return r.Describe(p.cfg) // public: WorldInfo.rules_json["regeneration"]
}
```

The attribute function passed to `Def` must be a **pure function of the
value**, because the kernel memoizes it. With `sim.VisibleOwner`, only the
owner and admins see the attribute. Use `sim.VisiblePublic` only for
things every player may know, and under fog of war foreign entities'
attributes also pass through disclosure rules.

The derived property reads the optional attribute binding:

```go
func (p *Plugin) amount(w *sim.World, id sim.EntityID) (int64, bool) {
    if _, ok := energy.GetStore(w, id); !ok {
        return 0, false // not a unit with a store
    }
    if p.cfg.AmountAttribute != "" {
        v, ok := p.attrs.Lookup(w, id, p.cfg.AmountAttribute)
        return min(max(v, 0), MaxAmount), ok
    }
    return p.cfg.Amount, true
}
```

## 7. Deterministic simulation behavior (the system)

Systems run once per tick on the simulation goroutine, in phase order:

```go
func (p *Plugin) step(ctx *sim.TickContext) {
    if !p.cfg.Enabled {
        return
    }
    w := ctx.World
    interval := uint64(p.cfg.Interval)
    for _, id := range w.EntityIDs() { // sorted → deterministic order
        if (uint64(ctx.Tick)+uint64(id))%interval != 0 {
            continue // staggered: each unit every `interval` ticks
        }
        e, _ := w.Entity(id)
        if e.Kind != sim.KindUnit || optOuts.Has(w, id) {
            continue
        }
        if _, moving := movement.Destination(w, id); moving && p.cfg.IdleOnly {
            continue
        }
        s, ok := energy.GetStore(w, id)
        if !ok {
            continue
        }
        amt, _ := w.Derived(DerivedAmount, id)
        limit := s.Capacity * p.cfg.LimitPercent / 100 // integer math only
        gain := min(amt, limit-s.Energy)
        if gain <= 0 {
            continue
        }
        energy.SetStored(w, id, s.Energy+gain)
        ctx.Emit(sim.Event{Type: EventRegen, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
            {Key: "amount", Value: sim.IntValue(gain)},
            {Key: "stored", Value: sim.IntValue(s.Energy + gain)},
        }})
    }
}
```

The determinism rules this follows (see
[state-and-determinism.md](state-and-determinism.md)):

- **Sorted iteration** (`World.EntityIDs`), never a Go map.
- **No wall clock, no `math/rand`.** The schedule is a pure function of
  tick and ID.
- **Integer math** with explicit rounding (`* LimitPercent / 100` floors).
- **Bounded work:** one pass over entities per tick, O(1) per unit.
- **State goes through supported APIs.** `energy.SetStored` (clamped to
  capacity) for energy, a component for the opt-out. Nothing lives in
  plugin fields.

## 8. Events

`ctx.Emit` with `sim.ToPlayer(owner)` sends the event to the owner and
admins only. Under fog of war a `sim.ToAll()` event reaches **every**
player regardless of detection, so never use it for information about
specific units. Events are notifications; anything a reconnecting client
needs must also be in attributes.

## 9. Commands: validation, authorization, budget

```go
func check(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
    e, ok := w.Entity(id)
    if !ok || e.Kind != sim.KindUnit || (!issuer.Admin && e.Owner != issuer.Player) {
        return sim.Unavailable("unit", id) // same error for missing, foreign and hidden units
    }
    return nil
}

func (p *Plugin) execSet(ctx *sim.TickContext, cmd sim.Command) error {
    m, ok := cmd.Payload.(*regenerationv1.SetRegenerationCommand)
    if !ok {
        return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetRegenerationCommand", cmd.Payload)
    }
    ids := m.GetUnitIds()
    if len(ids) == 0 || len(ids) > energy.MaxUnitsPerCommand {
        return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", energy.MaxUnitsPerCommand, len(ids))
    }
    seen := make(map[uint64]bool, len(ids))
    for _, id := range ids { // 1. validate everything
        if seen[id] {
            return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
        }
        seen[id] = true
        if err := check(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
            return err
        }
    }
    if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil { // 2. charge the budget
        return err
    }
    for _, id := range ids { // 3. only now mutate
        set(ctx.World, sim.EntityID(id), m.GetEnabled())
    }
    return nil
}
```

The order **validate → charge → mutate** makes the command atomic: if it
fails, nothing changed. `applySet` does the same for a single unit
(charging 1), which makes the command usable as a template in
`rts.metadata.v1.BatchCommand` over metadata selectors.

## 10. Initialization and cleanup

This plugin needs neither, but both hooks exist:

- `r.Init(func(w *sim.World) error)` runs once after config units are
  placed, before tick 1. Wells and terrain place their entities here.
- `r.OnCreate(func(w *sim.World, e sim.Entity))` runs for every new entity.
  Energy attaches stores here.

There are **no shutdown or removal hooks**:

- When an entity is removed, the kernel drops all of its components,
  including yours.
- Code holding a reference to another entity (a target, a receiver)
  must re-check it each time with `World.Entity`.
- When the process exits, nothing is persisted.

## 11. Tests

The example ships tests for each concern
([`regeneration_test.go`](../../../examples/plugins/regeneration/regeneration_test.go)):

| Test | Checks |
|---|---|
| `TestRegistration` | The schedule contains the system, the rules are described, the command is registered |
| `TestConfigValidation` | Unknown field, bad ranges and unknown attribute are each rejected |
| `TestMissingDependencyFailsRegistration` | Registration fails without `energy` |
| `TestDisabledByDefault`, `TestIdleUnitsRegenerateUpToLimit`, `TestMovingUnitsDoNotRegenerate` | Tick behavior |
| `TestSetRegenerationCommand` | Ownership (`not_found` for a foreign unit), state change, owner-visible attribute |
| `TestCommandBudgetIsAtomic` | Over budget: `resource_exhausted` and no change |
| `TestDeterministicAndReplayable` | Identical runs and replay produce identical state hashes |
| `TestWorksWithAllBuiltinPlugins` | The real plugin set plus this one, with `amount_attribute` bound to an attribute |
| `BenchmarkStep10k` | Tick cost with 10,000 units |

Run them on their own, or as part of the whole suite:

```bash
GOTOOLCHAIN=local go test ./examples/plugins/regeneration -v
GOTOOLCHAIN=local go test -run '^$' -bench . -benchmem ./examples/plugins/regeneration
make test
```

[testing.md](testing.md) explains the patterns.

## 12. Register the plugin with the server

Plugins are compiled in. The server registers exactly the plugins
returned by `game.Plugins()` in
[`internal/game/game.go`](../../../internal/game/game.go). There's no
other registry, flag or config switch. Two edits are needed.

**a) Add it to `game.Plugins()`**, passing the shared attributes
instance:

```diff
 import (
 	...
+	"github.com/dakotaknutson/unit-strategy-game/examples/plugins/regeneration"
 	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
 	...
 )

 func Plugins() []sim.Plugin {
 	attrs := attributes.New()
 	...
 	return []sim.Plugin{
 		...
 		logistics.New(attrs),
+		regeneration.New(attrs),
 		vis,
 	}
 }
```

The position in the slice doesn't matter: registration order comes from
`DependsOn`, with ties broken by name.

**b) List its proto file in [`internal/protoreg`](../../../internal/protoreg/protoreg.go)**
so the server can decode the `Any` payload and the gateway can translate
it to and from JSON:

```diff
 import (
 	...
 	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
+	regenerationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/examples/regeneration/v1"
 	...
 )

 var Files = []protoreflect.FileDescriptor{
 	...
 	logisticsv1.File_rts_logistics_v1_logistics_proto,
+	regenerationv1.File_rts_examples_regeneration_v1_regeneration_proto,
 	...
 }
```

If you forget (b), `TestEveryCommandTypeIsRegisteredForGateways` in
`internal/game` fails, and so do players' commands.

## 13. Enable and configure it

Add a section to your world config under `plugins`. For example, copy
`configs/dev.json` to `configs/regen.json` and add:

```json
"regeneration": { "enabled": true, "amount": 2, "interval": 10, "limit_percent": 80 }
```

To make it upgradable, define an attribute (say `solar`) in the
`attributes` section and set `"amount_attribute": "solar"`.

## 14. Build and run

```bash
make test                                  # the protoreg guard and everything else
GOTOOLCHAIN=local go build -o bin/ ./cmd/rts-server ./cmd/rts-gateway
bin/rts-server -config configs/regen.json &
bin/rts-gateway -web web/static &
```

The startup log shows the plugin and its place in the schedule:

```
world 32x20, 54 entities, 10 Hz, plugins [attributes metadata terrain visibility wells energy logistics movement regeneration upgrades cloning combat salvage automation]
tick schedule: automation: automation.evaluate; energy: energy.mine, energy.transfer; logistics: logistics.supply; regeneration: regeneration.step; salvage: salvage.collect; movement: movement.step; combat: combat.resolve; upgrades: upgrades.progress; cloning: cloning.progress
```

Rebuild **both** binaries: the gateway embeds the proto registry.

## 15. Verify through the public API

The player gateway speaks protojson over HTTP:

```bash
# 1. Connect: the plugin, its command type and its rules are published in WorldInfo.
TOKEN=$(curl -s -X POST localhost:8080/api/connect -d '{"joinToken":"red"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["sessionToken"])')

# 2. Pick one of your units and watch its energy rise (2 every 10 ticks while idle).
curl -s localhost:8080/api/snapshot -H "Authorization: Bearer $TOKEN"

# 3. Switch regeneration off for that unit.
curl -s -X POST localhost:8080/api/command -H "Authorization: Bearer $TOKEN" -d '{
  "commandId": "regen-off-1",
  "payload": {
    "@type": "type.googleapis.com/rts.examples.regeneration.v1.SetRegenerationCommand",
    "unitIds": ["<unit id>"],
    "enabled": false
  }}'

# 4. Events arrive on the stream.
curl -sN localhost:8080/api/observe -H "Authorization: Bearer $TOKEN"
```

What the verification run of these steps showed, condensed:

```
WorldInfo: plugins include "regeneration"; command_types include
           rts.examples.regeneration.v1.SetRegenerationCommand;
           rules_json.regeneration = {"enabled":true,"amount":2,"interval":10,"limit_percent":80,"idle_only":true}
tick 83   energy.stored 46   regeneration.disabled absent
tick 105  energy.stored 50   regeneration.disabled absent
{"commandId":"regen-off-1","accepted":true,"scheduledTick":"106","rejectionReason":""}
tick 111  energy.stored 50   regeneration.disabled {"boolValue":true}
tick 133  energy.stored 50   regeneration.disabled {"boolValue":true}
event (another idle unit): regeneration.regenerated {"amount":{"intValue":"2"},"stored":{"intValue":"58"}}
```

A gRPC client does the same with `SubmitCommand` and an `Any` payload. See
the [bot guide](../../api/bot-development.md).

## Undo

To ship the default game again, revert the two edits from step 12. The
example package itself stays compiled and tested either way.

## Next steps

- [Interfaces](interfaces.md) and [registration](registration.md) cover
  the full API surface and failure modes.
- [Commands and events](commands-and-events.md) covers orders,
  `ReportUnit`, batch behavior and information hiding.
- [Examples](examples.md) lists built-in plugins worth reading, from
  simplest (`wells`, `terrain`, `metadata`) to the most involved
  (`movement`, `visibility`).
