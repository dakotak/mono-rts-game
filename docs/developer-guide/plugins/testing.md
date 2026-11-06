# Testing plugins

All examples are from
[`examples/plugins/regeneration/regeneration_test.go`](../../../examples/plugins/regeneration/regeneration_test.go),
which runs with `make test`.

## Running tests

```bash
GOTOOLCHAIN=local go test ./examples/plugins/regeneration          # one plugin
GOTOOLCHAIN=local go test ./examples/plugins/regeneration -run TestConfig -v
GOTOOLCHAIN=local go test -race ./examples/plugins/regeneration
make test                                                         # everything, plugin included
```

A plugin inside `internal/plugins/<name>` is tested the same way. A
plugin you register in `game.Plugins()` is also exercised by the game,
server and scale suites. Run `make test` after registering.

## Two ways to build a world in tests

**Minimal composition.** Only your plugin and its dependencies, built
with `sim.NewRuleset` and `sim.NewEngine` directly. This is fast and
isolates your logic:

```go
func minimal(cfg map[string]string) ([]sim.Plugin, map[string]json.RawMessage) {
    attrs := attributes.New()
    plugins := []sim.Plugin{
        wells.New(), attrs, energy.NewWithAttributes(attrs), movement.NewWithAttributes(attrs),
        regeneration.New(attrs),
    }
    raw := map[string]json.RawMessage{"energy": json.RawMessage(`{"unit_capacity": 100, "initial_energy": 0}`)}
    for k, v := range cfg {
        raw[k] = json.RawMessage(v)
    }
    return plugins, raw
}

func engine(t testing.TB, plugins []sim.Plugin, cfg map[string]json.RawMessage) *sim.Engine {
    rules, err := sim.NewRuleset(plugins, cfg)
    if err != nil { t.Fatal(err) }
    e, err := sim.NewEngine(sim.Config{Width: 20, Height: 20, Seed: 1, IDKey: []byte("0123456789abcdef")}, rules)
    if err != nil { t.Fatal(err) }
    if err := e.Init(); err != nil { t.Fatal(err) }
    return e
}
```

Create units with `e.World().CreateEntity(sim.KindUnit, owner, pos)` (any
non-zero `sim.PlayerID` works), submit with `e.Submit(sim.Command{...})`,
and advance with `e.Step()`.

**Full composition.** The real plugin set plus yours, exactly as
registering it in `game.Plugins()` would wire it:

```go
func fullComposition() []sim.Plugin {
    plugins := game.Plugins()
    for _, p := range plugins {
        if a, ok := p.(*attributes.Plugin); ok {
            return append(plugins, regeneration.New(a)) // share the attributes instance
        }
    }
    panic("no attributes plugin")
}
```

For built-in plugins (already in `game.Plugins()`), use
[`internal/plugintest`](../../../internal/plugintest/plugintest.go):
`pt.New(t, map[string]string{...})` gives a ready world with two players
and `Unit`, `Submit`, `Step`, `Energy`, `Prop`, `Events` and `Result`
helpers. See [developer testing](../testing.md#the-plugin-test-harness).

## What to test

### Initialization and registration

```go
func TestRegistration(t *testing.T) {
    plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true}`})
    rules, err := sim.NewRuleset(plugins, cfg)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(strings.Join(rules.Schedule(), ";"), "regeneration: regeneration.step") {
        t.Fatalf("schedule = %v", rules.Schedule())
    }
    // also: rules.Rules()["regeneration"] non-empty, command in rules.CommandTypes(false)
}
```

### Registration failures

```go
func TestMissingDependencyFailsRegistration(t *testing.T) {
    attrs := attributes.New()
    _, err := sim.NewRuleset([]sim.Plugin{attrs, regeneration.New(attrs)}, nil)
    if err == nil || !strings.Contains(err.Error(), `depends on missing plugin "energy"`) {
        t.Fatalf("err = %v", err)
    }
}
```

### Configuration validation

A table of bad sections, each of which must fail `NewRuleset`:

```go
for name, c := range map[string]string{
    "unknown field":     `{"enabled": true, "amout": 2}`,
    "zero interval":     `{"interval": 0}`,
    "unknown attribute": `{"amount_attribute": "solar_panels"}`,
} {
    plugins, cfg := minimal(map[string]string{"regeneration": c})
    if _, err := sim.NewRuleset(plugins, cfg); err == nil {
        t.Errorf("%s: accepted %s", name, c)
    }
}
```

### Command execution

Check the success path, authorization (`not_found` for a foreign unit),
atomicity under the budget, and the resulting state and attributes:

```go
submit(t, e, sim.Principal{Player: red}, "steal",
    &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(theirs)}}, regeneration.CommandSet)
f := e.Step()
if r := result(f, "steal"); r.OK || r.Code != sim.CodeNotFound { t.Fatalf("%+v", r) }
```

Build the engine with `sim.Config{UnitCommandBudget: 1}` to check that an
over-budget command fails with `resource_exhausted` and changes nothing.

### Tick behavior

Step a known number of ticks and assert exact values. Integer rules make
exact assertions possible:

```go
// amount 3 every 5 ticks, capped at 40% of capacity 100
for range 10 { e.Step() }      // exactly two steps → 6
for range 200 { e.Step() }     // capped → 40
```

Also check the events you emit: type, entity, audience and fields.

### Deterministic outcomes and replay

```go
func TestDeterministicAndReplayable(t *testing.T) {
    a, log := run(t, nil) // records f.Commands each tick
    b, _ := run(t, nil)
    c, _ := run(t, log)   // replays with Engine.StepWith
    for i := range a {
        if a[i] != b[i] || a[i] != c[i] { t.Fatalf("tick %d differs", i+1) }
    }
}
```

`run` builds a fresh world, submits a scripted command at a fixed tick,
and returns `World.StateHash()` per tick. Every tick-running plugin
should have one.

### Plugin interactions

Run with the full composition and real configs for the mechanics you
touch. `TestWorksWithAllBuiltinPlugins` binds `amount_attribute` to a
config-defined attribute and checks the derived property and energy
gain together with every built-in plugin.

## Benchmarking

```go
func BenchmarkStep10k(b *testing.B) {
    plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "interval": 10}`})
    rules, _ := sim.NewRuleset(plugins, cfg)
    e, _ := sim.NewEngine(sim.Config{Width: 200, Height: 200, IDKey: []byte("0123456789abcdef")}, rules)
    _ = e.Init()
    for i := range 10000 {
        _, _ = e.World().CreateEntity(sim.KindUnit, red, sim.Pos{X: int32(i % 200), Y: int32(i / 200)})
    }
    for b.Loop() { e.Step() }
}
```

```bash
GOTOOLCHAIN=local go test -run '^$' -bench . -benchmem ./examples/plugins/regeneration
```

For whole-game numbers with your plugin registered, add a scenario to
[`internal/scale`](../../../internal/scale/scale.go) and run
`go run ./cmd/rts-bench`. The 10 Hz budget is 100 ms per tick for
**everything**, so a plugin should cost a small fraction of that at 10k
units.
