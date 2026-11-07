# Events and phases reference

Extracted from the plugins' `ctx.Emit` calls. "Owner" means the owner of
the event's entity. Admins receive every event. Entity IDs in fields are
decimal strings.

## Simulation phases

Per-tick order with every built-in plugin (`Ruleset.Schedule()`, also
`WorldInfo.schedule`):

| # | Phase | Systems | Declared by and constraints |
|---|---|---|---|
| 0 | *(commands)* | every queued command, in (admin, player ID, seq) order; each passes the match gate first | kernel |
| 1 | `automation` | `automation.evaluate` | automation (`Before: energy`) |
| 1b | `wells` | `wells.regenerate` | wells (energy declares `After: wells`) |
| 2 | `energy` | `energy.mine`, `energy.transfer` | energy (`After: wells`) |
| 3 | `logistics` | `logistics.supply` | logistics (`After: energy`) |
| 4 | `salvage` | `salvage.collect` | salvage (`After: energy`, `Before: movement`) |
| 5 | `movement` | `movement.step` | movement (`After: energy`) |
| 6 | `combat` | `combat.resolve` | combat (`After: movement`, `Before: upgrades, cloning`) |
| 7 | `upgrades` | `upgrades.progress` | upgrades (`After: energy`) |
| 8 | `cloning` | `cloning.progress` | cloning (`After: upgrades`) |
| 8b | `match` | `match.evaluate` (stats, elimination, victory, scores) | match (`After: combat, cloning, salvage, logistics`) |
| 9 | *(observation)* | `visibility` `Observations` | kernel calls the observation authority |

Phases run only while the match is RUNNING (the match plugin is the
kernel's gate). Commands and observation run every tick.

Ties between unconstrained phases are broken by name, which is how
`logistics` lands before `salvage`.

## Kernel (admin only)

| Event | Fields | When |
|---|---|---|
| `sim.entity_created` | `command_id`, `position` | `SpawnUnit` executed |
| `sim.entity_removed` | `command_id` | `DeleteEntity` executed |
| `sim.entity_moved` | `command_id`, `position` | `MoveEntity` executed |

## Movement

| Event | Audience | Fields | When |
|---|---|---|---|
| `movement.arrived` | Owner | `position` | Reached the destination |
| `movement.cancelled` | Owner | `destination`, `reason` | Gave up: blocked for `max_blocked_ticks` steps, or `unreachable` |
| `movement.stalled` | Owner | `reason`, `required`, `stored` | Started waiting for energy to pay for a step (once per episode) |

## Energy

| Event | Audience | Fields | When |
|---|---|---|---|
| `energy.mined` | Owner | `amount`, `well` | Each tick a miner gains energy |
| `energy.mining_stopped` | Owner | `reason` (`well_depleted`, `well_removed`, ...), `well` | A mining order ended |
| `energy.well_depleted` | Players whose units drained the well | — (entity = the well) | A finite well reached 0 |
| `energy.transferred` | Sender's owner | `to`, `amount` | Each tick a transfer moves energy |
| `energy.transfer_ended` | Owner | `to`, `reason` (`completed`, `sender_empty`, `receiver_full`, `not_adjacent`, `target_lost`), `remaining` | A transfer order ended |

## Upgrades and cloning

| Event | Audience | Fields |
|---|---|---|
| `upgrades.started` | Owner | `attribute`, `from`, `to`, `cost`, `duration` |
| `upgrades.progress` | Owner | `attribute`, `done`, `duration` (every `progress_event_interval` ticks) |
| `upgrades.completed` | Owner | `attribute`, `value`, `cost` |
| `upgrades.failed` | Owner (issuer's unit) | `attribute`, `reason`, `message` |
| `cloning.started` | Owner | `cost`, `duration`, `offspring_energy` |
| `cloning.progress` | Owner | `done`, `duration` |
| `cloning.blocked` | Owner | `reason` (no free tile; emitted once) |
| `cloning.completed` | Owner | `child`, `cost`, `position` |
| `cloning.failed` | Owner (issuer's unit) | `reason`, `message` |

## Combat and salvage

| Event | Audience | Fields |
|---|---|---|
| `combat.attack` | Attacker's owner | `target`, `damage`, `cost` |
| `combat.damaged` | Target's owner | `damage`, `health`, `attackers` |
| `combat.destroyed` | Owner and the attacking players | `owner`, `position`, `reason` |
| `combat.attack_ended` | Attacker's owner | `target`, `reason` (`target_lost`, `target_unavailable`, `incapable`) |
| `combat.out_of_range` | Attacker's owner | `target`, `distance`, `range` (once per episode) |
| `combat.stalled` | Attacker's owner | `required`, `stored` (once per episode) |
| `salvage.wreck_created` | Destroyed unit's owner and its attackers' owners | `unit`, `energy`, `position` |
| `salvage.collected` | Collector's owner | `wreck`, `amount` |
| `salvage.wreck_depleted` | Owners of the collectors that emptied it | — |
| `salvage.ended` | Collector's owner | `wreck`, `reason` |

## Visibility (fog of war only)

| Event | Audience | Fields |
|---|---|---|
| `visibility.discovered` | The observing player | `level` (1 basic, 2 estimated, 3 detailed) |
| `visibility.identified` | The observing player | `level`, `previous` |
| `visibility.lost` | The observing player | — (no longer detected; destruction looks the same) |

## Automation and logistics

| Event | Audience | Fields |
|---|---|---|
| `automation.state` | Owner | `behavior`, `state` |
| `automation.blocked` | Owner | `behavior`, `reason` |
| `automation.failed` | Owner | `behavior`, `reason` (the behavior is removed) |
| `automation.work` | Admins | `evaluated`, `deferred` (per tick) |
| `logistics.status` | Supplier's owner | `receiver`, `status` (on change) |
| `logistics.ended` | Supplier's owner | `receiver`, `reason` (`receiver_lost`) |
| `logistics.work` | Admins | `links`, `moved` (per tick, when links were active) |

## Match

| Event | Audience | Fields |
|---|---|---|
| `match.state` | Everyone | `state`, `previous`, `reason` (`all_ready`, `admin_start`, `admin_pause`, `admin_resume`, `last_player_standing`, `simultaneous_elimination`, `time_limit`, `admin`, custom) |
| `match.ready` | Everyone | `ready_count`, `expected` (no identities) |
| `match.eliminated` | Everyone | `player`, `reason` (`no_units`, `admin`) |
| `match.finished` | Everyone | `winner`, `draw`, `reason`, `run_ticks` |
| `match.score` | The player (everyone if scores are public) | `player`, `score` (every `scoring.interval_ticks`) |

Match state also travels as `MatchInfo` in snapshots and updates (see
[GameService](../api/game-service.md#getmatch)).

## Example plugins (not in the default build)

| Event | Audience | Fields |
|---|---|---|
| `regeneration.regenerated` | Owner | `amount`, `stored` |

`examples/plugins/koth` emits no events; its outcome appears as the match
result reason `king_of_the_hill`.
