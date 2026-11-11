# Computer player: decision architecture

The computer player is a client. Everything it knows comes from
`Connect` (world size, command budget, the published rules) and the
`Observe` stream (snapshots and per-tick updates filtered for its player);
everything it does is a `SubmitCommand` with a public command payload.

```text
            rts-server (authoritative)
   Connect / Observe │            ▲ SubmitCommand (public payloads)
                     ▼            │
 ┌──────────────────────────────────────────────────────────┐
 │ client.Runner      connect, observe, reconnect, submit   │
 │   state.Knowledge  world model: own units, detections,   │
 │                    memories, wells, explored tiles, rules│
 │   ai.Bot.Step      once per tick:                        │
 │     strategic   ── every strategic_interval ticks        │
 │     operational ── every operational_interval ticks      │
 │     tactical    ── every tactical_interval ticks         │
 │   ai.Commander     priorities, budget, dedupe, results   │
 └──────────────────────────────────────────────────────────┘
```

Packages:

| Package | Role |
| --- | --- |
| `internal/bot/state` | world model built only from observations; rules parser |
| `internal/bot/difficulty` | the ten levels (`levels.json`) and the personalities |
| `internal/bot/ai` | the decision layers, the commander, diagnostics, extension points |
| `internal/bot/client` | gRPC runner: connect, observe, submit, reconnect |
| `internal/bot` | configuration (flags/JSON), logger, assembling a bot |
| `internal/bot/host` | bot host: start/stop bots in configured slots over HTTP |
| `internal/bot/arena` | bot-vs-bot evaluation (hosts a server; not used by bots) |
| `cmd/mono-bot`, `cmd/mono-bothost`, `cmd/mono-arena` | the programs |

`TestImportBoundary` fails the build if `ai`, `state`, `client`,
`difficulty`, `host` or the two bot commands import anything from this
module other than generated API types and the bot's own packages.

## World model

`state.Knowledge` is updated from each observation before the bot
decides:

- **Rules.** `rules_json` is parsed into attribute tables (cost and
  duration per level), the stat bindings (which attribute is energy
  capacity, extraction, attack, health, range, transfer range, vision,
  sensor, stealth, optics), movement and transfer costs, clone offspring
  energy, which plugins exist, victory and scoring. The bot hardcodes no
  unit classes and no numbers the server publishes.
- **Confidence classes.** Every fact is one of:
  - *current*: own entities, and foreign entities detected this tick (exact
    or estimated attributes as disclosed; estimates are ranges and are read
    at their midpoint);
  - *remembered*: foreign entities seen before and no longer detected,
    kept with the tick they were last seen, aged out after 600 ticks, and
    weighted down with age by threat models ≥ 1;
  - *estimated*: what the bot infers — where each opponent lives (see
    *Opponents* below), enemy damage per hit
    (from damage events on its own units), whether a well regenerates
    (seen refilling after running dry);
  - *unknown*: everything else. Unexplored tiles are tracked so scouts can
    target them.
- **Wells** keep their last known energy and a depleted flag (from
  `energy.well_depleted` events or observations), cleared when seen
  refilled.
- **Match resets.** A new `match_id` discards the old world and adopts the
  new player ID.

## The decision layers

`ai.Bot.Step` runs each layer when its interval comes due. Each run gets a
compute budget (`compute_budget` operations, counted, not timed); when it
runs out the layer stops considering more candidates and acts on the best
found so far.

### Strategic layer

Chooses one of six strategies, each a *directive* for the lower layers:

| Strategy | Population | Miner share | Scouts / guards | Upgrade share | Attack ratio | Reserve |
| --- | --- | --- | --- | --- | --- | --- |
| `economic_expansion` | 26 | 60% | 2 / 0 | 30% | 2.5× | 30 |
| `defensive` | 20 | 45% | 1 / 3 | 50% | 3.0× | 40 |
| `aggressive` | 18 | 38% | 1 / 0 | 35% | 1.1× | 20 |
| `tech` | 16 | 45% | 1 / 1 | 75% | 1.6× | 30 |
| `balanced` | 20 | 50% | 1 / 1 | 45% | 1.5× | 30 |
| `resource_denial` | 18 | 42% | 2 / 0 | 40% | 1.2× | 20 (targets near wells first) |

*Attack ratio* is how much stronger than the enemy estimate the army must
be to attack (scaled by risk tolerance).

Adaptive bots (adaptation ≥ 500) also read the published scoring: in a
timed match where a unit is worth more score than the energy a clone
consumes, they raise the population target to 26 (more than that jammed
the small duel bases and lowered mining in measurements).

Utility of a strategy = personality bias + `adaptation` × Σ heuristics +
(at lower levels) noise of ±`mistake_rate`/2. Built-in heuristics read
features computed from knowledge (`ai.Features`: tick, ticks left, own
units, fighters, own and estimated enemy strength, enemies seen, threat at
home, wells known, recent losses, bank energy, explored share):

| Heuristic | Effect |
| --- | --- |
| `threat` | enemy strength at home: `defensive` +450, `economic_expansion` and `tech` −250 |
| `strength` | own/enemy strength above 1.6: `aggressive`, `resource_denial` +350; below 0.8: `defensive` +300, `tech` +150, `aggressive` −300 |
| `economy` | two or more known wells and under 12 units: `economic_expansion` +250; no known well: −200 |
| `time` | under 1500 ticks left in a timed match: `economic_expansion` +200, `aggressive` −100 (score matters) |
| `losses` | three or more units lost since the last evaluation: `defensive` +250 |

**Hysteresis**: the bot switches only if the best strategy beats the
current one by 150 utility *and* the current one has been held for
`min_commitment` ticks — except for an emergency switch to `defensive`
when the home is threatened (adaptation ≥ 500). Scores, features, the
choice and the reason are logged (`--debug`).

### Operational layer

Allocates units and energy:

1. **Bank.** One unit stores energy and pays for things — the unit next to
   the best-known well (skilled) or the first unit.

   "Skilled" economic choices (bank placement, least-loaded wells, cheapest
   cloner) phase in with `economic_efficiency`: never at 100, always from
   450, with a proportional chance between, so low levels differ smoothly.
2. **Roles.** Miners, fighters, scouts, guards and salvagers from the
   directive: miners up to the capacity of nearby wells (a few per well; a
   regenerating well that ran dry is worth one), a scout or two, guards
   and salvagers when those capabilities are unlocked; everyone else
   fights. Units keep their role while it is still wanted (with ±1
   hysteresis on the miner count). New miners are taken fullest-first
   (they must walk to a well). Roles are tagged on the server with batch
   `SetMetadataCommand role=<role>`, so they show in the client and can be
   used in selectors.
3. **Mining.** Each miner runs the server's `mine_and_deliver` automation
   to the bank, with `keep` set to the round-trip energy so it never
   strands itself. Wells are chosen least-loaded first (skilled) or
   nearest; wells the player cannot currently detect are approached first
   (automation needs a detected well).
4. **Spending** (utility per energy over the planning horizon): clone,
   enlarge storage (only when the bank is nearly full and no clone is
   possible), or extend transfer range (logistics). The **cloner** is the
   cheapest unit near the bank that can hold the clone price — the price
   includes the parent's embodied upgrades, so an upgraded bank is usually
   a poor parent; the bank funds it with a transfer. When the bank is full
   (deliveries blocked), full miners waiting next to it clone themselves.
   No clone starts that could not finish before the time limit.
5. **Upgrades.** While the population is still growing, skilled bots pool
   idle energy in the bank instead of upgrading. Afterwards each unit buys
   the next level of its role plan (miners: storage only — extraction
   upgrades are expensive, keep the miner busy and are capped by the well's
   rate, which measurably cost a fully specialized bot ~15% of its mining;
   fighters/guards: attack, health, armor, range; scouts: vision,
   propulsion, sensor) in chunks, never spending the energy needed to get
   home and never starting one that cannot finish before the time limit;
   the bank funds units that happen to be next to it (nobody walks to the
   bank just to be funded). `specialization` sets how often
   a unit follows its plan instead of a random attribute.
6. **Fuel.** Drained units next to the bank get a top-up before any
   savings; units that can still make it home go home; units that cannot
   go to the nearest reachable well and mine themselves full ("forage").
   Idle units are moved off the bank's neighboring tiles so miners can
   always deliver. Skilled bots keep supply links from the bank to the
   emptiest fighters and guards (persistent server-side logistics).
7. **Guards and salvage** use server automation (`guard` a few tiles from
   the bank toward the enemy, `auto_salvage` within 10 tiles).
8. **Exploration.** Scouts take 8×8 sectors with unexplored tiles, scored
   by unexplored area, distance and (for aggressive explorers) direction
   toward the inferred enemy; only sectors within the scout's energy
   reach. The way back may end at a known well instead of home
   (`exploration_aggression` ≥ 300), where the scout mines itself full,
   so scouts range across the map by hopping between wells. A scout too
   empty to reach anything fills up at the bank (or a closer well); the
   bank refuels scouts before other spending and never pools their
   energy. With nothing left to explore, adaptive bots re-scout the enemy
   base at most every 1200 ticks (constant walking cost more energy than
   the information was worth).
9. **Extension objectives** run last (see [Extending](extending.md)).

### Opponents

Each opponent is tracked separately for the whole match (`intel.go`):
where their units have been seen (smoothed, larger groups weigh more),
when, and the most seen at once. Averaging every enemy together would
point at the empty ground between two opponents.

- **Whom to fight**: the opponent whose territory is closest to home,
  discounted by the largest army they have shown.
- **Sightings near home** (within 12 tiles) are raiders or scouts: they
  reveal that a player exists, and the bot guesses their base further out
  along that direction until it sees them away from its base.
- **Nobody found yet**: the target is a *search area*, the unexplored
  region (sector plus unexplored neighbors) furthest from home, kept for
  600 ticks so the bot does not dither. Campaigns toward it are armed
  reconnaissance: they turn into an attack on the first opponent found,
  and move on to the next unexplored area while the army can still get
  home.
- **Empty territory**: an army that reaches an opponent's area and sees
  none of their units forgets that estimate until they are seen again.
- Only with the whole map explored and nobody seen does the bot fall back
  to the mirror of its own base.

### Tactical layer

Reacts to the situation every few ticks:

- **Idle fighters** wait near a camp a few tiles toward the enemy and do
  not move unless they drift more than 6 tiles away; they walk to the bank
  for fuel only when an attack is likely (offensive strategy or enemies
  seen near home). Measurement showed idle shuffling was the largest
  energy leak.
- **Threats** are enemies near home or in a miner's area, noticed for at
  least `reaction_delay` ticks. Fighters engage them (defense takes
  priority over an ongoing campaign unless the bot is coordinated and its
  guards can hold).
- **Target selection**: score = finish weak targets first (inverse health)
  + threat value (threat model ≥ 1) + well proximity under
  `resource_denial` − distance. A coordinated bot (chance =
  `coordination`) focus-fires one target with a group move and one
  `AttackCommand` for the whole group; otherwise each unit attacks its
  nearest target.
- **Campaigns** start when the fueled fighters (energy for the trip plus a
  fight), at least three and a third of the army, exceed the enemy
  estimate by the strategy's attack ratio; the rest stay home as a
  reserve.
- **Mustering**: when the whole army would be strong enough but too few
  fighters are fueled, the bot musters. Fighters walk to the bank (four
  at a time), the bank fuels them up to what the march needs before
  clones or upgrades, and they stop spending their own energy on
  upgrades or pooling it in the bank. A muster that has not launched
  after 900 ticks (stranded fighters, a poor bank) is abandoned for 900
  ticks so the economy is not starved. Coordinated bots first *raid*: the nearest
  known enemy position at a well away from home (miners at work), judged
  against what was seen there. Otherwise the target is the inferred enemy
  base and the estimate includes unseen units (at least half of the bot's
  own count at base strength). Coordinated armies gather at a rally point
  before marching.
- **Retreat** (when unlocked): if local enemy strength exceeds own by the
  margin set by `risk_tolerance`, the army falls back home.
- **Strength** depends on the threat model: unit count; attack × health;
  plus armor and damage inferred from hits taken.

## Command management

All orders go through `ai.Commander`:

- **Keys and deduplication.** Every order has a key naming what it
  controls (`auto:<unit>`, `move:<unit>`, `grp:army`, `bank:xfer`, …).
  A *standing* order (automation, destination, tag) identical to the last
  one sent for its key is not re-sent for 600 ticks. A *one-shot* order
  (clone, transfer, upgrade) may repeat once its result arrived. Within a
  tick a later order for the same key replaces an earlier one (so "cancel
  automation" + "new automation" sends only the latter).
- **Replanning.** A failed command (from the result stream) or a rejected
  submission clears its key so the planners re-issue or choose differently;
  failure-specific handling drops stale assignments (e.g. a miner's well).
  A standing move whose units stopped short of the destination is
  re-issued; automation that the server ended is re-sent.
- **Energy guard.** Moves are filtered: units that could not afford the
  trip plus a reserve are left out (they refuel instead). Refuel, retreat
  and bank-clearing moves are exempt.
- **Budget.** Orders are sorted by priority (lifecycle > tactical >
  economy > roles > exploration) and submitted within the server's
  per-tick unit-command budget (each order is charged per unit, as the
  server charges); what does not fit is dropped and replanned. Role tags
  use at most a quarter of the budget per tick, the rest carried over.
- **Batching.** Group moves, group attacks and role tags are single
  commands for many units; miners, guards and salvagers run server-side
  automation instead of per-tick micromanagement.
- **Statistics**: submitted, deduplicated, deferred, rejected, succeeded,
  failed (by code), unit-commands charged, and submissions by order kind,
  in the diagnostics.

## Determinism and budgets

Decisions are a pure function of knowledge, configuration and the seeded
PCG generator; maps are iterated in sorted order where it matters; compute
budgets count operations. See [Determinism](README.md#determinism) for how
this relates to match determinism, and [Evaluation](evaluation.md) for the
lockstep arena.
