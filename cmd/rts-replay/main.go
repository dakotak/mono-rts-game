// Command rts-replay inspects and verifies Mono replay files (format
// mono-replay/1, exported with AdminService.ExportReplay or the admin
// client). Verification rebuilds the match from the recorded
// configuration and seed, replays every recorded command at its tick with
// no clients connected, and compares frame digests and state hashes.
//
//	rts-replay info   match.json
//	rts-replay verify match.json      # exit status 1 on divergence
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/replay"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "info" && os.Args[1] != "verify") {
		fmt.Fprintln(os.Stderr, "usage: rts-replay info|verify FILE")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	r, err := replay.Parse(data)
	if err != nil {
		log.Fatal(err)
	}
	commands := 0
	for _, t := range r.Ticks {
		commands += len(t.Commands)
	}
	fmt.Printf("match      %s\nformat     %s\nengine     %s (this build: %s)\nruleset    %s\nseed       %d\nticks      %d (%d commands in %d ticks, %d checkpoints)\n",
		r.MatchID, r.Format, r.EngineVersion, game.EngineVersion, r.RulesetHash, r.Seed, r.FinalTick, commands, len(r.Ticks), len(r.Checkpoints))
	for _, p := range r.Players {
		fmt.Printf("player     slot %d %q id %d\n", p.Slot, p.Name, p.ID)
	}
	if len(r.Result) > 0 {
		var res map[string]any
		_ = json.Unmarshal(r.Result, &res)
		fmt.Printf("result     winner %v draw %v reason %v\n", res["winnerId"], res["draw"], res["reason"])
	}
	if os.Args[1] == "info" {
		return
	}
	start := time.Now()
	rep, err := replay.Verify(r)
	if err != nil {
		log.Fatal(err)
	}
	if !rep.EngineMatches || !rep.RulesetMatches {
		fmt.Printf("warning    engine matches: %v, ruleset matches: %v (results may legitimately differ)\n", rep.EngineMatches, rep.RulesetMatches)
	}
	if rep.OK {
		fmt.Printf("verify     OK: %d ticks replayed, %d checkpoints matched, final hash %s (%v)\n",
			rep.TicksReplayed, rep.CheckpointsChecked, rep.FinalHash, time.Since(start).Round(time.Millisecond))
		return
	}
	fmt.Printf("verify     DIVERGED at tick %d: %s\n", rep.FirstDivergentTick, rep.Detail)
	os.Exit(1)
}
