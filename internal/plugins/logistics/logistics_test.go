package logistics_test

import (
	"math/rand/v2"
	"strings"
	"testing"

	logisticsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/logistics/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/logistics"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func harness(t testing.TB, logisticsCfg string) *pt.Harness {
	cfg := map[string]string{"energy": `{"unit_capacity": 100, "initial_energy": 0, "transfer_rate": 5}`}
	if logisticsCfg != "" {
		cfg["logistics"] = logisticsCfg
	}
	return pt.New(t, cfg)
}

func supply(h *pt.Harness, slot int, to sim.EntityID, reserve, fill, resume int64, from ...sim.EntityID) string {
	ids := make([]uint64, len(from))
	for i, f := range from {
		ids[i] = uint64(f)
	}
	return h.Submit(slot, logistics.CommandSupply, &logisticsv1.SupplyCommand{UnitIds: ids, ReceiverId: uint64(to), Reserve: reserve, FillTo: fill, ResumeBelow: resume})
}

func ok(t *testing.T, f *sim.Frame, id string) {
	t.Helper()
	if r, found := pt.Result(f, id); !found || !r.OK {
		t.Fatalf("command %s: %+v", id, r)
	}
}

func status(h *pt.Harness, from, to sim.EntityID) string {
	for _, l := range logistics.Links(h.W()) {
		if l.From == from && l.To == to {
			return l.Status
		}
	}
	return "none"
}

func total(h *pt.Harness, us ...sim.EntityID) int64 {
	var n int64
	for _, u := range us {
		n += h.Energy(u)
	}
	return n
}

func TestReserveFillAndResume(t *testing.T) {
	h := harness(t, `{"range": {"default": 2}}`)
	src := h.Unit(1, 5, 5, 100)
	dst := h.Unit(1, 7, 5, 0)
	c := supply(h, 1, dst, 30, 50, 30, src)
	f := h.Step()
	ok(t, f, c)
	if h.Energy(dst) != 5 || h.Energy(src) != 95 {
		t.Fatalf("first tick: src %d dst %d (rate 5)", h.Energy(src), h.Energy(dst))
	}
	h.Steps(20)
	if h.Energy(dst) != 50 || h.Energy(src) != 50 || status(h, src, dst) != "satisfied" {
		t.Fatalf("after fill: src %d dst %d status %s", h.Energy(src), h.Energy(dst), status(h, src, dst))
	}
	// Above resume_below: the link rests (no top-up oscillation).
	energy.SetStored(h.W(), dst, 35)
	h.Steps(3)
	if h.Energy(dst) != 35 {
		t.Fatalf("topped up above resume_below: %d", h.Energy(dst))
	}
	// Below resume_below: resumes, until the supplier hits its reserve.
	energy.SetStored(h.W(), dst, 0)
	h.Steps(20)
	if h.Energy(src) != 30 || h.Energy(dst) != 20 || status(h, src, dst) != "reserve" {
		t.Fatalf("reserve: src %d dst %d status %s", h.Energy(src), h.Energy(dst), status(h, src, dst))
	}
}

// A relay chain moves energy one hop per tick, conserving the total.
func TestRelayChainConservesAndDoesNotTeleport(t *testing.T) {
	h := harness(t, `{"range": {"default": 2}}`)
	a := h.Unit(1, 2, 2, 100)
	b := h.Unit(1, 4, 2, 0)
	c := h.Unit(1, 6, 2, 0)
	d := h.Unit(1, 8, 2, 0)
	supply(h, 1, b, 0, 0, 0, a)
	supply(h, 1, c, 0, 0, 0, b)
	supply(h, 1, d, 0, 0, 0, c)
	for tick := 1; tick <= 60; tick++ {
		h.Step()
		if got := total(h, a, b, c, d); got != 100 {
			t.Fatalf("tick %d: total %d", tick, got)
		}
		if tick < 3 && h.Energy(d) != 0 {
			t.Fatalf("tick %d: energy reached the end of a 3-hop chain early", tick)
		}
		if tick == 3 && h.Energy(d) == 0 {
			t.Fatal("tick 3: nothing reached the end")
		}
	}
	if h.Energy(d) != 100 {
		t.Fatalf("end of chain holds %d, want everything (100)", h.Energy(d))
	}
}

func TestContentionForReceiverCapacity(t *testing.T) {
	h := harness(t, `{"range": {"default": 3}}`)
	dst := h.Unit(1, 10, 10, 90) // 10 free
	s1 := h.Unit(1, 9, 10, 100)
	s2 := h.Unit(1, 11, 10, 100)
	s3 := h.Unit(1, 10, 11, 100)
	supply(h, 1, dst, 0, 0, 0, s1, s2, s3)
	h.Step()
	if h.Energy(dst) != 100 || total(h, dst, s1, s2, s3) != 390 {
		t.Fatalf("dst %d total %d", h.Energy(dst), total(h, dst, s1, s2, s3))
	}
	// 10 split over three suppliers of 5: 3, 3, 4 (remainder to the
	// highest ID).
	got := map[int64]int{}
	for _, s := range []sim.EntityID{s1, s2, s3} {
		got[100-h.Energy(s)]++
	}
	if got[3] != 2 || got[4] != 1 {
		t.Fatalf("split = %v", got)
	}
}

func TestSupplierSplitsRateOverReceivers(t *testing.T) {
	h := harness(t, `{"range": {"default": 3}}`)
	src := h.Unit(1, 10, 10, 100)
	r1 := h.Unit(1, 9, 10, 0)
	r2 := h.Unit(1, 11, 10, 0)
	supply(h, 1, r1, 0, 0, 0, src)
	supply(h, 1, r2, 0, 0, 0, src)
	h.Step()
	if a, b := h.Energy(r1), h.Energy(r2); a+b != 5 || max(a, b)-min(a, b) > 1 {
		t.Fatalf("rate split %d/%d", a, b)
	}
}

func TestOutOfRangeWaitsAndRangeUpgrades(t *testing.T) {
	rules := strings.Replace(pt.Rules, `"attributes": {`, `"attributes": {
    "transfer_range": {"default": 1, "min": 1, "max": 4, "mass": "0", "cost": "v * 10", "duration": "1"},`, 1)
	h := pt.New(t, map[string]string{
		"attributes": rules,
		"energy":     `{"unit_capacity": 100, "initial_energy": 0, "transfer_rate": 5}`,
		"logistics":  `{"range": {"property": "transfer_range", "default": 1}}`,
	})
	src := h.Unit(1, 5, 5, 100)
	dst := h.Unit(1, 8, 5, 0)
	supply(h, 1, dst, 0, 0, 0, src)
	h.Steps(3)
	if h.Energy(dst) != 0 || status(h, src, dst) != "out_of_range" {
		t.Fatalf("out of range: dst %d status %s", h.Energy(dst), status(h, src, dst))
	}
	i, _ := h.Attrs().Index("transfer_range")
	h.Attrs().Apply(h.W(), src, i, 3, 0)
	h.Step()
	if h.Energy(dst) != 5 || status(h, src, dst) != "supplying" {
		t.Fatalf("upgraded range: dst %d status %s", h.Energy(dst), status(h, src, dst))
	}
	// The receiver walks away and comes back: the link persists.
	h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(dst), Destination: &rtsv1.Position{X: 20, Y: 5}}}})
	h.Steps(60)
	before := h.Energy(dst)
	h.Steps(3)
	if h.Energy(dst) != before || status(h, src, dst) != "out_of_range" {
		t.Fatal("supplied out of range")
	}
	h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(dst), Destination: &rtsv1.Position{X: 7, Y: 5}}}})
	h.Steps(80)
	if h.Energy(dst) <= before {
		t.Fatal("did not resume after returning to range")
	}
}

func TestDestroyedParticipants(t *testing.T) {
	h := harness(t, `{"range": {"default": 2}}`)
	src := h.Unit(1, 5, 5, 100)
	dst := h.Unit(1, 6, 5, 0)
	src2 := h.Unit(1, 4, 5, 100)
	other := h.Unit(1, 3, 5, 0)
	supply(h, 1, dst, 0, 0, 0, src)
	supply(h, 1, other, 0, 0, 0, src2)
	h.Step()
	h.W().RemoveEntity(dst)
	h.W().RemoveEntity(src2)
	f := h.Step()
	ev := pt.Events([]*sim.Frame{f}, logistics.EventEnded)
	if len(ev) != 1 || ev[0].Entity != src || pt.Field(ev[0], "reason").Str != "receiver_lost" {
		t.Fatalf("ended = %v", ev)
	}
	if len(logistics.Links(h.W())) != 0 {
		t.Fatalf("links left: %v", logistics.Links(h.W()))
	}
}

func TestRejectedLinks(t *testing.T) {
	h := harness(t, "")
	a := h.Unit(1, 1, 1, 0)
	b := h.Unit(1, 2, 1, 0)
	c := h.Unit(1, 3, 1, 0)
	enemy := h.Unit(2, 4, 1, 0)
	supply(h, 1, b, 0, 0, 0, a)
	supply(h, 1, c, 0, 0, 0, b)
	h.Step()
	cases := map[string]string{
		"cycle":   supply(h, 1, a, 0, 0, 0, c),
		"self":    supply(h, 1, a, 0, 0, 0, a),
		"foreign": supply(h, 1, enemy, 0, 0, 0, a),
		"theirs":  supply(h, 1, a, 0, 0, 0, enemy),
		"resume":  supply(h, 1, c, 0, 10, 20, a),
	}
	f := h.Step()
	for name, id := range cases {
		if r, _ := pt.Result(f, id); r.OK {
			t.Errorf("%s accepted", name)
		}
	}
	// At most MaxLinks receivers per supplier.
	hub := h.Unit(1, 10, 10, 0)
	var last string
	for i := range logistics.MaxLinks + 1 {
		last = supply(h, 1, h.Unit(1, int32(9+i), 12, 0), 0, 0, 0, hub)
	}
	if r, _ := pt.Result(h.Step(), last); r.OK {
		t.Fatal("fifth link accepted")
	}
}

// Random acyclic networks: totals are conserved, no store leaves
// [0, capacity], and reserves are respected.
func TestRandomNetworksConserveEnergy(t *testing.T) {
	for seed := range uint64(5) {
		h := harness(t, `{"range": {"default": 3}}`)
		r := rand.New(rand.NewPCG(seed, 1))
		var us []sim.EntityID
		var sum int64
		for i := range 40 {
			e := int64(r.IntN(101))
			us = append(us, h.Unit(1, int32(i%8)*2, int32(i/8)*2, e))
			sum += e
		}
		for range 80 {
			from, to := us[r.IntN(len(us))], us[r.IntN(len(us))]
			supply(h, 1, to, int64(r.IntN(30)), int64(r.IntN(101)), 0, from)
		}
		for tick := range 100 {
			h.Step()
			var got int64
			for _, u := range us {
				e := h.Energy(u)
				if e < 0 || e > 100 {
					t.Fatalf("seed %d tick %d: store %d", seed, tick, e)
				}
				got += e
			}
			if got != sum {
				t.Fatalf("seed %d tick %d: total %d want %d", seed, tick, got, sum)
			}
		}
		if len(logistics.Links(h.W())) == 0 {
			t.Fatal("no links accepted")
		}
	}
}

func TestLogisticsReplay(t *testing.T) {
	run := func(log [][]sim.Command) ([]uint64, [][]sim.Command) {
		h := harness(t, `{"range": {"default": 3}}`)
		r := rand.New(rand.NewPCG(9, 9))
		var us []sim.EntityID
		for i := range 30 {
			us = append(us, h.Unit(1, int32(i%6)*2, int32(i/6)*2, int64(r.IntN(101))))
		}
		var hs []uint64
		var out [][]sim.Command
		for tick := range 60 {
			if log != nil {
				h.Engine.StepWith(log[tick])
			} else {
				if tick < 20 {
					supply(h, 1, us[r.IntN(30)], int64(r.IntN(20)), 0, 0, us[r.IntN(30)])
				}
				out = append(out, h.Step().Commands)
			}
			hs = append(hs, h.W().StateHash())
		}
		return hs, out
	}
	a, log := run(nil)
	b, _ := run(log)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("tick %d differs on replay", i+1)
		}
	}
}
