package server_test

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	wellsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/wells/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// adminObserver reads the admin ObserveWorld stream.
type adminObserver struct {
	t      *testing.T
	stream interface {
		Recv() (*rtsv1.ObserveWorldResponse, error)
	}
	snap *rtsv1.WorldSnapshot
}

func (e *env) observeAdmin() *adminObserver {
	e.t.Helper()
	s, err := e.admin.ObserveWorld(authed(e.ctx, "admin"), &rtsv1.ObserveWorldRequest{})
	if err != nil {
		e.t.Fatal(err)
	}
	first, err := s.Recv()
	if err != nil || first.GetSnapshot() == nil {
		e.t.Fatalf("first admin message: %v %v", first, err)
	}
	return &adminObserver{t: e.t, stream: s, snap: first.GetSnapshot()}
}

// until reads updates until fn returns true, failing after 200 updates.
func (o *adminObserver) until(fn func(u *rtsv1.TickUpdate) bool) *rtsv1.TickUpdate {
	o.t.Helper()
	for range 200 {
		m, err := o.stream.Recv()
		if err != nil {
			o.t.Fatal(err)
		}
		if u := m.GetUpdate(); u != nil && fn(u) {
			return u
		}
	}
	o.t.Fatal("condition not reached")
	return nil
}

func resultFor(u *rtsv1.TickUpdate, id string) *rtsv1.CommandResult {
	for _, r := range u.GetCommandResults() {
		if r.GetCommandId() == id {
			return r
		}
	}
	return nil
}

func adminCommand(e *env, msg proto.Message) *rtsv1.SubmitAdminCommandResponse {
	e.t.Helper()
	payload, err := anypb.New(msg)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := e.admin.SubmitAdminCommand(authed(e.ctx, "admin"), &rtsv1.SubmitAdminCommandRequest{Payload: payload})
	if err != nil || !resp.GetAccepted() {
		e.t.Fatalf("admin command: %v %v", resp, err)
	}
	return resp
}

func ptr[T any](v T) *T { return &v }

// runEngine runs the simulation loop in the background, as the server does.
func (e *env) runEngine(interval time.Duration) {
	go func() { _ = e.engine.Run(e.ctx, interval) }()
}

func TestAdminMutationsDuringActiveSimulation(t *testing.T) {
	e := newEnv(t)
	e.runEngine(2 * time.Millisecond)
	adm := authed(e.ctx, "admin")
	red := e.connect("red")
	ao := e.observeAdmin()

	// A player keeps commanding while the admin mutates the world.
	red.submit("walk", move(e.redUnit, 6, 0))

	spawn, err := e.admin.SpawnUnit(adm, &rtsv1.SpawnUnitRequest{OwnerId: red.id, Position: &rtsv1.Position{X: 5, Y: 5}})
	if err != nil {
		t.Fatal(err)
	}
	u := ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, spawn.GetCommandId()) != nil })
	if r := resultFor(u, spawn.GetCommandId()); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED || r.GetTick() != spawn.GetScheduledTick() {
		t.Fatalf("spawn result = %v (scheduled %d)", r, spawn.GetScheduledTick())
	}
	var spawned uint64
	for _, ev := range u.GetEvents() {
		if ev.GetType() == sim.EventEntityCreated {
			spawned = ev.GetEntityId()
		}
	}
	if spawned == 0 || find(u.GetChanged(), spawned).GetOwnerId() != red.id {
		t.Fatalf("spawned entity not reported: %v", u)
	}

	// Reposition, then delete it.
	mv, _ := e.admin.MoveEntity(adm, &rtsv1.MoveEntityRequest{EntityId: spawned, Position: &rtsv1.Position{X: 1, Y: 6}})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, mv.GetCommandId()) != nil })
	if p := find(u.GetChanged(), spawned).GetPosition(); p.GetX() != 1 || p.GetY() != 6 {
		t.Fatalf("moved to %v", p)
	}
	del, _ := e.admin.DeleteEntity(adm, &rtsv1.DeleteEntityRequest{EntityId: spawned})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, del.GetCommandId()) != nil })
	if len(u.GetRemoved()) != 1 || u.GetRemoved()[0] != spawned {
		t.Fatalf("removed = %v", u.GetRemoved())
	}

	// Invalid mutations fail at execution with coded results.
	bad, _ := e.admin.MoveEntity(adm, &rtsv1.MoveEntityRequest{EntityId: e.blueUnit, Position: &rtsv1.Position{X: 99, Y: 0}})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, bad.GetCommandId()) != nil })
	if r := resultFor(u, bad.GetCommandId()); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_FAILED {
		t.Fatalf("out-of-bounds move = %v", r)
	}
	gone, _ := e.admin.DeleteEntity(adm, &rtsv1.DeleteEntityRequest{EntityId: spawned})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, gone.GetCommandId()) != nil })
	if r := resultFor(u, gone.GetCommandId()); r.GetErrorCode() != sim.CodeNotFound {
		t.Fatalf("double delete = %v", r)
	}

	// Wells: create, configure, delete.
	cw := adminCommand(e, &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 3, Y: 3}, Energy: ptr[int64](40), Capacity: ptr[int64](50)})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, cw.GetCommandId()) != nil })
	var well *rtsv1.Entity
	for _, en := range u.GetChanged() {
		if en.GetKind() == rtsv1.EntityKind_ENTITY_KIND_RESOURCE {
			well = en
		}
	}
	if well == nil || well.GetAttributes()["wells.energy"].GetIntValue() != 40 || well.GetAttributes()["wells.extraction_rate"].GetIntValue() != 5 {
		t.Fatalf("well = %v", well)
	}
	cfg := adminCommand(e, &wellsv1.ConfigureWellCommand{WellId: well.GetId(), ExtractionRate: ptr[int64](9), Unlimited: ptr(true)})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, cfg.GetCommandId()) != nil })
	if w := find(u.GetChanged(), well.GetId()); w.GetAttributes()["wells.extraction_rate"].GetIntValue() != 9 || !w.GetAttributes()["wells.unlimited"].GetBoolValue() {
		t.Fatalf("configured well = %v", w)
	}
	badCfg := adminCommand(e, &wellsv1.ConfigureWellCommand{WellId: well.GetId(), Energy: ptr[int64](51)})
	u = ao.until(func(u *rtsv1.TickUpdate) bool { return resultFor(u, badCfg.GetCommandId()) != nil })
	if r := resultFor(u, badCfg.GetCommandId()); r.GetErrorCode() != sim.CodeInvalidArgument {
		t.Fatalf("energy above capacity = %v", r)
	}

	// The player's own command kept executing throughout.
	snap, err := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if find(snap.GetSnapshot().GetEntities(), e.redUnit).GetPosition().GetX() != 6 {
		t.Fatal("player movement did not complete during admin activity")
	}
}

func TestPauseResumeAndStep(t *testing.T) {
	e := newEnv(t)
	adm := authed(e.ctx, "admin")
	e.runEngine(2 * time.Millisecond)

	tickNow := func() uint64 {
		s, err := e.admin.GetStatus(adm, &rtsv1.GetStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return s.GetStatus().GetTick()
	}
	waitTick := func(want uint64) {
		deadline := time.Now().Add(5 * time.Second)
		for tickNow() < want {
			if time.Now().After(deadline) {
				t.Fatalf("tick stuck at %d, want %d", tickNow(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	waitTick(3)
	if _, err := e.admin.Step(adm, &rtsv1.StepRequest{Count: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("step while running: %v", err)
	}
	if _, err := e.admin.SetPaused(adm, &rtsv1.SetPausedRequest{Paused: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // let an in-flight tick finish
	frozen := tickNow()
	time.Sleep(30 * time.Millisecond)
	if got := tickNow(); got != frozen {
		t.Fatalf("tick advanced while paused: %d -> %d", frozen, got)
	}
	if s, _ := e.admin.GetStatus(adm, &rtsv1.GetStatusRequest{}); !s.GetStatus().GetPaused() {
		t.Fatal("status not paused")
	}

	// Commands submitted while paused wait for the next executed tick.
	red := e.connect("red")
	ack := red.submit("queued", move(e.redUnit, 1, 0))
	if ack.GetScheduledTick() != frozen+1 {
		t.Fatalf("scheduled %d, want %d", ack.GetScheduledTick(), frozen+1)
	}

	step, err := e.admin.Step(adm, &rtsv1.StepRequest{Count: 3})
	if err != nil || step.GetFirstTick() != frozen+1 || step.GetCount() != 3 {
		t.Fatalf("step: %v %v", step, err)
	}
	waitTick(frozen + 3)
	time.Sleep(20 * time.Millisecond)
	if got := tickNow(); got != frozen+3 {
		t.Fatalf("after step 3: tick %d, want exactly %d", got, frozen+3)
	}
	snap, _ := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if find(snap.GetSnapshot().GetEntities(), e.redUnit).GetPosition().GetX() != 1 {
		t.Fatal("queued command did not execute during stepping")
	}
	if _, err := e.admin.Step(adm, &rtsv1.StepRequest{Count: 5000}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized step: %v", err)
	}

	if _, err := e.admin.SetPaused(adm, &rtsv1.SetPausedRequest{Paused: false}); err != nil {
		t.Fatal(err)
	}
	waitTick(frozen + 10)
}

func TestAdminAuthorizationOnAllRPCs(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	for name, ctx := range map[string]*player{"no token": {e: e, ctx: e.ctx}, "player session": red, "join token": {e: e, ctx: authed(e.ctx, "red")}} {
		c := ctx.ctx
		calls := map[string]error{}
		_, calls["GetWorldSnapshot"] = e.admin.GetWorldSnapshot(c, &rtsv1.GetWorldSnapshotRequest{})
		_, calls["GetStatus"] = e.admin.GetStatus(c, &rtsv1.GetStatusRequest{})
		_, calls["SpawnUnit"] = e.admin.SpawnUnit(c, &rtsv1.SpawnUnitRequest{Position: &rtsv1.Position{}})
		_, calls["DeleteEntity"] = e.admin.DeleteEntity(c, &rtsv1.DeleteEntityRequest{EntityId: e.redUnit})
		_, calls["MoveEntity"] = e.admin.MoveEntity(c, &rtsv1.MoveEntityRequest{EntityId: e.redUnit, Position: &rtsv1.Position{}})
		_, calls["SubmitAdminCommand"] = e.admin.SubmitAdminCommand(c, &rtsv1.SubmitAdminCommandRequest{})
		_, calls["SetPaused"] = e.admin.SetPaused(c, &rtsv1.SetPausedRequest{Paused: true})
		_, calls["Step"] = e.admin.Step(c, &rtsv1.StepRequest{})
		stream, err := e.admin.ObserveWorld(c, &rtsv1.ObserveWorldRequest{})
		if err == nil {
			_, err = stream.Recv()
		}
		calls["ObserveWorld"] = err
		for rpc, err := range calls {
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s with %s: %v", rpc, name, err)
			}
		}
	}
	if e.engine.Paused() {
		t.Fatal("unauthorized SetPaused took effect")
	}
}

func TestPlayersCannotUseAdminCommandsOrSeeAdminData(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	ro := red.observe()
	// Admin-only plugin commands are rejected for players at submission.
	if ack := red.submit("w", &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 4, Y: 4}}); ack.GetAccepted() {
		t.Fatalf("player created a well: %v", ack)
	}
	cw := adminCommand(e, &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 4, Y: 4}})
	e.engine.Step()
	u := ro.next()
	// Players see the well (public attributes) but not the admin's result
	// or the admin-only creation event.
	if len(u.GetCommandResults()) != 0 || len(u.GetEvents()) != 0 {
		t.Fatalf("player saw admin data: %v", u)
	}
	var sawWell bool
	for _, en := range u.GetChanged() {
		if en.GetKind() == rtsv1.EntityKind_ENTITY_KIND_RESOURCE && en.GetAttributes()["wells.energy"] != nil {
			sawWell = true
		}
	}
	if !sawWell || cw.GetScheduledTick() != 1 {
		t.Fatal("player did not see the new well")
	}
}
