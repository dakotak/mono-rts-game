package ai

import (
	"fmt"
	"slices"
	"strings"
)

// Directive is what a strategy asks of the operational and tactical
// layers.
type Directive struct {
	// Population is the target number of own units.
	Population int
	// MinerShare (permille of units) devoted to mining.
	MinerShare int
	// Scouts and Guards wanted (before difficulty and personality).
	Scouts, Guards int
	// UpgradeShare (permille): preference for upgrades over clones when
	// both are affordable.
	UpgradeShare int
	// AttackRatio (permille): attack when own strength >= enemy estimate x
	// ratio; 0 never attacks.
	AttackRatio int
	// Reserve energy the bank keeps.
	Reserve int64
	// DenyResources prefers enemy units near wells (likely miners).
	DenyResources bool
}

// Strategy is a named strategic approach.
type Strategy struct {
	Name      string
	Directive Directive
}

// Strategies are the built-in strategies (exported so extensions can add
// to them).
var Strategies = []Strategy{
	{"economic_expansion", Directive{Population: 26, MinerShare: 600, Scouts: 2, UpgradeShare: 300, AttackRatio: 2500, Reserve: 30}},
	{"defensive", Directive{Population: 20, MinerShare: 450, Scouts: 1, Guards: 3, UpgradeShare: 500, AttackRatio: 3000, Reserve: 40}},
	{"aggressive", Directive{Population: 18, MinerShare: 380, Scouts: 1, UpgradeShare: 350, AttackRatio: 1100, Reserve: 20}},
	{"tech", Directive{Population: 16, MinerShare: 450, Scouts: 1, Guards: 1, UpgradeShare: 750, AttackRatio: 1600, Reserve: 30}},
	{"balanced", Directive{Population: 20, MinerShare: 500, Scouts: 1, Guards: 1, UpgradeShare: 450, AttackRatio: 1500, Reserve: 30}},
	{"resource_denial", Directive{Population: 18, MinerShare: 420, Scouts: 2, UpgradeShare: 400, AttackRatio: 1200, Reserve: 20, DenyResources: true}},
}

func strategyByName(name string) (Strategy, bool) {
	for _, s := range Strategies {
		if s.Name == name {
			return s, true
		}
	}
	return Strategy{}, false
}

// Features summarize the bot's knowledge for strategy selection. All of
// them come from the bot's own observations.
type Features struct {
	Tick          uint64
	TicksLeft     int64 // -1: no time limit
	OwnUnits      int
	Fighters      int
	OwnStrength   int64
	EnemyStrength int64 // estimate (current + remembered, decayed)
	EnemiesSeen   int
	ThreatHome    int64 // enemy strength near home now
	WellsKnown    int
	LostRecently  int
	BankEnergy    int64
	ExploredPm    int // permille of the map explored
}

// Heuristic scores how well a strategy fits the situation (extension
// point). Scores are permille-ish utilities; positive favors.
type Heuristic interface {
	Name() string
	Score(f *Features, strategy string) int
}

// HeuristicFunc adapts a function.
type HeuristicFunc struct {
	N string
	F func(f *Features, strategy string) int
}

func (h HeuristicFunc) Name() string                           { return h.N }
func (h HeuristicFunc) Score(f *Features, strategy string) int { return h.F(f, strategy) }

func builtinHeuristics() []Heuristic {
	return []Heuristic{
		HeuristicFunc{"threat", func(f *Features, s string) int {
			if f.ThreatHome == 0 {
				return 0
			}
			switch s {
			case "defensive":
				return 450
			case "economic_expansion", "tech":
				return -250
			}
			return 0
		}},
		HeuristicFunc{"strength", func(f *Features, s string) int {
			if f.EnemiesSeen == 0 {
				return 0
			}
			ratio := f.OwnStrength * 1000 / max(f.EnemyStrength, 1)
			switch {
			case ratio > 1600 && (s == "aggressive" || s == "resource_denial"):
				return 350
			case ratio < 800 && s == "defensive":
				return 300
			case ratio < 800 && s == "tech":
				return 150
			case ratio < 800 && s == "aggressive":
				return -300
			}
			return 0
		}},
		HeuristicFunc{"economy", func(f *Features, s string) int {
			if f.WellsKnown >= 2 && f.OwnUnits < 12 && s == "economic_expansion" {
				return 250
			}
			if f.WellsKnown == 0 && s == "economic_expansion" {
				return -200
			}
			return 0
		}},
		HeuristicFunc{"time", func(f *Features, s string) int {
			// Late in a timed match, score (units, investment) matters.
			if f.TicksLeft >= 0 && f.TicksLeft < 1500 {
				switch s {
				case "economic_expansion":
					return 200
				case "aggressive":
					return -100
				}
			}
			return 0
		}},
		HeuristicFunc{"losses", func(f *Features, s string) int {
			if f.LostRecently >= 3 && s == "defensive" {
				return 250
			}
			return 0
		}},
	}
}

// strategyState is the current strategy with commitment bookkeeping.
type strategyState struct {
	name   string
	since  uint64
	scores map[string]int
	lost   int // own units lost since the last strategic evaluation
}

// strategic evaluates strategies and keeps or switches (with hysteresis).
func (b *Bot) strategic() {
	f := b.features()
	scores := map[string]int{}
	for _, s := range Strategies {
		if !b.spend(len(b.heuristics)) {
			break
		}
		u := b.Pers.StrategyBias[s.Name]
		var h int
		for _, hr := range b.heuristics {
			h += hr.Score(&f, s.Name)
		}
		u += h * b.P.Adaptation / 1000
		if b.P.MistakeRate > 0 {
			u += b.rng.IntN(b.P.MistakeRate+1) - b.P.MistakeRate/2
		}
		scores[s.Name] = u
	}
	b.strategy.scores = scores
	b.strategy.lost = 0
	names := make([]string, 0, len(scores))
	for n := range scores {
		names = append(names, n)
	}
	slices.Sort(names)
	best := ""
	for _, n := range names {
		if best == "" || scores[n] > scores[best] {
			best = n
		}
	}
	cur := b.strategy.name
	emergency := f.ThreatHome > 0 && best == "defensive" && b.P.Adaptation >= 500
	switch {
	case cur == "":
		b.switchStrategy(best, "initial", scores)
	case best != cur && (emergency ||
		(scores[best] >= scores[cur]+150 && b.K.Tick >= b.strategy.since+uint64(b.P.MinCommitment))):
		reason := "utility"
		if emergency {
			reason = "threat"
		}
		b.switchStrategy(best, reason, scores)
	}
	if b.debug {
		b.log.Debug("strategy scores", "tick", b.K.Tick, "scores", fmtScores(scores), "features", fmt.Sprintf("%+v", f))
	}
	s, _ := strategyByName(b.strategy.name)
	b.dir = s.Directive
	// A bot that understands the published scoring keeps growing while a
	// unit is worth more score than the energy it costs (in a timed match
	// scored on units, a clone of a fresh unit is net positive).
	if b.P.Adaptation >= 500 && b.cloneScoreGain() > 0 {
		b.dir.Population = max(b.dir.Population, ScorePopulation)
	}
	// Personality tilts the role mix.
	b.dir.MinerShare = max(200, b.dir.MinerShare-b.Pers.FighterShare-b.Pers.ScoutShare/2)
	if b.Pers.ScoutShare > 0 {
		b.dir.Scouts++
	}
}

func (b *Bot) switchStrategy(name, reason string, scores map[string]int) {
	b.log.Info("strategy", "tick", b.K.Tick, "strategy", name, "previous", b.strategy.name, "reason", reason, "utility", scores[name])
	b.strategy.name, b.strategy.since = name, b.K.Tick
	b.Status.StrategySwitches++
}

func fmtScores(m map[string]int) string {
	var parts []string
	for _, s := range Strategies {
		if v, ok := m[s.Name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", s.Name, v))
		}
	}
	return strings.Join(parts, " ")
}

// features computes the strategic features from knowledge.
func (b *Bot) features() Features {
	k := b.K
	f := Features{Tick: k.Tick, TicksLeft: -1, ExploredPm: k.Explored * 1000 / max(int(k.Width)*int(k.Height), 1)}
	if m := k.Match; m != nil && m.GetTimeLimitTicks() > 0 {
		f.TicksLeft = m.GetTicksRemaining()
	}
	for _, u := range k.OwnUnits() {
		f.OwnUnits++
		if b.Roles[u.ID] == RoleFighter {
			f.Fighters++
			f.OwnStrength += b.strength(u, true)
		}
	}
	f.EnemyStrength, f.EnemiesSeen = b.enemyEstimate()
	f.ThreatHome = b.threatNear(b.home, 12)
	f.WellsKnown = len(k.UsableWells())
	f.LostRecently = b.strategy.lost
	if bk := b.bank(); bk != nil {
		f.BankEnergy, _ = energy(bk)
	}
	return f
}

// ScorePopulation is the population target of score-aware bots.
const ScorePopulation = 26

// cloneScoreGain estimates the score a clone of a fresh unit adds, from
// the published scoring weights: the unit itself, minus the energy the
// clone consumes (which would otherwise count as stored energy).
func (b *Bot) cloneScoreGain() int64 {
	r := b.K.Rules
	if r == nil || r.TimeLimitTicks <= 0 {
		return 0
	}
	w := r.ScoreWeights
	price := int64(61) // a fresh unit's typical price on the shipped presets
	if b.clonerNeed > 0 {
		price = b.clonerNeed - r.CloneOffspring // the offspring energy is kept (stored) by the child
	}
	return w["units"] - w["stored_energy"]*price
}

// ticksLeft returns the match clock's remaining ticks, or -1.
func (b *Bot) ticksLeft() int64 {
	if m := b.K.Match; m != nil && m.GetTimeLimitTicks() > 0 {
		return m.GetTicksRemaining()
	}
	return -1
}
