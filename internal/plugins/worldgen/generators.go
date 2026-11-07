package worldgen

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Arena is a simple, mostly geometric map: optional border walls, home
// wells near every spawn, extra wells evenly spaced on a ring around the
// center, and spawns at the layout anchors.
type Arena struct {
	Params struct {
		// Walls surrounds the map with WallType terrain.
		Walls    bool   `json:"walls"`
		WallType string `json:"wall_type"`
	}
}

func (*Arena) Name() string { return "arena" }

func (a *Arena) Configure(raw json.RawMessage) error {
	a.Params.WallType = "rock"
	return decodeParams(raw, &a.Params)
}

func (a *Arena) Terrain(c *Context) error {
	if !a.Params.Walls {
		return nil
	}
	for x := range c.Width {
		for _, y := range []int32{0, c.Height - 1} {
			if p := (sim.Pos{X: x, Y: y}); c.Empty(p) {
				if err := c.AddTerrain(a.Params.WallType, p); err != nil {
					return err
				}
			}
		}
	}
	for y := int32(1); y < c.Height-1; y++ {
		for _, x := range []int32{0, c.Width - 1} {
			if p := (sim.Pos{X: x, Y: y}); c.Empty(p) {
				if err := c.AddTerrain(a.Params.WallType, p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *Arena) Resources(c *Context) error {
	if err := PlaceHomeWells(c); err != nil {
		return err
	}
	// Extra wells on the perimeter of a rectangle at half size around the
	// center, evenly spaced (integer geometry).
	n := int64(c.Config.Wells.Extra)
	if n == 0 {
		return nil
	}
	cx, cy := c.Width/2, c.Height/2
	hw, hh := max(c.Width/4, 1), max(c.Height/4, 1)
	w, h := int64(2*hw), int64(2*hh)
	per := 2 * (w + h)
	for i := range n {
		o := i * per / n
		var p sim.Pos
		switch {
		case o < w:
			p = sim.Pos{X: cx - hw + int32(o), Y: cy - hh}
		case o < w+h:
			p = sim.Pos{X: cx + hw, Y: cy - hh + int32(o-w)}
		case o < 2*w+h:
			p = sim.Pos{X: cx + hw - int32(o-w-h), Y: cy + hh}
		default:
			p = sim.Pos{X: cx - hw, Y: cy + hh - int32(o-2*w-h)}
		}
		if q, ok := c.NearestFree(p, 3); ok {
			_ = c.AddWell(q)
		}
	}
	return nil
}

func (a *Arena) Spawns(c *Context) error { return SpawnsAtAnchors(c) }

// Procedural is a seeded random map: clusters of impassable obstacles,
// patches of rough terrain, home wells near every spawn and extra wells
// scattered elsewhere. Spawn regions are kept clear.
type Procedural struct {
	Params struct {
		ObstacleType    string `json:"obstacle_type"`
		ObstaclePercent int    `json:"obstacle_percent"`
		ClusterSize     int    `json:"cluster_size"`
		RoughType       string `json:"rough_type"`
		RoughPercent    int    `json:"rough_percent"`
		// ClearRadius keeps this Chebyshev radius around spawn regions
		// free of terrain.
		ClearRadius int `json:"clear_radius"`
	}
}

func (*Procedural) Name() string { return "procedural" }

func (g *Procedural) Configure(raw json.RawMessage) error {
	g.Params.ObstacleType = "rock"
	g.Params.ObstaclePercent = 8
	g.Params.ClusterSize = 6
	g.Params.ClearRadius = 4
	if err := decodeParams(raw, &g.Params); err != nil {
		return err
	}
	switch {
	case g.Params.ObstaclePercent < 0 || g.Params.ObstaclePercent > 40:
		return fmt.Errorf("obstacle_percent must be in 0..40")
	case g.Params.RoughPercent < 0 || g.Params.RoughPercent > 40:
		return fmt.Errorf("rough_percent must be in 0..40")
	case g.Params.ClusterSize < 1 || g.Params.ClusterSize > 64:
		return fmt.Errorf("cluster_size must be in 1..64")
	case g.Params.ClearRadius < 0:
		return fmt.Errorf("clear_radius must be >= 0")
	case g.Params.RoughPercent > 0 && g.Params.RoughType == "":
		return fmt.Errorf("rough_type is required with rough_percent")
	}
	return nil
}

// scatter places random-walk clusters of a terrain type until target tiles
// are placed (bounded attempts).
func (g *Procedural) scatter(c *Context, typ string, target int) error {
	placed := 0
	clear := int32(g.Params.ClearRadius)
	for tries := 0; placed < target && tries < 50*target+50; tries++ {
		p := c.RandomPos(0)
		for range g.Params.ClusterSize {
			if placed >= target {
				break
			}
			if c.Empty(p) && !c.NearAnchor(p, clear) {
				if err := c.AddTerrain(typ, p); err != nil {
					return err
				}
				placed++
			}
			d := dirs[c.Rand.Intn(len(dirs))]
			p = sim.Pos{X: p.X + d.X, Y: p.Y + d.Y}
			if !c.InBounds(p) {
				break
			}
		}
	}
	return nil
}

func (g *Procedural) Terrain(c *Context) error {
	area := int(c.Width) * int(c.Height)
	if err := g.scatter(c, g.Params.ObstacleType, area*g.Params.ObstaclePercent/100); err != nil {
		return err
	}
	if g.Params.RoughPercent > 0 {
		return g.scatter(c, g.Params.RoughType, area*g.Params.RoughPercent/100)
	}
	return nil
}

func (g *Procedural) Resources(c *Context) error {
	if err := PlaceHomeWells(c); err != nil {
		return err
	}
	placed := 0
	for tries := 0; placed < c.Config.Wells.Extra && tries < 400; tries++ {
		p := c.RandomPos(1)
		if c.Free(p) && !c.NearAnchor(p, int32(c.Config.Wells.NearDistance)) {
			if err := c.AddWell(p); err == nil {
				placed++
			}
		}
	}
	return nil
}

func (g *Procedural) Spawns(c *Context) error { return SpawnsAtAnchors(c) }
