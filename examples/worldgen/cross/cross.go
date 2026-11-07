// Package cross is an example world generator: two walls cross at the map
// center, each with passages, dividing the map into four quadrants. Home
// wells and spawns use the worldgen helpers; one extra well sits next to
// the center as a contested prize.
//
// Register it with the worldgen plugin before the ruleset is built (see
// docs/developer-guide/plugins/worldgen.md) and select it with
// "worldgen": {"generator": "cross", "params": {"wall_type": "rock", "gap": 3}}.
package cross

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/worldgen"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Generator implements worldgen.Generator.
type Generator struct {
	Params struct {
		WallType string `json:"wall_type"` // an impassable terrain type
		Gap      int32  `json:"gap"`       // passage width at each quarter point
	}
}

func (*Generator) Name() string { return "cross" }

// Configure decodes "params" strictly.
func (g *Generator) Configure(raw json.RawMessage) error {
	g.Params.WallType, g.Params.Gap = "rock", 3
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&g.Params); err != nil {
			return err
		}
	}
	if g.Params.Gap < 1 {
		return fmt.Errorf("gap must be >= 1")
	}
	return nil
}

// inGap reports whether coordinate v (along a wall of length n) falls in a
// passage centered on the wall's quarter points.
func (g *Generator) inGap(v, n int32) bool {
	for _, q := range []int32{n / 4, 3 * n / 4} {
		if v >= q-g.Params.Gap/2 && v < q-g.Params.Gap/2+g.Params.Gap {
			return true
		}
	}
	return false
}

// Terrain draws both walls, skipping passages and spawn regions.
func (g *Generator) Terrain(c *worldgen.Context) error {
	cx, cy := c.Width/2, c.Height/2
	for y := range c.Height {
		if p := (sim.Pos{X: cx, Y: y}); !g.inGap(y, c.Height) && c.Empty(p) && !c.NearAnchor(p, 2) {
			if err := c.AddTerrain(g.Params.WallType, p); err != nil {
				return err
			}
		}
	}
	for x := range c.Width {
		if p := (sim.Pos{X: x, Y: cy}); !g.inGap(x, c.Width) && c.Empty(p) && !c.NearAnchor(p, 2) {
			if err := c.AddTerrain(g.Params.WallType, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// Resources: home wells near every spawn, plus one well beside the center.
func (g *Generator) Resources(c *worldgen.Context) error {
	if err := worldgen.PlaceHomeWells(c); err != nil {
		return err
	}
	if p, ok := c.NearestFree(sim.Pos{X: c.Width/2 + 1, Y: c.Height/2 + 1}, 3); ok {
		return c.AddWell(p)
	}
	return nil
}

// Spawns: the free tile nearest each spawn region.
func (g *Generator) Spawns(c *worldgen.Context) error { return worldgen.SpawnsAtAnchors(c) }
