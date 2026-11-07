package match_test

import (
	"encoding/json"
	"testing"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func harness(t *testing.T, matchCfg string) *pt.Harness {
	return pt.New(t, map[string]string{
		"energy": `{"unit_capacity": 100, "initial_energy": 50}`,
		"match":  matchCfg,
	})
}

func state(h *pt.Harness) match.State { return match.Get(h.W()).State }

func ok(t *testing.T, f *sim.Frame, id string) {
	t.Helper()
	if r, found := pt.Result(f, id); !found || !r.OK {
		t.Fatalf("command %s: %+v", id, r)
	}
}

func failed(t *testing.T, f *sim.Frame, id string) sim.CommandResult {
	t.Helper()
	r, found := pt.Result(f, id)
	if !found || r.OK {
		t.Fatalf("command %s should fail: %+v", id, r)
	}
	return r
}

func move(h *pt.Harness, slot int, u sim.EntityID, x, y int32) string {
	return h.Submit(slot, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{
		{UnitId: uint64(u), Destination: &rtsv1.Position{X: x, Y: y}}}})
}

func kill(h *pt.Harness, u sim.EntityID) string {
	return h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(u), Health: 0})
}

func TestDefaultsRunImmediatelyWithoutVictory(t *testing.T) {
	h := harness(t, "")
	if state(h) != match.Running {
		t.Fatalf("state = %s", state(h))
	}
	u := h.Unit(1, 1, 1, 50)
	kill(h, u)
	h.Steps(5)
	if state(h) != match.Running || match.Get(h.W()).Eliminated[0] {
		t.Fatal("default config must not track elimination or end the match")
	}
}

func TestLifecycleTransitionsAndGating(t *testing.T) {
	h := harness(t, `{"start": "manual"}`)
	u := h.Unit(1, 1, 1, 50)
	if state(h) != match.Ready {
		t.Fatalf("state = %s", state(h))
	}
	// Gameplay commands and invalid transitions are rejected while ready.
	m := move(h, 1, u, 5, 1)
	p := h.Submit(0, match.CommandPause, &matchv1.PauseMatchCommand{})
	f := h.Step()
	if r := failed(t, f, m); r.Code != sim.CodeFailedPrecondition {
		t.Fatalf("move while ready: %+v", r)
	}
	failed(t, f, p)
	h.Steps(3)
	if pos(h, u) != (sim.Pos{X: 1, Y: 1}) {
		t.Fatal("unit moved while ready")
	}
	// Players cannot run admin lifecycle commands.
	if _, err := h.Engine.Submit(sim.Command{ID: "x", Issuer: sim.Principal{Player: h.P(1)}, Type: match.CommandStart, Payload: &matchv1.StartMatchCommand{}}); err == nil {
		t.Fatal("player submitted an admin command")
	}
	// Start → running: the move works.
	s := h.Submit(0, match.CommandStart, &matchv1.StartMatchCommand{})
	ok(t, h.Step(), s)
	m = move(h, 1, u, 5, 1)
	ok(t, h.Step(), m)
	h.Steps(10)
	if pos(h, u) != (sim.Pos{X: 5, Y: 1}) {
		t.Fatalf("unit at %v", pos(h, u))
	}
	// Pause: systems stop, timer stops, commands rejected; resume continues.
	ps := h.Submit(0, match.CommandPause, &matchv1.PauseMatchCommand{})
	ok(t, h.Step(), ps)
	run := match.Get(h.W()).RunTicks
	m = move(h, 1, u, 1, 1)
	f = h.Step()
	failed(t, f, m)
	h.Steps(5)
	if match.Get(h.W()).RunTicks != run || state(h) != match.Paused {
		t.Fatal("paused match advanced its timer")
	}
	rs := h.Submit(0, match.CommandResume, &matchv1.ResumeMatchCommand{})
	ok(t, h.Step(), rs)
	// End → finished; nothing can restart it.
	e := h.Submit(0, match.CommandEnd, &matchv1.EndMatchCommand{WinnerId: uint32(h.P(2)), Reason: "test"})
	ok(t, h.Step(), e)
	st := match.Get(h.W())
	if st.State != match.Finished || st.Winner != h.P(2) || st.Reason != "test" {
		t.Fatalf("finished status = %+v", st)
	}
	s2 := h.Submit(0, match.CommandStart, &matchv1.StartMatchCommand{})
	r2 := h.Submit(0, match.CommandResume, &matchv1.ResumeMatchCommand{})
	e2 := h.Submit(0, match.CommandEnd, &matchv1.EndMatchCommand{})
	f = h.Step()
	failed(t, f, s2)
	failed(t, f, r2)
	failed(t, f, e2)
	m = move(h, 1, u, 3, 3)
	failed(t, h.Step(), m)
}

func pos(h *pt.Harness, u sim.EntityID) sim.Pos {
	e, _ := h.W().Entity(u)
	return e.Pos
}

func TestReadinessStartsMatch(t *testing.T) {
	h := harness(t, `{"start": "all_ready"}`)
	r1 := h.Submit(1, match.CommandReady, &matchv1.SetReadyCommand{Ready: true})
	f := h.Step()
	ok(t, f, r1)
	if state(h) != match.Ready {
		t.Fatal("started with one of two players ready")
	}
	ev := pt.Events([]*sim.Frame{f}, match.EventReady)
	if len(ev) != 1 || pt.Field(ev[0], "ready_count").Int != 1 || pt.Field(ev[0], "expected").Int != 2 {
		t.Fatalf("ready event = %v", ev)
	}
	// Un-ready and ready again, then the second player.
	h.Submit(1, match.CommandReady, &matchv1.SetReadyCommand{Ready: false})
	h.Submit(1, match.CommandReady, &matchv1.SetReadyCommand{Ready: true})
	r2 := h.Submit(2, match.CommandReady, &matchv1.SetReadyCommand{Ready: true})
	f = h.Step()
	ok(t, f, r2)
	if state(h) != match.Running || match.Get(h.W()).StartTick != f.Tick {
		t.Fatalf("state %s start %d", state(h), match.Get(h.W()).StartTick)
	}
	// Readiness cannot change once running.
	r3 := h.Submit(1, match.CommandReady, &matchv1.SetReadyCommand{})
	failed(t, h.Step(), r3)
}

func TestEliminationVictoryAndDraw(t *testing.T) {
	h := harness(t, `{"victory": {"elimination": true}}`)
	h.Unit(1, 1, 1, 0)
	b := h.Unit(2, 5, 5, 0)
	h.Unit(2, 6, 6, 0)
	kill(h, b)
	h.Steps(2)
	if state(h) != match.Running {
		t.Fatal("ended while player 2 still had a unit")
	}
	h.Unit(1, 9, 9, 0) // player 1 has two units
	for _, id := range h.W().EntityIDs() {
		if e, _ := h.W().Entity(id); e.Kind == sim.KindUnit && e.Owner == h.P(2) {
			kill(h, id)
		}
	}
	fs := h.Steps(2)
	st := match.Get(h.W())
	if st.State != match.Finished || st.Winner != h.P(1) || st.Reason != match.ReasonLastOne || !st.Eliminated[1] {
		t.Fatalf("status = %+v", st)
	}
	if len(pt.Events(fs, match.EventElim)) != 1 || len(pt.Events(fs, match.EventFinished)) != 1 {
		t.Fatal("missing elimination or finish events")
	}
	if st.Stats[1].UnitsLost != 2 {
		t.Fatalf("units lost = %d", st.Stats[1].UnitsLost)
	}

	// Simultaneous elimination is a draw.
	h2 := harness(t, `{"victory": {"elimination": true}}`)
	x, y := h2.Unit(1, 1, 1, 0), h2.Unit(2, 5, 5, 0)
	kill(h2, x)
	kill(h2, y)
	h2.Steps(2)
	st = match.Get(h2.W())
	if st.State != match.Finished || !st.Draw || st.Winner != 0 || st.Reason != match.ReasonAllGone {
		t.Fatalf("draw status = %+v", st)
	}
}

func TestTimeLimitScoringAndPrecedence(t *testing.T) {
	h := harness(t, `{"victory": {"time_limit_ticks": 5}, "scoring": {"weights": {"units": 10, "stored_energy": 1}}}`)
	h.Unit(1, 1, 1, 30)
	h.Unit(1, 2, 1, 0)
	h.Unit(2, 5, 5, 40)
	h.Steps(4)
	if state(h) != match.Running {
		t.Fatal("ended early")
	}
	h.Step()
	st := match.Get(h.W())
	// p1: 2 units*10 + 30 = 50; p2: 10 + 40 = 50 → draw.
	if st.State != match.Finished || !st.Draw || st.Reason != match.ReasonTime || st.Scores[0] != 50 || st.Scores[1] != 50 {
		t.Fatalf("tie status = %+v", st)
	}

	// Elimination takes precedence over a time limit reached the same tick.
	h2 := harness(t, `{"victory": {"elimination": true, "time_limit_ticks": 3}}`)
	h2.Unit(1, 1, 1, 0)
	v := h2.Unit(2, 5, 5, 100)
	h2.Steps(2)
	kill(h2, v)
	h2.Step()
	st = match.Get(h2.W())
	if st.Reason != match.ReasonLastOne || st.Winner != h2.P(1) {
		t.Fatalf("precedence: %+v", st)
	}
}

func TestAdminElimination(t *testing.T) {
	h := harness(t, `{"victory": {"elimination": true}}`)
	h.Unit(1, 1, 1, 0)
	u := h.Unit(2, 5, 5, 0)
	e := h.Submit(0, match.CommandEliminate, &matchv1.EliminatePlayerCommand{PlayerId: uint32(h.P(2)), RemoveUnits: true})
	ok(t, h.Step(), e)
	if _, alive := h.W().Entity(u); alive {
		t.Fatal("units not removed")
	}
	st := match.Get(h.W())
	if st.State != match.Finished || st.Winner != h.P(1) {
		t.Fatalf("status = %+v", st)
	}
}

func TestEliminatedPlayersCommandsRejected(t *testing.T) {
	h := harness(t, `{"victory": {"elimination": true}}`)
	h.Unit(1, 1, 1, 0)
	h.Unit(1, 3, 3, 0)
	u := h.Unit(2, 7, 7, 0)
	e := h.Submit(0, match.CommandEliminate, &matchv1.EliminatePlayerCommand{PlayerId: uint32(h.P(2))})
	ok(t, h.Step(), e)
	m := move(h, 2, u, 8, 8)
	if r := failed(t, h.Step(), m); r.Code != sim.CodeFailedPrecondition {
		t.Fatalf("eliminated player's command: %+v", r)
	}
}

// Custom conditions and metrics register on the plugin before the ruleset
// is built — exactly how game.Plugins() would wire them.
type firstToUnits struct{ n int64 }

func (firstToUnits) Name() string { return "first_to_units" }
func (c firstToUnits) Check(v *match.View) *match.Outcome {
	for i := range int(v.Status.N) {
		if p := v.Status.Players[i]; v.Aggregate(p).Units >= c.n {
			return &match.Outcome{Winner: p, Reason: "first_to_units"}
		}
	}
	return nil
}

func TestCustomConditionAndMetric(t *testing.T) {
	plugins := game.Plugins()
	var m *match.Plugin
	for _, p := range plugins {
		if mp, ok := p.(*match.Plugin); ok {
			m = mp
		}
	}
	m.AddCondition(firstToUnits{n: 3})
	m.AddMetric(match.NewMetric("tiles_east", false, func(v *match.View, p sim.PlayerID) int64 {
		var s int64
		for _, id := range v.World.EntityIDs() {
			if e, _ := v.World.Entity(id); e.Owner == p {
				s += int64(e.Pos.X)
			}
		}
		return s
	}))
	m.SetPlayers([]sim.PlayerID{11, 22})
	rules, err := sim.NewRuleset(plugins, map[string]json.RawMessage{
		"match": json.RawMessage(`{"scoring": {"weights": {"tiles_east": 1}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := sim.NewEngine(sim.Config{Width: 20, Height: 20, IDKey: []byte("0123456789abcdef")}, rules)
	if err := e.Init(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		_, _ = e.World().CreateEntity(sim.KindUnit, 22, sim.Pos{X: int32(10 + i), Y: 1})
	}
	e.Step()
	st := match.Get(e.World())
	if st.State != match.Finished || st.Winner != 22 || st.Reason != "first_to_units" || st.Scores[1] != 33 {
		t.Fatalf("custom outcome = %+v", st)
	}
	// Unknown metric names are rejected at startup.
	if _, err := sim.NewRuleset(game.Plugins(), map[string]json.RawMessage{
		"match": json.RawMessage(`{"scoring": {"weights": {"nope": 1}}}`),
	}); err == nil {
		t.Fatal("unknown metric accepted")
	}
}

func TestKillStatistics(t *testing.T) {
	h := pt.New(t, map[string]string{
		"energy": `{"unit_capacity": 100, "initial_energy": 50}`,
		"combat": `{"attack": {"default": 50}, "max_health": {"default": 50}, "cooldown": {"default": 1}}`,
	})
	a := h.Unit(1, 1, 1, 50)
	b := h.Unit(2, 2, 1, 50)
	h.Submit(1, combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(a)}, TargetId: uint64(b)})
	h.Steps(3)
	st := match.Get(h.W())
	if st.Stats[0].Kills != 1 || st.Stats[1].UnitsLost != 1 {
		t.Fatalf("stats = %+v", st.Stats[:2])
	}
}
