package server

import (
	"cmp"
	"slices"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// viewer is who is looking. It decides which entities, attributes, events
// and command results are disclosed.
//
// Without fog of war, every entity is visible and attributes are filtered
// by their visibility (owner / public). With fog of war, a player sees
// their own entities in full and foreign entities only through the
// observation authority's already-filtered views; the global entity list
// is never consulted for foreign entities. Admins always see everything.
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

// entity converts a full view, filtering attributes by visibility. Under
// fog it is only used for the viewer's own entities.
func (v viewer) entity(e sim.EntityView, fog bool) *rtsv1.Entity {
	out := &rtsv1.Entity{
		Id:       uint64(e.ID),
		Kind:     kindToProto(e.Kind),
		OwnerId:  uint32(e.Owner),
		Position: posToProto(e.Pos),
		Passable: e.Passable,
	}
	if fog {
		out.Identification = rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_OWN
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

// observed converts a foreign entity as disclosed by the observation
// authority. Its attributes are already filtered: all are sent.
func (v viewer) observed(oe sim.ObservedEntity) *rtsv1.Entity {
	e := oe.View
	out := &rtsv1.Entity{
		Id:             uint64(e.ID),
		Kind:           kindToProto(e.Kind),
		OwnerId:        uint32(e.Owner),
		Position:       posToProto(e.Pos),
		Passable:       e.Passable,
		Identification: levelToProto(oe.Level),
	}
	for _, a := range e.Attrs {
		if out.Attributes == nil {
			out.Attributes = make(map[string]*rtsv1.Value, len(e.Attrs))
		}
		out.Attributes[a.Key] = valueToProto(a.Value)
	}
	return out
}

func observationFor(f *sim.Frame, p sim.PlayerID) sim.Observation {
	i, ok := slices.BinarySearchFunc(f.Observations, p, func(o sim.Observation, p sim.PlayerID) int { return cmp.Compare(o.Player, p) })
	if !ok {
		return sim.Observation{}
	}
	return f.Observations[i]
}

func (v viewer) update(pub *published) *rtsv1.TickUpdate {
	f := pub.frame
	u := &rtsv1.TickUpdate{Tick: uint64(f.Tick)}
	if f.ObservationMode && !v.admin {
		// Own entities in full; foreign entities only as observed.
		for _, e := range f.Changed {
			if e.Owner != 0 && e.Owner == v.player {
				u.Changed = append(u.Changed, v.entity(e, true))
			}
		}
		for _, id := range f.Removed {
			if o := pub.removedOwner[id]; o != 0 && o == v.player {
				u.Removed = append(u.Removed, uint64(id))
			}
		}
		obs := observationFor(f, v.player)
		for _, oe := range obs.Upserts {
			u.Changed = append(u.Changed, v.observed(oe))
		}
		for _, id := range obs.Lost {
			u.Removed = append(u.Removed, uint64(id))
		}
		slices.SortFunc(u.Changed, func(a, b *rtsv1.Entity) int { return cmp.Compare(a.GetId(), b.GetId()) })
		slices.Sort(u.Removed)
		for _, t := range obs.TilesEntered {
			u.TilesEntered = append(u.TilesEntered, posToProto(t))
		}
		for _, t := range obs.TilesExited {
			u.TilesExited = append(u.TilesExited, posToProto(t))
		}
	} else {
		for _, e := range f.Changed {
			u.Changed = append(u.Changed, v.entity(e, false))
		}
		for _, id := range f.Removed {
			u.Removed = append(u.Removed, uint64(id))
		}
	}
	if pub.status != nil && pub.static != nil {
		u.Match = matchInfo(pub.static, *pub.status, f.Tick, v)
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
		switch {
		case !r.OK:
			pr.Status = rtsv1.CommandStatus_COMMAND_STATUS_FAILED
			pr.ErrorCode, pr.ErrorMessage = r.Code, r.Message
		case r.Partial:
			pr.Status = rtsv1.CommandStatus_COMMAND_STATUS_PARTIAL
		}
		for _, u := range r.Units {
			pr.UnitResults = append(pr.UnitResults, &rtsv1.UnitResult{UnitId: uint64(u.Unit), Ok: u.OK, ErrorCode: u.Code, ErrorMessage: u.Message})
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
	case sim.ValueRange:
		return &rtsv1.Value{Kind: &rtsv1.Value_RangeValue{RangeValue: &rtsv1.Range{Low: v.Int, High: v.Hi}}}
	}
	return &rtsv1.Value{}
}

func levelToProto(l sim.Level) rtsv1.IdentificationLevel {
	switch l {
	case sim.LevelBasic:
		return rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_BASIC
	case sim.LevelEstimated:
		return rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_ESTIMATED
	case sim.LevelDetailed:
		return rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_DETAILED
	}
	return rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_UNSPECIFIED
}
