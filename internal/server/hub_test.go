package server

import (
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func TestHubDropsLaggingSubscriber(t *testing.T) {
	h := NewHub(0, []sim.EntityView{{ID: 1, Kind: sim.KindUnit, Owner: 1}}, false)
	snap, slow := h.Subscribe(adminViewer)
	if len(snap.GetEntities()) != 1 {
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

	snap, fresh := h.Subscribe(adminViewer)
	defer h.Unsubscribe(fresh)
	if snap.GetTick() != subscriberBuffer+1 || snap.GetEntities()[0].GetPosition().GetX() != subscriberBuffer {
		t.Fatalf("resync snapshot = %v", snap)
	}
}
