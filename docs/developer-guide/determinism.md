# Determinism and replay

**Guarantee:** given the same initial configuration (including
`world.seed` and `world.id_key`) and the same per-tick command log,
`Engine.StepWith` reproduces every tick bit for bit. Every intermediate
`World.StateHash()` matches, and so do the frames.

Wall-clock timing, network arrival order, goroutine scheduling and Go map
iteration order **never** influence results.

## How the kernel ensures it

| Source of nondeterminism | Countermeasure |
|---|---|
| Command arrival order | Commands are sorted by (admin, player ID, per-issuer `Seq`) before execution. Only an issuer's *own* order is preserved. |
| Map iteration | The kernel iterates entities (`World.EntityIDs`), components (`Component.Entities`), plugins, phases and systems in sorted order. Topological sorts break ties by name. |
| Randomness | `World.Rand()` is a SplitMix64 implemented in the kernel (`sim.Rand`), seeded from `world.seed`. Its sequence can't change with Go releases, and its state is part of `StateHash`. |
| Floating point | None in the simulation. All quantities are integers, and formulas use [`internal/expr`](../../internal/expr/expr.go) (int64, total, saturating). |
| Wall clock | Never read inside a tick. `Engine.Run` only decides *when* to call `Step`. |
| Entity IDs | `HMAC-SHA256(id_key, counter)`: deterministic given the key. The counter is part of `StateHash`. |
| Concurrency | None inside a tick: one goroutine runs all commands, systems and observation. |
| Admin actions | All world mutations, admin ones included, are scheduled commands and appear in `Frame.Commands`. Pause and step change *when* ticks run, not *what* they compute. |

## Replaying

```go
// Record: run normally and keep each frame's executed commands.
var log [][]sim.Command
for range ticks {
    f := engine.Step()
    log = append(log, f.Commands)
}

// Replay: same config (same seed and id_key), then feed the log.
for _, cmds := range log {
    replayEngine.StepWith(cmds)
}
```

The repository verifies this in several places:

- `internal/game/determinism_test.go`: replays, including with reversed
  within-tick submission order
- `internal/game/fog_scale_test.go`: 4,000 units with fog
- `internal/plugins/automation`, `internal/plugins/logistics`: per-plugin
  replay tests
- `internal/scale`: repeated-run hashes for every scale scenario
- `examples/plugins/regeneration`

## Limits of what's guaranteed

- **No replay files.** Logs live in memory. Command payloads are
  protobuf messages, so persisting them is straightforward, but there's no
  file format, CLI or tooling for it yet.
- **Same build only.** A replay is exact only with the same plugin set,
  configuration and code. Changing a plugin's logic, adding a component
  (the component list is part of `StateHash`) or reordering phases
  changes results. There is no cross-version replay compatibility.
- **The ID key is part of the initial state.** With an empty `id_key` the
  server generates a random one, and the run can't be reproduced unless
  you record it.
- **Observation is deterministic too**, but clients don't receive the
  authoritative state, only their view.

## Rules for code that runs inside a tick

These apply to kernel and plugin code alike. The full checklist with
examples is in
[plugins/state-and-determinism.md](plugins/state-and-determinism.md).

1. Never let the iteration order of a Go map decide an outcome. Iterate
   sorted IDs.
2. Never read the wall clock, environment, files or network in a system
   or handler.
3. Use `World.Rand()` for randomness, never `math/rand` or `crypto/rand`.
4. Use integer math, and define rounding explicitly (floor division,
   permille).
5. Resolve simultaneous claims from one snapshot with an explicit
   tie-break.
6. Store all state that affects future ticks in components, never in
   plugin struct fields. Fields hold configuration and reusable scratch
   buffers only.
