# Configuring a world

A world is defined by one JSON file passed to `rts-server -config`. The
file is parsed strictly: **unknown fields are errors** at the top level and
inside every plugin section, so a typo fails at startup instead of quietly
falling back to a default. Every field, default and limit is listed in the
[configuration reference](../reference/configuration.md). This page explains
the structure and shows worked examples.

## Structure

```json
{
  "world":   { "width": 32, "height": 20, "seed": 42, "tick_rate_hz": 10, "unit_command_budget": 500 },
  "players": [ { "slot": 1, "name": "Red", "join_token": "red" },
               { "slot": 2, "name": "Blue", "join_token": "blue" } ],
  "admin_token": "dev-admin",
  "units":   [ { "owner": 1, "x": 2, "y": 3, "w": 4, "h": 3 } ],
  "plugins": { "energy": { "initial_energy": 30 } }
}
```

| Section | Parsed by | Purpose |
|---|---|---|
| `world` | [`game.WorldConfig`](../../internal/game/game.go) | Size, seed, tick rate, ID key, command budget. |
| `players` | `game.PlayerConfig` | Player slots and their join tokens. |
| `admin_token` | `game.Config.AdminToken` | Enables `AdminService` (empty disables it). |
| `units` | `game.UnitBlock` | Initial units, as rectangles of units per owner slot. |
| `plugins` | each plugin's `Register` via `Registrar.DecodeConfig` | One section per plugin, keyed by plugin name. |

How loading works ([`game.LoadConfig`](../../internal/game/game.go) →
[`game.New`](../../internal/game/game.go)):

1. The file is decoded with `DisallowUnknownFields`, and `Config.Validate`
   checks the world, slots, tokens and owners.
2. `sim.NewRuleset` registers every compiled-in plugin in dependency
   order. Each plugin strictly decodes its own section (over its defaults)
   and validates it. A section for a plugin that doesn't exist is an error.
3. The engine is created, the `units` blocks are placed, and plugins place
   their own initial entities (wells, terrain) in `Init`.

Any error stops the server with a message naming the field, for example
`plugin "movement" config: json: unknown field "tick_per_move"`.

**Plugins are always present.** Every built-in plugin is compiled in and
registered whether or not it has a config section. A missing section means
"use the defaults". Some mechanics are inert until configured:

- fog of war needs `visibility.enabled: true`
- there are no wells until `wells.wells` lists some
- there is no terrain until `terrain.tiles` lists some

## Players and identity

- `slot` is a number local to the config file, used for `units[].owner`.
  It is never sent to clients.
- A player's public ID is derived from the world's ID key. Anyone who
  presents a `join_token` plays as that slot, and several sessions per
  player are allowed.
- `world.id_key` (hex, at least 16 bytes) makes IDs reproducible across
  runs. Leave it empty in production: a random key is generated at startup.
  Anyone holding the key can predict entity IDs.

## Worked examples

### A minimal world

No wells, no fog, all defaults. Units can move and fight with default
stats.

```json
{
  "world": { "width": 40, "height": 40, "seed": 1, "tick_rate_hz": 10 },
  "players": [ { "slot": 1, "name": "A", "join_token": "token-a" },
               { "slot": 2, "name": "B", "join_token": "token-b" } ],
  "admin_token": "change-me",
  "units": [ { "owner": 1, "x": 2, "y": 2, "w": 3, "h": 3 },
             { "owner": 2, "x": 35, "y": 35, "w": 3, "h": 3 } ]
}
```

### An energy economy

Three wells, movement costs energy, and units start with a little.

```json
"plugins": {
  "wells": {
    "defaults": { "energy": 500, "capacity": 500, "extraction_rate": 4, "unlimited": false },
    "wells": [ { "x": 20, "y": 20 }, { "x": 10, "y": 30, "unlimited": true, "extraction_rate": 6 } ]
  },
  "energy":   { "unit_capacity": 100, "initial_energy": 30, "extraction_rate": 2, "transfer_rate": 5 },
  "movement": { "ticks_per_move": 2, "energy_per_step": 1 }
}
```

### Upgradable units

Attributes are defined entirely in config. Mechanics read them only where
you **bind** them:

```json
"attributes": {
  "base_mass": 10,
  "attributes": {
    "propulsion":        { "default": 1, "min": 1, "max": 10, "mass": "v", "cost": "(v - 1) * (v - 1) * 20", "duration": "(v - 1) * 10" },
    "energy_capacity":   { "default": 100, "min": 100, "max": 400, "mass": "v / 50", "cost": "(v - 100) * (v - 100) / 40 + (v - 100)", "duration": "(v - 100) / 5" },
    "mining_efficiency": { "default": 2, "min": 1, "max": 10, "mass": "v", "cost": "pow(v, 3) * 2", "duration": "v * 10" }
  },
  "derived": { "ticks_per_move": "clamp(mass / (propulsion * 8), 1, 20)" }
},
"energy":   { "capacity_attribute": "energy_capacity", "extraction_attribute": "mining_efficiency" },
"movement": { "pace_attribute": "ticks_per_move" }
```

`cost` and `duration` are **cumulative** formulas of the level `v` (see
[formulas](../reference/formulas.md)). Heavier units move slower here
because pace is derived from `mass`.

### Fog of war

```json
"visibility": {
  "enabled": true,
  "vision": { "default": 4 }, "sensor": { "default": 4 }, "identification": { "default": 1 },
  "disclosure": [ { "key": "combat.health", "basic": 50, "estimated": 10, "detailed": 1 } ]
}
```

Foreign attributes are hidden unless a `disclosure` rule lets them through.
Here enemy health is shown as a range 50 wide at basic identification, 10
wide when estimated, and exact when detailed. See
[visibility](../user-guide/visibility.md).

### Terrain, automation and logistics

```json
"terrain": {
  "types": { "rock": { "passable": false }, "marsh": { "passable": true, "cost": 3 } },
  "tiles": [ { "type": "rock", "x": 13, "y": 7, "w": 1, "h": 7 }, { "type": "marsh", "x": 20, "y": 10, "w": 3, "h": 4 } ]
},
"automation": { "eval_interval": 5, "max_evaluations_per_tick": 2000 },
"logistics":  { "range": { "default": 2 } }
```

## Matches and presets

The `worldgen` and `match` sections turn a world into a complete game:

- a generated map with spawns and starting units
- a lobby
- victory conditions and scoring

Three presets in [`configs/presets`](../../configs/presets) (sandbox,
standard, bot arena) are complete examples, documented in
[running matches](../operations/matches.md#presets).

## The complete development config

[`configs/dev.json`](../../configs/dev.json) uses every plugin and is the
best real example. It defines 12 attributes, combat bound to attributes,
fog with disclosure rules, terrain, and the automation and logistics
settings.

## Checking a config

Start the server with it. Config errors are fatal and printed before
anything listens:

```bash
go run ./cmd/rts-server -config my-world.json
```

The startup log then prints the plugin order and the tick schedule. Players
and admins can read the effective rules (each plugin's configuration after
defaults) from `WorldInfo.rules_json`. The admin client shows them under
**Rules**.
