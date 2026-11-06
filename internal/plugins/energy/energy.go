// Package energy is the energy-economy plugin: per-unit energy storage,
// persistent mining orders against wells, and persistent energy transfers
// between adjacent friendly units. Other plugins spend energy through the
// exported Stored and Spend functions (movement does).
//
// # Units
//
// Energy is an int64 quantity. Every unit gets a store when it is created
// (initial_energy of unit_capacity). There is no passive upkeep: energy only
// changes through mining, transfers, plugin spending (Spend) and admin
// commands.
//
// # Rules (phase "energy", which runs before movement each tick)
//
// Mining. A unit with a mining order extracts only while it is adjacent
// (Chebyshev distance 1) to its well. Its request is
// min(extraction rate, free capacity). Each well releases at most its
// extraction_rate per tick, and a finite well at most its remaining energy.
// When requests exceed what the well releases, it is split max-min fairly:
// requests are visited from smallest to largest (ties by entity ID) and each
// receives min(request, remaining / miners left). The total never exceeds
// what the well releases and no energy is lost. Finite wells lose exactly
// what units gain; unlimited wells do not deplete. When a finite well
// reaches 0, every order on it ends.
//
// Transfers. Each tick, in ascending sender ID order, a sender moves
// min(transfer rate, amount left, sender energy, receiver free capacity) to
// its receiver. Energy is moved, never created or destroyed. The order ends
// with a reason: completed, sender_empty, receiver_full, not_adjacent or
// target_lost.
package energy

import (
	"fmt"
	"slices"
	"strconv"

	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/fairshare"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "energy"

// Identifiers registered by this plugin.
const (
	Phase = "energy"

	AttrStored            = "energy.stored"
	AttrCapacity          = "energy.capacity"
	AttrMiningWell        = "energy.mining_well"        // decimal entity ID
	AttrTransferTo        = "energy.transfer_to"        // decimal entity ID
	AttrTransferRemaining = "energy.transfer_remaining" // energy still to send

	DerivedExtractionRate = "energy.extraction_rate"
	DerivedTransferRate   = "energy.transfer_rate"

	EventMined         = "energy.mined"          // owner; fields amount, well
	EventMiningStopped = "energy.mining_stopped" // owner; fields reason, well
	EventWellDepleted  = "energy.well_depleted"  // everyone
	EventTransferred   = "energy.transferred"    // owner; entity = sender; fields to, amount
	EventTransferEnded = "energy.transfer_ended" // owner; fields to, reason, remaining
)

// Command types.
var (
	CommandMine       = string((&energyv1.MineCommand{}).ProtoReflect().Descriptor().FullName())
	CommandStopMining = string((&energyv1.StopMiningCommand{}).ProtoReflect().Descriptor().FullName())
	CommandTransfer   = string((&energyv1.TransferCommand{}).ProtoReflect().Descriptor().FullName())
	CommandSetEnergy  = string((&energyv1.SetUnitEnergyCommand{}).ProtoReflect().Descriptor().FullName())
)

// CodeInsufficientEnergy is the failure code for actions a unit cannot pay
// for from its own store.
const CodeInsufficientEnergy = "insufficient_energy"

// MaxUnitsPerCommand bounds the work a single command can request.
const MaxUnitsPerCommand = 4096

// Config is the plugin's configuration schema (the "energy" section).
type Config struct {
	// UnitCapacity is the storage capacity of newly created units.
	UnitCapacity int64 `json:"unit_capacity"`
	// InitialEnergy is the stored energy of newly created units.
	InitialEnergy int64 `json:"initial_energy"`
	// ExtractionRate is how much one unit can mine per tick.
	ExtractionRate int64 `json:"extraction_rate"`
	// TransferRate is how much one unit can send per tick.
	TransferRate int64 `json:"transfer_rate"`
	// CapacityAttribute, if set, names an attribute or derived property
	// (attributes plugin) that determines each unit's storage capacity,
	// replacing UnitCapacity. Capacity follows the property as it changes.
	CapacityAttribute string `json:"capacity_attribute,omitempty"`
	// ExtractionAttribute, if set, names an attribute or derived property
	// that determines each unit's extraction rate, replacing
	// ExtractionRate.
	ExtractionAttribute string `json:"extraction_attribute,omitempty"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{UnitCapacity: 100, InitialEnergy: 50, ExtractionRate: 2, TransferRate: 5}
}

func (c Config) validate() error {
	switch {
	case c.UnitCapacity < 0:
		return fmt.Errorf("unit_capacity must be >= 0, got %d", c.UnitCapacity)
	case c.InitialEnergy < 0 || c.InitialEnergy > c.UnitCapacity:
		return fmt.Errorf("initial_energy must be in 0..unit_capacity (%d), got %d", c.UnitCapacity, c.InitialEnergy)
	case c.ExtractionRate < 0:
		return fmt.Errorf("extraction_rate must be >= 0, got %d", c.ExtractionRate)
	case c.TransferRate < 0:
		return fmt.Errorf("transfer_rate must be >= 0, got %d", c.TransferRate)
	}
	return nil
}

// Store is a unit's energy storage.
type Store struct {
	Energy   int64
	Capacity int64
}

// Free returns the remaining capacity.
func (s Store) Free() int64 { return s.Capacity - s.Energy }

type miningOrder struct {
	Well sim.EntityID
}

type transferOrder struct {
	To        sim.EntityID
	Remaining int64
}

var (
	stores    = sim.NewComponent[Store]("energy.store")
	mining    = sim.NewComponent[miningOrder]("energy.mining")
	transfers = sim.NewComponent[transferOrder]("energy.transfer")
)

// Stored returns a unit's stored energy.
func Stored(w *sim.World, id sim.EntityID) (int64, bool) {
	s, ok := stores.Get(w, id)
	return s.Energy, ok
}

// Spend removes amount from a unit's store if it holds at least that much.
// Spent energy leaves the economy (it is consumed by the action).
func Spend(w *sim.World, id sim.EntityID, amount int64) bool {
	s, ok := stores.Get(w, id)
	if !ok || amount < 0 || s.Energy < amount {
		return false
	}
	s.Energy -= amount
	stores.Set(w, id, s)
	return true
}

// SetStored sets a unit's stored energy, clamped to [0, capacity]. It is
// for plugins that move energy into a new unit they created (cloning), so
// that the unit does not keep the free initial_energy it got on creation.
func SetStored(w *sim.World, id sim.EntityID, energy int64) {
	s, ok := stores.Get(w, id)
	if !ok {
		return
	}
	s.Energy = max(0, min(energy, s.Capacity))
	stores.Set(w, id, s)
}

// GetStore returns a unit's energy store.
func GetStore(w *sim.World, id sim.EntityID) (Store, bool) { return stores.Get(w, id) }

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg   Config
	attrs *attributes.Plugin

	// CanSee, if set, restricts which wells a player may refer to in
	// commands (fog of war). nil means every well.
	CanSee func(w *sim.World, player sim.PlayerID, id sim.EntityID) bool
}

// checkWell validates a well reference. Missing and undetected wells give
// players the same answer.
func (p *Plugin) checkWell(w *sim.World, issuer sim.Principal, well sim.EntityID) error {
	if _, ok := wells.Get(w, well); !ok {
		return sim.Unavailable("well", well)
	}
	if !issuer.Admin && p.CanSee != nil && !p.CanSee(w, issuer.Player, well) {
		return sim.Unavailable("well", well)
	}
	return nil
}

// New returns the energy plugin without attribute bindings.
func New() *Plugin { return &Plugin{} }

// NewWithAttributes returns the energy plugin able to bind capacity and
// extraction rate to attributes (see Config).
func NewWithAttributes(a *attributes.Plugin) *Plugin { return &Plugin{attrs: a} }

func (*Plugin) Name() string { return Name }

func (p *Plugin) DependsOn() []string {
	if p.attrs != nil {
		return []string{wells.Name, attributes.Name}
	}
	return []string{wells.Name}
}

// capacity returns a unit's capacity under the current configuration.
func (p *Plugin) capacity(w *sim.World, id sim.EntityID) int64 {
	if p.cfg.CapacityAttribute != "" {
		v, _ := p.attrs.Lookup(w, id, p.cfg.CapacityAttribute)
		return max(v, 0)
	}
	return p.cfg.UnitCapacity
}

func (p *Plugin) checkBinding(field, name string) error {
	if name == "" {
		return nil
	}
	if p.attrs == nil {
		return fmt.Errorf("%s requires the attributes plugin", field)
	}
	if !p.attrs.Has(name) {
		return fmt.Errorf("%s: %q is not an attribute or derived property", field, name)
	}
	return nil
}

func idString(id sim.EntityID) sim.Value { return sim.StringValue(strconv.FormatUint(uint64(id), 10)) }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if err := p.cfg.validate(); err != nil {
		return err
	}
	if err := p.checkBinding("capacity_attribute", p.cfg.CapacityAttribute); err != nil {
		return err
	}
	if err := p.checkBinding("extraction_attribute", p.cfg.ExtractionAttribute); err != nil {
		return err
	}
	if p.cfg.CapacityAttribute != "" {
		// Keep stored capacity in sync with the attribute. Capacity only
		// shrinks through admin edits; excess energy is then lost.
		p.attrs.OnChange(func(w *sim.World, id sim.EntityID) {
			s, ok := stores.Get(w, id)
			if !ok {
				return
			}
			s.Capacity = p.capacity(w, id)
			s.Energy = min(s.Energy, s.Capacity)
			stores.Set(w, id, s)
		})
	}
	if err := r.Describe(p.cfg); err != nil {
		return err
	}
	// Energy levels and orders are private to the owner.
	if err := r.Component(stores.Def(sim.VisibleOwner, func(s Store) []sim.Field {
		return []sim.Field{
			{Key: AttrStored, Value: sim.IntValue(s.Energy)},
			{Key: AttrCapacity, Value: sim.IntValue(s.Capacity)},
		}
	})); err != nil {
		return err
	}
	if err := r.Component(mining.Def(sim.VisibleOwner, func(o miningOrder) []sim.Field {
		return []sim.Field{{Key: AttrMiningWell, Value: idString(o.Well)}}
	})); err != nil {
		return err
	}
	if err := r.Component(transfers.Def(sim.VisibleOwner, func(o transferOrder) []sim.Field {
		return []sim.Field{
			{Key: AttrTransferTo, Value: idString(o.To)},
			{Key: AttrTransferRemaining, Value: sim.IntValue(o.Remaining)},
		}
	})); err != nil {
		return err
	}
	// Rates are derived properties so future upgrades/formulas can vary
	// them per unit without touching the systems.
	if err := r.Derived(sim.DerivedDef{Name: DerivedExtractionRate, Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
		if p.cfg.ExtractionAttribute != "" {
			v, ok := p.attrs.Lookup(w, id, p.cfg.ExtractionAttribute)
			return max(v, 0), ok && stores.Has(w, id)
		}
		return p.cfg.ExtractionRate, stores.Has(w, id)
	}}); err != nil {
		return err
	}
	if err := r.Derived(sim.DerivedDef{Name: DerivedTransferRate, Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
		return p.cfg.TransferRate, stores.Has(w, id)
	}}); err != nil {
		return err
	}
	r.OnCreate(func(w *sim.World, e sim.Entity) {
		if e.Kind == sim.KindUnit {
			c := p.capacity(w, e.ID)
			stores.Set(w, e.ID, Store{Energy: min(p.cfg.InitialEnergy, c), Capacity: c})
		}
	})
	if err := r.Phase(sim.PhaseDef{Name: Phase}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "energy.mine", Phase: Phase, Run: p.mine}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "energy.transfer", Phase: Phase, After: []string{"energy.mine"}, Run: p.transfer}); err != nil {
		return err
	}
	for _, c := range []sim.CommandDef{
		{Type: CommandMine, Execute: p.execMine, ApplyToUnit: p.applyMine},
		{Type: CommandStopMining, Execute: p.execStopMining, ApplyToUnit: p.applyStopMining},
		{Type: CommandTransfer, Execute: p.execTransfer},
		{Type: CommandSetEnergy, AdminOnly: true, Execute: p.execSetEnergy},
	} {
		if err := r.Command(c); err != nil {
			return err
		}
	}
	return nil
}

// ---- commands ---------------------------------------------------------------

// ownedUnit validates that id is a unit with energy storage that the issuer
// may command.
func ownedUnit(w *sim.World, issuer sim.Principal, id uint64) (sim.Entity, error) {
	e, ok := w.Entity(sim.EntityID(id))
	if !ok || e.Kind != sim.KindUnit || !stores.Has(w, e.ID) {
		return e, sim.Unavailable("unit", sim.EntityID(id))
	}
	if !issuer.Admin && e.Owner != issuer.Player {
		return e, sim.Unavailable("unit", sim.EntityID(id))
	}
	return e, nil
}

func checkUnits(w *sim.World, issuer sim.Principal, ids []uint64) error {
	if len(ids) == 0 || len(ids) > MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", MaxUnitsPerCommand, len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		if _, err := ownedUnit(w, issuer, id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) execMine(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*energyv1.MineCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want MineCommand", cmd.Payload)
	}
	w := ctx.World
	if err := checkUnits(w, cmd.Issuer, m.GetUnitIds()); err != nil {
		return err
	}
	well := sim.EntityID(m.GetWellId())
	if err := p.checkWell(w, cmd.Issuer, well); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		mining.Set(w, sim.EntityID(id), miningOrder{Well: well})
	}
	return nil
}

// applyMine gives one unit a mining order (batch commands; the template's
// well_id applies).
func (p *Plugin) applyMine(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*energyv1.MineCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want MineCommand", cmd.Payload)
	}
	if _, err := ownedUnit(ctx.World, cmd.Issuer, uint64(unit)); err != nil {
		return err
	}
	well := sim.EntityID(m.GetWellId())
	if err := p.checkWell(ctx.World, cmd.Issuer, well); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	mining.Set(ctx.World, unit, miningOrder{Well: well})
	return nil
}

// applyStopMining cancels one unit's mining order (batch commands).
func (p *Plugin) applyStopMining(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	if _, err := ownedUnit(ctx.World, cmd.Issuer, uint64(unit)); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	mining.Remove(ctx.World, unit)
	return nil
}

func (p *Plugin) execStopMining(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*energyv1.StopMiningCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want StopMiningCommand", cmd.Payload)
	}
	if err := checkUnits(ctx.World, cmd.Issuer, m.GetUnitIds()); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		mining.Remove(ctx.World, sim.EntityID(id))
	}
	return nil
}

func adjacent(a, b sim.Pos) bool {
	dx, dy := a.X-b.X, a.Y-b.Y
	return max(dx, -dx, dy, -dy) == 1
}

func (p *Plugin) execTransfer(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*energyv1.TransferCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want TransferCommand", cmd.Payload)
	}
	w := ctx.World
	if m.GetAmount() <= 0 {
		return sim.Fail(sim.CodeInvalidArgument, "amount must be positive, got %d", m.GetAmount())
	}
	if m.GetFromUnitId() == m.GetToUnitId() {
		return sim.Fail(sim.CodeInvalidArgument, "cannot transfer to the same unit")
	}
	from, err := ownedUnit(w, cmd.Issuer, m.GetFromUnitId())
	if err != nil {
		return err
	}
	to, ok := w.Entity(sim.EntityID(m.GetToUnitId()))
	if !ok || to.Kind != sim.KindUnit || !stores.Has(w, to.ID) {
		return sim.Unavailable("unit", sim.EntityID(m.GetToUnitId()))
	}
	// Friendly means same owner; an admin may only move energy between
	// units of one owner as well.
	if to.Owner != from.Owner {
		if !cmd.Issuer.Admin {
			// Same answer as for a missing unit: no probing of enemies.
			return sim.Unavailable("unit", to.ID)
		}
		return sim.Fail(sim.CodePermissionDenied, "unit %d is not friendly", m.GetToUnitId())
	}
	if !adjacent(from.Pos, to.Pos) {
		return sim.Fail(sim.CodeFailedPrecondition, "units %d and %d are not adjacent", from.ID, to.ID)
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	transfers.Set(w, from.ID, transferOrder{To: to.ID, Remaining: m.GetAmount()})
	return nil
}

func (p *Plugin) execSetEnergy(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*energyv1.SetUnitEnergyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetUnitEnergyCommand", cmd.Payload)
	}
	id := sim.EntityID(m.GetUnitId())
	s, ok := stores.Get(ctx.World, id)
	if !ok {
		return sim.Unavailable("unit", sim.EntityID(m.GetUnitId()))
	}
	if m.Capacity != nil {
		s.Capacity = m.GetCapacity()
	}
	s.Energy = m.GetEnergy()
	if s.Capacity < 0 || s.Energy < 0 || s.Energy > s.Capacity {
		return sim.Fail(sim.CodeInvalidArgument, "need 0 <= energy (%d) <= capacity (%d)", s.Energy, s.Capacity)
	}
	stores.Set(ctx.World, id, s)
	return nil
}

// ---- systems ----------------------------------------------------------------

func (p *Plugin) stopMining(ctx *sim.TickContext, unit sim.EntityID, well sim.EntityID, reason string) {
	mining.Remove(ctx.World, unit)
	e, _ := ctx.World.Entity(unit)
	ctx.Emit(sim.Event{Type: EventMiningStopped, Entity: unit, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
		{Key: "reason", Value: sim.StringValue(reason)},
		{Key: "well", Value: idString(well)},
	}})
}

func (p *Plugin) mine(ctx *sim.TickContext) {
	w := ctx.World
	claims := map[sim.EntityID][]fairshare.Claim{}
	var wellOrder []sim.EntityID
	for _, id := range mining.Entities(w) {
		o, _ := mining.Get(w, id)
		wv, ok := wells.Get(w, o.Well)
		if !ok {
			p.stopMining(ctx, id, o.Well, "well_removed")
			continue
		}
		if !wv.Unlimited && wv.Energy == 0 {
			p.stopMining(ctx, id, o.Well, "well_depleted")
			continue
		}
		unit, _ := w.Entity(id)
		wellEnt, _ := w.Entity(o.Well)
		if !adjacent(unit.Pos, wellEnt.Pos) {
			continue // waits until adjacent
		}
		s, _ := stores.Get(w, id)
		rate, _ := w.Derived(DerivedExtractionRate, id)
		want := min(rate, s.Free())
		if want <= 0 {
			continue
		}
		if claims[o.Well] == nil {
			wellOrder = append(wellOrder, o.Well)
		}
		claims[o.Well] = append(claims[o.Well], fairshare.Claim{ID: uint64(id), Want: want})
	}
	slices.Sort(wellOrder)

	for _, wid := range wellOrder {
		wv, _ := wells.Get(w, wid)
		avail := wv.ExtractionRate
		if !wv.Unlimited {
			avail = min(avail, wv.Energy)
		}
		cs := claims[wid]
		grants := fairshare.Split(cs, avail)
		var total int64
		var miners []sim.PlayerID
		for i, c := range cs {
			g := grants[i]
			if g == 0 {
				continue
			}
			unit := sim.EntityID(c.ID)
			s, _ := stores.Get(w, unit)
			s.Energy += g
			stores.Set(w, unit, s)
			total += g
			e, _ := w.Entity(unit)
			if !slices.Contains(miners, e.Owner) {
				miners = append(miners, e.Owner)
			}
			ctx.Emit(sim.Event{Type: EventMined, Entity: unit, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
				{Key: "amount", Value: sim.IntValue(g)},
				{Key: "well", Value: idString(wid)},
			}})
		}
		if wv.Unlimited || total == 0 {
			continue
		}
		wv.Energy -= total
		wells.Set(w, wid, wv)
		if wv.Energy == 0 {
			// Only players whose units just drained it learn of it; everyone
			// else sees the well change only if they observe it.
			slices.Sort(miners)
			for _, o := range miners {
				ctx.Emit(sim.Event{Type: EventWellDepleted, Entity: wid, Audience: sim.ToPlayer(o)})
			}
			for _, id := range mining.Entities(w) {
				if o, _ := mining.Get(w, id); o.Well == wid {
					p.stopMining(ctx, id, wid, "well_depleted")
				}
			}
		}
	}
}

func (p *Plugin) endTransfer(ctx *sim.TickContext, sender sim.EntityID, o transferOrder, reason string) {
	transfers.Remove(ctx.World, sender)
	e, _ := ctx.World.Entity(sender)
	ctx.Emit(sim.Event{Type: EventTransferEnded, Entity: sender, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
		{Key: "to", Value: idString(o.To)},
		{Key: "reason", Value: sim.StringValue(reason)},
		{Key: "remaining", Value: sim.IntValue(o.Remaining)},
	}})
}

func (p *Plugin) transfer(ctx *sim.TickContext) {
	w := ctx.World
	for _, id := range transfers.Entities(w) {
		o, _ := transfers.Get(w, id)
		from, _ := w.Entity(id)
		to, ok := w.Entity(o.To)
		if !ok || !stores.Has(w, o.To) {
			p.endTransfer(ctx, id, o, "target_lost")
			continue
		}
		if !adjacent(from.Pos, to.Pos) {
			p.endTransfer(ctx, id, o, "not_adjacent")
			continue
		}
		s, _ := stores.Get(w, id)
		r, _ := stores.Get(w, o.To)
		rate, _ := w.Derived(DerivedTransferRate, id)
		amt := min(rate, o.Remaining, s.Energy, r.Free())
		if amt > 0 {
			s.Energy -= amt
			r.Energy += amt
			stores.Set(w, id, s)
			stores.Set(w, o.To, r)
			o.Remaining -= amt
			ctx.Emit(sim.Event{Type: EventTransferred, Entity: id, Audience: sim.ToPlayer(from.Owner), Fields: []sim.Field{
				{Key: "to", Value: idString(o.To)},
				{Key: "amount", Value: sim.IntValue(amt)},
			}})
		}
		switch {
		case o.Remaining == 0:
			p.endTransfer(ctx, id, o, "completed")
		case s.Energy == 0:
			p.endTransfer(ctx, id, o, "sender_empty")
		case r.Free() == 0:
			p.endTransfer(ctx, id, o, "receiver_full")
		default:
			transfers.Set(w, id, o)
		}
	}
}
