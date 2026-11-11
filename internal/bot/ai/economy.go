package ai

import (
	"cmp"
	"fmt"
	"slices"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	logisticsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/logistics/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// operational runs the operational layer.
func (b *Bot) operational() {
	units := b.K.OwnUnits()
	if len(units) == 0 {
		return
	}
	b.chooseBank(units)
	b.assignRoles(units)
	b.planMining()
	b.planBank()
	b.planUpgrades()
	b.planLogistics()
	b.planGuards()
	b.planSalvage()
	b.planScouts()
	b.planForage()
	b.clearBank()
	b.Status.Objectives = b.Status.Objectives[:0]
	for _, o := range b.objectives {
		if active, detail := o.Plan(b); active {
			b.Status.Objectives = append(b.Status.Objectives, o.Name()+" "+detail)
		}
	}
}

// chooseBank picks the unit that stores energy, clones and funds upgrades:
// the one nearest the closest known well (skilled) or the first unit.
func (b *Bot) chooseBank(units []*state.Entity) {
	if b.bank() != nil {
		b.home = b.bank().Pos
		return
	}
	best := units[0]
	if wells := b.K.UsableWells(); len(wells) > 0 && b.econSkill() {
		bd := int32(1 << 30)
		for _, u := range units {
			for _, w := range wells {
				if d := state.Cheb(u.Pos, w.Pos); d < bd {
					best, bd = u, d
				}
			}
		}
	}
	b.bankID, b.home = best.ID, best.Pos
	b.setRole(best, RoleBank)
	b.log.Info("bank", "tick", b.K.Tick, "unit", best.ID, "pos", fmt.Sprint(best.Pos))
}

// homeWells returns usable wells near home, nearest first, limited by the
// candidates parameter (at least one).
func (b *Bot) homeWells() []*state.Well {
	ws := b.K.UsableWells()
	slices.SortStableFunc(ws, func(a, c *state.Well) int {
		return cmp.Compare(state.Cheb(a.Pos, b.home), state.Cheb(c.Pos, b.home))
	})
	limit := max(1, min(len(ws), 2+b.P.Candidates/4))
	// Far wells cost more in travel than they yield without a depot.
	maxDist := int32(8 + b.P.EconomicEfficiency/100)
	var out []*state.Well
	for _, w := range ws {
		if len(out) >= limit || state.Cheb(w.Pos, b.home) > maxDist {
			break
		}
		out = append(out, w)
	}
	return out
}

// minersPerWell: a well yields a fixed rate to adjacent miners and the
// round trip takes time, so a few miners per well keep it busy; skilled
// bots stop at the useful number and spread over more wells.
func (b *Bot) minersPerWell() int {
	if b.P.EconomicEfficiency >= 600 {
		return 4
	}
	return 5
}

// wellCap is how many miners a well is worth: a regenerating well that ran
// dry yields a trickle (one miner); otherwise minersPerWell.
func (b *Bot) wellCap(w *state.Well) int {
	if w.Depleted && !w.Unlimited {
		return 1
	}
	return b.minersPerWell()
}

// assignRoles sets the role mix from the directive, keeping existing
// assignments where possible.
func (b *Bot) assignRoles(units []*state.Entity) {
	n := len(units)
	want := map[Role]int{RoleBank: 1}
	wells := b.homeWells()
	slots := 0
	for _, w := range wells {
		slots += b.wellCap(w)
	}
	miners := min(slots, max(1, (n-1)*b.dir.MinerShare/1000))
	if len(wells) == 0 {
		miners = 0
	}
	want[RoleMiner] = min(miners, n-1)
	// Hysteresis: a one-unit dip does not pull a miner off its route.
	if cur := len(b.unitsByRole(RoleMiner)); len(wells) > 0 && cur == want[RoleMiner]+1 {
		want[RoleMiner] = cur
	}
	if b.P.Scout && n >= 3 {
		s := b.dir.Scouts * (500 + b.P.ExplorationAggression) / 1000
		if len(wells) == 0 {
			s = max(s, 2)
		}
		want[RoleScout] = min(max(s, 1), max(1, n/5))
	}
	if b.P.UseGuards && b.K.Rules.HasAutomation && n >= 6 {
		want[RoleGuard] = min(b.dir.Guards, n/6)
	}
	if b.P.UseSalvage && b.K.Rules.HasAutomation && n >= 6 && len(b.K.Wrecks()) > 0 {
		want[RoleSalvager] = 1
	}
	// Keep units already in a wanted role, up to the count.
	have := map[Role]int{}
	var pool []*state.Entity
	for _, u := range units {
		r := b.Roles[u.ID]
		if r.Extension() {
			continue // claimed by an extension objective
		}
		if r == RoleBank && u.ID != b.bankID {
			r = RoleNone
		}
		if r != RoleNone && r != RoleFighter && have[r] < want[r] {
			have[r]++
			continue
		}
		if r == RoleBank {
			continue
		}
		pool = append(pool, u)
	}
	// Fill deficits from the pool, nearest home first (deterministic).
	slices.SortStableFunc(pool, func(a, c *state.Entity) int {
		return cmp.Or(cmp.Compare(state.Cheb(a.Pos, b.home), state.Cheb(c.Pos, b.home)), cmp.Compare(a.ID, c.ID))
	})
	for _, r := range []Role{RoleMiner, RoleScout, RoleGuard, RoleSalvager} {
		if r == RoleMiner {
			// New miners need energy to reach the well: fullest first.
			slices.SortStableFunc(pool, func(a, c *state.Entity) int {
				ea, _ := energy(a)
				ec, _ := energy(c)
				return cmp.Or(cmp.Compare(ec, ea), cmp.Compare(state.Cheb(a.Pos, b.home), state.Cheb(c.Pos, b.home)), cmp.Compare(a.ID, c.ID))
			})
		} else if r == RoleScout {
			slices.SortStableFunc(pool, func(a, c *state.Entity) int {
				return cmp.Or(cmp.Compare(state.Cheb(a.Pos, b.home), state.Cheb(c.Pos, b.home)), cmp.Compare(a.ID, c.ID))
			})
		}
		for have[r] < want[r] && len(pool) > 0 {
			u := pool[0]
			pool = pool[1:]
			if busy(u) && r != RoleMiner {
				pool = append(pool, u)
				if len(pool) == 1 {
					break
				}
				continue
			}
			b.setRole(u, r)
			have[r]++
		}
	}
	for _, u := range pool {
		b.setRole(u, RoleFighter)
	}
}

// planMining keeps every miner on mine-and-deliver automation to the bank.
func (b *Bot) planMining() {
	wells := b.homeWells()
	bank := b.bank()
	if len(wells) == 0 || bank == nil {
		return
	}
	load := map[uint64]int{}
	valid := map[uint64]bool{}
	for _, w := range wells {
		valid[w.ID] = true
	}
	for m, w := range b.mining {
		if valid[w] && b.Roles[m] == RoleMiner {
			load[w]++
		} else {
			delete(b.mining, m)
		}
	}
	for _, u := range b.unitsByRole(RoleMiner) {
		w, ok := b.mining[u.ID]
		if !ok {
			// Skilled: the least-loaded nearby well; unskilled: the nearest.
			best := wells[0]
			if b.econSkill() && !b.mistake() {
				for _, c := range wells[1:] {
					if load[c.ID]*b.wellCap(best) < load[best.ID]*b.wellCap(c) {
						best = c
					}
				}
			}
			w = best.ID
			b.mining[u.ID] = w
			load[w]++
		}
		var well *state.Well
		for _, c := range wells {
			if c.ID == w {
				well = c
			}
		}
		// Automation only accepts wells the player currently detects:
		// walk toward a remembered well first (an already running
		// automation keeps going on its own).
		if !well.Current {
			if u.Str("automation.behavior") != "mine_and_deliver" {
				b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioEconomy, Desc: "miner to well",
					Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(b.freeNeighbor(well.Pos))}})
			}
			continue
		}
		// Keep enough for the round trip between the well and the bank
		// (terrain may cost more than a step), or the miner strands itself.
		keep := int64(0)
		if step := b.K.Rules.EnergyPerStep; step > 0 {
			keep = int64(state.Cheb(well.Pos, bank.Pos))*step*3 + step*6
		}
		key := fmt.Sprintf("auto:%d", u.ID)
		if u.Str("automation.behavior") == "" {
			// The server ended it (or it never started): send it again.
			b.Cmd.ForgetOlder(key, b.K.Tick, 20)
		}
		b.issue(Order{Key: key, Units: 1, Priority: PrioEconomy, Desc: "mine and deliver",
			Payload: &automationv1.AutomateCommand{UnitIds: []uint64{u.ID}, Behavior: &automationv1.AutomateCommand_MineAndDeliver{
				MineAndDeliver: &automationv1.MineAndDeliver{WellId: w, ReceiverId: bank.ID, Keep: keep}}}})
	}
	// The bank mines too when it stands next to a well.
	for _, w := range wells {
		if state.Cheb(bank.Pos, w.Pos) <= 1 {
			b.issue(Order{Key: "bank:mine", Units: 1, Priority: PrioEconomy, Desc: "bank mines",
				Payload: &energyv1.MineCommand{UnitIds: []uint64{bank.ID}, WellId: w.ID}})
			break
		}
	}
}

// cloneQuote returns the bank's current price to clone (cost + offspring).
func cloneQuote(u *state.Entity) (int64, bool) {
	c, ok, _ := u.Int("cloning.quote_cost")
	if !ok {
		return 0, false
	}
	return c + u.IntOr("cloning.quote_offspring_energy", 0), true
}

// econSkill decides whether this economic choice is made the skilled way:
// never at economic_efficiency 100, always from 450, linearly between, so
// skill phases in gradually across the low levels.
func (b *Bot) econSkill() bool {
	return b.rng.IntN(350) < b.P.EconomicEfficiency-100
}

// chooseCloner returns the unit that should clone next and the energy it
// needs (quote + offspring energy). The price includes the parent's embodied
// investment, so an upgraded bank is often a poor parent: skilled bots pick
// the cheapest unit next to the bank that can hold the price; unskilled
// bots use the bank whenever it can.
func (b *Bot) chooseCloner(bank *state.Entity) (*state.Entity, int64) {
	var best *state.Entity
	var bestNeed int64
	var bestDist int32
	// A full bank blocks deliveries: then miners waiting next to it with
	// enough energy may clone themselves.
	be, bc := energy(bank)
	bankFull := be*10 >= bc*9
	bankChecked := false
	for _, u := range b.K.OwnUnits() {
		r := b.Roles[u.ID]
		d := state.Cheb(u.Pos, bank.Pos)
		minerOK := r == RoleMiner && bankFull && d <= 1
		if busy(u) || r.Extension() || (u != bank && ((r == RoleMiner && !minerOK) || r == RoleScout || d > 8)) || !b.spend(1) {
			continue
		}
		need, ok := cloneQuote(u)
		e, capacity := energy(u)
		if !ok || need > capacity || (minerOK && e < need) {
			continue
		}
		if u == bank && !bankChecked {
			bankChecked = true
			if !b.econSkill() {
				return u, need // naive: the bank clones whenever it can
			}
		}
		if best == nil || need < bestNeed || (need == bestNeed && (d < bestDist || (d == bestDist && u.ID < best.ID))) {
			best, bestNeed, bestDist = u, need, d
		}
	}
	return best, bestNeed
}

// freeNeighbor returns a free tile next to p (deterministic order), or p.
func (b *Bot) freeNeighbor(p state.Pos) state.Pos {
	occ := b.occupancy()
	for _, d := range [8][2]int32{{0, -1}, {1, -1}, {1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}} {
		q := state.Pos{X: p.X + d[0], Y: p.Y + d[1]}
		if b.K.InBounds(q) && !b.K.Occupied(q, occ) {
			occ[q] = true // claimed: the next caller this tick gets another tile
			return q
		}
	}
	return p
}

// planBank decides how the bank spends: clone, enlarge its storage, or
// fund another unit's upgrade (utility per energy).
func (b *Bot) planBank() {
	bank := b.bank()
	if bank == nil {
		return
	}
	e, capacity := energy(bank)
	n := len(b.K.OwnUnits())
	rules := b.K.Rules
	type cand struct {
		name  string
		cost  int64
		value int64 // benefit within the planning horizon (energy-equivalent)
		avail int64 // energy that can pay for it
		do    func()
	}
	var cands []cand
	horizon := int64(b.P.PlanningHorizon)
	b.clonerID, b.clonerNeed = 0, 0
	c, need := b.chooseCloner(bank)
	if c != nil {
		// A clone that cannot finish before the time limit only burns energy.
		if left := b.ticksLeft(); left >= 0 && left < c.IntOr("cloning.quote_ticks", 50)+10 {
			c = nil
		}
	}
	// While the army musters for an attack, its fuel comes first.
	if c != nil && n < b.dir.Population && b.K.CommandTypes["rts.cloning.v1.CloneCommand"] && !b.muster {
		b.clonerID, b.clonerNeed = c.ID, need
		// A new unit mines roughly one extraction per two ticks over the
		// horizon (less travel), and adds score/strength.
		value := horizon*rules.DefaultAttack/4 + horizon/2*max(1, b.attr(c, rules.ExtractionAttr, 2))/2
		ce, _ := energy(c)
		avail := e
		if c != bank {
			avail += ce
		}
		cands = append(cands, cand{"clone", need, value, avail, func() {
			// Direct transfers need adjacency (transfer range is for supply).
			if c != bank && state.Cheb(c.Pos, bank.Pos) > 1 && ce < need {
				b.issue(Order{Key: fmt.Sprintf("move:%d", c.ID), Units: 1, Priority: PrioEconomy, Essential: true, Desc: "cloner to bank",
					Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{c.ID}, Destination: pos(b.freeNeighbor(bank.Pos))}})
				return
			}
			if ce >= need {
				b.issue(Order{Key: "clone:" + fmtID(c.ID), Units: 1, Priority: PrioEconomy + 5, Desc: "clone " + fmtID(c.ID),
					OneShot: true,
					Payload: &cloningv1.CloneCommand{UnitIds: []uint64{c.ID}}})
				return
			}
			b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy + 5, Desc: "fund clone " + fmtID(c.ID),
				OneShot: true,
				Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: c.ID, Amount: need - ce}})
		}})
	}
	// Storage: when nearly full, deliveries stall; more capacity helps.
	cloneNow := len(cands) > 0 && cands[0].avail-b.dir.Reserve >= cands[0].cost
	if c := b.K.Own[b.clonerID]; c != nil && c != bank && state.Cheb(c.Pos, bank.Pos) > 1 {
		cloneNow = false // the cloner is still on its way
	}
	if cur := b.attr(bank, rules.CapacityAttr, 0); rules.CapacityAttr != "" && e*10 >= capacity*9 && !cloneNow && !busy(bank) {
		if cost, _, ok := rules.UpgradeCost(rules.CapacityAttr, cur, cur+25); ok {
			cands = append(cands, cand{"bank capacity", cost, horizon / 2, e, func() {
				b.upgrade(bank, rules.CapacityAttr, cur+25)
			}})
		}
	}
	// Transfer range lets the bank supply a whole camp (skilled play).
	if cur := b.attr(bank, rules.TransferRangeAttr, 1); b.P.UseLogistics && rules.TransferRangeAttr != "" && cur < 3 && !busy(bank) {
		if cost, _, ok := rules.UpgradeCost(rules.TransferRangeAttr, cur, cur+1); ok {
			cands = append(cands, cand{"transfer range", cost, horizon / 3, e, func() {
				b.upgrade(bank, rules.TransferRangeAttr, cur+1)
			}})
		}
	}
	scores := make([]int64, len(cands))
	for i, c := range cands {
		w := int64(1000)
		if c.name == "clone" {
			w = int64(1000 - b.dir.UpgradeShare/2)
		}
		scores[i] = c.value * w / max(c.cost, 1)
	}
	// Spend on the best affordable candidates (at most two per cycle).
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(scores[y], scores[x]) })
	if b.mistake() && len(order) > 1 {
		order[0], order[1] = order[1], order[0]
	}
	if b.debug {
		for _, i := range order {
			b.log.Debug("bank candidate", "tick", b.K.Tick, "action", cands[i].name, "cost", cands[i].cost, "avail", cands[i].avail,
				"value", cands[i].value, "score", scores[i])
		}
	}
	acted := 0
	for _, i := range order {
		c := cands[i]
		if acted == 2 || !b.spend(1) {
			break
		}
		if c.avail-b.dir.Reserve < c.cost {
			continue
		}
		c.do()
		e -= min(c.cost, e)
		acted++
		if b.debug {
			b.log.Debug("bank spend", "tick", b.K.Tick, "action", c.name, "cost", c.cost, "score", scores[i])
		}
	}
}

func (b *Bot) upgrade(u *state.Entity, attr string, to int64) {
	b.issue(Order{Key: fmt.Sprintf("up:%d", u.ID), Units: 1, Priority: PrioEconomy, Desc: fmt.Sprintf("upgrade %s to %d", attr, to),
		OneShot: true,
		Payload: &upgradesv1.UpgradeCommand{UnitIds: []uint64{u.ID}, Attribute: attr, Amount: &upgradesv1.UpgradeCommand_Target{Target: to}}})
}

// rolePlan lists the attributes a role invests in, in priority order (only
// attributes the server actually defines are used).
func (b *Bot) rolePlan(r Role) []string {
	rl := b.K.Rules
	switch r {
	case RoleMiner:
		// Storage means fewer round trips. Extraction upgrades are costly,
		// keep the miner busy (not mining) and are capped by the well's own
		// rate, so they rarely pay back within a match.
		return []string{rl.CapacityAttr}
	case RoleFighter, RoleGuard:
		return []string{rl.AttackAttr, rl.HealthAttr, "armor", rl.AttackAttr, rl.RangeAttr}
	case RoleScout:
		return []string{rl.VisionAttr, "propulsion", rl.SensorAttr}
	}
	return nil
}

// nextUpgrade picks the next attribute level for a unit: the planned
// attribute with the best value per cost (skilled) or a random one.
func (b *Bot) nextUpgrade(u *state.Entity) (string, int64, int64, bool) {
	plan := b.rolePlan(b.Roles[u.ID])
	rl := b.K.Rules
	var names []string
	for _, a := range plan {
		if _, ok := rl.Attr(a); ok && !slices.Contains(names, a) {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return "", 0, 0, false
	}
	if b.rng.IntN(1000) >= b.P.Specialization {
		// Unskilled: an arbitrary attribute.
		all := rl.Attributes
		a := all[b.rng.IntN(len(all))].Name
		names = []string{a}
	}
	bestName, bestTo, bestCost, bestScore := "", int64(0), int64(0), int64(-1)
	for i, a := range names {
		if !b.spend(1) {
			break
		}
		cur := b.attr(u, a, 0)
		// Wide ranges are bought in chunks (one upgrade order, one busy
		// period) rather than one point at a time.
		step := int64(1)
		if at, ok := rl.Attr(a); ok {
			step = max(1, (at.Max-at.Min)/16)
			if cur+step > at.Max {
				step = at.Max - cur
			}
		}
		if step <= 0 {
			continue
		}
		cost, ticks, ok := rl.UpgradeCost(a, cur, cur+step)
		if left := b.ticksLeft(); left >= 0 && ticks+5 > left {
			continue // would not finish before the time limit
		}
		if !ok || ticks > int64(b.P.PlanningHorizon) {
			continue
		}
		// Earlier plan entries matter more; cheaper is better.
		score := int64(len(names)-i) * 10000 / max(cost, 1)
		if score > bestScore {
			bestName, bestTo, bestCost, bestScore = a, cur+step, cost, score
		}
	}
	return bestName, bestTo, bestCost, bestName != ""
}

// planUpgrades upgrades units that can pay for it, and brings energy to
// those that cannot (bank transfers when adjacent, a trip home otherwise).
func (b *Bot) planUpgrades() {
	bank := b.bank()
	if bank == nil {
		return
	}
	bankE, _ := energy(bank)
	surplus := bankE - b.dir.Reserve
	if b.clonerID != 0 && len(b.K.OwnUnits()) < b.dir.Population && !b.muster {
		// Keep enough for the next clone unless the strategy favors tech.
		surplus -= b.clonerNeed * int64(1000-b.dir.UpgradeShare) / 1000
	}
	funded := false
	step := b.K.Rules.EnergyPerStep
	growing := len(b.K.OwnUnits()) < b.dir.Population*(300+b.P.EconomicEfficiency/2)/1000
	for _, u := range b.K.OwnUnits() {
		r := b.Roles[u.ID]
		if r == RoleBank || r == RoleNone || r.Extension() || u.ID == b.clonerID || busy(u) || !b.spend(2) {
			continue
		}
		e, capacity := energy(u)
		near := state.Cheb(u.Pos, bank.Pos) <= 1
		// An army mustering for an attack, and scouts about to leave, fill
		// up before anything else is funded.
		if near && !funded && bankE > b.dir.Reserve+10 && b.wantsFuel(u) {
			amt := min(capacity-e, bankE-b.dir.Reserve)
			b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy + 8, Desc: "fuel " + string(r) + " " + fmtID(u.ID), OneShot: true,
				Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: u.ID, Amount: amt}})
			funded = true
			bankE -= amt
			surplus -= amt
			continue
		}
		// Early on, idle energy is worth more in the bank (clones) than in
		// small upgrades: skilled bots pool it (never a scout's or a
		// mustering army's: they need it to move).
		if growing && near && r != RoleMiner && r != RoleScout && !(b.muster && r == RoleFighter) && u.ID != b.clonerID && e > step*20+10 && !funded {
			if _, bc := energy(bank); bankE+e-step*20 <= bc {
				b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy, Desc: "pool energy in bank " + fmtID(u.ID),
					OneShot: true,
					Payload: &energyv1.TransferCommand{FromUnitId: u.ID, ToUnitId: bank.ID, Amount: e - step*20}})
				funded = true
				continue
			}
		}
		// Units drained next to the bank block deliveries: a small top-up
		// comes before any savings.
		if near && b.low(u) && bankE >= 15 && !funded {
			amt := min(step*30, bankE-10)
			b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy + 8, Desc: "top up " + fmtID(u.ID), OneShot: true,
				Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: u.ID, Amount: amt}})
			funded = true
			bankE -= amt
			surplus -= amt
			continue
		}
		// Refuel units that came home low (fighters fill up for campaigns).
		if near && (b.low(u) || (r != RoleMiner && e*2 < capacity)) && surplus > capacity/4 && !funded {
			amt := min(capacity-e, surplus)
			b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy, Desc: "refuel " + fmtID(u.ID),
				OneShot: true,
				Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: u.ID, Amount: amt}})
			funded = true
			surplus -= amt
			continue
		}
		if growing || (b.muster && r == RoleFighter) {
			continue // a mustering army keeps its energy for the march
		}
		attr, to, cost, ok := b.nextUpgrade(u)
		if !ok {
			continue
		}
		// Never spend the energy needed to get home.
		margin := step*10 + int64(state.Cheb(u.Pos, bank.Pos))*step*3
		switch {
		case e >= cost+margin && r != RoleMiner:
			b.upgrade(u, attr, to)
		case r == RoleMiner && e >= cost+margin && u.Str("automation.state") == "delivering":
			b.upgrade(u, attr, to)
		case !funded && r != RoleMiner && surplus >= cost && cost+margin <= capacity && cost+margin > e && near:
			b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy, Desc: fmt.Sprintf("fund %s for %s", attr, fmtID(u.ID)),
				OneShot: true,
				Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: u.ID, Amount: cost + margin - e}})
			funded = true
			surplus -= cost
		}
	}
}

// planLogistics links the bank to fighters and guards with persistent
// supply links (skilled play): a link waits while its receiver is out of
// range and refills it whenever it comes back, so the army refuels by
// passing home. The server allows a few links per supplier; the bank's
// current links are read from its observed state.
func (b *Bot) planLogistics() {
	bank := b.bank()
	if bank == nil || !b.P.UseLogistics || !b.K.Rules.HasLogistics || busy(bank) {
		return
	}
	linked := map[uint64]bool{}
	for i := range 16 {
		if to := bank.IDAttr(fmt.Sprintf("logistics.supply.%d.to", i)); to != 0 {
			linked[to] = true
		}
	}
	if len(linked) >= 4 {
		return
	}
	// The emptiest unlinked fighter or guard (ties: lowest ID).
	var u *state.Entity
	var ue int64
	for _, c := range append(b.unitsByRole(RoleFighter), b.unitsByRole(RoleGuard)...) {
		if linked[c.ID] {
			continue
		}
		if e, _ := energy(c); u == nil || e < ue || (e == ue && c.ID < u.ID) {
			u, ue = c, e
		}
	}
	if u == nil {
		return
	}
	_, capacity := energy(u)
	b.issue(Order{Key: fmt.Sprintf("supply:%d", u.ID), Units: 1, Priority: PrioEconomy - 2, Desc: "supply link " + fmtID(u.ID),
		Payload: &logisticsv1.SupplyCommand{UnitIds: []uint64{bank.ID}, ReceiverId: u.ID, Reserve: b.dir.Reserve,
			FillTo: capacity / 2, ResumeBelow: capacity / 4}})
}

// clearBank moves idle units off the tiles around the bank so miners can
// always reach it to deliver (a ring of parked units starves the economy).
func (b *Bot) clearBank() {
	bank := b.bank()
	if bank == nil {
		return
	}
	for _, u := range b.K.OwnUnits() {
		r := b.Roles[u.ID]
		if r == RoleBank || r == RoleMiner || r.Extension() || u.ID == b.clonerID || busy(u) || state.Cheb(u.Pos, bank.Pos) != 1 ||
			u.Has("movement.destination") || u.Str("automation.behavior") == "guard" {
			continue
		}
		e, capacity := energy(u)
		if step := b.K.Rules.EnergyPerStep; e < step*2 {
			// Too empty to step away: a small emergency transfer.
			if be, _ := energy(bank); be >= 10 {
				b.issue(Order{Key: "bank:xfer", Units: 1, Priority: PrioEconomy + 6, Desc: "unstick " + fmtID(u.ID), OneShot: true,
					Payload: &energyv1.TransferCommand{FromUnitId: bank.ID, ToUnitId: u.ID, Amount: 10}})
			}
			continue
		}
		if e*2 < capacity && b.bankSurplus() >= capacity/4 {
			continue // about to be refueled
		}
		if b.wantsFuel(u) && b.bankSurplus() > 10 {
			continue // waiting to fill up for a march or a scouting trip
		}
		if t, ok := b.freeTileToward(bank.Pos, 2, 3, b.campPos()); ok && b.spend(1) {
			b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioEconomy, Essential: true, Desc: "clear the bank",
				Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(t)}})
		}
	}
}

// wantsFuel reports whether a unit should fill up at the bank before
// anything else is funded: fighters while the army musters for an attack,
// and scouts that cannot reach unexplored ground.
func (b *Bot) wantsFuel(u *state.Entity) bool {
	e, capacity := energy(u)
	switch b.Roles[u.ID] {
	case RoleFighter:
		return b.muster && e < min(b.musterFuel, capacity*9/10)
	case RoleScout:
		return e*4 < capacity*3
	}
	return false
}

// bankSurplus is what the bank can give beyond its reserve.
func (b *Bot) bankSurplus() int64 {
	bank := b.bank()
	if bank == nil {
		return 0
	}
	e, _ := energy(bank)
	return e - b.dir.Reserve
}

// freeTileToward returns the free tile at distance lo..hi from p closest
// to target (ties in scan order), and claims it for this tick.
func (b *Bot) freeTileToward(p state.Pos, lo, hi int32, target state.Pos) (state.Pos, bool) {
	occ := b.occupancy()
	best, found := state.Pos{}, false
	for dy := -hi; dy <= hi; dy++ {
		for dx := -hi; dx <= hi; dx++ {
			if d := max(dx, -dx, dy, -dy); d < lo {
				continue
			}
			q := state.Pos{X: p.X + dx, Y: p.Y + dy}
			if b.K.InBounds(q) && !b.K.Occupied(q, occ) && (!found || state.Cheb(q, target) < state.Cheb(best, target)) {
				best, found = q, true
			}
		}
	}
	if found {
		occ[best] = true
	}
	return best, found
}

// planGuards puts guards on guard automation around home.
func (b *Bot) planGuards() {
	for _, u := range b.unitsByRole(RoleGuard) {
		b.issue(Order{Key: fmt.Sprintf("auto:%d", u.ID), Units: 1, Priority: PrioRoles, Desc: "guard home",
			Payload: &automationv1.AutomateCommand{UnitIds: []uint64{u.ID}, Behavior: &automationv1.AutomateCommand_Guard{
				Guard: &automationv1.Guard{Position: pos(b.guardPost()), Radius: 6}}}})
	}
}

// guardPost is where guards hold: a few tiles from the bank toward the
// enemy, off the miners' delivery ring.
func (b *Bot) guardPost() state.Pos {
	g := b.enemyBaseGuess()
	return state.Pos{X: b.home.X + clamp(g.X-b.home.X, -3, 3), Y: b.home.Y + clamp(g.Y-b.home.Y, -3, 3)}
}

// planSalvage puts salvagers on auto-salvage automation.
func (b *Bot) planSalvage() {
	for _, u := range b.unitsByRole(RoleSalvager) {
		b.issue(Order{Key: fmt.Sprintf("auto:%d", u.ID), Units: 1, Priority: PrioRoles, Desc: "auto-salvage",
			Payload: &automationv1.AutomateCommand{UnitIds: []uint64{u.ID}, Behavior: &automationv1.AutomateCommand_AutoSalvage{
				AutoSalvage: &automationv1.AutoSalvage{Radius: 10}}}})
	}
}

func fmtID(id uint64) string { return fmt.Sprintf("%x", id&0xffffff) }

// planForage handles drained units. One that can still reach home walks
// back to be refueled by the bank; one that cannot heads for the nearest
// reachable known well and mines itself full, then resumes its role.
func (b *Bot) planForage() {
	step := b.K.Rules.EnergyPerStep
	bank := b.bank()
	if step <= 0 || bank == nil {
		return
	}
	for _, u := range b.K.OwnUnits() {
		r := b.Roles[u.ID]
		if r == RoleBank || r == RoleMiner || r.Extension() || busy(u) || !b.spend(2) {
			continue
		}
		e, capacity := energy(u)
		if w, ok := b.foraging[u.ID]; ok {
			well := b.K.Wells[w]
			switch {
			case e*4 >= capacity*3 || well == nil || well.Depleted:
				delete(b.foraging, u.ID)
				b.Cmd.Forget(fmt.Sprintf("move:%d", u.ID))
			case state.Cheb(u.Pos, well.Pos) <= 1 && !u.Has("energy.mining_well"):
				b.issue(Order{Key: fmt.Sprintf("forage:%d", u.ID), Units: 1, Priority: PrioEconomy + 10, Desc: "forage",
					Payload: &energyv1.MineCommand{UnitIds: []uint64{u.ID}, WellId: w}})
			}
			continue
		}
		if !b.low(u) {
			continue
		}
		if b.tripCost(u.Pos, bank.Pos) <= e {
			if state.Cheb(u.Pos, bank.Pos) > 1 {
				b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioEconomy + 10, Essential: true, Desc: "home to refuel",
					Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(b.freeNeighbor(bank.Pos))}})
			}
			continue
		}
		var best *state.Well
		for _, w := range b.K.UsableWells() {
			if b.tripCost(u.Pos, w.Pos) <= e+step && (best == nil || state.Cheb(u.Pos, w.Pos) < state.Cheb(u.Pos, best.Pos)) {
				best = w
			}
		}
		if best == nil {
			continue // stranded: nothing in reach
		}
		b.startForage(u, best)
	}
}

// startForage sends a unit to fill up at a well (planForage continues it
// until the unit is three quarters full).
func (b *Bot) startForage(u *state.Entity, w *state.Well) {
	b.foraging[u.ID] = w.ID
	if state.Cheb(u.Pos, w.Pos) > 1 {
		b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioEconomy + 10, Essential: true, Desc: "forage at well",
			Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(b.freeNeighbor(w.Pos))}})
	}
	b.issue(Order{Key: fmt.Sprintf("forage:%d", u.ID), Units: 1, Priority: PrioEconomy + 10, Desc: "forage",
		Payload: &energyv1.MineCommand{UnitIds: []uint64{u.ID}, WellId: w.ID}})
}

// nearestWell returns the closest usable known well to p.
func (b *Bot) nearestWell(p state.Pos) (*state.Well, int32) {
	var best *state.Well
	d := int32(1 << 30)
	for _, w := range b.K.UsableWells() {
		if c := state.Cheb(p, w.Pos); c < d || (c == d && w.ID < best.ID) {
			best, d = w, c
		}
	}
	return best, d
}
