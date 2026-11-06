// Package salvage implements wreckage and salvage collection.
//
// # Wreckage
//
// When combat destroys a unit, this plugin may leave a wreck on the unit's
// tile: a passable resource entity (units can stand on it, several wrecks
// can share a tile, it never blocks movement) holding
//
//	floor(min(embodied, invested) * embodied_recovery_permille / 1000)
//	  + floor(stored * stored_recovery_permille / 1000)
//
// energy. "embodied" and "invested" come from the attributes plugin's
// upgrade accounting; taking the minimum means energy granted by admin
// attribute edits (embodied but never paid) is never recovered. Fractions
// are integers in thousandths and rounding is always down, so a wreck never
// holds more than the configured share of what the unit carried. A unit
// that would leave 0 energy leaves no wreck.
//
// Wreckage is distinct from energy wells: it has its own component
// (salvage.energy / salvage.initial), cannot be mined, and is emptied only
// by salvaging. Wrecks are unowned and public.
//
// # Salvage (phase "salvage": after energy, before movement)
//
// SalvageCommand gives units a persistent order to collect from a wreck
// while within distance 1 (adjacent or on it). Each tick, each reaching
// collector asks for min(salvage_rate, free battery capacity); requests on
// one wreck are split max-min fairly (package fairshare) over the wreck's
// remaining energy. The wreck loses exactly what the collectors gain, so
// energy is never duplicated, and once it reaches 0 it is removed and every
// order on it ends ("depleted"). Orders on a wreck that disappears
// otherwise end with "wreck_gone".
package salvage

import (
	"fmt"
	"math"
	"slices"
	"strconv"

	salvagev1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/salvage/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/fairshare"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "salvage"

// Identifiers.
const (
	Phase = "salvage"

	EventWreckCreated  = "salvage.wreck_created"  // destroyed unit's owner + its attackers' owners
	EventCollected     = "salvage.collected"      // collector's owner: amount, wreck
	EventWreckDepleted = "salvage.wreck_depleted" // owners of the collectors that emptied it
	EventEnded         = "salvage.ended"          // collector's owner: wreck, reason
)

// Command types.
var (
	CommandSalvage   = string((&salvagev1.SalvageCommand{}).ProtoReflect().Descriptor().FullName())
	CommandStop      = string((&salvagev1.StopSalvageCommand{}).ProtoReflect().Descriptor().FullName())
	CommandCreate    = string((&salvagev1.CreateWreckCommand{}).ProtoReflect().Descriptor().FullName())
	CommandSetEnergy = string((&salvagev1.SetWreckEnergyCommand{}).ProtoReflect().Descriptor().FullName())
)

// MaxUnitsPerCommand bounds one command.
const MaxUnitsPerCommand = 4096

// Config is the plugin's configuration schema (the "salvage" section).
type Config struct {
	// Recovery fractions in thousandths (0..1000).
	EmbodiedRecoveryPermille int64 `json:"embodied_recovery_permille"`
	StoredRecoveryPermille   int64 `json:"stored_recovery_permille"`
	// SalvageRate is how much one unit collects per tick, optionally bound
	// to an attributes property.
	SalvageRate combat.Stat `json:"salvage_rate"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{EmbodiedRecoveryPermille: 500, StoredRecoveryPermille: 500, SalvageRate: combat.Stat{Default: 5}}
}

// Wreck is a wreck's energy.
type Wreck struct {
	Energy  int64
	Initial int64
}

type order struct {
	Wreck sim.EntityID
}

var (
	wrecks = sim.NewComponent[Wreck]("salvage.wreck")
	orders = sim.NewComponent[order]("salvage.order")
)

// GetWreck returns a wreck's state.
func GetWreck(w *sim.World, id sim.EntityID) (Wreck, bool) { return wrecks.Get(w, id) }

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg    Config
	attrs  *attributes.Plugin
	combat *combat.Plugin

	// CanSee, if set, restricts which wrecks a player may refer to in
	// commands (fog of war). nil means every wreck.
	CanSee func(w *sim.World, player sim.PlayerID, id sim.EntityID) bool
}

// New returns the salvage plugin. attrs may be nil (no upgrade investment
// to recover, constant salvage rate).
func New(attrs *attributes.Plugin, c *combat.Plugin) *Plugin {
	return &Plugin{attrs: attrs, combat: c}
}

func (*Plugin) Name() string { return Name }

// DependsOn includes movement so the salvage phase can run before it.
func (p *Plugin) DependsOn() []string {
	deps := []string{energy.Name, combat.Name, movement.Name}
	if p.attrs != nil {
		deps = append(deps, attributes.Name)
	}
	return deps
}

// Recoverable returns the wreck energy for a destroyed unit.
func (p *Plugin) Recoverable(d combat.Destroyed) int64 {
	return add(permille(min(d.Embodied, d.Invested), p.cfg.EmbodiedRecoveryPermille),
		permille(d.Stored, p.cfg.StoredRecoveryPermille))
}

// permille returns floor(x * f / 1000) for x >= 0, 0 <= f <= 1000, without
// overflow.
func permille(x, f int64) int64 {
	if x <= 0 || f <= 0 {
		return 0
	}
	return x/1000*f + x%1000*f/1000
}

func add(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	for name, v := range map[string]int64{"embodied_recovery_permille": p.cfg.EmbodiedRecoveryPermille, "stored_recovery_permille": p.cfg.StoredRecoveryPermille} {
		if v < 0 || v > 1000 {
			return fmt.Errorf("salvage %s must be in 0..1000, got %d", name, v)
		}
	}
	if p.cfg.SalvageRate.Default < 0 {
		return fmt.Errorf("salvage salvage_rate.default must be >= 0")
	}
	if pr := p.cfg.SalvageRate.Property; pr != "" && (p.attrs == nil || !p.attrs.Has(pr)) {
		return fmt.Errorf("salvage salvage_rate.property: %q is not an attribute or derived property", pr)
	}
	if err := r.Component(wrecks.Def(sim.VisiblePublic, func(wk Wreck) []sim.Field {
		return []sim.Field{
			{Key: "salvage.energy", Value: sim.IntValue(wk.Energy)},
			{Key: "salvage.initial", Value: sim.IntValue(wk.Initial)},
		}
	})); err != nil {
		return err
	}
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		return []sim.Field{{Key: "salvage.target", Value: sim.StringValue(strconv.FormatUint(uint64(o.Wreck), 10))}}
	})); err != nil {
		return err
	}
	p.combat.OnDestroyed(p.leaveWreck)
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{energy.Phase}, Before: []string{movement.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "salvage.collect", Phase: Phase, Run: p.collect}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config":       p.cfg,
		"wreck_energy": "floor(min(embodied, invested) * embodied_recovery_permille / 1000) + floor(stored * stored_recovery_permille / 1000)",
		"reach":        "Chebyshev distance <= 1 (adjacent or on the wreck)",
		"blocking":     "wreckage is passable",
	}); err != nil {
		return err
	}
	for _, d := range []sim.CommandDef{
		{Type: CommandSalvage, Execute: p.execSalvage, ApplyToUnit: p.applySalvage},
		{Type: CommandStop, Execute: p.execStop, ApplyToUnit: p.applyStop},
		{Type: CommandCreate, AdminOnly: true, Execute: p.execCreate},
		{Type: CommandSetEnergy, AdminOnly: true, Execute: p.execSetEnergy},
	} {
		if err := r.Command(d); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) leaveWreck(ctx *sim.TickContext, d combat.Destroyed) {
	amount := p.Recoverable(d)
	if amount <= 0 {
		return
	}
	id, err := ctx.World.CreatePassable(sim.KindResource, 0, d.Unit.Pos)
	if err != nil {
		panic(fmt.Sprintf("salvage: wreck at %v: %v", d.Unit.Pos, err))
	}
	wrecks.Set(ctx.World, id, Wreck{Energy: amount, Initial: amount})
	for _, o := range d.Audience {
		ctx.Emit(sim.Event{Type: EventWreckCreated, Entity: id, Audience: sim.ToPlayer(o), Fields: []sim.Field{
			{Key: "energy", Value: sim.IntValue(amount)},
			{Key: "position", Value: sim.PosValue(d.Unit.Pos)},
			{Key: "unit", Value: sim.StringValue(strconv.FormatUint(uint64(d.Unit.ID), 10))},
		}})
	}
}

func (p *Plugin) rate(w *sim.World, id sim.EntityID) int64 {
	if pr := p.cfg.SalvageRate.Property; pr != "" {
		v, _ := p.attrs.Lookup(w, id, pr)
		return max(v, 0)
	}
	return p.cfg.SalvageRate.Default
}

// ---- commands ---------------------------------------------------------------

func ownedUnit(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit {
		return sim.Unavailable("unit", sim.EntityID(id))
	}
	if _, hasStore := energy.GetStore(w, id); !hasStore {
		return sim.Unavailable("unit", id)
	}
	if !issuer.Admin && e.Owner != issuer.Player {
		return sim.Unavailable("unit", sim.EntityID(id))
	}
	return nil
}

// checkWreck validates a wreck reference. Missing and undetected wrecks
// give players the same answer.
func (p *Plugin) checkWreck(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
	if !wrecks.Has(w, id) {
		return sim.Unavailable("wreck", id)
	}
	if !issuer.Admin && p.CanSee != nil && !p.CanSee(w, issuer.Player, id) {
		return sim.Unavailable("wreck", id)
	}
	return nil
}

func unitList(ids []uint64) error {
	if len(ids) == 0 || len(ids) > MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", MaxUnitsPerCommand, len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
	}
	return nil
}

func (p *Plugin) execSalvage(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*salvagev1.SalvageCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SalvageCommand", cmd.Payload)
	}
	if err := unitList(m.GetUnitIds()); err != nil {
		return err
	}
	wreck := sim.EntityID(m.GetWreckId())
	if err := p.checkWreck(ctx.World, cmd.Issuer, wreck); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		if err := ownedUnit(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		orders.Set(ctx.World, sim.EntityID(id), order{Wreck: wreck})
	}
	return nil
}

func (p *Plugin) applySalvage(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*salvagev1.SalvageCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SalvageCommand", cmd.Payload)
	}
	wreck := sim.EntityID(m.GetWreckId())
	if err := p.checkWreck(ctx.World, cmd.Issuer, wreck); err != nil {
		return err
	}
	if err := ownedUnit(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	orders.Set(ctx.World, unit, order{Wreck: wreck})
	return nil
}

func (p *Plugin) execStop(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*salvagev1.StopSalvageCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want StopSalvageCommand", cmd.Payload)
	}
	if err := unitList(m.GetUnitIds()); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		if err := ownedUnit(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(m.GetUnitIds())); err != nil {
		return err
	}
	for _, id := range m.GetUnitIds() {
		orders.Remove(ctx.World, sim.EntityID(id))
	}
	return nil
}

func (p *Plugin) applyStop(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	if err := ownedUnit(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	orders.Remove(ctx.World, unit)
	return nil
}

func (p *Plugin) execCreate(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*salvagev1.CreateWreckCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CreateWreckCommand", cmd.Payload)
	}
	if m.GetPosition() == nil || m.GetEnergy() <= 0 {
		return sim.Fail(sim.CodeInvalidArgument, "position and positive energy required")
	}
	pos := sim.Pos{X: m.GetPosition().GetX(), Y: m.GetPosition().GetY()}
	id, err := ctx.World.CreatePassable(sim.KindResource, 0, pos)
	if err != nil {
		return sim.Fail(sim.CodeInvalidArgument, "%v", err)
	}
	wrecks.Set(ctx.World, id, Wreck{Energy: m.GetEnergy(), Initial: m.GetEnergy()})
	ctx.Emit(sim.Event{Type: sim.EventEntityCreated, Entity: id, Audience: sim.AdminAudience(), Fields: []sim.Field{
		{Key: "command_id", Value: sim.StringValue(cmd.ID)},
		{Key: "position", Value: sim.PosValue(pos)},
	}})
	return nil
}

func (p *Plugin) execSetEnergy(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*salvagev1.SetWreckEnergyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetWreckEnergyCommand", cmd.Payload)
	}
	id := sim.EntityID(m.GetWreckId())
	wk, ok := wrecks.Get(ctx.World, id)
	if !ok {
		return sim.Fail(sim.CodeNotFound, "wreck %d not found", m.GetWreckId())
	}
	if m.GetEnergy() < 0 {
		return sim.Fail(sim.CodeInvalidArgument, "energy must be >= 0")
	}
	if m.GetEnergy() == 0 {
		return ctx.World.RemoveEntity(id)
	}
	wk.Energy = m.GetEnergy()
	wk.Initial = max(wk.Initial, wk.Energy)
	wrecks.Set(ctx.World, id, wk)
	return nil
}

// ---- collection ---------------------------------------------------------------

func (p *Plugin) end(ctx *sim.TickContext, unit, wreck sim.EntityID, reason string) {
	orders.Remove(ctx.World, unit)
	e, _ := ctx.World.Entity(unit)
	ctx.Emit(sim.Event{Type: EventEnded, Entity: unit, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
		{Key: "wreck", Value: sim.StringValue(strconv.FormatUint(uint64(wreck), 10))},
		{Key: "reason", Value: sim.StringValue(reason)},
	}})
}

func (p *Plugin) collect(ctx *sim.TickContext) {
	w := ctx.World
	claims := map[sim.EntityID][]fairshare.Claim{}
	var wreckIDs []sim.EntityID
	for _, id := range orders.Entities(w) {
		o, _ := orders.Get(w, id)
		wk, ok := w.Entity(o.Wreck)
		if !ok || !wrecks.Has(w, o.Wreck) {
			p.end(ctx, id, o.Wreck, "wreck_gone")
			continue
		}
		u, _ := w.Entity(id)
		if combat.Distance(u.Pos, wk.Pos) > 1 {
			continue // waits until within reach
		}
		s, _ := energy.GetStore(w, id)
		want := min(p.rate(w, id), s.Free())
		if want <= 0 {
			continue
		}
		if claims[o.Wreck] == nil {
			wreckIDs = append(wreckIDs, o.Wreck)
		}
		claims[o.Wreck] = append(claims[o.Wreck], fairshare.Claim{ID: uint64(id), Want: want})
	}
	slices.Sort(wreckIDs)
	for _, wid := range wreckIDs {
		wk, _ := wrecks.Get(w, wid)
		cs := claims[wid]
		grants := fairshare.Split(cs, wk.Energy)
		var total int64
		var owners []sim.PlayerID
		for i, c := range cs {
			if grants[i] == 0 {
				continue
			}
			unit := sim.EntityID(c.ID)
			s, _ := energy.GetStore(w, unit)
			energy.SetStored(w, unit, s.Energy+grants[i])
			total += grants[i]
			e, _ := w.Entity(unit)
			if !slices.Contains(owners, e.Owner) {
				owners = append(owners, e.Owner)
			}
			ctx.Emit(sim.Event{Type: EventCollected, Entity: unit, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
				{Key: "amount", Value: sim.IntValue(grants[i])},
				{Key: "wreck", Value: sim.StringValue(strconv.FormatUint(uint64(wid), 10))},
			}})
		}
		wk.Energy -= total
		if wk.Energy > 0 {
			wrecks.Set(w, wid, wk)
			continue
		}
		// Emptied: remove it and end every order on it.
		slices.Sort(owners)
		for _, o := range owners {
			ctx.Emit(sim.Event{Type: EventWreckDepleted, Entity: wid, Audience: sim.ToPlayer(o)})
		}
		if err := w.RemoveEntity(wid); err != nil {
			panic(err)
		}
		for _, id := range orders.Entities(w) {
			if o, _ := orders.Get(w, id); o.Wreck == wid {
				p.end(ctx, id, wid, "depleted")
			}
		}
	}
}

// ---- API for other plugins (automation) -----------------------------------

// Wrecks returns all wrecks, ascending. Callers filter by visibility.
func Wrecks(w *sim.World) []sim.EntityID { return wrecks.Entities(w) }

// SetSalvage gives a unit a salvage order.
func SetSalvage(w *sim.World, id, wreck sim.EntityID) { orders.Set(w, id, order{Wreck: wreck}) }

// StopSalvage cancels a unit's salvage order.
func StopSalvage(w *sim.World, id sim.EntityID) { orders.Remove(w, id) }

// SalvageTarget returns the wreck a unit is salvaging.
func SalvageTarget(w *sim.World, id sim.EntityID) (sim.EntityID, bool) {
	o, ok := orders.Get(w, id)
	return o.Wreck, ok
}
