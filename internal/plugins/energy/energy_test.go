package energy_test

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"

	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

type harness struct {
	t *testing.T
	e *sim.Engine
	n int
}

// newHarness builds a world with wells, energy and movement. cfg maps
// plugin names to JSON config sections.
func newHarness(t *testing.T, cfg map[string]string) *harness {
	t.Helper()
	conf := map[string]json.RawMessage{}
	for k, v := range cfg {
		conf[k] = json.RawMessage(v)
	}
	rs, err := sim.NewRuleset([]sim.Plugin{wells.New(), energy.New(), movement.New()}, conf)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sim.NewEngine(sim.Config{Width: 20, Height: 20, Seed: 3, IDKey: []byte("energy-test-key-")}, rs)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, e: e}
}

func (h *harness) w() *sim.World { return h.e.World() }

func (h *harness) unit(owner sim.PlayerID, x, y int32) sim.EntityID {
	h.t.Helper()
	id, err := h.w().CreateEntity(sim.KindUnit, owner, sim.Pos{X: x, Y: y})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) well(x, y int32, v wells.Well) sim.EntityID {
	h.t.Helper()
	id, err := h.w().CreateEntity(sim.KindResource, 0, sim.Pos{X: x, Y: y})
	if err != nil {
		h.t.Fatal(err)
	}
	wells.Set(h.w(), id, v)
	return id
}

func (h *harness) submit(issuer sim.Principal, typ string, payload any) string {
	h.t.Helper()
	h.n++
	id := fmt.Sprintf("c%d", h.n)
	if _, err := h.e.Submit(sim.Command{ID: id, Issuer: issuer, Type: typ, Payload: payload}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func player(p sim.PlayerID) sim.Principal { return sim.Principal{Player: p} }

var admin = sim.Principal{Admin: true}

func (h *harness) mine(p sim.PlayerID, well sim.EntityID, units ...sim.EntityID) {
	m := &energyv1.MineCommand{WellId: uint64(well)}
	for _, u := range units {
		m.UnitIds = append(m.UnitIds, uint64(u))
	}
	h.submit(player(p), energy.CommandMine, m)
}

func (h *harness) transfer(p sim.PlayerID, from, to sim.EntityID, amount int64) {
	h.submit(player(p), energy.CommandTransfer, &energyv1.TransferCommand{FromUnitId: uint64(from), ToUnitId: uint64(to), Amount: amount})
}

func (h *harness) setEnergy(u sim.EntityID, e int64) {
	h.submit(admin, energy.CommandSetEnergy, &energyv1.SetUnitEnergyCommand{UnitId: uint64(u), Energy: e})
}

func (h *harness) stored(u sim.EntityID) int64 {
	h.t.Helper()
	s, ok := energy.GetStore(h.w(), u)
	if !ok {
		h.t.Fatalf("unit %d has no store", u)
	}
	return s.Energy
}

func (h *harness) wellEnergy(id sim.EntityID) int64 {
	v, _ := wells.Get(h.w(), id)
	return v.Energy
}

func (h *harness) step() *sim.Frame { return h.e.Step() }

func (h *harness) steps(n int) []*sim.Frame {
	var fs []*sim.Frame
	for range n {
		fs = append(fs, h.e.Step())
	}
	return fs
}

func events(fs []*sim.Frame, typ string) []sim.Event {
	var out []sim.Event
	for _, f := range fs {
		for _, e := range f.Events {
			if e.Type == typ {
				out = append(out, e)
			}
		}
	}
	return out
}

func field(e sim.Event, key string) sim.Value {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return sim.Value{}
}

// ---- storage -----------------------------------------------------------------

func TestUnitsGetDefaultStore(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 30, "initial_energy": 10}`})
	u := h.unit(1, 0, 0)
	if s, _ := energy.GetStore(h.w(), u); s != (energy.Store{Energy: 10, Capacity: 30}) {
		t.Fatalf("store = %+v", s)
	}
	// Stores are owner-visible attributes.
	f := h.step()
	var keys []string
	for _, a := range f.Changed[0].Attrs {
		if a.Visibility != sim.VisibleOwner {
			t.Errorf("%s visibility %v", a.Key, a.Visibility)
		}
		keys = append(keys, a.Key)
	}
	if fmt.Sprint(keys) != "[energy.stored energy.capacity]" {
		t.Errorf("attrs = %v", keys)
	}
	// Wells do not get a store.
	wid := h.well(5, 5, wells.Well{Energy: 1, Capacity: 1})
	if _, ok := energy.GetStore(h.w(), wid); ok {
		t.Error("well got an energy store")
	}
	// No passive upkeep: energy is unchanged over time.
	h.steps(50)
	if h.stored(u) != 10 {
		t.Errorf("energy drifted to %d", h.stored(u))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []string{
		`{"initial_energy": 200, "unit_capacity": 100}`,
		`{"extraction_rate": -1}`,
		`{"transfer_rate": -1}`,
		`{"nope": 1}`,
	} {
		if _, err := sim.NewRuleset([]sim.Plugin{wells.New(), energy.New()}, map[string]json.RawMessage{"energy": json.RawMessage(cfg)}); err == nil {
			t.Errorf("accepted %s", cfg)
		}
	}
	if _, err := sim.NewRuleset([]sim.Plugin{energy.New()}, nil); err == nil {
		t.Error("energy without wells accepted")
	}
}

// ---- mining ------------------------------------------------------------------

func TestMiningRespectsRatesAndCapacity(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 10, "initial_energy": 0, "extraction_rate": 3}`})
	u := h.unit(1, 1, 1)
	wid := h.well(2, 2, wells.Well{Energy: 100, Capacity: 100, ExtractionRate: 5}) // diagonal neighbour
	h.mine(1, wid, u)
	var got []int64
	for range 5 {
		h.step()
		got = append(got, h.stored(u))
	}
	// Unit rate 3 < well rate 5; capped by capacity 10.
	if fmt.Sprint(got) != "[3 6 9 10 10]" {
		t.Fatalf("energy per tick = %v", got)
	}
	if h.wellEnergy(wid) != 90 {
		t.Fatalf("well = %d, want 90 (exactly what the unit gained)", h.wellEnergy(wid))
	}
	// A full unit keeps its order and resumes after spending.
	h.setEnergy(u, 0)
	h.step()
	if h.stored(u) != 3 {
		t.Fatalf("did not resume mining: %d", h.stored(u))
	}
}

func TestMiningRequiresAdjacency(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"initial_energy": 0}`})
	u := h.unit(1, 0, 0)
	wid := h.well(5, 0, wells.Well{Energy: 100, Capacity: 100, ExtractionRate: 5})
	h.mine(1, wid, u)
	h.steps(3)
	if h.stored(u) != 0 {
		t.Fatal("mined from a distance")
	}
	// Walk next to the well; mining starts by itself.
	h.submit(player(1), movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: 4, Y: 0}}}})
	h.steps(4) // arrives on the 4th tick, mines from the 5th
	if h.stored(u) != 0 {
		t.Fatalf("mined before adjacent: %d", h.stored(u))
	}
	h.step()
	if h.stored(u) != 2 {
		t.Fatalf("energy after reaching well = %d", h.stored(u))
	}
}

func TestFiniteWellDepletes(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 100, "initial_energy": 0, "extraction_rate": 4}`})
	a := h.unit(1, 0, 0)
	b := h.unit(2, 2, 0)
	wid := h.well(1, 0, wells.Well{Energy: 13, Capacity: 50, ExtractionRate: 10})
	h.mine(1, wid, a)
	h.mine(2, wid, b)
	fs := h.steps(3)
	if h.wellEnergy(wid) != 0 || h.stored(a)+h.stored(b) != 13 {
		t.Fatalf("well %d, units %d+%d", h.wellEnergy(wid), h.stored(a), h.stored(b))
	}
	dep := events(fs, energy.EventWellDepleted)
	// Only the players whose units drained it are told (one event each).
	if len(dep) != 2 || dep[0].Entity != wid || dep[0].Audience != sim.ToPlayer(1) || dep[1].Audience != sim.ToPlayer(2) {
		t.Fatalf("depleted events = %+v", dep)
	}
	stopped := events(fs, energy.EventMiningStopped)
	if len(stopped) != 2 || field(stopped[0], "reason").Str != "well_depleted" {
		t.Fatalf("stopped events = %+v", stopped)
	}
	// Each owner hears only about their own unit.
	for _, ev := range stopped {
		e, _ := h.w().Entity(ev.Entity)
		if ev.Audience != sim.ToPlayer(e.Owner) {
			t.Fatalf("audience = %+v", ev.Audience)
		}
	}
	// Mining an empty well is rejected at the next tick.
	h.mine(1, wid, a)
	fs = h.steps(1)
	if ev := events(fs, energy.EventMiningStopped); len(ev) != 1 || field(ev[0], "reason").Str != "well_depleted" {
		t.Fatalf("events = %+v", ev)
	}
}

func TestUnlimitedWell(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 1000, "initial_energy": 0, "extraction_rate": 5}`})
	var units []sim.EntityID
	for _, p := range [][2]int32{{0, 0}, {1, 0}, {2, 0}, {0, 2}, {2, 2}} {
		units = append(units, h.unit(1, p[0], p[1]))
	}
	wid := h.well(1, 1, wells.Well{Energy: 3, Capacity: 3, ExtractionRate: 7, Unlimited: true})
	h.mine(1, wid, units...)
	h.steps(10)
	var total int64
	for _, u := range units {
		total += h.stored(u)
	}
	// Supply is unlimited (far beyond the well's nominal 3), but the
	// extraction rate still caps the total per tick.
	if total != 70 || h.wellEnergy(wid) != 3 {
		t.Fatalf("total %d, well %d", total, h.wellEnergy(wid))
	}
}

func TestSimultaneousExtractionContention(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 100, "initial_energy": 0, "extraction_rate": 4}`})
	// Three units of two players around one well releasing 5 per tick.
	a := h.unit(1, 0, 0)
	b := h.unit(2, 2, 0)
	c := h.unit(1, 1, 2)
	wid := h.well(1, 1, wells.Well{Energy: 12, Capacity: 12, ExtractionRate: 5})
	h.mine(1, wid, a, c)
	h.mine(2, wid, b)
	f := h.step()
	grants := map[sim.EntityID]int64{}
	var sum int64
	for _, ev := range events([]*sim.Frame{f}, energy.EventMined) {
		grants[ev.Entity] = field(ev, "amount").Int
		sum += field(ev, "amount").Int
	}
	// 5 split three ways: 1, 2, 2 (smallest-ID claimant first gets the
	// floor share), never more than the well releases.
	if sum != 5 || len(grants) != 3 {
		t.Fatalf("grants = %v", grants)
	}
	for _, g := range grants {
		if g < 1 || g > 2 {
			t.Fatalf("unfair split %v", grants)
		}
	}
	// Second tick: 5 again; third tick: only 2 left in the well.
	h.step()
	h.step()
	if h.wellEnergy(wid) != 0 || h.stored(a)+h.stored(b)+h.stored(c) != 12 {
		t.Fatalf("well %d, units %d %d %d", h.wellEnergy(wid), h.stored(a), h.stored(b), h.stored(c))
	}

	// The split is deterministic: an identical world gives identical grants.
	h2 := newHarness(t, map[string]string{"energy": `{"unit_capacity": 100, "initial_energy": 0, "extraction_rate": 4}`})
	a2, b2, c2 := h2.unit(1, 0, 0), h2.unit(2, 2, 0), h2.unit(1, 1, 2)
	w2 := h2.well(1, 1, wells.Well{Energy: 12, Capacity: 12, ExtractionRate: 5})
	h2.mine(1, w2, a2, c2)
	h2.mine(2, w2, b2)
	h2.steps(3)
	if h.w().StateHash() != h2.w().StateHash() {
		t.Fatal("same inputs, different outcome")
	}
}

func TestMiningCommandValidation(t *testing.T) {
	h := newHarness(t, nil)
	mine := h.unit(1, 0, 0)
	theirs := h.unit(2, 5, 5)
	wid := h.well(1, 1, wells.Well{Energy: 10, Capacity: 10, ExtractionRate: 1})
	h.step()
	cases := []struct {
		name string
		cmd  *energyv1.MineCommand
		code string
	}{
		{"no units", &energyv1.MineCommand{WellId: uint64(wid)}, sim.CodeInvalidArgument},
		{"not yours", &energyv1.MineCommand{WellId: uint64(wid), UnitIds: []uint64{uint64(theirs)}}, sim.CodeNotFound},
		{"no such well", &energyv1.MineCommand{WellId: 42, UnitIds: []uint64{uint64(mine)}}, sim.CodeNotFound},
		{"unit is not a well", &energyv1.MineCommand{WellId: uint64(theirs), UnitIds: []uint64{uint64(mine)}}, sim.CodeNotFound},
		{"well is not a unit", &energyv1.MineCommand{WellId: uint64(wid), UnitIds: []uint64{uint64(wid)}}, sim.CodeNotFound},
		{"duplicate", &energyv1.MineCommand{WellId: uint64(wid), UnitIds: []uint64{uint64(mine), uint64(mine)}}, sim.CodeInvalidArgument},
	}
	for _, c := range cases {
		h.submit(player(1), energy.CommandMine, c.cmd)
		if r := h.step().Results[0]; r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	// Stop mining.
	h.mine(1, wid, mine)
	h.step()
	h.submit(player(1), energy.CommandStopMining, &energyv1.StopMiningCommand{UnitIds: []uint64{uint64(mine)}})
	h.step()
	before := h.stored(mine)
	h.steps(3)
	if h.stored(mine) != before {
		t.Fatal("kept mining after stop")
	}
}

func TestWellRemovedEndsMining(t *testing.T) {
	h := newHarness(t, nil)
	u := h.unit(1, 0, 0)
	wid := h.well(1, 0, wells.Well{Energy: 100, Capacity: 100, ExtractionRate: 1})
	h.mine(1, wid, u)
	h.step()
	h.submit(admin, sim.CmdRemoveEntity, sim.RemoveEntity{ID: wid})
	fs := h.steps(1)
	if ev := events(fs, energy.EventMiningStopped); len(ev) != 1 || field(ev[0], "reason").Str != "well_removed" {
		t.Fatalf("events = %+v", ev)
	}
}

// ---- transfers -----------------------------------------------------------------

func TestTransferRespectsLimits(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"unit_capacity": 20, "initial_energy": 0, "transfer_rate": 4}`})
	a := h.unit(1, 0, 0)
	b := h.unit(1, 1, 1)
	h.setEnergy(a, 18)
	h.setEnergy(b, 9)
	h.step()
	h.transfer(1, a, b, 100)
	var seq [][2]int64
	var fs []*sim.Frame
	for range 4 {
		fs = append(fs, h.step())
		seq = append(seq, [2]int64{h.stored(a), h.stored(b)})
	}
	// Rate 4/tick, then capped by b's free capacity (20-17=3), then b full.
	if fmt.Sprint(seq) != "[[14 13] [10 17] [7 20] [7 20]]" {
		t.Fatalf("energy sequence = %v", seq)
	}
	end := events(fs, energy.EventTransferEnded)
	if len(end) != 1 || field(end[0], "reason").Str != "receiver_full" || field(end[0], "remaining").Int != 89 {
		t.Fatalf("end events = %+v", end)
	}
	var moved int64
	for _, ev := range events(fs, energy.EventTransferred) {
		moved += field(ev, "amount").Int
	}
	if moved != 11 {
		t.Fatalf("transferred events total %d, want 11", moved)
	}

	// Completes exactly at the requested amount; sender_empty otherwise.
	h.setEnergy(b, 0)
	h.step()
	h.transfer(1, a, b, 5)
	fs = h.steps(3)
	if h.stored(a) != 2 || h.stored(b) != 5 || field(events(fs, energy.EventTransferEnded)[0], "reason").Str != "completed" {
		t.Fatalf("a=%d b=%d", h.stored(a), h.stored(b))
	}
	h.transfer(1, a, b, 50)
	fs = h.steps(2)
	if h.stored(a) != 0 || field(events(fs, energy.EventTransferEnded)[0], "reason").Str != "sender_empty" {
		t.Fatalf("a=%d events=%+v", h.stored(a), fs[0].Events)
	}
}

func TestTransferEndsWhenUnitsSeparate(t *testing.T) {
	h := newHarness(t, map[string]string{"energy": `{"initial_energy": 50, "transfer_rate": 1}`})
	a := h.unit(1, 0, 0)
	b := h.unit(1, 1, 0)
	h.transfer(1, a, b, 30)
	h.step()
	h.submit(player(1), movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(b), Destination: &rtsv1.Position{X: 5, Y: 0}}}})
	fs := h.steps(3)
	if ev := events(fs, energy.EventTransferEnded); len(ev) != 1 || field(ev[0], "reason").Str != "not_adjacent" {
		t.Fatalf("events = %+v", ev)
	}
	if h.stored(a)+h.stored(b) != 100 {
		t.Fatal("energy not conserved")
	}
}

func TestInvalidAndUnauthorizedTransfers(t *testing.T) {
	h := newHarness(t, nil)
	a := h.unit(1, 0, 0)
	b := h.unit(1, 1, 0)
	far := h.unit(1, 5, 5)
	enemy := h.unit(2, 0, 1)
	wid := h.well(1, 1, wells.Well{Energy: 10, Capacity: 10, ExtractionRate: 1})
	h.step()
	cases := []struct {
		name     string
		issuer   sim.Principal
		from, to sim.EntityID
		amount   int64
		code     string
	}{
		{"zero amount", player(1), a, b, 0, sim.CodeInvalidArgument},
		{"negative amount", player(1), a, b, -5, sim.CodeInvalidArgument},
		{"to self", player(1), a, a, 5, sim.CodeInvalidArgument},
		{"sender not yours", player(2), a, b, 5, sim.CodeNotFound},
		{"to enemy", player(1), a, enemy, 5, sim.CodeNotFound},
		{"from enemy to me", player(1), enemy, a, 5, sim.CodeNotFound},
		{"admin across owners", admin, a, enemy, 5, sim.CodePermissionDenied},
		{"not adjacent", player(1), a, far, 5, sim.CodeFailedPrecondition},
		{"unknown receiver", player(1), a, 77, 5, sim.CodeNotFound},
		{"to a well", player(1), a, wid, 5, sim.CodeNotFound},
		{"from a well", player(1), wid, a, 5, sim.CodeNotFound},
	}
	before := []int64{h.stored(a), h.stored(b), h.stored(enemy)}
	for _, c := range cases {
		h.submit(c.issuer, energy.CommandTransfer, &energyv1.TransferCommand{FromUnitId: uint64(c.from), ToUnitId: uint64(c.to), Amount: c.amount})
		if r := h.step().Results[0]; r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	if after := []int64{h.stored(a), h.stored(b), h.stored(enemy)}; fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("energy changed by rejected transfers: %v -> %v", before, after)
	}
	// Admin-only energy setting is not available to players.
	if _, err := h.e.Submit(sim.Command{Issuer: player(1), Type: energy.CommandSetEnergy, Payload: &energyv1.SetUnitEnergyCommand{}}); err == nil {
		t.Fatal("player may set energy")
	}
	h.submit(admin, energy.CommandSetEnergy, &energyv1.SetUnitEnergyCommand{UnitId: uint64(a), Energy: 101})
	if r := h.step().Results[0]; r.Code != sim.CodeInvalidArgument {
		t.Fatalf("energy above capacity: %+v", r)
	}
}

// ---- conservation ------------------------------------------------------------------

// total returns all energy in units plus finite wells.
func total(w *sim.World) int64 {
	var sum int64
	for _, id := range w.EntityIDs() {
		if s, ok := energy.GetStore(w, id); ok {
			sum += s.Energy
		}
		if v, ok := wells.Get(w, id); ok && !v.Unlimited {
			sum += v.Energy
		}
	}
	return sum
}

func TestEnergyConservation(t *testing.T) {
	// Free movement, finite wells: mining and transfers only move energy,
	// so the total is invariant however units are commanded.
	h := newHarness(t, map[string]string{
		"energy":   `{"unit_capacity": 40, "initial_energy": 20, "extraction_rate": 3, "transfer_rate": 4}`,
		"movement": `{"energy_per_step": 0}`,
	})
	r := rand.New(rand.NewPCG(1, 2))
	var units []sim.EntityID
	for i := range 30 {
		units = append(units, h.unit(sim.PlayerID(1+i%2), int32(i%6)*3, int32(i/6)*3))
	}
	var wellIDs []sim.EntityID
	for _, p := range [][2]int32{{1, 1}, {7, 7}, {13, 4}} {
		wellIDs = append(wellIDs, h.well(p[0], p[1], wells.Well{Energy: 300, Capacity: 300, ExtractionRate: 6}))
	}
	want := total(h.w())
	for tick := range 400 {
		for range 3 {
			u := units[r.IntN(len(units))]
			e, _ := h.w().Entity(u)
			switch r.IntN(3) {
			case 0:
				h.mine(e.Owner, wellIDs[r.IntN(3)], u)
			case 1:
				v := units[r.IntN(len(units))]
				h.transfer(e.Owner, u, v, int64(1+r.IntN(30)))
			default:
				h.submit(player(e.Owner), movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: int32(r.IntN(20)), Y: int32(r.IntN(20))}}}})
			}
		}
		h.step()
		if got := total(h.w()); got != want {
			t.Fatalf("tick %d: total energy %d, want %d", tick+1, got, want)
		}
		for _, u := range units {
			if s, _ := energy.GetStore(h.w(), u); s.Energy < 0 || s.Energy > s.Capacity {
				t.Fatalf("store out of range: %+v", s)
			}
		}
	}
}

// ---- movement cost ---------------------------------------------------------------------

func TestMovementWithInsufficientEnergy(t *testing.T) {
	h := newHarness(t, map[string]string{
		"energy":   `{"unit_capacity": 10, "initial_energy": 7}`,
		"movement": `{"energy_per_step": 3, "max_blocked_ticks": 2}`,
	})
	u := h.unit(1, 0, 0)
	h.submit(player(1), movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: 9, Y: 0}}}})
	fs := h.steps(10)
	// 7 energy pays for two steps (6); then 1 < 3 and the unit stalls.
	if p, _ := h.w().Entity(u); p.Pos.X != 2 || h.stored(u) != 1 {
		t.Fatalf("pos %v energy %d", p.Pos, h.stored(u))
	}
	st := events(fs, movement.EventStalled)
	if len(st) != 1 || field(st[0], "required").Int != 3 || field(st[0], "stored").Int != 1 {
		t.Fatalf("stalled events = %+v", st)
	}
	// Stalling is not being blocked: the order survives max_blocked_ticks.
	if len(events(fs, movement.EventCancelled)) != 0 {
		t.Fatal("stalled order was cancelled")
	}
	// Refuelled by a friendly transfer, it continues.
	donor := h.unit(1, 1, 1)
	h.setEnergy(donor, 10)
	h.step()
	h.transfer(1, donor, u, 5)
	h.steps(2)
	if p, _ := h.w().Entity(u); p.Pos.X <= 2 {
		t.Fatalf("did not resume after refuel: %v", p.Pos)
	}
}
