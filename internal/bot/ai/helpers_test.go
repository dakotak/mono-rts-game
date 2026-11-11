package ai

import (
	"encoding/json"
	"sort"
	"testing"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// The tests build knowledge from the real server rules (the duel preset)
// and hand-made observations, so decisions are checked against the same
// rules the bot meets in a match. (Test code may import the game; the bot
// itself may not — see TestImportBoundary.)

const me, foe = uint32(7), uint32(9)

var worldInfo *rtsv1.WorldInfo

func testWorld(t testing.TB) *rtsv1.WorldInfo {
	t.Helper()
	if worldInfo != nil {
		return worldInfo
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := json.Marshal(g.Engine.Rules().Rules())
	if err != nil {
		t.Fatal(err)
	}
	worldInfo = &rtsv1.WorldInfo{Width: 48, Height: 32, TickRateHz: 10, UnitCommandBudget: 200,
		CommandTypes: g.Engine.Rules().CommandTypes(false), RulesJson: string(rules)}
	return worldInfo
}

func iv(v int64) *rtsv1.Value { return &rtsv1.Value{Kind: &rtsv1.Value_IntValue{IntValue: v}} }

// unit builds an observed unit with integer attributes.
func unit(id uint64, owner uint32, x, y int32, attrs map[string]int64) *rtsv1.Entity {
	e := &rtsv1.Entity{Id: id, Kind: rtsv1.EntityKind_ENTITY_KIND_UNIT, OwnerId: owner,
		Position: &rtsv1.Position{X: x, Y: y}, Attributes: map[string]*rtsv1.Value{}}
	base := map[string]int64{"energy.stored": 60, "energy.capacity": 100, "combat.health": 50, "combat.max_health": 50,
		"combat.attack": 1, "cloning.quote_cost": 61, "cloning.quote_offspring_energy": 20}
	for k, v := range base {
		e.Attributes[k] = iv(v)
	}
	for k, v := range attrs {
		e.Attributes[k] = iv(v)
	}
	return e
}

func well(id uint64, x, y int32, energy int64) *rtsv1.Entity {
	return &rtsv1.Entity{Id: id, Kind: rtsv1.EntityKind_ENTITY_KIND_RESOURCE, Position: &rtsv1.Position{X: x, Y: y},
		Attributes: map[string]*rtsv1.Value{"wells.energy": iv(energy)}}
}

// observe builds knowledge for player me from one snapshot. visible lists
// visible rows [y0, y1) x [x0, x1) under fog; nil means no fog.
func observe(t testing.TB, tick uint64, entities []*rtsv1.Entity, visible *[4]int32) *state.Knowledge {
	t.Helper()
	k := state.New()
	match := &rtsv1.MatchInfo{MatchId: "m1", State: rtsv1.MatchState_MATCH_STATE_RUNNING, YourPlayerId: me, TimeLimitTicks: 6000, TicksRemaining: 6000 - int64(tick)}
	if err := k.SetWorld(&rtsv1.ConnectResponse{PlayerId: me, World: testWorld(t), Match: match}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Id < entities[j].Id })
	snap := &rtsv1.WorldSnapshot{Tick: tick, Entities: entities, Match: match}
	if visible != nil {
		snap.FogOfWar = true
		for y := visible[1]; y < visible[3]; y++ {
			snap.VisibleTiles = append(snap.VisibleTiles, &rtsv1.TileRun{Y: y, X: visible[0], Length: visible[2] - visible[0]})
		}
	}
	k.BeginTick()
	k.ApplySnapshot(snap)
	return k
}

func newBot(t testing.TB, level int, k *state.Knowledge, extra ...Heuristic) *Bot {
	t.Helper()
	p, err := difficulty.Level(level)
	if err != nil {
		t.Fatal(err)
	}
	pers, _ := difficulty.PersonalityByName("balanced")
	return New(k, Config{Difficulty: p, Personality: pers, Seed: 1, Nonce: "t", Heuristics: extra})
}

// payloads returns the outgoing commands of a given Go type.
func payloadsOf[T any](out []Outgoing) []T {
	var r []T
	for _, o := range out {
		if v, ok := o.Payload.(T); ok {
			r = append(r, v)
		}
	}
	return r
}
