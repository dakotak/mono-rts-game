package sim

import (
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
)

// Entity is the core data every entity has. Everything else lives in
// plugin-registered components.
type Entity struct {
	ID    EntityID
	Kind  Kind
	Owner PlayerID
	Pos   Pos
}

// Move relocates one entity; see World.MoveEntities.
type Move struct {
	ID EntityID
	To Pos
}

// Errors returned by world mutations.
var (
	ErrOutOfBounds  = errors.New("position out of bounds")
	ErrOccupied     = errors.New("tile occupied")
	ErrNoSuchEntity = errors.New("no such entity")
)

// World is the authoritative simulation state. It must only be accessed from
// the simulation goroutine (or before the engine starts running).
//
// Milestone 1 invariant: every entity occupies exactly one tile and a tile
// holds at most one entity.
type World struct {
	width, height int32
	tick          Tick
	nextID        EntityID
	rng           *Rand
	rules         *Ruleset

	entities map[EntityID]Entity
	ids      []EntityID // sorted ascending
	occupied map[Pos]EntityID

	// components[componentID][entity] = value
	components map[string]map[EntityID]any

	// Change tracking for the current tick.
	dirty   map[EntityID]struct{}
	removed []EntityID
}

func newWorld(width, height int32, seed uint64, rules *Ruleset) *World {
	w := &World{
		width:      width,
		height:     height,
		nextID:     1,
		rng:        NewRand(seed),
		rules:      rules,
		entities:   make(map[EntityID]Entity),
		occupied:   make(map[Pos]EntityID),
		components: make(map[string]map[EntityID]any),
		dirty:      make(map[EntityID]struct{}),
	}
	for _, id := range rules.componentOrder {
		w.components[id] = make(map[EntityID]any)
	}
	return w
}

// Width returns the grid width in tiles.
func (w *World) Width() int32 { return w.width }

// Height returns the grid height in tiles.
func (w *World) Height() int32 { return w.height }

// Tick returns the tick currently being executed (or the last executed tick
// when called between steps).
func (w *World) Tick() Tick { return w.tick }

// Rand returns the world's seeded random source.
func (w *World) Rand() *Rand { return w.rng }

// InBounds reports whether p is on the grid.
func (w *World) InBounds(p Pos) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < w.width && p.Y < w.height
}

// Entity returns the core data of an entity.
func (w *World) Entity(id EntityID) (Entity, bool) {
	e, ok := w.entities[id]
	return e, ok
}

// EntityAt returns the entity occupying p, if any.
func (w *World) EntityAt(p Pos) (EntityID, bool) {
	id, ok := w.occupied[p]
	return id, ok
}

// EntityCount returns the number of live entities.
func (w *World) EntityCount() int { return len(w.ids) }

// EntityIDs returns all live entity IDs in ascending order. The returned
// slice must not be modified.
func (w *World) EntityIDs() []EntityID { return w.ids }

// CreateEntity adds a new entity at pos.
func (w *World) CreateEntity(kind Kind, owner PlayerID, pos Pos) (EntityID, error) {
	if !w.InBounds(pos) {
		return 0, fmt.Errorf("create %v at %v: %w", kind, pos, ErrOutOfBounds)
	}
	if _, taken := w.occupied[pos]; taken {
		return 0, fmt.Errorf("create %v at %v: %w", kind, pos, ErrOccupied)
	}
	id := w.nextID
	w.nextID++
	w.entities[id] = Entity{ID: id, Kind: kind, Owner: owner, Pos: pos}
	w.ids = append(w.ids, id) // IDs are monotonic, so this stays sorted
	w.occupied[pos] = id
	w.dirty[id] = struct{}{}
	return id, nil
}

// RemoveEntity deletes an entity and all of its components.
func (w *World) RemoveEntity(id EntityID) error {
	e, ok := w.entities[id]
	if !ok {
		return fmt.Errorf("remove %d: %w", id, ErrNoSuchEntity)
	}
	delete(w.entities, id)
	delete(w.occupied, e.Pos)
	if i, found := slices.BinarySearch(w.ids, id); found {
		w.ids = slices.Delete(w.ids, i, i+1)
	}
	for _, cid := range w.rules.componentOrder {
		delete(w.components[cid], id)
	}
	delete(w.dirty, id)
	w.removed = append(w.removed, id)
	return nil
}

// MoveEntities relocates several entities simultaneously. It is atomic: it
// either applies every move or returns an error and changes nothing. Because
// moves are simultaneous, an entity may move into a tile that another entity
// in the same batch is vacating.
func (w *World) MoveEntities(moves []Move) error {
	if len(moves) == 0 {
		return nil
	}
	moving := make(map[EntityID]struct{}, len(moves))
	targets := make(map[Pos]EntityID, len(moves))
	for _, m := range moves {
		if _, ok := w.entities[m.ID]; !ok {
			return fmt.Errorf("move %d: %w", m.ID, ErrNoSuchEntity)
		}
		if _, dup := moving[m.ID]; dup {
			return fmt.Errorf("move %d: entity listed twice", m.ID)
		}
		moving[m.ID] = struct{}{}
		if !w.InBounds(m.To) {
			return fmt.Errorf("move %d to %v: %w", m.ID, m.To, ErrOutOfBounds)
		}
		if other, dup := targets[m.To]; dup {
			return fmt.Errorf("move %d and %d to %v: %w", other, m.ID, m.To, ErrOccupied)
		}
		targets[m.To] = m.ID
	}
	for _, m := range moves {
		if occ, ok := w.occupied[m.To]; ok {
			if _, leaving := moving[occ]; !leaving {
				return fmt.Errorf("move %d to %v: %w by %d", m.ID, m.To, ErrOccupied, occ)
			}
		}
	}
	for _, m := range moves {
		delete(w.occupied, w.entities[m.ID].Pos)
	}
	for _, m := range moves {
		e := w.entities[m.ID]
		e.Pos = m.To
		w.entities[m.ID] = e
		w.occupied[m.To] = m.ID
		w.dirty[m.ID] = struct{}{}
	}
	return nil
}

// Derived evaluates a registered derived property for an entity.
func (w *World) Derived(name string, id EntityID) (int64, bool) {
	d, ok := w.rules.derived[name]
	if !ok {
		panic(fmt.Sprintf("sim: derived property %q is not registered", name))
	}
	return d.Compute(w, id)
}

// view builds the observable representation of an entity.
func (w *World) view(id EntityID) EntityView {
	e := w.entities[id]
	v := EntityView{ID: e.ID, Kind: e.Kind, Owner: e.Owner, Pos: e.Pos}
	for _, cid := range w.rules.componentOrder {
		def := w.rules.components[cid]
		if def.attributes == nil {
			continue
		}
		val, ok := w.components[cid][id]
		if !ok {
			continue
		}
		for _, f := range def.attributes(val) {
			v.Attrs = append(v.Attrs, Attr{Key: f.Key, Value: f.Value, Visibility: def.Visibility})
		}
	}
	return v
}

// FullView returns the observable state of every entity, sorted by ID.
func (w *World) FullView() []EntityView {
	out := make([]EntityView, 0, len(w.ids))
	for _, id := range w.ids {
		out = append(out, w.view(id))
	}
	return out
}

// takeChanges returns views of entities changed since the last call and the
// IDs removed since the last call, both sorted, and resets tracking.
func (w *World) takeChanges() ([]EntityView, []EntityID) {
	ids := make([]EntityID, 0, len(w.dirty))
	for id := range w.dirty {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	changed := make([]EntityView, 0, len(ids))
	for _, id := range ids {
		changed = append(changed, w.view(id))
	}
	removed := w.removed
	slices.Sort(removed)
	clear(w.dirty)
	w.removed = nil
	return changed, removed
}

// StateHash returns a hash of the complete authoritative state. Two worlds
// with equal hashes are, for all practical purposes, identical. Component
// values are hashed via their %#v formatting, which is deterministic for the
// plain comparable value types components are restricted to.
func (w *World) StateHash() uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "t%d n%d r%d w%dx%d|", w.tick, w.nextID, w.rng.State(), w.width, w.height)
	for _, id := range w.ids {
		e := w.entities[id]
		fmt.Fprintf(h, "e%d:%d:%d:%d,%d|", e.ID, e.Kind, e.Owner, e.Pos.X, e.Pos.Y)
	}
	for _, cid := range w.rules.componentOrder {
		store := w.components[cid]
		keys := make([]EntityID, 0, len(store))
		for id := range store {
			keys = append(keys, id)
		}
		slices.Sort(keys)
		fmt.Fprintf(h, "c%s|", cid)
		for _, id := range keys {
			fmt.Fprintf(h, "%d=%#v|", id, store[id])
		}
	}
	return h.Sum64()
}
