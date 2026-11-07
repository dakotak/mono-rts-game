// Package server exposes a running simulation over gRPC. It implements the
// public GameService and the privileged AdminService.
//
// Handlers never mutate the world: commands go through sim.Engine.Submit
// (a queue drained by the simulation goroutine) and reads come from the Hub,
// which is fed frames by the simulation goroutine.
//
// The server hosts one world at a time: the current match. A reset
// (AdminService.ResetMatch) builds a complete new game (world, plugins,
// engine, hub, replay recorder) and swaps it in; the previous match is
// archived (result and replay). Player sessions are bound to a player slot,
// not to a match, so they survive resets and resolve to the slot's player in
// whichever match is current.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/replay"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// maxCommandIDLen bounds client-chosen command IDs.
const maxCommandIDLen = 128

// ackHistory is how many acknowledgments are remembered per player for
// idempotent resubmission.
const ackHistory = 4096

// historySize is how many previous matches (results and replays) are kept.
const historySize = 16

// ReplayHashInterval is the number of ticks between full state hashes in
// recorded replays.
var ReplayHashInterval uint64 = 100

// Server holds the state shared by both services.
type Server struct {
	cfg *game.Config // the server's base configuration: players and admin token

	mu       sync.Mutex
	cur      *matchRun
	sessions map[string]int // session token -> player slot
	conns    map[int]int    // player slot -> open Observe streams
	history  []*matchRun    // most recent first
	stopRun  context.CancelFunc
	running  bool

	adminSeq atomic.Uint64
}

// matchRun is one match: a game and everything serving it.
type matchRun struct {
	id      string
	cfg     *game.Config
	game    *game.Game
	engine  *sim.Engine
	hub     *Hub
	info    *rtsv1.WorldInfo
	players []game.Player
	rec     *replay.Recorder
	static  *matchStatic

	acks     map[sim.PlayerID]*ackLog // guarded by Server.mu
	result   *rtsv1.MatchResult       // guarded by Server.mu; set when finished
	finished bool                     // simulation goroutine only
}

type ackLog struct {
	byID  map[string]*rtsv1.SubmitCommandResponse
	order []string
}

// New wires a server to a game, which becomes the first match. It installs
// the frame sink, so call it before the engine starts.
func New(cfg *game.Config, g *game.Game) *Server {
	s := &Server{cfg: cfg, sessions: map[string]int{}, conns: map[int]int{}}
	m, err := s.newMatch(cfg, g)
	if err != nil {
		panic(err) // only fails if the configuration cannot be serialized
	}
	s.cur = m
	return s
}

func newMatchID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// newMatch prepares a built game for serving: hub, world info, recorder and
// the frame sink.
func (s *Server) newMatch(cfg *game.Config, g *game.Game) (*matchRun, error) {
	engine := g.Engine
	m := &matchRun{id: newMatchID(), cfg: cfg, game: g, engine: engine, players: g.Players, acks: map[sim.PlayerID]*ackLog{}}
	rec, err := replay.NewRecorder(g, m.id, ReplayHashInterval)
	if err != nil {
		return nil, err
	}
	m.rec = rec
	m.static = newMatchStatic(m)
	m.hub = NewHub(engine.NextTick()-1, engine.World().FullView(), engine.Rules().HasObservationAuthority())
	m.hub.setMatch(m.static, match.Get(engine.World()))
	engine.SetFrameSink(func(f *sim.Frame) {
		final := false
		if v, ok := f.Globals[match.GlobalID]; ok && !m.finished {
			final = v.(match.Status).State == match.Finished
		}
		m.rec.Record(f, engine.World(), final)
		m.hub.Publish(f)
		if final {
			m.finished = true
			res := resultFrom(m.static, f.Globals[match.GlobalID].(match.Status))
			if b, err := protojson.Marshal(res); err == nil {
				m.rec.SetResult(json.RawMessage(b))
			}
			s.mu.Lock()
			m.result = res
			s.mu.Unlock()
		}
	})
	m.info = &rtsv1.WorldInfo{
		Width:             cfg.World.Width,
		Height:            cfg.World.Height,
		TickRateHz:        cfg.World.TickRateHz,
		CommandTypes:      engine.Rules().CommandTypes(false),
		Plugins:           engine.Rules().Plugins(),
		Schedule:          engine.Rules().Schedule(),
		UnitCommandBudget: uint32(cfg.World.UnitCommandBudget),
	}
	if rules, err := json.Marshal(engine.Rules().Rules()); err == nil {
		m.info.RulesJson = string(rules)
	}
	return m, nil
}

// current returns the current match.
func (s *Server) current() *matchRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Hub returns the current match's observation hub.
func (s *Server) Hub() *Hub { return s.current().hub }

// Engine returns the current match's engine.
func (s *Server) Engine() *sim.Engine { return s.current().engine }

// MatchID returns the current match's ID.
func (s *Server) MatchID() string { return s.current().id }

// Run drives the current match's engine at its tick rate until ctx ends,
// switching to the new engine whenever the match is reset.
func (s *Server) Run(ctx context.Context) error {
	for {
		s.mu.Lock()
		m := s.cur
		runCtx, cancel := context.WithCancel(ctx)
		s.stopRun, s.running = cancel, true
		s.mu.Unlock()
		err := m.engine.Run(runCtx, m.cfg.TickInterval())
		cancel()
		if ctx.Err() != nil {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
}

// Reset builds a new match from cfg (the current configuration when nil)
// and replaces the current one. If seed is non-nil it overrides the seed.
// On error nothing changes. Players and the admin token always come from
// the server's base configuration.
func (s *Server) Reset(cfg *game.Config, seed *uint64) (*matchRun, error) {
	if cfg == nil {
		c := *s.current().cfg
		cfg = &c
	}
	cfg.Players, cfg.AdminToken = s.cfg.Players, s.cfg.AdminToken
	if seed != nil {
		cfg.World.Seed, cfg.World.RandomSeed = *seed, false
	}
	g, err := game.New(cfg)
	if err != nil {
		return nil, err
	}
	m, err := s.newMatch(cfg, g)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	old := s.cur
	s.cur = m
	s.history = append([]*matchRun{old}, s.history...)
	if len(s.history) > historySize {
		s.history = s.history[:historySize]
	}
	stop := s.stopRun
	if s.running && stop != nil {
		stop() // the run loop moves on to the new engine
	}
	s.mu.Unlock()
	old.hub.Close() // observers resubscribe to the new match
	return m, nil
}

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

// slotFromContext authenticates a player session.
func (s *Server) slotFromContext(ctx context.Context) (int, error) {
	tok := bearer(ctx)
	s.mu.Lock()
	slot, ok := s.sessions[tok]
	s.mu.Unlock()
	if tok == "" || !ok {
		return 0, status.Error(codes.Unauthenticated, "missing or invalid session token")
	}
	return slot, nil
}

// playerFromContext authenticates a player session and resolves it to the
// slot's player in the current match.
func (s *Server) playerFromContext(ctx context.Context) (sim.PlayerID, *matchRun, error) {
	slot, err := s.slotFromContext(ctx)
	if err != nil {
		return 0, nil, err
	}
	m := s.current()
	return m.player(slot), m, nil
}

func (m *matchRun) player(slot int) sim.PlayerID {
	for _, p := range m.players {
		if p.Slot == slot {
			return p.ID
		}
	}
	return 0
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
	m := g.s.current()
	for _, p := range m.players {
		if subtle.ConstantTimeCompare([]byte(req.GetJoinToken()), []byte(p.JoinToken)) == 1 {
			tok := newToken()
			g.s.mu.Lock()
			g.s.sessions[tok] = p.Slot
			g.s.mu.Unlock()
			return &rtsv1.ConnectResponse{PlayerId: uint32(p.ID), PlayerName: p.Name, SessionToken: tok, World: m.info,
				Match: m.hub.MatchInfo(viewer{player: p.ID})}, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "unknown join token")
}

func (g *gameService) SubmitCommand(ctx context.Context, req *rtsv1.SubmitCommandRequest) (*rtsv1.SubmitCommandResponse, error) {
	player, m, err := g.s.playerFromContext(ctx)
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

	// Hold the lock across dedupe check and submit so concurrent retries of
	// one command id cannot both be scheduled. Acknowledgments are per
	// match: a reset starts a fresh log.
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	log := m.acks[player]
	if log == nil {
		log = &ackLog{byID: make(map[string]*rtsv1.SubmitCommandResponse)}
		m.acks[player] = log
	}
	if prev, ok := log.byID[id]; ok {
		return prev, nil
	}

	typ, msg, err := decodeCommand(req.GetPayload())
	if err != nil {
		return reject(err.Error()), nil
	}
	tick, err := m.engine.Submit(sim.Command{ID: id, Issuer: sim.Principal{Player: player}, Type: typ, Payload: msg})
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
	player, m, err := g.s.playerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	return &rtsv1.GetSnapshotResponse{Snapshot: m.hub.Snapshot(viewer{player: player})}, nil
}

func (g *gameService) GetMatch(ctx context.Context, _ *rtsv1.GetMatchRequest) (*rtsv1.GetMatchResponse, error) {
	player, m, err := g.s.playerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	return &rtsv1.GetMatchResponse{Match: m.hub.MatchInfo(viewer{player: player}), History: g.s.results()}, nil
}

// results returns finished matches' results, most recent first.
func (s *Server) results() []*rtsv1.MatchResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*rtsv1.MatchResult
	for _, m := range s.history {
		if m.result != nil {
			out = append(out, m.result)
		}
	}
	return out
}

func (g *gameService) Observe(_ *rtsv1.ObserveRequest, stream grpc.ServerStreamingServer[rtsv1.ObserveResponse]) error {
	slot, err := g.s.slotFromContext(stream.Context())
	if err != nil {
		return err
	}
	g.s.mu.Lock()
	g.s.conns[slot]++
	g.s.mu.Unlock()
	defer func() {
		g.s.mu.Lock()
		g.s.conns[slot]--
		g.s.mu.Unlock()
	}()
	return g.s.observe(stream.Context(), func(m *matchRun) viewer { return viewer{player: m.player(slot)} },
		func(snap *rtsv1.WorldSnapshot) error {
			return stream.Send(&rtsv1.ObserveResponse{Kind: &rtsv1.ObserveResponse_Snapshot{Snapshot: snap}})
		},
		func(u *rtsv1.TickUpdate) error {
			return stream.Send(&rtsv1.ObserveResponse{Kind: &rtsv1.ObserveResponse_Update{Update: u}})
		})
}

// observe streams a snapshot followed by per-tick updates, resynchronizing
// with a new snapshot whenever the hub drops this observer (lagging, or the
// match was reset; the viewer is then resolved again for the new match).
func (s *Server) observe(ctx context.Context, viewerFor func(*matchRun) viewer,
	sendSnapshot func(*rtsv1.WorldSnapshot) error, sendUpdate func(*rtsv1.TickUpdate) error) error {
	for {
		m := s.current()
		v := viewerFor(m)
		snap, sub := m.hub.Subscribe(v)
		if err := sendSnapshot(snap); err != nil {
			m.hub.Unsubscribe(sub)
			return err
		}
		lagged, err := func() (bool, error) {
			defer m.hub.Unsubscribe(sub)
			for {
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case f, ok := <-sub.ch:
					if !ok {
						return true, nil
					}
					if err := sendUpdate(v.update(f)); err != nil {
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

// protoUnmarshalOptions resolves Any payloads against the global registry,
// which contains every compiled-in plugin's command messages.
var protoUnmarshalOptions = proto.UnmarshalOptions{}

// decodeCommand unpacks an Any payload into a command type and message.
func decodeCommand(payload *anypb.Any) (string, proto.Message, error) {
	if payload == nil {
		return "", nil, errors.New("payload missing")
	}
	msg, err := anypb.UnmarshalNew(payload, protoUnmarshalOptions)
	if err != nil {
		return "", nil, fmt.Errorf("cannot decode payload %q: %v", payload.GetTypeUrl(), err)
	}
	return string(msg.ProtoReflect().Descriptor().FullName()), msg, nil
}
