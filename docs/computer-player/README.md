# Computer player

Mono ships an autonomous computer player, `mono-bot`. It joins a match the
way a human client does — with a player slot's join token, through the
public `GameService` gRPC API — and plays it without help: mining,
logistics, cloning, specialization through upgrades, scouting, defense and
attacks, with ten difficulty levels and five personalities.

It has the same powers and limits as a human player:

- **Public API only.** It calls `Connect`, `Observe` and `SubmitCommand`.
  It has no admin token, never calls `AdminService`, and does not link the
  simulation (enforced by `TestImportBoundary`).
- **Fog of war.** It knows only what the observation stream shows its
  player, plus its own memory of earlier sightings
  (`TestFogCompliance` checks every tick against the server's player view).
- **Same economy and budget.** No extra energy or units; it respects the
  server's per-tick unit-command budget and never exceeds it.
- **No server changes.** Difficulty changes decision *quality* (how often it
  thinks, how far ahead, how many options, how many deliberate mistakes),
  never what the rules allow.

Pages:

- This page: install, run, configure, difficulty, personalities,
  diagnostics, adding a computer opponent from the web client.
- [Decision architecture](architecture.md): world model, the strategic /
  operational / tactical layers, command management, determinism.
- [Bot-vs-bot evaluation and performance](evaluation.md): the arena,
  measured results, benchmarks.
- [Extending the AI](extending.md): new heuristics, objectives,
  personalities and difficulty parameters, with a working example.

## Quick start

```bash
make build                                   # builds bin/mono-bot, bin/mono-bothost, bin/mono-arena
bin/rts-server -config configs/presets/duel.json
```

```bash
bin/mono-bot --token duel-red  --difficulty 8 --player-name red
```

```bash
bin/mono-bot --token duel-blue --difficulty 4 --personality aggressive --player-name blue
```

The duel preset starts when both players are ready (`start: all_ready`);
each bot readies up by itself, plays until the match ends (elimination or
the 6000-tick time limit), logs the outcome and stays connected for the
next match (an administrator reset starts one; the bot adopts the new world
automatically).

To play against the computer yourself, join the duel as `duel-red` in the
browser and give `duel-blue` to a bot — either by running `mono-bot` as
above or from the web client (next section).

`make vs-bot` runs a server, a gateway and a bot host together for exactly
that (see the Makefile for the variables). To just watch, `make
watch-bots` starts three bots on the 3-player `trio` preset and opens the
admin console signed in ([details](../operations/matches.md#watching-computer-players)).

## Adding a computer opponent from the web client

The player client shows a **Computer opponents** panel when the gateway is
connected to a bot host:

```bash
bin/mono-bothost -config configs/presets/duel.json -slots 2
```

```bash
bin/rts-gateway -bothost http://127.0.0.1:8090
```

Open the player client, join with your token, pick a difficulty and a
personality for the free slot and click **Add computer**; **Remove** stops
it.

The bot host is the explicit, minimal orchestration boundary:

- It holds the join tokens of the slots it may fill — read from the
  `players` section of the server configuration, restricted by `-slots`, or
  given with `-slot-token SLOT=TOKEN` — and nothing else. It never reads or
  uses the admin token. Tokens are never sent to browsers.
- A bot it starts is the same client as `mono-bot`, in-process.
- Slots not listed are never taken, so nobody can take over another human's
  seat through it. Run it on localhost or behind the gateway; it has no
  authentication of its own (like the development gateway).

API (`/api/bots` on the bot host, proxied by the gateway):

| Method and path | Body / result |
| --- | --- |
| `GET /api/bots` | `{"slots":[{slot,name,running,difficulty,personality,started,error}], "difficulties":[{level,name}], "personalities":[...]}` |
| `POST /api/bots` | `{"slot":2,"difficulty":6,"personality":"aggressive"}` → the slot |
| `GET /api/bots/{slot}` | the slot with the bot's diagnostics (`status`) |
| `DELETE /api/bots/{slot}` | stops the bot |

## Configuration

Flags, or a JSON file with `--config` (flags given explicitly override the
file; unknown fields are errors):

| Flag | JSON | Default | Meaning |
| --- | --- | --- | --- |
| `--server` | `server` | `127.0.0.1:50051` | rts-server gRPC address |
| `--token` | `token` | (required) | the player slot's join token |
| `--player-name` | `player_name` | | label in logs (the server's player name comes from its configuration) |
| `--difficulty` | `difficulty` | `5` | 1 (beginner) … 10 (maximum) |
| `--personality` | `personality` | `balanced` | `balanced`, `aggressive`, `defensive`, `economic`, `exploratory` |
| `--seed` | `seed` | `0` | seed for the bot's own random choices (mistakes, tie-breaking noise) |
| `--deterministic` | `deterministic` | `false` | derive command IDs from the seed too (see below) |
| `--log-level` | `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `--debug` | `debug` | `false` | log utility scores and candidate evaluations (implies debug level) |
| `--status-addr` | `status_addr` | | serve diagnostics JSON at `http://ADDR/status` (bind to localhost) |
| `--ticks` | | `0` | stop after this many ticks (testing) |
| | `difficulty_overrides` | | JSON object overriding individual difficulty parameters (tuning) |

Example file:

```json
{
  "server": "127.0.0.1:50051",
  "token": "duel-blue",
  "player_name": "blue",
  "difficulty": 7,
  "personality": "defensive",
  "seed": 42,
  "difficulty_overrides": {"risk_tolerance": 300, "mistake_rate": 0}
}
```

Several bots can run at once (one per slot): separate processes, or one
bot host. `SIGINT`/`SIGTERM` stop a bot gracefully (it finishes the tick in
hand and logs a summary). If the connection drops, it reconnects with
backoff and resumes; if the server restarted (sessions are in memory) it
connects again with the join token. A rejected join token stops it with an
error.

## Difficulty

Levels are data: [`internal/bot/difficulty/levels.json`](../../internal/bot/difficulty/levels.json),
validated at start-up (levels 1..10, positive budgets). Every parameter
changes how well the bot decides, never what it may do:

| Parameter | Effect |
| --- | --- |
| `strategic_interval`, `operational_interval`, `tactical_interval` | ticks between evaluations of each layer (decision frequency) |
| `reaction_delay` | ticks a newly seen threat must persist before the bot reacts (response delay) |
| `planning_horizon` | ticks over which investments must pay back; upgrades taking longer are not considered |
| `candidates` | options compared per choice (wells, targets, sectors, plans) |
| `compute_budget` | evaluation operations per decision cycle — a deterministic unit of work, not time |
| `economic_efficiency` | miner allocation (least-loaded vs nearest well), bank/cloner choice, how far it mines, how long it grows before upgrading |
| `exploration_aggression` | scout count and reach; skilled scouts head for the unknown and the inferred enemy base |
| `coordination` | focus fire and group moves (vs per-unit orders); gathering before a march; raids on enemy miners |
| `threat_model` | 0 counts units; 1 weighs attack × health and remembered sightings; 2 also infers enemy damage from hits taken and armor |
| `specialization` | how often upgrades follow the role plan (vs a random attribute) |
| `adaptation` | how much observed conditions move strategy utilities (vs the personality default) |
| `risk_tolerance` | lower = attacks only with a clearer advantage; also the retreat threshold |
| `mistake_rate` | permille chance of deliberately taking a random option instead of the best (controlled mistakes) |
| `min_commitment` | ticks a strategy is kept before an ordinary switch (hysteresis) |
| `use_logistics`, `use_salvage`, `use_guards`, `retreat`, `scout` | capabilities unlocked by skill (supply links, auto-salvage, guard automation, retreating, dedicated scouts) |

The current values (`TestLevelsAreCompleteAndMonotonic` checks that no
parameter gets worse as the level rises):

<!-- difficulty-table -->
| Parameter | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |
|---|---|---|---|---|---|---|---|---|---|---|
| name | beginner | novice | casual | developing | intermediate | competent | advanced | expert | elite | maximum |
| `strategic_interval` | 400 | 367 | 333 | 300 | 267 | 233 | 200 | 167 | 133 | 100 |
| `operational_interval` | 60 | 54 | 49 | 43 | 38 | 32 | 27 | 21 | 16 | 10 |
| `tactical_interval` | 30 | 27 | 24 | 21 | 18 | 15 | 12 | 9 | 6 | 3 |
| `reaction_delay` | 40 | 36 | 31 | 27 | 22 | 18 | 13 | 9 | 4 | 0 |
| `planning_horizon` | 150 | 300 | 450 | 600 | 750 | 900 | 1050 | 1200 | 1350 | 1500 |
| `candidates` | 1 | 3 | 4 | 6 | 8 | 9 | 11 | 13 | 14 | 16 |
| `compute_budget` | 300 | 2489 | 4678 | 6867 | 9056 | 11244 | 13433 | 15622 | 17811 | 20000 |
| `economic_efficiency` | 150 | 244 | 339 | 433 | 528 | 622 | 717 | 811 | 906 | 1000 |
| `exploration_aggression` | 150 | 211 | 272 | 333 | 394 | 456 | 517 | 578 | 639 | 700 |
| `coordination` | 0 | 111 | 222 | 333 | 444 | 556 | 667 | 778 | 889 | 1000 |
| `threat_model` | 0 | 0 | 0 | 1 | 1 | 1 | 2 | 2 | 2 | 2 |
| `specialization` | 100 | 200 | 300 | 400 | 500 | 600 | 700 | 800 | 900 | 1000 |
| `adaptation` | 0 | 111 | 222 | 333 | 444 | 556 | 667 | 778 | 889 | 1000 |
| `risk_tolerance` | 900 | 839 | 778 | 717 | 656 | 594 | 533 | 472 | 411 | 350 |
| `mistake_rate` | 450 | 400 | 350 | 300 | 250 | 200 | 150 | 100 | 50 | 0 |
| `min_commitment` | 1200 | 1111 | 1022 | 933 | 844 | 756 | 667 | 578 | 489 | 400 |
| `use_logistics` | – | – | – | – | – | yes | yes | yes | yes | yes |
| `use_salvage` | – | – | – | yes | yes | yes | yes | yes | yes | yes |
| `use_guards` | – | – | yes | yes | yes | yes | yes | yes | yes | yes |
| `retreat` | – | – | – | – | yes | yes | yes | yes | yes | yes |
| `scout` | – | yes | yes | yes | yes | yes | yes | yes | yes | yes |

How the levels play, roughly: 1–2 mine the two nearest wells, mostly
clone from the bank, upgrade at random, make many deliberate mistakes and
attack in a straight line; 3–4 add guards, salvage, threat estimates and
increasingly skilled economic choices; 5–7 plan upgrades by role, choose
cloners by price, retreat, raid enemy miners, adapt their strategy and
read the scoring rules; 8–10 decide every few ticks with full candidate
lists and no deliberate mistakes. [Evaluation](evaluation.md) has the measured win rates.

## Personalities

A personality biases strategy selection and the role mix, independently of
difficulty: a level 9 defensive bot defends well; a level 2 aggressive bot
attacks often and simply.

| Personality | Strategy bias (utility) | Role mix |
| --- | --- | --- |
| `balanced` | balanced +150 | — |
| `aggressive` | aggressive +300, resource_denial +150 | more fighters (+12% of the miner share) |
| `defensive` | defensive +300, tech +100 | slightly more fighters |
| `economic` | economic_expansion +300, tech +100 | more miners |
| `exploratory` | economic_expansion +150, balanced +100 | an extra scout |

Strategies are `economic_expansion`, `defensive`, `aggressive`, `tech`,
`balanced` and `resource_denial`; see
[architecture](architecture.md#strategic-layer).

## Determinism

The bot's decisions are a deterministic function of what it has observed,
its configuration and its seed:

- All randomness (mistakes, tie-breaking noise) comes from a PCG generator
  seeded with `seed`; nothing reads the clock to decide. Compute budgets
  count operations, not time.
- Collections are iterated in sorted order wherever order affects a
  decision.
- With `--deterministic`, command IDs are derived from the seed as well, so
  the same observation stream produces byte-identical submissions
  (`TestDecisionsAreDeterministic`). Without it, a random nonce keeps IDs
  unique across restarts (the server deduplicates by ID).

This is not the same as *match* determinism. Against a live server, when a
command arrives relative to a tick boundary depends on network timing, so
two live matches with the same seeds can differ. The arena removes that
variable by stepping the server only after every bot has processed the
previous tick, and fixes the server's ID key from the map seed: arena
matches replay exactly (`TestReproducible`). The server's own determinism
(same inputs, same states) is described in
[Determinism and replay](../developer-guide/determinism.md); a recorded
match replays and verifies independently of the bots.

## Diagnostics

Structured logs (`log/slog` text format) on stderr:

```text
level=INFO msg=strategy bot=red tick=62 strategy=economic_expansion previous="" reason=initial utility=204
level=INFO msg=bank bot=red tick=62 unit=7745474258927287964 pos=(5,3)
level=INFO msg=status bot=red tick=1100 strategy=balanced units=17 roles="bank=1,miner=8,fighter=6,scout=1,guard=1" bank_energy=162 threats=0 own_strength=650 enemy_estimate=400 campaign="" commands=402 failed=0 avg_decision_us=38
level=INFO msg=campaign bot=red tick=2340 target=(42,27) raid=true army=6 own=1459 enemy_estimate=852
level=INFO msg="match finished" bot=red tick=6061 outcome=won reason=time_limit run_ticks=6000 scores="Red=10204 Blue=7385"
```

- `status` lines come every strategic interval; strategy switches, bank
  choice, campaigns, retreats and command failures are logged as they
  happen.
- `--debug` adds per-cycle utility scores of every strategy with the
  features they were computed from, the bank's spending candidates with
  their scores, and every order submitted.
- `--status-addr 127.0.0.1:8092` serves the live state as JSON at
  `/status`: strategy and its scores, role counts, bank energy, threats,
  own vs estimated enemy strength, campaign, explored share, command
  statistics (submitted, deduplicated, deferred, rejected, failed by code,
  by order kind), compute-budget exhaustion and decision latency.

## Limitations

- Tuned on the duel preset (48×32, two players, scarce wells). It reads all
  rules from the server, so it plays any configuration, but how well on
  very different rules (e.g. free movement, no fog, huge maps) is
  untested.
- Two-player oriented: with more players it treats every other player as
  one enemy and guesses a single enemy base.
- No pathfinding of its own: it relies on the server's movement and
  replans when a unit stops short.
- Energy shown by estimates (fog) is taken at the midpoint of the range.
- Combat is coarse compared to economy: few kills happen between bots of
  similar level, so most matches end at the time limit on score.

See [evaluation](evaluation.md#known-limitations) for what the measurements
show.
