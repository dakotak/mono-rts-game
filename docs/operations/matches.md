# Running matches: presets, resets, bot matches and replays

## Presets

Three ready-made configurations live in
[`configs/presets`](../../configs/presets). They use the normal
configuration format: edit them freely.

| Preset | For | Rules |
|---|---|---|
| [`sandbox.json`](../../configs/presets/sandbox.json) | Experimenting, debugging, testing mechanics | 40×28 arena, 2 players × 8 units with 100 energy, unlimited rich wells, free movement, **no fog, no victory conditions, no command budget**, starts immediately. |
| [`standard.json`](../../configs/presets/standard.json) | A reasonable competitive game | 64×40 procedural map (rock clusters, marsh), **random seed per match**, 2 players × 6 units with 40 energy, scarce finite wells (2 near each spawn plus 6 elsewhere, no regeneration), **fog of war**, budget 500, starts when both players are ready, **last player standing**, 30-minute time limit (18,000 ticks at 10 Hz) scored by units, investment, energy and kills. |
| [`duel.json`](../../configs/presets/duel.json) | Human vs computer, computer vs computer | 48×32 procedural map (fixed seed), 2 players × 6 units in opposite corners, 2 regenerating wells near each spawn plus 4 elsewhere, fog of war, starts when both are ready, last player standing, 10-minute limit (6000 ticks) scored by units, investment, energy and kills. Join tokens `duel-red` / `duel-blue`. The computer player is tuned on it. |
| [`trio.json`](../../configs/presets/trio.json) | Watching three computer players (`make watch-bots`) | Like bot-arena with 3 players in a ring on 48×48, 10 Hz, 6000-tick limit, scores public during the match. Tokens `trio-red`/`trio-blue`/`trio-green`, admin `trio-admin`. |
| [`bot-arena.json`](../../configs/presets/bot-arena.json) | Autonomous clients, reproducible evaluation | 48×48 procedural map with a **fixed seed and ID key** (same map and IDs every match), 4 players in a ring × 5 units, wells regenerate 1/tick, fog of war, **20 Hz**, budget 200, starts when all 4 bots are ready, last player standing, 10-minute limit (12,000 ticks), **scores public during the match**. |

All three share the attribute, combat, cloning and automation rules of
[`configs/dev.json`](../../configs/dev.json). Their tokens (`sandbox-*`,
`change-me-*`, `bot-*`) are placeholders: replace them before exposing a
server.

Validate any configuration without serving it:

```bash
bin/rts-server -check -config configs/presets/standard.json
```

`-check` builds the whole match, world generation included, and prints the
seed, entities per kind, units per player, the state after setup, the
plugins, the ruleset hash and the tick schedule. A configuration error,
or a map that can't be generated within `worldgen.max_attempts`, exits
non-zero with the reason.

### What can change, and when

| Setting | Changes take effect |
|---|---|
| Everything under `world` (except tokens), `units`, every `plugins` section | At the **next match**: send a new configuration with `ResetMatch` (admin client: *New match*), or restart the server. |
| `players`, `admin_token` | Only by **restarting the server**. `ResetMatch` ignores them, so sessions stay valid. |
| Match lifecycle (start, pause, resume, end, eliminate) | **During** a match, through admin commands. |
| Tick clock pause and single-step (`SetPaused`, `Step`) | Any time; for debugging. This is separate from pausing the *match*. |

Nothing about the rules can change in the middle of a match. That keeps a
match reproducible from its configuration and command log.

## Starting and resetting

1. Start the server with a preset: `CONFIG=configs/presets/standard.json make dev`.
2. Players connect and press **Ready** (or send `rts.match.v1.SetReadyCommand`).
   With `"start": "all_ready"`, the match starts in the tick the last one
   is ready. With `"manual"`, start it from the admin client or with
   `StartMatchCommand`.
3. The match ends on a victory condition, or by an admin's **End match**
   (`EndMatchCommand`).
4. **New match.** The admin client's *Reset / new match* button calls
   `ResetMatch`. You can supply:
   - a **seed**: blank uses the configuration (its seed, or a random one
     with `random_seed`)
   - a **random seed** checkbox
   - a whole new **configuration**: *Load current config*, edit, reset.
     It's validated first; on any error the current match continues.

Reset semantics:

- **Fresh world.** A complete new game is built: world, entity and player
  IDs, every plugin instance, the RNG, the command queue and
  acknowledgments, fog-of-war state, the observation hub and the replay
  recorder. Nothing leaks from the previous match. A test checks that
  resetting with the same seed and ID key reproduces the original initial
  state hash exactly.
- **Players follow automatically.** Their sessions survive. Open streams
  restart with a snapshot of the new match (a new `match_id`), and players
  get their new IDs in `MatchInfo.your_player_id`.
- **History is kept.** The previous match's result and replay stay
  available for the last 16 matches.

## Playing against the computer

```bash
make vs-bot                       # server (duel) + bot host + gateway
BOT_LEVEL=7 make vs-bot           # ... and a level 7 computer joins right away
```

Join http://127.0.0.1:8080 as `duel-red` and add (or remove) the computer
opponent from the *Computer opponents* panel. Any slot of any preset can be
given to `mono-bot` instead (`--token <slot token>`); see the
[computer player guide](../computer-player/README.md). For bot-vs-bot
evaluation without a server process, use `mono-arena`
([evaluation](../computer-player/evaluation.md)).

## Watching computer players

```bash
make watch-bots                                          # 3 bots: level 8 aggressive, 5 balanced, 3 economic
BOTS="9:aggressive 6:economic 3:defensive" make watch-bots
BOT_LOG=info OPEN=0 make watch-bots                      # bot logs on the console; do not open a browser
```

This starts the server with the 3-player [`trio.json`](../../configs/presets/trio.json)
preset (48×48, fog of war, 10 Hz, last player standing or a 6000-tick
limit), the gateway and one `mono-bot` per entry in `BOTS`
(`difficulty[:personality]`, slots Red, Blue, Green in order), then opens
the admin console already signed in (`http://127.0.0.1:8081/#token=…`; the
token is in the URL fragment, which is not sent to the server, and is
removed from the address bar). Scores are public during the match and
show in the admin *Match* panel; **View as** shows a bot's fog of war.
Ctrl-C stops everything. Other configurations work too
(`CONFIG=configs/presets/bot-arena.json BOTS="..."`), as long as every slot
the match waits for gets a bot.

## Running automated bot matches

```bash
CONFIG=configs/presets/bot-arena.json make dev          # terminal 1
for t in red blue green gold; do                          # terminal 2
  go run ./examples/bot -token bot-$t -ticks 3000 &       # the minimal example bot, or:
done                                                      # go run ./cmd/mono-bot --token bot-$t --difficulty 6
```

What happens:

- The example bot readies up automatically, plays until its tick budget
  runs out, and follows resets.
- Watch from the admin client (http://127.0.0.1:8081, token
  `bot-arena-admin`). It shows readiness, connections and live scores,
  and **View as** shows each bot's view.
- For unattended series, script the admin API:
  `GetMatchDetails`, `ResetMatch`, then `ExportReplay` after each match.
  The fixed seed in `bot-arena.json` replays the same map every match;
  pass a different `seed` to `ResetMatch` to vary it.

## Replays

Every match is recorded as it runs. A replay (format `mono-replay/1`) is
exported from the admin client (*Replays → Export replay*) or with
`AdminService.ExportReplay`. It can be the current match so far, or any of
the last 16 matches.

```bash
bin/rts-replay info   mono-replay-3eaf5cdf9cb9551c.json
bin/rts-replay verify mono-replay-3eaf5cdf9cb9551c.json
```

Verification rebuilds the match from the recorded configuration and seed,
with no clients, and replays every command at its recorded tick. It
compares a digest of every tick's frame and the full state hash at
checkpoints, then reports success or the **first divergent tick**:

```
verify     OK: 755 ticks replayed, 9 checkpoints matched, final hash 9c3d5bbc79b94a6f (42ms)
```

The admin client can verify an uploaded file too (*Verify a replay file*),
in an isolated simulation that never touches the live match. The format
and its guarantees are documented in
[developer-guide/replay.md](../developer-guide/replay.md).

Replays contain the world's **ID key** (needed to reproduce entity IDs)
but **no join or admin tokens**. Treat replays of unfinished matches as
admin-only information: they reveal everything.
