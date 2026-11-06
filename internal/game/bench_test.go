package game_test

import (
	"fmt"
	"math/rand/v2"
	"testing"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// BenchmarkStep measures one tick with every unit holding a move order.
func BenchmarkStep(b *testing.B) {
	for _, units := range []int{1000, 5000, 10000} {
		b.Run(fmt.Sprintf("units=%d", units), func(b *testing.B) {
			const size = 200
			cfg := &game.Config{
				World:   game.WorldConfig{Width: size, Height: size, Seed: 1, TickRateHz: 10},
				Players: []game.PlayerConfig{{ID: 1, JoinToken: "a"}},
			}
			for i := range units {
				cfg.Units = append(cfg.Units, game.UnitBlock{Owner: 1, X: int32(i % size), Y: int32(i / size * 2)})
			}
			e, err := game.NewEngine(cfg)
			if err != nil {
				b.Fatal(err)
			}
			r := rand.New(rand.NewPCG(1, 1))
			reorder := func() {
				m := &movementv1.MoveCommand{}
				flush := func() {
					if len(m.Orders) > 0 {
						_, _ = e.Submit(sim.Command{Issuer: sim.Principal{Player: 1}, Type: movement.CommandMove, Payload: m})
						m = &movementv1.MoveCommand{}
					}
				}
				for _, id := range e.World().EntityIDs() {
					m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(id), Destination: &rtsv1.Position{X: int32(r.IntN(size)), Y: int32(r.IntN(size))}})
					if len(m.Orders) == movement.MaxOrdersPerCommand {
						flush()
					}
				}
				flush()
			}
			reorder()
			i := 0
			for b.Loop() {
				i++
				if i%100 == 0 {
					b.StopTimer()
					reorder()
					b.StartTimer()
				}
				e.Step()
			}
		})
	}
}
