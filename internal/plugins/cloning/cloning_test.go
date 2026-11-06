package cloning_test

import (
	"encoding/json"
	"testing"

	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Large fixed stores; new units would get 30 free energy, which clones
// must NOT receive.
const store = `{"unit_capacity": 1000, "initial_energy": 30}`

func harness(t *testing.T, cloningCfg string) *pt.Harness {
	cfg := map[string]string{"energy": store}
	if cloningCfg != "" {
		cfg["cloning"] = cloningCfg
	}
	return pt.New(t, cfg)
}

func clone(dir cloningv1.Direction, units ...sim.EntityID) *cloningv1.CloneCommand {
	m := &cloningv1.CloneCommand{Direction: dir}
	for _, u := range units {
		m.UnitIds = append(m.UnitIds, uint64(u))
	}
	return m
}

func unitsOf(h *pt.Harness, owner sim.PlayerID) []sim.EntityID {
	var out []sim.EntityID
	for _, id := range h.W().EntityIDs() {
		if e, _ := h.W().Entity(id); e.Owner == owner {
			out = append(out, id)
		}
	}
	return out
}

func totalEnergy(h *pt.Harness) int64 {
	var sum int64
	for _, id := range h.W().EntityIDs() {
		sum += h.Energy(id)
	}
	return sum
}

func TestCloneCostInheritanceAndAccounting(t *testing.T) {
	h := harness(t, `{"base_cost": 40, "cost": "base + embodied", "duration": "5 + embodied / 50", "offspring_energy": 15}`)
	parent := h.Unit(1, 10, 10, 600)
	// Upgrade armor 0 -> 4 (cost 5*16 + 40 = 120) and propulsion 1 -> 2 (20).
	h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(parent)}, Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Target{Target: 4}})
	h.Steps(16)
	h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(parent)}, Attribute: "propulsion", Amount: &upgradesv1.UpgradeCommand_Target{Target: 2}})
	h.Steps(6)
	if h.Prop(parent, "embodied") != 140 || h.Energy(parent) != 460 {
		t.Fatalf("setup: embodied %d energy %d", h.Prop(parent, "embodied"), h.Energy(parent))
	}

	before := totalEnergy(h)
	id := h.Submit(1, cloning.CommandClone, clone(cloningv1.Direction_DIRECTION_EAST, parent))
	f := h.Step()
	if r, _ := pt.Result(f, id); !r.OK {
		t.Fatalf("clone rejected: %+v", r)
	}
	// cost = 40 + 140 = 180, plus 15 moved to the child; duration 5 + 2 = 7.
	st := pt.Events([]*sim.Frame{f}, cloning.EventStarted)
	if len(st) != 1 || pt.Field(st[0], "cost").Int != 180 || pt.Field(st[0], "duration").Int != 7 {
		t.Fatalf("started = %+v", st)
	}
	if h.Energy(parent) != 460-180-15 {
		t.Fatalf("parent energy = %d", h.Energy(parent))
	}
	fs := h.Steps(5)
	if len(unitsOf(h, h.P(1))) != 1 {
		t.Fatal("offspring appeared early")
	}
	fs = append(fs, h.Step()) // 7th tick
	done := pt.Events(fs, cloning.EventCompleted)
	if len(done) != 1 {
		t.Fatalf("completed = %+v", done)
	}
	child := sim.EntityID(0)
	for _, u := range unitsOf(h, h.P(1)) {
		if u != parent {
			child = u
		}
	}
	ce, _ := h.W().Entity(child)
	if ce.Pos != (sim.Pos{X: 11, Y: 10}) || ce.Owner != h.P(1) {
		t.Fatalf("child = %+v", ce)
	}
	// Inherited attributes; investment carried over; only offspring_energy.
	for _, attr := range []string{"armor", "propulsion", "energy_capacity", "mining_efficiency", "mass"} {
		if h.Prop(child, attr) != h.Prop(parent, attr) {
			t.Errorf("%s: child %d parent %d", attr, h.Prop(child, attr), h.Prop(parent, attr))
		}
	}
	if h.Prop(child, "invested") != 140 || h.Energy(child) != 15 {
		t.Fatalf("child invested %d energy %d", h.Prop(child, "invested"), h.Energy(child))
	}
	// No net energy: the world lost exactly the clone cost.
	if after := totalEnergy(h); before-after != 180 {
		t.Fatalf("energy before %d after %d: lost %d, want 180", before, after, before-after)
	}
	if _, busy := h.Attrs().Busy(h.W(), parent); busy {
		t.Fatal("parent still busy")
	}
	if child == parent || child == 0 {
		t.Fatal("offspring did not get a fresh ID")
	}
}

func TestCostNeverBelowEmbodied(t *testing.T) {
	h := harness(t, `{"base_cost": 0, "cost": "1", "duration": "1", "offspring_energy": 0}`)
	p := h.Unit(1, 5, 5, 900)
	a := h.Attrs()
	i, _ := a.Index("armor")
	a.Apply(h.W(), p, i, 10, 600) // embodied 600
	id := h.Submit(1, cloning.CommandClone, clone(0, p))
	f := h.Step()
	if r, _ := pt.Result(f, id); !r.OK {
		t.Fatalf("%+v", r)
	}
	if h.Energy(p) != 300 {
		t.Fatalf("parent paid %d, want the embodied 600", 900-h.Energy(p))
	}
}

func TestCloneValidation(t *testing.T) {
	h := harness(t, `{"base_cost": 50, "offspring_energy": 10}`)
	mine := h.Unit(1, 5, 5, 100)
	poor := h.Unit(1, 9, 9, 59) // needs 60
	theirs := h.Unit(2, 20, 20, 500)
	corner := h.Unit(1, 0, 0, 500)
	// Surround a unit completely.
	boxed := h.Unit(1, 30, 30, 500)
	for _, o := range [][2]int32{{-1, -1}, {0, -1}, {1, -1}, {-1, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}} {
		h.Unit(2, 30+o[0], 30+o[1], 0)
	}
	h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(mine)}, Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 1}})
	h.Step()
	cases := []struct {
		name string
		slot int
		cmd  *cloningv1.CloneCommand
		code string
	}{
		{"busy upgrading", 1, clone(0, mine), sim.CodeFailedPrecondition},
		{"insufficient energy", 1, clone(0, poor), energy.CodeInsufficientEnergy},
		{"not yours", 1, clone(0, theirs), sim.CodePermissionDenied},
		{"no free tile", 1, clone(0, boxed), sim.CodeFailedPrecondition},
		{"direction out of bounds", 1, clone(cloningv1.Direction_DIRECTION_NORTH, corner), sim.CodeFailedPrecondition},
		{"invalid direction", 1, clone(cloningv1.Direction(42), corner), sim.CodeInvalidArgument},
		{"unknown unit", 1, clone(0, 777), sim.CodeNotFound},
		{"no units", 1, clone(0), sim.CodeInvalidArgument},
	}
	for _, c := range cases {
		id := h.Submit(c.slot, cloning.CommandClone, c.cmd)
		if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	if h.Energy(poor) != 59 || h.Energy(theirs) != 500 {
		t.Fatal("rejected clones charged energy")
	}
	// A cloning unit cannot start an upgrade either.
	h.Submit(1, cloning.CommandClone, clone(0, corner))
	h.Step()
	id := h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(corner)}, Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 1}})
	if r, _ := pt.Result(h.Step(), id); r.Code != sim.CodeFailedPrecondition {
		t.Fatalf("upgrade during cloning: %+v", r)
	}
}

func TestSimultaneousCloningConflicts(t *testing.T) {
	h := harness(t, `{"base_cost": 10, "cost": "base", "duration": "3", "offspring_energy": 0}`)
	// Two parents at (5,5) and (7,5) both want (6,5), the only free tile
	// between them once the rest is filled with blockers.
	a := h.Unit(1, 5, 5, 100)
	b := h.Unit(2, 7, 5, 100)
	for x := int32(4); x <= 8; x++ {
		for y := int32(4); y <= 6; y++ {
			if (x == 5 || x == 7 || x == 6) && y == 5 {
				continue
			}
			h.Unit(2, x, y, 0)
		}
	}
	h.Submit(1, cloning.CommandClone, clone(0, a))
	h.Submit(2, cloning.CommandClone, clone(0, b))
	fs := h.Steps(3)
	done := pt.Events(fs, cloning.EventCompleted)
	blocked := pt.Events(fs, cloning.EventBlocked)
	if len(done) != 1 || len(blocked) != 1 {
		t.Fatalf("completed %v blocked %v", done, blocked)
	}
	// The lower entity ID wins the tile; the other waits.
	winner, loser := min(a, b), max(a, b)
	if done[0].Entity != winner || blocked[0].Entity != loser {
		t.Fatalf("winner %d (want %d), loser %d (want %d)", done[0].Entity, winner, blocked[0].Entity, loser)
	}
	// Free a tile next to the loser: it completes on the next tick.
	lp, _ := h.W().Entity(loser)
	blocker, _ := h.W().EntityAt(sim.Pos{X: lp.Pos.X, Y: 4})
	h.Submit(0, sim.CmdRemoveEntity, sim.RemoveEntity{ID: blocker})
	f := h.Step()
	if ev := pt.Events([]*sim.Frame{f}, cloning.EventCompleted); len(ev) != 1 || ev[0].Entity != loser {
		t.Fatalf("waiting clone did not complete: %+v", f.Events)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []string{
		`{"cost": "base + armor"}`,
		`{"duration": "1 +"}`,
		`{"base_cost": -1}`,
		`{"offspring_energy": -5}`,
		`{"bogus": 1}`,
	} {
		c := &game.Config{
			World:   game.WorldConfig{Width: 5, Height: 5, TickRateHz: 10},
			Plugins: map[string]json.RawMessage{"cloning": json.RawMessage(cfg)},
		}
		if _, err := game.New(c); err == nil {
			t.Errorf("accepted %s", cfg)
		}
	}
}
