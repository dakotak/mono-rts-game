package difficulty

import "testing"

func TestLevelsAreCompleteAndMonotonic(t *testing.T) {
	ls := Levels()
	if len(ls) != 10 {
		t.Fatalf("%d levels", len(ls))
	}
	for i, p := range ls {
		if p.Level != i+1 || p.Name == "" {
			t.Fatalf("level %d: %+v", i+1, p)
		}
		if p.StrategicInterval < 1 || p.OperationalInterval < 1 || p.TacticalInterval < 1 || p.ComputeBudget < 1 || p.Candidates < 1 {
			t.Fatalf("level %d has a non-positive budget: %+v", p.Level, p)
		}
		if i == 0 {
			continue
		}
		q := ls[i-1]
		// Higher levels decide more often and better: never worse on any axis.
		checks := []struct {
			name string
			ok   bool
		}{
			{"strategic_interval", p.StrategicInterval <= q.StrategicInterval},
			{"operational_interval", p.OperationalInterval <= q.OperationalInterval},
			{"tactical_interval", p.TacticalInterval <= q.TacticalInterval},
			{"reaction_delay", p.ReactionDelay <= q.ReactionDelay},
			{"planning_horizon", p.PlanningHorizon >= q.PlanningHorizon},
			{"candidates", p.Candidates >= q.Candidates},
			{"compute_budget", p.ComputeBudget >= q.ComputeBudget},
			{"economic_efficiency", p.EconomicEfficiency >= q.EconomicEfficiency},
			{"coordination", p.Coordination >= q.Coordination},
			{"threat_model", p.ThreatModel >= q.ThreatModel},
			{"specialization", p.Specialization >= q.Specialization},
			{"adaptation", p.Adaptation >= q.Adaptation},
			{"mistake_rate", p.MistakeRate <= q.MistakeRate},
			{"min_commitment", p.MinCommitment <= q.MinCommitment},
		}
		for _, c := range checks {
			if !c.ok {
				t.Errorf("%s gets worse from level %d to %d", c.name, q.Level, p.Level)
			}
		}
		for _, f := range [][2]bool{{q.UseLogistics, p.UseLogistics}, {q.UseSalvage, p.UseSalvage}, {q.UseGuards, p.UseGuards}, {q.Retreat, p.Retreat}, {q.Scout, p.Scout}} {
			if f[0] && !f[1] {
				t.Errorf("a capability is lost from level %d to %d", q.Level, p.Level)
			}
		}
	}
	if ls[9].MistakeRate != 0 {
		t.Error("level 10 makes deliberate mistakes")
	}
}

func TestLevelBounds(t *testing.T) {
	for _, n := range []int{0, 11, -1} {
		if _, err := Level(n); err == nil {
			t.Errorf("Level(%d) accepted", n)
		}
	}
	a, _ := Level(3)
	a.Candidates = 99
	if b, _ := Level(3); b.Candidates == 99 {
		t.Fatal("Level returns shared state")
	}
}

func TestPersonalities(t *testing.T) {
	for _, n := range []string{"balanced", "aggressive", "defensive", "economic", "exploratory"} {
		p, err := PersonalityByName(n)
		if err != nil || p.Name != n {
			t.Fatalf("%s: %v %+v", n, err, p)
		}
	}
	if _, err := PersonalityByName("reckless"); err == nil {
		t.Fatal("unknown personality accepted")
	}
}
