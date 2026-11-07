// Package worldgen is the world-generation plugin: it builds the initial
// terrain, energy wells, player spawn points and starting units of a match
// from configuration and the world's seeded RNG.
//
// # Stages (all in the plugin's Init, after config entities exist)
//
//  0. Spawn regions: one anchor per player from spawns.layout ("corners",
//     "ring" or "random"; integer geometry only).
//  1. Terrain: Generator.Terrain.
//  2. Resources: Generator.Resources (wells).
//  3. Starting positions: Generator.Spawns (exact spawn tiles).
//  4. Units: start.units per player on the reachable free tiles nearest
//     their spawn.
//  5. Validation: spawn distances, reachable space, mutual reachability,
//     nearby wells. An invalid plan is discarded and the stages rerun (the
//     RNG continues, so every attempt differs) up to max_attempts times;
//     then setup fails with the last reason.
//  6. Publish: the plan is applied to the world (terrain, wells, units),
//     which becomes the match's initial authoritative state.
//
// Stages 1–3 operate on a Plan over a grid model, never on the world, so a
// rejected attempt leaves nothing behind. Randomness comes only from the
// world RNG (World.Rand), so a seed reproduces the map exactly.
//
// Generators are an extension point: implement Generator and register it
// with AddGenerator before the ruleset is built.
package worldgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/terrain"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "worldgen"

// Generator builds terrain, resources and spawn points on a Context. Every
// method must be deterministic given the context (use only c.Rand).
type Generator interface {
	Name() string
	// Configure decodes the generator's "params" (may be empty). Called once
	// at registration; errors fail startup.
	Configure(params json.RawMessage) error
	Terrain(c *Context) error
	Resources(c *Context) error
	Spawns(c *Context) error
}

// Config is the "worldgen" configuration section.
type Config struct {
	// Generator selects a generator by name; "none" (default) generates
	// nothing and leaves placement to the units/wells/terrain sections.
	Generator   string          `json:"generator"`
	Params      json.RawMessage `json:"params,omitempty"`
	MaxAttempts int             `json:"max_attempts"`
	Wells       WellsConfig     `json:"wells"`
	Spawns      SpawnsConfig    `json:"spawns"`
	Start       StartConfig     `json:"start"`
}

// WellsConfig controls well placement by the built-in helpers.
type WellsConfig struct {
	// PerSpawn wells are placed within near_distance of every spawn.
	PerSpawn int `json:"per_spawn"`
	// NearDistance bounds the distance of those wells (Chebyshev, >= 3).
	NearDistance int `json:"near_distance"`
	// Extra wells are placed elsewhere.
	Extra int `json:"extra"`
	// Well overrides the wells plugin's defaults for generated wells.
	Well wells.WellSpec `json:"well"`
}

// SpawnsConfig controls spawn regions.
type SpawnsConfig struct {
	Layout      string `json:"layout"`       // corners | ring | random
	MinDistance int    `json:"min_distance"` // between spawns (Chebyshev)
	Margin      int    `json:"margin"`       // from the map edge
}

// StartConfig controls starting units.
type StartConfig struct {
	Units int `json:"units"`
	// Energy, if set, is each starting unit's energy (otherwise the energy
	// plugin's initial_energy).
	Energy *int64 `json:"energy,omitempty"`
	// MinFreeTiles is the reachable free area every spawn must have.
	MinFreeTiles int `json:"min_free_tiles"`
}

// Plan is a generated world before it is applied.
type Plan struct {
	Terrain []PlannedTerrain
	Wells   []PlannedWell
	Spawns  []sim.Pos   // one per player, in player order
	Units   [][]sim.Pos // per player
}

// PlannedTerrain is one terrain feature.
type PlannedTerrain struct {
	Type string
	Pos  sim.Pos
}

// PlannedWell is one well.
type PlannedWell struct {
	Pos  sim.Pos
	Well wells.Well
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg     Config
	terrain *terrain.Plugin
	wells   *wells.Plugin
	gens    map[string]Generator
	gen     Generator
	players []sim.PlayerID
	last    *Plan // the applied plan (for inspection and tests)
}

// New returns the plugin with the built-in generators ("arena",
// "procedural").
func New(t *terrain.Plugin, w *wells.Plugin) *Plugin {
	p := &Plugin{terrain: t, wells: w, gens: map[string]Generator{}}
	p.AddGenerator(&Arena{})
	p.AddGenerator(&Procedural{})
	return p
}

// AddGenerator registers a generator. Call before the ruleset is built.
func (p *Plugin) AddGenerator(g Generator) { p.gens[g.Name()] = g }

// SetPlayers sets the players to spawn, in configuration order (game.New).
func (p *Plugin) SetPlayers(ids []sim.PlayerID) { p.players = slices.Clone(ids) }

// Generator returns the configured generator name.
func (p *Plugin) Generator() string { return p.cfg.Generator }

// LastPlan returns the applied plan (nil before Init or with "none").
func (p *Plugin) LastPlan() *Plan { return p.last }

func (*Plugin) Name() string { return Name }

// DependsOn: terrain types and wells defaults must be configured, and
// starting energy is set through the energy plugin.
func (*Plugin) DependsOn() []string { return []string{terrain.Name, wells.Name, energy.Name} }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{
		Generator:   "none",
		MaxAttempts: 10,
		Wells:       WellsConfig{PerSpawn: 1, NearDistance: 8, Extra: 2},
		Spawns:      SpawnsConfig{Layout: "ring", MinDistance: 10, Margin: 3},
		Start:       StartConfig{Units: 4, MinFreeTiles: 25},
	}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	c := p.cfg
	switch {
	case c.MaxAttempts < 1 || c.MaxAttempts > 100:
		return fmt.Errorf("worldgen max_attempts must be in 1..100")
	case c.Wells.PerSpawn < 0 || c.Wells.Extra < 0 || c.Wells.NearDistance < 3:
		return fmt.Errorf("worldgen wells: per_spawn and extra must be >= 0, near_distance >= 3")
	case c.Spawns.Layout != "corners" && c.Spawns.Layout != "ring" && c.Spawns.Layout != "random":
		return fmt.Errorf("worldgen spawns.layout must be corners, ring or random, got %q", c.Spawns.Layout)
	case c.Spawns.MinDistance < 0 || c.Spawns.Margin < 0:
		return fmt.Errorf("worldgen spawns: min_distance and margin must be >= 0")
	case c.Start.Units < 0 || c.Start.Units > 256 || c.Start.MinFreeTiles < 0:
		return fmt.Errorf("worldgen start: units must be in 0..256, min_free_tiles >= 0")
	case c.Start.Energy != nil && *c.Start.Energy < 0:
		return fmt.Errorf("worldgen start.energy must be >= 0")
	}
	if err := p.wellTemplate().Validate(); err != nil {
		return fmt.Errorf("worldgen wells.well: %w", err)
	}
	if c.Generator != "none" {
		g, ok := p.gens[c.Generator]
		if !ok {
			return fmt.Errorf("worldgen: unknown generator %q (available: none, %v)", c.Generator, slices.Sorted(maps.Keys(p.gens)))
		}
		if err := g.Configure(c.Params); err != nil {
			return fmt.Errorf("worldgen %s params: %w", c.Generator, err)
		}
		p.gen = g
		r.Init(p.generate)
	}
	return r.Describe(map[string]any{
		"generator":  c.Generator,
		"generators": slices.Sorted(maps.Keys(p.gens)),
		"wells":      c.Wells,
		"spawns":     c.Spawns,
		"start":      c.Start,
		"stages":     []string{"spawn regions", "terrain", "resources", "spawns", "units", "validate", "publish"},
	})
}

func (p *Plugin) wellTemplate() wells.Well {
	base := wells.DefaultWell()
	if p.wells != nil {
		base = p.wells.Defaults()
	}
	s := p.cfg.Wells.Well
	if s.Capacity != nil {
		base.Capacity = *s.Capacity
		base.Energy = min(base.Energy, base.Capacity)
	}
	if s.Energy != nil {
		base.Energy = *s.Energy
	}
	if s.ExtractionRate != nil {
		base.ExtractionRate = *s.ExtractionRate
	}
	if s.Unlimited != nil {
		base.Unlimited = *s.Unlimited
	}
	if s.Regeneration != nil {
		base.Regeneration = *s.Regeneration
	}
	return base
}

// generate runs the stages with bounded retries, then applies the plan.
func (p *Plugin) generate(w *sim.World) error {
	var lastErr error
	for attempt := range p.cfg.MaxAttempts {
		c := p.newContext(w, attempt)
		plan, err := p.attempt(c)
		if err == nil {
			p.last = plan
			return p.apply(w, plan)
		}
		lastErr = err
	}
	return fmt.Errorf("worldgen %s: no valid world after %d attempts: %w", p.cfg.Generator, p.cfg.MaxAttempts, lastErr)
}

func (p *Plugin) attempt(c *Context) (*Plan, error) {
	if err := c.computeAnchors(); err != nil {
		return nil, fmt.Errorf("spawn regions: %w", err)
	}
	if err := p.gen.Terrain(c); err != nil {
		return nil, fmt.Errorf("terrain: %w", err)
	}
	if err := p.gen.Resources(c); err != nil {
		return nil, fmt.Errorf("resources: %w", err)
	}
	if err := p.gen.Spawns(c); err != nil {
		return nil, fmt.Errorf("spawns: %w", err)
	}
	if len(c.plan.Spawns) != c.Players {
		return nil, fmt.Errorf("spawns: generator produced %d spawns for %d players", len(c.plan.Spawns), c.Players)
	}
	if err := c.placeUnits(p.cfg.Start.Units); err != nil {
		return nil, fmt.Errorf("units: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	return c.plan, nil
}

func (p *Plugin) apply(w *sim.World, plan *Plan) error {
	for _, t := range plan.Terrain {
		if err := p.terrain.Place(w, t.Type, t.Pos); err != nil {
			return fmt.Errorf("worldgen apply terrain at %v: %w", t.Pos, err)
		}
	}
	for _, wl := range plan.Wells {
		if _, err := wells.Create(w, wl.Pos, wl.Well); err != nil {
			return fmt.Errorf("worldgen apply well at %v: %w", wl.Pos, err)
		}
	}
	for i, units := range plan.Units {
		for _, pos := range units {
			id, err := w.CreateEntity(sim.KindUnit, p.players[i], pos)
			if err != nil {
				return fmt.Errorf("worldgen apply unit at %v: %w", pos, err)
			}
			if e := p.cfg.Start.Energy; e != nil {
				energy.SetStored(w, id, *e)
			}
		}
	}
	return nil
}

// ---- context: the grid model generators work on ---------------------------------------

type cell uint8

const (
	free    cell = iota
	rough        // passable terrain
	blocked      // impassable terrain or a pre-existing blocking entity
	well
	unit
)

// Context is one generation attempt.
type Context struct {
	Width, Height int32
	Players       int
	Attempt       int
	// Rand is the world's deterministic RNG.
	Rand *sim.Rand
	// Config is the worldgen configuration.
	Config *Config
	// Anchors are the spawn regions (one per player), set before Terrain.
	Anchors []sim.Pos

	p    *Plugin
	grid []cell
	plan *Plan
}

func (p *Plugin) newContext(w *sim.World, attempt int) *Context {
	c := &Context{
		Width: w.Width(), Height: w.Height(), Players: len(p.players), Attempt: attempt,
		Rand: w.Rand(), Config: &p.cfg, p: p,
		grid: make([]cell, int(w.Width())*int(w.Height())),
		plan: &Plan{},
	}
	for y := range w.Height() {
		for x := range w.Width() {
			pos := sim.Pos{X: x, Y: y}
			if _, ok := w.EntityAt(pos); ok {
				c.grid[c.idx(pos)] = blocked
			} else if len(w.PassableAt(pos)) > 0 {
				c.grid[c.idx(pos)] = rough
			}
		}
	}
	return c
}

func (c *Context) idx(p sim.Pos) int { return int(p.Y)*int(c.Width) + int(p.X) }

// InBounds reports whether a tile is on the map.
func (c *Context) InBounds(p sim.Pos) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < c.Width && p.Y < c.Height
}

// Free reports whether a tile can take a blocking entity (in bounds, no
// blocking entity planned or existing; passable terrain is fine).
func (c *Context) Free(p sim.Pos) bool {
	return c.InBounds(p) && (c.grid[c.idx(p)] == free || c.grid[c.idx(p)] == rough)
}

// Empty reports whether a tile has nothing at all.
func (c *Context) Empty(p sim.Pos) bool { return c.InBounds(p) && c.grid[c.idx(p)] == free }

// Walkable reports whether units can cross a tile.
func (c *Context) Walkable(p sim.Pos) bool {
	return c.Free(p) || (c.InBounds(p) && c.grid[c.idx(p)] == unit)
}

// AddTerrain plans a terrain feature of a configured type on an empty tile.
func (c *Context) AddTerrain(typeName string, p sim.Pos) error {
	t, ok := c.p.terrain.Type(typeName)
	if !ok {
		return fmt.Errorf("unknown terrain type %q", typeName)
	}
	if !c.Empty(p) {
		return fmt.Errorf("tile %v is not empty", p)
	}
	c.grid[c.idx(p)] = blocked
	if t.Passable {
		c.grid[c.idx(p)] = rough
	}
	c.plan.Terrain = append(c.plan.Terrain, PlannedTerrain{Type: typeName, Pos: p})
	return nil
}

// AddWell plans a well (the generated-well template) on a free tile.
func (c *Context) AddWell(p sim.Pos) error {
	if !c.Free(p) {
		return fmt.Errorf("tile %v is not free", p)
	}
	c.grid[c.idx(p)] = well
	c.plan.Wells = append(c.plan.Wells, PlannedWell{Pos: p, Well: c.p.wellTemplate()})
	return nil
}

// SetSpawns sets the spawn tiles (one per player, in player order).
func (c *Context) SetSpawns(ps []sim.Pos) { c.plan.Spawns = slices.Clone(ps) }

// RandomPos returns a uniformly random in-bounds tile at least margin from
// the edge.
func (c *Context) RandomPos(margin int32) sim.Pos {
	w, h := max(c.Width-2*margin, 1), max(c.Height-2*margin, 1)
	return sim.Pos{X: margin + int32(c.Rand.Intn(int(w))), Y: margin + int32(c.Rand.Intn(int(h)))}
}

// NearAnchor reports whether p is within r (Chebyshev) of any anchor.
func (c *Context) NearAnchor(p sim.Pos, r int32) bool {
	for _, a := range c.Anchors {
		if cheb(a, p) <= r {
			return true
		}
	}
	return false
}

func cheb(a, b sim.Pos) int32 {
	dx, dy := a.X-b.X, a.Y-b.Y
	return max(dx, -dx, dy, -dy)
}

// 8-neighbourhood in a fixed order.
var dirs = [8]sim.Pos{{X: 0, Y: -1}, {X: 1, Y: -1}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0}, {X: -1, Y: -1}}

// bfs visits walkable tiles from start in breadth-first order (fixed
// neighbour order), calling visit(pos, depth) until it returns false.
func (c *Context) bfs(start sim.Pos, visit func(p sim.Pos, d int) bool) {
	if !c.Walkable(start) {
		return
	}
	seen := make([]bool, len(c.grid))
	type item struct {
		p sim.Pos
		d int
	}
	q := []item{{start, 0}}
	seen[c.idx(start)] = true
	for len(q) > 0 {
		it := q[0]
		q = q[1:]
		if !visit(it.p, it.d) {
			return
		}
		for _, d := range dirs {
			n := sim.Pos{X: it.p.X + d.X, Y: it.p.Y + d.Y}
			if c.InBounds(n) && !seen[c.idx(n)] && c.Walkable(n) {
				seen[c.idx(n)] = true
				q = append(q, item{n, it.d + 1})
			}
		}
	}
}

// NearestFree returns the free tile nearest to p (spiral by Chebyshev
// ring, rows then columns), searching up to radius r.
func (c *Context) NearestFree(p sim.Pos, r int32) (sim.Pos, bool) {
	for d := int32(0); d <= r; d++ {
		for y := p.Y - d; y <= p.Y+d; y++ {
			for x := p.X - d; x <= p.X+d; x++ {
				q := sim.Pos{X: x, Y: y}
				if cheb(p, q) == d && c.Free(q) {
					return q, true
				}
			}
		}
	}
	return sim.Pos{}, false
}

// computeAnchors places one spawn region per player.
func (c *Context) computeAnchors() error {
	n := c.Players
	if n == 0 {
		return nil
	}
	m := int32(c.Config.Spawns.Margin)
	x0, y0, x1, y1 := m, m, c.Width-1-m, c.Height-1-m
	if x1 < x0 || y1 < y0 {
		return errors.New("map too small for the spawn margin")
	}
	layout := c.Config.Spawns.Layout
	if layout == "corners" && n > 4 {
		layout = "ring"
	}
	switch layout {
	case "corners":
		corners := []sim.Pos{{X: x0, Y: y0}, {X: x1, Y: y1}, {X: x1, Y: y0}, {X: x0, Y: y1}}
		c.Anchors = corners[:n]
	case "ring":
		// Evenly spaced along the inset rectangle's perimeter, starting at
		// the top-left corner, clockwise. Integer arithmetic only.
		w, h := int64(x1-x0), int64(y1-y0)
		per := 2 * (w + h)
		for i := range int64(n) {
			o := i * per / int64(n)
			var pt sim.Pos
			switch {
			case o < w:
				pt = sim.Pos{X: x0 + int32(o), Y: y0}
			case o < w+h:
				pt = sim.Pos{X: x1, Y: y0 + int32(o-w)}
			case o < 2*w+h:
				pt = sim.Pos{X: x1 - int32(o-w-h), Y: y1}
			default:
				pt = sim.Pos{X: x0, Y: y1 - int32(o-2*w-h)}
			}
			c.Anchors = append(c.Anchors, pt)
		}
	case "random":
		for tries := 0; len(c.Anchors) < n && tries < 200*n; tries++ {
			pt := c.RandomPos(m)
			if !c.NearAnchor(pt, int32(c.Config.Spawns.MinDistance)-1) || c.Config.Spawns.MinDistance == 0 {
				c.Anchors = append(c.Anchors, pt)
			}
		}
		if len(c.Anchors) < n {
			return fmt.Errorf("could not place %d spawn regions %d apart", n, c.Config.Spawns.MinDistance)
		}
	}
	return nil
}

// placeUnits plans start units per player: the walkable free tiles nearest
// each spawn in BFS order.
func (c *Context) placeUnits(n int) error {
	c.plan.Units = make([][]sim.Pos, len(c.plan.Spawns))
	for i, s := range c.plan.Spawns {
		var got []sim.Pos
		c.bfs(s, func(p sim.Pos, _ int) bool {
			if c.Free(p) && len(got) < n {
				got = append(got, p)
			}
			return len(got) < n
		})
		if len(got) < n {
			return fmt.Errorf("player %d: room for %d of %d starting units", i+1, len(got), n)
		}
		for _, p := range got {
			c.grid[c.idx(p)] = unit
		}
		c.plan.Units[i] = got
	}
	return nil
}

// validate checks fairness and accessibility of the plan.
func (c *Context) validate() error {
	cfg := c.Config
	for i, s := range c.plan.Spawns {
		for j := range i {
			if d := cheb(s, c.plan.Spawns[j]); d < int32(cfg.Spawns.MinDistance) {
				return fmt.Errorf("spawns %d and %d are %d apart (< %d)", j+1, i+1, d, cfg.Spawns.MinDistance)
			}
		}
	}
	comp := make([]int, len(c.grid))
	for i, s := range c.plan.Spawns {
		reach := 0
		c.bfs(s, func(p sim.Pos, _ int) bool {
			reach++
			comp[c.idx(p)] |= 1 << i
			return true
		})
		if reach < cfg.Start.MinFreeTiles {
			return fmt.Errorf("spawn %d reaches only %d tiles (< %d)", i+1, reach, cfg.Start.MinFreeTiles)
		}
		if i > 0 && comp[c.idx(c.plan.Spawns[0])]&(1<<i) == 0 {
			return fmt.Errorf("spawn %d cannot reach spawn 1", i+1)
		}
		// Wells near this spawn with a reachable adjacent tile.
		near := 0
		for _, wl := range c.plan.Wells {
			if cheb(wl.Pos, s) > int32(cfg.Wells.NearDistance) {
				continue
			}
			for _, d := range dirs {
				n := sim.Pos{X: wl.Pos.X + d.X, Y: wl.Pos.Y + d.Y}
				if c.InBounds(n) && comp[c.idx(n)]&(1<<i) != 0 {
					near++
					break
				}
			}
		}
		if near < cfg.Wells.PerSpawn {
			return fmt.Errorf("spawn %d has %d accessible wells within %d (< %d)", i+1, near, cfg.Wells.NearDistance, cfg.Wells.PerSpawn)
		}
	}
	return nil
}

// ---- shared helpers for generators ---------------------------------------------------------

// PlaceHomeWells places wells.per_spawn wells at random free tiles between
// 3 and wells.near_distance from every anchor.
func PlaceHomeWells(c *Context) error {
	nd := int32(c.Config.Wells.NearDistance)
	for i, a := range c.Anchors {
		placed := 0
		for tries := 0; placed < c.Config.Wells.PerSpawn && tries < 400; tries++ {
			p := sim.Pos{X: a.X - nd + int32(c.Rand.Intn(int(2*nd+1))), Y: a.Y - nd + int32(c.Rand.Intn(int(2*nd+1)))}
			if cheb(p, a) < 3 || !c.Free(p) || nearOtherAnchor(c, p, i, 3) {
				continue
			}
			if err := c.AddWell(p); err == nil {
				placed++
			}
		}
		if placed < c.Config.Wells.PerSpawn {
			return fmt.Errorf("placed %d of %d wells near spawn %d", placed, c.Config.Wells.PerSpawn, i+1)
		}
	}
	return nil
}

func nearOtherAnchor(c *Context, p sim.Pos, self int, r int32) bool {
	for j, a := range c.Anchors {
		if j != self && cheb(a, p) < r {
			return true
		}
	}
	return false
}

// SpawnsAtAnchors sets each spawn to the free tile nearest its anchor.
func SpawnsAtAnchors(c *Context) error {
	var ps []sim.Pos
	for i, a := range c.Anchors {
		p, ok := c.NearestFree(a, 8)
		if !ok {
			return fmt.Errorf("no free tile near spawn region %d", i+1)
		}
		ps = append(ps, p)
	}
	c.SetSpawns(ps)
	return nil
}
