// Package game assembles a concrete game from configuration: it selects the
// compiled-in plugins, builds the ruleset and engine, assigns player
// identities and places the initial entities. It is the only place that
// knows which plugins exist.
package game

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"time"

	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/automation"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/logistics"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/metadata"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/terrain"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/upgrades"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/visibility"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/worldgen"
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
	// IDKey is the hex-encoded secret from which entity and player IDs are
	// derived. Leave it empty to generate a random key at startup (IDs then
	// differ between runs). Set it explicitly only when runs must be
	// reproducible, and keep it private: it makes IDs predictable.
	IDKey string `json:"id_key"`
	// UnitCommandBudget is how many unit-commands (one per unit a command
	// affects) each player may execute per tick. 0 means unlimited.
	UnitCommandBudget int `json:"unit_command_budget"`
	// RandomSeed draws a fresh random seed for every match instead of using
	// Seed. The chosen seed is recorded (Game.Seed, match info, replays).
	RandomSeed bool `json:"random_seed"`
}

// PlayerConfig declares a player slot. Whoever presents JoinToken plays as
// this player. Slot is a configuration-local handle used to reference the
// player within this file (e.g. unit owners); it is never exposed. The
// player's public ID is derived from the ID key.
type PlayerConfig struct {
	Slot      int    `json:"slot"`
	Name      string `json:"name"`
	JoinToken string `json:"join_token"`
}

// UnitBlock places a W x H rectangle of units (W and H default to 1). Owner
// is a player slot, or 0 for unowned.
type UnitBlock struct {
	Owner int   `json:"owner"`
	X     int32 `json:"x"`
	Y     int32 `json:"y"`
	W     int32 `json:"w"`
	H     int32 `json:"h"`
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
	if c.World.UnitCommandBudget < 0 {
		return fmt.Errorf("unit_command_budget must be >= 0, got %d", c.World.UnitCommandBudget)
	}
	if c.World.TickRateHz == 0 || c.World.TickRateHz > 1000 {
		return fmt.Errorf("tick_rate_hz must be in 1..1000, got %d", c.World.TickRateHz)
	}
	if c.World.IDKey != "" {
		key, err := hex.DecodeString(c.World.IDKey)
		if err != nil {
			return fmt.Errorf("id_key must be hex: %w", err)
		}
		if len(key) < sim.MinIDKeyLen {
			return fmt.Errorf("id_key must be at least %d bytes (%d hex characters)", sim.MinIDKeyLen, 2*sim.MinIDKeyLen)
		}
	}
	slots := map[int]bool{}
	tokens := map[string]bool{}
	for _, p := range c.Players {
		if p.Slot <= 0 || slots[p.Slot] {
			return fmt.Errorf("player slot %d is not positive or is duplicated", p.Slot)
		}
		if p.JoinToken == "" || tokens[p.JoinToken] {
			return fmt.Errorf("player slot %d: join_token is empty or duplicated", p.Slot)
		}
		slots[p.Slot], tokens[p.JoinToken] = true, true
	}
	if c.AdminToken != "" && tokens[c.AdminToken] {
		return errors.New("admin_token must differ from every join_token")
	}
	for i, u := range c.Units {
		if u.Owner != 0 && !slots[u.Owner] {
			return fmt.Errorf("units[%d]: unknown owner slot %d", i, u.Owner)
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

// Plugins returns the compiled-in plugins, wired together. Adding a
// mechanic means adding it here. Plugins that build on attributes share one
// registry instance (explicit composition rather than global lookup).
func Plugins() []sim.Plugin {
	attrs := attributes.New()
	vis := visibility.New(attrs)
	fighting := combat.New(attrs)
	energyP := energy.NewWithAttributes(attrs)
	salvageP := salvage.New(attrs, fighting)
	// Fog of war gates every command that refers to a foreign entity.
	// Detected is always true while fog is disabled.
	fighting.CanTarget = vis.Detected
	energyP.CanSee = vis.Detected
	salvageP.CanSee = vis.Detected
	auto := automation.New(fighting)
	auto.Detected = vis.Detected
	terrainP, wellsP := terrain.New(), wells.New()
	return []sim.Plugin{
		match.New(attrs, fighting),
		worldgen.New(terrainP, wellsP),
		terrainP,
		wellsP,
		attrs,
		energyP,
		upgrades.New(attrs),
		cloning.New(attrs),
		movement.NewWithAttributes(attrs),
		metadata.New(),
		fighting,
		salvageP,
		auto,
		logistics.New(attrs),
		vis,
	}
}

// PlayerAware is implemented by plugins that need the match's players (in
// configuration order) before the world initializes: world generation
// spawns them, the match plugin tracks readiness and elimination.
type PlayerAware interface {
	SetPlayers(ids []sim.PlayerID)
}

// Player is a resolved player slot.
type Player struct {
	ID        sim.PlayerID
	Slot      int
	Name      string
	JoinToken string
}

// EngineVersion identifies the simulation code for replay compatibility.
// Bump it whenever a change can alter simulation results.
const EngineVersion = "mono-m7"

// Game is an assembled, ready-to-run game.
type Game struct {
	Engine  *sim.Engine
	Players []Player // in configuration order
	// Seed and IDKey are the values actually used (resolved when the config
	// asked for random ones).
	Seed  uint64
	IDKey []byte
	// Resolved is the configuration with the seed and ID key filled in and
	// random_seed off: building a game from it reproduces this one.
	Resolved *Config
	plugins  []sim.Plugin
}

// RulesetHash fingerprints the ruleset: plugins, schedule, command types
// and every plugin's published rules (effective configuration).
func RulesetHash(rs *sim.Ruleset) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%v|%v|%v|", rs.Plugins(), rs.Schedule(), rs.CommandTypes(true))
	b, _ := json.Marshal(rs.Rules()) // map keys are sorted
	h.Write(b)
	return fmt.Sprintf("%016x", h.Sum64())
}

// Plugin returns the game's plugin instance of type T.
func Plugin[T sim.Plugin](g *Game) (T, bool) { return Find[T](g.plugins) }

// Find returns the plugin of type T in a plugin set (e.g. the shared
// *match.Plugin in Plugins(), to register conditions with it).
func Find[T sim.Plugin](plugins []sim.Plugin) (T, bool) {
	for _, p := range plugins {
		if t, ok := p.(T); ok {
			return t, true
		}
	}
	var zero T
	return zero, false
}

// PluginsOf returns the plugin instances a game was built with.
func PluginsOf(g *Game) []sim.Plugin { return g.plugins }

// PlayerBySlot returns the player declared with the given slot.
func (g *Game) PlayerBySlot(slot int) (Player, bool) {
	for _, p := range g.Players {
		if p.Slot == slot {
			return p, true
		}
	}
	return Player{}, false
}

// New builds the ruleset and engine from the compiled-in plugins
// (Plugins), derives player IDs and places the initial units.
func New(c *Config) (*Game, error) { return NewWithPlugins(c, Plugins()) }

// NewWithPlugins is New with an explicit plugin set, e.g. Plugins() plus a
// plugin under development (tests and tools; the server uses Plugins()).
// The plugins must be freshly constructed: a plugin instance belongs to one
// game.
func NewWithPlugins(c *Config, plugins []sim.Plugin) (*Game, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	key, err := idKey(c.World.IDKey)
	if err != nil {
		return nil, err
	}
	seed := c.World.Seed
	if c.World.RandomSeed {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("random seed: %w", err)
		}
		seed = binary.LittleEndian.Uint64(b[:])
	}
	resolved := *c
	resolved.World.Seed, resolved.World.RandomSeed, resolved.World.IDKey = seed, false, hex.EncodeToString(key)
	rules, err := sim.NewRuleset(plugins, c.Plugins)
	if err != nil {
		return nil, err
	}
	e, err := sim.NewEngine(sim.Config{Width: c.World.Width, Height: c.World.Height, Seed: seed, IDKey: key, UnitCommandBudget: c.World.UnitCommandBudget}, rules)
	if err != nil {
		return nil, err
	}
	g := &Game{Engine: e, Players: derivePlayers(key, c.Players), Seed: seed, IDKey: key, Resolved: &resolved, plugins: plugins}
	ids := make([]sim.PlayerID, len(g.Players))
	for i, p := range g.Players {
		ids[i] = p.ID
	}
	for _, p := range plugins {
		if pa, ok := p.(PlayerAware); ok {
			pa.SetPlayers(ids)
		}
	}

	w := e.World()
	for i, b := range c.Units {
		var owner sim.PlayerID
		if b.Owner != 0 {
			p, _ := g.PlayerBySlot(b.Owner)
			owner = p.ID
		}
		bw, bh := max(b.W, 1), max(b.H, 1)
		for dy := range bh {
			for dx := range bw {
				if _, err := w.CreateEntity(sim.KindUnit, owner, sim.Pos{X: b.X + dx, Y: b.Y + dy}); err != nil {
					return nil, fmt.Errorf("units[%d]: %w", i, err)
				}
			}
		}
	}
	// Plugins place their own initial entities (e.g. wells) after units.
	if err := e.Init(); err != nil {
		return nil, err
	}
	return g, nil
}

func idKey(hexKey string) ([]byte, error) {
	if hexKey != "" {
		return hex.DecodeString(hexKey)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate id key: %w", err)
	}
	return key, nil
}

// derivePlayers assigns each slot an unpredictable, unique, non-zero ID.
func derivePlayers(key []byte, cfg []PlayerConfig) []Player {
	used := map[sim.PlayerID]bool{0: true}
	var n uint64
	out := make([]Player, 0, len(cfg))
	for _, pc := range cfg {
		var id sim.PlayerID
		for used[id] {
			n++
			id = sim.PlayerID(sim.DeriveID(key, "player", n))
		}
		used[id] = true
		out = append(out, Player{ID: id, Slot: pc.Slot, Name: pc.Name, JoinToken: pc.JoinToken})
	}
	return out
}
