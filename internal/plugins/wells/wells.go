// Package wells is the energy-well plugin: resource entities that hold
// energy and release it at a limited rate. It owns the well data model and
// its administration (creation, configuration, initial placement). How
// units extract energy is the energy plugin's business; it uses the
// exported Get and Set functions.
//
// All quantities are int64 energy units.
package wells

import (
	"errors"
	"fmt"

	wellsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/wells/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "wells"

// Attribute keys (public: every player can see wells).
const (
	AttrEnergy         = "wells.energy"
	AttrCapacity       = "wells.capacity"
	AttrExtractionRate = "wells.extraction_rate"
	AttrUnlimited      = "wells.unlimited"
)

// Command types (admin only).
var (
	CommandCreate    = string((&wellsv1.CreateWellCommand{}).ProtoReflect().Descriptor().FullName())
	CommandConfigure = string((&wellsv1.ConfigureWellCommand{}).ProtoReflect().Descriptor().FullName())
)

// Well is the state of an energy well.
type Well struct {
	Energy   int64 `json:"energy"`
	Capacity int64 `json:"capacity"`
	// ExtractionRate is the most energy the well releases per tick, shared
	// by all units mining it.
	ExtractionRate int64 `json:"extraction_rate"`
	// Unlimited wells never deplete; Energy is not decremented.
	Unlimited bool `json:"unlimited"`
}

// Validate checks the well invariants.
func (w Well) Validate() error {
	switch {
	case w.Capacity < 0:
		return fmt.Errorf("capacity must be >= 0, got %d", w.Capacity)
	case w.Energy < 0 || w.Energy > w.Capacity:
		return fmt.Errorf("energy must be in 0..capacity (%d), got %d", w.Capacity, w.Energy)
	case w.ExtractionRate < 0:
		return fmt.Errorf("extraction_rate must be >= 0, got %d", w.ExtractionRate)
	}
	return nil
}

var component = sim.NewComponent[Well]("wells.well")

// Get returns a well's state; ok is false if id is not a well.
func Get(w *sim.World, id sim.EntityID) (Well, bool) { return component.Get(w, id) }

// Set stores a well's state. id must be an existing well.
func Set(w *sim.World, id sim.EntityID, v Well) { component.Set(w, id, v) }

// IDs returns all wells in ascending ID order.
func IDs(w *sim.World) []sim.EntityID { return component.Entities(w) }

// WellSpec is the configuration of a well; nil fields use the defaults.
type WellSpec struct {
	Energy         *int64 `json:"energy,omitempty"`
	Capacity       *int64 `json:"capacity,omitempty"`
	ExtractionRate *int64 `json:"extraction_rate,omitempty"`
	Unlimited      *bool  `json:"unlimited,omitempty"`
}

// PlacedWell is an initial well from configuration.
type PlacedWell struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	WellSpec
}

// Config is the plugin's configuration schema (the "wells" section).
type Config struct {
	// Defaults for wells created without explicit parameters.
	Defaults Well `json:"defaults"`
	// Wells placed at world creation.
	Wells []PlacedWell `json:"wells"`
}

// DefaultWell is the built-in default for Config.Defaults.
func DefaultWell() Well {
	return Well{Energy: 1000, Capacity: 1000, ExtractionRate: 5}
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg Config
}

// New returns the wells plugin.
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return nil }

func (p *Plugin) apply(base Well, s WellSpec) Well {
	if s.Capacity != nil {
		base.Capacity = *s.Capacity
		if s.Energy == nil {
			base.Energy = min(base.Energy, base.Capacity)
		}
	}
	if s.Energy != nil {
		base.Energy = *s.Energy
	}
	if s.ExtractionRate != nil {
		base.ExtractionRate = *s.ExtractionRate
	}
	if s.Unlimited != nil {
		base.Unlimited = *s.Unlimited
	}
	return base
}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{Defaults: DefaultWell()}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if err := p.cfg.Defaults.Validate(); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	for i, pw := range p.cfg.Wells {
		if err := p.apply(p.cfg.Defaults, pw.WellSpec).Validate(); err != nil {
			return fmt.Errorf("wells[%d]: %w", i, err)
		}
	}

	if err := r.Describe(p.cfg); err != nil {
		return err
	}
	if err := r.Component(component.Def(sim.VisiblePublic, func(v Well) []sim.Field {
		return []sim.Field{
			{Key: AttrEnergy, Value: sim.IntValue(v.Energy)},
			{Key: AttrCapacity, Value: sim.IntValue(v.Capacity)},
			{Key: AttrExtractionRate, Value: sim.IntValue(v.ExtractionRate)},
			{Key: AttrUnlimited, Value: sim.BoolValue(v.Unlimited)},
		}
	})); err != nil {
		return err
	}
	r.Init(func(w *sim.World) error {
		for i, pw := range p.cfg.Wells {
			if _, err := p.create(w, sim.Pos{X: pw.X, Y: pw.Y}, p.apply(p.cfg.Defaults, pw.WellSpec)); err != nil {
				return fmt.Errorf("wells[%d]: %w", i, err)
			}
		}
		return nil
	})
	if err := r.Command(sim.CommandDef{Type: CommandCreate, AdminOnly: true, Execute: p.execCreate}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandConfigure, AdminOnly: true, Execute: p.execConfigure})
}

func (p *Plugin) create(w *sim.World, pos sim.Pos, v Well) (sim.EntityID, error) {
	if err := v.Validate(); err != nil {
		return 0, err
	}
	id, err := w.CreateEntity(sim.KindResource, 0, pos)
	if err != nil {
		return 0, err
	}
	component.Set(w, id, v)
	return id, nil
}

func specFrom(energy, capacity, rate *int64, unlimited *bool) WellSpec {
	return WellSpec{Energy: energy, Capacity: capacity, ExtractionRate: rate, Unlimited: unlimited}
}

func (p *Plugin) execCreate(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*wellsv1.CreateWellCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CreateWellCommand", cmd.Payload)
	}
	if m.GetPosition() == nil {
		return sim.Fail(sim.CodeInvalidArgument, "position required")
	}
	pos := sim.Pos{X: m.GetPosition().GetX(), Y: m.GetPosition().GetY()}
	v := p.apply(p.cfg.Defaults, specFrom(m.Energy, m.Capacity, m.ExtractionRate, m.Unlimited))
	if err := v.Validate(); err != nil {
		return sim.Fail(sim.CodeInvalidArgument, "%v", err)
	}
	id, err := p.create(ctx.World, pos, v)
	if err != nil {
		code := sim.CodeFailedPrecondition
		if errors.Is(err, sim.ErrOutOfBounds) {
			code = sim.CodeInvalidArgument
		}
		return sim.Fail(code, "%v", err)
	}
	ctx.Emit(sim.Event{Type: sim.EventEntityCreated, Entity: id, Audience: sim.AdminAudience(), Fields: []sim.Field{
		{Key: "command_id", Value: sim.StringValue(cmd.ID)},
		{Key: "position", Value: sim.PosValue(pos)},
	}})
	return nil
}

func (p *Plugin) execConfigure(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*wellsv1.ConfigureWellCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want ConfigureWellCommand", cmd.Payload)
	}
	id := sim.EntityID(m.GetWellId())
	cur, ok := component.Get(ctx.World, id)
	if !ok {
		return sim.Fail(sim.CodeNotFound, "well %d not found", m.GetWellId())
	}
	v := p.apply(cur, specFrom(m.Energy, m.Capacity, m.ExtractionRate, m.Unlimited))
	if err := v.Validate(); err != nil {
		return sim.Fail(sim.CodeInvalidArgument, "%v", err)
	}
	component.Set(ctx.World, id, v)
	return nil
}
