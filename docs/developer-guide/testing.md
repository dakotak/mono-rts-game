# Testing and benchmarking

```bash
make test       # go test ./...      (every package, including examples/)
make race       # go test -race ./...
make lint       # gofmt, go vet, staticcheck, buf lint
make bench      # BenchmarkStep in internal/game
make scale      # cmd/rts-bench: 1k–25k units × 7 workloads
```

All targets set `GOTOOLCHAIN=local`. To run one package or test:

```bash
GOTOOLCHAIN=local go test ./internal/plugins/logistics -run TestRelayChain -v
```

There's no CI configuration in the repository. Run `make lint test`
before submitting changes.

## Test layers

| Layer | Where | What it uses |
|---|---|---|
| Kernel | [`internal/sim/sim_test.go`](../../internal/sim/sim_test.go) | Tiny in-test plugins against `sim.NewRuleset` and `sim.NewEngine`: ordering, atomicity, registration errors, change tracking. |
| Plugin unit tests | `internal/plugins/<name>/*_test.go` | Mostly [`internal/plugintest`](../../internal/plugintest/plugintest.go): the **real** plugin composition with test configuration. |
| Game and integration | [`internal/game`](../../internal/game) | Determinism and replay, energy conservation, scale scenarios with fog, combat and evolution, and the protoreg guard (`TestEveryCommandTypeIsRegisteredForGateways`). |
| Server and API | [`internal/server`](../../internal/server) | Full gRPC over `bufconn`: auth, ownership, fog leaks, snapshot + update consistency, admin RPCs. |
| Gateway | [`internal/gateway`](../../internal/gateway) | HTTP/NDJSON translation, static file caching headers. |
| Web clients | [`web/web_test.go`](../../web/web_test.go), [`web/admintest`](../../web/admintest) | The admin page's wiring (every script embedded, every element the scripts use present, every panel in both default layouts) and, when Node.js is installed, the admin workspace's pure logic (`node --test web/admintest/*.test.js`: log buffer and filters, player statistics, inspector sections, default and saved layout validation). Docking, resizing and the board are checked in a browser; the project has no browser test runner. |
| Scale | [`internal/scale`](../../internal/scale) | Determinism of every scenario, allocation and timing regression bounds, `BenchmarkScale`. |
| Examples | [`examples/plugins/regeneration`](../../examples/plugins/regeneration) | A plugin's tests outside `internal/`. |

## The plugin test harness

`internal/plugintest` builds a 40×40 world with two players from
`game.Plugins()`, so tests exercise plugins exactly as wired in the
server:

```go
h := pt.New(t, map[string]string{          // plugin name → JSON config section
    "energy": `{"unit_capacity": 100, "initial_energy": 0}`,
})
u := h.Unit(1, 5, 5, 40)                   // slot 1 unit at (5,5) with 40 energy
id := h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{...}) // slot 0 = admin
f := h.Step()                              // or h.Steps(n)
r, _ := pt.Result(f, id)                   // the CommandResult
evs := pt.Events(h.Steps(10), "movement.arrived")
h.Energy(u); h.Prop(u, "mass"); h.W()      // inspection helpers
```

Use `pt.NewWithBudget(t, n, cfg)` to test the per-tick budget.
`pt.Rules` is a representative attributes configuration.

Watch the argument order. In `pt.Result(h.Step(), h.Submit(...))`, Go
evaluates `h.Step()` first, so the step runs **before** the submit.
Submit first, then step.

## Determinism tests

The standard pattern runs a scenario twice and replays it once, then
compares per-tick hashes:

```go
a, log := scenario(t, nil)   // run, recording f.Commands each tick
b, _ := scenario(t, nil)     // identical run
c, _ := scenario(t, log)     // replay via Engine.StepWith
// require a[i] == b[i] == c[i] for every tick
```

Examples: `TestAutomationReplayEquivalence`, `TestLogisticsReplay`,
`TestFogDeterminismAndReplayAtScale`, `TestScenariosAreDeterministic`.

## Benchmarks

| Benchmark / tool | Measures |
|---|---|
| `make bench` (`BenchmarkStep` in `internal/game`) | Per-tick cost at 1k/5k/10k moving units |
| `BenchmarkFogTick` (`internal/game`) | Fog observation with 4,000 units |
| `BenchmarkScale` (`internal/scale`) | ns/tick per scenario at 1k and 5k; compare with `benchstat` |
| `make scale` / `go run ./cmd/rts-bench` | p50/p95/max tick time, allocations, heap, events and changed views per tick, against the 100 ms budget. Supports `-cpuprofile` and `-memprofile`. |
| `make sim` (`cmd/rts-sim`) | Long headless runs with throughput and final state hash |

Profiling a scenario:

```bash
go run ./cmd/rts-bench -units 10000 -scenarios sensors -ticks 30 -cpuprofile /tmp/cpu.prof
go tool pprof -top /tmp/cpu.prof
```

Current numbers and bottlenecks are in
[design-notes.md](design-notes.md#performance-milestone-6).

## Testing a plugin

See [plugins/testing.md](plugins/testing.md). It covers registration
failures, config validation, commands, ticks, determinism, interactions
and benchmarks, using the example plugin's test file.
