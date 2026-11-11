package ai

import (
	"strconv"
	"time"
)

// Status is the bot's diagnostic state (decisions, not server secrets).
type Status struct {
	Tick             uint64         `json:"tick"`
	Level            int            `json:"difficulty"`
	Personality      string         `json:"personality"`
	Strategy         string         `json:"strategy"`
	Outcome          string         `json:"outcome,omitempty"` // won, lost or draw once the match ended
	StrategySince    uint64         `json:"strategy_since"`
	StrategyScores   map[string]int `json:"strategy_scores,omitempty"`
	StrategySwitches int            `json:"strategy_switches"`
	Objectives       []string       `json:"objectives"`
	Units            int            `json:"units"`
	Roles            map[Role]int   `json:"roles"`
	RoleChanges      int            `json:"role_changes"`
	BankEnergy       int64          `json:"bank_energy"`
	Threats          int            `json:"threats"`
	OwnStrength      int64          `json:"own_strength"`
	EnemyEstimate    int64          `json:"enemy_estimate"`
	EnemiesKnown     int            `json:"enemies_known"`
	InferredEnemyHit int64          `json:"inferred_enemy_hit"`
	Campaign         string         `json:"campaign"`
	LastEngagement   string         `json:"last_engagement"`
	ExploredPm       int            `json:"explored_permille"`
	Commands         CommandStats   `json:"commands"`
	PendingCommands  int            `json:"pending_commands"`
	BudgetExhausted  int            `json:"compute_budget_exhausted"`

	Decisions     int           `json:"decisions"`
	LastDecision  time.Duration `json:"last_decision_ns"`
	MaxDecision   time.Duration `json:"max_decision_ns"`
	TotalDecision time.Duration `json:"total_decision_ns"`
}

func (s *Status) update(b *Bot, sent int, took time.Duration) {
	s.Tick, s.Level, s.Personality = b.K.Tick, b.P.Level, b.Pers.Name
	s.Strategy, s.StrategySince, s.StrategyScores = b.strategy.name, b.strategy.since, b.strategy.scores
	s.Commands, s.PendingCommands = b.Cmd.Stats, b.Cmd.Pending()
	s.Decisions++
	s.LastDecision, s.TotalDecision = took, s.TotalDecision+took
	s.MaxDecision = max(s.MaxDecision, took)
	if b.K.Tick%uint64(max(b.P.OperationalInterval, 1)) != 0 {
		return
	}
	s.Roles = map[Role]int{}
	s.Units = 0
	s.OwnStrength = 0
	for _, u := range b.K.OwnUnits() {
		s.Units++
		s.Roles[b.Roles[u.ID]]++
		if b.Roles[u.ID] == RoleFighter {
			s.OwnStrength += b.strength(u, true)
		}
	}
	s.EnemyEstimate, s.EnemiesKnown = b.enemyEstimate()
	if bk := b.bank(); bk != nil {
		s.BankEnergy, _ = energy(bk)
	}
	s.Campaign = ""
	if b.campaign != nil {
		s.Campaign = "target " + b.campaign.target.String()
		if b.campaign.probe {
			s.Campaign = "search " + b.campaign.target.String()
		}
	} else if b.muster {
		s.Campaign = "mustering"
	}
	s.ExploredPm = b.K.Explored * 1000 / max(int(b.K.Width)*int(b.K.Height), 1)
	if b.K.Tick%uint64(max(b.P.StrategicInterval, 1)) == 0 {
		b.log.Info("status", "tick", s.Tick, "strategy", s.Strategy, "units", s.Units, "roles", rolesString(s.Roles),
			"bank_energy", s.BankEnergy, "threats", s.Threats, "own_strength", s.OwnStrength, "enemy_estimate", s.EnemyEstimate,
			"campaign", s.Campaign, "commands", s.Commands.Submitted, "failed", s.Commands.Failed,
			"avg_decision_us", s.TotalDecision.Microseconds()/int64(max(s.Decisions, 1)))
	}
}

func rolesString(m map[Role]int) string {
	out := ""
	for _, r := range []Role{RoleBank, RoleMiner, RoleFighter, RoleScout, RoleGuard, RoleSalvager, RoleNone} {
		if n := m[r]; n > 0 {
			if out != "" {
				out += ","
			}
			name := string(r)
			if name == "" {
				name = "unassigned"
			}
			out += name + "=" + strconv.Itoa(n)
		}
	}
	return out
}
