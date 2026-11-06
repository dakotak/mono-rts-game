package salvage_test

import (
	"testing"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	salvagev1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/salvage/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func harness(t *testing.T, salvageCfg string) *pt.Harness {
	cfg := map[string]string{
		"energy": `{"unit_capacity": 1000, "initial_energy": 0}`,
		"combat": `{"attack": {"default": 10}, "max_health": {"default": 20}, "cooldown": {"default": 1}}`,
	}
	if salvageCfg != "" {
		cfg["salvage"] = salvageCfg
	}
	return pt.New(t, cfg)
}

// kill destroys a unit through the admin health override (same path as
// combat destruction) and returns the wreck it left, if any.
func kill(h *pt.Harness, u sim.EntityID) (sim.EntityID, int64) {
	h.T.Helper()
	pos, _ := h.W().Entity(u)
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(u), Health: 0})
	h.Step()
	if _, ok := h.W().Entity(u); ok {
		h.T.Fatal("unit survived")
	}
	for _, id := range h.W().EntityIDs() {
		e, _ := h.W().Entity(id)
		if wk, ok := salvage.GetWreck(h.W(), id); ok && e.Pos == pos.Pos {
			return id, wk.Energy
		}
	}
	return 0, 0
}

func setAttr(h *pt.Harness, u sim.EntityID, attr string, v, invested int64) {
	i, _ := h.Attrs().Index(attr)
	h.Attrs().Apply(h.W(), u, i, v, invested)
}

func TestWreckRecoveryFractions(t *testing.T) {
	cases := []struct {
		cfg                        string
		stored, embodied, invested int64
		want                       int64
	}{
		// defaults: 500/1000 of each, rounded down.
		{"", 101, 0, 0, 50},
		{"", 0, 0, 0, 0}, // nothing to recover: no wreck
		{`{"embodied_recovery_permille": 1000, "stored_recovery_permille": 0}`, 500, 75, 75, 75},
		{`{"embodied_recovery_permille": 333, "stored_recovery_permille": 333}`, 10, 75, 75, 24 + 3}, // floor(24.975) + floor(3.33)
		{`{"embodied_recovery_permille": 0, "stored_recovery_permille": 0}`, 999, 75, 75, 0},
		// Admin-granted (embodied but never paid) investment is not recovered.
		{`{"embodied_recovery_permille": 1000, "stored_recovery_permille": 0}`, 0, 75, 0, 0},
		{`{"embodied_recovery_permille": 1000, "stored_recovery_permille": 0}`, 0, 75, 40, 40},
	}
	for i, c := range cases {
		h := harness(t, c.cfg)
		u := h.Unit(1, 3, 3, c.stored)
		if c.embodied > 0 {
			setAttr(h, u, "armor", 3, c.invested) // armor 3 embodies 5*9+30 = 75 in plugintest.Rules
		}
		if got := h.Prop(u, "embodied"); got != c.embodied {
			t.Fatalf("case %d: embodied = %d", i, got)
		}
		wreck, amount := kill(h, u)
		if amount != c.want || (c.want == 0) != (wreck == 0) {
			t.Errorf("case %d: wreck %d with %d energy, want %d", i, wreck, amount, c.want)
		}
	}
}

func TestWreckIsPassableAndDistinct(t *testing.T) {
	h := harness(t, "")
	victim := h.Unit(2, 5, 5, 100)
	walker := h.Unit(1, 3, 5, 0)
	miner := h.Unit(1, 9, 9, 0)
	wreck, amount := kill(h, victim)
	if wreck == 0 || amount != 50 {
		t.Fatalf("wreck %d energy %d", wreck, amount)
	}
	e, _ := h.W().Entity(wreck)
	if !e.Passable || e.Kind != sim.KindResource || e.Owner != 0 {
		t.Fatalf("wreck entity = %+v", e)
	}
	// Units walk through it.
	h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(walker), Destination: &rtsv1.Position{X: 7, Y: 5}}}})
	h.Steps(4)
	if p, _ := h.W().Entity(walker); p.Pos != (sim.Pos{X: 7, Y: 5}) {
		t.Fatalf("walker blocked at %v", p.Pos)
	}
	// A wreck is not a well, and a well is not a wreck.
	well, _ := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: 9, Y: 10})
	wells.Set(h.W(), well, wells.Well{Energy: 10, Capacity: 10, ExtractionRate: 1})
	mine := h.Submit(1, energy.CommandMine, &energyv1.MineCommand{UnitIds: []uint64{uint64(miner)}, WellId: uint64(wreck)})
	salv := h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(miner)}, WreckId: uint64(well)})
	f := h.Step()
	r1, _ := pt.Result(f, mine)
	r2, _ := pt.Result(f, salv)
	if r1.OK || r2.OK {
		t.Fatalf("mine wreck %+v / salvage well %+v", r1, r2)
	}
}

func TestSalvageContentionCapacityAndDepletion(t *testing.T) {
	h := harness(t, `{"embodied_recovery_permille": 0, "stored_recovery_permille": 1000, "salvage_rate": {"default": 4}}`)
	victim := h.Unit(2, 10, 10, 13)
	wreck, amount := kill(h, victim)
	if amount != 13 {
		t.Fatalf("wreck energy %d", amount)
	}
	// Three collectors of two players; one is standing on the wreck, one
	// has room for only 2 more energy.
	a := h.Unit(1, 9, 9, 0)
	b := h.Unit(2, 10, 10, 0) // on the wreck (passable)
	c := h.Unit(1, 11, 11, 998)
	far := h.Unit(1, 20, 20, 0)
	h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(a), uint64(c), uint64(far)}, WreckId: uint64(wreck)})
	h.Submit(2, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(b)}, WreckId: uint64(wreck)})
	f := h.Step()
	// Requests: a 4, b 4, c min(4, 2) = 2; total 10 <= 13: all served.
	if h.Energy(a) != 4 || h.Energy(b) != 4 || h.Energy(c) != 1000 || h.Energy(far) != 0 {
		t.Fatalf("after tick 1: a %d b %d c %d far %d", h.Energy(a), h.Energy(b), h.Energy(c), h.Energy(far))
	}
	if wk, _ := salvage.GetWreck(h.W(), wreck); wk.Energy != 3 {
		t.Fatalf("wreck = %+v", wk)
	}
	// Tick 2: a and b each want 4, only 3 left: split 1/2 or 2/1, never more.
	fs := append([]*sim.Frame{f}, h.Step())
	if got := h.Energy(a) + h.Energy(b); got != 11 {
		t.Fatalf("a+b = %d, want 11", got)
	}
	if _, ok := h.W().Entity(wreck); ok {
		t.Fatal("emptied wreck not removed")
	}
	var collected int64
	for _, ev := range pt.Events(fs, salvage.EventCollected) {
		collected += pt.Field(ev, "amount").Int
	}
	if collected != 13 {
		t.Fatalf("collected %d of 13", collected)
	}
	if n := len(pt.Events(fs, salvage.EventWreckDepleted)); n != 2 { // one per collecting owner
		t.Fatalf("depleted events = %d", n)
	}
	// Every order on it ended, including the out-of-reach one.
	ended := pt.Events(fs, salvage.EventEnded)
	if len(ended) != 4 {
		t.Fatalf("ended = %+v", ended)
	}
	// Salvaging a wreck that no longer exists is rejected.
	id := h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(a)}, WreckId: uint64(wreck)})
	if r, _ := pt.Result(h.Step(), id); r.OK {
		t.Fatal("salvaged a removed wreck")
	}
}

func TestSalvageWaitsForReachAndBattery(t *testing.T) {
	h := harness(t, `{"stored_recovery_permille": 1000, "salvage_rate": {"default": 10}}`)
	victim := h.Unit(2, 10, 10, 100)
	wreck, _ := kill(h, victim)
	u := h.Unit(1, 14, 10, 1000) // far and full
	h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(u)}, WreckId: uint64(wreck)})
	h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: 11, Y: 10}}}})
	h.Steps(5)
	if wk, _ := salvage.GetWreck(h.W(), wreck); wk.Energy != 100 {
		t.Fatal("collected without battery room or reach")
	}
	energy.SetStored(h.W(), u, 995)
	h.Step()
	if wk, _ := salvage.GetWreck(h.W(), wreck); wk.Energy != 95 || h.Energy(u) != 1000 {
		t.Fatalf("wreck %+v unit %d", wk, h.Energy(u))
	}
}

func TestEnergyConservationThroughCombatAndSalvage(t *testing.T) {
	h := harness(t, `{"embodied_recovery_permille": 0, "stored_recovery_permille": 600, "salvage_rate": {"default": 3}}`)
	var reds, blues []sim.EntityID
	for i := range 8 {
		reds = append(reds, h.Unit(1, int32(10+i), 10, 100))
		blues = append(blues, h.Unit(2, int32(10+i), 11, int64(37+i)))
	}
	total := func() (units, wrecks int64) {
		for _, id := range h.W().EntityIDs() {
			units += h.Energy(id)
			if wk, ok := salvage.GetWreck(h.W(), id); ok {
				wrecks += wk.Energy
			}
		}
		return
	}
	u0, _ := total()
	// Reds destroy all blues (attacks are free in this config: energy cost
	// defaults to 0). Each blue leaves floor(0.6 * stored).
	for i, r := range reds {
		h.Submit(1, combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(r)}, TargetId: uint64(blues[i])})
	}
	h.Steps(2)
	var expectWrecks, lostStored int64
	for i := range blues {
		s := int64(37 + i)
		expectWrecks += s * 600 / 1000
		lostStored += s
	}
	u1, w1 := total()
	if w1 != expectWrecks || u0-u1 != lostStored {
		t.Fatalf("wrecks %d (want %d), units lost %d (want %d)", w1, expectWrecks, u0-u1, lostStored)
	}
	// Salvaging only moves energy from wrecks to units.
	for _, id := range h.W().EntityIDs() {
		if _, ok := salvage.GetWreck(h.W(), id); ok {
			e, _ := h.W().Entity(id)
			for _, r := range reds {
				// The red directly above the wreck (one collector per wreck).
				if p, _ := h.W().Entity(r); p.Pos.X == e.Pos.X && combat.Distance(p.Pos, e.Pos) <= 1 {
					h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(r)}, WreckId: uint64(id)})
					break
				}
			}
		}
	}
	for range 40 {
		h.Step()
		u, w := total()
		if u+w != u1+w1 {
			t.Fatalf("energy not conserved during salvage: %d + %d != %d", u, w, u1+w1)
		}
	}
	if _, w := total(); w != 0 {
		t.Fatalf("wrecks not emptied: %d left", w)
	}
}

func TestAdminWreckMutations(t *testing.T) {
	h := harness(t, "")
	u := h.Unit(1, 0, 0, 0)
	id := h.Submit(0, salvage.CommandCreate, &salvagev1.CreateWreckCommand{Position: &rtsv1.Position{X: 0, Y: 0}, Energy: 40})
	bad := h.Submit(0, salvage.CommandCreate, &salvagev1.CreateWreckCommand{Position: &rtsv1.Position{X: 0, Y: 0}, Energy: 0})
	f := h.Step()
	r, _ := pt.Result(f, id)
	rb, _ := pt.Result(f, bad)
	if !r.OK || rb.OK {
		t.Fatalf("create %+v / bad %+v", r, rb)
	}
	var wreck sim.EntityID
	for _, e := range h.W().EntityIDs() {
		if _, ok := salvage.GetWreck(h.W(), e); ok {
			wreck = e
		}
	}
	if at, _ := h.W().EntityAt(sim.Pos{}); at != u {
		t.Fatal("admin wreck displaced the unit")
	}
	if _, err := h.Engine.Submit(sim.Command{Issuer: sim.Principal{Player: h.P(1)}, Type: salvage.CommandCreate, Payload: &salvagev1.CreateWreckCommand{}}); err == nil {
		t.Fatal("player may create wrecks")
	}
	h.Submit(0, salvage.CommandSetEnergy, &salvagev1.SetWreckEnergyCommand{WreckId: uint64(wreck), Energy: 75})
	h.Step()
	if wk, _ := salvage.GetWreck(h.W(), wreck); wk.Energy != 75 || wk.Initial != 75 {
		t.Fatalf("wreck = %+v", wk)
	}
	h.Submit(0, salvage.CommandSetEnergy, &salvagev1.SetWreckEnergyCommand{WreckId: uint64(wreck), Energy: 0})
	h.Step()
	if _, ok := h.W().Entity(wreck); ok {
		t.Fatal("zero-energy wreck not removed")
	}
}
