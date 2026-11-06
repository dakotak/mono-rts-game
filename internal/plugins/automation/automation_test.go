package automation_test

import (
	"fmt"
	"testing"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/automation"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const combatCfg = `{"attack": {"default": 10}, "max_health": {"default": 1000}, "range": {"default": 2}, "cooldown": {"default": 1}}`

func harness(t testing.TB, extra map[string]string) *pt.Harness {
	cfg := map[string]string{
		"energy":     `{"unit_capacity": 100, "initial_energy": 0}`,
		"combat":     combatCfg,
		"automation": `{"eval_interval": 2}`,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return pt.New(t, cfg)
}

func well(h *pt.Harness, x, y int32, energyAmount int64) sim.EntityID {
	h.T.Helper()
	id, err := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: x, Y: y})
	if err != nil {
		h.T.Fatal(err)
	}
	wells.Set(h.W(), id, wells.Well{Energy: energyAmount, Capacity: energyAmount, ExtractionRate: 10})
	return id
}

func ids(us ...sim.EntityID) []uint64 {
	out := make([]uint64, len(us))
	for i, u := range us {
		out[i] = uint64(u)
	}
	return out
}

func mineDeliver(h *pt.Harness, slot int, w, recv sim.EntityID, us ...sim.EntityID) string {
	return h.Submit(slot, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(us...),
		Behavior: &automationv1.AutomateCommand_MineAndDeliver{MineAndDeliver: &automationv1.MineAndDeliver{WellId: uint64(w), ReceiverId: uint64(recv)}}})
}

func posOf(h *pt.Harness, u sim.EntityID) sim.Pos {
	e, _ := h.W().Entity(u)
	return e.Pos
}

// attr returns an owner-visible automation attribute.
func attr(h *pt.Harness, u sim.EntityID, key string) string {
	v, ok := h.W().View(u)
	if !ok {
		return ""
	}
	for _, a := range v.Attrs {
		if a.Key == key {
			return fmt.Sprint(valStr(a.Value))
		}
	}
	return ""
}

func mustOK(t *testing.T, f *sim.Frame, id string) {
	t.Helper()
	r, ok := pt.Result(f, id)
	if !ok || !r.OK {
		t.Fatalf("command %s: %+v", id, r)
	}
}

func mustFail(t *testing.T, f *sim.Frame, id string) sim.CommandResult {
	t.Helper()
	r, ok := pt.Result(f, id)
	if !ok || r.OK {
		t.Fatalf("command %s should fail: %+v", id, r)
	}
	return r
}

func states(fs []*sim.Frame, u sim.EntityID) []string {
	var out []string
	for _, e := range pt.Events(fs, automation.EventState) {
		if e.Entity == u {
			out = append(out, pt.Field(e, "state").Str)
		}
	}
	return out
}

func TestMineAndDeliverCycleConservesEnergy(t *testing.T) {
	h := harness(t, nil)
	wl := well(h, 12, 5, 1000)
	recv := h.Unit(1, 2, 5, 0)
	energy.SetStored(h.W(), recv, 0)
	miner := h.Unit(1, 6, 5, 0)
	id := mineDeliver(h, 1, wl, recv, miner)
	fs := h.Steps(1)
	mustOK(t, fs[0], id)
	fs = append(fs, h.Steps(250)...)

	got := states(fs, miner)
	want := []string{"to_well", "mining", "to_receiver", "delivering", "to_well", "mining"}
	if len(got) < len(want) || fmt.Sprint(got[:len(want)]) != fmt.Sprint(want) {
		t.Fatalf("state sequence = %v, want prefix %v", got, want)
	}
	wv, _ := wells.Get(h.W(), wl)
	mined := 1000 - wv.Energy
	if h.Energy(recv) < 100 {
		t.Fatalf("receiver got %d, want at least one full load", h.Energy(recv))
	}
	if mined != h.Energy(recv)+h.Energy(miner) {
		t.Fatalf("energy not conserved: mined %d, receiver %d, miner %d", mined, h.Energy(recv), h.Energy(miner))
	}
	if attr(h, miner, "automation.behavior") != "mine_and_deliver" {
		t.Fatal("automation status not exposed")
	}
}

func TestCancelStopsActivities(t *testing.T) {
	h := harness(t, nil)
	wl := well(h, 6, 5, 1000)
	recv := h.Unit(1, 2, 5, 0)
	miner := h.Unit(1, 5, 5, 0)
	mineDeliver(h, 1, wl, recv, miner)
	h.Steps(3)
	if _, ok := energy.MiningWell(h.W(), miner); !ok {
		t.Fatal("adjacent miner did not start mining")
	}
	c := h.Submit(1, automation.CommandCancel, &automationv1.CancelAutomationCommand{UnitIds: ids(miner)})
	mustOK(t, h.Step(), c)
	if _, ok := energy.MiningWell(h.W(), miner); ok || len(automation.Tasks(h.W())) != 0 {
		t.Fatal("cancel left mining or the task in place")
	}
	before := h.Energy(miner)
	h.Steps(10)
	if h.Energy(miner) != before {
		t.Fatal("unit kept mining after cancel")
	}
}

func TestFailureWhenTargetsDisappear(t *testing.T) {
	for _, gone := range []string{"well", "receiver"} {
		t.Run(gone, func(t *testing.T) {
			h := harness(t, nil)
			wl := well(h, 12, 5, 1000)
			recv := h.Unit(1, 2, 5, 0)
			miner := h.Unit(1, 6, 5, 0)
			mineDeliver(h, 1, wl, recv, miner)
			h.Steps(3)
			target := wl
			if gone == "receiver" {
				target = recv
			}
			h.W().RemoveEntity(target)
			fs := h.Steps(4)
			ev := pt.Events(fs, automation.EventFailed)
			if len(ev) != 1 || pt.Field(ev[0], "reason").Str != gone+"_unavailable" {
				t.Fatalf("failed events = %v", ev)
			}
			if ev[0].Audience != sim.ToPlayer(h.P(1)) {
				t.Fatalf("failure audience = %v", ev[0].Audience)
			}
			if len(automation.Tasks(h.W())) != 0 || hasDest(h, miner) {
				t.Fatal("failed behavior left state behind")
			}
		})
	}
}

func TestDepletedWellFails(t *testing.T) {
	h := harness(t, nil)
	wl := well(h, 6, 5, 30)
	recv := h.Unit(1, 2, 5, 0)
	miner := h.Unit(1, 5, 5, 0)
	mineDeliver(h, 1, wl, recv, miner)
	ev := pt.Events(h.Steps(30), automation.EventFailed)
	if len(ev) != 1 || pt.Field(ev[0], "reason").Str != "well_depleted" {
		t.Fatalf("failed events = %v", ev)
	}
}

func TestAuthorizationAndValidation(t *testing.T) {
	h := harness(t, nil)
	wl := well(h, 12, 5, 1000)
	mine := h.Unit(1, 2, 5, 0)
	theirs := h.Unit(2, 30, 30, 0)
	c1 := mineDeliver(h, 1, wl, mine, theirs)                                                       // foreign unit
	c2 := mineDeliver(h, 1, wl, theirs, mine)                                                       // foreign receiver
	c3 := mineDeliver(h, 1, wl, mine, mine)                                                         // self as receiver
	c4 := mineDeliver(h, 1, sim.EntityID(987654), theirs, mine)                                     // bogus well
	c5 := h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(mine), // radius too large
		Behavior: &automationv1.AutomateCommand_AutoSalvage{AutoSalvage: &automationv1.AutoSalvage{Radius: 99}}})
	c6 := h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(mine), // follow an enemy
		Behavior: &automationv1.AutomateCommand_Follow{Follow: &automationv1.Follow{LeaderId: uint64(theirs), Distance: 2}}})
	f := h.Step()
	for _, c := range []string{c1, c2, c3, c4, c5, c6} {
		mustFail(t, f, c)
	}
	if len(automation.Tasks(h.W())) != 0 {
		t.Fatal("rejected commands created tasks")
	}
}

// With fog of war, a well the owner does not detect cannot be named, and the
// rejection is indistinguishable from a made-up ID.
func TestHiddenWellIsUnavailable(t *testing.T) {
	h := harness(t, map[string]string{
		"visibility": `{"enabled": true, "vision": {"default": 3}, "sensor": {"default": 10}, "identification": {"default": 1}}`,
	})
	hidden := well(h, 30, 30, 1000)
	recv := h.Unit(1, 2, 5, 0)
	miner := h.Unit(1, 3, 5, 0)
	h.Step()
	c1 := mineDeliver(h, 1, hidden, recv, miner)
	c2 := mineDeliver(h, 1, sim.EntityID(987654), recv, miner)
	f := h.Step()
	r1, r2 := mustFail(t, f, c1), mustFail(t, f, c2)
	if r1.Code != r2.Code {
		t.Fatalf("hidden well distinguishable: %v vs %v", r1, r2)
	}
}

func killed(h *pt.Harness, u sim.EntityID) sim.EntityID {
	h.T.Helper()
	pos := posOf(h, u)
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(u), Health: 0})
	h.Step()
	for _, id := range salvage.Wrecks(h.W()) {
		if e, _ := h.W().Entity(id); e.Pos == pos {
			return id
		}
	}
	h.T.Fatal("no wreck")
	return 0
}

func TestAutoSalvagePicksNearestThenStops(t *testing.T) {
	h := harness(t, nil)
	far := killed(h, h.Unit(2, 10, 10, 50))
	near := killed(h, h.Unit(2, 7, 10, 50))
	outside := killed(h, h.Unit(2, 30, 10, 50))
	u := h.Unit(1, 4, 10, 0)
	c := h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(u),
		Behavior: &automationv1.AutomateCommand_AutoSalvage{AutoSalvage: &automationv1.AutoSalvage{Radius: 8}}})
	f := h.Step()
	mustOK(t, f, c)
	if attr(h, u, "automation.target") != fmt.Sprint(uint64(near)) {
		t.Fatalf("first target = %s, want nearest %d", attr(h, u, "automation.target"), near)
	}
	fs := h.Steps(200)
	if _, ok := h.W().Entity(near); ok {
		t.Fatal("nearest wreck not collected")
	}
	if _, ok := h.W().Entity(far); ok {
		t.Fatal("second wreck within radius not collected")
	}
	if _, ok := h.W().Entity(outside); !ok {
		t.Fatal("collected a wreck outside the radius")
	}
	ev := pt.Events(fs, automation.EventBlocked)
	if len(ev) == 0 || pt.Field(ev[len(ev)-1], "reason").Str != "no_wreckage" {
		t.Fatalf("blocked events = %v", ev)
	}
	if attr(h, u, "automation.status") != "blocked" {
		t.Fatal("status not blocked")
	}
}

func guard(h *pt.Harness, u sim.EntityID, x, y, r int32) string {
	return h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(u),
		Behavior: &automationv1.AutomateCommand_Guard{Guard: &automationv1.Guard{Position: &rtsv1.Position{X: x, Y: y}, Radius: r}}})
}

func TestGuardTargetSelectionAndReturn(t *testing.T) {
	h := harness(t, nil)
	g := h.Unit(1, 10, 10, 0)
	c := guard(h, g, 10, 10, 5)
	mustOK(t, h.Step(), c)
	h.Steps(4)
	if attr(h, g, "automation.state") != "holding" {
		t.Fatalf("state = %s", attr(h, g, "automation.state"))
	}
	// Two hostiles at equal distance: the lower ID wins; one outside the
	// area is ignored.
	a := h.Unit(2, 13, 10, 0)
	b := h.Unit(2, 7, 10, 0)
	h.Unit(2, 20, 10, 0)
	h.Steps(3)
	want := min(a, b)
	if tg, _ := combat.Target(h.W(), g); tg != want {
		t.Fatalf("target = %d, want %d", tg, want)
	}
	// The guard closes in but never leaves the area.
	for range 20 {
		h.Step()
		if p := posOf(h, g); max(p.X-10, 10-p.X, p.Y-10, 10-p.Y) > 5 {
			t.Fatalf("guard left its area: %v", p)
		}
	}
	// Threats gone: it returns to its post.
	h.W().RemoveEntity(a)
	h.W().RemoveEntity(b)
	h.Steps(40)
	if posOf(h, g) != (sim.Pos{X: 10, Y: 10}) || attr(h, g, "automation.state") != "holding" {
		t.Fatalf("guard at %v state %s", posOf(h, g), attr(h, g, "automation.state"))
	}
	if _, ok := combat.Target(h.W(), g); ok {
		t.Fatal("guard kept a target")
	}
}

func TestFollowKeepsDistanceAndFailsWhenLeaderGone(t *testing.T) {
	h := harness(t, nil)
	leader := h.Unit(1, 5, 5, 0)
	f := h.Unit(1, 5, 7, 0)
	c := h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(f),
		Behavior: &automationv1.AutomateCommand_Follow{Follow: &automationv1.Follow{LeaderId: uint64(leader), Distance: 2}}})
	mustOK(t, h.Step(), c)
	h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(leader), Destination: &rtsv1.Position{X: 25, Y: 20}}}})
	h.Steps(120)
	lp, fp := posOf(h, leader), posOf(h, f)
	if lp != (sim.Pos{X: 25, Y: 20}) || max(lp.X-fp.X, fp.X-lp.X, lp.Y-fp.Y, fp.Y-lp.Y) > 2 {
		t.Fatalf("leader %v follower %v", lp, fp)
	}
	h.W().RemoveEntity(leader)
	ev := pt.Events(h.Steps(3), automation.EventFailed)
	if len(ev) != 1 || pt.Field(ev[0], "reason").Str != "leader_unavailable" {
		t.Fatalf("failed = %v", ev)
	}
}

func TestEvaluationBudget(t *testing.T) {
	h := harness(t, map[string]string{"automation": `{"eval_interval": 1, "max_evaluations_per_tick": 2}`})
	var us []sim.EntityID
	for i := range 5 {
		us = append(us, h.Unit(1, int32(i*2), 0, 0))
	}
	h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(us...),
		Behavior: &automationv1.AutomateCommand_AutoSalvage{AutoSalvage: &automationv1.AutoSalvage{Radius: 3}}})
	fs := h.Steps(4)
	for i, e := range pt.Events(fs, automation.EventWork) {
		if n := pt.Field(e, "evaluated").Int; n > 2 {
			t.Fatalf("work event %d evaluated %d > budget", i, n)
		}
		if e.Audience != sim.AdminAudience() {
			t.Fatal("work event not admin-only")
		}
	}
	if d := pt.Field(pt.Events(fs[:1], automation.EventWork)[0], "deferred").Int; d != 3 {
		t.Fatalf("deferred = %d, want 3", d)
	}
}

// scenario runs a mixed automation scenario and returns per-tick hashes and
// the accepted command log.
func scenario(t *testing.T, log [][]sim.Command) ([]uint64, [][]sim.Command) {
	h := harness(t, nil)
	wl := well(h, 20, 20, 5000)
	recv := h.Unit(1, 14, 20, 0)
	var miners []sim.EntityID
	for i := range 6 {
		miners = append(miners, h.Unit(1, 16, int32(14+i*2), 0))
	}
	g := h.Unit(1, 24, 24, 0)
	enemy := h.Unit(2, 30, 30, 0)
	follower := h.Unit(2, 31, 31, 0)
	var hashes []uint64
	var out [][]sim.Command
	for tick := range 150 {
		if log != nil {
			h.Engine.StepWith(log[tick])
		} else {
			switch tick {
			case 0:
				mineDeliver(h, 1, wl, recv, miners...)
				guard(h, g, 24, 24, 6)
				h.Submit(2, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(follower),
					Behavior: &automationv1.AutomateCommand_Follow{Follow: &automationv1.Follow{LeaderId: uint64(enemy), Distance: 1}}})
			case 10:
				h.Submit(2, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(enemy), Destination: &rtsv1.Position{X: 26, Y: 26}}}})
			case 80:
				h.Submit(1, automation.CommandCancel, &automationv1.CancelAutomationCommand{UnitIds: ids(miners[0])})
			}
			out = append(out, h.Step().Commands)
		}
		hashes = append(hashes, h.W().StateHash())
	}
	return hashes, out
}

func TestAutomationReplayEquivalence(t *testing.T) {
	a, log := scenario(t, nil)
	b, _ := scenario(t, nil)
	c, _ := scenario(t, log)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("tick %d differs between runs", i+1)
		}
		if a[i] != c[i] {
			t.Fatalf("tick %d differs on replay", i+1)
		}
	}
}

func hasDest(h *pt.Harness, u sim.EntityID) bool {
	_, ok := movement.Destination(h.W(), u)
	return ok
}

func valStr(v sim.Value) any {
	switch v.Kind {
	case sim.ValueString:
		return v.Str
	case sim.ValueInt:
		return v.Int
	}
	return v
}

// Assigning automation is charged per unit against the command budget,
// atomically.
func TestAutomationBudget(t *testing.T) {
	h := pt.NewWithBudget(t, 2, map[string]string{"energy": `{"unit_capacity": 100, "initial_energy": 0}`})
	var us []sim.EntityID
	for i := range 3 {
		us = append(us, h.Unit(1, int32(i*2), 0, 0))
	}
	salvage := func(units ...sim.EntityID) string {
		return h.Submit(1, automation.CommandAutomate, &automationv1.AutomateCommand{UnitIds: ids(units...),
			Behavior: &automationv1.AutomateCommand_AutoSalvage{AutoSalvage: &automationv1.AutoSalvage{Radius: 3}}})
	}
	over := salvage(us...)
	f := h.Step()
	if r := mustFail(t, f, over); r.Code != "resource_exhausted" {
		t.Fatalf("over budget: %+v", r)
	}
	if len(automation.Tasks(h.W())) != 0 {
		t.Fatal("partial assignment over budget")
	}
	within := salvage(us[:2]...)
	mustOK(t, h.Step(), within)
}
