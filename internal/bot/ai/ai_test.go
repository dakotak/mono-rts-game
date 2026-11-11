package ai

import (
	"fmt"
	"slices"
	"testing"
	"time"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"google.golang.org/protobuf/proto"
)

// homeBase is six own units around (5,5) and two wells nearby.
func homeBase() []*rtsv1.Entity {
	es := []*rtsv1.Entity{well(1001, 8, 5, 600), well(1002, 5, 9, 600)}
	for i := range 6 {
		es = append(es, unit(uint64(100+i), me, 4+int32(i%3), 4+int32(i/3), nil))
	}
	return es
}

func TestStrategySelectionReactsToThreat(t *testing.T) {
	es := homeBase()
	for i := range 6 {
		es = append(es, unit(uint64(900+i), foe, 7+int32(i%2), 6+int32(i/2), map[string]int64{"combat.attack": 4}))
	}
	b := newBot(t, 10, observe(t, 400, es, nil))
	b.Step()
	if b.strategy.name != "defensive" {
		t.Fatalf("strategy with a strong enemy force at home = %q (scores %v), want defensive", b.strategy.name, b.strategy.scores)
	}
}

// constHeuristic favors one strategy by a fixed amount.
func constHeuristic(s string, v int) Heuristic {
	return HeuristicFunc{N: "test-" + s, F: func(_ *Features, name string) int {
		if name == s {
			return v
		}
		return 0
	}}
}

func TestStrategyHysteresis(t *testing.T) {
	b := newBot(t, 10, observe(t, 0, homeBase(), nil))
	eval := func() { b.ops = b.P.ComputeBudget; b.strategic() }
	eval()
	first := b.strategy.name
	// A slightly better alternative does not cause a switch...
	b.heuristics = append(b.heuristics, constHeuristic("tech", 100))
	b.K.Tick = uint64(b.P.MinCommitment) * 2
	eval()
	if b.strategy.name != first && b.strategy.scores["tech"] < b.strategy.scores[first]+150 {
		t.Fatalf("switched to %q on a small margin", b.strategy.name)
	}
	// ...a much better one does, but only after the minimum commitment.
	b.heuristics = append(b.heuristics, constHeuristic("tech", 5000))
	b.strategy.since = b.K.Tick
	b.K.Tick++
	eval()
	if b.strategy.name == "tech" {
		t.Fatal("switched before the minimum commitment elapsed")
	}
	b.K.Tick += uint64(b.P.MinCommitment)
	eval()
	if b.strategy.name != "tech" {
		t.Fatalf("strategy = %q, want tech after commitment", b.strategy.name)
	}
}

func TestRoleAllocationAndBank(t *testing.T) {
	es := homeBase()
	for i := range 10 {
		es = append(es, unit(uint64(200+i), me, 2+int32(i), 12, nil))
	}
	b := newBot(t, 8, observe(t, 100, es, nil))
	b.ops = b.P.ComputeBudget
	b.strategic()
	b.ops = b.P.ComputeBudget
	b.operational()
	if b.bank() == nil {
		t.Fatal("no bank chosen")
	}
	count := map[Role]int{}
	for _, r := range b.Roles {
		count[r]++
	}
	if count[RoleBank] != 1 {
		t.Fatalf("banks = %d, want 1", count[RoleBank])
	}
	maxMiners := 0
	for _, w := range b.homeWells() {
		maxMiners += b.wellCap(w)
	}
	if count[RoleMiner] == 0 || count[RoleMiner] > maxMiners {
		t.Fatalf("miners = %d, want 1..%d (well capacity)", count[RoleMiner], maxMiners)
	}
	if count[RoleFighter] == 0 {
		t.Fatalf("no fighters among %v", count)
	}
	// Roles are tagged through public batch metadata commands, one per
	// role.
	b.flushTags()
	tagged := 0
	for _, m := range payloadsOf[*metadatav1.SetMetadataCommand](b.Cmd.Flush(b.K.Tick)) {
		if m.GetKey() != "role" || Role(m.GetValue()) == RoleNone {
			t.Fatalf("bad tag %v", m)
		}
		tagged += len(m.GetUnitIds())
	}
	if tagged != len(es)-2 {
		t.Fatalf("%d units tagged, want %d", tagged, len(es)-2)
	}
}

func TestSpecializationFollowsRole(t *testing.T) {
	b := newBot(t, 10, observe(t, 100, homeBase(), nil))
	u := b.K.OwnUnits()[0]
	for _, r := range []Role{RoleFighter, RoleMiner, RoleScout} {
		b.Roles[u.ID] = r
		plan := b.rolePlan(r)
		for range 20 {
			b.ops = 1000
			attr, to, cost, ok := b.nextUpgrade(u)
			if !ok {
				t.Fatalf("%s: no upgrade", r)
			}
			if !slices.Contains(plan, attr) {
				t.Fatalf("%s: upgrade %s not in the role plan %v", r, attr, plan)
			}
			if to <= b.attr(u, attr, 0) || cost <= 0 {
				t.Fatalf("%s: bad upgrade %s to %d cost %d", r, attr, to, cost)
			}
		}
	}
	// An unskilled bot wanders off-plan sometimes.
	lo := newBot(t, 1, observe(t, 100, homeBase(), nil))
	lo.Roles[u.ID] = RoleFighter
	off := 0
	for range 50 {
		lo.ops = 1000
		if attr, _, _, ok := lo.nextUpgrade(lo.K.OwnUnits()[0]); ok && !slices.Contains(lo.rolePlan(RoleFighter), attr) {
			off++
		}
	}
	if off == 0 {
		t.Fatal("level 1 never upgraded off-plan")
	}
}

func TestTargetSelectionFocusesWeakest(t *testing.T) {
	es := homeBase()
	es = append(es, unit(901, foe, 9, 9, map[string]int64{"combat.health": 50}), unit(902, foe, 10, 10, map[string]int64{"combat.health": 8}))
	b := newBot(t, 10, observe(t, 100, es, nil))
	b.ops = 1000
	b.engage(b.K.OwnUnits(), b.K.Enemies(), "attack")
	atk := payloadsOf[*combatv1.AttackCommand](b.Cmd.Flush(b.K.Tick))
	if len(atk) != 1 || atk[0].GetTargetId() != 902 || len(atk[0].GetUnitIds()) < 2 {
		t.Fatalf("coordinated attack = %v, want one focus-fire order on the weak unit 902", atk)
	}
	// An uncoordinated bot gives per-unit orders.
	lo := newBot(t, 1, observe(t, 100, es, nil))
	lo.ops = 1000
	lo.engage(lo.K.OwnUnits(), lo.K.Enemies(), "attack")
	if n := len(payloadsOf[*combatv1.AttackCommand](lo.Cmd.Flush(lo.K.Tick))); n < 2 {
		t.Fatalf("level 1 attack orders = %d, want per-unit orders", n)
	}
}

func TestRetreatWhenOutmatched(t *testing.T) {
	es := []*rtsv1.Entity{unit(100, me, 20, 20, nil)}
	for i := range 5 {
		es = append(es, unit(uint64(900+i), foe, 22+int32(i), 22, map[string]int64{"combat.attack": 6}))
	}
	hi := newBot(t, 10, observe(t, 100, es, nil))
	if !hi.shouldRetreat(hi.K.OwnUnits(), hi.K.Enemies()) {
		t.Fatal("level 10 does not retreat from a hopeless fight")
	}
	lo := newBot(t, 1, observe(t, 100, es, nil))
	if lo.shouldRetreat(lo.K.OwnUnits(), lo.K.Enemies()) {
		t.Fatal("level 1 retreats (it has no retreat logic)")
	}
	// Even odds: no retreat.
	even := newBot(t, 10, observe(t, 100, []*rtsv1.Entity{unit(100, me, 20, 20, nil), unit(900, foe, 22, 22, nil)}, nil))
	if even.shouldRetreat(even.K.OwnUnits(), even.K.Enemies()) {
		t.Fatal("retreats from an even fight")
	}
}

func TestExplorationTargetsUnexploredReachableSectors(t *testing.T) {
	// Only the top-left 16x16 is explored.
	k := observe(t, 100, homeBase(), &[4]int32{0, 0, 16, 16})
	b := newBot(t, 10, k)
	b.home = state.Pos{X: 5, Y: 5}
	for range 10 {
		b.ops = 10000
		p, ok := b.chooseSector(b.home, map[state.Pos]bool{}, 100)
		if !ok {
			t.Fatal("no sector chosen")
		}
		if b.unexploredIn(sectorOf(p)) == 0 {
			t.Fatalf("chose explored sector %v", p)
		}
	}
	// Out of reach: nothing (a scout must not strand itself).
	b.ops = 10000
	if p, ok := b.chooseSector(b.home, map[state.Pos]bool{}, 4); ok {
		t.Fatalf("chose %v beyond reach", p)
	}
}

func TestFogKnowledgeOnlyFromObservations(t *testing.T) {
	// A foreign unit outside the visible area is simply not in the
	// snapshot; the bot cannot know it and its guess for the enemy base is
	// the mirror of home, not the real position.
	k := observe(t, 100, homeBase(), &[4]int32{0, 0, 12, 12})
	b := newBot(t, 10, k)
	b.home = state.Pos{X: 5, Y: 5}
	if len(k.Enemies()) != 0 {
		t.Fatal("enemies known without observation")
	}
	if g := b.enemyBaseGuess(); g != (state.Pos{X: 42, Y: 26}) {
		t.Fatalf("enemy base guess = %v, want the mirror of home", g)
	}
	// Seen once, then gone: remembered (not current), and it ages out.
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 101, Changed: []*rtsv1.Entity{unit(900, foe, 10, 10, nil)}})
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 102, Removed: []uint64{900}})
	if len(k.Enemies()) != 0 || len(k.RememberedEnemies()) != 1 {
		t.Fatalf("after losing sight: current %d remembered %d", len(k.Enemies()), len(k.RememberedEnemies()))
	}
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 102 + state.MemoryTicks + 1})
	if len(k.RememberedEnemies()) != 0 {
		t.Fatal("memory never ages out")
	}
}

func TestCommandBudgetAndPriority(t *testing.T) {
	c := newCommander(3, "x")
	for i := range 5 {
		c.Issue(1, Order{Key: fmt.Sprint(i), Units: 1, Priority: i * 10, Payload: &energyv1.StopMiningCommand{UnitIds: []uint64{uint64(i)}}})
	}
	out := c.Flush(1)
	if len(out) != 3 {
		t.Fatalf("flushed %d, budget 3", len(out))
	}
	if out[0].Order.Priority != 40 || out[2].Order.Priority != 20 {
		t.Fatalf("not highest priority first: %v %v", out[0].Order.Priority, out[2].Order.Priority)
	}
	if c.Stats.Deferred != 2 {
		t.Fatalf("deferred = %d", c.Stats.Deferred)
	}
	// A multi-unit order is charged per unit.
	c.Issue(2, Order{Key: "big", Units: 4, Payload: &energyv1.StopMiningCommand{}})
	if out := c.Flush(2); len(out) != 0 {
		t.Fatal("a 4-unit order exceeded the 3-unit budget")
	}
}

func TestCommandDedupeOneShotAndSupersede(t *testing.T) {
	c := newCommander(100, "x")
	stand := Order{Key: "auto:1", Units: 1, Payload: &energyv1.StopMiningCommand{UnitIds: []uint64{1}}}
	c.Issue(1, stand)
	out := c.Flush(1)
	c.Issue(2, stand)
	if len(c.Flush(2)) != 0 {
		t.Fatal("standing order repeated")
	}
	c.Issue(1+Refresh, stand)
	if len(c.Flush(1+Refresh)) != 1 {
		t.Fatal("standing order not refreshed")
	}
	// A failure allows an immediate retry.
	_ = out
	last := c.Flush(1 + Refresh)
	_ = last
	c.Issue(Refresh+2, stand)
	if len(c.Flush(Refresh+2)) != 0 {
		t.Fatal("repeated before failure")
	}
	for id := range c.pending {
		c.Results([]*rtsv1.CommandResult{{CommandId: id, Status: rtsv1.CommandStatus_COMMAND_STATUS_FAILED, ErrorCode: "not_found"}})
	}
	c.Issue(Refresh+3, stand)
	if len(c.Flush(Refresh+3)) != 1 {
		t.Fatal("failed order not retried")
	}
	// One-shot orders may repeat once their result arrived.
	shot := Order{Key: "xfer", Units: 1, OneShot: true, Payload: &energyv1.TransferCommand{FromUnitId: 1, ToUnitId: 2, Amount: 5}}
	c.Issue(10, shot)
	o := c.Flush(10)
	c.Issue(11, shot)
	if len(c.Flush(11)) != 0 {
		t.Fatal("one-shot repeated before its result")
	}
	c.Results([]*rtsv1.CommandResult{{CommandId: o[0].ID, Status: rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED}})
	c.Issue(12, shot)
	if len(c.Flush(12)) != 1 {
		t.Fatal("one-shot not repeatable after success")
	}
	// The later order for a key in the same tick wins.
	c.Issue(20, Order{Key: "auto:5", Units: 1, Desc: "cancel", Payload: &energyv1.StopMiningCommand{UnitIds: []uint64{5}}})
	c.Issue(20, Order{Key: "auto:5", Units: 1, Desc: "mine", Payload: &energyv1.MineCommand{UnitIds: []uint64{5}, WellId: 9}})
	if out := c.Flush(20); len(out) != 1 || out[0].Order.Desc != "mine" {
		t.Fatalf("same-key orders = %v", out)
	}
}

func TestFailedOrderIsReplanned(t *testing.T) {
	b := newBot(t, 8, observe(t, 100, homeBase(), nil))
	b.Step()
	var id string
	for pid, o := range b.Cmd.pending {
		if o.Desc == "mine and deliver" {
			id = pid
			break
		}
	}
	if id == "" {
		t.Fatal("no mining order issued")
	}
	before := len(b.mining)
	b.K.BeginTick()
	b.K.ApplyUpdate(&rtsv1.TickUpdate{Tick: 101, CommandResults: []*rtsv1.CommandResult{{CommandId: id, Status: rtsv1.CommandStatus_COMMAND_STATUS_FAILED, ErrorCode: "not_found", ErrorMessage: "well gone"}}})
	b.Step()
	if b.Status.Commands.Failed != 1 {
		t.Fatalf("failures = %d", b.Status.Commands.Failed)
	}
	if len(b.mining) >= before && before > 0 {
		// The assignment was dropped and (possibly) re-made on the next
		// operational pass; either way the order may be sent again.
		if _, ok := b.Cmd.last[b.Cmd.pending[id].Key]; ok {
			t.Fatal("failed order still deduplicated")
		}
	}
}

func TestDecisionsAreDeterministic(t *testing.T) {
	run := func() [][]byte {
		b := newBot(t, 7, observe(t, 0, homeBase(), nil))
		var all [][]byte
		for tick := uint64(1); tick < 200; tick++ {
			b.K.BeginTick()
			b.K.ApplyUpdate(&rtsv1.TickUpdate{Tick: tick})
			for _, o := range b.Step() {
				data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(o.Payload)
				all = append(all, append([]byte(o.ID+"|"), data...))
			}
		}
		return all
	}
	a, c := run(), run()
	if len(a) == 0 || len(a) != len(c) {
		t.Fatalf("runs issued %d and %d commands", len(a), len(c))
	}
	for i := range a {
		if string(a[i]) != string(c[i]) {
			t.Fatalf("command %d differs", i)
		}
	}
}

func TestLargePopulationStaysWithinBudgets(t *testing.T) {
	for _, n := range []int{1000, 10000} {
		es := []*rtsv1.Entity{well(1, 50, 50, 600), well(2, 60, 60, 600)}
		for i := range n {
			es = append(es, unit(uint64(10+i), me, int32(i%120), int32(i/120), nil))
		}
		k := observe(t, 0, es, nil)
		k.Width, k.Height = 120, 100
		b := newBot(t, 10, k)
		var worst time.Duration
		units := 0
		for tick := uint64(1); tick <= 30; tick++ {
			k.BeginTick()
			k.ApplyUpdate(&rtsv1.TickUpdate{Tick: tick})
			start := time.Now()
			out := b.Step()
			worst = max(worst, time.Since(start))
			charged := 0
			for _, o := range out {
				charged += o.Order.Units
			}
			if charged > k.Budget {
				t.Fatalf("n=%d tick %d: %d unit-commands > budget %d", n, tick, charged, k.Budget)
			}
			units += charged
		}
		t.Logf("n=%d: worst decision %v, unit-commands %d in 30 ticks, compute budget exhausted %d times", n, worst, units, b.Status.BudgetExhausted)
		if worst > 2*time.Second {
			t.Fatalf("n=%d: a decision took %v", n, worst)
		}
	}
}
