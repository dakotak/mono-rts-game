package sim

import (
	"fmt"
	"slices"
)

// Component is a typed handle to a plugin-defined component store. Values
// must be plain comparable value types (no pointers, slices or maps) so that
// they are safely copyable, hashable and cheap to diff.
//
// A plugin creates a handle with NewComponent, registers it with
// Registrar.Component, and then uses the handle inside its systems and
// command handlers.
type Component[T comparable] struct {
	id string
}

// NewComponent returns a handle for the component with the given ID. The ID
// must be prefixed with the owning plugin's name, e.g. "movement.order".
func NewComponent[T comparable](id string) Component[T] {
	return Component[T]{id: id}
}

// ID returns the component's identifier.
func (c Component[T]) ID() string { return c.id }

// Def builds a registration for this component. attributes, if non-nil,
// converts a value into observable attributes shown with the given
// visibility. Components without attributes are internal to the simulation.
func (c Component[T]) Def(vis Visibility, attributes func(T) []Field) ComponentDef {
	def := ComponentDef{ID: c.id, Visibility: vis}
	if attributes != nil {
		def.attributes = func(v any) []Field { return attributes(v.(T)) }
	}
	return def
}

func (c Component[T]) store(w *World) map[EntityID]any {
	s, ok := w.components[c.id]
	if !ok {
		panic(fmt.Sprintf("sim: component %q is not registered", c.id))
	}
	return s
}

// Get returns the component value for an entity.
func (c Component[T]) Get(w *World, id EntityID) (T, bool) {
	v, ok := c.store(w)[id]
	if !ok {
		var zero T
		return zero, false
	}
	return v.(T), true
}

// Has reports whether the entity has this component.
func (c Component[T]) Has(w *World, id EntityID) bool {
	_, ok := c.store(w)[id]
	return ok
}

// Set stores a value for an entity. It panics if the entity does not exist.
func (c Component[T]) Set(w *World, id EntityID, v T) {
	s := c.store(w)
	if _, ok := w.entities[id]; !ok {
		panic(fmt.Sprintf("sim: set component %q on missing entity %d", c.id, id))
	}
	if old, ok := s[id]; ok && old.(T) == v {
		return
	}
	s[id] = v
	w.dirty[id] = struct{}{}
}

// Remove deletes the component from an entity, if present.
func (c Component[T]) Remove(w *World, id EntityID) {
	s := c.store(w)
	if _, ok := s[id]; ok {
		delete(s, id)
		w.dirty[id] = struct{}{}
	}
}

// Entities returns the IDs of all entities with this component, ascending.
func (c Component[T]) Entities(w *World) []EntityID {
	s := c.store(w)
	keys := make([]EntityID, 0, len(s))
	for id := range s {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	return keys
}

// ComponentDef registers a component. Build it with Component.Def.
type ComponentDef struct {
	ID         string
	Visibility Visibility
	attributes func(any) []Field
}
