package server

import (
	"slices"
	"sync"

	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// subscriberBuffer is how many frames a slow observer may fall behind before
// it is dropped and resynchronized with a fresh snapshot.
const subscriberBuffer = 128

// Hub keeps a read-only copy of the latest world state, built purely from
// frames the engine publishes, and fans frames out to observers. Network
// handlers read from the hub and never touch the World.
type Hub struct {
	mu       sync.Mutex
	tick     sim.Tick
	entities map[sim.EntityID]sim.EntityView
	subs     map[*subscription]struct{}
}

type subscription struct {
	ch chan *sim.Frame
}

// NewHub creates a hub seeded with the world's initial state.
func NewHub(tick sim.Tick, initial []sim.EntityView) *Hub {
	h := &Hub{
		tick:     tick,
		entities: make(map[sim.EntityID]sim.EntityView, len(initial)),
		subs:     make(map[*subscription]struct{}),
	}
	for _, v := range initial {
		h.entities[v.ID] = v
	}
	return h
}

// Publish applies a frame and forwards it to subscribers. It runs on the
// simulation goroutine and never blocks: a subscriber whose buffer is full
// is closed, and its reader resubscribes to get a fresh snapshot.
func (h *Hub) Publish(f *sim.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tick = f.Tick
	for _, v := range f.Changed {
		h.entities[v.ID] = v
	}
	for _, id := range f.Removed {
		delete(h.entities, id)
	}
	for s := range h.subs {
		select {
		case s.ch <- f:
		default:
			close(s.ch)
			delete(h.subs, s)
		}
	}
}

// snapshotLocked returns all entities sorted by ID.
func (h *Hub) snapshotLocked() []sim.EntityView {
	out := make([]sim.EntityView, 0, len(h.entities))
	for _, v := range h.entities {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b sim.EntityView) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// Snapshot returns the latest tick and all entities.
func (h *Hub) Snapshot() (sim.Tick, []sim.EntityView) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tick, h.snapshotLocked()
}

// Subscribe atomically captures a snapshot and starts delivering every
// subsequent frame, so applying the frames to the snapshot never misses or
// repeats a tick.
func (h *Hub) Subscribe() (sim.Tick, []sim.EntityView, *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &subscription{ch: make(chan *sim.Frame, subscriberBuffer)}
	h.subs[s] = struct{}{}
	return h.tick, h.snapshotLocked(), s
}

// Unsubscribe stops delivery. It is safe to call after the hub dropped s.
func (h *Hub) Unsubscribe(s *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
}

// SubscriberCount returns the number of active observers.
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
