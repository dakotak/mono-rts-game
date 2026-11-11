// Package bot assembles a computer player from configuration: difficulty,
// personality, seed and diagnostics. The decision logic lives in
// internal/bot/ai; the network client in internal/bot/client; the world
// model in internal/bot/state. None of them import simulation internals:
// the computer player uses only the public GameService API.
package bot

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// Config is a computer player's configuration (JSON file and/or flags).
type Config struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
	PlayerName  string `json:"player_name"` // label for logs; the server's name comes from its config
	Difficulty  int    `json:"difficulty"`
	Personality string `json:"personality"`
	Seed        uint64 `json:"seed"`
	// Deterministic: command IDs derive from the seed only, so identical
	// inputs produce byte-identical output.
	Deterministic bool   `json:"deterministic"`
	LogLevel      string `json:"log_level"`
	Debug         bool   `json:"debug"`
	StatusAddr    string `json:"status_addr"`
	// DifficultyOverrides adjusts individual parameters of the level
	// (any field of difficulty.Params).
	DifficultyOverrides json.RawMessage `json:"difficulty_overrides,omitempty"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{Server: "127.0.0.1:50051", Difficulty: 5, Personality: "balanced", LogLevel: "info"}
}

// LoadFile merges a JSON configuration file over c (unknown fields are
// errors).
func (c *Config) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	if c.Token == "" {
		return fmt.Errorf("a join token is required (--token)")
	}
	if c.Difficulty < 1 || c.Difficulty > 10 {
		return fmt.Errorf("difficulty must be 1..10, got %d", c.Difficulty)
	}
	if _, err := difficulty.PersonalityByName(c.Personality); err != nil {
		return err
	}
	if _, err := c.params(); err != nil {
		return err
	}
	return nil
}

func (c *Config) params() (difficulty.Params, error) {
	p, err := difficulty.Level(c.Difficulty)
	if err != nil {
		return p, err
	}
	if len(c.DifficultyOverrides) > 0 {
		dec := json.NewDecoder(bytes.NewReader(c.DifficultyOverrides))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return p, fmt.Errorf("difficulty_overrides: %w", err)
		}
		if p.StrategicInterval < 1 || p.OperationalInterval < 1 || p.TacticalInterval < 1 {
			return p, fmt.Errorf("difficulty_overrides: intervals must be >= 1")
		}
	}
	return p, nil
}

// Logger builds the structured logger for a level name.
func (c *Config) Logger() *slog.Logger {
	lvl := slog.LevelInfo
	switch c.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	if c.Debug {
		lvl = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	lg := slog.New(h)
	if c.PlayerName != "" {
		lg = lg.With("bot", c.PlayerName)
	}
	return lg
}

// Extensions add custom strategy heuristics and operational objectives to
// the built-in ones (see docs/computer-player/extending.md).
type Extensions struct {
	Heuristics []ai.Heuristic
	Objectives []ai.Objective
}

// NewBot builds the decision system for known world rules.
func (c *Config) NewBot(k *state.Knowledge, lg *slog.Logger, ext ...Extensions) *ai.Bot {
	var x Extensions
	for _, e := range ext {
		x.Heuristics = append(x.Heuristics, e.Heuristics...)
		x.Objectives = append(x.Objectives, e.Objectives...)
	}
	p, _ := c.params()
	pers, _ := difficulty.PersonalityByName(c.Personality)
	seed := c.Seed
	nonce := fmt.Sprintf("bot%x", seed&0xffffffff)
	if !c.Deterministic {
		var b [8]byte
		_, _ = rand.Read(b[:])
		nonce = fmt.Sprintf("bot%x", binary.LittleEndian.Uint32(b[:]))
	}
	return ai.New(k, ai.Config{Difficulty: p, Personality: pers, Seed: seed, Nonce: nonce, Logger: lg, Debug: c.Debug,
		Heuristics: x.Heuristics, Objectives: x.Objectives})
}
