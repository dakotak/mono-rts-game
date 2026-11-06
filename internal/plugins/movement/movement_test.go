package movement_test

import (
	"encoding/json"
	"fmt"
	"testing"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

type harness struct {
	t *testing.T
	e *sim.Engine
	n int
}

func newHarness(t *testing.T, w, h int32, cfg string) *harness {
	t.Helper()
	var conf map[string]json.RawMessage
	if cfg != "" {
		conf = map[string]json.RawMessage{movement.Name: json.RawMessage(cfg)}
	}
	rs, err := sim.NewRuleset([]sim.Plugin{movement.New()}, conf)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sim.NewEngine(sim.Config{Width: w, Height: h, Seed: 7}, rs)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, e: e}
}

func (h *harness) unit(owner sim.PlayerID, x, y int32) sim.EntityID {
	h.t.Helper()
	id, err := h.e.World().CreateEntity(sim.KindUnit, owner, sim.Pos{X: x, Y: y})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) move(player sim.PlayerID, dests map[sim.EntityID]sim.Pos) string {
	h.t.Helper()
	m := &movementv1.MoveCommand{}
	for id, d := range dests {
		m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(id), Destination: &rtsv1.Position{X: d.X, Y: d.Y}})
	}
	return h.submit(player, movement.CommandMove, m)
}

func (h *harness) submit(player sim.PlayerID, typ string, payload any) string {
	h.t.Helper()
	h.n++
	id := fmt.Sprintf("c%d", h.n)
	if _, err := h.e.Submit(sim.Command{ID: id, Issuer: sim.Principal{Player: player}, Type: typ, Payload: payload}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) pos(id sim.EntityID) sim.Pos {
	e, ok := h.e.World().Entity(id)
	if !ok {
		h.t.Fatalf("entity %d missing", id)
	}
	return e.Pos
}

func (h *harness) steps(n int) []*sim.Frame {
	var out []*sim.Frame
	for range n {
		out = append(out, h.e.Step())
	}
	return out
}

func p(x, y int32) sim.Pos { return sim.Pos{X: x, Y: y} }

func TestMoveToEmptyTileAndArrive(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	u := h.unit(1, 0, 0)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(3, 1)})
	f := h.steps(1)[0]
	if !f.Results[0].OK {
		t.Fatalf("result = %+v", f.Results[0])
	}
	if got := h.pos(u); got != p(1, 1) {
		t.Fatalf("after tick 1: %v (diagonal step expected)", got)
	}
	h.steps(1)
	if got := h.pos(u); got != p(2, 1) {
		t.Fatalf("after tick 2: %v", got)
	}
	f = h.steps(1)[0]
	if got := h.pos(u); got != p(3, 1) {
		t.Fatalf("after tick 3: %v", got)
	}
	if len(f.Events) != 1 || f.Events[0].Type != movement.EventArrived || f.Events[0].Audience != sim.ToPlayer(1) {
		t.Fatalf("events = %+v", f.Events)
	}
	// Order removed: the unit's view no longer carries a destination.
	if v := f.Changed[0]; len(v.Attrs) != 0 {
		t.Errorf("attrs after arrival = %+v", v.Attrs)
	}
	if f := h.steps(1)[0]; len(f.Changed) != 0 {
		t.Errorf("idle unit changed: %+v", f.Changed)
	}
}

func TestDestinationIsOwnerVisibleAttribute(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	u := h.unit(1, 0, 0)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(5, 0)})
	f := h.steps(1)[0]
	a := f.Changed[0].Attrs
	if len(a) != 1 || a[0].Key != movement.AttrDestination || a[0].Value.Pos != p(5, 0) || a[0].Visibility != sim.VisibleOwner {
		t.Fatalf("attrs = %+v", a)
	}
}

func TestTicksPerMove(t *testing.T) {
	h := newHarness(t, 10, 10, `{"ticks_per_move": 3}`)
	u := h.unit(1, 0, 0)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(9, 0)})
	var xs []int32
	for range 7 {
		h.steps(1)
		xs = append(xs, h.pos(u).X)
	}
	if fmt.Sprint(xs) != "[1 1 1 2 2 2 3]" {
		t.Errorf("x per tick = %v", xs)
	}
}

func TestInvalidCommands(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	mine := h.unit(1, 0, 0)
	theirs := h.unit(2, 5, 5)
	h.steps(1) // flush setup changes
	cases := []struct {
		name string
		typ  string
		cmd  any
		code string
	}{
		{"empty", movement.CommandMove, &movementv1.MoveCommand{}, sim.CodeInvalidArgument},
		{"unknown unit", movement.CommandMove, mv(999, 1, 1), sim.CodeNotFound},
		{"not owner", movement.CommandMove, mv(uint64(theirs), 1, 1), sim.CodePermissionDenied},
		{"out of bounds x", movement.CommandMove, mv(uint64(mine), 10, 0), sim.CodeInvalidArgument},
		{"out of bounds y", movement.CommandMove, mv(uint64(mine), 0, -1), sim.CodeInvalidArgument},
		{"missing destination", movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(mine)}}}, sim.CodeInvalidArgument},
		{"duplicate unit", movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{
			{UnitId: uint64(mine), Destination: &rtsv1.Position{X: 1}},
			{UnitId: uint64(mine), Destination: &rtsv1.Position{X: 2}},
		}}, sim.CodeInvalidArgument},
		{"wrong payload type", movement.CommandMove, &movementv1.StopCommand{}, sim.CodeInvalidArgument},
		{"stop not owner", movement.CommandStop, &movementv1.StopCommand{UnitIds: []uint64{uint64(theirs)}}, sim.CodePermissionDenied},
		{"stop empty", movement.CommandStop, &movementv1.StopCommand{}, sim.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.submit(1, c.typ, c.cmd)
			f := h.steps(1)[0]
			if r := f.Results[0]; r.OK || r.Code != c.code {
				t.Fatalf("result = %+v, want code %s", r, c.code)
			}
			if len(f.Changed) != 0 {
				t.Fatalf("failed command changed state: %+v", f.Changed)
			}
		})
	}
}

func TestMixedValidityCommandIsAtomic(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	a := h.unit(1, 0, 0)
	b := h.unit(2, 3, 3)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(5, 0), b: p(5, 5)})
	f := h.steps(1)[0]
	if f.Results[0].OK || h.pos(a) != p(0, 0) {
		t.Fatalf("partial command applied: %+v, a at %v", f.Results[0], h.pos(a))
	}
}

func TestBoundaries(t *testing.T) {
	h := newHarness(t, 4, 3, "")
	u := h.unit(1, 0, 0)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(3, 2)}) // far corner
	h.steps(5)
	if got := h.pos(u); got != p(3, 2) {
		t.Fatalf("pos = %v", got)
	}
}

func TestOccupiedDestinationBlocksThenCancels(t *testing.T) {
	h := newHarness(t, 10, 10, `{"max_blocked_ticks": 3}`)
	mover := h.unit(1, 0, 0)
	h.unit(2, 1, 0) // stationary blocker
	h.move(1, map[sim.EntityID]sim.Pos{mover: p(1, 0)})
	frames := h.steps(3)
	if h.pos(mover) != p(0, 0) {
		t.Fatalf("moved into occupied tile")
	}
	ev := frames[2].Events
	if len(ev) != 1 || ev[0].Type != movement.EventCancelled || ev[0].Entity != mover {
		t.Fatalf("events = %+v", ev)
	}
	if len(frames[0].Events)+len(frames[1].Events) != 0 {
		t.Fatal("cancelled early")
	}
}

func TestContentionAllFail(t *testing.T) {
	h := newHarness(t, 10, 10, `{"contention_backoff_ticks": 0, "max_blocked_ticks": 0}`)
	a := h.unit(1, 0, 0)
	b := h.unit(2, 2, 0)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(1, 0)})
	h.move(2, map[sim.EntityID]sim.Pos{b: p(1, 0)})
	h.steps(5)
	// Without backoff the symmetric conflict never resolves: by design,
	// neither unit is privileged.
	if h.pos(a) != p(0, 0) || h.pos(b) != p(2, 0) {
		t.Fatalf("a=%v b=%v", h.pos(a), h.pos(b))
	}
}

func TestContentionBackoffBreaksTie(t *testing.T) {
	h := newHarness(t, 10, 10, `{"contention_backoff_ticks": 3}`)
	a := h.unit(1, 0, 0)
	b := h.unit(2, 2, 0)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(1, 0)})
	h.move(2, map[sim.EntityID]sim.Pos{b: p(1, 0)})
	h.steps(30)
	at, bt := h.pos(a) == p(1, 0), h.pos(b) == p(1, 0)
	if at == bt {
		t.Fatalf("exactly one unit should reach the contested tile: a=%v b=%v", h.pos(a), h.pos(b))
	}
}

func TestChainFollowsLeader(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	// Column of three moving right; the leader has free space.
	c := h.unit(1, 0, 0)
	b := h.unit(1, 1, 0)
	a := h.unit(1, 2, 0)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(9, 0), b: p(9, 0), c: p(9, 0)})
	h.steps(1)
	if h.pos(a) != p(3, 0) || h.pos(b) != p(2, 0) || h.pos(c) != p(1, 0) {
		t.Fatalf("a=%v b=%v c=%v", h.pos(a), h.pos(b), h.pos(c))
	}
}

func TestChainBehindBlockedLeaderFails(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	b := h.unit(1, 0, 0)
	a := h.unit(1, 1, 0)
	h.unit(2, 2, 0) // blocker
	h.move(1, map[sim.EntityID]sim.Pos{a: p(9, 0), b: p(9, 0)})
	h.steps(1)
	if h.pos(a) != p(1, 0) || h.pos(b) != p(0, 0) {
		t.Fatalf("a=%v b=%v", h.pos(a), h.pos(b))
	}
}

func TestSwapFails(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	a := h.unit(1, 0, 0)
	b := h.unit(1, 1, 0)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(1, 0), b: p(0, 0)})
	h.steps(2)
	if h.pos(a) != p(0, 0) || h.pos(b) != p(1, 0) {
		t.Fatalf("swap happened: a=%v b=%v", h.pos(a), h.pos(b))
	}
}

func TestRotationCycleFails(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	// Four units in a 2x2 square each stepping clockwise.
	a := h.unit(1, 0, 0)
	b := h.unit(1, 1, 0)
	c := h.unit(1, 1, 1)
	d := h.unit(1, 0, 1)
	h.move(1, map[sim.EntityID]sim.Pos{a: p(1, 0), b: p(1, 1), c: p(0, 1), d: p(0, 0)})
	h.steps(1)
	if h.pos(a) != p(0, 0) || h.pos(b) != p(1, 0) || h.pos(c) != p(1, 1) || h.pos(d) != p(0, 1) {
		t.Fatal("cycle rotated")
	}
}

func TestStopCommand(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	u := h.unit(1, 0, 0)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(9, 0)})
	h.steps(2)
	h.submit(1, movement.CommandStop, &movementv1.StopCommand{UnitIds: []uint64{uint64(u)}})
	h.steps(3)
	if h.pos(u) != p(2, 0) {
		t.Fatalf("pos = %v", h.pos(u))
	}
}

func TestNewOrderReplacesOld(t *testing.T) {
	h := newHarness(t, 10, 10, "")
	u := h.unit(1, 5, 5)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(9, 5)})
	h.steps(1)
	h.move(1, map[sim.EntityID]sim.Pos{u: p(0, 5)})
	h.steps(2)
	if h.pos(u) != p(4, 5) {
		t.Fatalf("pos = %v", h.pos(u))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []string{`{"ticks_per_move":0}`, `{"max_blocked_ticks":-1}`, `{"bogus":1}`} {
		_, err := sim.NewRuleset([]sim.Plugin{movement.New()}, map[string]json.RawMessage{movement.Name: json.RawMessage(cfg)})
		if err == nil {
			t.Errorf("config %s accepted", cfg)
		}
	}
}

func mv(id uint64, x, y int32) *movementv1.MoveCommand {
	return &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: id, Destination: &rtsv1.Position{X: x, Y: y}}}}
}
