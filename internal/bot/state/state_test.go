package state

import (
	"encoding/json"
	"testing"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

func serverRules(t *testing.T, preset string) string {
	t.Helper()
	cfg, err := game.LoadConfig("../../../configs/presets/" + preset + ".json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(g.Engine.Rules().Rules())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The bot learns everything about the game from the published rules; the
// parser must understand what the real server publishes.
func TestParseServerRules(t *testing.T) {
	for _, preset := range []string{"duel", "standard", "sandbox", "bot-arena"} {
		r, err := ParseRules(serverRules(t, preset))
		if err != nil {
			t.Fatalf("%s: %v", preset, err)
		}
		if r.CapacityAttr == "" || r.ExtractionAttr == "" || r.AttackAttr == "" || r.HealthAttr == "" {
			t.Fatalf("%s: combat/energy attributes not found: %+v", preset, r)
		}
		if _, ok := r.Attr(r.CapacityAttr); !ok {
			t.Fatalf("%s: capacity attribute %q has no table", preset, r.CapacityAttr)
		}
		cost, ticks, ok := r.UpgradeCost(r.AttackAttr, 1, 3)
		if !ok || cost <= 0 || ticks <= 0 {
			t.Fatalf("%s: attack 1->3 = %d, %d, %v", preset, cost, ticks, ok)
		}
		if _, _, ok := r.UpgradeCost(r.AttackAttr, 3, 1); ok {
			t.Fatalf("%s: a downgrade has a price", preset)
		}
		if r.EnergyPerStep < 0 || r.DefaultHealth <= 0 || (preset == "duel" && r.EnergyPerStep == 0) {
			t.Fatalf("%s: movement/combat defaults missing: %+v", preset, r)
		}
	}
	if _, err := ParseRules("{not json"); err == nil {
		t.Fatal("bad rules accepted")
	}
}

func iv(v int64) *rtsv1.Value { return &rtsv1.Value{Kind: &rtsv1.Value_IntValue{IntValue: v}} }

func rng(lo, hi int64) *rtsv1.Value {
	return &rtsv1.Value{Kind: &rtsv1.Value_RangeValue{RangeValue: &rtsv1.Range{Low: lo, High: hi}}}
}

func knowledge(t *testing.T) *Knowledge {
	k := New()
	m := &rtsv1.MatchInfo{MatchId: "a", State: rtsv1.MatchState_MATCH_STATE_RUNNING, YourPlayerId: 7}
	if err := k.SetWorld(&rtsv1.ConnectResponse{PlayerId: 7, World: &rtsv1.WorldInfo{Width: 20, Height: 10, RulesJson: serverRules(t, "duel")}, Match: m}); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestWellsDepleteAndRefill(t *testing.T) {
	k := knowledge(t)
	w := &rtsv1.Entity{Id: 5, Kind: rtsv1.EntityKind_ENTITY_KIND_RESOURCE, Position: &rtsv1.Position{X: 3, Y: 3}, Attributes: map[string]*rtsv1.Value{"wells.energy": iv(40)}}
	k.ApplySnapshot(&rtsv1.WorldSnapshot{Tick: 1, Entities: []*rtsv1.Entity{w}})
	if len(k.UsableWells()) != 1 || k.Wells[5].Energy != 40 {
		t.Fatal("well not known")
	}
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 2, Events: []*rtsv1.Event{{Type: "energy.well_depleted", EntityId: 5}}})
	if !k.Wells[5].Depleted || len(k.UsableWells()) != 0 {
		t.Fatal("depleted well still usable")
	}
	// An estimate that cannot exclude zero changes nothing...
	w.Attributes["wells.energy"] = rng(0, 99)
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 3, Changed: []*rtsv1.Entity{w}})
	if !k.Wells[5].Depleted {
		t.Fatal("ambiguous estimate cleared depletion")
	}
	// ...seeing it refilled marks it as regenerating: a usable trickle even
	// when it runs dry again.
	w.Attributes["wells.energy"] = iv(12)
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 4, Changed: []*rtsv1.Entity{w}})
	if k.Wells[5].Depleted || k.Wells[5].Regen == 0 {
		t.Fatalf("refill not noticed: %+v", k.Wells[5])
	}
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 5, Events: []*rtsv1.Event{{Type: "energy.well_depleted", EntityId: 5}}})
	if len(k.UsableWells()) != 1 {
		t.Fatal("regenerating well dropped")
	}
}

func TestOwnLossesAndMatchReset(t *testing.T) {
	k := knowledge(t)
	u := &rtsv1.Entity{Id: 1, Kind: rtsv1.EntityKind_ENTITY_KIND_UNIT, OwnerId: 7, Position: &rtsv1.Position{X: 1, Y: 1}}
	f := &rtsv1.Entity{Id: 2, Kind: rtsv1.EntityKind_ENTITY_KIND_UNIT, OwnerId: 9, Position: &rtsv1.Position{X: 4, Y: 1}}
	k.ApplySnapshot(&rtsv1.WorldSnapshot{Tick: 1, Entities: []*rtsv1.Entity{u, f}, Match: k.Match})
	if len(k.OwnUnits()) != 1 || len(k.Enemies()) != 1 {
		t.Fatal("ownership not split")
	}
	k.BeginTick()
	k.ApplyUpdate(&rtsv1.TickUpdate{Tick: 2, Removed: []uint64{1, 2}})
	if len(k.LostOwn) != 1 || k.LostOwn[0] != 1 || len(k.RememberedEnemies()) != 1 {
		t.Fatalf("lost %v, remembered %d", k.LostOwn, len(k.RememberedEnemies()))
	}
	// A new match ID means a new world: everything is forgotten and the
	// (new) player ID adopted.
	k.BeginTick()
	k.ApplySnapshot(&rtsv1.WorldSnapshot{Tick: 0, Match: &rtsv1.MatchInfo{MatchId: "b", YourPlayerId: 8}})
	if !k.Reset || k.Me != 8 || len(k.RememberedEnemies()) != 0 || len(k.Wells) != 0 {
		t.Fatalf("reset not applied: reset=%v me=%d", k.Reset, k.Me)
	}
}

func TestFogTracksVisibleAndExplored(t *testing.T) {
	k := knowledge(t)
	k.ApplySnapshot(&rtsv1.WorldSnapshot{Tick: 1, FogOfWar: true, VisibleTiles: []*rtsv1.TileRun{{Y: 2, X: 0, Length: 5}}})
	if !k.Visible(Pos{4, 2}) || k.Visible(Pos{5, 2}) || k.Explored != 5 {
		t.Fatalf("visibility wrong, explored %d", k.Explored)
	}
	k.BeginTick()
	k.ApplySnapshot(&rtsv1.WorldSnapshot{Tick: 2, FogOfWar: true, VisibleTiles: []*rtsv1.TileRun{{Y: 3, X: 0, Length: 2}}})
	if k.Visible(Pos{4, 2}) || !k.IsExplored(Pos{4, 2}) || k.Explored != 7 {
		t.Fatalf("explored memory wrong: %d", k.Explored)
	}
}
