package state

import (
	"encoding/json"
	"fmt"
)

// Rules is what the bot understands of the published rules
// (WorldInfo.rules_json): public game information.
type Rules struct {
	Attributes []AttrRule
	byName     map[string]*AttrRule

	// Stat bindings (attribute names, empty = constant).
	CapacityAttr, ExtractionAttr, RangeAttr, AttackAttr, HealthAttr, TransferRangeAttr string
	VisionAttr, SensorAttr, StealthAttr, OpticsAttr                                    string

	EnergyPerStep  int64
	TransferRate   int64
	CloneOffspring int64
	DefaultHealth  int64
	DefaultAttack  int64
	DefaultRange   int64
	HasLogistics   bool
	HasAutomation  bool
	HasVisibility  bool
	VictoryElim    bool
	TimeLimitTicks int64
	ScoreWeights   map[string]int64
}

// AttrRule is one upgradable attribute.
type AttrRule struct {
	Name          string  `json:"name"`
	Default       int64   `json:"default"`
	Min           int64   `json:"min"`
	Max           int64   `json:"max"`
	CostTable     []int64 `json:"cost_table"`
	DurationTable []int64 `json:"duration_table"`
}

type stat struct {
	Property string `json:"property"`
	Default  int64  `json:"default"`
}

// ParseRules decodes rules_json leniently: unknown plugins and fields are
// ignored, missing ones leave defaults.
func ParseRules(raw string) (*Rules, error) {
	r := &Rules{byName: map[string]*AttrRule{}, DefaultHealth: 100, DefaultAttack: 10, DefaultRange: 1, TransferRate: 5}
	if raw == "" {
		return r, nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, fmt.Errorf("rules_json: %w", err)
	}
	var attrs struct {
		Attributes []AttrRule `json:"attributes"`
	}
	if b := all["attributes"]; b != nil {
		_ = json.Unmarshal(b, &attrs)
	}
	r.Attributes = attrs.Attributes
	for i := range r.Attributes {
		r.byName[r.Attributes[i].Name] = &r.Attributes[i]
	}
	var energy struct {
		CapacityAttribute   string `json:"capacity_attribute"`
		ExtractionAttribute string `json:"extraction_attribute"`
		TransferRate        int64  `json:"transfer_rate"`
	}
	_ = json.Unmarshal(all["energy"], &energy)
	r.CapacityAttr, r.ExtractionAttr = energy.CapacityAttribute, energy.ExtractionAttribute
	if energy.TransferRate > 0 {
		r.TransferRate = energy.TransferRate
	}
	var movement struct {
		EnergyPerStep int64 `json:"energy_per_step"`
	}
	_ = json.Unmarshal(all["movement"], &movement)
	r.EnergyPerStep = movement.EnergyPerStep
	var combat struct {
		Config struct {
			Attack    stat `json:"attack"`
			Range     stat `json:"range"`
			MaxHealth stat `json:"max_health"`
		} `json:"config"`
	}
	if b := all["combat"]; b != nil {
		_ = json.Unmarshal(b, &combat)
		c := combat.Config
		r.AttackAttr, r.RangeAttr, r.HealthAttr = c.Attack.Property, c.Range.Property, c.MaxHealth.Property
		r.DefaultAttack, r.DefaultRange, r.DefaultHealth = c.Attack.Default, max(c.Range.Default, 1), max(c.MaxHealth.Default, 1)
	}
	var cloning struct {
		Config struct {
			OffspringEnergy int64 `json:"offspring_energy"`
		} `json:"config"`
	}
	_ = json.Unmarshal(all["cloning"], &cloning)
	r.CloneOffspring = cloning.Config.OffspringEnergy
	var logistics struct {
		Config struct {
			Range stat `json:"range"`
		} `json:"config"`
	}
	if b := all["logistics"]; b != nil {
		r.HasLogistics = true
		_ = json.Unmarshal(b, &logistics)
		r.TransferRangeAttr = logistics.Config.Range.Property
	}
	_, r.HasAutomation = all["automation"]
	var vis struct {
		Config struct {
			Enabled bool `json:"enabled"`
			Vision  stat `json:"vision"`
			Sensor  stat `json:"sensor"`
			Sig     stat `json:"signature"`
			Ident   stat `json:"identification"`
		} `json:"config"`
	}
	if b := all["visibility"]; b != nil {
		_ = json.Unmarshal(b, &vis)
		c := vis.Config
		r.HasVisibility = c.Enabled
		r.VisionAttr, r.SensorAttr, r.StealthAttr, r.OpticsAttr = c.Vision.Property, c.Sensor.Property, c.Sig.Property, c.Ident.Property
	}
	var match struct {
		Config struct {
			Victory struct {
				Elimination    bool  `json:"elimination"`
				TimeLimitTicks int64 `json:"time_limit_ticks"`
			} `json:"victory"`
			Scoring struct {
				Weights map[string]int64 `json:"weights"`
			} `json:"scoring"`
		} `json:"config"`
	}
	if b := all["match"]; b != nil {
		_ = json.Unmarshal(b, &match)
		r.VictoryElim, r.TimeLimitTicks, r.ScoreWeights = match.Config.Victory.Elimination, match.Config.Victory.TimeLimitTicks, match.Config.Scoring.Weights
	}
	return r, nil
}

// Attr returns an attribute rule.
func (r *Rules) Attr(name string) (*AttrRule, bool) {
	if name == "" {
		return nil, false
	}
	a, ok := r.byName[name]
	return a, ok
}

// UpgradeCost returns the energy and ticks to raise an attribute from one
// level to another, from the published tables; ok=false if impossible.
func (r *Rules) UpgradeCost(name string, from, to int64) (cost, ticks int64, ok bool) {
	a, found := r.Attr(name)
	if !found || to <= from || to > a.Max || from < a.Min || int(to-a.Min) >= len(a.CostTable) {
		return 0, 0, false
	}
	cost = a.CostTable[to-a.Min] - a.CostTable[from-a.Min]
	ticks = max(1, a.DurationTable[to-a.Min]-a.DurationTable[from-a.Min])
	return cost, ticks, true
}
