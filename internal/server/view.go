package server

import (
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// viewer is who is looking. It decides which attributes, events and command
// results are disclosed. Milestone 1 filters by ownership only; fog of war
// will extend this to per-entity visibility.
type viewer struct {
	player sim.PlayerID
	admin  bool
}

func (v viewer) canSee(a sim.Attr, owner sim.PlayerID) bool {
	switch {
	case v.admin, a.Visibility == sim.VisiblePublic:
		return true
	case a.Visibility == sim.VisibleOwner:
		return owner != 0 && owner == v.player
	}
	return false
}

func (v viewer) entity(e sim.EntityView) *rtsv1.Entity {
	out := &rtsv1.Entity{
		Id:       uint64(e.ID),
		Kind:     kindToProto(e.Kind),
		OwnerId:  uint32(e.Owner),
		Position: posToProto(e.Pos),
	}
	for _, a := range e.Attrs {
		if !v.canSee(a, e.Owner) {
			continue
		}
		if out.Attributes == nil {
			out.Attributes = make(map[string]*rtsv1.Value)
		}
		out.Attributes[a.Key] = valueToProto(a.Value)
	}
	return out
}

func (v viewer) entities(es []sim.EntityView) []*rtsv1.Entity {
	out := make([]*rtsv1.Entity, len(es))
	for i, e := range es {
		out[i] = v.entity(e)
	}
	return out
}

func (v viewer) snapshot(tick sim.Tick, es []sim.EntityView) *rtsv1.WorldSnapshot {
	return &rtsv1.WorldSnapshot{Tick: uint64(tick), Entities: v.entities(es)}
}

func (v viewer) update(f *sim.Frame) *rtsv1.TickUpdate {
	u := &rtsv1.TickUpdate{Tick: uint64(f.Tick), Changed: v.entities(f.Changed)}
	for _, id := range f.Removed {
		u.Removed = append(u.Removed, uint64(id))
	}
	for _, ev := range f.Events {
		if !v.admin && !ev.Audience.All && ev.Audience.Player != v.player {
			continue
		}
		pe := &rtsv1.Event{Type: ev.Type, EntityId: uint64(ev.Entity)}
		if len(ev.Fields) > 0 {
			pe.Fields = make(map[string]*rtsv1.Value, len(ev.Fields))
			for _, fld := range ev.Fields {
				pe.Fields[fld.Key] = valueToProto(fld.Value)
			}
		}
		u.Events = append(u.Events, pe)
	}
	for _, r := range f.Results {
		if r.Issuer.Admin != v.admin || (!v.admin && r.Issuer.Player != v.player) {
			continue
		}
		pr := &rtsv1.CommandResult{CommandId: r.CommandID, Tick: uint64(r.Tick), Status: rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED}
		if !r.OK {
			pr.Status = rtsv1.CommandStatus_COMMAND_STATUS_FAILED
			pr.ErrorCode, pr.ErrorMessage = r.Code, r.Message
		}
		u.CommandResults = append(u.CommandResults, pr)
	}
	return u
}

func posToProto(p sim.Pos) *rtsv1.Position { return &rtsv1.Position{X: p.X, Y: p.Y} }

func kindToProto(k sim.Kind) rtsv1.EntityKind {
	switch k {
	case sim.KindUnit:
		return rtsv1.EntityKind_ENTITY_KIND_UNIT
	case sim.KindResource:
		return rtsv1.EntityKind_ENTITY_KIND_RESOURCE
	case sim.KindTerrain:
		return rtsv1.EntityKind_ENTITY_KIND_TERRAIN
	}
	return rtsv1.EntityKind_ENTITY_KIND_UNSPECIFIED
}

func valueToProto(v sim.Value) *rtsv1.Value {
	switch v.Kind {
	case sim.ValueInt:
		return &rtsv1.Value{Kind: &rtsv1.Value_IntValue{IntValue: v.Int}}
	case sim.ValueBool:
		return &rtsv1.Value{Kind: &rtsv1.Value_BoolValue{BoolValue: v.Bool}}
	case sim.ValueString:
		return &rtsv1.Value{Kind: &rtsv1.Value_StringValue{StringValue: v.Str}}
	case sim.ValuePos:
		return &rtsv1.Value{Kind: &rtsv1.Value_PositionValue{PositionValue: posToProto(v.Pos)}}
	}
	return &rtsv1.Value{}
}
