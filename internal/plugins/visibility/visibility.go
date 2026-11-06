// Package visibility is the fog-of-war plugin: the single authority on what
// each player observes of entities they do not own.
//
// # Model (computed independently for every player)
//
//   - Visible: a tile within vision range of at least one of the player's
//     units (configurable grid metric; no occlusion yet).
//   - Detected: a foreign entity on a visible tile whose detection score
//     reaches detection_threshold for at least one observer that sees the
//     tile. Detection reveals ID, position, owner and kind only.
//   - Identified: the best identification score among detecting observers
//     selects a level: basic (always, when detected), estimated
//     (>= estimated_threshold) or detailed (>= detailed_threshold).
//
// Scores are integer formulas (package expr) over observer_<p>,
// target_<p> and distance, where <p> is a sensor stat (vision, sensor,
// signature, identification) or any attributes property. Non-unit targets
// (wells, wreckage) have signature = resource_signature and 0 for every
// other property. Nothing is random.
//
// Players always know everything about their own units; that never passes
// through this plugin.
//
// # Disclosure
//
// A foreign entity's attributes are disclosed only through disclosure
// rules. A rule names an observable attribute key ("combat.health") or a
// prefix ("attributes.*") and, per level, how much to disclose: 0 = nothing,
// 1 = exact, N > 1 = an estimate [floor(v/N)*N, floor(v/N)*N + N-1]. A level
// without a setting inherits the nearest lower level's. Attributes without a
// rule, including metadata and orders, are never disclosed.
//
// # Timing
//
// Observation runs after every system of a tick (commands, energy,
// salvage, movement, combat, upgrades, cloning), so players only see the
// settled end-of-tick state: positions after movement, damage and
// destruction after combat, offspring after cloning, admin mutations after
// they executed. Commands that need detection (attacks, mining, salvage)
// use the observation computed at the end of the previous tick — what the
// player actually knew when issuing them.
//
// # Memory
//
// The plugin keeps only current state per player (visible tiles, and the
// currently known foreign entities with the view last disclosed, needed to
// send deltas). There is no explored-map history; clients remember.
package visibility

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strings"

	"github.com/dakotaknutson/unit-strategy-game/internal/expr"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "visibility"

// Events (to the observing player only).
const (
	EventDiscovered = "visibility.discovered" // level
	EventLost       = "visibility.lost"
	EventIdentified = "visibility.identified" // level, previous
)

// MaxVision bounds every vision range.
const MaxVision = 32

// Stat binds a sensor stat to an attributes property, or a constant.
type Stat struct {
	Property string `json:"property,omitempty"`
	Default  int64  `json:"default"`
}

// Rule is a disclosure rule. Level fields: 0 nothing, 1 exact, N>1 bucket.
type Rule struct {
	Key       string `json:"key"`
	Basic     int64  `json:"basic,omitempty"`
	Estimated int64  `json:"estimated,omitempty"`
	Detailed  int64  `json:"detailed,omitempty"`
}

// Config is the plugin's configuration schema (the "visibility" section).
type Config struct {
	// Enabled turns fog of war on. When false (the default, for
	// compatibility) players observe everything as in earlier milestones.
	Enabled bool `json:"enabled"`

	Vision         Stat `json:"vision"`
	Sensor         Stat `json:"sensor"`
	Signature      Stat `json:"signature"`
	Identification Stat `json:"identification"`

	ResourceSignature int64 `json:"resource_signature"`

	Detection          string `json:"detection_formula"`
	DetectionThreshold int64  `json:"detection_threshold"`
	IdentificationExpr string `json:"identification_formula"`
	EstimatedThreshold int64  `json:"estimated_threshold"`
	DetailedThreshold  int64  `json:"detailed_threshold"`

	// DistanceMetric: chebyshev (default), manhattan or euclidean (rounded
	// to the nearest integer).
	DistanceMetric string `json:"distance_metric"`
	// Occlusion: only "none" is supported; terrain line-of-sight is planned.
	Occlusion string `json:"occlusion"`

	Disclosure []Rule `json:"disclosure"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Vision:             Stat{Default: 5},
		Sensor:             Stat{Default: 5},
		Signature:          Stat{Default: 0},
		Identification:     Stat{Default: 2},
		Detection:          "observer_sensor - target_signature - distance",
		DetectionThreshold: 0,
		IdentificationExpr: "observer_identification - distance",
		EstimatedThreshold: 0,
		DetailedThreshold:  3,
		DistanceMetric:     "chebyshev",
		Occlusion:          "none",
	}
}

var sensorStats = []string{"vision", "sensor", "signature", "identification"}

type varRef struct {
	side int // 0 observer, 1 target, 2 distance
	name string
}

type formula struct {
	prog *expr.Program
	vars []varRef
}

type known struct {
	level    sim.Level
	view     sim.EntityView
	observer sim.EntityID
	detect   int64
	ident    int64
}

type playerState struct {
	visible []uint64 // bitset over tiles, index y*width+x
	known   map[sim.EntityID]known
}

// sensorSummary is the owner-visible summary of a unit's sensors.
type sensorSummary struct {
	Vision, Sensor, Signature, Identification int64
}

var summaries = sim.NewComponent[sensorSummary]("visibility.sensors")

// Plugin implements sim.Plugin and sim.ObservationAuthority.
type Plugin struct {
	cfg     Config
	attrs   *attributes.Plugin
	detect  formula
	ident   formula
	rules   map[string]Rule // exact keys
	prefix  []Rule          // "x.*" rules, longest prefix first
	players map[sim.PlayerID]*playerState
	width   int32
	height  int32
}

// New returns the visibility plugin. attrs may be nil (constant stats).
func New(attrs *attributes.Plugin) *Plugin {
	return &Plugin{attrs: attrs, players: map[sim.PlayerID]*playerState{}}
}

func (*Plugin) Name() string { return Name }

func (p *Plugin) DependsOn() []string {
	if p.attrs != nil {
		return []string{attributes.Name}
	}
	return nil
}

// Enabled reports whether fog of war is on.
func (p *Plugin) Enabled() bool { return p.cfg.Enabled }

// Detected reports whether a player currently detects an entity (as of the
// end of the last tick). Own entities are always "detected".
func (p *Plugin) Detected(w *sim.World, player sim.PlayerID, id sim.EntityID) bool {
	if !p.cfg.Enabled {
		return true
	}
	if e, ok := w.Entity(id); ok && e.Owner == player && player != 0 {
		return true
	}
	ps := p.players[player]
	if ps == nil {
		return false
	}
	_, ok := ps.known[id]
	return ok
}

func (p *Plugin) hasProperty(name string) bool {
	return slices.Contains(sensorStats, name) || (p.attrs != nil && p.attrs.Has(name))
}

func (p *Plugin) compile(field, src string) (formula, error) {
	e, err := expr.Parse(src)
	if err != nil {
		return formula{}, fmt.Errorf("visibility %s_formula: %w", field, err)
	}
	var f formula
	slot := map[string]int{}
	f.prog, err = e.Compile(func(name string) (int, bool) {
		if i, ok := slot[name]; ok {
			return i, true
		}
		ref := varRef{side: -1}
		switch {
		case name == "distance":
			ref = varRef{side: 2}
		case strings.HasPrefix(name, "observer_"):
			ref = varRef{side: 0, name: strings.TrimPrefix(name, "observer_")}
		case strings.HasPrefix(name, "target_"):
			ref = varRef{side: 1, name: strings.TrimPrefix(name, "target_")}
		}
		if ref.side < 0 || (ref.side < 2 && !p.hasProperty(ref.name)) {
			return 0, false
		}
		slot[name] = len(f.vars)
		f.vars = append(f.vars, ref)
		return slot[name], true
	})
	if err != nil {
		return formula{}, fmt.Errorf("visibility %s: %w (available: observer_<p>, target_<p>, distance; <p> = vision, sensor, signature, identification or an attributes property)", field, err)
	}
	return f, nil
}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	var err error
	if p.detect, err = p.compile("detection", p.cfg.Detection); err != nil {
		return err
	}
	if p.ident, err = p.compile("identification", p.cfg.IdentificationExpr); err != nil {
		return err
	}
	if err := r.Component(summaries.Def(sim.VisibleOwner, func(s sensorSummary) []sim.Field {
		return []sim.Field{
			{Key: "visibility.vision", Value: sim.IntValue(s.Vision)},
			{Key: "visibility.sensor", Value: sim.IntValue(s.Sensor)},
			{Key: "visibility.signature", Value: sim.IntValue(s.Signature)},
			{Key: "visibility.identification", Value: sim.IntValue(s.Identification)},
		}
	})); err != nil {
		return err
	}
	refresh := func(w *sim.World, id sim.EntityID) {
		if e, ok := w.Entity(id); ok && e.Kind == sim.KindUnit {
			summaries.Set(w, id, sensorSummary{
				Vision:         p.vision(w, id),
				Sensor:         p.stat(w, id, p.cfg.Sensor),
				Signature:      p.stat(w, id, p.cfg.Signature),
				Identification: p.stat(w, id, p.cfg.Identification),
			})
		}
	}
	r.OnCreate(func(w *sim.World, e sim.Entity) { refresh(w, e.ID) })
	if p.attrs != nil {
		p.attrs.OnChange(refresh)
	}
	if err := r.Describe(map[string]any{
		"config": p.cfg,
		"model":  "visible: tile within an own unit's vision; detected: on a visible tile and detection >= detection_threshold for an observer seeing it; level: basic, estimated (identification >= estimated_threshold), detailed (>= detailed_threshold)",
		"timing": "computed after all systems each tick; commands use the observation from the end of the previous tick",
	}); err != nil {
		return err
	}
	if p.cfg.Enabled {
		return r.ObservationAuthority(p)
	}
	return nil
}

func (p *Plugin) validate() error {
	c := p.cfg
	for name, s := range map[string]Stat{"vision": c.Vision, "sensor": c.Sensor, "signature": c.Signature, "identification": c.Identification} {
		if s.Property != "" && (p.attrs == nil || !p.attrs.Has(s.Property)) {
			return fmt.Errorf("visibility %s.property: %q is not an attribute or derived property", name, s.Property)
		}
	}
	if c.Vision.Default < 0 || c.Vision.Default > MaxVision {
		return fmt.Errorf("visibility vision.default must be in 0..%d", MaxVision)
	}
	if c.EstimatedThreshold > c.DetailedThreshold {
		return fmt.Errorf("visibility estimated_threshold (%d) must not exceed detailed_threshold (%d)", c.EstimatedThreshold, c.DetailedThreshold)
	}
	switch c.DistanceMetric {
	case "chebyshev", "manhattan", "euclidean":
	default:
		return fmt.Errorf("visibility distance_metric must be chebyshev, manhattan or euclidean, got %q", c.DistanceMetric)
	}
	if c.Occlusion != "none" {
		return fmt.Errorf("visibility occlusion %q is not supported yet (only \"none\")", c.Occlusion)
	}
	p.rules = map[string]Rule{}
	for i, r := range c.Disclosure {
		if r.Key == "" || r.Key == "*" || strings.Count(r.Key, "*") > 1 || (strings.Contains(r.Key, "*") && !strings.HasSuffix(r.Key, ".*")) {
			return fmt.Errorf("visibility disclosure[%d]: key %q must be an attribute key or end in \".*\"", i, r.Key)
		}
		levels := []int64{r.Basic, r.Estimated, r.Detailed}
		any, prev := false, int64(0)
		for _, b := range levels {
			if b < 0 {
				return fmt.Errorf("visibility disclosure[%d] %q: precision must be >= 0", i, r.Key)
			}
			if b == 0 {
				continue
			}
			// Higher levels must be at least as precise (smaller bucket).
			if prev != 0 && b > prev {
				return fmt.Errorf("visibility disclosure[%d] %q: a higher identification level cannot be less precise", i, r.Key)
			}
			any, prev = true, b
		}
		if !any {
			return fmt.Errorf("visibility disclosure[%d] %q discloses nothing at any level", i, r.Key)
		}
		if strings.HasSuffix(r.Key, ".*") {
			p.prefix = append(p.prefix, r)
		} else {
			if _, dup := p.rules[r.Key]; dup {
				return fmt.Errorf("visibility disclosure: duplicate key %q", r.Key)
			}
			p.rules[r.Key] = r
		}
	}
	slices.SortStableFunc(p.prefix, func(a, b Rule) int { return len(b.Key) - len(a.Key) })
	return nil
}

func (p *Plugin) stat(w *sim.World, id sim.EntityID, s Stat) int64 {
	if s.Property != "" {
		v, _ := p.attrs.Lookup(w, id, s.Property)
		return v
	}
	return s.Default
}

func (p *Plugin) vision(w *sim.World, id sim.EntityID) int64 {
	return max(0, min(p.stat(w, id, p.cfg.Vision), MaxVision))
}

// property resolves a formula variable for one entity.
func (p *Plugin) property(w *sim.World, e sim.Entity, name string) int64 {
	if e.Kind != sim.KindUnit {
		if name == "signature" {
			return p.cfg.ResourceSignature
		}
		return 0
	}
	switch name {
	case "vision":
		return p.vision(w, e.ID)
	case "sensor":
		return p.stat(w, e.ID, p.cfg.Sensor)
	case "signature":
		return p.stat(w, e.ID, p.cfg.Signature)
	case "identification":
		return p.stat(w, e.ID, p.cfg.Identification)
	}
	v, _ := p.attrs.Lookup(w, e.ID, name)
	return v
}

func (p *Plugin) eval(f formula, w *sim.World, obs, tgt sim.Entity, dist int64) int64 {
	var buf [16]int64
	env := buf[:0]
	for _, v := range f.vars {
		switch v.side {
		case 0:
			env = append(env, p.property(w, obs, v.name))
		case 1:
			env = append(env, p.property(w, tgt, v.name))
		default:
			env = append(env, dist)
		}
	}
	return f.prog.Eval(env)
}

// distance returns the configured metric's distance and whether it is
// within radius r.
func (p *Plugin) distance(dx, dy int64) int64 {
	dx, dy = max(dx, -dx), max(dy, -dy)
	switch p.cfg.DistanceMetric {
	case "manhattan":
		return dx + dy
	case "euclidean":
		// Rounded to the nearest integer using exact integer arithmetic:
		// r = isqrt(d2), plus one if d2 >= (r + 1/2)^2.
		d2 := dx*dx + dy*dy
		r := int64(math.Sqrt(float64(d2)))
		for r*r > d2 {
			r--
		}
		for (r+1)*(r+1) <= d2 {
			r++
		}
		if 4*d2 >= (2*r+1)*(2*r+1) {
			r++
		}
		return r
	}
	return max(dx, dy)
}

// ---- disclosure -----------------------------------------------------------------

func (p *Plugin) ruleFor(key string) (Rule, bool) {
	if r, ok := p.rules[key]; ok {
		return r, true
	}
	for _, r := range p.prefix {
		if strings.HasPrefix(key, strings.TrimSuffix(r.Key, "*")) {
			return r, true
		}
	}
	return Rule{}, false
}

func precision(r Rule, level sim.Level) int64 {
	b := int64(0)
	if level >= sim.LevelBasic && r.Basic > 0 {
		b = r.Basic
	}
	if level >= sim.LevelEstimated && r.Estimated > 0 {
		b = r.Estimated
	}
	if level >= sim.LevelDetailed && r.Detailed > 0 {
		b = r.Detailed
	}
	return b
}

// floorDiv rounds toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Disclose filters a full entity view down to what a player at the given
// level may know.
func (p *Plugin) Disclose(v sim.EntityView, level sim.Level) sim.EntityView {
	out := sim.EntityView{ID: v.ID, Kind: v.Kind, Owner: v.Owner, Pos: v.Pos, Passable: v.Passable}
	for _, a := range v.Attrs {
		r, ok := p.ruleFor(a.Key)
		if !ok {
			continue
		}
		b := precision(r, level)
		if b == 0 {
			continue
		}
		val := a.Value
		if b > 1 && val.Kind == sim.ValueInt {
			lo := floorDiv(val.Int, b) * b
			val = sim.RangeValue(lo, lo+b-1)
		}
		out.Attrs = append(out.Attrs, sim.Attr{Key: a.Key, Value: val, Visibility: sim.VisiblePublic})
	}
	return out
}

func sameView(a, b sim.EntityView) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Owner == b.Owner && a.Pos == b.Pos && a.Passable == b.Passable && slices.Equal(a.Attrs, b.Attrs)
}

// ---- observation ------------------------------------------------------------------

type candidate struct {
	level    sim.Level
	observer sim.EntityID
	detect   int64
	ident    int64
}

func better(a, b candidate) bool {
	switch {
	case a.level != b.level:
		return a.level > b.level
	case a.ident != b.ident:
		return a.ident > b.ident
	case a.detect != b.detect:
		return a.detect > b.detect
	}
	return a.observer < b.observer
}

// Observations implements sim.ObservationAuthority.
func (p *Plugin) Observations(ctx *sim.TickContext) []sim.Observation {
	w := ctx.World
	p.width, p.height = w.Width(), w.Height()
	words := (int(p.width)*int(p.height) + 63) / 64

	// One pass over the world: units by owner, passable entities by tile.
	unitsBy := map[sim.PlayerID][]sim.Entity{}
	passableAt := map[sim.Pos][]sim.EntityID{}
	for _, id := range w.EntityIDs() {
		e, _ := w.Entity(id)
		if e.Kind == sim.KindUnit && e.Owner != 0 {
			unitsBy[e.Owner] = append(unitsBy[e.Owner], e)
		}
		if e.Passable {
			passableAt[e.Pos] = append(passableAt[e.Pos], id)
		}
	}
	var players []sim.PlayerID
	for pl := range unitsBy {
		players = append(players, pl)
	}
	for pl := range p.players {
		if _, ok := unitsBy[pl]; !ok {
			players = append(players, pl)
		}
	}
	slices.Sort(players)

	var out []sim.Observation
	for _, pl := range players {
		visible := make([]uint64, words)
		best := map[sim.EntityID]candidate{}
		consider := func(o sim.Entity, tid sim.EntityID, dist int64) {
			t, _ := w.Entity(tid)
			if t.Owner == pl && pl != 0 {
				return
			}
			d := p.eval(p.detect, w, o, t, dist)
			if d < p.cfg.DetectionThreshold {
				return
			}
			id := p.eval(p.ident, w, o, t, dist)
			c := candidate{level: sim.LevelBasic, observer: o.ID, detect: d, ident: id}
			if id >= p.cfg.DetailedThreshold {
				c.level = sim.LevelDetailed
			} else if id >= p.cfg.EstimatedThreshold {
				c.level = sim.LevelEstimated
			}
			if prev, ok := best[tid]; !ok || better(c, prev) {
				best[tid] = c
			}
		}
		for _, o := range unitsBy[pl] {
			r := p.vision(w, o.ID)
			for y := max(0, int64(o.Pos.Y)-r); y <= min(int64(p.height)-1, int64(o.Pos.Y)+r); y++ {
				for x := max(0, int64(o.Pos.X)-r); x <= min(int64(p.width)-1, int64(o.Pos.X)+r); x++ {
					dist := p.distance(x-int64(o.Pos.X), y-int64(o.Pos.Y))
					if dist > r {
						continue
					}
					i := y*int64(p.width) + x
					visible[i/64] |= 1 << (i % 64)
					pos := sim.Pos{X: int32(x), Y: int32(y)}
					if tid, ok := w.EntityAt(pos); ok {
						consider(o, tid, dist)
					}
					for _, tid := range passableAt[pos] {
						consider(o, tid, dist)
					}
				}
			}
		}

		ps := p.players[pl]
		if ps == nil {
			ps = &playerState{visible: make([]uint64, words), known: map[sim.EntityID]known{}}
			p.players[pl] = ps
		}
		obs := sim.Observation{Player: pl}
		// Tile deltas, row-major order.
		for wi := range words {
			diff := visible[wi] ^ ps.visible[wi]
			for diff != 0 {
				b := bits.TrailingZeros64(diff)
				diff &^= 1 << b
				i := int64(wi)*64 + int64(b)
				pos := sim.Pos{X: int32(i % int64(p.width)), Y: int32(i / int64(p.width))}
				if visible[wi]&(1<<b) != 0 {
					obs.TilesEntered = append(obs.TilesEntered, pos)
				} else {
					obs.TilesExited = append(obs.TilesExited, pos)
				}
			}
		}
		// Lost entities.
		var oldIDs []sim.EntityID
		for id := range ps.known {
			oldIDs = append(oldIDs, id)
		}
		slices.Sort(oldIDs)
		for _, id := range oldIDs {
			if _, still := best[id]; !still {
				obs.Lost = append(obs.Lost, id)
				delete(ps.known, id)
				ctx.Emit(sim.Event{Type: EventLost, Entity: id, Audience: sim.ToPlayer(pl)})
			}
		}
		// Discovered and changed entities.
		var ids []sim.EntityID
		for id := range best {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			c := best[id]
			prev, had := ps.known[id]
			k := known{level: c.level, observer: c.observer, detect: c.detect, ident: c.ident, view: prev.view}
			if !had || prev.level != c.level || w.Changed(id) {
				full, _ := w.View(id)
				k.view = p.Disclose(full, c.level)
			}
			ps.known[id] = k
			switch {
			case !had:
				ctx.Emit(sim.Event{Type: EventDiscovered, Entity: id, Audience: sim.ToPlayer(pl), Fields: []sim.Field{
					{Key: "level", Value: sim.IntValue(int64(c.level))},
				}})
			case prev.level != c.level:
				ctx.Emit(sim.Event{Type: EventIdentified, Entity: id, Audience: sim.ToPlayer(pl), Fields: []sim.Field{
					{Key: "level", Value: sim.IntValue(int64(c.level))},
					{Key: "previous", Value: sim.IntValue(int64(prev.level))},
				}})
			}
			// Admin diagnostics change every tick; only resend to players
			// when what they may see changed.
			oe := sim.ObservedEntity{View: k.view, Level: c.level, Observer: c.observer, DetectScore: c.detect, Ident: c.ident}
			switch {
			case !had || prev.level != c.level || !sameView(prev.view, k.view):
				obs.Upserts = append(obs.Upserts, oe)
			case prev.observer != c.observer || prev.detect != c.detect || prev.ident != c.ident:
				obs.Diagnostics = append(obs.Diagnostics, oe) // admins only
			}
		}
		ps.visible = visible
		if len(obs.TilesEntered)+len(obs.TilesExited)+len(obs.Upserts)+len(obs.Lost)+len(obs.Diagnostics) > 0 {
			out = append(out, obs)
		}
	}
	return out
}
