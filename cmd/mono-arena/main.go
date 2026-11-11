// Command mono-arena evaluates computer players head to head: it hosts a
// Mono server in process, connects two bots through the public gRPC API and
// plays matches in lockstep (as fast as the bots decide), over several map
// seeds and repeats, then prints per-matchup statistics.
//
//	mono-arena -a 10 -b 1 -seeds 1-10
//	mono-arena -a 6:aggressive -b 6:economic -seeds 1-20 -json results.json
//	mono-arena -ladder -seeds 1-10        # every adjacent pair 1v2 .. 9v10, plus 1v10
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/arena"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

func main() {
	cfgPath := flag.String("config", "configs/presets/duel.json", "two-player server configuration")
	a := flag.String("a", "10", "player A: difficulty[:personality]")
	b := flag.String("b", "1", "player B: difficulty[:personality]")
	seeds := flag.String("seeds", "1-5", "map seeds: N, N-M or comma list")
	repeats := flag.Int("repeats", 1, "matches per seed (bot seeds vary per repeat)")
	aOv := flag.String("a-overrides", "", "JSON difficulty parameter overrides for player A (tuning)")
	bOv := flag.String("b-overrides", "", "JSON difficulty parameter overrides for player B (tuning)")
	ladder := flag.Bool("ladder", false, "run adjacent difficulties 1v2..9v10 and 1v10 instead of -a/-b")
	maxTicks := flag.Int("max-ticks", 0, "safety cap per match (0 = 20000)")
	jsonOut := flag.String("json", "", "write all match results as JSON to this file")
	verbose := flag.Bool("v", false, "print each match")
	logLevel := flag.String("log", "", "print the bots' logs at this level (debug, info)")
	parallel := flag.Int("parallel", runtime.NumCPU(), "matches to run at once (results do not depend on it)")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile to this file")
	flag.Parse()
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}

	cfg, err := game.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	seedList, err := parseSeeds(*seeds)
	if err != nil {
		log.Fatal(err)
	}
	type pair struct{ a, b arena.BotSpec }
	var pairs []pair
	if *ladder {
		for d := 1; d < 10; d++ {
			pairs = append(pairs, pair{arena.BotSpec{Difficulty: d + 1, Personality: "balanced"}, arena.BotSpec{Difficulty: d, Personality: "balanced"}})
		}
		pairs = append(pairs, pair{arena.BotSpec{Difficulty: 10, Personality: "balanced"}, arena.BotSpec{Difficulty: 1, Personality: "balanced"}})
	} else {
		pa, err := parseBot(*a)
		if err != nil {
			log.Fatal(err)
		}
		pb, err := parseBot(*b)
		if err != nil {
			log.Fatal(err)
		}
		if *aOv != "" {
			pa.Overrides = json.RawMessage(*aOv)
		}
		if *bOv != "" {
			pb.Overrides = json.RawMessage(*bOv)
		}
		pairs = append(pairs, pair{pa, pb})
	}
	var all []arena.Result
	fmt.Printf("%-24s %-24s %5s %5s %5s %5s %8s %10s %10s %8s %8s\n", "A", "B", "n", "A won", "B won", "draw", "avg tick", "A mined", "B mined", "A cmds", "A us/t")
	for _, p := range pairs {
		var specs []arena.Spec
		for _, s := range seedList {
			for r := 0; r < *repeats; r++ {
				// Alternate sides so spawn position does not bias the result.
				sa, sb := p.a, p.b
				sa.Seed, sb.Seed = s*1000+uint64(r)*2+1, s*1000+uint64(r)*2+2
				sa.Slot, sb.Slot = 1, 2
				if (int(s)+r)%2 == 1 {
					sa.Slot, sb.Slot = 2, 1
				}
				specs = append(specs, arena.Spec{Config: cfg, MapSeed: s, Bots: []arena.BotSpec{sa, sb}, MaxTicks: *maxTicks, Logger: logger(*logLevel), Debug: *logLevel == "debug"})
			}
		}
		results := make([]arena.Result, len(specs))
		errs := make([]error, len(specs))
		var wg sync.WaitGroup
		sem := make(chan struct{}, max(1, *parallel))
		for i, sp := range specs {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				results[i], errs[i] = arena.Play(context.Background(), sp)
			}()
		}
		wg.Wait()
		sum := arena.Summary{}
		for i, res := range results {
			if errs[i] != nil {
				log.Fatal(errs[i])
			}
			sp := specs[i]
			all = append(all, res)
			sum.Add(res, sp.Bots[0].Slot, sp.Bots[1].Slot)
			if *verbose {
				fmt.Printf("  seed=%d winner_slot=%d reason=%s ticks=%d A=%+v B=%+v\n", res.MapSeed, res.WinnerSlot, res.Reason, res.Ticks, res.Bots[0], res.Bots[1])
			}
		}
		fmt.Printf("%-24s %-24s %5d %5d %5d %5d %8.0f %10.0f %10.0f %8.0f %8.0f\n", label(p.a), label(p.b), sum.N, sum.AWins, sum.BWins, sum.Draws,
			sum.AvgTicks(), sum.AvgMined(0), sum.AvgMined(1), sum.AvgCommands(0), sum.AvgDecisionUs(0))
	}
	if *jsonOut != "" {
		data, _ := json.MarshalIndent(all, "", "  ")
		if err := os.WriteFile(*jsonOut, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func logger(level string) *slog.Logger {
	if level == "" {
		return nil
	}
	l := slog.LevelInfo
	if level == "debug" {
		l = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func label(b arena.BotSpec) string { return fmt.Sprintf("L%d/%s", b.Difficulty, b.Personality) }

func parseBot(s string) (arena.BotSpec, error) {
	d, p, _ := strings.Cut(s, ":")
	n, err := strconv.Atoi(d)
	if err != nil {
		return arena.BotSpec{}, fmt.Errorf("bad bot %q", s)
	}
	if p == "" {
		p = "balanced"
	}
	return arena.BotSpec{Difficulty: n, Personality: p}, nil
}

func parseSeeds(s string) ([]uint64, error) {
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.ParseUint(lo, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad seeds %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.ParseUint(hi, 10, 64); err != nil || b < a {
				return nil, fmt.Errorf("bad seeds %q", s)
			}
		}
		for v := a; v <= b; v++ {
			out = append(out, v)
		}
	}
	return out, nil
}
