package visibility_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	salvagev1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/salvage/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/metadata"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/visibility"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// SensorRules adds upgradeable sensor attributes to the standard set.
const SensorRules = `{
  "base_mass": 10,
  "attributes": {
    "vision":    {"default": 2, "min": 0, "max": 8,  "cost": "v * v * 10", "duration": "v"},
    "sensor":    {"default": 3, "min": 0, "max": 20, "cost": "v * v * 5",  "duration": "v"},
    "stealth":   {"default": 0, "min": 0, "max": 20, "cost": "v * v * 8",  "duration": "v"},
    "optics":    {"default": 1, "min": 0, "max": 10, "cost": "v * v * 6",  "duration": "v"},
    "mining_efficiency": {"default": 2, "min": 1, "max": 10, "cost": "v", "duration": "1"}
  }
}`

// fog: vision 2 (chebyshev); detected if sensor - stealth - distance >= 0;
// estimated at optics - distance >= 0, detailed at >= 2.
const fog = `{
  "enabled": true,
  "vision":         {"property": "vision"},
  "sensor":         {"property": "sensor"},
  "signature":      {"property": "stealth"},
  "identification": {"property": "optics"},
  "detection_formula":      "observer_sensor - target_signature - distance",
  "identification_formula": "observer_identification - distance",
  "estimated_threshold": 0,
  "detailed_threshold": 2,
  "resource_signature": 1,
  "disclosure": [
    {"key": "combat.health",   "basic": 50, "estimated": 10, "detailed": 1},
    {"key": "energy.stored",   "estimated": 25, "detailed": 1},
    {"key": "wells.energy",    "basic": 1000, "estimated": 100, "detailed": 1},
    {"key": "wells.unlimited", "basic": 1},
    {"key": "salvage.energy",  "estimated": 10, "detailed": 1},
    {"key": "attributes.*",    "detailed": 1}
  ]
}`

func harness(t *testing.T, overrides map[string]string) *pt.Harness {
	cfg := map[string]string{
		"attributes": SensorRules,
		"energy":     `{"unit_capacity": 1000, "initial_energy": 0}`,
		"visibility": fog,
		"combat":     `{"attack": {"default": 10}, "max_health": {"default": 100}, "cooldown": {"default": 1}, "range": {"default": 3}}`,
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return pt.New(t, cfg)
}

func obsFor(f *sim.Frame, p sim.PlayerID) sim.Observation {
	for _, o := range f.Observations {
		if o.Player == p {
			return o
		}
	}
	return sim.Observation{Player: p}
}

// view tracks one player's observation by applying frames, as a server or
// client would.
type view struct {
	player  sim.PlayerID
	tiles   map[sim.Pos]bool
	foreign map[sim.EntityID]sim.ObservedEntity
}

func newView(p sim.PlayerID) *view {
	return &view{player: p, tiles: map[sim.Pos]bool{}, foreign: map[sim.EntityID]sim.ObservedEntity{}}
}

func (v *view) apply(f *sim.Frame) sim.Observation {
	o := obsFor(f, v.player)
	for _, t := range o.TilesEntered {
		v.tiles[t] = true
	}
	for _, t := range o.TilesExited {
		delete(v.tiles, t)
	}
	for _, u := range o.Upserts {
		v.foreign[u.View.ID] = u
	}
	for _, id := range o.Lost {
		delete(v.foreign, id)
	}
	return o
}

func (v *view) step(h *pt.Harness, n int) []sim.Observation {
	var out []sim.Observation
	for range n {
		out = append(out, v.apply(h.Step()))
	}
	return out
}

func attrOf(o sim.ObservedEntity, key string) (sim.Value, bool) {
	for _, a := range o.View.Attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return sim.Value{}, false
}

func move(h *pt.Harness, slot int, u sim.EntityID, x, y int32) {
	h.Submit(slot, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(u), Destination: &rtsv1.Position{X: x, Y: y}}}})
}

func set(h *pt.Harness, u sim.EntityID, attr string, v int64) {
	i, _ := h.Attrs().Index(attr)
	h.Attrs().Apply(h.W(), u, i, v, 0)
}

func TestInitialAndCombinedVision(t *testing.T) {
	h := harness(t, nil)
	h.Unit(1, 5, 5, 0)
	h.Unit(1, 7, 5, 0) // overlapping vision
	h.Unit(2, 30, 30, 0)
	red, blue := newView(h.P(1)), newView(h.P(2))
	f := h.Step()
	red.apply(f)
	blue.apply(f)
	// Two 5x5 squares overlapping in 3 columns: 5 x 7 = 35 tiles.
	if len(red.tiles) != 35 || len(blue.tiles) != 25 {
		t.Fatalf("visible tiles red %d blue %d", len(red.tiles), len(blue.tiles))
	}
	if !red.tiles[sim.Pos{X: 9, Y: 7}] || red.tiles[sim.Pos{X: 10, Y: 5}] || red.tiles[sim.Pos{X: 5, Y: 8}] {
		t.Fatal("wrong tile set")
	}
	if len(red.foreign) != 0 || len(blue.foreign) != 0 {
		t.Fatal("enemies detected at start")
	}
	// Tile deltas are sorted row-major.
	o := obsFor(f, h.P(1))
	if !slices.IsSortedFunc(o.TilesEntered, func(a, b sim.Pos) int {
		if a.Y != b.Y {
			return int(a.Y - b.Y)
		}
		return int(a.X - b.X)
	}) {
		t.Fatal("tiles not sorted")
	}
	// A unit with vision 0 sees only its own tile.
	h2 := harness(t, map[string]string{"visibility": `{"enabled": true, "vision": {"default": 0}}`})
	h2.Unit(1, 3, 3, 0)
	if o := obsFor(h2.Step(), h2.P(1)); len(o.TilesEntered) != 1 {
		t.Fatalf("vision 0 tiles = %v", o.TilesEntered)
	}
}

func TestVisionGainedAndLostThroughMovement(t *testing.T) {
	h := harness(t, map[string]string{"attributes": SensorRules, "movement": `{"ticks_per_move": 1}`})
	u := h.Unit(1, 5, 5, 0)
	red := newView(h.P(1))
	red.step(h, 1)
	move(h, 1, u, 6, 5)
	o := red.step(h, 1)[0]
	// Moving one tile east: column x=8 enters, column x=3 leaves.
	if len(o.TilesEntered) != 5 || len(o.TilesExited) != 5 || o.TilesEntered[0].X != 8 || o.TilesExited[0].X != 3 {
		t.Fatalf("entered %v exited %v", o.TilesEntered, o.TilesExited)
	}
	if len(red.tiles) != 25 {
		t.Fatalf("tiles = %d", len(red.tiles))
	}
}

func TestDetectionThresholdsAndUpgrades(t *testing.T) {
	h := harness(t, nil)
	scout := h.Unit(1, 5, 5, 0)
	near := h.Unit(2, 6, 5, 0)   // distance 1: 3 - 0 - 1 = 2 >= 0
	far := h.Unit(2, 7, 5, 0)    // distance 2: 3 - 0 - 2 = 1 >= 0
	sneaky := h.Unit(2, 5, 6, 0) // distance 1 but stealth 3: 3 - 3 - 1 < 0
	set(h, sneaky, "stealth", 3)
	out := h.Unit(2, 9, 5, 0) // outside vision 2
	red := newView(h.P(1))
	red.step(h, 1)
	got := func() []sim.EntityID {
		var ids []sim.EntityID
		for id := range red.foreign {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids
	}
	want := []sim.EntityID{near, far}
	slices.Sort(want)
	if fmt.Sprint(got()) != fmt.Sprint(want) {
		t.Fatalf("detected %v, want %v", got(), want)
	}
	// A visible tile is not enough: stealth beats a weak sensor.
	if !red.tiles[sim.Pos{X: 5, Y: 6}] {
		t.Fatal("stealthy unit's tile should be visible")
	}
	// Specializing through the upgrade system: sensor 3 -> 5 detects it.
	h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(scout)}, Attribute: "sensor", Amount: &upgradesv1.UpgradeCommand_Target{Target: 5}})
	energy.SetStored(h.W(), scout, 1000)
	obs := red.step(h, 5)
	if _, ok := red.foreign[sneaky]; !ok {
		t.Fatal("upgraded sensor did not detect the stealthy unit")
	}
	if _, ok := red.foreign[out]; ok {
		t.Fatal("detected beyond vision")
	}
	// Discovered exactly once (at estimated level: optics 1 - distance 1).
	var discovered int
	for _, o := range obs {
		for _, u := range o.Upserts {
			if u.View.ID == sneaky {
				discovered++
			}
		}
	}
	if discovered != 1 {
		t.Fatalf("discovery upserts = %d", discovered)
	}
}

func TestIdentificationLevelsAndEstimates(t *testing.T) {
	h := harness(t, nil)
	scout := h.Unit(1, 5, 5, 0)
	set(h, scout, "optics", 2)
	adj := h.Unit(2, 6, 5, 777) // distance 1: optics 2 - 1 = 1 -> estimated
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(adj), Health: 63})
	red := newView(h.P(1))
	red.step(h, 1)
	o := red.foreign[adj]
	if o.Level != sim.LevelEstimated {
		t.Fatalf("level = %d", o.Level)
	}
	hp, _ := attrOf(o, "combat.health")
	en, _ := attrOf(o, "energy.stored")
	if hp.Kind != sim.ValueRange || hp.Int != 60 || hp.Hi != 69 || en.Int != 775 || en.Hi != 799 {
		t.Fatalf("estimates: health %+v energy %+v", hp, en)
	}
	if _, ok := attrOf(o, "attributes.sensor"); ok {
		t.Fatal("attributes disclosed below detailed")
	}
	// Adjacent observer with optics 3 -> detailed: exact values.
	set(h, scout, "optics", 3)
	red.step(h, 1)
	o = red.foreign[adj]
	hp, _ = attrOf(o, "combat.health")
	if o.Level != sim.LevelDetailed || hp.Kind != sim.ValueInt || hp.Int != 63 {
		t.Fatalf("detailed: level %d health %+v", o.Level, hp)
	}
	if v, ok := attrOf(o, "attributes.sensor"); !ok || v.Int != 3 {
		t.Fatal("prefix rule did not disclose attributes at detailed level")
	}
	// Never disclosed: orders, metadata, upgrade progress.
	h.Submit(2, metadata.CommandSet, &metadatav1.SetMetadataCommand{UnitIds: []uint64{uint64(adj)}, Key: "plan", Value: "secret"})
	move(h, 2, adj, 6, 9)
	red.step(h, 1)
	for _, a := range red.foreign[adj].View.Attrs {
		if a.Key == "metadata.plan" || a.Key == "movement.destination" || a.Key == "upgrades.done" {
			t.Fatalf("disclosed %s", a.Key)
		}
	}
	// Identical runs give identical estimates (deterministic).
	if !sameEstimates(t) {
		t.Fatal("estimates not deterministic")
	}
}

func sameEstimates(t *testing.T) bool {
	run := func() string {
		h := harness(t, nil)
		h.Unit(1, 5, 5, 0)
		e := h.Unit(2, 6, 5, 333)
		v := newView(h.P(1))
		v.step(h, 1)
		return fmt.Sprint(v.foreign[e].View.Attrs)
	}
	first, second := run(), run()
	return first == second
}

func TestHiddenActivityProducesNoObservations(t *testing.T) {
	h := harness(t, map[string]string{"cloning": `{"base_cost": 1, "cost": "base", "duration": "1", "offspring_energy": 0}`})
	h.Unit(1, 2, 2, 0)
	enemy := h.Unit(2, 30, 30, 500)
	friend := h.Unit(2, 31, 30, 0)
	well, _ := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: 30, Y: 31})
	wells.Set(h.W(), well, wells.Well{Energy: 100, Capacity: 100, ExtractionRate: 5})
	red := newView(h.P(1))
	red.step(h, 1)
	// Out of sight: move, mine, transfer, clone, upgrade, be destroyed.
	move(h, 2, enemy, 29, 29)
	h.Submit(2, energy.CommandMine, &energyv1.MineCommand{UnitIds: []uint64{uint64(friend)}, WellId: uint64(well)})
	h.Submit(2, energy.CommandTransfer, &energyv1.TransferCommand{FromUnitId: uint64(enemy), ToUnitId: uint64(friend), Amount: 50})
	h.Submit(2, cloning.CommandClone, &cloningv1.CloneCommand{UnitIds: []uint64{uint64(enemy)}})
	obs := red.step(h, 5)
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(friend), Health: 0})
	obs = append(obs, red.step(h, 3)...)
	for _, o := range obs {
		if len(o.Upserts)+len(o.Lost) != 0 {
			t.Fatalf("red observed hidden activity: %+v", o)
		}
	}
	// And red receives no events about any of it.
	for _, ev := range h.Step().Events {
		if ev.Audience == sim.ToPlayer(h.P(1)) || ev.Audience.All {
			t.Fatalf("red got event %+v", ev)
		}
	}
}

func TestEnteringAndLeavingDetection(t *testing.T) {
	h := harness(t, map[string]string{"movement": `{"ticks_per_move": 1}`})
	h.Unit(1, 5, 5, 0)
	enemy := h.Unit(2, 12, 5, 0)
	red := newView(h.P(1))
	red.step(h, 1)
	move(h, 2, enemy, 6, 5)
	var events []string
	for range 8 {
		f := h.Step()
		red.apply(f)
		for _, ev := range f.Events {
			if ev.Audience == sim.ToPlayer(h.P(1)) && ev.Entity == enemy {
				events = append(events, ev.Type)
			}
		}
	}
	if _, ok := red.foreign[enemy]; !ok {
		t.Fatal("approaching enemy not detected")
	}
	if p := red.foreign[enemy].View.Pos; p != (sim.Pos{X: 6, Y: 5}) {
		t.Fatalf("observed position %v", p)
	}
	move(h, 2, enemy, 15, 5)
	for range 10 {
		f := h.Step()
		red.apply(f)
		for _, ev := range f.Events {
			if ev.Audience == sim.ToPlayer(h.P(1)) && ev.Entity == enemy {
				events = append(events, ev.Type)
			}
		}
	}
	if _, ok := red.foreign[enemy]; ok {
		t.Fatal("departed enemy still observed")
	}
	// Discovered first, lost last; identification may change in between
	// as the distance changes.
	if len(events) < 2 || events[0] != visibility.EventDiscovered || events[len(events)-1] != visibility.EventLost {
		t.Fatalf("events = %v", events)
	}
	for _, e := range events[1 : len(events)-1] {
		if e != visibility.EventIdentified {
			t.Fatalf("unexpected event in %v", events)
		}
	}
}

func TestResourcesAndWreckage(t *testing.T) {
	h := harness(t, map[string]string{"salvage": `{"stored_recovery_permille": 1000}`})
	scout := h.Unit(1, 5, 5, 0)
	near, _ := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: 6, Y: 6})
	wells.Set(h.W(), near, wells.Well{Energy: 1234, Capacity: 2000, ExtractionRate: 5})
	far, _ := h.W().CreateEntity(sim.KindResource, 0, sim.Pos{X: 20, Y: 20})
	wells.Set(h.W(), far, wells.Well{Energy: 10, Capacity: 10, ExtractionRate: 1})
	red := newView(h.P(1))
	red.step(h, 1)
	if _, ok := red.foreign[far]; ok {
		t.Fatal("distant well disclosed")
	}
	o, ok := red.foreign[near]
	if !ok {
		t.Fatal("nearby well not discovered")
	}
	// optics 1 - distance 1 = 0 -> estimated: energy in 100-wide bucket.
	if v, _ := attrOf(o, "wells.energy"); v.Kind != sim.ValueRange || v.Int != 1200 || v.Hi != 1299 {
		t.Fatalf("well energy disclosed as %+v", v)
	}
	if _, ok := attrOf(o, "wells.capacity"); ok {
		t.Fatal("undisclosed well property sent")
	}
	// Mining an unseen well is indistinguishable from mining a nonexistent
	// one.
	r1 := h.Submit(1, energy.CommandMine, &energyv1.MineCommand{UnitIds: []uint64{uint64(scout)}, WellId: uint64(far)})
	r2 := h.Submit(1, energy.CommandMine, &energyv1.MineCommand{UnitIds: []uint64{uint64(scout)}, WellId: 4242})
	f := h.Step()
	red.apply(f)
	a, _ := pt.Result(f, r1)
	b, _ := pt.Result(f, r2)
	if a.OK || b.OK || a.Code != b.Code || stripIDs(a.Message) != stripIDs(b.Message) {
		t.Fatalf("probe distinguishable: %+v vs %+v", a, b)
	}
	// Wreckage: one created far away stays unknown, and its later removal
	// is not observable; one created nearby is discovered, then lost when
	// emptied (indistinguishable from leaving sight).
	enemyFar := h.Unit(2, 25, 25, 40)
	enemyNear := h.Unit(2, 4, 4, 40)
	red.step(h, 1) // the near enemy is now known
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(enemyFar), Health: 0})
	h.Submit(0, combat.CommandSetHealth, &combatv1.SetHealthCommand{UnitId: uint64(enemyNear), Health: 0})
	obs := red.step(h, 1)[0]
	var nearWreck sim.EntityID
	for _, u := range obs.Upserts {
		if _, ok := attrOf(u, "salvage.energy"); ok {
			if u.View.Pos != (sim.Pos{X: 4, Y: 4}) {
				t.Fatalf("distant wreck observed at %v", u.View.Pos)
			}
			nearWreck = u.View.ID
		}
	}
	if nearWreck == 0 || !slices.Contains(obs.Lost, enemyNear) {
		t.Fatalf("near wreck %d, lost %v", nearWreck, obs.Lost)
	}
	h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(scout)}, WreckId: uint64(nearWreck)})
	var lostWreck bool
	for range 10 {
		o := red.step(h, 1)[0]
		lostWreck = lostWreck || slices.Contains(o.Lost, nearWreck)
	}
	if !lostWreck {
		t.Fatal("emptied wreck not reported lost")
	}
}

func TestProbingThroughCommandsIsBlocked(t *testing.T) {
	h := harness(t, nil)
	mine := h.Unit(1, 0, 0, 100)
	hidden := h.Unit(2, 30, 30, 0)
	h.Step()
	nonexistent := sim.EntityID(987654321)
	try := func(target sim.EntityID) []sim.CommandResult {
		ids := []string{
			h.Submit(1, combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(mine)}, TargetId: uint64(target)}),
			h.Submit(1, energy.CommandTransfer, &energyv1.TransferCommand{FromUnitId: uint64(mine), ToUnitId: uint64(target), Amount: 1}),
			h.Submit(1, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: uint64(target), Destination: &rtsv1.Position{}}}}),
			h.Submit(1, upgrades.CommandUpgrade, &upgradesv1.UpgradeCommand{UnitIds: []uint64{uint64(target)}, Attribute: "sensor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 1}}),
			h.Submit(1, metadata.CommandSet, &metadatav1.SetMetadataCommand{UnitIds: []uint64{uint64(target)}, Key: "x", Value: "y"}),
			h.Submit(1, salvage.CommandSalvage, &salvagev1.SalvageCommand{UnitIds: []uint64{uint64(mine)}, WreckId: uint64(target)}),
		}
		f := h.Step()
		var out []sim.CommandResult
		for _, id := range ids {
			r, _ := pt.Result(f, id)
			out = append(out, r)
		}
		return out
	}
	a, b := try(hidden), try(nonexistent)
	for i := range a {
		if a[i].OK || a[i].Code != b[i].Code || stripIDs(a[i].Message) != stripIDs(b[i].Message) {
			t.Errorf("command %d distinguishes a hidden unit from a missing one: %q vs %q", i, a[i].Message, b[i].Message)
		}
	}
	// The hidden unit's owner was not notified of any of it.
	f := h.Step()
	for _, ev := range f.Events {
		if ev.Audience == sim.ToPlayer(h.P(2)) {
			t.Fatalf("victim notified: %+v", ev)
		}
	}
}

func stripIDs(s string) string {
	out := []rune{}
	for _, r := range s {
		if r < '0' || r > '9' {
			out = append(out, r)
		}
	}
	return string(out)
}

func TestCombatWithHiddenAndVisibleUnits(t *testing.T) {
	h := harness(t, map[string]string{"movement": `{"ticks_per_move": 1}`})
	a := h.Unit(1, 5, 5, 100)
	target := h.Unit(2, 7, 5, 100) // distance 2: detected
	h.Step()
	id := h.Submit(1, combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(a)}, TargetId: uint64(target)})
	f := h.Step()
	if r, _ := pt.Result(f, id); !r.OK {
		t.Fatalf("attack on detected unit rejected: %+v", r)
	}
	// It retreats out of detection; the order ends as "unavailable".
	move(h, 2, target, 20, 5)
	var reason string
	for range 6 {
		for _, ev := range h.Step().Events {
			if ev.Type == combat.EventEnded && ev.Entity == a {
				reason = pt.Field(ev, "reason").Str
			}
		}
	}
	if reason != "target_unavailable" {
		t.Fatalf("end reason = %q", reason)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]string{
		"thresholds inverted": `{"estimated_threshold": 5, "detailed_threshold": 1}`,
		"bad metric":          `{"distance_metric": "taxicab"}`,
		"occlusion":           `{"occlusion": "terrain"}`,
		"vision too big":      `{"vision": {"default": 99}}`,
		"unknown property":    `{"sensor": {"property": "radar"}}`,
		"bad formula":         `{"detection_formula": "observer_sensor -"}`,
		"unknown variable":    `{"detection_formula": "observer_charisma"}`,
		"bare variable":       `{"identification_formula": "sensor"}`,
		"wildcard all":        `{"disclosure": [{"key": "*", "basic": 1}]}`,
		"mid wildcard":        `{"disclosure": [{"key": "a*b", "basic": 1}]}`,
		"less precise higher": `{"disclosure": [{"key": "combat.health", "estimated": 5, "detailed": 10}]}`,
		"discloses nothing":   `{"disclosure": [{"key": "combat.health"}]}`,
		"duplicate key":       `{"disclosure": [{"key": "x.y", "basic": 1}, {"key": "x.y", "basic": 1}]}`,
		"negative":            `{"disclosure": [{"key": "x.y", "basic": -1}]}`,
	} {
		c := &game.Config{
			World:   game.WorldConfig{Width: 5, Height: 5, TickRateHz: 10},
			Plugins: map[string]json.RawMessage{"attributes": json.RawMessage(SensorRules), "visibility": json.RawMessage(cfg)},
		}
		if _, err := game.New(c); err == nil {
			t.Errorf("%s: accepted %s", name, cfg)
		}
	}
}

func TestDistanceMetrics(t *testing.T) {
	for metric, want := range map[string]int{"chebyshev": 25, "manhattan": 13, "euclidean": 21} {
		h := harness(t, map[string]string{"visibility": `{"enabled": true, "vision": {"default": 2}, "distance_metric": "` + metric + `"}`})
		h.Unit(1, 10, 10, 0)
		if got := len(obsFor(h.Step(), h.P(1)).TilesEntered); got != want {
			t.Errorf("%s: %d tiles, want %d", metric, got, want)
		}
	}
}
