package server

import (
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func TestHubDropsLaggingSubscriber(t *testing.T) {
	h := NewHub(0, []sim.EntityView{{ID: 1, Kind: sim.KindUnit, Owner: 1}})
	_, snap, slow := h.Subscribe()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	for i := range subscriberBuffer + 1 {
		h.Publish(&sim.Frame{Tick: sim.Tick(i + 1), Changed: []sim.EntityView{{ID: 1, Pos: sim.Pos{X: int32(i)}}}})
	}
	if h.SubscriberCount() != 0 {
		t.Fatal("lagging subscriber not dropped")
	}
	n := 0
	for range slow.ch {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("drained %d buffered frames", n)
	}
	h.Unsubscribe(slow) // must be safe after drop

	tick, snap, fresh := h.Subscribe()
	defer h.Unsubscribe(fresh)
	if tick != subscriberBuffer+1 || snap[0].Pos.X != subscriberBuffer {
		t.Fatalf("resync snapshot tick %d pos %v", tick, snap[0].Pos)
	}
}
