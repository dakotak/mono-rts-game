// Package ai is the computer player's decision system: one modular brain
// whose behavior is shaped by difficulty parameters and a personality.
//
// Layers (each at its own interval from the difficulty table):
//
//   - Strategic: score strategies with utility heuristics, keep one with
//     hysteresis, derive a Directive (population, role mix, investment,
//     attack threshold).
//   - Operational: assign roles, miners to wells (server mine-and-deliver
//     automation), cloning, upgrades and their funding, logistics, guards,
//     salvage, scouting routes.
//   - Tactical: threats, defense, target selection, campaigns, retreats.
//
// The bot reads only its Knowledge (built from the public API) and emits
// Orders through a Commander that respects the server's command budget.
// With a fixed seed it is deterministic: decisions are made on tick
// updates only, randomness is seeded, and compute budgets count operations.
package ai

import (
	"context"
	"fmt"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// Role is a strategic assignment of a unit (not a unit type).
type Role string

const (
	RoleNone     Role = ""
	RoleBank     Role = "bank"     // stores energy, clones, funds upgrades
	RoleMiner    Role = "miner"    // mine-and-deliver automation
	RoleFighter  Role = "fighter"  // army
	RoleScout    Role = "scout"    // exploration
	RoleGuard    Role = "guard"    // guard automation at home
	RoleSalvager Role = "salvager" // auto-salvage automation
)

// Extension reports a role owned by an extension ("ext:<name>"): the
// built-in planners never assign, reassign or command such units.
func (r Role) Extension() bool { return strings.HasPrefix(string(r), "ext:") }

// Config configures a bot.
type Config struct {
	Difficulty  difficulty.Params
	Personality difficulty.Personality
	Seed        uint64
	// Nonce prefixes command IDs (unique per run unless deterministic).
	Nonce  string
	Logger *slog.Logger
	Debug  bool
	// Heuristics and Objectives extend the built-in ones (see extend.go).
	Heuristics []Heuristic
	Objectives []Objective
}

// Bot is one computer player.
type Bot struct {
	P     difficulty.Params
	Pers  difficulty.Personality
	K     *state.Knowledge
	Cmd   *Commander
	log   *slog.Logger
	debug bool
	rng   *rand.Rand

	heuristics []Heuristic
	objectives []Objective

	Roles     map[uint64]Role
	occ       map[state.Pos]bool // occupancy, built once per tick
	occTick   uint64
	lastState rtsv1.MatchState
	monitorAt uint64 // next tick a scout may re-scout the enemy base
	// role index cache (see unitsByRole)
	byRole      map[Role][]*state.Entity
	byRoleFor   **state.Entity
	byRoleN     int
	byRoleDirty bool
	tags        map[Role][]uint64 // role changes to tag this tick
	clonerID    uint64
	clonerNeed  int64
	bankID      uint64
	home        state.Pos
	strategy    strategyState
	dir         Directive
	mining      map[uint64]uint64 // miner -> well
	scouting    map[uint64]state.Pos
	// foraging: units refilling at a well (unit -> well) because they could
	// not make it home.
	foraging map[uint64]uint64
	seenAt   map[uint64]uint64 // enemy -> tick first seen (reaction delay)
	campaign *campaign
	// intel: long-term knowledge of each opponent (intel.go); probe: the
	// unexplored area searched while no opponent has been found.
	intel map[uint32]*enemyIntel
	probe probeState
	// muster: the army is strong enough to attack but not fueled; fighters
	// fill up at the bank before anything else is funded.
	muster bool
	// musterFrom: when the current muster began; musterOff: no new muster
	// before this tick (the last one failed).
	musterFrom, musterOff uint64
	musterFuel            int64 // energy each fighter needs for the march
	readyAt               uint64
	ops                   int // compute operations left in the current cycle

	Status Status
}

// New creates a bot.
func New(k *state.Knowledge, cfg Config) *Bot {
	lg := cfg.Logger
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	b := &Bot{
		P: cfg.Difficulty, Pers: cfg.Personality, K: k, log: lg, debug: cfg.Debug,
		rng:        rand.New(rand.NewPCG(cfg.Seed, 0x6d6f6e6f)),
		heuristics: append(builtinHeuristics(), cfg.Heuristics...),
		objectives: append(builtinObjectives(), cfg.Objectives...),
		Cmd:        newCommander(k.Budget, cfg.Nonce),
	}
	b.resetMatch()
	return b
}

func (b *Bot) resetMatch() {
	b.Roles, b.mining, b.scouting, b.seenAt = map[uint64]Role{}, map[uint64]uint64{}, map[uint64]state.Pos{}, map[uint64]uint64{}
	b.foraging = map[uint64]uint64{}
	b.intel, b.probe, b.muster = map[uint32]*enemyIntel{}, probeState{}, false
	b.musterFrom, b.musterOff, b.musterFuel = 0, 0, 0
	b.byRoleDirty = true
	b.bankID, b.campaign, b.readyAt, b.monitorAt = 0, nil, 0, 0
	b.strategy = strategyState{}
	b.Cmd.pending = map[string]Order{}
	b.Cmd.last = map[string]sent{}
}

// Step makes this tick's decisions after Knowledge was updated. It returns
// the submissions to send, in order.
func (b *Bot) Step() []Outgoing {
	start := time.Now() // diagnostics only; never influences decisions
	k := b.K
	if k.Reset {
		b.log.Info("new match", "tick", k.Tick, "match", k.Match.GetMatchId())
		b.resetMatch()
	}
	for _, f := range b.Cmd.Results(k.Results) {
		b.log.Info("command failed", "tick", k.Tick, "order", f.Order.Desc, "code", f.Code, "msg", f.Message)
		b.onFailure(f)
	}
	for _, id := range k.LostOwn {
		delete(b.Roles, id)
		b.byRoleDirty = true
		delete(b.mining, id)
		delete(b.scouting, id)
		delete(b.foraging, id)
		if id == b.bankID {
			b.bankID = 0
			b.log.Info("bank lost; replanning", "tick", k.Tick)
		}
	}
	b.lifecycle()
	if k.Running() && len(k.Own) > 0 {
		t := int(k.Tick)
		if b.strategy.name == "" || t%b.P.StrategicInterval == 0 {
			b.ops = b.P.ComputeBudget
			b.strategic()
		}
		if t%b.P.OperationalInterval == 0 || b.bankID == 0 {
			b.ops = b.P.ComputeBudget
			b.operational()
		}
		if t%b.P.TacticalInterval == 0 {
			b.ops = b.P.ComputeBudget
			b.tactical()
		}
	}
	b.flushTags()
	out := b.Cmd.Flush(k.Tick)
	if b.log.Enabled(context.Background(), slog.LevelDebug) {
		for _, o := range out {
			b.log.Debug("order", "tick", k.Tick, "id", o.ID, "units", o.Order.Units, "desc", o.Order.Desc)
		}
	}
	b.Status.update(b, len(out), time.Since(start))
	return out
}

// spend consumes compute budget; false when exhausted (deterministic).
func (b *Bot) spend(n int) bool {
	if b.ops < n {
		b.Status.BudgetExhausted++
		return false
	}
	b.ops -= n
	return true
}

// mistake reports whether to deliberately take a suboptimal choice.
func (b *Bot) mistake() bool { return b.P.MistakeRate > 0 && b.rng.IntN(1000) < b.P.MistakeRate }

// pick returns the index of the best candidate (scores descending is best),
// considering at most P.Candidates, with controlled mistakes.
func (b *Bot) pick(scores []int64) int {
	n := min(len(scores), max(b.P.Candidates, 1))
	if n == 0 {
		return -1
	}
	if b.mistake() {
		return b.rng.IntN(n)
	}
	best := 0
	for i := 1; i < n; i++ {
		if !b.spend(1) {
			break
		}
		if scores[i] > scores[best] {
			best = i
		}
	}
	return best
}

// issue queues an order. Moves are energy-guarded: units that could not
// afford the trip plus a small reserve are left out (they refuel instead),
// so errands never strand units with empty stores.
func (b *Bot) issue(o Order) {
	if m, ok := o.Payload.(*movementv1.GroupMoveCommand); ok && !o.Essential && b.K.Rules.EnergyPerStep > 0 {
		dest := state.Pos{X: m.GetDestination().GetX(), Y: m.GetDestination().GetY()}
		var keep []uint64
		for _, id := range m.GetUnitIds() {
			u := b.K.Own[id]
			if u == nil {
				continue
			}
			if e, _ := energy(u); e >= b.tripCost(u.Pos, dest)+b.K.Rules.EnergyPerStep*10 {
				keep = append(keep, id)
			}
		}
		if len(keep) == 0 {
			return
		}
		// Standing move orders are not repeated, unless the units stopped
		// short of the destination (blocked path): then replan.
		stopped := true
		for _, id := range keep {
			if u := b.K.Own[id]; u.Has("movement.destination") || state.Cheb(u.Pos, dest) <= 1 {
				stopped = false
				break
			}
		}
		if stopped {
			b.Cmd.ForgetOlder(o.Key, b.K.Tick, 20)
		}
		if len(keep) < len(m.GetUnitIds()) {
			o.Payload = &movementv1.GroupMoveCommand{UnitIds: keep, Destination: m.GetDestination()}
			o.Units = len(keep)
		}
	}
	b.Cmd.Issue(b.K.Tick, o)
}

func (b *Bot) onFailure(f Failure) {
	// Assignments whose orders failed are forgotten so they are replanned.
	for id, w := range b.mining {
		if f.Order.Key == fmt.Sprintf("auto:%d", id) {
			_ = w
			delete(b.mining, id)
		}
	}
}

// lifecycle readies up in a lobby (retrying until the server says so) and
// reports the outcome when a match ends.
func (b *Bot) lifecycle() {
	m := b.K.Match
	if m != nil && m.GetState() != b.lastState {
		b.lastState = m.GetState()
		if r := m.GetResult(); b.lastState == rtsv1.MatchState_MATCH_STATE_FINISHED && r != nil {
			var scores []string
			for _, p := range r.GetPlayers() {
				scores = append(scores, fmt.Sprintf("%s=%d", p.GetName(), p.GetScore()))
			}
			outcome := "lost"
			switch {
			case r.GetDraw():
				outcome = "draw"
			case r.GetWinnerId() == b.K.Me:
				outcome = "won"
			}
			b.Status.Outcome = outcome
			b.log.Info("match finished", "tick", b.K.Tick, "outcome", outcome, "reason", r.GetReason(),
				"run_ticks", r.GetRunTicks(), "scores", strings.Join(scores, " "))
		} else {
			b.log.Info("match state", "tick", b.K.Tick, "state", strings.ToLower(strings.TrimPrefix(b.lastState.String(), "MATCH_STATE_")))
		}
	}
	if m == nil || m.GetState() != rtsv1.MatchState_MATCH_STATE_READY || m.GetYouReady() {
		return
	}
	if b.K.Tick >= b.readyAt {
		b.readyAt = b.K.Tick + 20
		b.issue(Order{Key: fmt.Sprintf("ready:%d", b.K.Tick), Payload: &matchv1.SetReadyCommand{Ready: true}, Priority: PrioLifecycle, Desc: "ready"})
	}
}

// setRole changes a unit's role and tags it (metadata role=<r>) so the
// assignment is visible and usable in selectors.
func (b *Bot) setRole(u *state.Entity, r Role) {
	if b.Roles[u.ID] == r {
		return
	}
	prev := b.Roles[u.ID]
	b.Roles[u.ID] = r
	b.byRoleDirty = true
	if prev != RoleNone {
		b.Status.RoleChanges++
	}
	if prev == RoleMiner {
		delete(b.mining, u.ID)
	}
	if prev == RoleScout {
		delete(b.scouting, u.ID)
	}
	if prev == RoleMiner || prev == RoleGuard || prev == RoleSalvager {
		b.issue(Order{Key: fmt.Sprintf("auto:%d", u.ID), Units: 1, Priority: PrioRoles, Desc: "cancel automation",
			Payload: &automationv1.CancelAutomationCommand{UnitIds: []uint64{u.ID}}})
	}
	if b.tags == nil {
		b.tags = map[Role][]uint64{}
	}
	b.tags[r] = append(b.tags[r], u.ID)
}

// flushTags tags this tick's role changes: one batch metadata command per
// role (a batch is charged per unit, but costs one submission).
func (b *Bot) flushTags() {
	room := max(1, b.Cmd.budget/4) // tags never crowd out real orders
	roles := make([]Role, 0, len(b.tags))
	for r := range b.tags {
		roles = append(roles, r)
	}
	slices.Sort(roles)
	for _, r := range roles {
		ids := b.tags[r]
		if len(ids) == 0 || room == 0 {
			continue
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		n := min(len(ids), room)
		room -= n
		batch := slices.Clone(ids[:n])
		b.tags[r] = ids[n:] // the rest next tick
		b.issue(Order{Key: fmt.Sprintf("tag:%s:%d", r, b.K.Tick), Units: n, Priority: PrioRoles - 5, Desc: "role " + string(r),
			Payload: &metadatav1.SetMetadataCommand{UnitIds: batch, Key: "role", Value: string(r)}})
	}
}

func (b *Bot) bank() *state.Entity { return b.K.Own[b.bankID] }

// unitsByRole returns own units with a role, in ID order. The index is
// rebuilt when the unit list or any role changes; callers must not modify
// the returned slice.
func (b *Bot) unitsByRole(r Role) []*state.Entity {
	units := b.K.OwnUnits()
	if b.byRole == nil || len(units) == 0 || b.byRoleFor != &units[0] || b.byRoleN != len(units) || b.byRoleDirty {
		b.byRole = map[Role][]*state.Entity{}
		for _, u := range units {
			role := b.Roles[u.ID]
			b.byRole[role] = append(b.byRole[role], u)
		}
		for k, v := range b.byRole {
			b.byRole[k] = v[:len(v):len(v)]
		}
		b.byRoleN, b.byRoleDirty = len(units), false
		if len(units) > 0 {
			b.byRoleFor = &units[0]
		}
	}
	return b.byRole[r]
}

func pos(p state.Pos) *rtsv1.Position { return &rtsv1.Position{X: p.X, Y: p.Y} }

func (b *Bot) attr(u *state.Entity, name string, def int64) int64 {
	if name == "" {
		return def
	}
	if v, ok, _ := u.Int("attributes." + name); ok {
		return v
	}
	if r, ok := b.K.Rules.Attr(name); ok {
		return r.Default // not disclosed: the rules' default level
	}
	return def
}

func energy(u *state.Entity) (stored, capacity int64) {
	return u.IntOr("energy.stored", 0), u.IntOr("energy.capacity", 0)
}

func busy(u *state.Entity) bool { return u.Has("attributes.busy") }

// tripCost estimates the energy to travel between two tiles (paths are
// longer than straight lines around obstacles).
func (b *Bot) tripCost(a, c state.Pos) int64 {
	return int64(state.Cheb(a, c)) * b.K.Rules.EnergyPerStep * 13 / 10
}

// low reports a unit too drained to take on errands: it should refuel.
func (b *Bot) low(u *state.Entity) bool {
	step := b.K.Rules.EnergyPerStep
	if step <= 0 {
		return false
	}
	e, _ := energy(u)
	return e < step*12
}

// active filters out units that are refueling.
func (b *Bot) active(us []*state.Entity) []*state.Entity {
	out := us[:0:0]
	for _, u := range us {
		if _, f := b.foraging[u.ID]; !f {
			out = append(out, u)
		}
	}
	return out
}

// occupancy returns the tiles blocked by terrain or units, built once per
// tick (several planners look for free tiles).
func (b *Bot) occupancy() map[state.Pos]bool {
	if b.occ == nil || b.occTick != b.K.Tick {
		b.occ, b.occTick = b.K.OccupancyMap(), b.K.Tick
	}
	return b.occ
}
