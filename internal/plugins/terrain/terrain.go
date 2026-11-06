// Package terrain places terrain features: entities of the existing
// terrain category. A terrain type is either impassable (a blocking entity:
// nothing can enter its tile) or passable with a movement cost multiplier
// (a passable entity: units walk over it, taking cost times longer per
// step). Terrain is static: it is placed from configuration at world
// creation; admins may remove it. Like any entity, it is subject to fog of
// war (terrain.* attributes are disclosed per the visibility rules).
package terrain

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "terrain"

// MaxCost bounds a terrain type's movement cost.
const MaxCost = 20

// TypeConfig configures a terrain type.
type TypeConfig struct {
	Passable bool  `json:"passable"`
	Cost     int64 `json:"cost,omitempty"` // passable types: step cost multiplier (default 1)
}

// Block places a W x H rectangle of a type (W, H default 1).
type Block struct {
	Type string `json:"type"`
	X    int32  `json:"x"`
	Y    int32  `json:"y"`
	W    int32  `json:"w"`
	H    int32  `json:"h"`
}

// Config is the plugin's configuration schema (the "terrain" section).
type Config struct {
	Types map[string]TypeConfig `json:"types"`
	Tiles []Block               `json:"tiles"`
}

// Feature is a terrain entity's state.
type Feature struct {
	Type     string
	Passable bool
	Cost     int64
}

var features = sim.NewComponent[Feature]("terrain.feature")

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg Config
}

// New returns the terrain plugin.
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return nil }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	for name, t := range p.cfg.Types {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("terrain type name %q is invalid", name)
		}
		if t.Passable && (t.Cost < 0 || t.Cost > MaxCost) {
			return fmt.Errorf("terrain type %q: cost must be in 1..%d", name, MaxCost)
		}
		if !t.Passable && t.Cost != 0 {
			return fmt.Errorf("terrain type %q: impassable types have no cost", name)
		}
	}
	for i, b := range p.cfg.Tiles {
		if _, ok := p.cfg.Types[b.Type]; !ok {
			return fmt.Errorf("terrain tiles[%d]: unknown type %q", i, b.Type)
		}
	}
	if err := r.Component(features.Def(sim.VisiblePublic, func(f Feature) []sim.Field {
		return []sim.Field{
			{Key: "terrain.type", Value: sim.StringValue(f.Type)},
			{Key: "terrain.passable", Value: sim.BoolValue(f.Passable)},
			{Key: "terrain.cost", Value: sim.IntValue(f.Cost)},
		}
	})); err != nil {
		return err
	}
	r.Init(p.place)
	return r.Describe(p.cfg)
}

func (p *Plugin) place(w *sim.World) error {
	for i, b := range p.cfg.Tiles {
		t := p.cfg.Types[b.Type]
		f := Feature{Type: b.Type, Passable: t.Passable, Cost: max(t.Cost, 1)}
		if !t.Passable {
			f.Cost = 0
		}
		for dy := range max(b.H, 1) {
			for dx := range max(b.W, 1) {
				pos := sim.Pos{X: b.X + dx, Y: b.Y + dy}
				var id sim.EntityID
				var err error
				if t.Passable {
					id, err = w.CreatePassable(sim.KindTerrain, 0, pos)
				} else {
					id, err = w.CreateEntity(sim.KindTerrain, 0, pos)
				}
				if err != nil {
					return fmt.Errorf("terrain tiles[%d] at %v: %w", i, pos, err)
				}
				features.Set(w, id, f)
			}
		}
	}
	return nil
}

// StepCost returns the cost multiplier of stepping onto pos and whether a
// unit can ever enter it. Static obstacles (blocking non-unit entities:
// impassable terrain, wells) cannot be entered; units are not obstacles
// here (they move). Passable terrain multiplies the cost (the highest
// multiplier among features on the tile).
func StepCost(w *sim.World, pos sim.Pos) (int64, bool) {
	if !w.InBounds(pos) {
		return 0, false
	}
	if id, ok := w.EntityAt(pos); ok {
		if e, _ := w.Entity(id); e.Kind != sim.KindUnit {
			return 0, false
		}
	}
	cost := int64(1)
	if !features.Registered(w) {
		return cost, true
	}
	for _, id := range w.PassableAt(pos) {
		if f, ok := features.Get(w, id); ok {
			cost = max(cost, f.Cost)
		}
	}
	return cost, true
}

// IsTerrain reports whether an entity is a terrain feature.
func IsTerrain(w *sim.World, id sim.EntityID) bool {
	return features.Registered(w) && features.Has(w, id)
}

// Types returns the configured type names, sorted.
func (p *Plugin) Types() []string {
	var out []string
	for n := range p.cfg.Types {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
