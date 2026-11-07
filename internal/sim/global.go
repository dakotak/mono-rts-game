package sim

import (
	"fmt"
	"slices"
)

// Global is a typed handle to a world-scoped value owned by a plugin: state
// that belongs to the match rather than to any entity (the match lifecycle,
// per-player statistics). Like component values, T must be a plain
// comparable value type (fixed-size arrays are fine; no pointers, slices or
// maps). Globals are part of StateHash and replay, and every tick's frame
// reports the globals whose value changed (Frame.Globals).
//
// A plugin creates a handle with NewGlobal, registers it with
// Registrar.Global, and reads or writes it from its hooks and systems.
type Global[T comparable] struct {
	id  string
	def T
}

// NewGlobal returns a handle for the global with the given ID (prefixed with
// the owning plugin's name) and initial value.
func NewGlobal[T comparable](id string, initial T) Global[T] {
	return Global[T]{id: id, def: initial}
}

// ID returns the global's identifier.
func (g Global[T]) ID() string { return g.id }

// Def builds the registration for this global.
func (g Global[T]) Def() GlobalDef { return GlobalDef{ID: g.id, initial: g.def} }

// Get returns the current value.
func (g Global[T]) Get(w *World) T {
	v, ok := w.globals[g.id]
	if !ok {
		panic(fmt.Sprintf("sim: global %q is not registered", g.id))
	}
	return v.(T)
}

// Set stores a value. Writing an equal value is a no-op (not reported as
// changed).
func (g Global[T]) Set(w *World, v T) {
	old, ok := w.globals[g.id]
	if !ok {
		panic(fmt.Sprintf("sim: global %q is not registered", g.id))
	}
	if old.(T) == v {
		return
	}
	w.globals[g.id] = v
	w.globalsDirty[g.id] = struct{}{}
}

// GlobalDef registers a global. Build it with Global.Def.
type GlobalDef struct {
	ID      string
	initial any
}

// Globals returns every global's current value, keyed by ID. Values are
// copies (plain values); the map is fresh.
func (w *World) Globals() map[string]any {
	out := make(map[string]any, len(w.globals))
	for k, v := range w.globals {
		out[k] = v
	}
	return out
}

func (w *World) takeGlobals() map[string]any {
	if len(w.globalsDirty) == 0 {
		return nil
	}
	out := make(map[string]any, len(w.globalsDirty))
	for k := range w.globalsDirty {
		out[k] = w.globals[k]
	}
	clear(w.globalsDirty)
	return out
}

func (w *World) globalIDs() []string {
	ids := make([]string, 0, len(w.globals))
	for k := range w.globals {
		ids = append(ids, k)
	}
	slices.Sort(ids)
	return ids
}

// Gate is implemented by the plugin that decides, per tick, whether
// commands may execute and whether systems run (the match lifecycle). At
// most one gate may be registered. Both methods run on the simulation
// goroutine at the points described and must be deterministic.
type Gate interface {
	// AllowCommand is called before each command's handler. A non-nil
	// error fails the command (it is reported as the command's result) and
	// the handler is not called.
	AllowCommand(w *World, cmd Command) error
	// RunSystems is called after commands; false skips every phase this
	// tick. The observation authority still runs, so players keep seeing
	// the (frozen) world.
	RunSystems(w *World) bool
}
