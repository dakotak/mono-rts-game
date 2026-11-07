# Custom victory conditions and scoring rules

Source: [`internal/plugins/match`](../../../internal/plugins/match/match.go).
Working example: [`examples/plugins/koth`](../../../examples/plugins/koth/koth.go)
("king of the hill": a condition and a metric, with tests).

## The match plugin in brief

- **State.** The match status (lifecycle state, players, readiness,
  elimination, cumulative stats, scores, result) is one world **global**,
  `match.status`, read with `match.Get(w)`. It is hashed and replayed, and
  reported in frames when it changes.
- **Gate.** The plugin is the kernel's `sim.Gate`. Player gameplay
  commands run only while the match is RUNNING, and systems (yours
  included) run only while RUNNING.
- **Evaluation.** At the end of every running tick (phase `match`, after
  combat, cloning, salvage and logistics) it updates statistics and then
  evaluates victory in this order:

  ```
  admin EndMatchCommand (immediately, when it executes)
  > elimination (if enabled)
  > custom conditions, in AddCondition order
  > time limit (highest score wins; ties are a draw)
  ```

## Victory conditions

```go
type Condition interface {
    Name() string
    // Check runs at the end of every running tick. Return non-nil to finish
    // the match. Deterministic and read-only.
    Check(v *match.View) *match.Outcome
}

type Outcome struct {
    Winner sim.PlayerID // 0 with Draw = no winner
    Draw   bool
    Reason string       // recorded in the result, e.g. "king_of_the_hill"
}
```

`*match.View` gives read-only access:

| Member | Use |
|---|---|
| `World` | The world (read only!) |
| `Status` | `*match.Status`: `Players[:N]`, `Eliminated`, `Stats`, `Scores`, `RunTicks`, … |
| `Aggregate(p)` | Current units, embodied investment and stored energy of a player (computed once per view) |
| `Stats(p)` | Cumulative energy mined, kills, units lost, investment destroyed |
| `Score(p)` | The configured weighted score |

## Scoring metrics

```go
type Metric interface {
    Name() string         // weighted by name in match.scoring.weights
    Cumulative() bool     // informational: accumulates over the match vs. current state
    Value(v *match.View, p sim.PlayerID) int64
}
```

`match.NewMetric(name, cumulative, func(v *match.View, p sim.PlayerID) int64)`
builds one from a function. Configured weights must name existing metrics,
or startup fails with the list of available ones.

## A complete example: king of the hill

A player "holds" the hill in a tick when only their units are within
`radius` of the map center. The first to hold it for `hold_ticks` running
ticks wins, and the hold time is a scoring metric. The plugin needs
**state** (hold time per player), so it is a full plugin:

- its own config section
- a world global for the hold time
- a system that counts control each tick

It registers a condition and a metric with the match plugin:

```go
type Hill struct {
    Holder sim.PlayerID
    Ticks  [match.MaxPlayers]int64        // by match player index
    Owners [match.MaxPlayers]sim.PlayerID
}

var hill = sim.NewGlobal("koth.hill", Hill{})

func New(m *match.Plugin) *Plugin {
    p := &Plugin{}
    m.AddCondition(p)                                            // victory condition
    m.AddMetric(match.NewMetric("hill_ticks", true, p.hillTicks)) // scoring rule
    return p
}

func (*Plugin) DependsOn() []string { return []string{match.Name} }

func (p *Plugin) Register(r *sim.Registrar) error {
    p.cfg = Config{Radius: 2, HoldTicks: 300}
    if err := r.DecodeConfig(&p.cfg); err != nil {
        return err
    }
    if err := r.Global(hill.Def()); err != nil { // hashed and replayed with the world
        return err
    }
    // Count control before the match evaluates its conditions.
    if err := r.Phase(sim.PhaseDef{Name: "koth", Before: []string{match.Phase}}); err != nil {
        return err
    }
    if err := r.System(sim.SystemDef{Name: "koth.control", Phase: "koth", Run: p.control}); err != nil {
        return err
    }
    return r.Describe(p.cfg)
}

func (p *Plugin) Check(v *match.View) *match.Outcome {
    if !p.cfg.Enabled {
        return nil
    }
    h := hill.Get(v.World)
    for i := range int(v.Status.N) {
        if h.Ticks[i] >= p.cfg.HoldTicks {
            return &match.Outcome{Winner: v.Status.Players[i], Reason: "king_of_the_hill"}
        }
    }
    return nil
}
```

Why a global and a system rather than computing everything in `Check`:

- **State must live in authoritative storage.** Hold time accumulates over
  ticks. Only components and globals are hashed, replayed and reset with
  the world, never plugin struct fields.
- **Systems only run while the match is RUNNING** (the gate), so control
  is never counted in the lobby or while paused.

### Registering it

In [`game.Plugins()`](../../../internal/game/game.go), create the match
plugin as a variable, pass it to the condition plugin, and list both:

```diff
+	m := match.New(attrs, fighting)
 	return []sim.Plugin{
-		match.New(attrs, fighting),
+		m,
+		koth.New(m),
 		worldgen.New(terrainP, wellsP),
```

Configure both sections:

```json
"koth":  { "enabled": true, "radius": 2, "hold_ticks": 600 },
"match": { "victory": { "elimination": true, "time_limit_ticks": 12000 },
           "scoring": { "weights": { "units": 100, "hill_ticks": 5 } } }
```

`MatchInfo.victory_conditions` then lists
`["elimination", "king_of_the_hill", "time_limit"]`. Results report the
reason `king_of_the_hill`.

For tests, compose without editing `game.Plugins()`, exactly as
[`koth_test.go`](../../../examples/plugins/koth/koth_test.go) does:

```go
plugins := game.Plugins()
m, _ := game.Find[*match.Plugin](plugins)
g, err := game.NewWithPlugins(cfg, append(plugins, koth.New(m)))
```

The tests cover:

- a holder winning at exactly `hold_ticks`, with score `2 × hill_ticks`
- a contested hill counting for nobody
- the plugin disabled by default
- invalid configuration rejected

## Rules for conditions and metrics

- **Deterministic and read-only.** Use `v.World` and `v.Status` only for
  reading. Iterate sorted IDs (`World.EntityIDs()`), and never use maps
  for ordering.
- **Keep them cheap.** They run every running tick (conditions) or every
  `scoring.interval_ticks` (metrics). `View.Aggregate` already does one
  pass over entities and caches it.
- **Information.** Conditions and metrics see everything, but players only
  see what the match publishes: their own score while running, everything
  after the match. A condition's `Reason` and a metric's value become
  public in the result.
- **Limits.**
  - At most 16 players (`match.MaxPlayers`).
  - Conditions can't veto or reorder the built-in ones.
  - Scores are integers (int64).
  - Conditions and metrics are compiled in: there's no scripting.
