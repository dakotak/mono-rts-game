# World generation and custom generators

Source: [`internal/plugins/worldgen`](../../../internal/plugins/worldgen/worldgen.go)
and [`generators.go`](../../../internal/plugins/worldgen/generators.go).
Working example: [`examples/worldgen/cross`](../../../examples/worldgen/cross/cross.go)
(with tests).

## How generation works

The `worldgen` plugin runs in its `Init` hook, after config units exist
and after the terrain and wells plugins have placed their configured
entities. With `"generator": "none"` (the default) it does nothing.
Otherwise it runs these stages:

| # | Stage | Who |
|---|---|---|
| 0 | **Spawn regions**: one anchor per player from `spawns.layout` (`corners`, `ring` or `random`) | worldgen |
| 1 | **Terrain** | `Generator.Terrain` |
| 2 | **Resources** (wells) | `Generator.Resources` |
| 3 | **Starting positions** (exact spawn tiles) | `Generator.Spawns` |
| 4 | **Units**: `start.units` per player on the reachable free tiles nearest their spawn | worldgen |
| 5 | **Validation**: spawn distance ≥ `spawns.min_distance`; ≥ `start.min_free_tiles` reachable tiles from each spawn; all spawns mutually reachable; ≥ `wells.per_spawn` accessible wells within `wells.near_distance` of each spawn | worldgen |
| 6 | **Publish**: apply the plan to the world (terrain, wells, units, starting energy) | worldgen |

Stages 1–5 work on a **plan** over a grid model (`worldgen.Context`), not
on the world. An invalid attempt is simply discarded and the stages run
again, with the RNG continuing so each attempt differs, up to
`max_attempts` times. Then setup fails:

```
worldgen procedural: no valid world after 10 attempts: validate: spawns 1 and 2 are 12 apart (< 14)
```

`game.New`, `rts-server -check` and `ResetMatch` report this error. A
failed reset leaves the current match running.

**Determinism.**

- All randomness is `c.Rand`, the world's `sim.Rand`, seeded from
  `world.seed`. The same seed and configuration produce the same world,
  bit for bit (tested).
- Generation uses integer geometry only: no trigonometry or floats, whose
  results could differ across CPU architectures and break cross-platform
  replays.
- A random seed per match (`world.random_seed`) is drawn once, recorded
  (`Game.Seed`, match info, results, replays), and the world is then
  generated from it like any seed.

## Configuration

```json
"worldgen": {
  "generator": "procedural",
  "params": { "obstacle_type": "rock", "obstacle_percent": 10, "cluster_size": 8, "rough_type": "marsh", "rough_percent": 5, "clear_radius": 5 },
  "spawns": { "layout": "random", "min_distance": 36, "margin": 4 },
  "wells": { "per_spawn": 2, "near_distance": 7, "extra": 6, "well": { "energy": 600, "capacity": 600, "regeneration": 0 } },
  "start": { "units": 6, "energy": 40, "min_free_tiles": 80 },
  "max_attempts": 30
}
```

Terrain types (`rock`, `marsh`) must be defined in the `terrain`
section. Every field is listed in the
[configuration reference](../../reference/configuration.md#plugin-sections).

### Built-in generators

| Generator | Map |
|---|---|
| `arena` | Optional border walls (`params.walls`, `wall_type`). Home wells near each spawn, and `wells.extra` wells evenly spaced on a rectangle around the center. Deterministic geometry: the seed only affects home-well positions. |
| `procedural` | Random-walk clusters of `obstacle_type` (`obstacle_percent` of the map) and `rough_type` patches, kept `clear_radius` away from spawns. Home wells, then `wells.extra` scattered wells. |

Neither guarantees a perfectly balanced map. They guarantee what
validation checks: distance, space, reachability and nearby energy.

## Writing a generator

Implement `worldgen.Generator`:

```go
type Generator interface {
    Name() string                           // selected by "worldgen.generator"
    Configure(params json.RawMessage) error // decode "worldgen.params" (strictly); called once at registration
    Terrain(c *worldgen.Context) error
    Resources(c *worldgen.Context) error
    Spawns(c *worldgen.Context) error       // must call c.SetSpawns with one tile per player
}
```

What the `Context` gives you:

| Member | Use |
|---|---|
| `Width`, `Height`, `Players`, `Attempt`, `Config` | Dimensions, player count, retry number, the worldgen config |
| `Rand` | The **only** randomness you may use |
| `Anchors` | Spawn regions, one per player (computed before `Terrain`) |
| `InBounds(p)`, `Empty(p)` (nothing there), `Free(p)` (no blocking entity), `Walkable(p)` | Grid queries over the plan *and* pre-existing entities |
| `NearAnchor(p, r)`, `NearestFree(p, r)`, `RandomPos(margin)` | Placement helpers |
| `AddTerrain(type, p)`, `AddWell(p)`, `SetSpawns(ps)` | Build the plan. `AddWell` uses the configured well template. |

Shared helpers: `worldgen.PlaceHomeWells(c)` (per-spawn wells honoring
`wells.per_spawn` and `near_distance`) and `worldgen.SpawnsAtAnchors(c)`.

The example generator draws two crossing walls with passages, then
reuses both helpers:

```go
func (g *Generator) Terrain(c *worldgen.Context) error {
    cx, cy := c.Width/2, c.Height/2
    for y := range c.Height {
        if p := (sim.Pos{X: cx, Y: y}); !g.inGap(y, c.Height) && c.Empty(p) && !c.NearAnchor(p, 2) {
            if err := c.AddTerrain(g.Params.WallType, p); err != nil {
                return err
            }
        }
    }
    // … the horizontal wall likewise …
    return nil
}

func (g *Generator) Resources(c *worldgen.Context) error {
    if err := worldgen.PlaceHomeWells(c); err != nil {
        return err
    }
    if p, ok := c.NearestFree(sim.Pos{X: c.Width/2 + 1, Y: c.Height/2 + 1}, 3); ok {
        return c.AddWell(p)
    }
    return nil
}

func (g *Generator) Spawns(c *worldgen.Context) error { return worldgen.SpawnsAtAnchors(c) }
```

Rules:

- **Return an error to reject an attempt.** For example, when you can't
  place something. Worldgen retries with fresh randomness.
- **Never touch the world** from a generator. Everything goes through the
  `Context`.
- **Keep work bounded.** Use loops with fixed try limits, as the helpers
  do.

## Registering a generator

Generators register with the shared worldgen plugin in
[`game.Plugins()`](../../../internal/game/game.go), before the ruleset is
built:

```diff
 	terrainP, wellsP := terrain.New(), wells.New()
+	wg := worldgen.New(terrainP, wellsP)
+	wg.AddGenerator(&cross.Generator{})
 	return []sim.Plugin{
 		match.New(attrs, fighting),
-		worldgen.New(terrainP, wellsP),
+		wg,
```

Then select it in the configuration:

```json
"worldgen": { "generator": "cross", "params": { "wall_type": "rock", "gap": 3 }, "spawns": { "layout": "corners" } }
```

An unknown generator name fails startup and lists the available ones.

For tests and tools, register without editing `game.Plugins()`:

```go
plugins := game.Plugins()
wg, _ := game.Find[*worldgen.Plugin](plugins)
wg.AddGenerator(&cross.Generator{})
g, err := game.NewWithPlugins(cfg, plugins)
```

[`examples/worldgen/cross/cross_test.go`](../../../examples/worldgen/cross/cross_test.go)
does exactly this and checks the walls, passages, unit counts and
determinism.

## Limitations

- One generator per match. Stages can't be mixed across generators except
  through the shared helpers.
- Validation is fixed: distance, space, reachability, wells. A generator
  can reject attempts itself by returning an error, but can't add
  validation rules to the built-in step.
- Generated terrain is static (as all terrain is). There are no biomes,
  heightmaps or line of sight.
- Spawns are assigned to players in configuration order.
