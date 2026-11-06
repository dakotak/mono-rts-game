// Package server exposes a running simulation over gRPC. It implements the
// public GameService and the privileged AdminService.
//
// Handlers never mutate the world: commands go through sim.Engine.Submit
// (a queue drained by the simulation goroutine) and reads come from the Hub,
// which is fed frames by the simulation goroutine.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// maxCommandIDLen bounds client-chosen command IDs.
const maxCommandIDLen = 128

// ackHistory is how many acknowledgments are remembered per player for
// idempotent resubmission.
const ackHistory = 4096

// Server holds the state shared by both services.
type Server struct {
	cfg     *game.Config
	players []game.Player
	engine  *sim.Engine
	hub     *Hub
	info    *rtsv1.WorldInfo

	mu       sync.Mutex
	sessions map[string]sim.PlayerID // session token -> player
	acks     map[sim.PlayerID]*ackLog

	adminSeq atomic.Uint64
}

type ackLog struct {
	byID  map[string]*rtsv1.SubmitCommandResponse
	order []string
}

// New wires a server to a game. It installs the hub as the engine's frame
// sink, so call it before the engine starts.
func New(cfg *game.Config, g *game.Game) *Server {
	engine := g.Engine
	hub := NewHub(engine.NextTick()-1, engine.World().FullView())
	engine.SetFrameSink(hub.Publish)

	info := &rtsv1.WorldInfo{
		Width:        cfg.World.Width,
		Height:       cfg.World.Height,
		TickRateHz:   cfg.World.TickRateHz,
		CommandTypes: engine.Rules().CommandTypes(false),
		Plugins:      engine.Rules().Plugins(),
	}
	return &Server{
		cfg:      cfg,
		players:  g.Players,
		engine:   engine,
		hub:      hub,
		info:     info,
		sessions: make(map[string]sim.PlayerID),
		acks:     make(map[sim.PlayerID]*ackLog),
	}
}

// Hub returns the observation hub.
func (s *Server) Hub() *Hub { return s.hub }

// Register adds both services to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	rtsv1.RegisterGameServiceServer(g, &gameService{s: s})
	rtsv1.RegisterAdminServiceServer(g, &adminService{s: s})
}

func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if tok, ok := strings.CutPrefix(v, "Bearer "); ok {
			return tok
		}
	}
	return ""
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// playerFromContext authenticates a player session.
func (s *Server) playerFromContext(ctx context.Context) (sim.PlayerID, error) {
	tok := bearer(ctx)
	s.mu.Lock()
	p, ok := s.sessions[tok]
	s.mu.Unlock()
	if tok == "" || !ok {
		return 0, status.Error(codes.Unauthenticated, "missing or invalid session token")
	}
	return p, nil
}

// requireAdmin authenticates an admin request. Player sessions are never
// accepted here, and the admin token is never accepted by GameService.
func (s *Server) requireAdmin(ctx context.Context) error {
	tok := bearer(ctx)
	if s.cfg.AdminToken == "" {
		return status.Error(codes.PermissionDenied, "admin API disabled (no admin_token configured)")
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid admin token")
	}
	return nil
}

type gameService struct {
	rtsv1.UnimplementedGameServiceServer
	s *Server
}

func (g *gameService) Connect(_ context.Context, req *rtsv1.ConnectRequest) (*rtsv1.ConnectResponse, error) {
	for _, p := range g.s.players {
		if subtle.ConstantTimeCompare([]byte(req.GetJoinToken()), []byte(p.JoinToken)) == 1 {
			tok := newToken()
			g.s.mu.Lock()
			g.s.sessions[tok] = p.ID
			g.s.mu.Unlock()
			return &rtsv1.ConnectResponse{PlayerId: uint32(p.ID), PlayerName: p.Name, SessionToken: tok, World: g.s.info}, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "unknown join token")
}

func (g *gameService) SubmitCommand(ctx context.Context, req *rtsv1.SubmitCommandRequest) (*rtsv1.SubmitCommandResponse, error) {
	player, err := g.s.playerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	id := req.GetCommandId()
	if id == "" || len(id) > maxCommandIDLen {
		return nil, status.Errorf(codes.InvalidArgument, "command_id must be 1..%d bytes", maxCommandIDLen)
	}
	reject := func(reason string) *rtsv1.SubmitCommandResponse {
		return &rtsv1.SubmitCommandResponse{CommandId: id, RejectionReason: reason}
	}
	if req.GetPayload() == nil {
		return reject("payload missing"), nil
	}

	// Hold the lock across dedupe check and submit so concurrent retries of
	// one command id cannot both be scheduled.
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	log := g.s.acks[player]
	if log == nil {
		log = &ackLog{byID: make(map[string]*rtsv1.SubmitCommandResponse)}
		g.s.acks[player] = log
	}
	if prev, ok := log.byID[id]; ok {
		return prev, nil
	}

	msg, err := anypb.UnmarshalNew(req.GetPayload(), protoUnmarshalOptions)
	if err != nil {
		return reject(fmt.Sprintf("cannot decode payload %q: %v", req.GetPayload().GetTypeUrl(), err)), nil
	}
	typ := string(msg.ProtoReflect().Descriptor().FullName())
	tick, err := g.s.engine.Submit(sim.Command{ID: id, Issuer: sim.Principal{Player: player}, Type: typ, Payload: msg})
	if err != nil {
		return reject(err.Error()), nil
	}
	ack := &rtsv1.SubmitCommandResponse{CommandId: id, Accepted: true, ScheduledTick: uint64(tick)}
	log.byID[id] = ack
	log.order = append(log.order, id)
	if len(log.order) > ackHistory {
		delete(log.byID, log.order[0])
		log.order = log.order[1:]
	}
	return ack, nil
}

func (g *gameService) GetSnapshot(ctx context.Context, _ *rtsv1.GetSnapshotRequest) (*rtsv1.GetSnapshotResponse, error) {
	player, err := g.s.playerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	tick, es := g.s.hub.Snapshot()
	return &rtsv1.GetSnapshotResponse{Snapshot: viewer{player: player}.snapshot(tick, es)}, nil
}

func (g *gameService) Observe(_ *rtsv1.ObserveRequest, stream grpc.ServerStreamingServer[rtsv1.ObserveResponse]) error {
	player, err := g.s.playerFromContext(stream.Context())
	if err != nil {
		return err
	}
	return g.s.observe(stream, viewer{player: player})
}

// observe streams a snapshot followed by per-tick updates, resynchronizing
// with a new snapshot whenever the hub drops this observer for lagging.
func (s *Server) observe(stream grpc.ServerStreamingServer[rtsv1.ObserveResponse], v viewer) error {
	ctx := stream.Context()
	for {
		tick, es, sub := s.hub.Subscribe()
		err := stream.Send(&rtsv1.ObserveResponse{Kind: &rtsv1.ObserveResponse_Snapshot{Snapshot: v.snapshot(tick, es)}})
		if err != nil {
			s.hub.Unsubscribe(sub)
			return err
		}
		lagged, err := func() (bool, error) {
			defer s.hub.Unsubscribe(sub)
			for {
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case f, ok := <-sub.ch:
					if !ok {
						return true, nil
					}
					if err := stream.Send(&rtsv1.ObserveResponse{Kind: &rtsv1.ObserveResponse_Update{Update: v.update(f)}}); err != nil {
						return false, err
					}
				}
			}
		}()
		if !lagged {
			return err
		}
	}
}

type adminService struct {
	rtsv1.UnimplementedAdminServiceServer
	s *Server
}

func (a *adminService) GetWorldSnapshot(ctx context.Context, _ *rtsv1.GetWorldSnapshotRequest) (*rtsv1.GetWorldSnapshotResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	tick, es := a.s.hub.Snapshot()
	resp := &rtsv1.GetWorldSnapshotResponse{Snapshot: viewer{admin: true}.snapshot(tick, es)}
	for _, p := range a.s.players {
		resp.Players = append(resp.Players, &rtsv1.PlayerInfo{Id: uint32(p.ID), Name: p.Name})
	}
	return resp, nil
}

func (a *adminService) SpawnUnit(ctx context.Context, req *rtsv1.SpawnUnitRequest) (*rtsv1.SpawnUnitResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	owner := sim.PlayerID(req.GetOwnerId())
	known := owner == 0
	for _, p := range a.s.players {
		known = known || p.ID == owner
	}
	if !known {
		return nil, status.Errorf(codes.InvalidArgument, "unknown owner %d", owner)
	}
	if req.GetPosition() == nil {
		return nil, status.Error(codes.InvalidArgument, "position required")
	}
	id := fmt.Sprintf("admin-%d", a.s.adminSeq.Add(1))
	tick, err := a.s.engine.Submit(sim.Command{
		ID:      id,
		Issuer:  sim.Principal{Admin: true},
		Type:    sim.CmdCreateEntity,
		Payload: sim.CreateEntity{Kind: sim.KindUnit, Owner: owner, Pos: sim.Pos{X: req.GetPosition().GetX(), Y: req.GetPosition().GetY()}},
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &rtsv1.SpawnUnitResponse{CommandId: id, ScheduledTick: uint64(tick)}, nil
}

func (a *adminService) SetPaused(ctx context.Context, req *rtsv1.SetPausedRequest) (*rtsv1.SetPausedResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	a.s.engine.SetPaused(req.GetPaused())
	return &rtsv1.SetPausedResponse{Paused: req.GetPaused(), Tick: uint64(a.s.engine.NextTick() - 1)}, nil
}

// protoUnmarshalOptions resolves Any payloads against the global registry,
// which contains every compiled-in plugin's command messages.
var protoUnmarshalOptions = proto.UnmarshalOptions{}
