package arena

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

func duel(t *testing.T) *game.Config {
	t.Helper()
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestReproducible: same map seed and bot seeds, same match (deterministic
// decision mode plus lockstep stepping).
func TestReproducible(t *testing.T) {
	spec := Spec{Config: duel(t), MapSeed: 3, MaxTicks: 1500,
		Bots: []BotSpec{{Slot: 1, Difficulty: 7, Personality: "aggressive", Seed: 11}, {Slot: 2, Difficulty: 4, Personality: "economic", Seed: 12}}}
	a, err := Play(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Play(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(r Result) Result {
		r.Elapsed = 0
		for i := range r.Bots {
			r.Bots[i].AvgDecisionUs, r.Bots[i].MaxDecisionUs = 0, 0 // wall clock
		}
		return r
	}
	if !reflect.DeepEqual(strip(a), strip(b)) {
		t.Fatalf("runs differ:\n%+v\n%+v", a, b)
	}
	for _, br := range a.Bots {
		if br.Commands == 0 || br.EnergyMined == 0 || br.FailedCommands*10 > br.Commands {
			t.Fatalf("bot did not play properly: %+v", br)
		}
	}
}

// TestDifficultySeparation: level 10 beats level 1 (full matches; slow).
func TestDifficultySeparation(t *testing.T) {
	if testing.Short() {
		t.Skip("full matches")
	}
	// Same seeding and side alternation as mono-arena (seeds 1-6 of the
	// 20-seed evaluation, where level 10 won 18).
	seeds := []uint64{1, 2, 3, 4, 5, 6}
	res := make([]Result, len(seeds))
	var wg sync.WaitGroup
	for i, s := range seeds {
		wg.Go(func() {
			sa, sb := 1, 2
			if s%2 == 1 {
				sa, sb = 2, 1
			}
			r, err := Play(context.Background(), Spec{Config: duel(t), MapSeed: s,
				Bots: []BotSpec{{Slot: sa, Difficulty: 10, Personality: "balanced", Seed: s*1000 + 1}, {Slot: sb, Difficulty: 1, Personality: "balanced", Seed: s*1000 + 2}}})
			if err != nil {
				t.Error(err)
			}
			res[i] = r
		})
	}
	wg.Wait()
	wins := 0
	for _, r := range res {
		t.Logf("seed %d: %s after %d ticks, %+v", r.MapSeed, r.Reason, r.Ticks, r.Bots)
		if r.Ticks < 6000 && r.Reason == "time_limit" {
			t.Errorf("seed %d ended early", r.MapSeed)
		}
		for _, b := range r.Bots {
			if b.Difficulty == 10 && b.Won {
				wins++
			}
		}
	}
	if wins < 5 {
		t.Fatalf("level 10 won %d of %d against level 1", wins, len(seeds))
	}
}
