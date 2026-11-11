# Mono documentation

Everything here describes the code as it is in this repository (through
Milestone 8). Where something is planned but not built, the page says so.
[Design notes](developer-guide/design-notes.md#not-implemented) keeps the
list of what isn't implemented.

## Getting started

- [Quickstart](getting-started/quickstart.md): run a local game and play a
  first match in the browser.
- [Installation](getting-started/installation.md): toolchain, building,
  binaries and flags.
- [Configuration](getting-started/configuration.md): how a world is
  configured, with worked examples.

## User guide (players)

- [Concepts](user-guide/concepts.md): the world, ticks, the single unit
  type, information.
- [Gameplay and the web client](user-guide/gameplay.md): controls, a first
  match, strategy.
- [Units, movement and groups](user-guide/units.md)
- [Energy](user-guide/energy.md): wells, storage, mining, transfers.
- [Upgrades and cloning](user-guide/upgrades.md)
- [Combat and salvage](user-guide/combat.md)
- [Visibility, sensors and intelligence](user-guide/visibility.md)
- [Automation and logistics](user-guide/automation.md)
- [Matches: lobby, victory, scoring and results](user-guide/matches.md)

## API and bots

- [API overview](api/overview.md): services, the command lifecycle, keeping
  client state.
- [GameService](api/game-service.md) · [AdminService](api/admin-service.md)
- [Authentication](api/authentication.md) · [Streaming and synchronization](api/streaming.md)
- [Bot development](api/bot-development.md): a working Go bot and a Python
  client.

## Computer player

- [Computer player](computer-player/README.md): running `mono-bot`,
  configuration, difficulty levels, personalities, diagnostics, adding a
  computer opponent from the web client.
- [Decision architecture](computer-player/architecture.md): world model,
  strategic / operational / tactical layers, command management.
- [Bot-vs-bot evaluation and performance](computer-player/evaluation.md)
- [Extending the AI](computer-player/extending.md): heuristics,
  objectives, personalities, new difficulty parameters, testing.

## Developer guide

- [Architecture](developer-guide/architecture.md): packages, processes,
  data flow (diagrams).
- [Simulation](developer-guide/simulation.md): the tick lifecycle, command
  scheduling, conflict resolution, concurrency.
- [Entities and components](developer-guide/entities.md)
- [Attributes and derived properties](developer-guide/attributes.md)
- [Commands](developer-guide/commands.md) · [Events and synchronization](developer-guide/events.md)
- [Determinism and replay](developer-guide/determinism.md)
- [Testing and benchmarking](developer-guide/testing.md)
- [Contributing](developer-guide/contributing.md)
- [Mechanics rules reference](developer-guide/mechanics.md): the exact rules
  of every built-in mechanic.
- [Replay format and reproducibility](developer-guide/replay.md)
- [Design notes, limitations and roadmap](developer-guide/design-notes.md)

### Plugin development

- [Overview](developer-guide/plugins/overview.md): what a plugin is,
  extension points, compatibility and limitations.
- [Tutorial: build a plugin](developer-guide/plugins/getting-started.md):
  step by step, from package to running server.
- [Interfaces](developer-guide/plugins/interfaces.md) · [Registration](developer-guide/plugins/registration.md) · [Lifecycle](developer-guide/plugins/lifecycle.md)
- [Commands and events](developer-guide/plugins/commands-and-events.md) · [Attributes and formulas](developer-guide/plugins/attributes-and-formulas.md)
- [State and determinism rules](developer-guide/plugins/state-and-determinism.md)
- [Testing plugins](developer-guide/plugins/testing.md) · [Examples](developer-guide/plugins/examples.md)
- [World generators](developer-guide/plugins/worldgen.md) · [Victory conditions and scoring rules](developer-guide/plugins/match-extensions.md)

## Operations

- [Deployment](operations/deployment.md): processes, ports, flags,
  hardening, limits.
- [Administration](operations/administration.md): the admin client and
  API, pause/step, world edits.
- [Running matches](operations/matches.md): presets, starting and
  resetting, bot matches, replays.
- [Troubleshooting](operations/troubleshooting.md)

## Reference

- [Configuration](reference/configuration.md): every option, default and
  valid range.
- [Commands](reference/commands.md): every command and error code.
- [Events and phases](reference/events.md)
- [Formulas and attributes](reference/formulas.md)
- [Glossary](reference/glossary.md)
