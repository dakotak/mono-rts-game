package combat_test

import (
	"encoding/json"
	"fmt"
	"testing"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// CombatRules: every unit starts identical (attack 2, range 1, armor 0,
// structure 50); specialization comes from upgrades.
const CombatRules = `{
  "base_mass": 10,
  "attributes": {
    "attack":       {"default": 2,  "min": 0,  "max": 50,  "mass": "v",     "cost": "v * v * 2", "duration": "v * 2"},
    "attack_range": {"default": 1,  "min": 1,  "max": 6,   "mass": "v",     "cost": "(v - 1) * (v - 1) * 25", "duration": "v * 3"},
    "armor":        {"default": 0,  "min": 0,  "max": 20,  "mass": "v * 3", "cost": "v * v * 5", "duration": "v * 4"},
    "structure":    {"default": 50, "min": 50, "max": 500, "mass": "v / 25", "cost": "(v - 50) * 2", "duration": "(v - 50) / 10"},
    "propulsion":   {"default": 1,  "min": 1,  "max": 10,  "mass": "v",     "cost": "(v - 1) * 20", "duration": "v"}
  },
  "derived": {
    "attack_cooldown": "clamp(mass / 8, 1, 10)",
    "ticks_per_move":  "1"
  }
}`

const combatCfg = `{
  "max_health": {"property": "structure", "default": 100},
  "attack":     {"property": "attack", "default": 0},
  "range":      {"property": "attack_range", "default": 1},
  "cooldown":   {"property": "attack_cooldown", "default": 1},
  "damage":     "max(1, attacker_attack * 10 / (10 + target_armor))",
  "energy_cost": "1 + attacker_attack / 5"
}`

func harness(t *testing.T, overrides map[string]string) *pt.Harness {
	cfg := map[string]string{
		"attributes": CombatRules,
		"energy":     `{"unit_capacity": 1000, "initial_energy": 0}`,
		"movement":   `{"pace_attribute": "ticks_per_move"}`,
		"combat":     combatCfg,
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return pt.New(t, cfg)
}

func attack(target sim.EntityID, units ...sim.EntityID) *combatv1.AttackCommand {
	m := &combatv1.AttackCommand{TargetId: uint64(target)}
	for _, u := range units {
		m.UnitIds = append(m.UnitIds, uint64(u))
	}
	return m
}

func hp(h *pt.Harness, id sim.EntityID) int64 {
	v, ok := combat.GetHealth(h.W(), id)
	if !ok {
		return -1 // destroyed
	}
	return v.HP
}

func alive(h *pt.Harness, id sim.EntityID) bool {
	_, ok := h.W().Entity(id)
	return ok
}

func set(h *pt.Harness, u sim.EntityID, attr string, v int64) {
	i, ok := h.Attrs().Index(attr)
	if !ok {
		h.T.Fatalf("no attribute %s", attr)
	}
	h.Attrs().Apply(h.W(), u, i, v, 0)
}

func TestAttackValidation(t *testing.T) {
	h := harness(t, nil)
	a := h.Unit(1, 0, 0, 100)
	friend := h.Unit(1, 1, 0, 100)
	enemy := h.Unit(2, 0, 1, 100)
	unarmed := h.Unit(1, 5, 5, 100)
	set(h, unarmed, "attack", 0)
	wellID, _ := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: 9, Y: 9})
	h.Step()
	cases := []struct {
		name string
		slot int
		cmd  *combatv1.AttackCommand
		code string
	}{
		{"friendly target", 1, attack(friend, a), sim.CodePermissionDenied},
		{"self", 1, attack(a, a), sim.CodePermissionDenied},
		{"not your unit", 2, attack(friend, a), sim.CodePermissionDenied},
		{"unknown target", 1, attack(4242, a), sim.CodeNotFound},
		{"non-unit target", 1, attack(wellID, a), sim.CodeNotFound},
		{"unknown attacker", 1, attack(enemy, 777), sim.CodeNotFound},
		{"no attack capability", 1, attack(enemy, unarmed), sim.CodeFailedPrecondition},
		{"no units", 1, attack(enemy), sim.CodeInvalidArgument},
		{"duplicate", 1, attack(enemy, a, a), sim.CodeInvalidArgument},
	}
	for _, c := range cases {
		id := h.Submit(c.slot, combat.CommandAttack, c.cmd)
		if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	// Players cannot use the admin health override.
	if _, err := h.Engine.Submit(sim.Command{Issuer: sim.Principal{Player: h.P(1)}, Type: combat.CommandSetHealth, Payload: &combatv1.SetHealthCommand{}}); err == nil {
		t.Fatal("player may set health")
	}
	// A valid order is accepted and visible to the owner only.
	id := h.Submit(1, combat.CommandAttack, attack(enemy, a))
	f := h.Step()
	if r, _ := pt.Result(f, id); !r.OK {
		t.Fatalf("valid attack: %+v", r)
	}
}

func TestRangeCooldownAndEnergy(t *testing.T) {
	h := harness(t, nil)
	a := h.Unit(1, 0, 0, 100)
	target := h.Unit(2, 3, 0, 0) // distance 3, range 1
	h.Step()
	// Defaults: attack 2, damage max(1, 2*10/10) = 2, cost 1 + 2/5 = 1,
	// mass 10+2+1+0+2+1 = 16 -> cooldown 2.
	h.Submit(1, combat.CommandAttack, attack(target, a))
	fs := h.Steps(3)
	if hp(h, target) != 50 {
		t.Fatal("attacked out of range")
	}
	if ev := pt.Events(fs, combat.EventOutOfRange); len(ev) != 1 || pt.Field(ev[0], "distance").Int != 3 {
		t.Fatalf("out-of-range events = %+v", ev) // once per episode
	}
	// Bring the target into range (no pursuit: we move the target).
	h.Submit(0, sim.CmdMoveEntity, sim.MoveEntity{ID: target, To: sim.Pos{X: 1, Y: 1}})
	var hps []int64
	for range 6 {
		h.Step()
		hps = append(hps, hp(h, target))
	}
	// Fires on the first tick in range, then every 2 ticks.
	if fmt.Sprint(hps) != "[48 48 46 46 44 44]" {
		t.Fatalf("health per tick = %v", hps)
	}
	if h.Energy(a) != 100-3 {
		t.Fatalf("energy spent = %d, want 3", 100-h.Energy(a))
	}
	// Out of energy: the order waits (stalled once) and resumes on refuel.
	energy.SetStored(h.W(), a, 0)
	fs = h.Steps(4)
	if hp(h, target) != 44 || len(pt.Events(fs, combat.EventStalled)) != 1 {
		t.Fatalf("hp %d stalled %v", hp(h, target), pt.Events(fs, combat.EventStalled))
	}
	energy.SetStored(h.W(), a, 1)
	h.Step()
	if hp(h, target) != 42 || h.Energy(a) != 0 {
		t.Fatalf("after refuel hp %d energy %d", hp(h, target), h.Energy(a))
	}
}

func TestArmorMitigation(t *testing.T) {
	h := harness(t, nil)
	var targets []sim.EntityID
	for i, armor := range []int64{0, 5, 10, 30 - 10} {
		a := h.Unit(1, int32(i*3), 0, 100)
		set(h, a, "attack", 20)
		tg := h.Unit(2, int32(i*3), 1, 0)
		set(h, tg, "armor", armor)
		targets = append(targets, tg)
		h.Submit(1, combat.CommandAttack, attack(tg, a))
	}
	h.Step()
	var dmg []int64
	for _, tg := range targets {
		dmg = append(dmg, 50-hp(h, tg))
	}
	// damage = max(1, 20*10/(10+armor)): 20, 13, 10, 6 — diminishing.
	if fmt.Sprint(dmg) != "[20 13 10 6]" {
		t.Fatalf("damage by armor = %v", dmg)
	}
}

func TestSimultaneousMutualDestruction(t *testing.T) {
	h := harness(t, nil)
	a := h.Unit(1, 5, 5, 100)
	b := h.Unit(2, 6, 5, 100)
	for _, u := range []sim.EntityID{a, b} {
		set(h, u, "attack", 30)
	}
	// Both at 20 integrity: each would kill the other with one hit.
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(a), Health: 20})
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(b), Health: 20})
	h.Submit(1, combat.CommandAttack, attack(b, a))
	h.Submit(2, combat.CommandAttack, attack(a, b))
	f := h.Step()
	if alive(h, a) || alive(h, b) {
		t.Fatal("a unit destroyed this tick did not get to fire")
	}
	if n := len(pt.Events([]*sim.Frame{f}, combat.EventAttack)); n != 2 {
		t.Fatalf("%d attack events, want 2", n)
	}
	// Each player hears about both destructions (own unit + its victim).
	dest := pt.Events([]*sim.Frame{f}, combat.EventDestroyed)
	if len(dest) != 4 {
		t.Fatalf("destroyed events = %+v", dest)
	}
}

func TestMultipleAttackersAggregate(t *testing.T) {
	h := harness(t, nil)
	target := h.Unit(2, 10, 10, 0)
	var attackers []sim.EntityID
	for _, p := range [][2]int32{{9, 9}, {10, 9}, {11, 9}, {9, 10}, {11, 10}} {
		u := h.Unit(1, p[0], p[1], 100)
		set(h, u, "attack", 11)
		attackers = append(attackers, u)
	}
	h.Submit(1, combat.CommandAttack, attack(target, attackers...))
	f := h.Step()
	// 5 x 11 = 55 >= 50: destroyed in one tick, one damaged event with the
	// aggregate.
	if alive(h, target) {
		t.Fatal("target survived 55 damage")
	}
	dmg := pt.Events([]*sim.Frame{f}, combat.EventDamaged)
	if len(dmg) != 1 || pt.Field(dmg[0], "damage").Int != 55 || pt.Field(dmg[0], "attackers").Int != 5 || pt.Field(dmg[0], "health").Int != 0 {
		t.Fatalf("damaged = %+v", dmg)
	}
	// Orders on the dead target end next tick.
	fs := h.Steps(1)
	if ev := pt.Events(fs, combat.EventEnded); len(ev) != 5 || pt.Field(ev[0], "reason").Str != "target_lost" {
		t.Fatalf("ended = %+v", ev)
	}
}

func TestMovingTargetLeavesRange(t *testing.T) {
	h := harness(t, nil)
	a := h.Unit(1, 0, 0, 100)
	set(h, a, "attack", 1) // damage 1, cooldown 1? mass 10+1+1+0+2+1=15 -> 1
	target := h.Unit(2, 1, 0, 100)
	h.Submit(1, combat.CommandAttack, attack(target, a))
	h.Steps(2)
	if hp(h, target) != 48 {
		t.Fatalf("hp = %d", hp(h, target))
	}
	// The target walks away; the attacker does not pursue.
	h.Submit(2, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(target), Destination: &rtsv1.Position{X: 6, Y: 0}}}})
	fs := h.Steps(6)
	if e, _ := h.W().Entity(a); e.Pos != (sim.Pos{}) {
		t.Fatal("attacker moved")
	}
	if len(pt.Events(fs, combat.EventOutOfRange)) != 1 {
		t.Fatal("no out_of_range event")
	}
	// Movement happens before combat, so the step that leaves range takes
	// effect before the attack that tick: no further hits after the first
	// move tick.
	if hp(h, target) != 48 {
		t.Fatalf("hit while out of range: %d", hp(h, target))
	}
	// The order persists and resumes when the target returns.
	h.Submit(2, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(target), Destination: &rtsv1.Position{X: 1, Y: 1}}}})
	h.Steps(6)
	if hp(h, target) >= 48 {
		t.Fatal("did not resume when the target came back")
	}
}

func TestDestructionCancelsEverything(t *testing.T) {
	h := harness(t, map[string]string{"energy": `{"unit_capacity": 1000, "initial_energy": 0}`})
	victim := h.Unit(2, 5, 5, 500)
	other := h.Unit(2, 7, 7, 0)
	killer := h.Unit(1, 4, 5, 100) // within range of (5,5) and (5,6)
	set(h, killer, "attack", 50)
	// The victim is upgrading, moving and has its own attack order.
	h.Submit(2, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(victim)}, Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Target{Target: 3}})
	h.Submit(2, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(victim), Destination: &rtsv1.Position{X: 5, Y: 6}}}})
	h.Submit(2, combat.CommandAttack, attack(killer, victim))
	h.Step()
	h.Submit(1, combat.CommandAttack, attack(victim, killer))
	set(h, victim, "attack", 0) // keep the killer alive for this test
	f := h.Step()
	if alive(h, victim) {
		t.Fatal("victim survived")
	}
	// Every component (orders, upgrade, busy mark) left with the entity.
	if len(f.Removed) != 1 || f.Removed[0] != victim {
		t.Fatalf("removed = %v", f.Removed)
	}
	if _, busy := h.Attrs().Busy(h.W(), victim); busy {
		t.Fatal("upgrade reservation outlived the unit")
	}
	// It does not act afterwards: no upgrade completion, no movement.
	fs := h.Steps(20)
	if len(pt.Events(fs, upgrades.EventCompleted)) != 0 {
		t.Fatal("destroyed unit completed an upgrade")
	}
	// The victim hit the killer once (tick 1) before being disarmed.
	if !alive(h, other) || hp(h, killer) != 48 {
		t.Fatalf("collateral changes: other alive %v, killer hp %d", alive(h, other), hp(h, killer))
	}
}

func TestOutcomeIndependentOfIDs(t *testing.T) {
	// Two mirror-image duels: whichever side has the lower entity ID, the
	// result is the same (both die, or both survive with equal health).
	h := harness(t, nil)
	var pairs [][2]sim.EntityID
	for i := range 6 {
		a := h.Unit(1, int32(i*3), 0, 100)
		b := h.Unit(2, int32(i*3)+1, 0, 100)
		set(h, a, "attack", 25)
		set(h, b, "attack", 25)
		pairs = append(pairs, [2]sim.EntityID{a, b})
		h.Submit(1, combat.CommandAttack, attack(b, a))
		h.Submit(2, combat.CommandAttack, attack(a, b))
	}
	h.Steps(1)
	for _, pr := range pairs {
		if hp(h, pr[0]) != hp(h, pr[1]) || hp(h, pr[0]) != 25 {
			t.Fatalf("asymmetric duel: %d vs %d", hp(h, pr[0]), hp(h, pr[1]))
		}
	}
	h.Steps(10)
	for _, pr := range pairs {
		if alive(h, pr[0]) || alive(h, pr[1]) {
			t.Fatal("mirror duel did not end in mutual destruction")
		}
	}
}

func TestStructureUpgradeRepairsAndStatsQuote(t *testing.T) {
	h := harness(t, nil)
	u := h.Unit(1, 0, 0, 400)
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(u), Health: 30})
	h.Step()
	// structure 50 -> 70 costs 40, takes 2 ticks; integrity rises by 20.
	h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(u)}, Attribute: "structure", Amount: &upgradesv1.UpgradeCommand_Target{Target: 70}})
	f := h.Steps(2)[1]
	hh, _ := combat.GetHealth(h.W(), u)
	if hh.HP != 50 || hh.Max != 70 {
		t.Fatalf("health = %+v", hh)
	}
	stats := map[string]int64{}
	for _, a := range f.Changed[0].Attrs {
		stats[a.Key] = a.Value.Int
	}
	if stats["combat.attack"] != 2 || stats["combat.range"] != 1 || stats["combat.attack_cost"] != 1 || stats["combat.max_health"] != 70 {
		t.Fatalf("stats = %v", stats)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown property":        `{"attack": {"property": "nope"}}`,
		"zero cooldown":           `{"cooldown": {"default": 0}}`,
		"zero max health":         `{"max_health": {"default": 0}}`,
		"negative attack default": `{"attack": {"default": -1}}`,
		"bad damage formula":      `{"damage": "attacker_attack +"}`,
		"unknown damage variable": `{"damage": "attacker_charisma"}`,
		"bare variable":           `{"damage": "attack"}`,
		"target in energy cost":   `{"energy_cost": "target_armor"}`,
		"dynamic energy cost":     `{"energy_cost": "attacker_health"}`,
		"distance in energy cost": `{"energy_cost": "distance"}`,
		"unknown field":           `{"damages": "1"}`,
	} {
		c := &game.Config{
			World: game.WorldConfig{Width: 5, Height: 5, TickRateHz: 10},
			Plugins: map[string]json.RawMessage{
				"attributes": json.RawMessage(CombatRules),
				"combat":     json.RawMessage(cfg),
			},
		}
		if _, err := game.New(c); err == nil {
			t.Errorf("%s: accepted %s", name, cfg)
		}
	}
	// The default configuration works without any attributes.
	if _, err := game.New(&game.Config{World: game.WorldConfig{Width: 5, Height: 5, TickRateHz: 10}}); err != nil {
		t.Errorf("defaults: %v", err)
	}
}
