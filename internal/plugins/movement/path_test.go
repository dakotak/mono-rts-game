package movement_test

import (
	"fmt"
	"testing"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// wallCfg: a vertical wall at x=10, y=0..14, with a gap at y=15; rough
// ground (cost 4) at x=5..7, y=0..3.
const wallCfg = `{
  "types": {"wall": {"passable": false}, "rough": {"passable": true, "cost": 4}},
  "tiles": [{"type": "wall", "x": 10, "y": 0, "w": 1, "h": 15}, {"type": "rough", "x": 5, "y": 0, "w": 3, "h": 4}]
}`

func ph(t *testing.T, movementCfg string) *pt.Harness {
	return pt.New(t, map[string]string{
		"terrain":  wallCfg,
		"movement": movementCfg,
		"energy":   `{"unit_capacity": 1000, "initial_energy": 0}`,
	})
}

func moveTo(h *pt.Harness, slot int, u sim.EntityID, x, y int32) {
	h.Submit(slot, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: x, Y: y}}}})
}

func posOf(h *pt.Harness, u sim.EntityID) sim.Pos {
	e, _ := h.W().Entity(u)
	return e.Pos
}

func trace(h *pt.Harness, u sim.EntityID, ticks int) []sim.Pos {
	var out []sim.Pos
	for range ticks {
		h.Step()
		out = append(out, posOf(h, u))
	}
	return out
}

func TestPathAroundWall(t *testing.T) {
	h := ph(t, `{"pathfinding": "auto"}`)
	u := h.Unit(1, 8, 5, 0)
	moveTo(h, 1, u, 12, 5)
	tr := trace(h, u, 40)
	if posOf(h, u) != (sim.Pos{X: 12, Y: 5}) {
		t.Fatalf("did not get around the wall: ended at %v", posOf(h, u))
	}
	for _, p := range tr {
		if p.X == 10 && p.Y < 15 {
			t.Fatalf("walked through the wall at %v", p)
		}
	}
	// Shortest: 1 greedy step, then down to the gap at y=15 and back up
	// (10 + 10), plus one extra step on each side of the gap because routes
	// may not cut the wall's corner: 23 positions.
	steps := 0
	for i := range tr {
		if i == 0 || tr[i] != tr[i-1] {
			steps++
		}
	}
	if steps > 23 {
		t.Fatalf("route too long: %d steps", steps)
	}
}

func TestPathsAreDeterministic(t *testing.T) {
	run := func() string {
		h := ph(t, `{"pathfinding": "always"}`)
		for i := range 6 {
			u := h.Unit(1, 2+int32(i), 8, 0)
			moveTo(h, 1, u, 14+int32(i%3), 2+int32(i))
		}
		h.Steps(60)
		return fmt.Sprint(h.W().StateHash())
	}
	if a, b := run(), run(); a != b {
		t.Fatal("pathfinding differs between identical runs")
	}
}

func TestEquivalentRoutesTieBreak(t *testing.T) {
	// From (0,0) to (3,1) every 3-step route costs the same; the planner
	// must pick the straightest one deterministically: (1,1),(2,1),(3,1).
	h := ph(t, `{"pathfinding": "always"}`)
	u := h.Unit(1, 0, 10, 0)
	moveTo(h, 1, u, 3, 11)
	got := fmt.Sprint(trace(h, u, 3))
	if got != "[(1,11) (2,11) (3,11)]" {
		t.Fatalf("route = %s", got)
	}
}

func TestCostlyTerrainAvoidedWhenCheaper(t *testing.T) {
	h := ph(t, `{"pathfinding": "always"}`)
	u := h.Unit(1, 4, 1, 0)
	moveTo(h, 1, u, 8, 1)
	tr := trace(h, u, 30)
	for _, p := range tr {
		if p.X >= 5 && p.X <= 7 && p.Y <= 3 {
			t.Fatalf("crossed costly terrain at %v instead of going around", p)
		}
	}
	if posOf(h, u) != (sim.Pos{X: 8, Y: 1}) {
		t.Fatalf("ended at %v", posOf(h, u))
	}
	// In auto mode the unit walks straight through but slower (cost 4).
	h2 := ph(t, `{"pathfinding": "auto"}`)
	v := h2.Unit(1, 4, 1, 0)
	moveTo(h2, 1, v, 8, 1)
	tr2 := trace(h2, v, 6)
	if tr2[0] != (sim.Pos{X: 5, Y: 1}) || tr2[3] != (sim.Pos{X: 5, Y: 1}) || tr2[4] != (sim.Pos{X: 6, Y: 1}) {
		t.Fatalf("rough terrain did not slow the unit: %v", tr2)
	}
}

func TestUnreachableDestination(t *testing.T) {
	// Enclose (20,20) completely with walls.
	h := pt.New(t, map[string]string{
		"terrain": `{"types": {"wall": {"passable": false}}, "tiles": [
			{"type": "wall", "x": 19, "y": 19, "w": 3, "h": 1}, {"type": "wall", "x": 19, "y": 21, "w": 3, "h": 1},
			{"type": "wall", "x": 19, "y": 20}, {"type": "wall", "x": 21, "y": 20}]}`,
		"energy": `{"unit_capacity": 1000, "initial_energy": 0}`,
	})
	u := h.Unit(1, 15, 20, 0)
	moveTo(h, 1, u, 20, 20)
	fs := h.Steps(30)
	ev := pt.Events(fs, movement.EventCancelled)
	if len(ev) != 1 || pt.Field(ev[0], "reason").Str != "unreachable" {
		t.Fatalf("cancelled events = %+v", ev)
	}
	// It got as close as possible first.
	if p := posOf(h, u); movementCheb(p, sim.Pos{X: 20, Y: 20}) != 2 {
		t.Fatalf("stopped at %v", p)
	}
}

func movementCheb(a, b sim.Pos) int32 {
	dx, dy := a.X-b.X, a.Y-b.Y
	return max(dx, -dx, dy, -dy)
}

func TestCongestionReplansAroundBlockers(t *testing.T) {
	// A unit whose straight route is blocked by a parked enemy line walks
	// around it after replanning.
	h := ph(t, `{"pathfinding": "auto", "replan_after_blocked": 2, "max_blocked_ticks": 0}`)
	u := h.Unit(1, 20, 5, 0)
	for y := int32(3); y <= 7; y++ {
		h.Unit(2, 22, y, 0) // parked wall of units
	}
	moveTo(h, 1, u, 25, 5)
	h.Steps(30)
	if p := posOf(h, u); p != (sim.Pos{X: 25, Y: 5}) {
		t.Fatalf("stuck behind units at %v", p)
	}
}

func TestGroupMove(t *testing.T) {
	h := ph(t, `{}`)
	var us []uint64
	for i := range 9 {
		us = append(us, uint64(h.Unit(1, int32(i), 20, 0)))
	}
	enemy := h.Unit(2, 30, 25, 0) // occupies a tile near the destination
	us = append(us, uint64(enemy), 777)
	id := h.Submit(1, movement.CommandGroup, &movementv1.GroupMoveCommand{UnitIds: us, Destination: &rtsv1.Position{X: 30, Y: 25}})
	f := h.Step()
	r, _ := pt.Result(f, id)
	if !r.OK || !r.Partial || len(r.Units) != 11 {
		t.Fatalf("result = %+v", r)
	}
	dests := map[sim.Pos]bool{}
	for _, raw := range us[:9] {
		d, ok := movement.Destination(h.W(), sim.EntityID(raw))
		if !ok || dests[d] || d == (sim.Pos{X: 30, Y: 25}) {
			t.Fatalf("unit %d got destination %v (ok %v)", raw, d, ok)
		}
		dests[d] = true
		if movementCheb(d, sim.Pos{X: 30, Y: 25}) > 2 {
			t.Fatalf("destination %v far from the target", d)
		}
	}
	h.Steps(60)
	occupied := map[sim.Pos]bool{}
	for _, raw := range us[:9] {
		p := posOf(h, sim.EntityID(raw))
		if occupied[p] {
			t.Fatal("two units on one tile")
		}
		occupied[p] = true
	}
	// Budget: a group of 9 with a budget of 5 moves 5, reports 4 exhausted.
	hb := pt.NewWithBudget(t, 5, map[string]string{"energy": `{"unit_capacity": 1000, "initial_energy": 0}`})
	var bs []uint64
	for i := range 9 {
		bs = append(bs, uint64(hb.Unit(1, int32(i), 3, 0)))
	}
	id = hb.Submit(1, movement.CommandGroup, &movementv1.GroupMoveCommand{UnitIds: bs, Destination: &rtsv1.Position{X: 20, Y: 20}})
	r, _ = pt.Result(hb.Step(), id)
	exhausted := 0
	for _, u := range r.Units {
		if u.Code == sim.CodeResourceExhausted {
			exhausted++
		}
	}
	if !r.Partial || exhausted != 4 {
		t.Fatalf("budgeted group: %+v", r)
	}
}
