// Command rts-sim is a headless simulation harness. It builds a world with
// many units, drives it with pseudo-random move orders for many ticks
// without any network or clients, and reports throughput and the final
// state hash (equal hashes across runs demonstrate determinism).
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"time"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func main() {
	units := flag.Int("units", 5000, "number of units")
	ticks := flag.Int("ticks", 2000, "ticks to simulate")
	size := flag.Int("size", 200, "world width and height")
	players := flag.Int("players", 4, "number of players")
	seed := flag.Uint64("seed", 1, "world and script seed")
	orderEvery := flag.Int("order-every", 20, "issue new orders for each player every N ticks")
	flag.Parse()

	if *units > *size**size/2 {
		log.Fatalf("too many units (%d) for a %dx%d world; keep density <= 50%%", *units, *size, *size)
	}
	// A fixed ID key keeps runs comparable via the state hash.
	cfg := &game.Config{World: game.WorldConfig{Width: int32(*size), Height: int32(*size), Seed: *seed, TickRateHz: 10, IDKey: fmt.Sprintf("%032x", *seed)}}
	for p := 1; p <= *players; p++ {
		cfg.Players = append(cfg.Players, game.PlayerConfig{Slot: p, JoinToken: fmt.Sprint(p)})
	}
	// Scatter units deterministically.
	r := rand.New(rand.NewPCG(*seed, 1))
	taken := map[[2]int32]bool{}
	for i := range *units {
		for {
			x, y := int32(r.IntN(*size)), int32(r.IntN(*size))
			if !taken[[2]int32{x, y}] {
				taken[[2]int32{x, y}] = true
				cfg.Units = append(cfg.Units, game.UnitBlock{Owner: 1 + i%*players, X: x, Y: y})
				break
			}
		}
	}
	gm, err := game.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	engine := gm.Engine

	byOwner := map[sim.PlayerID][]sim.EntityID{}
	for _, id := range engine.World().EntityIDs() {
		e, _ := engine.World().Entity(id)
		byOwner[e.Owner] = append(byOwner[e.Owner], id)
	}

	durations := make([]time.Duration, 0, *ticks)
	var moved, events int
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	start := time.Now()
	for t := range *ticks {
		if t%*orderEvery == 0 {
			// Each player sends half its units toward a random rally point.
			for _, pl := range gm.Players {
				ids := byOwner[pl.ID]
				tx, ty := int32(r.IntN(*size)), int32(r.IntN(*size))
				m := &movementv1.MoveCommand{}
				for i, id := range ids {
					if i%2 == t/(*orderEvery)%2 {
						dx, dy := int32(r.IntN(21)-10), int32(r.IntN(21)-10)
						m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(id), Destination: &rtsv1.Position{
							X: min(max(tx+dx, 0), int32(*size)-1), Y: min(max(ty+dy, 0), int32(*size)-1),
						}})
					}
				}
				for len(m.Orders) > 0 {
					n := min(len(m.Orders), movement.MaxOrdersPerCommand)
					chunk := &movementv1.MoveCommand{Orders: m.Orders[:n]}
					m.Orders = m.Orders[n:]
					if _, err := engine.Submit(sim.Command{ID: fmt.Sprint(t), Issuer: sim.Principal{Player: pl.ID}, Type: movement.CommandMove, Payload: chunk}); err != nil {
						log.Fatal(err)
					}
				}
			}
		}
		t0 := time.Now()
		f := engine.Step()
		durations = append(durations, time.Since(t0))
		moved += len(f.Changed)
		events += len(f.Events)
	}
	total := time.Since(start)
	runtime.ReadMemStats(&ms1)

	slices.Sort(durations)
	pct := func(p float64) time.Duration { return durations[min(int(float64(len(durations))*p), len(durations)-1)] }
	w := os.Stdout
	fmt.Fprintf(w, "world        %dx%d, %d units, %d players, seed %d\n", *size, *size, *units, *players, *seed)
	fmt.Fprintf(w, "ticks        %d in %v (%.0f ticks/s)\n", *ticks, total.Round(time.Millisecond), float64(*ticks)/total.Seconds())
	fmt.Fprintf(w, "tick time    p50 %v  p95 %v  p99 %v  max %v\n", pct(0.50), pct(0.95), pct(0.99), durations[len(durations)-1])
	fmt.Fprintf(w, "activity     %d entity changes, %d events\n", moved, events)
	fmt.Fprintf(w, "allocations  %.1f MB total, %d mallocs\n", float64(ms1.TotalAlloc-ms0.TotalAlloc)/1e6, ms1.Mallocs-ms0.Mallocs)
	fmt.Fprintf(w, "state hash   %016x\n", engine.World().StateHash())
}
