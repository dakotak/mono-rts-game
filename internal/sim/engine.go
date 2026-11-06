package sim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes a world.
type Config struct {
	Width, Height int32
	// UnitCommandBudget is how many unit-commands each player may execute
	// per tick (one per unit a command affects). 0 means unlimited. Admin
	// commands are never charged.
	UnitCommandBudget int
	// Seed drives World.Rand (gameplay randomness).
	Seed uint64
	// IDKey is the secret key from which entity IDs are derived (at least
	// MinIDKeyLen bytes). It must never be sent to clients: anyone holding
	// it can predict IDs. It is part of the initial state for replays.
	IDKey []byte
}

// TickContext is passed to systems and command handlers.
type TickContext struct {
	World  *World
	Tick   Tick
	events []Event

	rules  *Ruleset
	budget int
	used   map[PlayerID]int
	units  []UnitResult // per-unit results of the command being executed
}

// Emit records an event for this tick.
func (c *TickContext) Emit(e Event) { c.events = append(c.events, e) }

// Charge consumes n unit-commands from the issuer's budget for this tick.
// It fails, consuming nothing, if the budget does not cover n.
func (c *TickContext) Charge(issuer Principal, n int) error {
	if issuer.Admin || c.budget <= 0 || n <= 0 {
		return nil
	}
	left := c.budget - c.used[issuer.Player]
	if n > left {
		return Fail(CodeResourceExhausted, "unit command budget exhausted: %d needed, %d of %d left this tick", n, left, c.budget)
	}
	c.used[issuer.Player] += n
	return nil
}

// BudgetLeft returns how many unit-commands the issuer may still execute
// this tick (-1 for unlimited).
func (c *TickContext) BudgetLeft(issuer Principal) int {
	if issuer.Admin || c.budget <= 0 {
		return -1
	}
	return c.budget - c.used[issuer.Player]
}

// ReportUnit records a per-unit outcome for the command being executed.
func (c *TickContext) ReportUnit(unit EntityID, err error) {
	r := UnitResult{Unit: unit, OK: err == nil}
	if err != nil {
		r.Code, r.Message = errorCode(err)
	}
	c.units = append(c.units, r)
}

// ApplyToUnit applies a unit-applicable command (see
// CommandDef.ApplyToUnit) to one unit, with the issuer of cmd. It enforces
// the command's admin restriction.
func (c *TickContext) ApplyToUnit(cmd Command, unit EntityID) error {
	def, ok := c.rules.commands[cmd.Type]
	if !ok || def.ApplyToUnit == nil {
		return Fail(CodeInvalidArgument, "command %q cannot be applied per unit", cmd.Type)
	}
	if def.AdminOnly && !cmd.Issuer.Admin {
		return Fail(CodePermissionDenied, "command %q requires admin privileges", cmd.Type)
	}
	return def.ApplyToUnit(c, cmd, unit)
}

// EntityView is the observable state of an entity, including all attributes
// tagged with their visibility. Filtering per observer happens outside the
// kernel.
type EntityView struct {
	ID       EntityID
	Kind     Kind
	Owner    PlayerID
	Pos      Pos
	Passable bool
	Attrs    []Attr
}

// Frame is everything that changed during one tick. Frames are immutable
// once produced and may be shared between goroutines.
type Frame struct {
	Tick    Tick
	Changed []EntityView // sorted by ID
	Removed []EntityID   // sorted
	Events  []Event
	Results []CommandResult
	// Commands executed this tick, in execution order. This, together with
	// the initial world, is the replay log.
	Commands []Command
	// Observations is set when an observation authority (the visibility
	// plugin) is active: it then defines exactly what each player may see
	// of entities they do not own, and servers must not show players
	// anything else about foreign entities. Sorted by player.
	Observations []Observation
	// ObservationMode reports whether an observation authority is active
	// (even if no player observed anything this tick).
	ObservationMode bool
}

// Observation is one player's change in observation over a tick.
type Observation struct {
	Player PlayerID
	// Tiles that became / stopped being visible, sorted (y, then x).
	TilesEntered, TilesExited []Pos
	// Foreign entities that became observable or whose observable (already
	// filtered) state changed. Sorted by ID.
	Upserts []ObservedEntity
	// Foreign entities that are no longer observable, sorted. Loss of
	// detection and destruction are indistinguishable here.
	Lost []EntityID
	// Diagnostics carries detection changes (observer, scores) that did not
	// change what the player sees. For administrators only; servers must
	// never send it to players.
	Diagnostics []ObservedEntity
}

// Identification levels of a foreign entity.
type Level uint8

const (
	LevelNone Level = iota
	LevelBasic
	LevelEstimated
	LevelDetailed
)

// ObservedEntity is a foreign entity as one player observes it: already
// filtered to what the player is allowed to know.
type ObservedEntity struct {
	View  EntityView
	Level Level
	// Diagnostics for administrators (never sent to players).
	Observer           EntityID
	DetectScore, Ident int64
}

// ObservationAuthority is implemented by a plugin that decides what each
// player observes. Registering one (Registrar.ObservationAuthority) turns
// on ObservationMode.
type ObservationAuthority interface {
	// Observations returns this tick's per-player deltas. It runs after
	// all systems, on the simulation goroutine.
	Observations(ctx *TickContext) []Observation
}

// Errors returned by Submit.
var (
	ErrUnknownCommand = errors.New("unknown command type")
	ErrAdminOnly      = errors.New("command requires admin privileges")
	ErrNotPaused      = errors.New("simulation is not paused")
	ErrStepBusy       = errors.New("a step request is already pending")
)

// MaxStepRequest bounds RequestSteps.
const MaxStepRequest = 1000

// Engine drives a World through ticks.
//
// Concurrency: Submit, SetPaused, Paused, RequestSteps and NextTick may be
// called from any goroutine. Everything else, and all world access, belongs to the
// simulation goroutine (the one calling Run or Step), or to setup code before
// the engine starts.
type Engine struct {
	rules  *Ruleset
	world  *World
	sink   func(*Frame)
	budget int

	mu       sync.Mutex
	nextTick Tick
	pending  []Command
	seq      map[Principal]uint64

	paused   atomic.Bool
	stepReq  chan int
	initDone bool
}

// NewEngine creates an engine with an empty world.
func NewEngine(cfg Config, rules *Ruleset) (*Engine, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("invalid world size %dx%d", cfg.Width, cfg.Height)
	}
	if len(cfg.IDKey) < MinIDKeyLen {
		return nil, fmt.Errorf("IDKey must be at least %d bytes, got %d", MinIDKeyLen, len(cfg.IDKey))
	}
	return &Engine{
		rules:    rules,
		budget:   cfg.UnitCommandBudget,
		world:    newWorld(cfg.Width, cfg.Height, cfg.Seed, cfg.IDKey, rules),
		nextTick: 1,
		seq:      make(map[Principal]uint64),
		stepReq:  make(chan int, 1),
	}, nil
}

// Init runs the plugins' Init functions. Call it once, after creating the
// initial entities from configuration and before the first tick.
func (e *Engine) Init() error {
	if e.initDone {
		return errors.New("engine already initialized")
	}
	e.initDone = true
	for _, in := range e.rules.inits {
		if err := in.fn(e.world); err != nil {
			return fmt.Errorf("init plugin %q: %w", in.plugin, err)
		}
	}
	return nil
}

// World returns the world. Only use it from the simulation goroutine or
// before the engine starts.
func (e *Engine) World() *World { return e.world }

// Rules returns the ruleset.
func (e *Engine) Rules() *Ruleset { return e.rules }

// SetFrameSink installs a callback invoked on the simulation goroutine with
// each produced frame. It must not block and must not retain the World.
// Call before the engine starts.
func (e *Engine) SetFrameSink(fn func(*Frame)) { e.sink = fn }

// Submit schedules a command for the next tick to execute and returns that
// tick. It only checks the envelope (known type, privileges); world-state
// validation happens at execution and is reported in the CommandResult.
func (e *Engine) Submit(cmd Command) (Tick, error) {
	def, ok := e.rules.commands[cmd.Type]
	if !ok {
		return 0, fmt.Errorf("%w %q", ErrUnknownCommand, cmd.Type)
	}
	if def.AdminOnly && !cmd.Issuer.Admin {
		return 0, ErrAdminOnly
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq[cmd.Issuer]++
	cmd.Seq = e.seq[cmd.Issuer]
	cmd.Tick = e.nextTick
	e.pending = append(e.pending, cmd)
	return cmd.Tick, nil
}

// NextTick returns the tick that will execute next.
func (e *Engine) NextTick() Tick {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nextTick
}

// SetPaused pauses or resumes Run. Commands submitted while paused execute
// on the first tick after resuming.
func (e *Engine) SetPaused(p bool) { e.paused.Store(p) }

// Paused reports whether Run is paused.
func (e *Engine) Paused() bool { return e.paused.Load() }

// RequestSteps asks Run to execute n ticks while paused. It returns the
// first tick that will execute; the steps happen asynchronously on the
// simulation goroutine. Only one request may be pending at a time.
func (e *Engine) RequestSteps(n int) (Tick, error) {
	if n < 1 || n > MaxStepRequest {
		return 0, fmt.Errorf("step count must be 1..%d, got %d", MaxStepRequest, n)
	}
	if !e.paused.Load() {
		return 0, ErrNotPaused
	}
	first := e.NextTick()
	select {
	case e.stepReq <- n:
		return first, nil
	default:
		return 0, ErrStepBusy
	}
}

// Step executes one tick with all commands submitted since the previous tick.
func (e *Engine) Step() *Frame {
	e.mu.Lock()
	tick := e.nextTick
	cmds := e.pending
	e.pending = nil
	e.nextTick++
	e.mu.Unlock()
	return e.execute(tick, cmds)
}

// StepWith executes one tick with exactly the given commands, ignoring the
// submission queue. It is used for deterministic replay; commands keep
// their recorded Seq values.
func (e *Engine) StepWith(cmds []Command) *Frame {
	e.mu.Lock()
	tick := e.nextTick
	e.nextTick++
	e.mu.Unlock()
	return e.execute(tick, slices.Clone(cmds))
}

func (e *Engine) execute(tick Tick, cmds []Command) *Frame {
	w := e.world
	w.tick = tick
	ctx := &TickContext{World: w, Tick: tick, rules: e.rules, budget: e.budget, used: map[PlayerID]int{}}

	slices.SortStableFunc(cmds, func(a, b Command) int {
		switch {
		case commandLess(&a, &b):
			return -1
		case commandLess(&b, &a):
			return 1
		}
		return 0
	})
	results := make([]CommandResult, 0, len(cmds))
	for i := range cmds {
		cmds[i].Tick = tick
		def, ok := e.rules.commands[cmds[i].Type]
		var err error
		ctx.units = nil
		if !ok {
			err = Fail(CodeInvalidArgument, "unknown command type %q", cmds[i].Type)
		} else {
			err = def.Execute(ctx, cmds[i])
		}
		results = append(results, resultFor(&cmds[i], tick, err, ctx.units))
	}

	for _, p := range e.rules.phases {
		for _, s := range p.systems {
			s.Run(ctx)
		}
	}

	// The observation authority runs last, after every system, so players
	// never see intermediate states. Its events join the tick's events.
	var obs []Observation
	if e.rules.observer != nil {
		obs = e.rules.observer.Observations(ctx)
	}
	changed, removed := w.takeChanges()
	f := &Frame{
		Tick:            tick,
		Changed:         changed,
		Removed:         removed,
		Events:          ctx.events,
		Results:         results,
		Commands:        cmds,
		Observations:    obs,
		ObservationMode: e.rules.observer != nil,
	}
	if e.sink != nil {
		e.sink(f)
	}
	return f
}

// Run steps the simulation at a fixed rate until ctx is cancelled. The wall
// clock only decides when a tick happens, never what it computes.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if !e.paused.Load() {
				e.Step()
			}
		case n := <-e.stepReq:
			for range n {
				e.Step()
			}
		}
	}
}
