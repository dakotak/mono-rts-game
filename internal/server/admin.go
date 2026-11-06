package server

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
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
	return &rtsv1.SimulationStatus{
		Tick:       uint64(s.hub.Tick()),
		Paused:     s.engine.Paused(),
		TickRateHz: s.cfg.World.TickRateHz,
	}
}

// submitAdmin schedules an admin command with a server-generated ID.
func (s *Server) submitAdmin(typ string, payload any) (string, sim.Tick, error) {
	id := fmt.Sprintf("admin-%d", s.adminSeq.Add(1))
	tick, err := s.engine.Submit(sim.Command{ID: id, Issuer: sim.Principal{Admin: true}, Type: typ, Payload: payload})
	return id, tick, err
}

func (a *adminService) GetWorldSnapshot(ctx context.Context, _ *rtsv1.GetWorldSnapshotRequest) (*rtsv1.GetWorldSnapshotResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	tick, es := a.s.hub.Snapshot()
	resp := &rtsv1.GetWorldSnapshotResponse{Snapshot: adminViewer.snapshot(tick, es), Status: a.s.status(), World: a.s.info}
	for _, p := range a.s.players {
		resp.Players = append(resp.Players, &rtsv1.PlayerInfo{Id: uint32(p.ID), Name: p.Name})
	}
	return resp, nil
}

func (a *adminService) ObserveWorld(_ *rtsv1.ObserveWorldRequest, stream grpc.ServerStreamingServer[rtsv1.ObserveWorldResponse]) error {
	if err := a.s.requireAdmin(stream.Context()); err != nil {
		return err
	}
	return a.s.observe(stream.Context(), adminViewer,
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
	for _, p := range a.s.players {
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
	a.s.engine.SetPaused(req.GetPaused())
	return &rtsv1.SetPausedResponse{Paused: req.GetPaused(), Tick: uint64(a.s.hub.Tick())}, nil
}

func (a *adminService) Step(ctx context.Context, req *rtsv1.StepRequest) (*rtsv1.StepResponse, error) {
	if err := a.s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	n := max(int(req.GetCount()), 1)
	first, err := a.s.engine.RequestSteps(n)
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
