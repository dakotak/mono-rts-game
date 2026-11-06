package upgrades_test

import (
	"testing"

	attributesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/attributes/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// bigStore gives units a fixed 1000-energy capacity (not bound to the
// energy_capacity attribute) so tests can fund expensive upgrades.
const bigStore = `{"unit_capacity": 1000, "initial_energy": 0, "extraction_attribute": "mining_efficiency"}`

func harness(t *testing.T, extra map[string]string) *pt.Harness {
	cfg := map[string]string{"energy": bigStore}
	for k, v := range extra {
		cfg[k] = v
	}
	return pt.New(t, cfg)
}

func target(attr string, to int64, units ...sim.EntityID) *upgradesv1.UpgradeCommand {
	m := &upgradesv1.UpgradeCommand{Attribute: attr, Amount: &upgradesv1.UpgradeCommand_Target{Target: to}}
	for _, u := range units {
		m.UnitIds = append(m.UnitIds, uint64(u))
	}
	return m
}

func increase(attr string, by int64, units ...sim.EntityID) *upgradesv1.UpgradeCommand {
	m := target(attr, 0, units...)
	m.Amount = &upgradesv1.UpgradeCommand_Increase{Increase: by}
	return m
}

func TestUpgradeDurationCostAndCompletion(t *testing.T) {
	h := harness(t, map[string]string{"upgrades": `{"progress_event_interval": 5}`})
	u := h.Unit(1, 5, 5, 100)
	// armor 0 -> 3: cost(3) - cost(0) = 5*9 + 30 = 75; duration 12 ticks.
	id := h.Submit(1, upgrades.CommandUpgrade, target("armor", 3, u))
	frames := h.Steps(11)
	if r, _ := pt.Result(frames[0], id); !r.OK {
		t.Fatalf("result = %+v", r)
	}
	// Paid immediately, not applied yet.
	if h.Energy(u) != 25 || h.Prop(u, "armor") != 0 {
		t.Fatalf("energy %d armor %d before completion", h.Energy(u), h.Prop(u, "armor"))
	}
	started := pt.Events(frames, upgrades.EventStarted)
	if len(started) != 1 || pt.Field(started[0], "cost").Int != 75 || pt.Field(started[0], "duration").Int != 12 {
		t.Fatalf("started = %+v", started)
	}
	if prog := pt.Events(frames, upgrades.EventProgress); len(prog) != 2 || pt.Field(prog[1], "done").Int != 10 {
		t.Fatalf("progress = %+v", prog)
	}
	// Owner-visible progress attribute.
	var done int64
	for _, a := range frames[10].Changed[0].Attrs {
		if a.Key == "upgrades.done" {
			done = a.Value.Int
		}
	}
	if done != 11 {
		t.Fatalf("observed progress = %d", done)
	}
	last := h.Step() // 12th tick of the upgrade
	if h.Prop(u, "armor") != 3 || h.Prop(u, "invested") != 75 || h.Prop(u, "embodied") != 75 {
		t.Fatalf("armor %d invested %d", h.Prop(u, "armor"), h.Prop(u, "invested"))
	}
	if c := pt.Events([]*sim.Frame{last}, upgrades.EventCompleted); len(c) != 1 || pt.Field(c[0], "value").Int != 3 {
		t.Fatalf("completed = %+v", c)
	}
	if _, busy := h.Attrs().Busy(h.W(), u); busy {
		t.Fatal("unit still busy after completion")
	}
	// Mass now includes armor: 15 + 9.
	if h.Prop(u, "mass") != 24 {
		t.Fatalf("mass = %d", h.Prop(u, "mass"))
	}
}

func TestNonlinearCostsCharged(t *testing.T) {
	h := harness(t, nil)
	var costs []int64
	for to := int64(1); to <= 5; to++ {
		u := h.Unit(1, int32(to), 0, 400)
		h.Submit(1, upgrades.CommandUpgrade, target("armor", to, u))
		h.Step()
		costs = append(costs, 400-h.Energy(u))
	}
	// cost(v) = 5v² + 10v: 15, 40, 75, 120, 175 — strictly convex.
	want := []int64{15, 40, 75, 120, 175}
	for i := range want {
		if costs[i] != want[i] {
			t.Fatalf("costs = %v, want %v", costs, want)
		}
		if i >= 2 && costs[i]-costs[i-1] <= costs[i-1]-costs[i-2] {
			t.Fatalf("marginal cost not increasing: %v", costs)
		}
	}
}

func TestInsufficientEnergy(t *testing.T) {
	h := harness(t, nil)
	u := h.Unit(1, 0, 0, 74)
	id := h.Submit(1, upgrades.CommandUpgrade, target("armor", 3, u)) // costs 75
	f := h.Step()
	r, _ := pt.Result(f, id)
	if r.OK || r.Code != energy.CodeInsufficientEnergy {
		t.Fatalf("result = %+v", r)
	}
	if h.Energy(u) != 74 {
		t.Fatal("energy charged for a rejected upgrade")
	}
	if ev := pt.Events([]*sim.Frame{f}, upgrades.EventFailed); len(ev) != 1 || pt.Field(ev[0], "reason").Str != energy.CodeInsufficientEnergy {
		t.Fatalf("failed events = %+v", ev)
	}
	// The command is atomic: one poor unit rejects the whole list.
	rich := h.Unit(1, 1, 0, 400)
	id = h.Submit(1, upgrades.CommandUpgrade, target("armor", 3, rich, u))
	f = h.Step()
	if r, _ := pt.Result(f, id); r.OK || h.Energy(rich) != 400 {
		t.Fatalf("partial application: %+v, energy %d", r, h.Energy(rich))
	}
}

func TestInvalidAndUnauthorizedUpgrades(t *testing.T) {
	h := harness(t, nil)
	mine := h.Unit(1, 0, 0, 400)
	theirs := h.Unit(2, 5, 5, 400)
	h.Step()
	cases := []struct {
		name string
		cmd  *upgradesv1.UpgradeCommand
		code string
	}{
		{"not yours", target("armor", 1, theirs), sim.CodePermissionDenied},
		{"unknown unit", target("armor", 1, 12345), sim.CodeNotFound},
		{"unknown attribute", target("charisma", 1, mine), sim.CodeInvalidArgument},
		{"not higher (irreversible)", target("propulsion", 1, mine), sim.CodeInvalidArgument},
		{"downgrade", target("energy_capacity", 50, mine), sim.CodeInvalidArgument},
		{"above max", target("armor", 21, mine), sim.CodeInvalidArgument},
		{"zero increase", increase("armor", 0, mine), sim.CodeInvalidArgument},
		{"no amount", &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(mine)}, Attribute: "armor"}, sim.CodeInvalidArgument},
		{"no units", target("armor", 1), sim.CodeInvalidArgument},
		{"duplicate", target("armor", 1, mine, mine), sim.CodeInvalidArgument},
	}
	for _, c := range cases {
		id := h.Submit(1, upgrades.CommandUpgrade, c.cmd)
		if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	if h.Energy(mine) != 400 || h.Energy(theirs) != 400 {
		t.Fatal("rejected upgrades charged energy")
	}
	// Players cannot use the admin attribute override.
	if _, err := h.Engine.Submit(sim.Command{Issuer: sim.Principal{Player: h.P(1)}, Type: attributes.CommandSet, Payload: &attributesv1.SetAttributeCommand{}}); err == nil {
		t.Fatal("player used admin command")
	}
}

func TestNoOverlappingUpgrades(t *testing.T) {
	h := harness(t, nil)
	u := h.Unit(1, 0, 0, 400)
	h.Submit(1, upgrades.CommandUpgrade, target("armor", 2, u)) // 8 ticks
	h.Step()
	id := h.Submit(1, upgrades.CommandUpgrade, target("propulsion", 2, u))
	if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != sim.CodeFailedPrecondition {
		t.Fatalf("overlapping upgrade: %+v", r)
	}
	h.Steps(6)
	if h.Prop(u, "armor") != 2 {
		t.Fatal("first upgrade did not complete")
	}
	// Now free again; increase is relative to the new value.
	id = h.Submit(1, upgrades.CommandUpgrade, increase("armor", 1, u))
	f := h.Step()
	if r, _ := pt.Result(f, id); !r.OK {
		t.Fatalf("follow-up upgrade: %+v", r)
	}
	if ev := pt.Events([]*sim.Frame{f}, upgrades.EventStarted); pt.Field(ev[0], "from").Int != 2 || pt.Field(ev[0], "to").Int != 3 {
		t.Fatalf("started = %+v", ev)
	}
}

func TestSupersededByAdmin(t *testing.T) {
	h := harness(t, nil)
	u := h.Unit(1, 0, 0, 400)
	h.Submit(1, upgrades.CommandUpgrade, target("armor", 2, u))
	h.Step()
	h.Submit(0, attributes.CommandSet, &attributesv1.SetAttributeCommand{UnitId: uint64(u), Attribute: "armor", Value: 5})
	fs := h.Steps(8)
	if ev := pt.Events(fs, upgrades.EventFailed); len(ev) != 1 || pt.Field(ev[0], "reason").Str != "superseded" {
		t.Fatalf("failed = %+v", ev)
	}
	if h.Prop(u, "armor") != 5 || h.Energy(u) != 360 {
		t.Fatalf("armor %d energy %d", h.Prop(u, "armor"), h.Energy(u))
	}
}

func TestEnergyAccountingAndCapacityUpgrade(t *testing.T) {
	h := pt.New(t, nil)
	u := h.Unit(1, 0, 0, 100)
	// energy_capacity 100 -> 150: (50*50)/50 + 50 = 100 energy, 5 ticks.
	h.Submit(1, upgrades.CommandUpgrade, target("energy_capacity", 150, u))
	h.Steps(5)
	s, _ := energy.GetStore(h.W(), u)
	if s.Energy != 0 || s.Capacity != 150 {
		t.Fatalf("store = %+v", s)
	}
	// Energy spent == investment recorded.
	if h.Prop(u, "invested") != 100 || h.Prop(u, "embodied") != 100 {
		t.Fatalf("invested %d", h.Prop(u, "invested"))
	}
	// A bigger store lets the unit hold (and spend) more: 140 is now legal.
	energy.SetStored(h.W(), u, 140)
	if h.Energy(u) != 140 {
		t.Fatalf("energy after refill = %d", h.Energy(u))
	}
}

func TestUpgradeDeterminism(t *testing.T) {
	run := func() uint64 {
		h := harness(t, nil)
		var units []sim.EntityID
		for i := range 40 {
			units = append(units, h.Unit(1+i%2, int32(i%8)*2, int32(i/8)*2, int64(50+i*7)))
		}
		for i, u := range units {
			e, _ := h.W().Entity(u)
			slot := 1
			if e.Owner == h.P(2) {
				slot = 2
			}
			attr := []string{"armor", "propulsion", "mining_efficiency", "energy_capacity"}[i%4]
			h.Submit(slot, upgrades.CommandUpgrade, increase(attr, int64(1+i%3), u))
		}
		h.Steps(60)
		return h.W().StateHash()
	}
	first, second := run(), run()
	if first != second {
		t.Fatal("upgrades are not deterministic")
	}
}
