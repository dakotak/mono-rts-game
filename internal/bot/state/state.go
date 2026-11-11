// Package state is the computer player's knowledge of the world, built
// only from what the public GameService sends this player: its own
// entities, the foreign entities it currently detects (already filtered by
// the server), visible tiles, events, command results and match info.
//
// Knowledge has four confidence classes:
//
//   - Current: own entities and foreign entities detected this tick
//     (Knowledge.Own, Knowledge.Foreign).
//   - Remembered: foreign entities no longer detected, with the tick they
//     were last seen (Knowledge.Memory). Never treated as current; they age
//     out after MemoryTicks.
//   - Estimated: values inferred from observations (estimate ranges,
//     defaults from published rules, enemy base guesses).
//   - Unknown: tiles never explored, entities never seen.
//
// The package imports only the public API (gen/rts/v1).
package state

import (
	"cmp"
	"slices"
	"strconv"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
)

// Pos is a tile coordinate.
type Pos struct{ X, Y int32 }

func (p Pos) String() string {
	return "(" + strconv.Itoa(int(p.X)) + "," + strconv.Itoa(int(p.Y)) + ")"
}

// Cheb is the Chebyshev distance.
func Cheb(a, b Pos) int32 { return max(abs(a.X-b.X), abs(a.Y-b.Y)) }

func abs(v int32) int32 { return max(v, -v) }

// Entity is an observed entity with convenient attribute access.
type Entity struct {
	ID       uint64
	Kind     rtsv1.EntityKind
	Owner    uint32
	Pos      Pos
	Passable bool
	Level    rtsv1.IdentificationLevel
	Attrs    map[string]*rtsv1.Value
}

func fromProto(e *rtsv1.Entity) *Entity {
	p := e.GetPosition()
	return &Entity{ID: e.GetId(), Kind: e.GetKind(), Owner: e.GetOwnerId(), Pos: Pos{p.GetX(), p.GetY()},
		Passable: e.GetPassable(), Level: e.GetIdentification(), Attrs: e.GetAttributes()}
}

// Int returns an integer attribute. Estimates return their midpoint and
// ok=true; exact is false for estimates.
func (e *Entity) Int(key string) (v int64, ok, exact bool) {
	a := e.Attrs[key]
	if a == nil {
		return 0, false, false
	}
	if r := a.GetRangeValue(); r != nil {
		return (r.GetLow() + r.GetHigh()) / 2, true, false
	}
	if _, isInt := a.GetKind().(*rtsv1.Value_IntValue); isInt {
		return a.GetIntValue(), true, true
	}
	return 0, false, false
}

// IntOr returns an integer attribute or a default.
func (e *Entity) IntOr(key string, def int64) int64 {
	if v, ok, _ := e.Int(key); ok {
		return v
	}
	return def
}

// Str returns a string attribute.
func (e *Entity) Str(key string) string { return e.Attrs[key].GetStringValue() }

// ID returns an ID-valued attribute (IDs travel as decimal strings).
func (e *Entity) IDAttr(key string) uint64 {
	v, _ := strconv.ParseUint(e.Str(key), 10, 64)
	return v
}

// Has reports whether an attribute is present.
func (e *Entity) Has(key string) bool { return e.Attrs[key] != nil }

// IsUnit reports whether the entity is a unit.
func (e *Entity) IsUnit() bool { return e.Kind == rtsv1.EntityKind_ENTITY_KIND_UNIT }

// IsWell reports whether the entity is an energy well (a resource that
// blocks its tile; wreckage is passable).
func (e *Entity) IsWell() bool { return e.Kind == rtsv1.EntityKind_ENTITY_KIND_RESOURCE && !e.Passable }

// IsWreck reports whether the entity is wreckage.
func (e *Entity) IsWreck() bool { return e.Kind == rtsv1.EntityKind_ENTITY_KIND_RESOURCE && e.Passable }

// Memory is a foreign entity that is no longer detected.
type Memory struct {
	Entity   *Entity
	LastSeen uint64
}

// Well is what the player knows about a well.
type Well struct {
	ID        uint64
	Pos       Pos
	LastSeen  uint64
	Current   bool  // detected now
	Energy    int64 // estimate; -1 unknown
	Depleted  bool  // empty when last known (regenerating wells refill)
	Unlimited bool
	Regen     int64 // regeneration per tick, when disclosed
}

// MemoryTicks is how long remembered enemies are kept.
const MemoryTicks = 600

// Knowledge is the player's world model.
type Knowledge struct {
	Me            uint32
	Name          string
	Tick          uint64
	Width, Height int32
	TickRateHz    uint32
	Budget        int // unit-commands per tick (0 = unlimited)
	Fog           bool
	CommandTypes  map[string]bool
	Rules         *Rules
	Match         *rtsv1.MatchInfo

	Own     map[uint64]*Entity // current
	Foreign map[uint64]*Entity // current (detected)
	Memory  map[uint64]*Memory // remembered foreign entities
	Wells   map[uint64]*Well
	Blocked map[Pos]bool // impassable terrain seen

	visible  []bool // current visibility (fog) or all true
	explored []bool
	Explored int

	// Per-tick news, reset by BeginTick.
	Events  []*rtsv1.Event
	Results []*rtsv1.CommandResult
	Reset   bool // the match changed (new world) this tick
	// Lost own units this tick (removed from Own).
	LostOwn []uint64

	ownCache []*Entity
}

// New returns empty knowledge.
func New() *Knowledge {
	return &Knowledge{Own: map[uint64]*Entity{}, Foreign: map[uint64]*Entity{}, Memory: map[uint64]*Memory{},
		Wells: map[uint64]*Well{}, Blocked: map[Pos]bool{}, CommandTypes: map[string]bool{}}
}

// SetWorld records what Connect returned.
func (k *Knowledge) SetWorld(resp *rtsv1.ConnectResponse) error {
	w := resp.GetWorld()
	k.Me, k.Name = resp.GetPlayerId(), resp.GetPlayerName()
	k.Width, k.Height, k.TickRateHz, k.Budget = w.GetWidth(), w.GetHeight(), w.GetTickRateHz(), int(w.GetUnitCommandBudget())
	for _, t := range w.GetCommandTypes() {
		k.CommandTypes[t] = true
	}
	r, err := ParseRules(w.GetRulesJson())
	if err != nil {
		return err
	}
	k.Rules = r
	k.resize()
	if resp.GetMatch() != nil {
		k.applyMatch(resp.GetMatch(), false)
	}
	return nil
}

func (k *Knowledge) resize() {
	n := int(k.Width) * int(k.Height)
	k.visible, k.explored, k.Explored = make([]bool, n), make([]bool, n), 0
}

func (k *Knowledge) idx(p Pos) int { return int(p.Y)*int(k.Width) + int(p.X) }

// InBounds reports whether p is on the map.
func (k *Knowledge) InBounds(p Pos) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < k.Width && p.Y < k.Height
}

// Visible reports current visibility of a tile.
func (k *Knowledge) Visible(p Pos) bool { return k.InBounds(p) && (!k.Fog || k.visible[k.idx(p)]) }

// IsExplored reports whether a tile was ever visible.
func (k *Knowledge) IsExplored(p Pos) bool { return k.InBounds(p) && (!k.Fog || k.explored[k.idx(p)]) }

func (k *Knowledge) see(p Pos, on bool) {
	if !k.InBounds(p) {
		return
	}
	i := k.idx(p)
	k.visible[i] = on
	if on && !k.explored[i] {
		k.explored[i] = true
		k.Explored++
	}
}

// BeginTick clears per-tick news.
func (k *Knowledge) BeginTick() {
	k.ownCache = nil
	k.Events, k.Results, k.Reset, k.LostOwn = nil, nil, false, nil
}

func (k *Knowledge) applyMatch(m *rtsv1.MatchInfo, snapshot bool) {
	if m == nil {
		return
	}
	if k.Match != nil && k.Match.GetMatchId() != m.GetMatchId() {
		// New world: forget everything about the old one.
		k.Reset = true
		k.Own, k.Foreign, k.Memory, k.Wells, k.Blocked = map[uint64]*Entity{}, map[uint64]*Entity{}, map[uint64]*Memory{}, map[uint64]*Well{}, map[Pos]bool{}
		if m.GetWidth() > 0 {
			k.Width, k.Height = m.GetWidth(), m.GetHeight()
		}
		k.resize()
	}
	if id := m.GetYourPlayerId(); id != 0 {
		k.Me = id
	}
	k.Match = m
}

// ApplySnapshot replaces everything with a snapshot.
func (k *Knowledge) ApplySnapshot(s *rtsv1.WorldSnapshot) {
	defer func() { k.ownCache = nil }()
	k.applyMatch(s.GetMatch(), true)
	k.Tick = s.GetTick()
	k.Fog = s.GetFogOfWar()
	// Foreign entities seen before but not in the snapshot become memories.
	now := map[uint64]bool{}
	for _, e := range s.GetEntities() {
		now[e.GetId()] = true
	}
	for id, e := range k.Foreign {
		if !now[id] {
			k.remember(e)
		}
	}
	k.Own, k.Foreign = map[uint64]*Entity{}, map[uint64]*Entity{}
	for _, e := range s.GetEntities() {
		k.upsert(fromProto(e))
	}
	clear(k.visible)
	for _, r := range s.GetVisibleTiles() {
		for i := range r.GetLength() {
			k.see(Pos{r.GetX() + i, r.GetY()}, true)
		}
	}
	if !k.Fog {
		for i := range k.visible {
			k.visible[i] = true
		}
	}
}

// ApplyUpdate merges one tick update.
func (k *Knowledge) ApplyUpdate(u *rtsv1.TickUpdate) {
	defer func() { k.ownCache = nil }()
	k.applyMatch(u.GetMatch(), false)
	k.Tick = u.GetTick()
	for _, p := range u.GetTilesEntered() {
		k.see(Pos{p.GetX(), p.GetY()}, true)
	}
	for _, p := range u.GetTilesExited() {
		k.see(Pos{p.GetX(), p.GetY()}, false)
	}
	for _, e := range u.GetChanged() {
		k.upsert(fromProto(e))
	}
	for _, id := range u.GetRemoved() {
		if _, ok := k.Own[id]; ok {
			delete(k.Own, id)
			k.LostOwn = append(k.LostOwn, id)
			continue
		}
		if e, ok := k.Foreign[id]; ok {
			delete(k.Foreign, id)
			k.remember(e)
		}
	}
	k.Events = append(k.Events, u.GetEvents()...)
	k.Results = append(k.Results, u.GetCommandResults()...)
	for _, ev := range u.GetEvents() {
		if ev.GetType() == "energy.well_depleted" {
			if w := k.Wells[ev.GetEntityId()]; w != nil {
				w.Depleted = true
			}
		}
	}
	// Age out stale memories (bounded memory).
	for id, m := range k.Memory {
		if k.Tick > m.LastSeen+MemoryTicks && !m.Entity.IsWell() {
			delete(k.Memory, id)
		}
	}
	for _, w := range k.Wells {
		_, w.Current = k.Foreign[w.ID]
	}
}

func (k *Knowledge) remember(e *Entity) {
	k.Memory[e.ID] = &Memory{Entity: e, LastSeen: k.Tick}
}

func (k *Knowledge) upsert(e *Entity) {
	if e.Owner != 0 && e.Owner == k.Me {
		k.Own[e.ID] = e
		return
	}
	k.Foreign[e.ID] = e
	delete(k.Memory, e.ID)
	switch {
	case e.IsWell():
		w := k.Wells[e.ID]
		if w == nil {
			w = &Well{ID: e.ID, Energy: -1}
			k.Wells[e.ID] = w
		}
		w.Pos, w.LastSeen, w.Current = e.Pos, k.Tick, true
		if v, ok, _ := e.Int("wells.energy"); ok {
			w.Energy = v
			switch r := e.Attrs["wells.energy"].GetRangeValue(); {
			case r != nil && r.GetHigh() == 0, r == nil && v == 0:
				w.Depleted = true
			case r != nil && r.GetLow() > 0, r == nil && v > 0:
				if w.Depleted && w.Regen == 0 {
					w.Regen = 1 // seen refilling: it regenerates (rate not disclosed)
				}
				w.Depleted = false
			}
		}
		w.Unlimited = e.Attrs["wells.unlimited"].GetBoolValue()
		w.Regen = e.IntOr("wells.regeneration", w.Regen)
	case e.Kind == rtsv1.EntityKind_ENTITY_KIND_TERRAIN && !e.Passable:
		k.Blocked[e.Pos] = true
	}
}

// Sorted returns map keys in ascending order (deterministic iteration).
func Sorted[V any](m map[uint64]V) []uint64 {
	ids := make([]uint64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// OwnUnits returns own units in ID order. The slice is cached until the
// next observation is applied: callers must not modify it.
func (k *Knowledge) OwnUnits() []*Entity {
	if k.ownCache != nil {
		return k.ownCache
	}
	out := []*Entity{}
	for _, id := range Sorted(k.Own) {
		if e := k.Own[id]; e.IsUnit() {
			out = append(out, e)
		}
	}
	k.ownCache = out[:len(out):len(out)] // appends by callers copy
	return k.ownCache
}

// Enemies returns currently detected enemy units in ID order.
func (k *Knowledge) Enemies() []*Entity {
	var out []*Entity
	for _, id := range Sorted(k.Foreign) {
		if e := k.Foreign[id]; e.IsUnit() && e.Owner != 0 && e.Owner != k.Me {
			out = append(out, e)
		}
	}
	return out
}

// RememberedEnemies returns enemy units seen before but not now, most
// recently seen first (ties by ID).
func (k *Knowledge) RememberedEnemies() []*Memory {
	var out []*Memory
	for _, id := range Sorted(k.Memory) {
		if m := k.Memory[id]; m.Entity.IsUnit() && m.Entity.Owner != k.Me {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b *Memory) int { return cmp.Compare(b.LastSeen, a.LastSeen) })
	return out
}

// Wrecks returns detected wrecks in ID order.
func (k *Knowledge) Wrecks() []*Entity {
	var out []*Entity
	for _, id := range Sorted(k.Foreign) {
		if e := k.Foreign[id]; e.IsWreck() {
			out = append(out, e)
		}
	}
	return out
}

// UsableWells returns wells worth mining, in ID order: not known to be
// empty, or empty but regenerating (a trickle).
func (k *Knowledge) UsableWells() []*Well {
	var out []*Well
	for _, id := range Sorted(k.Wells) {
		if w := k.Wells[id]; !w.Depleted || w.Regen > 0 || w.Unlimited {
			out = append(out, w)
		}
	}
	return out
}

// Occupied reports whether a tile is known to hold a blocking entity now.
func (k *Knowledge) Occupied(p Pos, occ map[Pos]bool) bool { return k.Blocked[p] || occ[p] }

// OccupancyMap returns the tiles held by currently known blocking entities.
func (k *Knowledge) OccupancyMap() map[Pos]bool {
	occ := make(map[Pos]bool, len(k.Own)+len(k.Foreign))
	for _, e := range k.Own {
		if !e.Passable {
			occ[e.Pos] = true
		}
	}
	for _, e := range k.Foreign {
		if !e.Passable {
			occ[e.Pos] = true
		}
	}
	return occ
}

// Running reports whether the match accepts gameplay commands.
func (k *Knowledge) Running() bool {
	return k.Match == nil || k.Match.GetState() == rtsv1.MatchState_MATCH_STATE_RUNNING
}
