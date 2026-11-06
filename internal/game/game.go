// Package game assembles a concrete game from configuration: it selects the
// compiled-in plugins, builds the ruleset and engine, and places the initial
// entities. It is the only place that knows which plugins exist.
package game

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Config is the server configuration file.
type Config struct {
	World      WorldConfig                `json:"world"`
	Players    []PlayerConfig             `json:"players"`
	AdminToken string                     `json:"admin_token"`
	Units      []UnitBlock                `json:"units"`
	Plugins    map[string]json.RawMessage `json:"plugins"`
}

// WorldConfig sizes the world and the clock.
type WorldConfig struct {
	Width      int32  `json:"width"`
	Height     int32  `json:"height"`
	Seed       uint64 `json:"seed"`
	TickRateHz uint32 `json:"tick_rate_hz"`
}

// PlayerConfig declares a player slot. Whoever presents JoinToken plays as
// this player.
type PlayerConfig struct {
	ID        sim.PlayerID `json:"id"`
	Name      string       `json:"name"`
	JoinToken string       `json:"join_token"`
}

// UnitBlock places a W x H rectangle of units (W and H default to 1).
type UnitBlock struct {
	Owner sim.PlayerID `json:"owner"`
	X     int32        `json:"x"`
	Y     int32        `json:"y"`
	W     int32        `json:"w"`
	H     int32        `json:"h"`
}

// TickInterval returns the wall-clock duration of one tick.
func (c *Config) TickInterval() time.Duration {
	return time.Second / time.Duration(c.World.TickRateHz)
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	if c.World.Width <= 0 || c.World.Height <= 0 {
		return fmt.Errorf("world size must be positive, got %dx%d", c.World.Width, c.World.Height)
	}
	if c.World.TickRateHz == 0 || c.World.TickRateHz > 1000 {
		return fmt.Errorf("tick_rate_hz must be in 1..1000, got %d", c.World.TickRateHz)
	}
	ids := map[sim.PlayerID]bool{}
	tokens := map[string]bool{}
	for _, p := range c.Players {
		if p.ID == 0 || ids[p.ID] {
			return fmt.Errorf("player id %d is zero or duplicated", p.ID)
		}
		if p.JoinToken == "" || tokens[p.JoinToken] {
			return fmt.Errorf("player %d: join_token is empty or duplicated", p.ID)
		}
		ids[p.ID], tokens[p.JoinToken] = true, true
	}
	if c.AdminToken != "" && tokens[c.AdminToken] {
		return errors.New("admin_token must differ from every join_token")
	}
	for i, u := range c.Units {
		if u.Owner != 0 && !ids[u.Owner] {
			return fmt.Errorf("units[%d]: unknown owner %d", i, u.Owner)
		}
	}
	return nil
}

// LoadConfig reads and validates a configuration file. Unknown fields are
// rejected so typos do not silently fall back to defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Plugins returns the compiled-in plugins. Adding a mechanic means adding
// it here.
func Plugins() []sim.Plugin {
	return []sim.Plugin{movement.New()}
}

// NewEngine builds the ruleset and engine and places the initial units.
func NewEngine(c *Config) (*sim.Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rules, err := sim.NewRuleset(Plugins(), c.Plugins)
	if err != nil {
		return nil, err
	}
	e, err := sim.NewEngine(sim.Config{Width: c.World.Width, Height: c.World.Height, Seed: c.World.Seed}, rules)
	if err != nil {
		return nil, err
	}
	w := e.World()
	for i, b := range c.Units {
		bw, bh := max(b.W, 1), max(b.H, 1)
		for dy := range bh {
			for dx := range bw {
				if _, err := w.CreateEntity(sim.KindUnit, b.Owner, sim.Pos{X: b.X + dx, Y: b.Y + dy}); err != nil {
					return nil, fmt.Errorf("units[%d]: %w", i, err)
				}
			}
		}
	}
	return e, nil
}
