// Package automation implements persistent unit behaviors as small typed
// state machines: mine-and-deliver, auto-salvage, guard and follow.
//
// # Execution
//
// Behaviors are evaluated in phase "automation", the first phase of a tick
// (right after commands, before energy, movement and combat), so their
// decisions use the state left by the previous tick — the same state the
// owner observed. A unit is evaluated when (id + tick) % eval_interval == 0
// (or right after its behavior is assigned), and at most
// max_evaluations_per_tick units are evaluated per tick; the rest wait for
// their next slot. Work per unit per evaluation is bounded (fixed-size
// neighbourhood scans, no recursion), and behaviors never issue commands:
// they set the unit's ordinary orders (movement, mining, transfer, attack,
// salvage), which the regular mechanics then execute.
//
// # Information
//
// Behaviors see only what the owner may: wells, wrecks and enemies must be
// detected by the owner (visibility plugin, end of previous tick);
// receivers and leaders must be the owner's own units.
//
// # States
//
//	mine_and_deliver: to_well -> mining -> to_receiver -> delivering -> to_well
//	auto_salvage:     searching -> approaching -> collecting -> searching
//	guard:            holding <-> engaging
//	follow:           following
//
// A behavior is "blocked" (it waits, with a reason) when it cannot progress
// for now — receiver full, no wreckage, battery full, no free tile — and
// "failed" (it is removed) when it can never continue — well or receiver
// unavailable, well depleted, leader gone.
package automation

import (
	"fmt"
	"slices"
	"strconv"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "automation"

// Events (owner only; automation.work is admin only).
const (
	Phase         = "automation"
	EventState    = "automation.state"
	EventBlocked  = "automation.blocked"
	EventFailed   = "automation.failed"
	EventWork     = "automation.work"
	MaxRadius     = 16
	MaxFollowDist = 8
)

// Command types.
var (
	CommandAutomate = string((&automationv1.AutomateCommand{}).ProtoReflect().Descriptor().FullName())
	CommandCancel   = string((&automationv1.CancelAutomationCommand{}).ProtoReflect().Descriptor().FullName())
)

// Behavior kinds.
const (
	kindMine uint8 = iota + 1
	kindSalvage
	kindGuard
	kindFollow
)

var kindNames = map[uint8]string{kindMine: "mine_and_deliver", kindSalvage: "auto_salvage", kindGuard: "guard", kindFollow: "follow"}

// States.
const (
	stNew uint8 = iota
	stToWell
	stMining
	stToReceiver
	stDelivering
	stSearching
	stApproaching
	stCollecting
	stHolding
	stEngaging
	stFollowing
)

var stateNames = map[uint8]string{
	stNew: "starting", stToWell: "to_well", stMining: "mining", stToReceiver: "to_receiver", stDelivering: "delivering",
	stSearching: "searching", stApproaching: "approaching", stCollecting: "collecting",
	stHolding: "holding", stEngaging: "engaging", stFollowing: "following",
}

// task is a unit's automation state (comparable, hashed and replayed with
// the world).
type task struct {
	Kind, State uint8
	Blocked     bool
	Reason      string
	A, B        sim.EntityID // well & receiver; leader
	Pos         sim.Pos      // guard position
	N1, N2      int64        // deliver_at & keep; radius; distance
	Target      sim.EntityID // current objective (wreck, enemy)
}

var tasks = sim.NewComponent[task]("automation.task")

// Config is the plugin's configuration schema (the "automation" section).
type Config struct {
	EvalInterval          int `json:"eval_interval"`
	MaxEvaluationsPerTick int `json:"max_evaluations_per_tick"`
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg    Config
	combat *combat.Plugin
	// Detected reports whether a player currently detects an entity (fog
	// of war); nil means everything is observable.
	Detected func(w *sim.World, player sim.PlayerID, id sim.EntityID) bool
}

// New returns the automation plugin.
func New(c *combat.Plugin) *Plugin { return &Plugin{combat: c} }

func (*Plugin) Name() string { return Name }

// DependsOn lists every mechanic it drives (and energy for phase order).
func (*Plugin) DependsOn() []string {
	return []string{energy.Name, movement.Name, combat.Name, salvage.Name, wells.Name}
}

func idv(id sim.EntityID) sim.Value { return sim.StringValue(strconv.FormatUint(uint64(id), 10)) }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{EvalInterval: 5, MaxEvaluationsPerTick: 2000}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.EvalInterval < 1 || p.cfg.EvalInterval > 100 || p.cfg.MaxEvaluationsPerTick < 1 {
		return fmt.Errorf("automation: eval_interval must be 1..100 and max_evaluations_per_tick >= 1")
	}
	if err := r.Component(tasks.Def(sim.VisibleOwner, func(t task) []sim.Field {
		status := "active"
		if t.Blocked {
			status = "blocked"
		}
		f := []sim.Field{
			{Key: "automation.behavior", Value: sim.StringValue(kindNames[t.Kind])},
			{Key: "automation.state", Value: sim.StringValue(stateNames[t.State])},
			{Key: "automation.status", Value: sim.StringValue(status)},
		}
		if t.Reason != "" {
			f = append(f, sim.Field{Key: "automation.reason", Value: sim.StringValue(t.Reason)})
		}
		switch t.Kind {
		case kindMine:
			f = append(f, sim.Field{Key: "automation.well", Value: idv(t.A)}, sim.Field{Key: "automation.receiver", Value: idv(t.B)})
		case kindGuard:
			f = append(f, sim.Field{Key: "automation.position", Value: sim.PosValue(t.Pos)}, sim.Field{Key: "automation.radius", Value: sim.IntValue(t.N1)})
		case kindFollow:
			f = append(f, sim.Field{Key: "automation.leader", Value: idv(t.A)})
		case kindSalvage:
			f = append(f, sim.Field{Key: "automation.radius", Value: sim.IntValue(t.N1)})
		}
		if t.Target != 0 {
			f = append(f, sim.Field{Key: "automation.target", Value: idv(t.Target)})
		}
		return f
	})); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase, Before: []string{energy.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "automation.evaluate", Phase: Phase, Run: p.evaluate}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config":    p.cfg,
		"behaviors": []string{"mine_and_deliver", "auto_salvage", "guard", "follow"},
		"limits":    map[string]int{"max_radius": MaxRadius, "max_follow_distance": MaxFollowDist},
		"timing":    "first phase of each tick; unit evaluated when (id + tick) % eval_interval == 0, at most max_evaluations_per_tick per tick",
	}); err != nil {
		return err
	}
	if err := r.Command(sim.CommandDef{Type: CommandAutomate, Execute: p.execAutomate, ApplyToUnit: p.applyAutomate}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandCancel, Execute: p.execCancel, ApplyToUnit: p.applyCancel})
}

func (p *Plugin) sees(w *sim.World, owner sim.PlayerID, id sim.EntityID) bool {
	return p.Detected == nil || p.Detected(w, owner, id)
}

func ownUnit(w *sim.World, owner sim.PlayerID, id sim.EntityID) (sim.Entity, bool) {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit || e.Owner != owner {
		return e, false
	}
	_, hasStore := energy.GetStore(w, id)
	return e, hasStore
}

// ---- commands -----------------------------------------------------------------

// plan validates a behavior for one unit and returns its initial task.
func (p *Plugin) plan(w *sim.World, issuer sim.Principal, m *automationv1.AutomateCommand, unit sim.EntityID) (task, error) {
	u, ok := w.Entity(unit)
	if !ok || u.Kind != sim.KindUnit || (!issuer.Admin && u.Owner != issuer.Player) {
		return task{}, sim.Unavailable("unit", unit)
	}
	owner := u.Owner
	switch b := m.GetBehavior().(type) {
	case *automationv1.AutomateCommand_MineAndDeliver:
		well, recv := sim.EntityID(b.MineAndDeliver.GetWellId()), sim.EntityID(b.MineAndDeliver.GetReceiverId())
		if _, ok := wells.Get(w, well); !ok || !p.sees(w, owner, well) {
			return task{}, sim.Unavailable("well", well)
		}
		if _, ok := ownUnit(w, owner, recv); !ok || recv == unit {
			return task{}, sim.Unavailable("receiver", recv)
		}
		if b.MineAndDeliver.GetDeliverAt() < 0 || b.MineAndDeliver.GetKeep() < 0 {
			return task{}, sim.Fail(sim.CodeInvalidArgument, "deliver_at and keep must be >= 0")
		}
		return task{Kind: kindMine, A: well, B: recv, N1: b.MineAndDeliver.GetDeliverAt(), N2: b.MineAndDeliver.GetKeep()}, nil
	case *automationv1.AutomateCommand_AutoSalvage:
		r := int64(b.AutoSalvage.GetRadius())
		if r < 1 || r > MaxRadius {
			return task{}, sim.Fail(sim.CodeInvalidArgument, "radius must be 1..%d", MaxRadius)
		}
		return task{Kind: kindSalvage, N1: r}, nil
	case *automationv1.AutomateCommand_Guard:
		r := int64(b.Guard.GetRadius())
		if r < 0 || r > MaxRadius || b.Guard.GetPosition() == nil {
			return task{}, sim.Fail(sim.CodeInvalidArgument, "position and radius 0..%d required", MaxRadius)
		}
		pos := sim.Pos{X: b.Guard.GetPosition().GetX(), Y: b.Guard.GetPosition().GetY()}
		if !w.InBounds(pos) {
			return task{}, sim.Fail(sim.CodeInvalidArgument, "guard position %v outside the world", pos)
		}
		if a, _ := p.combat.Capability(w, unit); a <= 0 {
			return task{}, sim.Fail(sim.CodeFailedPrecondition, "unit %d cannot attack", unit)
		}
		return task{Kind: kindGuard, Pos: pos, N1: r}, nil
	case *automationv1.AutomateCommand_Follow:
		leader := sim.EntityID(b.Follow.GetLeaderId())
		d := int64(b.Follow.GetDistance())
		if d < 1 || d > MaxFollowDist {
			return task{}, sim.Fail(sim.CodeInvalidArgument, "distance must be 1..%d", MaxFollowDist)
		}
		if _, ok := ownUnit(w, owner, leader); !ok || leader == unit {
			return task{}, sim.Unavailable("leader", leader)
		}
		return task{Kind: kindFollow, A: leader, N1: d}, nil
	}
	return task{}, sim.Fail(sim.CodeInvalidArgument, "behavior required")
}

// stopActivities clears the orders automation drives.
func stopActivities(w *sim.World, id sim.EntityID) {
	movement.ClearDestination(w, id)
	energy.StopMining(w, id)
	salvage.StopSalvage(w, id)
	combat.ClearTarget(w, id)
}

func (p *Plugin) execAutomate(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*automationv1.AutomateCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want AutomateCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > 4096 {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..4096 units, got %d", len(ids))
	}
	seen := map[uint64]bool{}
	plans := make([]task, len(ids))
	for i, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		t, err := p.plan(ctx.World, cmd.Issuer, m, sim.EntityID(id))
		if err != nil {
			return err
		}
		plans[i] = t
	}
	if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil {
		return err
	}
	for i, id := range ids {
		stopActivities(ctx.World, sim.EntityID(id))
		tasks.Set(ctx.World, sim.EntityID(id), plans[i])
	}
	return nil
}

func (p *Plugin) applyAutomate(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*automationv1.AutomateCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want AutomateCommand", cmd.Payload)
	}
	t, err := p.plan(ctx.World, cmd.Issuer, m, unit)
	if err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	stopActivities(ctx.World, unit)
	tasks.Set(ctx.World, unit, t)
	return nil
}

func (p *Plugin) cancelOne(w *sim.World, issuer sim.Principal, unit sim.EntityID) error {
	u, ok := w.Entity(unit)
	if !ok || u.Kind != sim.KindUnit || (!issuer.Admin && u.Owner != issuer.Player) {
		return sim.Unavailable("unit", unit)
	}
	return nil
}

func (p *Plugin) execCancel(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*automationv1.CancelAutomationCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CancelAutomationCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > 4096 {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..4096 units, got %d", len(ids))
	}
	for _, id := range ids {
		if err := p.cancelOne(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil {
		return err
	}
	for _, id := range ids {
		if tasks.Has(ctx.World, sim.EntityID(id)) {
			tasks.Remove(ctx.World, sim.EntityID(id))
			stopActivities(ctx.World, sim.EntityID(id))
		}
	}
	return nil
}

func (p *Plugin) applyCancel(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	if err := p.cancelOne(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	if tasks.Has(ctx.World, unit) {
		tasks.Remove(ctx.World, unit)
		stopActivities(ctx.World, unit)
	}
	return nil
}

// ---- evaluation -----------------------------------------------------------------

func cheb(a, b sim.Pos) int64 {
	dx, dy := int64(a.X-b.X), int64(a.Y-b.Y)
	return max(dx, -dx, dy, -dy)
}

func (p *Plugin) evaluate(ctx *sim.TickContext) {
	w := ctx.World
	evaluated, deferred := 0, 0
	for _, id := range tasks.Entities(w) {
		t, _ := tasks.Get(w, id)
		due := t.State == stNew || (uint64(id)+uint64(ctx.Tick))%uint64(p.cfg.EvalInterval) == 0
		if !due {
			continue
		}
		if evaluated >= p.cfg.MaxEvaluationsPerTick {
			deferred++
			continue
		}
		evaluated++
		u, ok := w.Entity(id)
		if !ok {
			continue
		}
		before := t
		next, fail := p.step(ctx, u, t)
		switch {
		case fail != "":
			tasks.Remove(w, id)
			stopActivities(w, id)
			ctx.Emit(sim.Event{Type: EventFailed, Entity: id, Audience: sim.ToPlayer(u.Owner), Fields: []sim.Field{
				{Key: "behavior", Value: sim.StringValue(kindNames[t.Kind])},
				{Key: "reason", Value: sim.StringValue(fail)},
			}})
			continue
		case next.Blocked && (!before.Blocked || before.Reason != next.Reason):
			ctx.Emit(sim.Event{Type: EventBlocked, Entity: id, Audience: sim.ToPlayer(u.Owner), Fields: []sim.Field{
				{Key: "behavior", Value: sim.StringValue(kindNames[t.Kind])},
				{Key: "reason", Value: sim.StringValue(next.Reason)},
			}})
		case next.State != before.State:
			ctx.Emit(sim.Event{Type: EventState, Entity: id, Audience: sim.ToPlayer(u.Owner), Fields: []sim.Field{
				{Key: "behavior", Value: sim.StringValue(kindNames[t.Kind])},
				{Key: "state", Value: sim.StringValue(stateNames[next.State])},
			}})
		}
		tasks.Set(w, id, next)
	}
	if evaluated+deferred > 0 {
		ctx.Emit(sim.Event{Type: EventWork, Audience: sim.AdminAudience(), Fields: []sim.Field{
			{Key: "evaluated", Value: sim.IntValue(int64(evaluated))},
			{Key: "deferred", Value: sim.IntValue(int64(deferred))},
		}})
	}
}

func blocked(t task, reason string) task {
	t.Blocked, t.Reason = true, reason
	return t
}

func active(t task, state uint8) task {
	t.State, t.Blocked, t.Reason = state, false, ""
	return t
}

// goNear sends a unit next to (or onto, for passable targets) a target.
func goNear(w *sim.World, u sim.Entity, target sim.Pos) bool {
	dest, ok := movement.FreeTileNear(w, target, u.Pos, u.ID)
	if !ok {
		return false
	}
	movement.SetDestination(w, u.ID, dest)
	return true
}

// step advances one task. It returns the next state, or a failure reason.
func (p *Plugin) step(ctx *sim.TickContext, u sim.Entity, t task) (task, string) {
	switch t.Kind {
	case kindMine:
		return p.stepMine(ctx.World, u, t)
	case kindSalvage:
		return p.stepSalvage(ctx.World, u, t), ""
	case kindGuard:
		return p.stepGuard(ctx.World, u, t), ""
	case kindFollow:
		return p.stepFollow(ctx.World, u, t)
	}
	return t, "unknown_behavior"
}

func (p *Plugin) stepMine(w *sim.World, u sim.Entity, t task) (task, string) {
	wellEnt, ok := w.Entity(t.A)
	wv, isWell := wells.Get(w, t.A)
	if !ok || !isWell || !p.sees(w, u.Owner, t.A) {
		return t, "well_unavailable"
	}
	recv, ok := ownUnit(w, u.Owner, t.B)
	if !ok {
		return t, "receiver_unavailable"
	}
	s, _ := energy.GetStore(w, u.ID)
	deliverAt := s.Capacity
	if t.N1 > 0 {
		deliverAt = min(t.N1, s.Capacity)
	}
	switch t.State {
	case stNew, stToWell:
		if cheb(u.Pos, wellEnt.Pos) <= 1 {
			movement.ClearDestination(w, u.ID)
			energy.SetMining(w, u.ID, t.A)
			return active(t, stMining), ""
		}
		if !goNear(w, u, wellEnt.Pos) {
			return blocked(active(t, stToWell), "well_crowded"), ""
		}
		return active(t, stToWell), ""
	case stMining:
		if !wv.Unlimited && wv.Energy == 0 {
			return t, "well_depleted"
		}
		if s.Energy >= deliverAt {
			energy.StopMining(w, u.ID)
			return p.stepMine(w, u, active(t, stToReceiver))
		}
		if cheb(u.Pos, wellEnt.Pos) > 1 {
			return active(t, stToWell), ""
		}
		energy.SetMining(w, u.ID, t.A)
		return active(t, stMining), ""
	case stToReceiver:
		if cheb(u.Pos, recv.Pos) <= 1 {
			movement.ClearDestination(w, u.ID)
			if amount := s.Energy - t.N2; amount > 0 {
				energy.SetTransfer(w, u.ID, recv.ID, amount)
			}
			return active(t, stDelivering), ""
		}
		if !goNear(w, u, recv.Pos) {
			return blocked(active(t, stToReceiver), "receiver_crowded"), ""
		}
		return active(t, stToReceiver), ""
	case stDelivering:
		if energy.Transferring(w, u.ID) {
			return t, ""
		}
		if s.Energy <= t.N2 {
			return p.stepMine(w, u, active(t, stToWell))
		}
		if rs, _ := energy.GetStore(w, recv.ID); rs.Free() == 0 {
			return blocked(t, "receiver_full"), ""
		}
		return p.stepMine(w, u, active(t, stToReceiver)) // moved away: chase
	}
	return t, "bad_state"
}

func (p *Plugin) stepSalvage(w *sim.World, u sim.Entity, t task) task {
	s, _ := energy.GetStore(w, u.ID)
	if s.Free() == 0 {
		salvage.StopSalvage(w, u.ID)
		return blocked(t, "battery_full")
	}
	valid := func(id sim.EntityID) (sim.Entity, bool) {
		e, ok := w.Entity(id)
		if !ok || !p.sees(w, u.Owner, id) {
			return e, false
		}
		wk, isWreck := salvage.GetWreck(w, id)
		return e, isWreck && wk.Energy > 0
	}
	target, ok := valid(t.Target)
	if t.Target == 0 || !ok {
		// Search: the nearest observable wreck within radius (ties by ID).
		t.Target = 0
		var bestD int64
		for _, id := range salvage.Wrecks(w) {
			e, ok := valid(id)
			if !ok {
				continue
			}
			if d := cheb(u.Pos, e.Pos); d <= t.N1 && (t.Target == 0 || d < bestD) {
				t.Target, bestD, target = id, d, e
			}
		}
		if t.Target == 0 {
			salvage.StopSalvage(w, u.ID)
			return blocked(active(t, stSearching), "no_wreckage")
		}
	}
	if cheb(u.Pos, target.Pos) <= 1 {
		movement.ClearDestination(w, u.ID)
		salvage.SetSalvage(w, u.ID, t.Target)
		return active(t, stCollecting)
	}
	if !goNear(w, u, target.Pos) {
		return blocked(active(t, stApproaching), "wreck_crowded")
	}
	return active(t, stApproaching)
}

func (p *Plugin) stepGuard(w *sim.World, u sim.Entity, t task) task {
	attack, rng := p.combat.Capability(w, u.ID)
	if attack <= 0 {
		combat.ClearTarget(w, u.ID)
		return blocked(t, "cannot_attack")
	}
	// Threats: detected hostile units within radius of the guard position.
	var best sim.Entity
	var bestD int64 = -1
	r := t.N1
	for y := t.Pos.Y - int32(r); y <= t.Pos.Y+int32(r); y++ {
		for x := t.Pos.X - int32(r); x <= t.Pos.X+int32(r); x++ {
			id, ok := w.EntityAt(sim.Pos{X: x, Y: y})
			if !ok {
				continue
			}
			e, _ := w.Entity(id)
			if e.Kind != sim.KindUnit || e.Owner == u.Owner || !p.sees(w, u.Owner, id) {
				continue
			}
			if d := cheb(u.Pos, e.Pos); bestD < 0 || d < bestD || (d == bestD && id < best.ID) {
				best, bestD = e, d
			}
		}
	}
	if bestD < 0 {
		combat.ClearTarget(w, u.ID)
		t.Target = 0
		if u.Pos != t.Pos {
			if id, taken := w.EntityAt(t.Pos); taken && id != u.ID {
				goNear(w, u, t.Pos)
			} else {
				movement.SetDestination(w, u.ID, t.Pos)
			}
		}
		return active(t, stHolding)
	}
	t.Target = best.ID
	combat.SetTarget(w, u.ID, best.ID)
	if bestD > rng {
		// Close in, but never leave the guarded area.
		if dest, ok := movement.FreeTileNear(w, best.Pos, u.Pos, u.ID); ok && cheb(dest, t.Pos) <= r {
			movement.SetDestination(w, u.ID, dest)
		}
	} else {
		movement.ClearDestination(w, u.ID)
	}
	return active(t, stEngaging)
}

func (p *Plugin) stepFollow(w *sim.World, u sim.Entity, t task) (task, string) {
	leader, ok := ownUnit(w, u.Owner, t.A)
	if !ok {
		return t, "leader_unavailable"
	}
	if cheb(u.Pos, leader.Pos) <= t.N1 {
		movement.ClearDestination(w, u.ID)
		return active(t, stFollowing), ""
	}
	if !goNear(w, u, leader.Pos) {
		return blocked(active(t, stFollowing), "leader_crowded"), ""
	}
	return active(t, stFollowing), ""
}

// Tasks returns automated units, ascending (for tests and tools).
func Tasks(w *sim.World) []sim.EntityID { return slices.Clone(tasks.Entities(w)) }
