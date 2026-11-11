package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func ov() json.RawMessage {
	if v := os.Getenv("ARENA_OV"); v != "" {
		return json.RawMessage(v)
	}
	return nil
}

// TestDebugDump prints one bot's units periodically (ARENA_DEBUG=1;
// ARENA_SEED, ARENA_A (slot 1 level), ARENA_B, ARENA_EVERY, ARENA_TICKS).
func TestDebugDump(t *testing.T) {
	if os.Getenv("ARENA_DEBUG") == "" {
		t.Skip("diagnostics only")
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	every := uint64(envInt("ARENA_EVERY", 500))
	var lg *slog.Logger
	if os.Getenv("ARENA_LOG") != "" {
		lg = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	res, err := Play(context.Background(), Spec{Config: cfg, MapSeed: uint64(envInt("ARENA_SEED", 1)), MaxTicks: envInt("ARENA_TICKS", 3000),
		Logger: lg, Debug: lg != nil,
		Bots: []BotSpec{{Slot: 1, Difficulty: envInt("ARENA_A", 10), Personality: "balanced", Seed: 1, Overrides: ov()}, {Slot: 2, Difficulty: envInt("ARENA_B", 1), Personality: "balanced", Seed: 2}},
		Inspect: func(slot int, b *ai.Bot) {
			if slot != envInt("ARENA_SLOT", 1) || b.K.Tick%every != 0 {
				return
			}
			s := b.Status
			fmt.Printf("tick %d role_changes=%d strategy=%s units=%d campaign=%q threats=%d own=%d est=%d\n", b.K.Tick, s.RoleChanges, s.Strategy, len(b.K.OwnUnits()), s.Campaign, s.Threats, s.OwnStrength, s.EnemyEstimate)
			for _, id := range state.Sorted(b.K.Wells) {
				w := b.K.Wells[id]
				fmt.Printf("  well %s current=%v energy=%d depleted=%v regen=%d\n", w.Pos, w.Current, w.Energy, w.Depleted, w.Regen)
			}
			for _, u := range b.K.OwnUnits() {
				fmt.Printf("  %4d %-8s %-9s e=%3d/%3d q=%3d %s/%s/%s busy=%s me=%d atk=%d hp=%d\n", u.ID%10000, u.Pos, b.Roles[u.ID],
					u.IntOr("energy.stored", -1), u.IntOr("energy.capacity", -1), u.IntOr("cloning.quote_cost", -1),
					u.Str("automation.behavior"), u.Str("automation.state"), u.Str("automation.reason"), u.Str("attributes.busy"),
					u.IntOr("attributes.mining_efficiency", -1), u.IntOr("attributes.attack", -1), u.IntOr("combat.health", -1))
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("result: %+v\n", res)
}

// TestDebugBisect applies single parameters of level ARENA_A to level
// ARENA_B (both sides, mirror match) and prints the average energy mined.
func TestDebugBisect(t *testing.T) {
	if os.Getenv("ARENA_BISECT") == "" {
		t.Skip("diagnostics only")
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	hi, lo := envInt("ARENA_A", 10), envInt("ARENA_B", 1)
	hp, _ := difficulty.Level(hi)
	raw, _ := json.Marshal(hp)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	run := func(name string, ov json.RawMessage) {
		var mined, units int64
		n := 0
		for seed := uint64(1); seed <= 6; seed++ {
			res, err := Play(context.Background(), Spec{Config: cfg, MapSeed: seed, MaxTicks: envInt("ARENA_TICKS", 3000),
				Bots: []BotSpec{{Slot: 1, Difficulty: lo, Personality: "balanced", Seed: seed, Overrides: ov},
					{Slot: 2, Difficulty: lo, Personality: "balanced", Seed: seed + 100, Overrides: ov}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range res.Bots {
				mined += b.EnergyMined
				units += b.Units
				n++
			}
		}
		fmt.Printf("%-24s mined=%6d units=%3d\n", name, mined/int64(n), units/int64(n))
	}
	run("baseline", nil)
	for k, v := range fields {
		if k == "level" || k == "name" {
			continue
		}
		run(k, json.RawMessage(fmt.Sprintf(`{%q:%s}`, k, v)))
	}
}

// TestDebugBisectWins plays level ARENA_B with one parameter taken from
// level ARENA_A against plain level ARENA_B and prints wins per parameter.
func TestDebugBisectWins(t *testing.T) {
	if os.Getenv("ARENA_BISECT_WINS") == "" {
		t.Skip("diagnostics only")
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	hi, lo := envInt("ARENA_A", 10), envInt("ARENA_B", 9)
	hp, _ := difficulty.Level(hi)
	raw, _ := json.Marshal(hp)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	seeds := uint64(envInt("ARENA_SEEDS", 12))
	run := func(name string, ov json.RawMessage) {
		wins, mined := 0, int64(0)
		for seed := uint64(1); seed <= seeds; seed++ {
			sa, sb := 1, 2
			if seed%2 == 0 {
				sa, sb = 2, 1
			}
			res, err := Play(context.Background(), Spec{Config: cfg, MapSeed: seed,
				Bots: []BotSpec{{Slot: sa, Difficulty: lo, Personality: "balanced", Seed: seed, Overrides: ov},
					{Slot: sb, Difficulty: lo, Personality: "balanced", Seed: seed + 100}}})
			if err != nil {
				t.Fatal(err)
			}
			if res.WinnerSlot == sa {
				wins++
			}
			for _, b := range res.Bots {
				if b.Slot == sa {
					mined += b.EnergyMined
				}
			}
		}
		fmt.Printf("%-24s wins=%2d/%d mined=%d\n", name, wins, seeds, mined/int64(seeds))
	}
	run("baseline", nil)
	for k, v := range fields {
		if k == "level" || k == "name" {
			continue
		}
		run(k, json.RawMessage(fmt.Sprintf(`{%q:%s}`, k, v)))
	}
}

// TestDebugRevert plays level ARENA_A with one parameter reverted to level
// ARENA_B's value against level ARENA_OPP, for every parameter, and prints
// the wins (which parameters hurt the higher level).
func TestDebugRevert(t *testing.T) {
	if os.Getenv("ARENA_REVERT") == "" {
		t.Skip("diagnostics only")
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	hi, lo, opp := envInt("ARENA_A", 10), envInt("ARENA_B", 7), envInt("ARENA_OPP", 5)
	lp, _ := difficulty.Level(lo)
	raw, _ := json.Marshal(lp)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	names := []string{"baseline"}
	for k := range fields {
		if k != "level" && k != "name" {
			names = append(names, k)
		}
	}
	seeds := envInt("ARENA_SEEDS", 12)
	type job struct {
		name string
		seed int
	}
	wins := map[string]int{}
	var mu sync.Mutex
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range runtime.NumCPU() / 2 {
		wg.Go(func() {
			for j := range jobs {
				var ov json.RawMessage
				if j.name != "baseline" {
					ov = json.RawMessage(fmt.Sprintf(`{%q:%s}`, j.name, fields[j.name]))
				}
				sa, sb := 1, 2
				if j.seed%2 == 0 {
					sa, sb = 2, 1
				}
				res, err := Play(context.Background(), Spec{Config: cfg, MapSeed: uint64(j.seed),
					Bots: []BotSpec{{Slot: sa, Difficulty: hi, Personality: "balanced", Seed: uint64(j.seed), Overrides: ov},
						{Slot: sb, Difficulty: opp, Personality: "balanced", Seed: uint64(j.seed) + 100}}})
				if err != nil {
					t.Error(err)
					continue
				}
				mu.Lock()
				if res.WinnerSlot == sa {
					wins[j.name]++
				}
				mu.Unlock()
			}
		})
	}
	for _, n := range names {
		for s := 1; s <= seeds; s++ {
			jobs <- job{n, s}
		}
	}
	close(jobs)
	wg.Wait()
	for _, n := range names {
		fmt.Printf("%-24s wins=%2d/%d\n", n, wins[n], seeds)
	}
}

// TestDebugMoves measures movement energy per role for both bots
// (ARENA_MOVES=1, ARENA_SEED, ARENA_A, ARENA_B).
func TestDebugMoves(t *testing.T) {
	if os.Getenv("ARENA_MOVES") == "" {
		t.Skip("diagnostics only")
	}
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	type acc struct {
		prev  map[uint64]state.Pos
		steps map[ai.Role]int64
	}
	accs := map[int]*acc{1: {map[uint64]state.Pos{}, map[ai.Role]int64{}}, 2: {map[uint64]state.Pos{}, map[ai.Role]int64{}}}
	var mu sync.Mutex
	res, err := Play(context.Background(), Spec{Config: cfg, MapSeed: uint64(envInt("ARENA_SEED", 1)), MaxTicks: envInt("ARENA_TICKS", 6000),
		Bots: []BotSpec{{Slot: 1, Difficulty: envInt("ARENA_A", 10), Personality: "balanced", Seed: 1, Overrides: ov()}, {Slot: 2, Difficulty: envInt("ARENA_B", 5), Personality: "balanced", Seed: 2}},
		Inspect: func(slot int, b *ai.Bot) {
			mu.Lock()
			defer mu.Unlock()
			a := accs[slot]
			for _, u := range b.K.OwnUnits() {
				if p, ok := a.prev[u.ID]; ok {
					a.steps[b.Roles[u.ID]] += int64(state.Cheb(p, u.Pos))
				}
				a.prev[u.ID] = u.Pos
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.Bots {
		fmt.Printf("slot %d L%d score=%d mined=%d emb=%d stored=%d units=%d moves=%v\n  orders=%v\n", b.Slot, b.Difficulty, b.Score, b.EnergyMined, b.Embodied, b.Stored, b.Units, accs[b.Slot].steps, b.OrdersByKind)
	}
}
