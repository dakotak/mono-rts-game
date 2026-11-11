package ai

import (
	"fmt"
	"runtime"
	"testing"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
)

// BenchmarkDecide measures one decision tick (knowledge update + all layers
// due that tick + command flush) at level 10, averaged over many ticks so
// the strategic/operational/tactical cadence is represented, for armies of
// 100 to 10,000 own units facing a tenth as many enemies.
func BenchmarkDecide(b *testing.B) {
	for _, n := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("units=%d", n), func(b *testing.B) {
			side := int32(1)
			for int(side*side) < n*3 {
				side++
			}
			es := []*rtsv1.Entity{well(1, side/4, side/4, 600), well(2, side/3, side/5, 600), well(3, side/2, side/2, 600)}
			for i := range n {
				es = append(es, unit(uint64(10+i), me, int32(i)%side, int32(i)/side, nil))
			}
			for i := range n / 10 {
				es = append(es, unit(uint64(10_000_000+i), foe, side-1-int32(i)%side, side-1-int32(i)/side, nil))
			}
			k := observe(b, 0, es, nil)
			k.Width, k.Height = side, side
			bot := newBot(b, 10, k)
			// Settle: the first ticks assign every role.
			tick := uint64(0)
			for range 50 {
				tick++
				k.BeginTick()
				k.ApplyUpdate(&rtsv1.TickUpdate{Tick: tick})
				bot.Step()
			}
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			heap := float64(ms.HeapAlloc) / (1 << 20)
			units, cmds := 0, 0
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				tick++
				k.BeginTick()
				k.ApplyUpdate(&rtsv1.TickUpdate{Tick: tick})
				for _, o := range bot.Step() {
					units += o.Order.Units
					cmds++
				}
			}
			b.ReportMetric(float64(cmds)/float64(b.N), "cmds/tick")
			b.ReportMetric(float64(units)/float64(b.N), "unitcmds/tick")
			b.ReportMetric(heap, "heapMB")
			runtime.KeepAlive(bot)
		})
	}
}
