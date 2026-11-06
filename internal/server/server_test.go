package server_test

import (
	"cmp"
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// env runs a real gRPC server over an in-memory listener. The test goroutine
// plays the simulation goroutine by calling step.
type env struct {
	// Entity IDs are opaque, so tests look up the one unit each player
	// starts with instead of assuming IDs.
	redUnit, blueUnit uint64
	t                 *testing.T
	ctx               context.Context
	engine            *sim.Engine
	game              rtsv1.GameServiceClient
	admin             rtsv1.AdminServiceClient
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &game.Config{
		World:      game.WorldConfig{Width: 12, Height: 8, Seed: 1, TickRateHz: 10},
		Players:    []game.PlayerConfig{{Slot: 1, Name: "Red", JoinToken: "red"}, {Slot: 2, Name: "Blue", JoinToken: "blue"}},
		AdminToken: "admin",
		Units: []game.UnitBlock{
			{Owner: 1, X: 0, Y: 0},  // red unit
			{Owner: 2, X: 11, Y: 7}, // blue unit
		},
	}
	gm, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := gm.Engine
	srv := server.New(cfg, gm)
	redID, _ := engine.World().EntityAt(sim.Pos{X: 0, Y: 0})
	blueID, _ := engine.World().EntityAt(sim.Pos{X: 11, Y: 7})

	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &env{redUnit: uint64(redID), blueUnit: uint64(blueID), t: t, ctx: ctx, engine: engine, game: rtsv1.NewGameServiceClient(conn), admin: rtsv1.NewAdminServiceClient(conn)}
}

func authed(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

type player struct {
	e   *env
	id  uint32
	ctx context.Context
}

func (e *env) connect(join string) *player {
	e.t.Helper()
	resp, err := e.game.Connect(e.ctx, &rtsv1.ConnectRequest{JoinToken: join})
	if err != nil {
		e.t.Fatal(err)
	}
	return &player{e: e, id: resp.GetPlayerId(), ctx: authed(e.ctx, resp.GetSessionToken())}
}

func (p *player) submit(id string, msg proto.Message) *rtsv1.SubmitCommandResponse {
	p.e.t.Helper()
	payload, err := anypb.New(msg)
	if err != nil {
		p.e.t.Fatal(err)
	}
	ack, err := p.e.game.SubmitCommand(p.ctx, &rtsv1.SubmitCommandRequest{CommandId: id, Payload: payload})
	if err != nil {
		p.e.t.Fatal(err)
	}
	return ack
}

// observer wraps an Observe stream that has already received its initial
// snapshot, so the subscription is guaranteed to be live.
type observer struct {
	t      *testing.T
	stream grpc.ServerStreamingClient[rtsv1.ObserveResponse]
	snap   *rtsv1.WorldSnapshot
	cancel context.CancelFunc
}

func (p *player) observe() *observer {
	p.e.t.Helper()
	ctx, cancel := context.WithCancel(p.ctx)
	stream, err := p.e.game.Observe(ctx, &rtsv1.ObserveRequest{})
	if err != nil {
		p.e.t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		p.e.t.Fatal(err)
	}
	if first.GetSnapshot() == nil {
		p.e.t.Fatalf("first message is not a snapshot: %v", first)
	}
	return &observer{t: p.e.t, stream: stream, snap: first.GetSnapshot(), cancel: cancel}
}

func (o *observer) next() *rtsv1.TickUpdate {
	o.t.Helper()
	m, err := o.stream.Recv()
	if err != nil {
		o.t.Fatal(err)
	}
	if m.GetUpdate() == nil {
		o.t.Fatalf("expected update, got %v", m)
	}
	return m.GetUpdate()
}

// snapshotOf fetches the player's current snapshot entities.
func snapshotOf(p *player) []*rtsv1.Entity {
	p.e.t.Helper()
	resp, err := p.e.game.GetSnapshot(p.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil {
		p.e.t.Fatal(err)
	}
	return resp.GetSnapshot().GetEntities()
}

func move(unit uint64, x, y int32) *movementv1.MoveCommand {
	return &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: unit, Destination: &rtsv1.Position{X: x, Y: y}}}}
}

func find(es []*rtsv1.Entity, id uint64) *rtsv1.Entity {
	for _, e := range es {
		if e.GetId() == id {
			return e
		}
	}
	return nil
}

func TestTwoPlayersMoveConcurrently(t *testing.T) {
	e := newEnv(t)
	red, blue := e.connect("red"), e.connect("blue")
	if red.id == 0 || blue.id == 0 || red.id == blue.id {
		t.Fatalf("player ids %d %d", red.id, blue.id)
	}
	if u := find(snapshotOf(red), e.redUnit); u.GetOwnerId() != red.id {
		t.Fatalf("red unit owner = %d, want %d", u.GetOwnerId(), red.id)
	}
	ro, bo := red.observe(), blue.observe()
	if len(ro.snap.GetEntities()) != 2 {
		t.Fatalf("snapshot = %v", ro.snap)
	}

	if ack := red.submit("r1", move(e.redUnit, 3, 0)); !ack.GetAccepted() || ack.GetScheduledTick() != 1 {
		t.Fatalf("red ack = %v", ack)
	}
	if ack := blue.submit("b1", move(e.blueUnit, 8, 7)); !ack.GetAccepted() {
		t.Fatalf("blue ack = %v", ack)
	}
	e.engine.Step()

	ru, bu := ro.next(), bo.next()
	if ru.GetTick() != 1 || bu.GetTick() != 1 {
		t.Fatalf("ticks %d %d", ru.GetTick(), bu.GetTick())
	}
	// Each player sees only their own command result.
	if len(ru.GetCommandResults()) != 1 || ru.GetCommandResults()[0].GetCommandId() != "r1" ||
		ru.GetCommandResults()[0].GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED {
		t.Fatalf("red results = %v", ru.GetCommandResults())
	}
	if len(bu.GetCommandResults()) != 1 || bu.GetCommandResults()[0].GetCommandId() != "b1" {
		t.Fatalf("blue results = %v", bu.GetCommandResults())
	}
	// Both see both units move; only the owner sees the destination.
	for _, u := range []*rtsv1.TickUpdate{ru, bu} {
		r, b := find(u.GetChanged(), e.redUnit), find(u.GetChanged(), e.blueUnit)
		if r.GetPosition().GetX() != 1 || b.GetPosition().GetX() != 10 {
			t.Fatalf("positions: %v %v", r, b)
		}
	}
	if find(ru.GetChanged(), e.redUnit).GetAttributes()["movement.destination"] == nil {
		t.Error("red cannot see own destination")
	}
	if find(ru.GetChanged(), e.blueUnit).GetAttributes()["movement.destination"] != nil {
		t.Error("red can see blue's destination")
	}
	if find(bu.GetChanged(), e.redUnit).GetAttributes()["movement.destination"] != nil {
		t.Error("blue can see red's destination")
	}

	// Walk to arrival; red receives a private arrival event.
	e.engine.Step()
	bo.next()
	e.engine.Step()
	bo.next()
	ro.next()
	ru = ro.next()
	if len(ru.GetEvents()) != 1 || ru.GetEvents()[0].GetType() != "movement.arrived" {
		t.Fatalf("red events = %v", ru.GetEvents())
	}
}

func TestOwnershipEnforcedAtExecution(t *testing.T) {
	e := newEnv(t)
	red, blue := e.connect("red"), e.connect("blue")
	ro, bo := red.observe(), blue.observe()
	// The envelope is valid, so it is accepted; execution fails.
	if ack := blue.submit("steal", move(e.redUnit, 5, 5)); !ack.GetAccepted() {
		t.Fatalf("ack = %v", ack)
	}
	e.engine.Step()
	r := bo.next().GetCommandResults()
	if len(r) != 1 || r[0].GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_FAILED || r[0].GetErrorCode() != sim.CodePermissionDenied {
		t.Fatalf("blue results = %v", r)
	}
	ru := ro.next()
	if len(ru.GetCommandResults()) != 0 || len(ru.GetChanged()) != 2 {
		// tick 1 carries the initial entities as changed; nothing else.
		t.Fatalf("red update = %v", ru)
	}
	if find(ru.GetChanged(), e.redUnit).GetPosition().GetX() != 0 {
		t.Fatal("red unit moved")
	}
}

func TestRejectedCommands(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	// A registered proto type that is not a command.
	if ack := red.submit("x", &rtsv1.Position{}); ack.GetAccepted() || ack.GetRejectionReason() == "" {
		t.Fatalf("non-command accepted: %v", ack)
	}
	// Unknown type URL.
	ack, err := e.game.SubmitCommand(red.ctx, &rtsv1.SubmitCommandRequest{CommandId: "y", Payload: &anypb.Any{TypeUrl: "type.googleapis.com/nope.Nope"}})
	if err != nil || ack.GetAccepted() {
		t.Fatalf("unknown type: %v %v", ack, err)
	}
	ack, err = e.game.SubmitCommand(red.ctx, &rtsv1.SubmitCommandRequest{CommandId: "z"})
	if err != nil || ack.GetAccepted() {
		t.Fatalf("missing payload: %v %v", ack, err)
	}
	_, err = e.game.SubmitCommand(red.ctx, &rtsv1.SubmitCommandRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id: %v", err)
	}
}

func TestDuplicateCommandIDIsIdempotent(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	ro := red.observe()
	a1 := red.submit("same", move(e.redUnit, 5, 0))
	e.engine.Step()
	a2 := red.submit("same", move(e.redUnit, 0, 5))
	if !proto.Equal(a1, a2) {
		t.Fatalf("acks differ: %v vs %v", a1, a2)
	}
	e.engine.Step()
	if n := len(ro.next().GetCommandResults()) + len(ro.next().GetCommandResults()); n != 1 {
		t.Fatalf("executed %d times", n)
	}
	// Another player may reuse the id.
	if ack := e.connect("blue").submit("same", move(e.blueUnit, 0, 0)); !ack.GetAccepted() || ack.GetScheduledTick() != 3 {
		t.Fatalf("blue ack = %v", ack)
	}
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	if _, err := e.game.Connect(e.ctx, &rtsv1.ConnectRequest{JoinToken: "nope"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("bad join token: %v", err)
	}
	if _, err := e.game.GetSnapshot(e.ctx, &rtsv1.GetSnapshotRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no session: %v", err)
	}
	if _, err := e.game.GetSnapshot(authed(e.ctx, "admin"), &rtsv1.GetSnapshotRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("admin token accepted by GameService: %v", err)
	}
	red := e.connect("red")
	if _, err := e.admin.GetWorldSnapshot(red.ctx, &rtsv1.GetWorldSnapshotRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("session accepted by AdminService: %v", err)
	}
	if _, err := e.admin.GetWorldSnapshot(authed(e.ctx, "red"), &rtsv1.GetWorldSnapshotRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("join token accepted by AdminService: %v", err)
	}
}

func TestAdminSnapshotAndSpawn(t *testing.T) {
	e := newEnv(t)
	red, blue := e.connect("red"), e.connect("blue")
	red.submit("r", move(e.redUnit, 5, 0))
	blue.submit("b", move(e.blueUnit, 5, 7))
	adm := authed(e.ctx, "admin")
	resp, err := e.admin.SpawnUnit(adm, &rtsv1.SpawnUnitRequest{OwnerId: blue.id, Position: &rtsv1.Position{X: 6, Y: 4}})
	if err != nil || resp.GetScheduledTick() != 1 {
		t.Fatalf("spawn: %v %v", resp, err)
	}
	if _, err := e.admin.SpawnUnit(adm, &rtsv1.SpawnUnitRequest{OwnerId: 9, Position: &rtsv1.Position{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown owner: %v", err)
	}
	e.engine.Step()

	// Admin sees everything, including both players' private destinations.
	snap, err := e.admin.GetWorldSnapshot(adm, &rtsv1.GetWorldSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	es := snap.GetSnapshot().GetEntities()
	var spawned *rtsv1.Entity
	for _, en := range es {
		if en.GetPosition().GetX() == 6 && en.GetPosition().GetY() == 4 {
			spawned = en
		}
	}
	if len(es) != 3 || spawned.GetOwnerId() != blue.id {
		t.Fatalf("admin snapshot = %v", es)
	}
	if len(snap.GetPlayers()) != 2 {
		t.Fatalf("admin players = %v", snap.GetPlayers())
	}
	if find(es, e.redUnit).GetAttributes()["movement.destination"] == nil || find(es, e.blueUnit).GetAttributes()["movement.destination"] == nil {
		t.Fatal("admin missing private attributes")
	}
	// Players see the spawned unit too.
	ps, err := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil || len(ps.GetSnapshot().GetEntities()) != 3 || ps.GetSnapshot().GetTick() != 1 {
		t.Fatalf("player snapshot: %v %v", ps, err)
	}
	if p, err := e.admin.SetPaused(adm, &rtsv1.SetPausedRequest{Paused: true}); err != nil || !p.GetPaused() || !e.engine.Paused() {
		t.Fatalf("pause: %v %v", p, err)
	}
}

// applyUpdate folds a TickUpdate into an entity list, as a client would.
func applyUpdate(es []*rtsv1.Entity, u *rtsv1.TickUpdate) []*rtsv1.Entity {
	byID := map[uint64]*rtsv1.Entity{}
	for _, e := range es {
		byID[e.GetId()] = e
	}
	for _, e := range u.GetChanged() {
		byID[e.GetId()] = e
	}
	for _, id := range u.GetRemoved() {
		delete(byID, id)
	}
	out := make([]*rtsv1.Entity, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b *rtsv1.Entity) int { return cmp.Compare(a.GetId(), b.GetId()) })
	return out
}

func TestSnapshotPlusUpdatesEqualsLaterSnapshot(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	ro := red.observe()
	state := ro.snap.GetEntities()
	red.submit("a", move(e.redUnit, 9, 6))
	e.connect("blue").submit("b", move(e.blueUnit, 2, 1))
	for range 6 {
		e.engine.Step()
		state = applyUpdate(state, ro.next())
	}
	snap, err := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.GetSnapshot().GetTick() != 6 {
		t.Fatalf("tick = %d", snap.GetSnapshot().GetTick())
	}
	want := &rtsv1.WorldSnapshot{Tick: 6, Entities: state}
	if !proto.Equal(want, snap.GetSnapshot()) {
		t.Fatalf("incremental state diverged\n got %v\nwant %v", snap.GetSnapshot(), want)
	}
}

func TestReconnectRecoversState(t *testing.T) {
	e := newEnv(t)
	red := e.connect("red")
	ro := red.observe()
	red.submit("a", move(e.redUnit, 4, 0))
	e.engine.Step()
	ro.next()
	ro.cancel() // client drops

	// Simulation keeps running without the client.
	for range 3 {
		e.engine.Step()
	}

	// Same session can resubscribe...
	again := red.observe()
	if again.snap.GetTick() != 4 || find(again.snap.GetEntities(), e.redUnit).GetPosition().GetX() != 4 {
		t.Fatalf("resubscribe snapshot = %v", again.snap)
	}
	// ...and a fresh Connect with the join token yields the same player.
	red2 := e.connect("red")
	if red2.id != red.id {
		t.Fatalf("reconnected as %d", red2.id)
	}
	o2 := red2.observe()
	if !proto.Equal(o2.snap, again.snap) {
		t.Fatal("snapshots differ between sessions of the same player")
	}
	e.engine.Step()
	if again.next().GetTick() != 5 || o2.next().GetTick() != 5 {
		t.Fatal("observers out of sync")
	}
}
