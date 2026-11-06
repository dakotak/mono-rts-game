// Package logistics implements persistent supply links between a player's
// own units: ranged, rate-limited, reserve-respecting energy transfers that
// chain into relays (see proto/rts/logistics/v1/logistics.proto for the
// player-facing rules).
//
// # Allocation (phase "logistics", after energy, every tick)
//
// All amounts are computed from the energy every unit holds at the start
// of the phase, then applied at once, so simultaneous transfers do not
// depend on processing order and energy advances at most one link per
// tick:
//
//  1. Each link wants min(receiver fill level - receiver energy, ...) when
//     in range and not resting; resting links (receiver reached fill_to)
//     want nothing until the receiver drops below resume_below.
//  2. Each supplier offers min(transfer rate, energy - reserve) and splits
//     it max-min fairly over its links' wants (ties by receiver ID).
//  3. Each receiver accepts at most its free capacity, split max-min fairly
//     over its incoming grants (ties by supplier ID).
//  4. Every grant is moved: supplier -= g, receiver += g.
//
// Since a supplier sends at most energy - reserve and a receiver accepts at
// most capacity - energy (both at phase start), no store goes negative or
// over capacity, and the sum of all stores is unchanged.
package logistics

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"

	logisticsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/logistics/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/fairshare"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "logistics"

// Identifiers.
const (
	Phase       = "logistics"
	EventStatus = "logistics.status" // owner; entity = supplier; fields receiver, status
	EventEnded  = "logistics.ended"  // owner; entity = supplier; fields receiver, reason
	EventWork   = "logistics.work"   // admin; fields links, moved
	MaxLinks    = 4
	MaxRange    = 16
)

// Command types.
var (
	CommandSupply = string((&logisticsv1.SupplyCommand{}).ProtoReflect().Descriptor().FullName())
	CommandCancel = string((&logisticsv1.CancelSupplyCommand{}).ProtoReflect().Descriptor().FullName())
)

// Link statuses.
const (
	stPending uint8 = iota
	stSupplying
	stSatisfied
	stReserve
	stReceiverFull
	stOutOfRange
)

var statusNames = [...]string{"pending", "supplying", "satisfied", "reserve", "receiver_full", "out_of_range"}

type link struct {
	To             sim.EntityID
	FillTo, Resume int64
	Resting        bool
	Status         uint8
}

// order is a supplier's set of links (sorted by receiver ID).
type order struct {
	Reserve int64
	N       uint8
	Links   [MaxLinks]link
}

var orders = sim.NewComponent[order]("logistics.supply")

// Stat binds a value to an attributes property, or a constant.
type Stat struct {
	Property string `json:"property,omitempty"`
	Default  int64  `json:"default"`
}

// Config is the plugin's configuration schema (the "logistics" section).
type Config struct {
	// Range is the supplier's transfer range (Chebyshev), 1..16.
	Range Stat `json:"range"`
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg   Config
	attrs *attributes.Plugin

	// scratch, reused every tick
	entries []entry
	work    []work
	claims  []fairshare.Claim
	delta   map[sim.EntityID]int64
}

type entry struct {
	from, to sim.EntityID
	wi, li   int
	want     int64
	offered  int64 // stage-1 grant
	grant    int64
}

type work struct {
	id      sim.EntityID
	owner   sim.PlayerID
	orig, o order
}

// New returns the logistics plugin.
func New(a *attributes.Plugin) *Plugin { return &Plugin{attrs: a} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return []string{energy.Name, attributes.Name} }

func idv(id sim.EntityID) sim.Value { return sim.StringValue(strconv.FormatUint(uint64(id), 10)) }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = Config{Range: Stat{Default: 1}}
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.Range.Default < 1 || p.cfg.Range.Default > MaxRange {
		return fmt.Errorf("logistics range.default must be 1..%d", MaxRange)
	}
	if p.cfg.Range.Property != "" && !p.attrs.Has(p.cfg.Range.Property) {
		return fmt.Errorf("logistics range.property: %q is not an attribute or derived property", p.cfg.Range.Property)
	}
	if err := r.Component(orders.Def(sim.VisibleOwner, func(o order) []sim.Field {
		f := []sim.Field{{Key: "logistics.reserve", Value: sim.IntValue(o.Reserve)}}
		for i := range o.N {
			l := o.Links[i]
			k := "logistics.supply." + strconv.Itoa(int(i))
			f = append(f,
				sim.Field{Key: k + ".to", Value: idv(l.To)},
				sim.Field{Key: k + ".status", Value: sim.StringValue(statusNames[l.Status])},
			)
		}
		return f
	})); err != nil {
		return err
	}
	if err := r.Derived(sim.DerivedDef{Name: "logistics.range", Compute: func(w *sim.World, id sim.EntityID) (int64, bool) {
		_, ok := energy.GetStore(w, id)
		return p.rangeOf(w, id), ok
	}}); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{energy.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "logistics.supply", Phase: Phase, Run: p.supply}); err != nil {
		return err
	}
	if err := r.Describe(map[string]any{
		"config":     p.cfg,
		"max_links":  MaxLinks,
		"allocation": "start-of-phase snapshot; supplier offer min(rate, energy-reserve) split max-min over links; receiver free capacity split max-min over suppliers",
	}); err != nil {
		return err
	}
	if err := r.Command(sim.CommandDef{Type: CommandSupply, Execute: p.execSupply, ApplyToUnit: p.applySupply}); err != nil {
		return err
	}
	return r.Command(sim.CommandDef{Type: CommandCancel, Execute: p.execCancel, ApplyToUnit: p.applyCancel})
}

func (p *Plugin) rangeOf(w *sim.World, id sim.EntityID) int64 {
	if p.cfg.Range.Property != "" {
		v, _ := p.attrs.Lookup(w, id, p.cfg.Range.Property)
		return min(max(v, 1), MaxRange)
	}
	return p.cfg.Range.Default
}

// ---- commands ---------------------------------------------------------------

func unitOf(w *sim.World, issuer sim.Principal, id sim.EntityID) (sim.Entity, error) {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit || (!issuer.Admin && e.Owner != issuer.Player) {
		return e, sim.Unavailable("unit", id)
	}
	if _, ok := energy.GetStore(w, id); !ok {
		return e, sim.Unavailable("unit", id)
	}
	return e, nil
}

// reaches reports whether energy can flow from start to any of targets
// along existing links (plus pending ones).
func reaches(w *sim.World, start sim.EntityID, targets map[sim.EntityID]bool, extra map[sim.EntityID][]sim.EntityID) bool {
	seen := map[sim.EntityID]bool{start: true}
	stack := []sim.EntityID{start}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if targets[n] {
			return true
		}
		o, _ := orders.Get(w, n)
		next := extra[n]
		for i := range o.N {
			next = append(next, o.Links[i].To)
		}
		for _, m := range next {
			if !seen[m] {
				seen[m] = true
				stack = append(stack, m)
			}
		}
	}
	return false
}

// validate checks a supply request for a set of suppliers.
func (p *Plugin) validate(w *sim.World, issuer sim.Principal, m *logisticsv1.SupplyCommand, units []sim.EntityID) error {
	if m.GetReserve() < 0 || m.GetFillTo() < 0 || m.GetResumeBelow() < 0 {
		return sim.Fail(sim.CodeInvalidArgument, "reserve, fill_to and resume_below must be >= 0")
	}
	if m.GetFillTo() > 0 && m.GetResumeBelow() > m.GetFillTo() {
		return sim.Fail(sim.CodeInvalidArgument, "resume_below must not exceed fill_to")
	}
	recvID := sim.EntityID(m.GetReceiverId())
	recv, ok := w.Entity(recvID)
	targets := map[sim.EntityID]bool{}
	for _, u := range units {
		e, err := unitOf(w, issuer, u)
		if err != nil {
			return err
		}
		if u == recvID {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d cannot supply itself", u)
		}
		if ok && recv.Owner != e.Owner {
			return sim.Unavailable("receiver", recvID)
		}
		o, _ := orders.Get(w, u)
		if o.N == MaxLinks && find(o, recvID) < 0 {
			return sim.Fail(sim.CodeFailedPrecondition, "unit %d already supplies %d receivers", u, MaxLinks)
		}
		targets[u] = true
	}
	if !ok || recv.Kind != sim.KindUnit {
		return sim.Unavailable("receiver", recvID)
	}
	if _, has := energy.GetStore(w, recvID); !has {
		return sim.Unavailable("receiver", recvID)
	}
	if reaches(w, recvID, targets, nil) {
		return sim.Fail(sim.CodeFailedPrecondition, "supply link to %d would form a cycle", recvID)
	}
	return nil
}

func find(o order, to sim.EntityID) int {
	for i := range o.N {
		if o.Links[i].To == to {
			return int(i)
		}
	}
	return -1
}

func (p *Plugin) add(w *sim.World, u sim.EntityID, m *logisticsv1.SupplyCommand) {
	o, _ := orders.Get(w, u)
	o.Reserve = m.GetReserve()
	l := link{To: sim.EntityID(m.GetReceiverId()), FillTo: m.GetFillTo(), Resume: m.GetResumeBelow()}
	if i := find(o, l.To); i >= 0 {
		o.Links[i] = l
	} else {
		o.Links[o.N] = l
		o.N++
		s := o.Links[:o.N]
		slices.SortFunc(s, func(a, b link) int { return cmp.Compare(a.To, b.To) })
	}
	orders.Set(w, u, o)
}

func (p *Plugin) execSupply(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*logisticsv1.SupplyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SupplyCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > energy.MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", energy.MaxUnitsPerCommand, len(ids))
	}
	units := make([]sim.EntityID, len(ids))
	seen := map[uint64]bool{}
	for i, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		units[i] = sim.EntityID(id)
	}
	if err := p.validate(ctx.World, cmd.Issuer, m, units); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, len(units)); err != nil {
		return err
	}
	for _, u := range units {
		p.add(ctx.World, u, m)
	}
	return nil
}

func (p *Plugin) applySupply(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*logisticsv1.SupplyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SupplyCommand", cmd.Payload)
	}
	if err := p.validate(ctx.World, cmd.Issuer, m, []sim.EntityID{unit}); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	p.add(ctx.World, unit, m)
	return nil
}

func remove(w *sim.World, u, to sim.EntityID) {
	o, ok := orders.Get(w, u)
	if !ok {
		return
	}
	if to == 0 {
		orders.Remove(w, u)
		return
	}
	i := find(o, to)
	if i < 0 {
		return
	}
	copy(o.Links[i:], o.Links[i+1:o.N])
	o.N--
	o.Links[o.N] = link{}
	if o.N == 0 {
		orders.Remove(w, u)
		return
	}
	orders.Set(w, u, o)
}

func (p *Plugin) execCancel(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*logisticsv1.CancelSupplyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CancelSupplyCommand", cmd.Payload)
	}
	ids := m.GetUnitIds()
	if len(ids) == 0 || len(ids) > energy.MaxUnitsPerCommand {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..%d units, got %d", energy.MaxUnitsPerCommand, len(ids))
	}
	for _, id := range ids {
		if _, err := unitOf(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil {
		return err
	}
	for _, id := range ids {
		remove(ctx.World, sim.EntityID(id), sim.EntityID(m.GetReceiverId()))
	}
	return nil
}

func (p *Plugin) applyCancel(ctx *sim.TickContext, cmd sim.Command, unit sim.EntityID) error {
	m, ok := cmd.Payload.(*logisticsv1.CancelSupplyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want CancelSupplyCommand", cmd.Payload)
	}
	if _, err := unitOf(ctx.World, cmd.Issuer, unit); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	remove(ctx.World, unit, sim.EntityID(m.GetReceiverId()))
	return nil
}

// ---- simulation ---------------------------------------------------------------

func cheb(a, b sim.Pos) int64 {
	dx, dy := int64(a.X-b.X), int64(a.Y-b.Y)
	return max(dx, -dx, dy, -dy)
}

func (p *Plugin) supply(ctx *sim.TickContext) {
	w := ctx.World
	suppliers := orders.Entities(w)
	if len(suppliers) == 0 {
		return
	}
	ws := p.work[:0]
	es := p.entries[:0]
	// Stage 0: drop links to lost receivers; compute wants and resting.
	for _, sid := range suppliers {
		o, _ := orders.Get(w, sid)
		from, _ := w.Entity(sid)
		wi := work{id: sid, owner: from.Owner, orig: o}
		rng := p.rangeOf(w, sid)
		for i := 0; i < int(o.N); {
			l := &o.Links[i]
			to, ok := w.Entity(l.To)
			rs, hasStore := energy.GetStore(w, l.To)
			if !ok || !hasStore || to.Owner != from.Owner {
				ctx.Emit(sim.Event{Type: EventEnded, Entity: sid, Audience: sim.ToPlayer(from.Owner), Fields: []sim.Field{
					{Key: "receiver", Value: idv(l.To)},
					{Key: "reason", Value: sim.StringValue("receiver_lost")},
				}})
				copy(o.Links[i:], o.Links[i+1:o.N])
				o.N--
				o.Links[o.N] = link{}
				continue
			}
			fill := rs.Capacity
			if l.FillTo > 0 {
				fill = min(l.FillTo, rs.Capacity)
			}
			resume := fill
			if l.Resume > 0 {
				resume = min(l.Resume, fill)
			}
			if rs.Energy >= fill {
				l.Resting = true
			} else if rs.Energy < resume {
				l.Resting = false
			}
			switch {
			case cheb(from.Pos, to.Pos) > rng:
				l.Status = stOutOfRange
			case l.Resting:
				l.Status = stSatisfied
			case rs.Free() == 0:
				l.Status = stReceiverFull
			default:
				l.Status = stPending
				es = append(es, entry{from: sid, to: l.To, wi: len(ws), li: i, want: fill - rs.Energy})
			}
			i++
		}
		wi.o = o
		ws = append(ws, wi)
	}
	// Stage 1: each supplier splits its offer over its wanting links.
	// Entries are grouped by supplier (ascending) and receiver (ascending).
	for i := 0; i < len(es); {
		j := i
		for j < len(es) && es[j].from == es[i].from {
			j++
		}
		s, _ := energy.GetStore(w, es[i].from)
		rate, _ := w.Derived(energy.DerivedTransferRate, es[i].from)
		offer := max(0, min(rate, s.Energy-ws[es[i].wi].o.Reserve))
		p.claims = p.claims[:0]
		for k := i; k < j; k++ {
			p.claims = append(p.claims, fairshare.Claim{ID: uint64(es[k].to), Want: es[k].want})
		}
		for k, g := range fairshare.Split(p.claims, offer) {
			es[i+k].grant, es[i+k].offered = g, g
		}
		i = j
	}
	// Stage 2: each receiver splits its free capacity over incoming grants.
	slices.SortFunc(es, func(a, b entry) int { return cmp.Or(cmp.Compare(a.to, b.to), cmp.Compare(a.from, b.from)) })
	for i := 0; i < len(es); {
		j := i
		for j < len(es) && es[j].to == es[i].to {
			j++
		}
		rs, _ := energy.GetStore(w, es[i].to)
		p.claims = p.claims[:0]
		for k := i; k < j; k++ {
			p.claims = append(p.claims, fairshare.Claim{ID: uint64(es[k].from), Want: es[k].grant})
		}
		for k, g := range fairshare.Split(p.claims, rs.Free()) {
			es[i+k].grant = g
		}
		i = j
	}
	// Stage 3: apply. Deltas were all computed from the phase-start
	// snapshot; summing per unit first keeps the result order-free.
	var moved int64
	if p.delta == nil {
		p.delta = map[sim.EntityID]int64{}
	}
	clear(p.delta)
	for _, e := range es {
		p.delta[e.from] -= e.grant
		p.delta[e.to] += e.grant
		moved += e.grant
		l := &ws[e.wi].o.Links[e.li]
		switch {
		case e.grant > 0:
			l.Status = stSupplying
		case e.offered == 0:
			l.Status = stReserve
		default:
			l.Status = stReceiverFull
		}
	}
	for _, e := range es {
		if d := p.delta[e.to]; d != 0 {
			s, _ := energy.GetStore(w, e.to)
			energy.SetStored(w, e.to, s.Energy+d)
			p.delta[e.to] = 0
		}
		if d := p.delta[e.from]; d != 0 {
			s, _ := energy.GetStore(w, e.from)
			energy.SetStored(w, e.from, s.Energy+d)
			p.delta[e.from] = 0
		}
	}
	// Write back changed orders; report status transitions.
	for _, wi := range ws {
		if wi.o == wi.orig {
			continue
		}
		if wi.o.N == 0 {
			orders.Remove(w, wi.id)
			continue
		}
		orders.Set(w, wi.id, wi.o)
		for i := range wi.o.N {
			l := wi.o.Links[i]
			if j := find(wi.orig, l.To); j >= 0 && wi.orig.Links[j].Status == l.Status {
				continue
			}
			ctx.Emit(sim.Event{Type: EventStatus, Entity: wi.id, Audience: sim.ToPlayer(wi.owner), Fields: []sim.Field{
				{Key: "receiver", Value: idv(l.To)},
				{Key: "status", Value: sim.StringValue(statusNames[l.Status])},
			}})
		}
	}
	p.entries, p.work = es[:0], ws[:0]
	if len(es) > 0 {
		ctx.Emit(sim.Event{Type: EventWork, Audience: sim.AdminAudience(), Fields: []sim.Field{
			{Key: "links", Value: sim.IntValue(int64(len(es)))},
			{Key: "moved", Value: sim.IntValue(moved)},
		}})
	}
}

// Link is a supply relationship (for admin inspection and tools).
type Link struct {
	From, To       sim.EntityID
	Reserve        int64
	FillTo, Resume int64
	Status         string
}

// Links returns every supply link, by supplier then receiver.
func Links(w *sim.World) []Link {
	var out []Link
	for _, id := range orders.Entities(w) {
		o, _ := orders.Get(w, id)
		for i := range o.N {
			l := o.Links[i]
			out = append(out, Link{From: id, To: l.To, Reserve: o.Reserve, FillTo: l.FillTo, Resume: l.Resume, Status: statusNames[l.Status]})
		}
	}
	return out
}
