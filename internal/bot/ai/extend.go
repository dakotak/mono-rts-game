package ai

import "github.com/dakotaknutson/unit-strategy-game/internal/bot/state"

// Objective is an operational-layer extension: a goal the bot pursues
// alongside the built-in economy, defense, scouting and attack objectives.
// Plan runs every operational cycle after the built-in planning; it may
// read b.K (knowledge), b.Roles and b.Directive(), and issue orders with
// b.Issue. It reports whether it is active and a short detail string for
// diagnostics.
type Objective interface {
	Name() string
	Plan(b *Bot) (active bool, detail string)
}

func builtinObjectives() []Objective { return nil }

// Issue queues an order (for extensions).
func (b *Bot) Issue(o Order) { b.issue(o) }

// Directive returns the current strategic directive.
func (b *Bot) Directive() Directive { return b.dir }

// StrategyName returns the current strategy.
func (b *Bot) StrategyName() string { return b.strategy.name }

// Home returns the bot's base position.
func (b *Bot) Home() (x, y int32) { return b.home.X, b.home.Y }

// UnitsByRole returns own units with a role, in ID order (for extensions).
func (b *Bot) UnitsByRole(r Role) []*state.Entity { return b.unitsByRole(r) }

// SetRole assigns a role (tagged on the server with metadata role=<r>).
func (b *Bot) SetRole(u *state.Entity, r Role) { b.setRole(u, r) }

// Tick returns the current tick.
func (b *Bot) Tick() uint64 { return b.K.Tick }
