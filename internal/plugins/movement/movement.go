// Package movement is the Milestone 1 movement plugin: persistent
// destination-based orders, one-unit-per-tile occupancy, and deterministic
// simultaneous conflict resolution. It uses only the public sim plugin API.
//
// # Rules
//
// Each tick, in the "movement" phase, every unit with an order that is
// eligible to move (see TicksPerMove and contention backoff) proposes a
// single step toward its destination: one tile in each axis toward the
// target (8-directional, diagonals allowed; no pathfinding). All proposed
// steps are then resolved simultaneously:
//
//   - Contention: if two or more units propose the same tile, all of them
//     fail this tick. Each then waits a seeded-random 0..ContentionBackoff
//     extra ticks before trying again, which breaks symmetric deadlocks
//     without favouring any unit.
//   - Empty tile: the step succeeds.
//   - Occupied by a unit that is not stepping this tick: the step fails.
//   - Occupied by a unit that is stepping this tick: the step succeeds if
//     and only if that unit's step succeeds (chains move together, e.g. a
//     column of units following its leader).
//   - Cycles, including two units swapping places, fail as a whole.
//
// A failed step does not cancel the order; the unit retries on later ticks.
// After MaxBlockedTicks consecutive failures the order is cancelled and a
// "movement.cancelled" event is sent to the owner. On reaching its
// destination a unit's order completes with a "movement.arrived" event.
package movement

import (
	"fmt"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "movement"

// Identifiers registered by this plugin.
const (
	Phase           = "movement"
	SystemStep      = "movement.step"
	AttrDestination = "movement.destination"
	DerivedPace     = "movement.ticks_per_move"
	EventArrived    = "movement.arrived"
	EventCancelled  = "movement.cancelled"
)

// Command types (fully-qualified protobuf names of the payloads).
var (
	CommandMove = string((&movementv1.MoveCommand{}).ProtoReflect().Descriptor().FullName())
	CommandStop = string((&movementv1.StopCommand{}).ProtoReflect().Descriptor().FullName())
)

// MaxOrdersPerCommand bounds the work a single command can request.
const MaxOrdersPerCommand = 4096

// Config is the plugin's configuration schema (the "movement" section).
type Config struct {
	// TicksPerMove is how many ticks a unit waits between steps (>= 1).
	TicksPerMove int `json:"ticks_per_move"`
	// MaxBlockedTicks cancels an order after this many consecutive failed
	// steps. 0 means never.
	MaxBlockedTicks int `json:"max_blocked_ticks"`
	// ContentionBackoff is the maximum number of extra ticks a unit waits
	// after losing a contested tile.
	ContentionBackoff int `json:"contention_backoff_ticks"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{TicksPerMove: 1, MaxBlockedTicks: 20, ContentionBackoff: 2}
}

func (c Config) validate() error {
	if c.TicksPerMove < 1 {
		return fmt.Errorf("ticks_per_move must be >= 1, got %d", c.TicksPerMove)
	}
	if c.MaxBlockedTicks < 0 {
		return fmt.Errorf("max_blocked_ticks must be >= 0, got %d", c.MaxBlockedTicks)
	}
	if c.ContentionBackoff < 0 {
		return fmt.Errorf("contention_backoff_ticks must be >= 0, got %d", c.ContentionBackoff)
	}
	return nil
}

// order is a unit's persistent movement order.
type order struct {
	Dest    sim.Pos
	Blocked int32 // consecutive failed steps
}

var (
	orders  = sim.NewComponent[order]("movement.order")
	readyAt = sim.NewComponent[sim.Tick]("movement.ready_at") // earliest tick of next step
)

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg Config
}

// New returns the movement plugin.
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return nil }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if err := p.cfg.validate(); err != nil {
		return err
	}
	// The destination is private to the owner: opponents can see where a
	// unit is, not where it is going.
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		return []sim.Field{{Key: AttrDestination, Value: sim.PosValue(o.Dest)}}
	})); err != nil {
		return err
	}
	if err := r.Component(readyAt.Def(sim.VisibleAdmin, nil)); err != nil {
		return err
	}
	// Pace is a derived property so a future formula (mass, propulsion...)
	// can replace the constant without touching the movement system.
	if err := r.Derived(sim.DerivedDef{Name: DerivedPace, Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
		if _, ok := w.Entity(id); !ok {
			return 0, false
		}
		return int64(p.cfg.TicksPerMove), true
	}}); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: SystemStep, Phase: Phase, Run: p.step}); err != nil {
		return err
	}
	if err := r.Command(sim.CommandDef{Type: CommandMove, Execute: p.execMove}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandStop, Execute: p.execStop})
}

// checkUnit validates that id is a unit the issuer may command.
func checkUnit(w *sim.World, issuer sim.Principal, id uint64, seen map[sim.EntityID]bool) error {
	eid := sim.EntityID(id)
	if seen[eid] {
		return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
	}
	seen[eid] = true
	e, ok := w.Entity(eid)
	if !ok || e.Kind != sim.KindUnit {
		return sim.Fail(sim.CodeNotFound, "unit %d not found", id)
	}
	if !issuer.Admin && e.Owner != issuer.Player {
		return sim.Fail(sim.CodePermissionDenied, "unit %d is not yours", id)
	}
	return nil
}

func (p *Plugin) execMove(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*movementv1.MoveCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want MoveCommand", cmd.Payload)
	}
	n := len(m.GetOrders())
	if n == 0 || n > MaxOrdersPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d orders, got %d", MaxOrdersPerCommand, n)
	}
	w := ctx.World
	seen := make(map[sim.EntityID]bool, n)
	for _, o := range m.GetOrders() {
		if err := checkUnit(w, cmd.Issuer, o.GetUnitId(), seen); err != nil {
			return err
		}
		if o.GetDestination() == nil {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d: destination missing", o.GetUnitId())
		}
		d := sim.Pos{X: o.GetDestination().GetX(), Y: o.GetDestination().GetY()}
		if !w.InBounds(d) {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d: destination %v outside %dx%d world", o.GetUnitId(), d, w.Width(), w.Height())
		}
	}
	// All valid: apply. A new order replaces any previous one.
	for _, o := range m.GetOrders() {
		id := sim.EntityID(o.GetUnitId())
		orders.Set(w, id, order{Dest: sim.Pos{X: o.GetDestination().GetX(), Y: o.GetDestination().GetY()}})
	}
	return nil
}

func (p *Plugin) execStop(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*movementv1.StopCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want StopCommand", cmd.Payload)
	}
	n := len(m.GetUnitIds())
	if n == 0 || n > MaxOrdersPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", MaxOrdersPerCommand, n)
	}
	seen := make(map[sim.EntityID]bool, n)
	for _, id := range m.GetUnitIds() {
		if err := checkUnit(ctx.World, cmd.Issuer, id, seen); err != nil {
			return err
		}
	}
	for _, id := range m.GetUnitIds() {
		orders.Remove(ctx.World, sim.EntityID(id))
	}
	return nil
}

func sign(v int32) int32 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// Resolution states for a proposed step.
const (
	unresolved uint8 = iota
	visiting
	succeeded
	failed
)

type intent struct {
	to    sim.Pos
	state uint8
}

func (p *Plugin) step(ctx *sim.TickContext) {
	w := ctx.World
	tick := ctx.Tick
	ids := orders.Entities(w) // sorted: all iteration below is deterministic

	// 1. Collect proposed steps.
	intents := make(map[sim.EntityID]*intent, len(ids))
	claims := make(map[sim.Pos]int, len(ids))
	var proposers []sim.EntityID
	for _, id := range ids {
		o, _ := orders.Get(w, id)
		e, ok := w.Entity(id)
		if !ok {
			continue
		}
		if e.Pos == o.Dest {
			p.complete(ctx, e)
			continue
		}
		if r, ok := readyAt.Get(w, id); ok && tick < r {
			continue
		}
		to := sim.Pos{X: e.Pos.X + sign(o.Dest.X-e.Pos.X), Y: e.Pos.Y + sign(o.Dest.Y-e.Pos.Y)}
		intents[id] = &intent{to: to}
		claims[to]++
		proposers = append(proposers, id)
	}

	// 2. Resolve. The outcome of each step is a pure function of the intent
	// graph, so it does not depend on visiting order.
	var resolve func(id sim.EntityID) uint8
	resolve = func(id sim.EntityID) uint8 {
		in := intents[id]
		switch in.state {
		case succeeded, failed:
			return in.state
		case visiting:
			return failed // cycle (including swaps)
		}
		in.state = visiting
		result := succeeded
		if claims[in.to] > 1 {
			result = failed
		} else if occ, taken := w.EntityAt(in.to); taken {
			if _, moving := intents[occ]; !moving || resolve(occ) != succeeded {
				result = failed
			}
		}
		in.state = result
		return result
	}
	var moves []sim.Move
	for _, id := range proposers {
		if resolve(id) == succeeded {
			moves = append(moves, sim.Move{ID: id, To: intents[id].to})
		}
	}
	if err := w.MoveEntities(moves); err != nil {
		// Resolution guarantees a valid batch; anything else is a bug.
		panic(fmt.Sprintf("movement: resolved moves rejected: %v", err))
	}

	// 3. Update orders.
	for _, id := range proposers {
		in := intents[id]
		o, _ := orders.Get(w, id)
		e, _ := w.Entity(id)
		if in.state == succeeded {
			pace, _ := w.Derived(DerivedPace, id)
			readyAt.Set(w, id, tick+sim.Tick(pace))
			if e.Pos == o.Dest {
				p.complete(ctx, e)
				continue
			}
			o.Blocked = 0
			orders.Set(w, id, o)
			continue
		}
		o.Blocked++
		if p.cfg.MaxBlockedTicks > 0 && int(o.Blocked) >= p.cfg.MaxBlockedTicks {
			orders.Remove(w, id)
			ctx.Emit(sim.Event{Type: EventCancelled, Entity: id, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
				{Key: "reason", Value: sim.StringValue("blocked")},
				{Key: "destination", Value: sim.PosValue(o.Dest)},
			}})
			continue
		}
		orders.Set(w, id, o)
		if claims[in.to] > 1 && p.cfg.ContentionBackoff > 0 {
			readyAt.Set(w, id, tick+1+sim.Tick(w.Rand().Intn(p.cfg.ContentionBackoff+1)))
		}
	}
}

func (p *Plugin) complete(ctx *sim.TickContext, e sim.Entity) {
	orders.Remove(ctx.World, e.ID)
	ctx.Emit(sim.Event{Type: EventArrived, Entity: e.ID, Audience: sim.ToPlayer(e.Owner), Fields: []sim.Field{
		{Key: "position", Value: sim.PosValue(e.Pos)},
	}})
}
