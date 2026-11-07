package server

import (
	"cmp"
	"slices"
	"sync"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// subscriberBuffer is how many frames a slow observer may fall behind before
// it is dropped and resynchronized with a fresh snapshot.
const subscriberBuffer = 128

// published is a frame plus what the hub knew when publishing it.
type published struct {
	frame *sim.Frame
	// removedOwner maps removed entity IDs to their last owner, so a
	// player is only told about removals of their own entities.
	removedOwner map[sim.EntityID]sim.PlayerID
	// status is the match status after this frame when it changed.
	status *match.Status
	static *matchStatic
}

// playerObs is one player's current observation under fog of war, built
// purely from frames: the tiles they see and the foreign entities they
// detect, exactly as disclosed to them.
type playerObs struct {
	visible map[sim.Pos]struct{}
	foreign map[sim.EntityID]sim.ObservedEntity
}

// Hub keeps a read-only copy of the latest world state, built purely from
// frames the engine publishes, and fans frames out to observers. Network
// handlers read from the hub and never touch the World.
type Hub struct {
	mu       sync.Mutex
	tick     sim.Tick
	fog      bool
	entities map[sim.EntityID]sim.EntityView
	players  map[sim.PlayerID]*playerObs
	subs     map[*subscription]struct{}
	static   *matchStatic
	status   match.Status
	closed   bool
}

type subscription struct {
	ch chan *published
}

// NewHub creates a hub seeded with the world's initial state. fog says
// whether an observation authority (fog of war) is active; before the
// first tick, players then observe nothing foreign.
func NewHub(tick sim.Tick, initial []sim.EntityView, fog bool) *Hub {
	h := &Hub{
		tick:     tick,
		fog:      fog,
		entities: make(map[sim.EntityID]sim.EntityView, len(initial)),
		players:  make(map[sim.PlayerID]*playerObs),
		subs:     make(map[*subscription]struct{}),
	}
	for _, v := range initial {
		h.entities[v.ID] = v
	}
	return h
}

func (h *Hub) player(p sim.PlayerID) *playerObs {
	po := h.players[p]
	if po == nil {
		po = &playerObs{visible: map[sim.Pos]struct{}{}, foreign: map[sim.EntityID]sim.ObservedEntity{}}
		h.players[p] = po
	}
	return po
}

// Publish applies a frame and forwards it to subscribers. It runs on the
// simulation goroutine and never blocks: a subscriber whose buffer is full
// is closed, and its reader resubscribes to get a fresh snapshot.
func (h *Hub) Publish(f *sim.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	pub := &published{frame: f, removedOwner: make(map[sim.EntityID]sim.PlayerID, len(f.Removed)), static: h.static}
	h.tick = f.Tick
	if v, ok := f.Globals[match.GlobalID]; ok {
		h.status = v.(match.Status)
		st := h.status
		pub.status = &st
	}
	for _, id := range f.Removed {
		pub.removedOwner[id] = h.entities[id].Owner
		delete(h.entities, id)
	}
	for _, v := range f.Changed {
		h.entities[v.ID] = v
	}
	for _, o := range f.Observations {
		po := h.player(o.Player)
		for _, t := range o.TilesEntered {
			po.visible[t] = struct{}{}
		}
		for _, t := range o.TilesExited {
			delete(po.visible, t)
		}
		for _, id := range o.Lost {
			delete(po.foreign, id)
		}
		for _, u := range o.Upserts {
			po.foreign[u.View.ID] = u
		}
		for _, d := range o.Diagnostics {
			if cur, ok := po.foreign[d.View.ID]; ok {
				cur.Observer, cur.DetectScore, cur.Ident = d.Observer, d.DetectScore, d.Ident
				po.foreign[d.View.ID] = cur
			}
		}
	}
	for s := range h.subs {
		select {
		case s.ch <- pub:
		default:
			close(s.ch)
			delete(h.subs, s)
		}
	}
}

// snapshotLocked builds what a viewer currently observes.
func (h *Hub) snapshotLocked(v viewer) *rtsv1.WorldSnapshot {
	snap := &rtsv1.WorldSnapshot{Tick: uint64(h.tick)}
	fog := h.fog && !v.admin
	snap.FogOfWar = fog
	var es []*rtsv1.Entity
	for _, e := range h.entities {
		if fog && (e.Owner == 0 || e.Owner != v.player) {
			continue // foreign entities come only from the observation
		}
		es = append(es, v.entity(e, fog))
	}
	if fog {
		po := h.players[v.player]
		if po != nil {
			for _, oe := range po.foreign {
				es = append(es, v.observed(oe))
			}
			snap.VisibleTiles = tileRuns(po.visible)
		}
	}
	slices.SortFunc(es, func(a, b *rtsv1.Entity) int { return cmp.Compare(a.GetId(), b.GetId()) })
	snap.Entities = es
	snap.Match = h.matchInfoLocked(v)
	return snap
}

// Snapshot returns what a viewer currently observes.
func (h *Hub) Snapshot(v viewer) *rtsv1.WorldSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked(v)
}

// PlayerView returns a player's snapshot and the detections behind it.
func (h *Hub) PlayerView(p sim.PlayerID) (*rtsv1.WorldSnapshot, []*rtsv1.Detection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := h.snapshotLocked(viewer{player: p})
	var dets []*rtsv1.Detection
	if po := h.players[p]; po != nil {
		for id, oe := range po.foreign {
			dets = append(dets, &rtsv1.Detection{
				EntityId: uint64(id), Level: levelToProto(oe.Level), ObserverId: uint64(oe.Observer),
				DetectionScore: oe.DetectScore, IdentificationScore: oe.Ident,
			})
		}
	}
	slices.SortFunc(dets, func(a, b *rtsv1.Detection) int { return cmp.Compare(a.GetEntityId(), b.GetEntityId()) })
	return snap, dets
}

// Fog reports whether fog of war is active.
func (h *Hub) Fog() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fog
}

// Tick returns the last published tick.
func (h *Hub) Tick() sim.Tick {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tick
}

// Subscribe atomically captures the viewer's snapshot and starts delivering
// every subsequent frame, so applying the frames to the snapshot never
// misses or repeats a tick.
func (h *Hub) Subscribe(v viewer) (*rtsv1.WorldSnapshot, *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &subscription{ch: make(chan *published, subscriberBuffer)}
	if h.closed {
		close(s.ch) // the match was replaced: the caller resubscribes
		return h.snapshotLocked(v), s
	}
	h.subs[s] = struct{}{}
	return h.snapshotLocked(v), s
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

// tileRuns encodes a tile set as sorted horizontal runs.
func tileRuns(tiles map[sim.Pos]struct{}) []*rtsv1.TileRun {
	ps := make([]sim.Pos, 0, len(tiles))
	for p := range tiles {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b sim.Pos) int { return cmp.Or(cmp.Compare(a.Y, b.Y), cmp.Compare(a.X, b.X)) })
	var runs []*rtsv1.TileRun
	for _, p := range ps {
		if n := len(runs); n > 0 && runs[n-1].Y == p.Y && runs[n-1].X+runs[n-1].Length == p.X {
			runs[n-1].Length++
			continue
		}
		runs = append(runs, &rtsv1.TileRun{Y: p.Y, X: p.X, Length: 1})
	}
	return runs
}
