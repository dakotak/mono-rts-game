// Package sim is the deterministic simulation kernel.
//
// The kernel owns world lifecycle, entity identity and storage, tick
// scheduling, command ingestion, plugin registration, phase ordering, event
// generation, snapshots and seeded randomness. It contains no game-specific
// mechanics: movement, combat, mining and so on are provided by plugins.
//
// Determinism rules for all code running inside a tick (kernel and plugins):
//   - never iterate a Go map to produce output or mutate state; iterate
//     sorted IDs instead (World and Component helpers return sorted slices),
//   - never read the wall clock, use Tick,
//   - use World.Rand for randomness,
//   - use integer arithmetic for authoritative calculations,
//   - never mutate world state from another goroutine.
//
// The kernel has no dependency on protobuf or networking.
package sim

import "fmt"

// EntityID identifies an entity. IDs are unpredictable 64-bit values derived
// from the world's secret ID key (see DeriveID), so they reveal neither how
// many entities exist nor their creation order. Zero is never used.
type EntityID uint64

// PlayerID identifies a player. Zero means "no owner". Like entity IDs,
// player IDs are opaque and unpredictable; the kernel does not allocate them.
type PlayerID uint32

// Tick is a simulation step number. Tick 0 is the initial state; the first
// executed step is tick 1.
type Tick uint64

// Kind is the conceptual category of an entity.
type Kind uint8

const (
	KindUnit Kind = iota + 1
	KindResource
	KindTerrain
)

func (k Kind) String() string {
	switch k {
	case KindUnit:
		return "unit"
	case KindResource:
		return "resource"
	case KindTerrain:
		return "terrain"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Pos is a tile coordinate. (0,0) is the top-left tile.
type Pos struct {
	X, Y int32
}

func (p Pos) String() string { return fmt.Sprintf("(%d,%d)", p.X, p.Y) }

// Principal is the identity a command is issued on behalf of.
type Principal struct {
	Player PlayerID
	Admin  bool
}

func (p Principal) String() string {
	if p.Admin {
		return "admin"
	}
	return fmt.Sprintf("player %d", p.Player)
}

// Visibility controls who may observe an attribute.
type Visibility uint8

const (
	// VisibleAdmin attributes are only shown through the admin API.
	VisibleAdmin Visibility = iota
	// VisibleOwner attributes are shown to the owning player (and admins).
	VisibleOwner
	// VisiblePublic attributes are shown to every player.
	VisiblePublic
)

// ValueKind discriminates Value.
type ValueKind uint8

const (
	ValueInt ValueKind = iota + 1
	ValueBool
	ValueString
	ValuePos
)

// Value is a small tagged union used for plugin-defined attributes and event
// fields. It is comparable and has no pointers, so it is safe to copy and
// hash.
type Value struct {
	Kind ValueKind
	Int  int64
	Bool bool
	Str  string
	Pos  Pos
}

func IntValue(v int64) Value     { return Value{Kind: ValueInt, Int: v} }
func BoolValue(v bool) Value     { return Value{Kind: ValueBool, Bool: v} }
func StringValue(v string) Value { return Value{Kind: ValueString, Str: v} }
func PosValue(v Pos) Value       { return Value{Kind: ValuePos, Pos: v} }

// Field is a named Value.
type Field struct {
	Key   string
	Value Value
}

// Attr is an observable attribute of an entity, tagged with its visibility.
type Attr struct {
	Key        string
	Value      Value
	Visibility Visibility
}

// Audience selects who receives an event.
type Audience struct {
	All    bool
	Player PlayerID
}

// ToAll addresses an event to every observer.
func ToAll() Audience { return Audience{All: true} }

// ToPlayer addresses an event to a single player (and admins).
func ToPlayer(p PlayerID) Audience { return Audience{Player: p} }

// Event is something that happened during a tick.
type Event struct {
	Type     string // "<plugin>.<name>", e.g. "movement.arrived"
	Entity   EntityID
	Audience Audience
	Fields   []Field
}
