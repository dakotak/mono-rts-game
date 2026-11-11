# Extending the computer player

The AI is built to be extended without forking it. In order of effort:

| You want to… | Do this |
| --- | --- |
| change how a level plays | edit [`levels.json`](../../internal/bot/difficulty/levels.json) or use `difficulty_overrides` |
| add a play style | add a personality |
| change when a strategy is chosen | add a **heuristic** |
| give the bot a new goal | add an **objective** |
| change how a decision is made | edit the economic / tactical planners |
| add a difficulty parameter | add a field to `difficulty.Params` |

A worked example of a heuristic and an objective lives in
[`examples/botext`](../../examples/botext): a "watchtower" objective that
posts a lookout at the map center, and an "expansionist" heuristic. Its
test plays it in a real arena match.

```bash
go test ./examples/botext
```

```bash
go run ./examples/botext/cmd --token duel-red --difficulty 7
```

## Extensions API

Extensions are passed when the bot is built:

```go
cfg := bot.Defaults()
cfg.Token, cfg.Difficulty = "duel-red", 7
bot := cfg.NewBot(knowledge, logger, bot.Extensions{
	Heuristics: []ai.Heuristic{myHeuristic},
	Objectives: []ai.Objective{&myObjective{}},
})
```

`client.Options.NewBot` is the place to do that in your own program (see
`examples/botext/cmd`); `arena.BotSpec.Extensions` does it in the arena.

### Heuristics (strategic layer)

```go
type Heuristic interface {
	Name() string
	Score(f *ai.Features, strategy string) int // utility contribution
}
```

Every strategic cycle the bot sums all heuristics (built-in and yours) for
each strategy, scales the sum by the level's `adaptation`, adds the
personality bias and picks the best, subject to hysteresis. `ai.Features`
is computed from knowledge only. Return 0 when the heuristic has no
opinion; built-in heuristics use magnitudes of 100–450.

```go
var Expansionist = ai.HeuristicFunc{N: "expansionist", F: func(f *ai.Features, s string) int {
	if s == "economic_expansion" && f.ExploredPm < 300 && f.Tick < 2000 {
		return 200
	}
	return 0
}}
```

### Objectives (operational layer)

```go
type Objective interface {
	Name() string
	Plan(b *ai.Bot) (active bool, detail string)
}
```

`Plan` runs at the end of every operational cycle. It may read `b.K`
(knowledge), `b.Roles`, `b.Directive()`, `b.StrategyName()`, `b.Home()`,
`b.UnitsByRole(...)`, `b.Tick()`, and act through:

- `b.Issue(ai.Order{Key, Payload, Units, Priority, Desc, OneShot})` — any
  public command payload. Choose a key naming what the order controls:
  orders with the same key replace each other and identical standing
  orders are not re-sent (see [command management](architecture.md#command-management)).
  `Units` is what the server charges (units in the command).
- `b.SetRole(unit, ai.Role("ext:<name>"))` — claim a unit. Roles starting
  with `ext:` belong to extensions: the built-in planners never reassign,
  fuel, upgrade or command those units. Release a unit by setting a
  built-in role (e.g. `ai.RoleFighter`).

Active objectives and their detail strings appear in the diagnostics
(`objectives`).

### Personalities

Add an entry to `difficulty.Personalities`:

```go
"turtle": {Name: "turtle", StrategyBias: map[string]int{"defensive": 400, "tech": 200}, FighterShare: 100},
```

`StrategyBias` adds utility to strategies; `FighterShare` and
`ScoutShare` move permille of the miner share to fighters or add scouts.
Personalities are independent of difficulty.

### Strategies

`ai.Strategies` lists the strategies and their directives (target
population, miner share, scouts, guards, upgrade share, attack ratio,
reserve, resource denial). A new strategy needs an entry there and at least
one heuristic that ever favors it.

### Economic and tactical customization

The planners are ordinary methods in `internal/bot/ai`:

- `economy.go`: bank choice, role allocation (`assignRoles`), mining
  (`planMining`), spending (`planBank`, `chooseCloner`), upgrades
  (`rolePlan`, `nextUpgrade`, `planUpgrades`), fuel (`planForage`,
  `clearBank`), logistics, guards, salvage.
- `combat.go`: strength and threat models, target scoring (`engage`),
  campaigns and raids (`tactical`, `runCampaign`, `raidTarget`), retreat.
- `exploration.go`: sector scoring.

Rules of the road: read only `b.K` (never anything server-side), go
through `b.issue`, spend compute with `b.spend(n)` for loops over
candidates, use `b.pick` for choices that should respect `candidates` and
`mistake_rate`, iterate maps in sorted order, and draw randomness only
from `b.rng`.

## Adding a difficulty parameter

1. Add the field to `difficulty.Params` with a JSON name and a comment.
2. Add a value to every level in `levels.json` (monotonic: never worse at
   a higher level) and, if it is numeric, extend
   `TestLevelsAreCompleteAndMonotonic`.
3. Read it as `b.P.YourField` where the decision is made.
4. Measure it (next section).

## Testing changes

- Unit tests (`internal/bot/ai/*_test.go`) build knowledge from the real
  server rules (`configs/presets/duel.json`) and hand-made observations;
  copy a test such as `TestTargetSelectionFocusesWeakest` to pin a
  decision.
- `go test ./internal/bot/...` also runs fog compliance, reconnection,
  reproducibility and the import boundary.
- Measure strength in the arena: compare your change against the stock bot
  at the same level, with enough seeds that the difference is not noise
  (20+ matches per pair; a 12-of-20 result is not significant):

```bash
go run ./cmd/mono-arena -a 7 -b 7 -a-overrides '{"risk_tolerance":250}' -seeds 1-40
```

- The diagnostic tests in `internal/bot/arena/debug_test.go` (skipped
  unless enabled by environment variables) dump a bot's units over a
  match, and measure each difficulty parameter's effect on mining or on
  win rate (`ARENA_REVERT=1 ARENA_A=10 ARENA_B=7 ARENA_OPP=5 go test
  -run TestDebugRevert ./internal/bot/arena -v`).
