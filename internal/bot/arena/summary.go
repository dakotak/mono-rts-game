package arena

// Summary aggregates one matchup (A vs B) over several matches.
type Summary struct {
	N, AWins, BWins, Draws int
	ticks                  int64
	mined, cmds            [2]int64
	decUs                  [2]float64
}

// Add records a match in which A played slotA and B slotB.
func (s *Summary) Add(r Result, slotA, slotB int) {
	s.N++
	switch r.WinnerSlot {
	case slotA:
		s.AWins++
	case slotB:
		s.BWins++
	default:
		s.Draws++
	}
	s.ticks += r.Ticks
	for _, b := range r.Bots {
		i := 0
		if b.Slot == slotB {
			i = 1
		}
		s.mined[i] += b.EnergyMined
		s.cmds[i] += int64(b.Commands)
		s.decUs[i] += b.AvgDecisionUs
	}
}

func (s *Summary) div(v float64) float64 { return v / float64(max(s.N, 1)) }

// AvgTicks is the mean match duration.
func (s *Summary) AvgTicks() float64 { return s.div(float64(s.ticks)) }

// AvgMined is side i's mean energy mined.
func (s *Summary) AvgMined(i int) float64 { return s.div(float64(s.mined[i])) }

// AvgCommands is side i's mean submitted commands.
func (s *Summary) AvgCommands(i int) float64 { return s.div(float64(s.cmds[i])) }

// AvgDecisionUs is side i's mean per-tick decision time.
func (s *Summary) AvgDecisionUs(i int) float64 { return s.div(s.decUs[i]) }
