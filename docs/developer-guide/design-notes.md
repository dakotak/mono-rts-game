# Design notes, limitations and roadmap

Why the engine is built the way it is, the assumptions made where the
specification was silent, what is known not to work yet, and what comes
next. Status as of Milestone 7.

## Key decisions and tradeoffs

| Decision | Why | Cost / revisit when |
|---|---|---|
| Kernel is proto-free; command payloads are `any` | Kernel stays testable and transport-agnostic | Persisting replay logs to disk needs a payload codec (plugins' payloads are proto already) |
| Typed generic component handles over `map[..]any` | Type safety at call sites, no ECS dependency | Profile if entity counts reach ~10⁵ |
| All contenders fail on a contested tile | Literal spec rule, no ID bias | Needs random backoff to avoid livelock (implemented) |
| Cycles and swaps fail | Simplest well-defined rule | Revisit if formations feel sticky |
| One entity per tile, kernel-enforced | Spec invariant; trivial spatial index | Resources and terrain may need layered occupancy |
| Hub holds a copy of views | Network reads never contend with the sim | Memory is O(entities). Fine. |
| Sessions in memory, issued from config join tokens | Simplest auth that separates player and admin | Real accounts and persistence later |
| Full entity replacement in updates | Simple client merge | Attribute-level deltas if bandwidth matters |
| Admin mutations are ordinary scheduled commands | One code path for ordering, validation and replay | Admin can't act "between" ticks. Fine. |
| Cumulative cost/duration functions | Path-independent prices; embodied investment falls out for free; tables make costs exact for clients | Operators write cumulative formulas rather than per-level ones |
| Total integer formula semantics (x/0 = 0, saturation) | Formulas cannot fail mid-simulation, no float nondeterminism | A wrong formula yields odd values rather than an error; validation catches monotonicity, not intent |
| Upfront payment for upgrades and clones | Simple, clear rejection rule; no stalled half-paid projects | Large upgrades need storage, so `energy_capacity` becomes a prerequisite (intended) |
| Clone cost floor = embodied investment | Guarantees cloning can't launder upgrades or create energy | Cheap-clone strategies are impossible by construction |
| Lowest ID wins clone placement | Deterministic, never wastes the paid energy | The fixed (but random-looking) order favours some units per world |
| Kernel-level per-tick unit budget | Fairness and server protection apply to every plugin's commands | One budget for all command kinds; per-kind weights later if needed |
| Derived properties computed on demand | No cache invalidation; trivially deterministic | Per-call cost; observation and pacing re-evaluate formulas |
| Wells and energy are separate plugins | Wells (data and admin) shipped and tested before the economy existed | One more inter-plugin API (`wells.Get`/`Set`) |
| Movement depends on energy | Simplest way to charge for steps | A movement-only game must also load energy (cost 0 keeps it free) |
| Max-min fair split for contested wells | No first-come advantage, deterministic, conserves energy | Small integer remainders favour lower-ID claimants by at most 1 |
| Persistent transfer orders with a per-tick rate | Makes "transfer rate" meaningful | A one-shot transfer can take several ticks |
| Energy levels are owner-only | Hidden information for future sensors | Opponents see wells but not unit energy |

## Assumptions made (unspecified details)

- Tick 0 is the initial state, and the first executed tick is 1. A command
  accepted while tick *N* is being computed runs in tick *N+1*.
- Admin commands run before player commands in a tick. Players run in
  ascending ID order. Because player IDs are derived from the key, that
  order is fixed per world but doesn't favour any particular config slot.
- Player IDs are `DeriveID(id_key, "player", n)` truncated to 32 bits,
  skipping zero and duplicates. Config files refer to players by a `slot`
  number that is never exposed.
- If `id_key` is not configured, a random one is generated at startup. IDs
  then differ between server runs, which is fine because sessions don't
  survive restarts anyway.
- 8-directional movement, with diagonal steps costing the same as straight
  ones.
- Moving to the current tile completes immediately with `movement.arrived`.
- Join tokens are static per config, and anyone with a token plays that
  slot. Multiple concurrent sessions per player are allowed.
- Admin command results and admin-only events reach admins only, through
  `ObserveWorld`. Several admin sessions share one admin identity and see
  each other's results.
- `MoveEntity` (teleport) leaves movement, mining and transfer orders in
  place. Adjacency rules then apply from the new position.
- A mining order may target a distant well and waits until the unit is
  adjacent. Units that never reach an adjacent tile wait indefinitely; the
  player can stop them.
- A full miner keeps its order. Transfers end, rather than wait, when the
  receiver is full or the sender is empty.
- Admins may set any unit's energy and capacity (`SetUnitEnergyCommand`).
  This is the one sanctioned way to create or destroy energy outside
  mining and movement.
- Entity IDs inside attribute and event values (`energy.mining_well`,
  `energy.transfer_to`, `to`, `well`) are decimal strings, because
  `Value.int_value` is signed 64-bit.
- Initial entities appear as "changed" in tick 1's update. That's harmless,
  because clients start from a snapshot.

## Not implemented

These are frequently assumed but do **not** exist in the current code:

- Runtime (dynamic) plugin loading. Plugins are compiled in; see
  [plugins/overview.md](plugins/overview.md#compatibility-and-limitations).
- Persistence: save/load of world state, durable sessions, a replay
  database. Replays exist as exported files (Milestone 7), but matches live
  in memory: a restart starts a fresh world and loses unexported replays.
- Matchmaking, lobbies across servers, player accounts, rankings; more
  than one world per server process.
- Accounts, TLS, rate limiting beyond the per-tick unit-command budget.
- Teams, alliances and shared vision (owner = team).
- Terrain line of sight (`visibility.occlusion` accepts only `"none"`).
- Runtime rule changes (rates, formulas) through the admin API.
- Upgrade cancellation and refunds; more than one upgrade per unit at a
  time.
- Formations, pursuit outside a guard area, projectile or area damage.
- CI configuration, container images and deployment manifests are not part
  of the repository.

## Milestone 7 limitations

- **Resets need a new world.** A match can't be "rewound" or restarted
  in place; a reset rebuilds the game. Changing `players` or
  `admin_token` needs a server restart.
- **Player IDs are per match.** Clients must follow
  `MatchInfo.your_player_id` after a reset.
- **Disconnect policy is fixed** at "keep playing": units, orders and
  automation continue. A configurable alternative (pause, forfeit after a
  timeout) isn't implemented, because it would need server-driven admin
  commands with wall-clock timers.
- **Elimination is public.** With elimination victory, everyone learns
  that a player has lost all units: an intended rule-level disclosure.
  Without it, elimination isn't tracked.
- **Maps are validated, not balanced.** Spawn distance, reachable space,
  mutual reachability and nearby wells are guaranteed; fairness beyond
  that is not.
- **Replays are tied to the build**, kept in memory (current match plus
  the last 16), and capped at 64 MiB per gRPC message. Recording costs one
  digest per tick and a full state hash every 100 ticks.
- **Up to 16 players per match** (`match.MaxPlayers`). Victory conditions
  and metrics are compiled in.
- **The match status is sent every running tick** in `TickUpdate.match`,
  because the clock changes. That's small, but it adds to per-tick
  traffic.

## Known limitations

- **Fog at 25k units** exceeds the 10 Hz budget (see above).
- **Automation latency:** behaviors are evaluated every `eval_interval`
  ticks (default 5), so reactions (a guard spotting an enemy, a miner
  noticing a full battery) take up to that many ticks. Manual orders on an
  automated unit are overwritten at its next evaluation.
- **Guard** never pursues outside its area and does not prioritize
  threats beyond nearest-then-ID. **Follow** is to a single leader; there
  are no formations (out of scope for this milestone).
- **Logistics** splits are max-min fair per stage. Energy left unallocated
  because a receiver was full is not redistributed to the supplier's other
  links in the same tick (it is the next tick). Supply links only connect a
  player's own units.
- **Pathfinding** routes are at most 32 waypoints (longer routes replan
  when exhausted). Plans ignore moving units except during congestion
  replans, and there is no flow-field or hierarchical planning for very
  large maps.
- **Terrain** is static. Admins can delete features but not place new ones
  at runtime.

### Earlier limitations that still apply



- **No in-progress control:** upgrades can't be cancelled, and the energy
  paid for an interrupted upgrade or clone is lost.
- **Log noise:** batch failures produce one event per unit as well as the
  summary result. The client shows both, which is noisy for large batches.
- **Metadata scope:** offspring don't inherit metadata, and selectors match
  only the issuer's own units (no shared or team selectors).
- **Clone placement:** only adjacent tiles are considered, so a fully
  surrounded parent waits indefinitely, holding its paid energy.
- **Coarse budget:** the command budget counts units, not work. A 1-unit
  move and a 1-unit clone cost the same.
- **Typo protection:** attribute bindings are validated at startup, but an
  attribute that nothing uses (armor) is accepted silently.


## Performance (Milestone 6)

Measured with `make scale` (`internal/scale`, `cmd/rts-bench`); see the
README for the full table. The harness builds a lattice world at 25%
density with two interleaved players, issues each workload's standing
orders in an unmeasured setup tick, then measures `Engine.Step` (all
phases, including fog observation) over 30 ticks. Setups are seeded, and
`TestScenariosAreDeterministic` checks that identical runs produce
identical state hashes. `TestPerformanceRegression` fails on
order-of-magnitude regressions in allocations per tick (always) and p95
tick time (skipped under `-race`) at 1k units. `BenchmarkScale` gives
ns/tick figures for benchstat comparisons.

Optimizations, in order, each verified against the full suite:

1. **Kernel indexes** (start of the milestone): a dense occupancy grid
   instead of a position map, a per-tile passable index, cached sorted
   component membership, and memoized attribute output per component value.
   Fog with 4,000 units and dense vision went from 15.1 to 10.1 ms. The
   movement step went from 3.4 to 2.1 ms at 5k units and from 7.7 to 5.1 ms
   at 10k. Allocations roughly halved.
2. **Fog observation pass:** formula variables memoized per unit per pass
   (dense per-tile array); per-player scratch reused across ticks (no map
   or bitset rebuilt per tick); visibility marked by row spans with word
   operations; only occupied tiles are probed, with a fast empty-tile check
   for passable entities; the best candidate per unit target kept in a
   dense per-tile array. The output is byte-identical to the previous
   implementation (observation digests compared over 40 ticks of the
   4,000-unit fog scenario).
3. **Views:** views are built in a reused buffer and copied at exact size
   (they used to preallocate 32 attributes), and fog disclosure filters a
   scratch view rather than allocating a full one. Bytes allocated per tick
   fell 30–70% (sensors at 25k: 55 → 21 MB; mixed at 25k: 43 → 14 MB).

Steps 2 and 3 moved tick *time* by less than run-to-run noise. The cost
that remains is algorithmic, not allocation. One experiment was reverted:
a single slab per tick for all changed views raised heap and tick time.

**Remaining bottlenecks** (25k units, from profiles):

- **Fog of war:** about 70% of the `sensors` tick. The work is
  players × observers × tiles in vision (about 4.2M tile probes at
  vision 6) plus two formula evaluations per (observer, target) pair
  (about 1M). Every pass recomputes everything. Over budget at 25k (p50
  112 ms); fine at 10k (41 ms).
- **Change views:** every changed entity is re-serialized in full every
  tick (about 20 attributes). `mining` changes all 25k units every tick:
  about 40% of its tick, plus page-fault (`madvise`) cost from the
  allocation churn. GC pacing (`GOGC=400`) made no difference.
- **Event volume:** `energy.mined` fires per miner per tick (25k
  events/tick = 250k/s at 10 Hz). Combat in lockstep emits 16k events in
  one tick every cooldown, which causes the p95 spikes (`combat` p50 2.7 ms,
  p95 65 ms).
- **Entity lookups:** `World.Entity` is a hash-map lookup. It is about
  13% of the fog pass.

## Recommended scope for Milestone 8

1. **Persistence and operations.** Save and restore a world snapshot plus
   its command log, write replays to disk automatically at match end, and
   export metrics (tick time, players, match state).
2. **Incremental fog of war and attribute-level deltas**, the scale
   bottlenecks recorded in Milestone 6.
3. **Teams and shared vision** (alliances, team victory), built on the
   match plugin's player model.
4. **Configurable disconnect policy** and spectators, as read-only viewers
   of finished or running matches through the public API.
5. **Tournament tooling for bots:** scripted series of seeds, result
   aggregation, replay archives. Built on `ResetMatch`, `ExportReplay` and
   the bot-arena preset.

## Proposed next milestones (Milestone 6 recommendations)

Recommendations after Milestone 6 (earlier proposals below were largely
delivered by Milestones 4–6):

1. **Incremental fog of war.** Track observation-relevant changes
   (position, kind, sensor and signature stats) and recompute only the
   sectors within max vision of a change, reusing cached per-sector
   results elsewhere. A complementary option is per-player observation in
   parallel. That phase is derived, not authoritative, and players are
   independent, but it first needs per-worker scratch and read-only access
   to kernel memo caches.
2. **Attribute-level deltas.** Send only changed attributes per entity
   (with a per-subscriber baseline) instead of full views. This cuts
   `takeChanges`, hub fan-out and network volume.
3. **Event aggregation.** Opt-in per-tick summaries for high-frequency
   events (mining, transfers, damage), and jittered combat cooldowns.
4. **Dense entity storage.** A slot index per entity, so tile probes and
   lookups avoid hash maps.
5. **Operations.** Replay files and a replay CLI, save and load, runtime
   rule changes, metrics (tick time, per-phase work, events) exported for
   monitoring.
