package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/replay"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// adminService implements AdminService. Every handler authenticates with
// requireAdmin first; every mutation is submitted to the engine as an
// admin-issued command, so it executes on the simulation goroutine in the
// deterministic command order and is part of the replay log.
type adminService struct {
	rtsv1.UnimplementedAdminServiceServer
	s *Server
}

var adminViewer = viewer{admin: true}

func (s *Server) status() *rtsv1.SimulationStatus {
	m := s.current()
	return &rtsv1.SimulationStatus{
		Tick:       uint64(m.hub.Tick()),
		Paused:     m.engine.Paused(),
		TickRateHz: m.cfg.World.TickRateHz,
		Match:      m.hub.MatchInfo(adminViewer),
	}
}

// submitAdmin schedules an admin command with a server-generated ID.
func (s *Server) submitAdmin(typ string, payload any) (string, sim.Tick, error) {
	id := fmt.Sprintf("admin-%d", s.adminSeq.Add(1))
	tick, err := s.current().engine.Submit(sim.Command{ID: id, Issuer: sim.Principal{Admin: true}, Type: typ, Payload: payload})
	return id, tick, err
}

func (a *adminService) GetWorldSnapshot(ctx context.Context, _ *rtsv1.GetWorldSnapshotRequest) (*rtsv1.GetWorldSnapshotResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	m := a.s.current()
	resp := &rtsv1.GetWorldSnapshotResponse{Snapshot: m.hub.Snapshot(adminViewer), Status: a.s.status(), World: m.info}
	for _, p := range m.players {
		resp.Players = append(resp.Players, &rtsv1.PlayerInfo{Id: uint32(p.ID), Name: p.Name})
	}
	return resp, nil
}

func (a *adminService) ObserveWorld(_ *rtsv1.ObserveWorldRequest, stream grpc.ServerStreamingServer[rtsv1.ObserveWorldResponse]) error {
	if err := a.s.requireAdmin(stream.Context()); err != nil {
		return err
	}
	return a.s.observe(stream.Context(), func(*matchRun) viewer { return adminViewer },
		func(snap *rtsv1.WorldSnapshot) error {
			return stream.Send(&rtsv1.ObserveWorldResponse{Kind: &rtsv1.ObserveWorldResponse_Snapshot{Snapshot: snap}})
		},
		func(u *rtsv1.TickUpdate) error {
			return stream.Send(&rtsv1.ObserveWorldResponse{Kind: &rtsv1.ObserveWorldResponse_Update{Update: u}})
		})
}

func (a *adminService) GetStatus(ctx context.Context, _ *rtsv1.GetStatusRequest) (*rtsv1.GetStatusResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	return &rtsv1.GetStatusResponse{Status: a.s.status()}, nil
}

func (a *adminService) SpawnUnit(ctx context.Context, req *rtsv1.SpawnUnitRequest) (*rtsv1.SpawnUnitResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	owner := sim.PlayerID(req.GetOwnerId())
	known := owner == 0
	for _, p := range a.s.current().players {
		known = known || p.ID == owner
	}
	if !known {
		return nil, status.Errorf(codes.InvalidArgument, "unknown owner %d", owner)
	}
	if req.GetPosition() == nil {
		return nil, status.Error(codes.InvalidArgument, "position required")
	}
	id, tick, err := a.s.submitAdmin(sim.CmdCreateEntity,
		sim.CreateEntity{Kind: sim.KindUnit, Owner: owner, Pos: sim.Pos{X: req.GetPosition().GetX(), Y: req.GetPosition().GetY()}})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &rtsv1.SpawnUnitResponse{CommandId: id, ScheduledTick: uint64(tick)}, nil
}

func (a *adminService) DeleteEntity(ctx context.Context, req *rtsv1.DeleteEntityRequest) (*rtsv1.DeleteEntityResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.GetEntityId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "entity_id required")
	}
	id, tick, err := a.s.submitAdmin(sim.CmdRemoveEntity, sim.RemoveEntity{ID: sim.EntityID(req.GetEntityId())})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &rtsv1.DeleteEntityResponse{CommandId: id, ScheduledTick: uint64(tick)}, nil
}

func (a *adminService) MoveEntity(ctx context.Context, req *rtsv1.MoveEntityRequest) (*rtsv1.MoveEntityResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.GetEntityId() == 0 || req.GetPosition() == nil {
		return nil, status.Error(codes.InvalidArgument, "entity_id and position required")
	}
	id, tick, err := a.s.submitAdmin(sim.CmdMoveEntity, sim.MoveEntity{
		ID: sim.EntityID(req.GetEntityId()),
		To: sim.Pos{X: req.GetPosition().GetX(), Y: req.GetPosition().GetY()},
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &rtsv1.MoveEntityResponse{CommandId: id, ScheduledTick: uint64(tick)}, nil
}

func (a *adminService) SubmitAdminCommand(ctx context.Context, req *rtsv1.SubmitAdminCommandRequest) (*rtsv1.SubmitAdminCommandResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	typ, msg, err := decodeCommand(req.GetPayload())
	if err != nil {
		return &rtsv1.SubmitAdminCommandResponse{RejectionReason: err.Error()}, nil
	}
	id, tick, err := a.s.submitAdmin(typ, msg)
	if err != nil {
		return &rtsv1.SubmitAdminCommandResponse{CommandId: id, RejectionReason: err.Error()}, nil
	}
	return &rtsv1.SubmitAdminCommandResponse{CommandId: id, Accepted: true, ScheduledTick: uint64(tick)}, nil
}

func (a *adminService) SetPaused(ctx context.Context, req *rtsv1.SetPausedRequest) (*rtsv1.SetPausedResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	m := a.s.current()
	m.engine.SetPaused(req.GetPaused())
	return &rtsv1.SetPausedResponse{Paused: req.GetPaused(), Tick: uint64(m.hub.Tick())}, nil
}

func (a *adminService) Step(ctx context.Context, req *rtsv1.StepRequest) (*rtsv1.StepResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	n := max(int(req.GetCount()), 1)
	first, err := a.s.current().engine.RequestSteps(n)
	switch {
	case errors.Is(err, sim.ErrNotPaused):
		return nil, status.Error(codes.FailedPrecondition, "pause the simulation before stepping")
	case errors.Is(err, sim.ErrStepBusy):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &rtsv1.StepResponse{FirstTick: uint64(first), Count: uint32(n)}, nil
}

func (a *adminService) GetPlayerView(ctx context.Context, req *rtsv1.GetPlayerViewRequest) (*rtsv1.GetPlayerViewResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	p := sim.PlayerID(req.GetPlayerId())
	m := a.s.current()
	known := false
	for _, pl := range m.players {
		known = known || pl.ID == p
	}
	if !known {
		return nil, status.Errorf(codes.InvalidArgument, "unknown player %d", p)
	}
	snap, dets := m.hub.PlayerView(p)
	return &rtsv1.GetPlayerViewResponse{Snapshot: snap, Detections: dets}, nil
}

// ---- match administration ---------------------------------------------------------

// redactedConfig returns a configuration as JSON without secrets.
func redactedConfig(c *game.Config) string {
	cp := *c
	cp.AdminToken = ""
	cp.Players = append([]game.PlayerConfig(nil), c.Players...)
	for i := range cp.Players {
		cp.Players[i].JoinToken = ""
	}
	b, _ := json.MarshalIndent(cp, "", "  ")
	return string(b)
}

func (a *adminService) GetMatchDetails(ctx context.Context, _ *rtsv1.GetMatchDetailsRequest) (*rtsv1.GetMatchDetailsResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	m := a.s.current()
	st := m.hub.Status()
	resp := &rtsv1.GetMatchDetailsResponse{Match: m.hub.MatchInfo(adminViewer), ConfigJson: redactedConfig(m.cfg), Seed: m.game.Seed, History: a.s.results()}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	for _, p := range m.players {
		ps := &rtsv1.AdminPlayerStatus{PlayerId: uint32(p.ID), Name: p.Name, Slot: int32(p.Slot), Connections: uint32(max(a.s.conns[p.Slot], 0))}
		if i := st.Index(p.ID); i >= 0 {
			ps.Ready, ps.Eliminated = st.Ready[i], st.Eliminated[i]
			ps.Stats = playerResult(m.static, st, i, p.ID)
		}
		resp.Players = append(resp.Players, ps)
	}
	return resp, nil
}

func (a *adminService) ResetMatch(ctx context.Context, req *rtsv1.ResetMatchRequest) (*rtsv1.ResetMatchResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	var cfg *game.Config
	if req.GetConfigJson() != "" {
		dec := json.NewDecoder(strings.NewReader(req.GetConfigJson()))
		dec.DisallowUnknownFields()
		cfg = &game.Config{}
		if err := dec.Decode(cfg); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "config: %v", err)
		}
	}
	m, err := a.s.Reset(cfg, req.Seed)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "new match not started (the current one continues): %v", err)
	}
	return &rtsv1.ResetMatchResponse{MatchId: m.id, Seed: m.game.Seed}, nil
}

func (a *adminService) ExportReplay(ctx context.Context, req *rtsv1.ExportReplayRequest) (*rtsv1.ExportReplayResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	m := a.s.find(req.GetMatchId())
	if m == nil {
		return nil, status.Errorf(codes.NotFound, "no match %q in this server's recent history", req.GetMatchId())
	}
	rp, err := m.rec.Snapshot()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "replay incomplete: %v", err)
	}
	b, err := json.Marshal(rp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &rtsv1.ExportReplayResponse{ReplayJson: string(b)}, nil
}

// find returns the current match (empty id) or a recent one.
func (s *Server) find(id string) *matchRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || id == s.cur.id {
		return s.cur
	}
	for _, m := range s.history {
		if m.id == id {
			return m
		}
	}
	return nil
}

func (a *adminService) VerifyReplay(ctx context.Context, req *rtsv1.VerifyReplayRequest) (*rtsv1.VerifyReplayResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rp, err := replay.Parse([]byte(req.GetReplayJson()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	rep, err := replay.Verify(rp) // an isolated engine; the live match is untouched
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &rtsv1.VerifyReplayResponse{Ok: rep.OK, TicksReplayed: rep.TicksReplayed, EngineMatches: rep.EngineMatches,
		RulesetMatches: rep.RulesetMatches, FirstDivergentTick: rep.FirstDivergentTick, Detail: rep.Detail,
		FinalHash: rep.FinalHash, CheckpointsChecked: uint32(rep.CheckpointsChecked)}, nil
}
