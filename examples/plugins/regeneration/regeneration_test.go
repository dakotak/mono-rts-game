package regeneration_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/examples/plugins/regeneration"
	regenerationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/examples/regeneration/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const (
	red  sim.PlayerID = 101
	blue sim.PlayerID = 202
)

// minimal builds the smallest composition the plugin needs: its
// dependencies (energy needs wells) plus the plugin itself.
func minimal(cfg map[string]string) ([]sim.Plugin, map[string]json.RawMessage) {
	attrs := attributes.New()
	plugins := []sim.Plugin{
		wells.New(), attrs, energy.NewWithAttributes(attrs), movement.NewWithAttributes(attrs),
		regeneration.New(attrs),
	}
	raw := map[string]json.RawMessage{"energy": json.RawMessage(`{"unit_capacity": 100, "initial_energy": 0}`)}
	for k, v := range cfg {
		raw[k] = json.RawMessage(v)
	}
	return plugins, raw
}

func engine(t testing.TB, plugins []sim.Plugin, cfg map[string]json.RawMessage) *sim.Engine {
	t.Helper()
	rules, err := sim.NewRuleset(plugins, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sim.NewEngine(sim.Config{Width: 20, Height: 20, Seed: 1, IDKey: []byte("0123456789abcdef")}, rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Init(); err != nil {
		t.Fatal(err)
	}
	return e
}

func unit(t testing.TB, e *sim.Engine, owner sim.PlayerID, x, y int32) sim.EntityID {
	t.Helper()
	id, err := e.World().CreateEntity(sim.KindUnit, owner, sim.Pos{X: x, Y: y})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func stored(e *sim.Engine, id sim.EntityID) int64 {
	v, _ := energy.Stored(e.World(), id)
	return v
}

func submit(t testing.TB, e *sim.Engine, issuer sim.Principal, id string, payload any, typ string) {
	t.Helper()
	if _, err := e.Submit(sim.Command{ID: id, Issuer: issuer, Type: typ, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func result(f *sim.Frame, id string) sim.CommandResult {
	for _, r := range f.Results {
		if r.CommandID == id {
			return r
		}
	}
	return sim.CommandResult{}
}

// ---- registration and configuration ---------------------------------------

func TestRegistration(t *testing.T) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true}`})
	rules, err := sim.NewRuleset(plugins, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rules.Schedule(), ";"), "regeneration: regeneration.step") {
		t.Fatalf("schedule = %v", rules.Schedule())
	}
	if len(rules.Rules()["regeneration"]) == 0 {
		t.Fatal("rules not described")
	}
	found := false
	for _, c := range rules.CommandTypes(false) {
		found = found || c == regeneration.CommandSet
	}
	if !found {
		t.Fatal("command not registered")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, c := range map[string]string{
		"unknown field":     `{"enabled": true, "amout": 2}`,
		"zero interval":     `{"interval": 0}`,
		"limit too high":    `{"limit_percent": 101}`,
		"negative amount":   `{"amount": -1}`,
		"unknown attribute": `{"amount_attribute": "solar_panels"}`,
	} {
		plugins, cfg := minimal(map[string]string{"regeneration": c})
		if _, err := sim.NewRuleset(plugins, cfg); err == nil {
			t.Errorf("%s: accepted %s", name, c)
		}
	}
}

func TestMissingDependencyFailsRegistration(t *testing.T) {
	attrs := attributes.New()
	_, err := sim.NewRuleset([]sim.Plugin{attrs, regeneration.New(attrs)}, nil)
	if err == nil || !strings.Contains(err.Error(), `depends on missing plugin "energy"`) {
		t.Fatalf("err = %v", err)
	}
}

// ---- tick behavior ------------------------------------------------------------

func TestDisabledByDefault(t *testing.T) {
	plugins, cfg := minimal(nil)
	e := engine(t, plugins, cfg)
	u := unit(t, e, red, 1, 1)
	for range 50 {
		e.Step()
	}
	if stored(e, u) != 0 {
		t.Fatalf("regenerated while disabled: %d", stored(e, u))
	}
}

func TestIdleUnitsRegenerateUpToLimit(t *testing.T) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "amount": 3, "interval": 5, "limit_percent": 40}`})
	e := engine(t, plugins, cfg)
	u := unit(t, e, red, 1, 1)
	var events int
	for range 10 { // exactly two steps for every unit in 10 ticks
		for _, ev := range e.Step().Events {
			if ev.Type == regeneration.EventRegen && ev.Entity == u && ev.Audience == sim.ToPlayer(red) {
				events++
			}
		}
	}
	if stored(e, u) != 6 || events != 2 {
		t.Fatalf("after 10 ticks: stored %d, %d events; want 6, 2", stored(e, u), events)
	}
	for range 200 {
		e.Step()
	}
	if stored(e, u) != 40 { // 40% of capacity 100
		t.Fatalf("limit: stored %d, want 40", stored(e, u))
	}
}

func TestMovingUnitsDoNotRegenerate(t *testing.T) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "interval": 1}`})
	e := engine(t, plugins, cfg)
	u := unit(t, e, red, 1, 1)
	energy.SetStored(e.World(), u, 0)
	submit(t, e, sim.Principal{Player: red}, "go", &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{
		{UnitId: uint64(u), Destination: &rtsv1.Position{X: 15, Y: 1}},
	}}, movement.CommandMove)
	e.Step() // command executes; the order exists before regeneration runs
	if stored(e, u) != 0 {
		t.Fatalf("moving unit regenerated: %d", stored(e, u))
	}
}

// ---- commands -------------------------------------------------------------------

func TestSetRegenerationCommand(t *testing.T) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "interval": 1}`})
	e := engine(t, plugins, cfg)
	mine := unit(t, e, red, 1, 1)
	theirs := unit(t, e, blue, 5, 5)

	submit(t, e, sim.Principal{Player: red}, "off", &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(mine)}}, regeneration.CommandSet)
	submit(t, e, sim.Principal{Player: red}, "steal", &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(theirs)}}, regeneration.CommandSet)
	f := e.Step()
	if r := result(f, "off"); !r.OK {
		t.Fatalf("off: %+v", r)
	}
	if r := result(f, "steal"); r.OK || r.Code != sim.CodeNotFound {
		t.Fatalf("foreign unit: %+v", r)
	}
	// The opt-out is applied before the system runs in the same tick.
	if stored(e, mine) != 0 {
		t.Fatalf("disabled unit regenerated: %d", stored(e, mine))
	}
	v, _ := e.World().View(mine)
	if len(v.Attrs) == 0 || !hasAttr(v, regeneration.AttrDisabled) {
		t.Fatal("owner-visible opt-out attribute missing")
	}

	submit(t, e, sim.Principal{Player: red}, "on", &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(mine)}, Enabled: true}, regeneration.CommandSet)
	e.Step()
	if stored(e, mine) != 1 {
		t.Fatalf("re-enabled unit: stored %d, want 1", stored(e, mine))
	}
}

func hasAttr(v sim.EntityView, key string) bool {
	for _, a := range v.Attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

func TestCommandBudgetIsAtomic(t *testing.T) {
	plugins, cfg := minimal(nil)
	rules, err := sim.NewRuleset(plugins, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := sim.NewEngine(sim.Config{Width: 20, Height: 20, IDKey: []byte("0123456789abcdef"), UnitCommandBudget: 1}, rules)
	_ = e.Init()
	a, b := unit(t, e, red, 1, 1), unit(t, e, red, 2, 2)
	submit(t, e, sim.Principal{Player: red}, "two", &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(a), uint64(b)}}, regeneration.CommandSet)
	f := e.Step()
	if r := result(f, "two"); r.OK || r.Code != sim.CodeResourceExhausted {
		t.Fatalf("over budget: %+v", r)
	}
	v, _ := e.World().View(a)
	if hasAttr(v, regeneration.AttrDisabled) {
		t.Fatal("failed command changed state")
	}
}

// ---- determinism ---------------------------------------------------------------------

// run plays a scripted game and returns per-tick state hashes and the
// command log; with a log it replays it instead.
func run(t *testing.T, log [][]sim.Command) ([]uint64, [][]sim.Command) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "interval": 3}`})
	e := engine(t, plugins, cfg)
	var us []sim.EntityID
	for i := range 6 {
		us = append(us, unit(t, e, red, int32(i), 0))
	}
	var hashes []uint64
	var out [][]sim.Command
	for tick := range 30 {
		if log != nil {
			e.StepWith(log[tick])
		} else {
			if tick == 4 {
				submit(t, e, sim.Principal{Player: red}, "off", &regenerationv1.SetRegenerationCommand{UnitIds: []uint64{uint64(us[2])}}, regeneration.CommandSet)
			}
			out = append(out, e.Step().Commands)
		}
		hashes = append(hashes, e.World().StateHash())
	}
	return hashes, out
}

func TestDeterministicAndReplayable(t *testing.T) {
	a, log := run(t, nil)
	b, _ := run(t, nil)
	c, _ := run(t, log)
	for i := range a {
		if a[i] != b[i] || a[i] != c[i] {
			t.Fatalf("tick %d: hashes differ (run %v, replay %v)", i+1, a[i] == b[i], a[i] == c[i])
		}
	}
}

// ---- integration with the full game ---------------------------------------------------

// fullComposition adds the plugin to the real, wired plugin set, sharing
// the attributes instance — exactly what registering it in game.Plugins()
// does.
func fullComposition() []sim.Plugin {
	plugins := game.Plugins()
	for _, p := range plugins {
		if a, ok := p.(*attributes.Plugin); ok {
			return append(plugins, regeneration.New(a))
		}
	}
	panic("no attributes plugin")
}

func TestWorksWithAllBuiltinPlugins(t *testing.T) {
	cfg := map[string]json.RawMessage{
		"attributes": json.RawMessage(`{"base_mass": 10, "attributes": {"solar": {"default": 2, "min": 0, "max": 5, "cost": "v * 10", "duration": "v"}}}`),
		"energy":     json.RawMessage(`{"unit_capacity": 100, "initial_energy": 0}`),
		// amount comes from the upgradable "solar" attribute.
		"regeneration": json.RawMessage(`{"enabled": true, "amount_attribute": "solar", "interval": 1, "limit_percent": 100}`),
	}
	e := engine(t, fullComposition(), cfg)
	u := unit(t, e, red, 3, 3)
	for range 5 {
		e.Step()
	}
	if stored(e, u) != 10 {
		t.Fatalf("stored %d, want 5 ticks x solar 2 = 10", stored(e, u))
	}
	if v, _ := e.World().Derived(regeneration.DerivedAmount, u); v != 2 {
		t.Fatalf("derived amount %d", v)
	}
}

// ---- benchmark ---------------------------------------------------------------------------

func BenchmarkStep10k(b *testing.B) {
	plugins, cfg := minimal(map[string]string{"regeneration": `{"enabled": true, "interval": 10}`})
	rules, _ := sim.NewRuleset(plugins, cfg)
	e, _ := sim.NewEngine(sim.Config{Width: 200, Height: 200, IDKey: []byte("0123456789abcdef")}, rules)
	_ = e.Init()
	for i := range 10000 {
		_, _ = e.World().CreateEntity(sim.KindUnit, red, sim.Pos{X: int32(i % 200), Y: int32(i / 200)})
	}
	for b.Loop() {
		e.Step()
	}
}
