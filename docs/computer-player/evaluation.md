# Bot-vs-bot evaluation and performance

## The arena

`mono-arena` plays computer players against each other without a server
process:

```bash
go run ./cmd/mono-arena -a 10 -b 1 -seeds 1-20              # level 10 vs level 1
go run ./cmd/mono-arena -a 6:aggressive -b 6:economic -seeds 1-20 -repeats 2
go run ./cmd/mono-arena -ladder -seeds 1-20 -json ladder.json
go run ./cmd/mono-arena -a 7 -b 7 -a-overrides '{"coordination":200}' -seeds 1-40
```

| Flag | Meaning |
| --- | --- |
| `-config` | two-player server configuration (default `configs/presets/duel.json`) |
| `-a`, `-b` | `difficulty[:personality]` of each side |
| `-a-overrides`, `-b-overrides` | JSON difficulty parameter overrides (tuning experiments) |
| `-seeds` | map seeds: `N`, `N-M` or a comma list |
| `-repeats` | matches per seed (bot seeds differ per repeat) |
| `-ladder` | every adjacent pair 2v1 … 10v9, plus 10v1 |
| `-max-ticks` | safety cap (the configuration's time limit normally ends matches) |
| `-parallel` | matches at once (default: CPU count; results do not depend on it) |
| `-json` | write every match result to a file |
| `-v`, `-log` | print each match; print the bots' logs (`info`, `debug`) |

How it works (`internal/bot/arena`):

- It builds the game from the configuration with the map seed, and a
  server ID key derived from the map seed, and serves it over an in-memory
  gRPC connection. Each bot connects with its slot's join token through
  the same `client.Runner` as `mono-bot` — the public API only.
- **Lockstep**: the arena executes a tick only after every bot has
  processed the previous one and submitted its commands. Matches run as
  fast as the bots and the server compute, and are reproducible: the same
  map seed and bot seeds give byte-identical results (`TestReproducible`).
- Sides alternate between the two spawn corners across seeds, so the map
  does not favor one level.
- The arena is an evaluation harness, not a player: it uses the server
  in-process only to step ticks and read the final result.

Recorded per bot and match: win/draw, score, units, embodied investment,
stored energy, energy mined, kills, units lost, investment destroyed,
commands and unit-commands submitted, failed commands, average and maximum
decision latency, final strategy and strategy switches, and submissions by
order kind (with `-json`). The table printed per matchup gives wins,
average match length, energy mined, commands and decision time.

## Results

All results below: duel preset (48×32, fog of war, 6000-tick limit), map
seeds 1–20, one match per seed with sides alternating, bot seeds
`seed×1000+1` / `+2`, current code (`go run ./cmd/mono-arena -ladder
-seeds 1-20` and the pairs shown). **n = 20 per row**; the 95% interval
(Wilson) shows how much is noise: with 20 matches, anything between ~30%
and ~70% is consistent with an even matchup.

### Difficulty ladder (balanced personalities)

| Matchup (A vs B) | n | A–B wins | A win rate (95% CI) | score A / B | mined A / B | units A / B | commands A / B |
|---|---|---|---|---|---|---|---|
| L2 balanced vs L1 balanced | 20 | 13–7 | 65% (43–82%) | 9815 / 9072 | 9311 / 8636 | 22 / 20 | 469 / 465 |
| L3 balanced vs L2 balanced | 20 | 12–8 | 60% (39–78%) | 11082 / 10189 | 10759 / 9864 | 20 / 20 | 470 / 501 |
| L4 balanced vs L3 balanced | 20 | 14–6 | 70% (48–85%) | 12464 / 11127 | 12246 / 10690 | 21 / 20 | 520 / 496 |
| L5 balanced vs L4 balanced | 20 | 14–6 | 70% (48–85%) | 12661 / 11922 | 12408 / 11606 | 20 / 19 | 530 / 524 |
| L6 balanced vs L5 balanced | 20 | 10–10 | 50% (30–70%) | 13555 / 12655 | 13556 / 12393 | 26 / 20 | 590 / 580 |
| L7 balanced vs L6 balanced | 20 | 11–9 | 55% (34–74%) | 12758 / 12373 | 12698 / 12152 | 25 / 25 | 562 / 528 |
| L8 balanced vs L7 balanced | 20 | 12–8 | 60% (39–78%) | 12932 / 12251 | 12753 / 11993 | 26 / 26 | 584 / 558 |
| L9 balanced vs L8 balanced | 20 | 11–9 | 55% (34–74%) | 13611 / 12131 | 13753 / 11874 | 25 / 26 | 572 / 533 |
| L10 balanced vs L9 balanced | 20 | 10–10 | 50% (30–70%) | 12328 / 12355 | 12235 / 12026 | 26 / 26 | 507 / 540 |
| L10 balanced vs L1 balanced | 20 | 18–2 | 90% (70–97%) | 12424 / 9003 | 12430 / 8555 | 26 / 20 | 551 / 448 |
| L5 balanced vs L1 balanced | 20 | 20–0 | 100% (84–100%) | 12359 / 8670 | 12303 / 8158 | 20 / 21 | 534 / 452 |
| L7 balanced vs L3 balanced | 20 | 15–5 | 75% (53–89%) | 13101 / 10939 | 13167 / 10498 | 26 / 21 | 584 / 507 |
| L7 balanced vs L5 balanced | 20 | 11–9 | 55% (34–74%) | 12703 / 11938 | 12598 / 11576 | 25 / 20 | 541 / 550 |
| L10 balanced vs L5 balanced | 20 | 9–11 | 45% (26–66%) | 12600 / 12598 | 12654 / 12174 | 25 / 21 | 576 / 521 |

### Personalities (level 6 vs level 6)

| Matchup (A vs B) | n | A–B wins | A win rate (95% CI) | score A / B | mined A / B | units A / B | commands A / B |
|---|---|---|---|---|---|---|---|
| L6 aggressive vs L6 balanced | 20 | 7–13 | 35% (18–57%) | 10452 / 12281 | 10293 / 12223 | 25 / 25 | 570 / 588 |
| L6 defensive vs L6 aggressive | 20 | 12–8 | 60% (39–78%) | 11599 / 10898 | 11544 / 10701 | 25 / 25 | 517 / 605 |
| L6 economic vs L6 balanced | 20 | 13–7 | 65% (43–82%) | 13418 / 12121 | 13617 / 12036 | 26 / 25 | 577 / 529 |
| L6 exploratory vs L6 balanced | 20 | 12–8 | 60% (39–78%) | 13361 / 11876 | 13400 / 11840 | 26 / 25 | 564 / 535 |
| L6 economic vs L6 aggressive | 20 | 18–2 | 90% (70–97%) | 12565 / 9723 | 12521 / 9578 | 25 / 25 | 548 / 599 |

What this shows:

- **Level 10 substantially outperforms level 1**: 18–2 (90%, 95% CI
  70–97%), with 45% more energy mined; level 5 already beats level 1 20–0.
- **Adjacent levels are competitive**: every adjacent pair is between 50%
  and 70% for the higher level (never below 50%), and none is significant
  on its own.
- **The gradient flattens above level 5.** L7 vs L5 55%, L10 vs L5 45%,
  L10 vs L9 50%. Levels 6–10 add faster planning, better threat models,
  coordination, retreats and raids — but between bots on this preset
  almost no fighting happens (12 kills in the 200 ladder matches; every
  match ended at the time limit), so matches are decided by economy, and
  the economy is saturated by level 5 (both sides mine nearly all energy
  their two home wells and nearby wells produce). See
  [limitations](#known-limitations).
- **Personalities matter** independently of level: at level 6, economic
  beats aggressive 18–2 and balanced 13–7; aggressive loses to balanced
  (7–13) because on this preset, decided on score, its larger army and early
  campaigns cost economy without the kills to pay for it; defensive beats
  aggressive 12–8.
- **Command usage** is modest: 450–600 submissions per bot per match
  (≈0.1 per tick) thanks to server-side automation, batch role tags and
  deduplication; failed commands were 298 of 210,714 submitted across all
  ladder matches (0.14%; mostly races such as a clone tile taken in the
  same tick, each one replanned).

How the tuning went (for whoever tunes next): the first versions had
level 10 *losing* to level 5. Measurement found, in order: units
stranding themselves with empty stores, a bank ringed by idle units so
miners could not deliver, an upgraded bank that could never afford to
clone, regenerating wells marked dead forever, miners buying expensive
extraction upgrades, scouts re-scouting forever, and idle fighters walking
back and forth to refuel (the largest energy leak: up to 40% of mined
energy). Each was fixed in the decision logic, the parameter that exposed
it kept, and the effect re-measured with `TestDebugRevert` /
`-a-overrides` sweeps (12–20 matches per variant).

## Live validation against the real server

Beyond the arena, the bot was run as separate processes against
`rts-server` over TCP gRPC, in real time:

- `mono-bot --difficulty 9` (red) vs `mono-bot --difficulty 3
  --personality aggressive` (blue) on the duel preset at 50 Hz: both
  readied up, played the full 6000-tick match with no failed commands, and
  logged the outcome (`match finished … outcome=won reason=time_limit
  scores="Red=11857 Blue=3830"`). The server's replay of that match,
  exported through the admin API, verifies with `rts-replay verify`
  (15,020 ticks, 152 checkpoints).
- Human vs computer in the browser: `rts-server` (duel) + `mono-bothost
  -slots 2` + `rts-gateway -bothost`. The player client showed the
  *Computer opponents* panel; adding a level 7 aggressive bot made it join
  Blue and ready up; the human readied and the match ran with the bot
  playing; *Remove* stopped it.
- `TestFogCompliance` (every tick of a 1500-tick match: everything the bot
  knows about foreign entities is in the server's player view),
  `TestReconnect` (transport killed and restored mid-match; the bot
  reconnects with its session and keeps playing) and `TestBadTokenStops`
  run against a real in-process server.

## Performance

Decision cost, `go test -run '^$' -bench BenchmarkDecide -benchmem ./internal/bot/ai`
(`make bot-bench`), level 10, one tick = knowledge update + every layer
due that tick + command flush, averaged over 500 ticks (so the 10 / 3 /
100-tick cadences are represented), own units on a square map facing a
tenth as many enemies, Apple M3:

| Own units | Decision time per tick (avg) | Worst tick (30-tick run) | Heap | Allocations per tick | Commands per tick | Unit-commands per tick |
| --- | --- | --- | --- | --- | --- | --- |
| 100 | 26 µs | — | 0.9 MB | 9.7 KB / 121 | 1.95 | 3.6 |
| 1,000 | 0.17 ms | 2.8 ms | 1.9 MB | 69 KB / 450 | 4.9 | 4.9 |
| 5,000 | 0.76 ms | — | 5.7 MB | 365 KB / 903 | 3.7 | 8.5 |
| 10,000 | 1.45 ms | 7.8 ms | 10.3 MB | 711 KB / 1,082 | 0.7 | 15.4 |

The worst tick is the first operational pass (assigning every role).
At 10,000 units the compute budget caps work per cycle (it was exhausted
67,596 times in 30 ticks — the bot acts on the best candidates found so
far rather than taking longer); the unit-command budget (200/tick) was
never exceeded (`TestLargePopulationStaysWithinBudgets`). Even the worst
tick is far inside a 100 ms (10 Hz) tick.

In a real match (duel, L10 vs L5, seed 3, `-parallel 1`): 6000 ticks in
0.97 s for server and both bots together, L10 averaging 4.5 µs per tick
(max 165 µs) and 719 commands (831 unit-commands) for the match; the whole
process peaked at 42 MB resident. A 20-seed matchup takes about 10–20 s
on 8 cores.

Optimizations made after profiling: own units and the role index are
cached per tick (the army was re-sorted on every query), the commander's
same-key check is a map (was quadratic), the occupancy map is built once
per tick, role tags go out as chunked batch commands, and hot loops avoid
sorting when a single pass finds the answer.

## Known limitations

- **The top half of the ladder plateaus** (above). Making levels 6–10
  clearly stronger needs fighting to matter between bots: e.g. presets
  where wells are contested, or combat that decides score; and an army
  that converts strength into kills efficiently (it currently raids
  rarely and retreats readily).
- **Tuned on one preset.** The bot reads all rules from the server and
  plays any configuration (the four shipped presets parse and run), but
  its constants (camp distance, well distance, population 26, monitor
  interval) were tuned on the duel map. Larger maps or more players will
  likely need retuning.
- **Two-player view of the world**: all other players are one enemy with
  one base guess.
- **No forward depots**: miners always deliver to the one bank, so distant
  wells cost a lot of travel; the bot limits how far it mines instead.
- **Congestion**: more than ~26 units jam the small duel bases (measured:
  35 units cut mining by half), so population is capped rather than
  spread out.
- **Arena vs live**: arena results are lockstep and reproducible; live
  matches add network timing and are not bit-reproducible (the server's
  replay still is).
- **Sample sizes**: 20 matches per pair separate large differences (L10
  vs L1) but not small ones (adjacent levels); run more seeds before
  drawing conclusions from a single adjacent pair.
