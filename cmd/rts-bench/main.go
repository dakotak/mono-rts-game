// Command rts-bench runs the repeatable scale scenarios (internal/scale)
// and prints tick timing, allocation, memory and event-volume figures
// against the 100 ms tick interval (10 Hz).
//
//	go run ./cmd/rts-bench -units 1000,5000,10000,25000 -scenarios all -ticks 40
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dakotaknutson/unit-strategy-game/internal/scale"
)

func main() {
	unitsFlag := flag.String("units", "1000,5000,10000,25000", "comma-separated unit counts")
	scenFlag := flag.String("scenarios", "all", "comma-separated scenarios, or all: "+strings.Join(scale.Scenarios, ","))
	ticks := flag.Int("ticks", 40, "measured ticks per run")
	seed := flag.Uint64("seed", 1, "seed")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of all runs to this file")
	memprofile := flag.String("memprofile", "", "write an allocation profile of all runs to this file")
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}

	scenarios := scale.Scenarios
	if *scenFlag != "all" {
		scenarios = strings.Split(*scenFlag, ",")
	}
	var sizes []int
	for _, s := range strings.Split(*unitsFlag, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			log.Fatalf("bad unit count %q", s)
		}
		sizes = append(sizes, n)
	}
	fmt.Printf("%s %s/%s, GOMAXPROCS %d (simulation is single-threaded), tick budget %v\n\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), scale.TickInterval)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "scenario\tunits\tp50\tp95\tmax\tp95/budget\tallocs/tick\tKB/tick\theap MB\tevents/tick\tchanged/tick\tobserved/tick\tsetup\t")
	for _, sc := range scenarios {
		for _, n := range sizes {
			r, err := scale.Run(sc, n, *ticks, *seed)
			if err != nil {
				log.Fatalf("%s/%d: %v", sc, n, err)
			}
			ms := func(d time.Duration) string { return fmt.Sprintf("%.2fms", float64(d)/1e6) }
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%.0f%%\t%d\t%d\t%.0f\t%.0f\t%.0f\t%.0f\t%s\t\n",
				sc, n, ms(r.P50), ms(r.P95), ms(r.Max), 100*r.BudgetFraction95, r.AllocsPerTick, r.BytesPerTick/1024,
				r.HeapMB, r.EventsPerTick, r.ChangedPerTick, r.ObservedPerTick, r.Setup.Round(time.Millisecond))
		}
	}
	tw.Flush()
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			log.Fatal(err)
		}
		f.Close()
	}
}
