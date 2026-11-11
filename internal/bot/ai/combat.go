package ai

import (
	"cmp"
	"fmt"
	"slices"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// campaign is an offensive operation toward an objective.
type campaign struct {
	target  state.Pos
	started uint64
	march   bool
	members map[uint64]bool // fighters fueled for the trip when it started
}

// strength estimates a unit's fighting value. ThreatModel 0 counts units;
// 1 uses attack x health; 2 also discounts by armor-like durability and
// keeps inferred enemy attack from observed damage.
func (b *Bot) strength(u *state.Entity, own bool) int64 {
	if b.P.ThreatModel == 0 {
		return 100
	}
	rl := b.K.Rules
	health := u.IntOr("combat.health", rl.DefaultHealth)
	var attack int64
	if own {
		attack = u.IntOr("combat.attack", rl.DefaultAttack)
	} else {
		attack = max(rl.DefaultAttack, b.Status.InferredEnemyHit)
	}
	s := max(attack, 1) * max(health, 1)
	if b.P.ThreatModel >= 2 && !own {
		// Unknown upgrades: assume enemies kept pace with what they showed.
		if v, ok, _ := u.Int("attributes.armor"); ok {
			s += s * v / 10
		}
	}
	return s
}

// enemyEstimate is the estimated enemy army strength from current and
// (for better threat models) remembered sightings, decayed by age.
func (b *Bot) enemyEstimate() (int64, int) {
	var total int64
	n := 0
	for _, e := range b.K.Enemies() {
		total += b.strength(e, false)
		n++
	}
	if b.P.ThreatModel >= 1 {
		for _, m := range b.K.RememberedEnemies() {
			age := int64(b.K.Tick - m.LastSeen)
			w := max(0, int64(state.MemoryTicks)-age)
			total += b.strength(m.Entity, false) * w / state.MemoryTicks
			n++
		}
	}
	return total, n
}

// raidTarget picks the nearest known enemy position next to a well away
// from home (miners at work), if any.
func (b *Bot) raidTarget() (state.Pos, bool) {
	var cands []state.Pos
	add := func(p state.Pos) {
		if state.Cheb(p, b.home) <= 10 {
			return
		}
		for _, w := range b.K.UsableWells() {
			if state.Cheb(w.Pos, p) <= 2 {
				cands = append(cands, w.Pos)
				return
			}
		}
	}
	for _, e := range b.K.Enemies() {
		add(e.Pos)
	}
	for _, m := range b.K.RememberedEnemies() {
		add(m.Entity.Pos)
	}
	if len(cands) == 0 {
		return state.Pos{}, false
	}
	slices.SortStableFunc(cands, func(x, y state.Pos) int {
		return cmp.Or(cmp.Compare(state.Cheb(x, b.home), state.Cheb(y, b.home)), cmp.Compare(x.Y, y.Y), cmp.Compare(x.X, y.X))
	})
	scores := make([]int64, len(cands))
	for i := range cands {
		scores[i] = -int64(i)
	}
	return cands[b.pick(scores)], true
}

// knownNear sums the strength of current and remembered enemies near p.
func (b *Bot) knownNear(p state.Pos, r int32) int64 {
	var s int64
	for _, e := range b.K.Enemies() {
		if state.Cheb(e.Pos, p) <= r {
			s += b.strength(e, false)
		}
	}
	for _, m := range b.K.RememberedEnemies() {
		if state.Cheb(m.Entity.Pos, p) <= r {
			s += b.strength(m.Entity, false)
		}
	}
	return s
}

// baseStrength is the strength of a unit with default attributes.
func (b *Bot) baseStrength() int64 {
	r := b.K.Rules
	return max(1, r.DefaultAttack) * max(1, r.DefaultHealth)
}

// threatNear sums current enemy strength within r of p.
func (b *Bot) threatNear(p state.Pos, r int32) int64 {
	var s int64
	for _, e := range b.K.Enemies() {
		if state.Cheb(e.Pos, p) <= r {
			s += b.strength(e, false)
		}
	}
	return s
}

// inferEnemyHit learns enemy damage per hit from damage reports (only our
// own units' damage events: legitimately observed).
func (b *Bot) inferEnemyHit() {
	for _, ev := range b.K.Events {
		if ev.GetType() != "combat.damaged" || b.P.ThreatModel < 2 {
			continue
		}
		f := ev.GetFields()
		if n := f["attackers"].GetIntValue(); n > 0 {
			b.Status.InferredEnemyHit = max(b.Status.InferredEnemyHit, f["damage"].GetIntValue()/n)
		}
	}
}

// enemyBaseGuess infers where the enemy is: the recency-weighted centroid
// of sightings, or (nothing seen) the mirror of our own base, a common map
// symmetry. Inference from observation and geometry only.
func (b *Bot) enemyBaseGuess() state.Pos {
	var sx, sy, sw int64
	for _, e := range b.K.Enemies() {
		sx, sy, sw = sx+int64(e.Pos.X)*4, sy+int64(e.Pos.Y)*4, sw+4
	}
	if b.P.ThreatModel >= 1 {
		for _, m := range b.K.RememberedEnemies() {
			w := max(1, int64(state.MemoryTicks)-int64(b.K.Tick-m.LastSeen)) / 100
			sx, sy, sw = sx+int64(m.Entity.Pos.X)*w, sy+int64(m.Entity.Pos.Y)*w, sw+w
		}
	}
	if sw > 0 {
		return state.Pos{X: int32(sx / sw), Y: int32(sy / sw)}
	}
	return state.Pos{X: b.K.Width - 1 - b.home.X, Y: b.K.Height - 1 - b.home.Y}
}

// tactical runs the tactical layer.
func (b *Bot) tactical() {
	k := b.K
	b.inferEnemyHit()
	enemies := k.Enemies()
	live := map[uint64]bool{}
	for _, e := range enemies {
		live[e.ID] = true
		if _, ok := b.seenAt[e.ID]; !ok {
			b.seenAt[e.ID] = k.Tick
		}
	}
	for id := range b.seenAt {
		if !live[id] {
			delete(b.seenAt, id)
		}
	}
	b.strategy.lost += len(k.LostOwn)
	fighters := b.active(b.unitsByRole(RoleFighter))
	b.Status.Threats = 0
	// Threats: enemies near home or near our miners, noticed long enough.
	minerCells := map[[2]int32]bool{}
	for _, u := range b.unitsByRole(RoleMiner) {
		minerCells[[2]int32{u.Pos.X / 8, u.Pos.Y / 8}] = true
	}
	var threats []*state.Entity
	for _, e := range enemies {
		if k.Tick < b.seenAt[e.ID]+uint64(b.P.ReactionDelay) {
			continue
		}
		if state.Cheb(e.Pos, b.home) <= 10 || minerCells[[2]int32{e.Pos.X / 8, e.Pos.Y / 8}] {
			threats = append(threats, e)
		}
	}
	b.Status.Threats = len(threats)
	if len(fighters) == 0 {
		return
	}
	if len(threats) > 0 {
		if b.campaign != nil && b.P.Coordination >= 600 {
			// A coordinated bot keeps its offensive if the home threat is
			// small relative to its guards.
			if b.threatNear(b.home, 10) <= int64(len(b.unitsByRole(RoleGuard)))*b.strength(fighters[0], true) {
				b.runCampaign(fighters, enemies)
				return
			}
		}
		if b.campaign != nil {
			b.log.Info("campaign aborted", "tick", k.Tick, "reason", "defend home")
			b.campaign = nil
		}
		b.engage(fighters, threats, "defend")
		return
	}
	if b.campaign != nil {
		var army, rest []*state.Entity
		for _, u := range fighters {
			if b.campaign.members[u.ID] {
				army = append(army, u)
			} else {
				rest = append(rest, u)
			}
		}
		if len(army) == 0 {
			b.log.Info("campaign over", "tick", k.Tick, "reason", "no army left")
			b.campaign = nil
		} else {
			b.runCampaign(army, enemies)
			b.camp(rest)
			return
		}
	}
	// Start a campaign when strong enough relative to the estimate, with
	// the fighters that carry enough energy for the march and the fight.
	// Coordinated bots raid enemy miners at wells first: weak, valuable
	// targets (kills, destroyed investment) away from the enemy army.
	target, raid := b.enemyBaseGuess(), false
	if b.P.Coordination >= 500 {
		if t, ok := b.raidTarget(); ok {
			target, raid = t, true
		}
	}
	own := int64(0)
	var ready []*state.Entity
	for _, u := range fighters {
		if e, _ := energy(u); b.K.Rules.EnergyPerStep == 0 || e >= b.tripCost(u.Pos, target)+15 {
			ready = append(ready, u)
			own += b.strength(u, true)
		}
	}
	est, _ := b.enemyEstimate()
	if est == 0 {
		// Nothing seen: assume the enemy has what we started with.
		est = int64(4) * b.strength(fighters[0], true)
	}
	switch {
	case raid:
		// A raid only has to beat what guards the well (remembered), with
		// a margin for the unseen.
		est = max(b.knownNear(target, 6)*3/2, 2*b.baseStrength())
	case b.P.ThreatModel >= 1:
		// Unseen does not mean absent: assume the enemy fields at least
		// half our unit count at our base strength (symmetric starts).
		est = max(est, int64(len(b.K.OwnUnits())/2)*b.baseStrength())
	}
	// Most of the army must be fueled: a campaign with a fraction of it
	// is defeated in detail.
	minArmy := max(3, len(fighters)*6/10)
	if b.dir.AttackRatio > 0 && len(ready) >= minArmy && own*1000 >= est*int64(b.dir.AttackRatio)*int64(1350-b.P.RiskTolerance)/1000 {
		b.campaign = &campaign{target: target, started: k.Tick, march: b.P.Coordination < 300, members: map[uint64]bool{}}
		for _, u := range ready {
			b.campaign.members[u.ID] = true
		}
		b.log.Info("campaign", "tick", k.Tick, "target", fmt.Sprint(b.campaign.target), "raid", raid, "army", len(ready), "own", own, "enemy_estimate", est)
		return
	}
	b.camp(fighters)
}

// lowForCamp: an idle fighter worth a refueling trip (a quarter full).
func (b *Bot) lowForCamp(u *state.Entity) bool {
	e, capacity := energy(u)
	return e*4 < capacity
}

// attackLikely reports whether fueling idle fighters is worth the walk:
// the strategy is offensive or the enemy has been seen near home.
func (b *Bot) attackLikely() bool {
	return b.dir.AttackRatio <= 1300 || b.Status.Threats > 0
}

// campPos is where idle fighters wait: a few tiles from home toward the
// enemy.
func (b *Bot) campPos() state.Pos {
	g := b.enemyBaseGuess()
	return state.Pos{X: b.home.X + clamp(g.X-b.home.X, -3, 3), Y: b.home.Y + clamp(g.Y-b.home.Y, -3, 3)}
}

// camp keeps idle fighters near home (between home and the enemy).
func (b *Bot) camp(fighters []*state.Entity) {
	c := b.campPos()
	var far []uint64
	bank := b.bank()
	refuel := 0
	for _, u := range fighters {
		// Half-empty fighters top up at the bank (a few at a time).
		if refuel < 2 && bank != nil && b.attackLikely() && b.lowForCamp(u) && b.bankSurplus() >= 25 && !u.Has("movement.destination") {
			refuel++
			if state.Cheb(u.Pos, bank.Pos) > 1 {
				b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioTactical - 10, Essential: true, Desc: "to bank for fuel",
					Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(b.freeNeighbor(bank.Pos))}})
			}
			continue
		}
		// Idle fighters stay put unless they drifted away: every step
		// costs energy.
		if state.Cheb(u.Pos, c) > 6 && !u.Has("movement.destination") && !u.Has("combat.target") {
			far = append(far, u.ID)
		}
	}
	if len(far) > 0 {
		b.issue(Order{Key: "grp:camp", Units: len(far), Priority: PrioTactical - 10, Desc: "camp",
			Payload: &movementv1.GroupMoveCommand{UnitIds: far, Destination: pos(c)}})
	}
}

func clamp(v, lo, hi int32) int32 { return max(lo, min(hi, v)) }

func centroid(us []*state.Entity) state.Pos {
	var x, y int64
	for _, u := range us {
		x, y = x+int64(u.Pos.X), y+int64(u.Pos.Y)
	}
	n := int64(max(len(us), 1))
	return state.Pos{X: int32(x / n), Y: int32(y / n)}
}

func ids(us []*state.Entity) []uint64 {
	out := make([]uint64, len(us))
	for i, u := range us {
		out[i] = u.ID
	}
	return out
}

// runCampaign advances an offensive: gather, march, fight, retarget, or
// retreat from a losing fight.
func (b *Bot) runCampaign(fighters, enemies []*state.Entity) {
	c := b.campaign
	mid := centroid(fighters)
	var near []*state.Entity
	for _, e := range enemies {
		if state.Cheb(e.Pos, mid) <= 8 {
			near = append(near, e)
		}
	}
	if len(near) > 0 {
		if b.shouldRetreat(fighters, near) {
			b.log.Info("retreat", "tick", b.K.Tick, "army", len(fighters), "enemies", len(near))
			b.campaign = nil
			b.issue(Order{Key: "grp:army", Units: len(fighters), Priority: PrioTactical + 5, Essential: true, Desc: "retreat",
				Payload: &movementv1.GroupMoveCommand{UnitIds: ids(fighters), Destination: pos(b.home)}})
			return
		}
		b.engage(fighters, near, "attack")
		return
	}
	if !c.march {
		rally := state.Pos{X: b.home.X + clamp(c.target.X-b.home.X, -4, 4), Y: b.home.Y + clamp(c.target.Y-b.home.Y, -4, 4)}
		in := 0
		for _, u := range fighters {
			if state.Cheb(u.Pos, rally) <= 6 {
				in++
			}
		}
		if in*10 >= len(fighters)*7 || b.K.Tick > c.started+150 {
			c.march = true
		} else {
			b.issue(Order{Key: "grp:army", Units: len(fighters), Priority: PrioTactical, Desc: "gather",
				Payload: &movementv1.GroupMoveCommand{UnitIds: ids(fighters), Destination: pos(rally)}})
			return
		}
	}
	if state.Cheb(mid, c.target) <= 3 {
		// Objective reached and clear: the next freshest sighting, else done.
		for _, m := range b.K.RememberedEnemies() {
			if state.Cheb(m.Entity.Pos, mid) > 6 {
				c.target = m.Entity.Pos
				b.log.Info("campaign retarget", "tick", b.K.Tick, "target", fmt.Sprint(c.target))
				return
			}
		}
		b.log.Info("campaign complete", "tick", b.K.Tick)
		b.campaign = nil
		return
	}
	b.issue(Order{Key: "grp:army", Units: len(fighters), Priority: PrioTactical, Desc: "march",
		Payload: &movementv1.GroupMoveCommand{UnitIds: ids(fighters), Destination: pos(c.target)}})
}

// shouldRetreat compares local strengths with the bot's risk tolerance.
func (b *Bot) shouldRetreat(fighters, enemies []*state.Entity) bool {
	if !b.P.Retreat {
		return false
	}
	var own, foe int64
	for _, u := range fighters {
		own += b.strength(u, true)
	}
	for _, e := range enemies {
		foe += b.strength(e, false)
	}
	return own*1000 < foe*int64(1300-b.P.RiskTolerance)
}

// engage attacks targets with a group: focus fire on the best target
// (coordinated) or each unit on its nearest (uncoordinated).
func (b *Bot) engage(group, targets []*state.Entity, why string) {
	if len(targets) == 0 || len(group) == 0 {
		return
	}
	mid := centroid(group)
	scores := make([]int64, len(targets))
	order := make([]int, len(targets))
	for i, t := range targets {
		order[i] = i
		if !b.spend(1) {
			scores[i] = -int64(state.Cheb(t.Pos, mid))
			continue
		}
		health := max(t.IntOr("combat.health", b.K.Rules.DefaultHealth), 1)
		s := int64(100000) / health // finish weak units first
		if b.P.ThreatModel >= 1 {
			s += b.strength(t, false) / 50
		}
		if b.dir.DenyResources {
			for _, w := range b.K.Wells {
				if state.Cheb(w.Pos, t.Pos) <= 2 {
					s += s / 2
					break
				}
			}
		}
		scores[i] = s - int64(state.Cheb(t.Pos, mid))*20
	}
	slices.SortStableFunc(order, func(x, y int) int {
		return cmp.Or(cmp.Compare(scores[y], scores[x]), cmp.Compare(targets[x].ID, targets[y].ID))
	})
	sorted := make([]int64, len(order))
	for i, o := range order {
		sorted[i] = scores[o]
	}
	b.Status.LastEngagement = fmt.Sprintf("%s %d vs %d", why, len(group), len(targets))
	if b.rng.IntN(1000) < b.P.Coordination {
		t := targets[order[b.pick(sorted)]]
		us := ids(attackers(group))
		if len(us) == 0 {
			return
		}
		b.issue(Order{Key: "grp:army", Units: len(us), Priority: PrioTactical, Desc: why + " move",
			Payload: &movementv1.GroupMoveCommand{UnitIds: us, Destination: pos(t.Pos)}})
		b.issue(Order{Key: "grp:attack", Units: len(us), Priority: PrioTactical + 1, Desc: why + " focus " + fmtID(t.ID),
			Payload: &combatv1.AttackCommand{UnitIds: us, TargetId: t.ID}})
		return
	}
	for _, u := range attackers(group) {
		best := targets[0]
		for _, t := range targets[1:] {
			if state.Cheb(t.Pos, u.Pos) < state.Cheb(best.Pos, u.Pos) {
				best = t
			}
		}
		b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioTactical, Desc: why + " move",
			Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(best.Pos)}})
		b.issue(Order{Key: fmt.Sprintf("atk:%d", u.ID), Units: 1, Priority: PrioTactical + 1, Desc: why,
			Payload: &combatv1.AttackCommand{UnitIds: []uint64{u.ID}, TargetId: best.ID}})
	}
}

// attackers are the units that can attack (attack > 0).
func attackers(us []*state.Entity) []*state.Entity {
	var out []*state.Entity
	for _, u := range us {
		if u.IntOr("combat.attack", 1) > 0 {
			out = append(out, u)
		}
	}
	return out
}
