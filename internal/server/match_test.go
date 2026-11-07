package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// matchEnv: 30x20 arena, two players with 2 generated units each, start
// when both are ready, elimination victory and a 40-tick time limit.
func matchEnv(t *testing.T) *env {
	return newEnvWith(t, func(c *game.Config) {
		c.World.Width, c.World.Height = 30, 20
		c.Units = nil
		c.Plugins = map[string]json.RawMessage{
			"worldgen": json.RawMessage(`{"generator": "arena", "spawns": {"layout": "corners"}, "start": {"units": 2, "min_free_tiles": 10}}`),
			"match":    json.RawMessage(`{"start": "all_ready", "victory": {"elimination": true, "time_limit_ticks": 40}}`),
		}
	})
}

func (e *env) step(n int) {
	for range n {
		e.srv.Engine().Step()
	}
}

func (p *player) match() *rtsv1.GetMatchResponse {
	p.e.t.Helper()
	r, err := p.e.game.GetMatch(p.ctx, &rtsv1.GetMatchRequest{})
	if err != nil {
		p.e.t.Fatal(err)
	}
	return r
}

func TestSequentialMatchesInOneProcess(t *testing.T) {
	e := matchEnv(t)
	red, blue := e.connect("red"), e.connect("blue")
	ro := red.observe()
	if ro.snap.GetMatch().GetState() != rtsv1.MatchState_MATCH_STATE_READY {
		t.Fatalf("initial match = %v", ro.snap.GetMatch())
	}
	firstID := ro.snap.GetMatch().GetMatchId()

	// ---- match 1: readiness, gating, elimination ---------------------------
	m := red.match().GetMatch()
	if m.GetExpectedPlayers() != 2 || m.GetReadyPlayers() != 0 || len(m.GetScores()) != 0 || m.GetResult() != nil ||
		m.GetVictoryConditions()[0] != "elimination" || m.GetGenerator() != "arena" || m.GetYourPlayerId() != red.id {
		t.Fatalf("lobby info = %v", m)
	}
	var unit uint64
	for _, en := range ro.snap.GetEntities() {
		if en.GetOwnerId() == red.id {
			unit = en.GetId()
		}
	}
	red.submit("early-move", moveTo(unit, 15, 10))
	red.submit("ready", &matchv1.SetReadyCommand{Ready: true})
	e.step(1)
	u := ro.next()
	if r := resultFor(u, "early-move"); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_FAILED || r.GetErrorCode() != "failed_precondition" {
		t.Fatalf("move before start = %v", r)
	}
	if u.GetMatch().GetReadyPlayers() != 1 || !u.GetMatch().GetYouReady() {
		t.Fatalf("after ready: %v", u.GetMatch())
	}
	blue.submit("ready", &matchv1.SetReadyCommand{Ready: true})
	e.step(1)
	if st := ro.next().GetMatch().GetState(); st != rtsv1.MatchState_MATCH_STATE_RUNNING {
		t.Fatalf("state after both ready = %v", st)
	}
	e.step(3)
	m = red.match().GetMatch()
	if m.GetRunTicks() != 4 || m.GetTicksRemaining() != 36 || len(m.GetScores()) != 0 {
		t.Fatalf("running info = %v (scores must stay private)", m)
	}
	adminCommand(e, &matchv1.EliminatePlayerCommand{PlayerId: blue.id, RemoveUnits: true})
	e.step(1)
	bm := blue.match().GetMatch()
	res := bm.GetResult()
	if bm.GetState() != rtsv1.MatchState_MATCH_STATE_FINISHED || res.GetWinnerId() != red.id || res.GetReason() != "last_player_standing" ||
		res.GetSeed() == 0 || len(res.GetPlayers()) != 2 || !bm.GetYouEliminated() || len(bm.GetScores()) != 2 {
		t.Fatalf("finished info for the loser = %v", bm)
	}
	exp, err := e.admin.ExportReplay(authed(e.ctx, "admin"), &rtsv1.ExportReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ver, err := e.admin.VerifyReplay(authed(e.ctx, "admin"), &rtsv1.VerifyReplayRequest{ReplayJson: exp.GetReplayJson()})
	if err != nil || !ver.GetOk() || !ver.GetRulesetMatches() {
		t.Fatalf("verify match 1: %v %v", ver, err)
	}

	// ---- reset: sessions survive, streams resync, IDs are per match ---------
	seed := uint64(99)
	rr, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{Seed: &seed})
	if err != nil || rr.GetSeed() != 99 || rr.GetMatchId() == firstID {
		t.Fatalf("reset: %v %v", rr, err)
	}
	msg, err := ro.stream.Recv()
	for err == nil && msg.GetSnapshot() == nil { // updates of the old match may still be buffered
		msg, err = ro.stream.Recv()
	}
	if err != nil || msg.GetSnapshot().GetMatch().GetMatchId() != rr.GetMatchId() {
		t.Fatalf("stream after reset: %v %v", msg, err)
	}
	m = red.match().GetMatch()
	if m.GetState() != rtsv1.MatchState_MATCH_STATE_READY || m.GetMatchId() != rr.GetMatchId() || m.GetReadyPlayers() != 0 {
		t.Fatalf("new match = %v", m)
	}
	newRed := m.GetYourPlayerId()
	again := e.connect("red") // reconnecting resolves to the same slot, no duplicate identity
	if again.id != newRed {
		t.Fatalf("reconnect id %d, match says %d", again.id, newRed)
	}

	// ---- match 2: time limit -------------------------------------------------
	red.submit("r2", &matchv1.SetReadyCommand{Ready: true})
	blue.submit("b2", &matchv1.SetReadyCommand{Ready: true})
	e.step(1 + 40)
	m = red.match().GetMatch()
	if m.GetState() != rtsv1.MatchState_MATCH_STATE_FINISHED || m.GetResult().GetReason() != "time_limit" || m.GetResult().GetSeed() != 99 {
		t.Fatalf("match 2 = %v", m)
	}

	// ---- match 3: admin termination without a winner ---------------------------
	if _, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{}); err != nil {
		t.Fatal(err)
	}
	adminCommand(e, &matchv1.EndMatchCommand{Reason: "cancelled"})
	e.step(1)
	gm := red.match()
	if r := gm.GetMatch().GetResult(); !r.GetDraw() || r.GetReason() != "cancelled" || r.GetWinnerId() != 0 {
		t.Fatalf("match 3 result = %v", r)
	}
	if len(gm.GetHistory()) != 2 || gm.GetHistory()[1].GetMatchId() != firstID {
		t.Fatalf("history = %v", gm.GetHistory())
	}
	// Earlier matches' replays remain exportable and verifiable.
	exp, err = e.admin.ExportReplay(authed(e.ctx, "admin"), &rtsv1.ExportReplayRequest{MatchId: firstID})
	if err != nil {
		t.Fatal(err)
	}
	ver, _ = e.admin.VerifyReplay(authed(e.ctx, "admin"), &rtsv1.VerifyReplayRequest{ReplayJson: exp.GetReplayJson()})
	if !ver.GetOk() {
		t.Fatalf("match 1 replay after resets: %v", ver)
	}
}

func TestResetValidationAndAdminDetails(t *testing.T) {
	e := matchEnv(t)
	red := e.connect("red")
	// Players cannot use the admin match API.
	if _, err := e.admin.ResetMatch(red.ctx, &rtsv1.ResetMatchRequest{}); err == nil {
		t.Fatal("player reset the match")
	}
	before := e.srv.MatchID()
	if _, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{ConfigJson: `{"world": {"width": 0}}`}); err == nil {
		t.Fatal("invalid config accepted")
	}
	if e.srv.MatchID() != before {
		t.Fatal("failed reset replaced the match")
	}
	d, err := e.admin.GetMatchDetails(authed(e.ctx, "admin"), &rtsv1.GetMatchDetailsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetSeed() == 0 && d.GetMatch().GetMatchId() == "" || len(d.GetPlayers()) != 2 {
		t.Fatalf("details = %v", d)
	}
	var cfg game.Config
	if err := json.Unmarshal([]byte(d.GetConfigJson()), &cfg); err != nil || cfg.AdminToken != "" || cfg.Players[0].JoinToken != "" {
		t.Fatalf("config not redacted: %v", d.GetConfigJson())
	}
	// A new configuration (other generator settings) takes effect; tokens
	// stay those of the server.
	cfg.World.Width = 40
	b, _ := json.Marshal(cfg)
	if _, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{ConfigJson: string(b)}); err != nil {
		t.Fatal(err)
	}
	if m := e.connect("red").match().GetMatch(); m.GetWidth() != 40 {
		t.Fatalf("new config not applied: %v", m)
	}
}

// Resets while the clock runs: the run loop must hand over to the new
// engine and observers must follow (run with -race).
func TestResetWhileRunning(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) { c.World.TickRateHz = 200 })
	runCtx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.srv.Run(runCtx) }()
	red := e.connect("red")
	ro := red.observe()
	for i := range 3 {
		if _, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{}); err != nil {
			t.Fatal(err)
		}
		id := e.srv.MatchID()
		// The stream switches to a snapshot of the new match, then the new
		// engine's ticks arrive.
		var snapID string
		ticks := 0
		for ticks < 5 {
			msg, err := ro.stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if s := msg.GetSnapshot(); s != nil {
				snapID = s.GetMatch().GetMatchId()
			} else if snapID == id {
				ticks++
			}
		}
		if snapID != id {
			t.Fatalf("reset %d: stream on match %s, current %s", i, snapID, id)
		}
	}
	cancel()
	<-done
}

// A disconnected player's units keep their orders: disconnecting never
// eliminates or pauses anyone.
func TestDisconnectedPlayerKeepsPlaying(t *testing.T) {
	e := newEnv(t)
	blue := e.connect("blue")
	bo := blue.observe()
	blue.submit("go", moveTo(e.blueUnit, 2, 7))
	bo.cancel() // the client goes away
	e.step(30)
	snap, err := e.admin.GetWorldSnapshot(authed(e.ctx, "admin"), &rtsv1.GetWorldSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p := find(snap.GetSnapshot().GetEntities(), e.blueUnit).GetPosition(); p.GetX() != 2 || p.GetY() != 7 {
		t.Fatalf("disconnected player's unit at %v", p)
	}
	// The stream's teardown is asynchronous; give it a moment.
	for try := 0; ; try++ {
		d, _ := e.admin.GetMatchDetails(authed(e.ctx, "admin"), &rtsv1.GetMatchDetailsRequest{})
		open := 0
		for _, p := range d.GetPlayers() {
			if p.GetEliminated() {
				t.Fatalf("disconnected player eliminated: %v", p)
			}
			open += int(p.GetConnections())
		}
		if open == 0 {
			break
		}
		if try == 200 {
			t.Fatalf("connections still open: %v", d.GetPlayers())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A reset builds a completely fresh world: with the same seed and ID key
// the new match starts in exactly the state the first one did.
func TestResetIsolatesMatchState(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) {
		c.World.IDKey = "000102030405060708090a0b0c0d0e0f"
		c.Plugins = map[string]json.RawMessage{"match": json.RawMessage(`{"victory": {"time_limit_ticks": 1000}}`)}
	})
	initial := e.srv.Engine().World().StateHash()
	red := e.connect("red")
	red.submit("go", moveTo(e.redUnit, 5, 5))
	e.step(25)
	if e.srv.Engine().World().StateHash() == initial {
		t.Fatal("world did not change")
	}
	seed := uint64(1) // the test config's seed
	if _, err := e.admin.ResetMatch(authed(e.ctx, "admin"), &rtsv1.ResetMatchRequest{Seed: &seed}); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.Engine().World().StateHash(); got != initial {
		t.Fatalf("reset world differs from the original initial state: %x vs %x", got, initial)
	}
	if m := red.match().GetMatch(); m.GetRunTicks() != 0 || m.GetTick() != 0 {
		t.Fatalf("clock carried over: %v", m)
	}
}
