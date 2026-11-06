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
}

// Standard failure codes. Plugins may define more.
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeNotFound           = "not_found"
	CodePermissionDenied   = "permission_denied"
	CodeFailedPrecondition = "failed_precondition"
	CodeInternal           = "internal"
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

func resultFor(cmd *Command, tick Tick, err error) CommandResult {
	r := CommandResult{CommandID: cmd.ID, Issuer: cmd.Issuer, Tick: tick, OK: err == nil}
	if err != nil {
		var ce *CommandError
		if errors.As(err, &ce) {
			r.Code, r.Message = ce.Code, ce.Message
		} else {
			r.Code, r.Message = CodeInternal, err.Error()
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
}

// Built-in kernel commands.
const (
	// CmdCreateEntity creates an entity. Admin only. Payload: CreateEntity.
	CmdCreateEntity = "sim.CreateEntity"
)

// CreateEntity is the payload of CmdCreateEntity.
type CreateEntity struct {
	Kind  Kind
	Owner PlayerID
	Pos   Pos
}

func builtinCommands() []CommandDef {
	return []CommandDef{{
		Type:      CmdCreateEntity,
		AdminOnly: true,
		Execute: func(ctx *TickContext, cmd Command) error {
			p, ok := cmd.Payload.(CreateEntity)
			if !ok {
				return Fail(CodeInvalidArgument, "payload is %T, want sim.CreateEntity", cmd.Payload)
			}
			if _, err := ctx.World.CreateEntity(p.Kind, p.Owner, p.Pos); err != nil {
				return Fail(CodeFailedPrecondition, "%v", err)
			}
			return nil
		},
	}}
}
