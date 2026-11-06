package scale_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/dakotaknutson/unit-strategy-game/internal/scale"
)

func TestScenariosAreDeterministic(t *testing.T) {
	for _, sc := range scale.Scenarios {
		a, err := scale.Run(sc, 400, 10, 3)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := scale.Run(sc, 400, 10, 3)
		if a.Hash != b.Hash {
			t.Errorf("%s: state hash differs between identical runs", sc)
		}
	}
}

// Performance regression guard: generous bounds (several times current
// figures) that catch order-of-magnitude regressions — an accidental
// per-tick full recomputation, quadratic scan or allocation explosion —
// without flaking on slower machines. Timing is skipped under -race.
func TestPerformanceRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	limits := map[string]struct {
		allocs uint64
		p95    time.Duration
	}{
		"idle":      {100, 2 * time.Millisecond},
		"movement":  {12000, 25 * time.Millisecond},
		"mining":    {20000, 20 * time.Millisecond},
		"sensors":   {15000, 40 * time.Millisecond},
		"logistics": {10000, 15 * time.Millisecond},
		"combat":    {10000, 20 * time.Millisecond},
		"mixed":     {15000, 60 * time.Millisecond},
	}
	for _, sc := range scale.Scenarios {
		r, err := scale.Run(sc, 1000, 20, 1)
		if err != nil {
			t.Fatal(err)
		}
		l := limits[sc]
		if r.AllocsPerTick > l.allocs {
			t.Errorf("%s: %d allocs/tick > %d", sc, r.AllocsPerTick, l.allocs)
		}
		if !raceEnabled && r.P95 > l.p95 {
			t.Errorf("%s: p95 tick %v > %v", sc, r.P95, l.p95)
		}
		t.Logf("%s 1000 units: p50 %v p95 %v, %d allocs/tick, %.0f events/tick", sc, r.P50, r.P95, r.AllocsPerTick, r.EventsPerTick)
	}
}

// BenchmarkScale reports ns/op per tick for every scenario at 1k and 5k
// units (compare runs with benchstat).
func BenchmarkScale(b *testing.B) {
	for _, sc := range scale.Scenarios {
		for _, n := range []int{1000, 5000} {
			b.Run(fmt.Sprintf("%s/%d", sc, n), func(b *testing.B) {
				var total time.Duration
				var ticks int
				for b.Loop() {
					r, err := scale.Run(sc, n, 10, 1)
					if err != nil {
						b.Fatal(err)
					}
					total += r.Mean * 10
					ticks += 10
				}
				b.ReportMetric(float64(total.Nanoseconds())/float64(ticks), "ns/tick")
			})
		}
	}
}
