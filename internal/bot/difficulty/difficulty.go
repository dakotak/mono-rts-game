// Package difficulty is the computer player's centralized, data-driven
// difficulty and personality configuration. Every behavioral difference
// between levels comes from these parameters; no decision code checks the
// level number directly.
//
// Difficulty changes the quality of decisions only. Every level uses the
// same public API, the same server command budget and the same
// observations; none receives energy, units, speed or information.
package difficulty

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Params control decision quality. Permille values are 0..1000.
type Params struct {
	Level int    `json:"level"`
	Name  string `json:"name"`

	// Decision frequency (ticks between evaluations of each layer).
	StrategicInterval   int `json:"strategic_interval"`
	OperationalInterval int `json:"operational_interval"`
	TacticalInterval    int `json:"tactical_interval"`
	// ReactionDelay: ticks a newly seen threat must persist before the
	// bot reacts to it.
	ReactionDelay int `json:"reaction_delay"`

	// PlanningHorizon (ticks) used to value investments: an upgrade or clone
	// is only worth it if it pays back within the horizon.
	PlanningHorizon int `json:"planning_horizon"`
	// Candidates evaluated per choice (wells, targets, sectors, plans).
	Candidates int `json:"candidates"`
	// ComputeBudget: evaluation operations per decision cycle (deterministic
	// unit of work, not time).
	ComputeBudget int `json:"compute_budget"`

	// EconomicEfficiency: quality of miner allocation and spending order.
	EconomicEfficiency int `json:"economic_efficiency"`
	// ExplorationAggression: share of units devoted to scouting and how
	// far scouting reaches.
	ExplorationAggression int `json:"exploration_aggression"`
	// Coordination: how much units focus fire and move as groups.
	Coordination int `json:"coordination"`
	// ThreatModel: 0 counts units, 1 weighs health and attack, 2 also
	// armor, range, reinforcements and stale sightings.
	ThreatModel int `json:"threat_model"`
	// Specialization: how closely upgrades follow role plans (vs random).
	Specialization int `json:"specialization"`
	// Adaptation: how strongly strategy selection responds to observed
	// conditions (vs the personality default).
	Adaptation int `json:"adaptation"`
	// RiskTolerance: lower attacks only with clear superiority.
	RiskTolerance int `json:"risk_tolerance"`
	// MistakeRate: chance of picking a random candidate instead of the
	// best one (controlled imperfection).
	MistakeRate int `json:"mistake_rate"`
	// MinCommitment: ticks a strategy is kept before switching.
	MinCommitment int `json:"min_commitment"`

	// Capability toggles (better play unlocked by skill, not given power).
	UseLogistics bool `json:"use_logistics"` // supply links from the bank
	UseSalvage   bool `json:"use_salvage"`   // auto-salvage wreckage
	UseGuards    bool `json:"use_guards"`    // guard automation at home
	Retreat      bool `json:"retreat"`       // retreat from losing fights
	Scout        bool `json:"scout"`         // dedicated scouts
}

//go:embed levels.json
var levelsJSON []byte

var levels []Params

func init() {
	if err := json.Unmarshal(levelsJSON, &levels); err != nil {
		panic(fmt.Sprintf("difficulty: levels.json: %v", err))
	}
	for i, p := range levels {
		if p.Level != i+1 {
			panic("difficulty: levels.json must list levels 1..10 in order")
		}
	}
}

// Level returns the parameters for a difficulty level (1..10).
func Level(n int) (Params, error) {
	if n < 1 || n > len(levels) {
		return Params{}, fmt.Errorf("difficulty must be in 1..%d, got %d", len(levels), n)
	}
	return levels[n-1], nil
}

// Levels returns all levels.
func Levels() []Params { return append([]Params(nil), levels...) }

// Personality biases strategy selection and role mix, independent of
// difficulty: a skilled defensive bot defends well; an unskilled aggressive
// bot attacks simply.
type Personality struct {
	Name string
	// StrategyBias adds utility (permille) to strategies by name.
	StrategyBias map[string]int
	// ScoutShare and FighterShare tilt the role mix (permille offsets).
	ScoutShare, FighterShare int
}

// Personalities are the built-in personalities.
var Personalities = map[string]Personality{
	"balanced":    {Name: "balanced", StrategyBias: map[string]int{"balanced": 150}},
	"aggressive":  {Name: "aggressive", StrategyBias: map[string]int{"aggressive": 300, "resource_denial": 150}, FighterShare: 120},
	"defensive":   {Name: "defensive", StrategyBias: map[string]int{"defensive": 300, "tech": 100}, FighterShare: 60},
	"economic":    {Name: "economic", StrategyBias: map[string]int{"economic_expansion": 300, "tech": 100}, FighterShare: -80},
	"exploratory": {Name: "exploratory", StrategyBias: map[string]int{"economic_expansion": 150, "balanced": 100}, ScoutShare: 80},
}

// PersonalityByName returns a personality.
func PersonalityByName(name string) (Personality, error) {
	p, ok := Personalities[strings.ToLower(name)]
	if !ok {
		return Personality{}, fmt.Errorf("unknown personality %q (balanced, aggressive, defensive, economic, exploratory)", name)
	}
	return p, nil
}
