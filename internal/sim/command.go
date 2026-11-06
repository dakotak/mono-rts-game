package sim

import (
	"errors"
	"fmt"
)

// Command is a request to change the world, executed inside a tick.
type Command struct {
	// ID is chosen by the issuer and echoed in the result.
	ID     string
	Issuer Principal
	// Type selects the handler. For network commands it is the fully
	// qualified protobuf message name of the payload.
	Type    string
	Payload any

	// Assigned by the engine on submission.
	Tick Tick
	Seq  uint64
}

// commandLess is the deterministic execution order within a tick: admin
// commands first, then by player ID, then by the issuer's submission
// sequence. Wall-clock arrival order between different issuers never
// matters.
func commandLess(a, b *Command) bool {
	if a.Issuer.Admin != b.Issuer.Admin {
		return a.Issuer.Admin
	}
	if a.Issuer.Player != b.Issuer.Player {
		return a.Issuer.Player < b.Issuer.Player
	}
	return a.Seq < b.Seq
}

// CommandResult is the outcome of executing a command.
type CommandResult struct {
	CommandID string
	Issuer    Principal
	Tick      Tick
	OK        bool
	Code      string
	Message   string
	// Units holds per-unit outcomes for commands that report them (batch
	// commands). Partial is set when the command succeeded overall but some
	// units failed.
	Units   []UnitResult
	Partial bool
}

// UnitResult is the outcome of a command for one unit.
type UnitResult struct {
	Unit    EntityID
	OK      bool
	Code    string
	Message string
}

// Standard failure codes. Plugins may define more.
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeNotFound           = "not_found"
	CodePermissionDenied   = "permission_denied"
	CodeFailedPrecondition = "failed_precondition"
	CodeInternal           = "internal"
	// CodeResourceExhausted reports an exhausted per-tick command budget.
	CodeResourceExhausted = "resource_exhausted"
)

// CommandError is a command failure with a machine-readable code.
type CommandError struct {
	Code    string
	Message string
}

func (e *CommandError) Error() string { return e.Code + ": " + e.Message }

// Fail returns a CommandError.
func Fail(code, format string, args ...any) error {
	return &CommandError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func errorCode(err error) (code, msg string) {
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce.Code, ce.Message
	}
	return CodeInternal, err.Error()
}

func resultFor(cmd *Command, tick Tick, err error, units []UnitResult) CommandResult {
	r := CommandResult{CommandID: cmd.ID, Issuer: cmd.Issuer, Tick: tick, OK: err == nil, Units: units}
	if err != nil {
		r.Code, r.Message = errorCode(err)
	}
	for _, u := range units {
		if r.OK && !u.OK {
			r.Partial = true
		}
	}
	return r
}

// CommandDef registers a command handler.
//
// Execute runs on the simulation goroutine. It must be atomic: validate
// everything first and only then mutate, so a failed command leaves the
// world unchanged.
type CommandDef struct {
	Type      string
	AdminOnly bool
	Execute   func(ctx *TickContext, cmd Command) error
	// ApplyToUnit, if set, applies the command to a single unit: the
	// payload is a template whose own unit list is ignored. It makes the
	// command usable in batch commands (TickContext.ApplyToUnit). Like
	// Execute it must be atomic for that unit, and it must charge the
	// issuer's budget for the unit (TickContext.Charge).
	ApplyToUnit func(ctx *TickContext, cmd Command, unit EntityID) error
}

// Built-in kernel commands. All are admin only.
const (
	// CmdCreateEntity creates an entity. Payload: CreateEntity.
	CmdCreateEntity = "sim.CreateEntity"
	// CmdRemoveEntity deletes an entity and its components. Payload: RemoveEntity.
	CmdRemoveEntity = "sim.RemoveEntity"
	// CmdMoveEntity teleports an entity to an empty tile. Payload: MoveEntity.
	CmdMoveEntity = "sim.MoveEntity"
)

// Events emitted by built-in commands. They have an admin-only audience.
const (
	EventEntityCreated = "sim.entity_created"
	EventEntityRemoved = "sim.entity_removed"
	EventEntityMoved   = "sim.entity_moved"
)

// AdminAudience addresses an event to administrators only.
func AdminAudience() Audience { return Audience{} }

// CreateEntity is the payload of CmdCreateEntity.
type CreateEntity struct {
	Kind  Kind
	Owner PlayerID
	Pos   Pos
}

// RemoveEntity is the payload of CmdRemoveEntity.
type RemoveEntity struct {
	ID EntityID
}

// MoveEntity is the payload of CmdMoveEntity.
type MoveEntity struct {
	ID EntityID
	To Pos
}

func builtinCommands() []CommandDef {
	return []CommandDef{
		{
			Type:      CmdCreateEntity,
			AdminOnly: true,
			Execute: func(ctx *TickContext, cmd Command) error {
				p, ok := cmd.Payload.(CreateEntity)
				if !ok {
					return Fail(CodeInvalidArgument, "payload is %T, want sim.CreateEntity", cmd.Payload)
				}
				id, err := ctx.World.CreateEntity(p.Kind, p.Owner, p.Pos)
				if err != nil {
					return Fail(CodeFailedPrecondition, "%v", err)
				}
				ctx.Emit(Event{Type: EventEntityCreated, Entity: id, Audience: AdminAudience(), Fields: []Field{
					{Key: "command_id", Value: StringValue(cmd.ID)},
					{Key: "position", Value: PosValue(p.Pos)},
				}})
				return nil
			},
		},
		{
			Type:      CmdRemoveEntity,
			AdminOnly: true,
			Execute: func(ctx *TickContext, cmd Command) error {
				p, ok := cmd.Payload.(RemoveEntity)
				if !ok {
					return Fail(CodeInvalidArgument, "payload is %T, want sim.RemoveEntity", cmd.Payload)
				}
				if err := ctx.World.RemoveEntity(p.ID); err != nil {
					return Fail(CodeNotFound, "%v", err)
				}
				ctx.Emit(Event{Type: EventEntityRemoved, Entity: p.ID, Audience: AdminAudience(), Fields: []Field{
					{Key: "command_id", Value: StringValue(cmd.ID)},
				}})
				return nil
			},
		},
		{
			Type:      CmdMoveEntity,
			AdminOnly: true,
			Execute: func(ctx *TickContext, cmd Command) error {
				p, ok := cmd.Payload.(MoveEntity)
				if !ok {
					return Fail(CodeInvalidArgument, "payload is %T, want sim.MoveEntity", cmd.Payload)
				}
				if _, exists := ctx.World.Entity(p.ID); !exists {
					return Fail(CodeNotFound, "entity %d not found", p.ID)
				}
				if err := ctx.World.MoveEntities([]Move{{ID: p.ID, To: p.To}}); err != nil {
					return Fail(CodeFailedPrecondition, "%v", err)
				}
				ctx.Emit(Event{Type: EventEntityMoved, Entity: p.ID, Audience: AdminAudience(), Fields: []Field{
					{Key: "command_id", Value: StringValue(cmd.ID)},
					{Key: "position", Value: PosValue(p.To)},
				}})
				return nil
			},
		},
	}
}
