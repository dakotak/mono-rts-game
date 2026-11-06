package attributes_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	attributesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/attributes/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// rules is a representative configuration: four attributes with mass
// contributions and convex costs, plus derived properties.
const rules = `{
  "base_mass": 10,
  "attributes": {
    "armor":             {"default": 0,   "min": 0,   "max": 20,  "mass": "v * 3", "cost": "v * v * 5 + v * 10", "duration": "v * 4"},
    "propulsion":        {"default": 1,   "min": 1,   "max": 10,  "mass": "v",     "cost": "(v - 1) * (v - 1) * 20", "duration": "(v - 1) * 6"},
    "energy_capacity":   {"default": 100, "min": 100, "max": 400, "mass": "v / 50", "cost": "(v - 100) * (v - 100) / 50 + (v - 100)", "duration": "(v - 100) / 10"},
    "mining_efficiency": {"default": 2,   "min": 1,   "max": 10,  "mass": "v",     "cost": "pow(v, 3)", "duration": "v * 5"}
  },
  "derived": {
    "ticks_per_move": "clamp(mass / (propulsion * 4), 1, 20)",
    "top_speed":      "100 / ticks_per_move"
  }
}`

func ruleset(t *testing.T, attrCfg string, extra map[string]string) (*sim.Ruleset, *attributes.Plugin) {
	t.Helper()
	a := attributes.New()
	conf := map[string]json.RawMessage{attributes.Name: json.RawMessage(attrCfg)}
	for k, v := range extra {
		conf[k] = json.RawMessage(v)
	}
	rs, err := sim.NewRuleset([]sim.Plugin{wells.New(), a, energy.NewWithAttributes(a), movement.NewWithAttributes(a)}, conf)
	if err != nil {
		t.Fatal(err)
	}
	return rs, a
}

func engine(t *testing.T, extra map[string]string) (*sim.Engine, *attributes.Plugin) {
	t.Helper()
	rs, a := ruleset(t, rules, extra)
	e, err := sim.NewEngine(sim.Config{Width: 30, Height: 30, IDKey: []byte("attributes-tests")}, rs)
	if err != nil {
		t.Fatal(err)
	}
	return e, a
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		"bad name":            `{"attributes": {"Armor": {"max": 1}}}`,
		"reserved name":       `{"attributes": {"mass": {"max": 1}}}`,
		"function name":       `{"attributes": {"pow": {"max": 1}}}`,
		"default below min":   `{"attributes": {"a": {"default": 0, "min": 1, "max": 3}}}`,
		"default above max":   `{"attributes": {"a": {"default": 4, "max": 3}}}`,
		"negative min":        `{"attributes": {"a": {"min": -1, "default": 0, "max": 3}}}`,
		"too many levels":     `{"attributes": {"a": {"max": 1001}}}`,
		"bad formula":         `{"attributes": {"a": {"max": 3, "cost": "v *"}}}`,
		"foreign variable":    `{"attributes": {"a": {"max": 3, "cost": "v * mass"}}}`,
		"decreasing cost":     `{"attributes": {"a": {"max": 3, "cost": "10 - v"}}}`,
		"decreasing duration": `{"attributes": {"a": {"max": 3, "duration": "-v"}}}`,
		"derived unknown var": `{"attributes": {"a": {"max": 3}}, "derived": {"d": "a + b"}}`,
		"derived name clash":  `{"attributes": {"a": {"max": 3}}, "derived": {"a": "1"}}`,
		"derived cycle":       `{"attributes": {"a": {"max": 3}}, "derived": {"x": "y + a", "y": "z", "z": "x"}}`,
		"self reference":      `{"derived": {"x": "x + 1"}}`,
		"unknown field":       `{"attributes": {"a": {"max": 3, "costs": "v"}}}`,
	}
	for name, cfg := range cases {
		a := attributes.New()
		_, err := sim.NewRuleset([]sim.Plugin{a}, map[string]json.RawMessage{attributes.Name: json.RawMessage(cfg)})
		if err == nil {
			t.Errorf("%s: accepted %s", name, cfg)
		}
	}
	// The cycle error names the cycle.
	a := attributes.New()
	_, err := sim.NewRuleset([]sim.Plugin{a}, map[string]json.RawMessage{attributes.Name: json.RawMessage(cases["derived cycle"])})
	if err == nil || !strings.Contains(err.Error(), "x -> y -> z -> x") {
		t.Errorf("cycle error = %v", err)
	}
	// Bindings must name existing properties.
	for _, extra := range []map[string]string{
		{"energy": `{"capacity_attribute": "nope"}`},
		{"energy": `{"extraction_attribute": "nope"}`},
		{"movement": `{"pace_attribute": "nope"}`},
	} {
		a := attributes.New()
		conf := map[string]json.RawMessage{attributes.Name: json.RawMessage(rules)}
		for k, v := range extra {
			conf[k] = json.RawMessage(v)
		}
		if _, err := sim.NewRuleset([]sim.Plugin{wells.New(), a, energy.NewWithAttributes(a), movement.NewWithAttributes(a)}, conf); err == nil {
			t.Errorf("binding %v accepted", extra)
		}
	}
	// Bindings without the attributes plugin are rejected.
	if _, err := sim.NewRuleset([]sim.Plugin{wells.New(), energy.New()}, map[string]json.RawMessage{"energy": json.RawMessage(`{"capacity_attribute": "x"}`)}); err == nil {
		t.Error("binding without attributes plugin accepted")
	}
	// An empty configuration is valid (no attributes).
	if _, err := sim.NewRuleset([]sim.Plugin{attributes.New()}, nil); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestDefaultsDerivedAndMass(t *testing.T) {
	e, a := engine(t, nil)
	u, _ := e.World().CreateEntity(sim.KindUnit, 1, sim.Pos{})
	st, _ := a.Get(e.World(), u)
	prop := func(name string) int64 {
		v, ok := a.Property(st, name)
		if !ok {
			t.Fatalf("no property %q", name)
		}
		return v
	}
	if prop("armor") != 0 || prop("propulsion") != 1 || prop("energy_capacity") != 100 || prop("mining_efficiency") != 2 {
		t.Fatal("defaults not applied")
	}
	// mass = 10 + 0*3 + 1 + 100/50 + 2 = 15
	if prop("mass") != 15 {
		t.Fatalf("mass = %d", prop("mass"))
	}
	// ticks_per_move = clamp(15 / 4, 1, 20) = 3; top_speed = 100/3 = 33
	if prop("ticks_per_move") != 3 || prop("top_speed") != 33 {
		t.Fatalf("derived = %d %d", prop("ticks_per_move"), prop("top_speed"))
	}
	if prop("embodied") != 0 || prop("invested") != 0 {
		t.Fatal("fresh unit has investment")
	}
	// Changing attributes changes derived values.
	i, _ := a.Index("propulsion")
	a.Apply(e.World(), u, i, 4, 0)
	j, _ := a.Index("armor")
	a.Apply(e.World(), u, j, 10, 0)
	st, _ = a.Get(e.World(), u)
	// mass = 10 + 30 + 4 + 2 + 2 = 48; ticks = 48 / 16 = 3
	if prop("mass") != 48 || prop("ticks_per_move") != 3 {
		t.Fatalf("after upgrade mass %d ticks %d", prop("mass"), prop("ticks_per_move"))
	}
	// Kernel derived properties expose the same values.
	if v, _ := e.World().Derived("attributes.mass", u); v != 48 {
		t.Fatalf("kernel derived mass = %d", v)
	}
	// Owner-only observation includes attributes and derived properties.
	f := e.Step()
	var keys []string
	for _, at := range f.Changed[0].Attrs {
		if strings.HasPrefix(at.Key, attributes.AttrPrefix) {
			keys = append(keys, at.Key)
			if at.Visibility != sim.VisibleOwner {
				t.Errorf("%s is not owner-only", at.Key)
			}
		}
	}
	want := "attributes.armor attributes.energy_capacity attributes.mining_efficiency attributes.propulsion attributes.mass attributes.ticks_per_move attributes.top_speed attributes.invested attributes.embodied"
	if strings.Join(keys, " ") != want {
		t.Fatalf("observed keys\n got %s\nwant %s", strings.Join(keys, " "), want)
	}
}

func TestNonlinearPathIndependentCosts(t *testing.T) {
	_, a := engine(t, nil)
	armor, _ := a.Index("armor")
	// cost(v) = 5v² + 10v: each level costs more than the last.
	var prev int64
	for v := int64(0); v < 20; v++ {
		c, _ := a.UpgradeCost(armor, v, v+1)
		if v > 0 && c <= prev {
			t.Fatalf("cost of level %d (%d) not above previous (%d)", v+1, c, prev)
		}
		prev = c
	}
	// One jump costs exactly the same as the sum of single steps.
	total, ticks := a.UpgradeCost(armor, 0, 10)
	var sum, sumTicks int64
	for v := int64(0); v < 10; v++ {
		c, tk := a.UpgradeCost(armor, v, v+1)
		sum += c
		sumTicks += tk
	}
	if total != 600 || total != sum || ticks != 40 || ticks != sumTicks {
		t.Fatalf("jump %d/%d ticks vs steps %d/%d", total, ticks, sum, sumTicks)
	}
	// Durations of at least one tick even when the formula gives 0.
	ec, _ := a.Index("energy_capacity")
	if _, tk := a.UpgradeCost(ec, 100, 105); tk != 1 {
		t.Fatalf("minimum duration = %d", tk)
	}
	// Embodied investment equals the cost of getting there.
	u := a.Defaults()
	u.Values[armor] = 10
	mi, _ := a.Index("mining_efficiency")
	u.Values[mi] = 3 // 27 - 8 = 19
	if got := a.Embodied(u); got != 600+19 {
		t.Fatalf("embodied = %d", got)
	}
}

func TestEnergyAndMovementBindings(t *testing.T) {
	e, a := engine(t, map[string]string{
		"energy":   `{"capacity_attribute": "energy_capacity", "extraction_attribute": "mining_efficiency", "initial_energy": 80}`,
		"movement": `{"pace_attribute": "ticks_per_move"}`,
	})
	w := e.World()
	u, _ := w.CreateEntity(sim.KindUnit, 1, sim.Pos{X: 0, Y: 0})
	if s, _ := energy.GetStore(w, u); s.Capacity != 100 || s.Energy != 80 {
		t.Fatalf("store = %+v", s)
	}
	if r, _ := w.Derived(energy.DerivedExtractionRate, u); r != 2 {
		t.Fatalf("extraction = %d", r)
	}
	if p, _ := w.Derived(movement.DerivedPace, u); p != 3 {
		t.Fatalf("pace = %d", p)
	}
	// Capacity follows the attribute; so does extraction.
	ec, _ := a.Index("energy_capacity")
	a.Apply(w, u, ec, 250, 0)
	mi, _ := a.Index("mining_efficiency")
	a.Apply(w, u, mi, 7, 0)
	if s, _ := energy.GetStore(w, u); s.Capacity != 250 || s.Energy != 80 {
		t.Fatalf("store after upgrade = %+v", s)
	}
	if r, _ := w.Derived(energy.DerivedExtractionRate, u); r != 7 {
		t.Fatalf("extraction after upgrade = %d", r)
	}
	// mass is now 10 + 0 + 1 + 250/50 + 7 = 23, so pace = 23/4 = 5, and
	// pace drives real movement.
	if p, _ := w.Derived(movement.DerivedPace, u); p != 5 {
		t.Fatalf("pace after upgrade = %d", p)
	}
	_, err := e.Submit(sim.Command{ID: "m", Issuer: sim.Principal{Player: 1}, Type: movement.CommandMove,
		Payload: &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: 9}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var xs []int32
	for range 7 {
		e.Step()
		ent, _ := w.Entity(u)
		xs = append(xs, ent.Pos.X)
	}
	if got := fmt.Sprint(xs); got != "[1 1 1 1 1 2 2]" {
		t.Fatalf("x per tick = %s", got)
	}
}

func TestAdminSetAttribute(t *testing.T) {
	e, a := engine(t, map[string]string{"energy": `{"capacity_attribute": "energy_capacity"}`})
	w := e.World()
	u, _ := w.CreateEntity(sim.KindUnit, 1, sim.Pos{})
	admin := sim.Principal{Admin: true}
	submit := func(attr string, v int64) {
		if _, err := e.Submit(sim.Command{ID: attr, Issuer: admin, Type: attributes.CommandSet, Payload: &attributesv1.SetAttributeCommand{UnitId: uint64(u), Attribute: attr, Value: v}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Submit(sim.Command{Issuer: sim.Principal{Player: 1}, Type: attributes.CommandSet, Payload: &attributesv1.SetAttributeCommand{}}); err == nil {
		t.Fatal("player may set attributes")
	}
	submit("energy_capacity", 300)
	submit("armor", 99)
	submit("nope", 1)
	f := e.Step()
	if !f.Results[0].OK || f.Results[1].Code != sim.CodeInvalidArgument || f.Results[2].Code != sim.CodeInvalidArgument {
		t.Fatalf("results = %+v", f.Results)
	}
	if s, _ := energy.GetStore(w, u); s.Capacity != 300 {
		t.Fatalf("capacity = %d", s.Capacity)
	}
	if v, _ := a.Lookup(w, u, "invested"); v != 0 {
		t.Fatal("admin edit counted as investment")
	}
}

func TestDescribePublishesTables(t *testing.T) {
	rs, _ := ruleset(t, rules, nil)
	var d attributes.Description
	if err := json.Unmarshal(rs.Rules()[attributes.Name], &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Attributes) != 4 || d.Attributes[0].Name != "armor" || len(d.Attributes[0].CostTable) != 21 || d.Attributes[0].CostTable[10] != 600 {
		t.Fatalf("description = %+v", d.Attributes[0])
	}
	if strings.Join(d.DerivedOrder, ",") != "ticks_per_move,top_speed" {
		t.Fatalf("derived order = %v", d.DerivedOrder)
	}
	for _, p := range []string{"energy", "movement", "wells"} {
		if len(rs.Rules()[p]) == 0 {
			t.Errorf("plugin %s published no rules", p)
		}
	}
}
