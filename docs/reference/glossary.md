# Glossary

| Term | Meaning |
|---|---|
| **Admin** | The privileged principal behind `AdminService` (`sim.Principal{Admin: true}`). Its commands run first in a tick and aren't budget-charged. |
| **Attribute (observable)** | A `<plugin>.<name>` key/value on an entity view, produced by a component and tagged admin, owner or public. |
| **Attribute (unit stat)** | A configurable, upgradable stat managed by the attributes plugin (armor, propulsion, ...). Seen as `attributes.<name>`. |
| **Audience** | Who receives an event: one player (`ToPlayer`), everyone (`ToAll`), or admins only. |
| **Batch command** | `rts.metadata.v1.BatchCommand`: applies a per-unit command template to the units a selector matches at execution time. |
| **Binding** | Config that makes a mechanic read a stat from an attribute or derived property (e.g. `movement.pace_attribute`). |
| **Budget (unit-command budget)** | Unit-commands each player may execute per tick (`world.unit_command_budget`). |
| **Chebyshev distance** | `max(|dx|, |dy|)`: diagonal neighbors are distance 1. Used for adjacency, ranges and default vision. |
| **Command** | A request to change the world, executed at a scheduled tick. Distinct from its acknowledgment. |
| **CommandResult** | The outcome of an executed command: `SUCCEEDED`, `FAILED` (with code) or `PARTIAL` (per-unit results). |
| **Component** | Typed per-entity state owned by a plugin (`sim.Component[T]`). |
| **Derived property** | A value computed on demand from state (`World.Derived`), e.g. `movement.ticks_per_move`. |
| **Detection** | Whether a player perceives a foreign entity under fog of war (detection formula ≥ threshold). |
| **Disclosure rule** | `visibility.disclosure` entry: how precisely an attribute is shown to other players at each identification level. |
| **Embodied investment** | The energy value of a unit's attribute levels above default: `Σ cost(level) − cost(default)`. |
| **Entity** | Anything on the grid: unit, resource (well, wreck) or terrain. Identified by an opaque 64-bit ID. |
| **Event** | A notification emitted during a tick (`<plugin>.<name>`), delivered to its audience. Not state. |
| **Fog of war** | Per-player visibility, detection and identification (`visibility.enabled`). |
| **Frame** | Everything one tick produced: changed views, removals, events, results, commands, observations. |
| **Ghost** | Client-side memory of a foreign entity no longer observed. |
| **Hub** | The server component that keeps a read-only copy of views and per-player observations, and fans out updates. |
| **Identification level** | How well a detected entity is known: basic, estimated or detailed (`OWN` for your own). |
| **Invested** | Energy actually paid into a unit's levels (upgrades, plus embodied energy inherited by a clone). |
| **Join token** | Config secret that lets a client connect as a player slot. |
| **Max-min fair split** | Dividing a limited amount: smallest requests are filled first, the rest share evenly, ties by ID (`fairshare.Split`). |
| **Observation authority** | The single plugin (visibility) deciding what each player observes of foreign entities. |
| **Order** | Persistent per-unit intent stored as a component (move, mine, attack, upgrade, supply, ...) and advanced by a system each tick. |
| **Passable entity** | An entity that doesn't block its tile (wreckage, passable terrain). |
| **Phase** | A named stage of a tick. Phases are ordered by `After` and `Before` constraints and contain systems. |
| **Player ID** | Opaque 32-bit identifier derived from the ID key. Entities' `owner_id`. |
| **Plugin** | A compiled-in package implementing `sim.Plugin` that contributes a mechanic. |
| **Registrar** | The object a plugin declares its contributions through during `Register`. |
| **Relay** | A unit that both receives and supplies energy through supply links, forming chains. |
| **Rules description** | A plugin's published effective configuration (`WorldInfo.rules_json`). |
| **Ruleset** | The validated, immutable combination of registered plugins (`sim.Ruleset`). |
| **Selector** | Metadata conditions (plus optional IDs and a limit) choosing your own units for a batch. |
| **Session token** | Bearer token returned by `Connect`, valid until server restart. |
| **Snapshot** | A complete view at a tick (`WorldSnapshot`). |
| **State hash** | `World.StateHash()`: a hash of all authoritative state, used to prove determinism. |
| **Supply link** | Persistent logistics order moving energy from a supplier to a receiver within range. |
| **System** | A function run once per tick inside a phase. |
| **Tick** | One fixed simulation step. Tick 0 is the initial state. |
| **TickUpdate** | Per-tick delta for one viewer. Applying it to the previous snapshot gives the next. |
| **Well** | Resource entity holding extractable energy; finite or unlimited. |
| **Wreck (wreckage)** | Passable resource left by a destroyed unit, holding recoverable energy. |
