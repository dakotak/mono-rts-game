// Package match is the match plugin: the lifecycle of a game (setup,
// ready, running, paused, finished), player readiness, victory conditions,
// scoring and per-player statistics.
//
// # Lifecycle
//
// The state lives in the world-scoped global "match.status", so it is part
// of the authoritative state, StateHash and replay, and every transition
// happens inside a tick through commands or the match system:
//
//	SETUP ──Init──▶ READY ──start──▶ RUNNING ◀──pause/resume──▶ PAUSED
//	                  │                 │                          │
//	                  └──────── end / victory condition ───────────┴──▶ FINISHED
//
// SETUP is the state while the world is being built (before plugin Init).
// With start mode "immediate" (the default) Init goes straight to RUNNING,
// so configurations without a "match" section behave as before.
//
// The plugin is the kernel's Gate: player gameplay commands execute only
// while RUNNING (and never for eliminated players); systems run only while
// RUNNING. Admin commands are always allowed. Ticks keep advancing in every
// state; the match timer (RunTicks) counts RUNNING ticks only.
//
// # Victory (checked at the end of every running tick, in this order)
//
//  1. Elimination ("last player standing", when enabled): a player with no
//     units is eliminated (publicly announced). When at most one player of two or more remains, the match
//     ends: the survivor wins, or it is a draw if the last ones were
//     eliminated in the same tick.
//  2. Custom conditions registered with AddCondition, in registration order.
//  3. Time limit: after time_limit_ticks running ticks the highest score
//     wins; equal top scores are a draw.
//
// Admin EndMatchCommand ends a match immediately, before any of these.
//
// # Scoring
//
// score = Σ weight(metric) × metric(player). Built-in metrics: current
// state — units, embodied (upgrade investment carried by living units),
// stored_energy; cumulative over the match — energy_mined, kills,
// investment_destroyed (invested energy of enemy units destroyed),
// units_lost. Score is a measurement, never spendable.
package match

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/attributes"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/cloning"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/logistics"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/salvage"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "match"

// Identifiers.
const (
	Phase          = "match"
	EventState     = "match.state"      // everyone: state, previous, reason
	EventReady     = "match.ready"      // everyone: ready_count, expected (no identities)
	EventElim      = "match.eliminated" // everyone: player, reason
	EventFinished  = "match.finished"   // everyone: winner, draw, reason, run_ticks
	EventScore     = "match.score"      // the player (or everyone if public): score
	MaxPlayers     = 16
	ReasonNoUnits  = "no_units"
	ReasonAdmin    = "admin"
	ReasonLastOne  = "last_player_standing"
	ReasonAllGone  = "simultaneous_elimination"
	ReasonTime     = "time_limit"
	CodeNotRunning = "match_not_running"
	CodeEliminated = "eliminated"
)

// Command types.
var (
	CommandReady     = string((&matchv1.SetReadyCommand{}).ProtoReflect().Descriptor().FullName())
	CommandStart     = string((&matchv1.StartMatchCommand{}).ProtoReflect().Descriptor().FullName())
	CommandPause     = string((&matchv1.PauseMatchCommand{}).ProtoReflect().Descriptor().FullName())
	CommandResume    = string((&matchv1.ResumeMatchCommand{}).ProtoReflect().Descriptor().FullName())
	CommandEnd       = string((&matchv1.EndMatchCommand{}).ProtoReflect().Descriptor().FullName())
	CommandEliminate = string((&matchv1.EliminatePlayerCommand{}).ProtoReflect().Descriptor().FullName())
)

// State is the match lifecycle state.
type State uint8

const (
	Setup State = iota
	Ready
	Running
	Paused
	Finished
)

var stateNames = [...]string{"setup", "ready", "running", "paused", "finished"}

func (s State) String() string { return stateNames[s] }

// Stats are cumulative per-player match statistics.
type Stats struct {
	EnergyMined         int64
	Kills               int64
	UnitsLost           int64
	InvestmentDestroyed int64
}

// Status is the authoritative match state (the "match.status" global).
// Player arrays are indexed in configuration order (Players[:N]).
type Status struct {
	State      State
	N          uint8
	Players    [MaxPlayers]sim.PlayerID
	Ready      [MaxPlayers]bool
	Eliminated [MaxPlayers]bool
	Stats      [MaxPlayers]Stats
	Scores     [MaxPlayers]int64
	// Final holds each player's current totals, frozen when the match ends.
	Final      [MaxPlayers]Aggregates
	StartTick  sim.Tick
	FinishTick sim.Tick
	RunTicks   int64
	Winner     sim.PlayerID
	Draw       bool
	Reason     string
}

// Index returns a player's index, or -1.
func (s *Status) Index(p sim.PlayerID) int {
	for i := range int(s.N) {
		if s.Players[i] == p {
			return i
		}
	}
	return -1
}

var status = sim.NewGlobal(Name+".status", Status{})

// Get returns the current match status.
func Get(w *sim.World) Status { return status.Get(w) }

// GlobalID is the ID of the status global (for reading Frame.Globals).
var GlobalID = status.ID()

// Config is the plugin's configuration schema (the "match" section).
type Config struct {
	// Start: "immediate" (default: RUNNING right after setup), "all_ready"
	// (start when min_players players are ready) or "manual" (admin
	// StartMatchCommand).
	Start string `json:"start"`
	// MinPlayers ready players needed with "all_ready" (0 = every player).
	MinPlayers int `json:"min_players"`
	Victory    struct {
		Elimination    bool  `json:"elimination"`
		TimeLimitTicks int64 `json:"time_limit_ticks"`
	} `json:"victory"`
	Scoring struct {
		Weights map[string]int64 `json:"weights"`
		// PublicDuringMatch shows every player's score to everyone while
		// the match runs; otherwise players see only their own until it
		// finishes.
		PublicDuringMatch bool  `json:"public_during_match"`
		IntervalTicks     int64 `json:"interval_ticks"`
	} `json:"scoring"`
}

// Outcome ends a match.
type Outcome struct {
	Winner sim.PlayerID // 0 with Draw = no winner
	Draw   bool
	Reason string
}

// Condition is a victory condition (extension point). Check runs at the end
// of every running tick, after elimination bookkeeping; a non-nil outcome
// finishes the match. It must be deterministic and read-only.
type Condition interface {
	Name() string
	Check(v *View) *Outcome
}

// Metric is a scoring contribution (extension point). Value must be
// deterministic and read-only. Weighted in config by Name().
type Metric interface {
	Name() string
	// Cumulative reports whether the value accumulates over the match (as
	// opposed to measuring current state); informational.
	Cumulative() bool
	Value(v *View, p sim.PlayerID) int64
}

// Plugin implements sim.Plugin and sim.Gate.
type Plugin struct {
	cfg        Config
	attrs      *attributes.Plugin
	conditions []Condition
	metrics    []Metric
	byName     map[string]Metric
	weights    []weight // sorted by metric name
	players    []sim.PlayerID
}

type weight struct {
	m Metric
	w int64
}

// New returns the plugin. c is the combat plugin (destruction statistics),
// attrs the shared attributes plugin (embodied investment).
func New(attrs *attributes.Plugin, c *combat.Plugin) *Plugin {
	p := &Plugin{attrs: attrs, metrics: builtinMetrics()}
	c.OnDestroyed(p.destroyed)
	return p
}

// AddCondition registers a custom victory condition. Call before the
// ruleset is built (in game.Plugins()).
func (p *Plugin) AddCondition(c Condition) { p.conditions = append(p.conditions, c) }

// AddMetric registers a custom scoring metric. Call before the ruleset is
// built; weight it by name in match.scoring.weights.
func (p *Plugin) AddMetric(m Metric) { p.metrics = append(p.metrics, m) }

// SetPlayers sets the match's players in configuration order. game.New
// calls it before the engine initializes.
func (p *Plugin) SetPlayers(ps []sim.PlayerID) { p.players = slices.Clone(ps) }

// Config returns the effective configuration (immutable after Register).
func (p *Plugin) Config() Config { return p.cfg }

// ConditionNames lists active victory conditions in precedence order.
func (p *Plugin) ConditionNames() []string {
	var out []string
	if p.cfg.Victory.Elimination {
		out = append(out, "elimination")
	}
	for _, c := range p.conditions {
		out = append(out, c.Name())
	}
	if p.cfg.Victory.TimeLimitTicks > 0 {
		out = append(out, "time_limit")
	}
	return out
}

func (*Plugin) Name() string { return Name }

// DependsOn lists the plugins whose state the match reads; registering
// after them also places the match phase after theirs.
func (*Plugin) DependsOn() []string {
	return []string{attributes.Name, energy.Name, combat.Name, cloning.Name, salvage.Name, logistics.Name}
}

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg.Start = "immediate"
	p.cfg.Scoring.IntervalTicks = 50
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	if p.cfg.Scoring.Weights == nil {
		p.cfg.Scoring.Weights = map[string]int64{"units": 100, "embodied": 1, "stored_energy": 1, "kills": 50}
	}
	switch {
	case p.cfg.Start != "immediate" && p.cfg.Start != "all_ready" && p.cfg.Start != "manual":
		return fmt.Errorf("match start must be immediate, all_ready or manual, got %q", p.cfg.Start)
	case p.cfg.MinPlayers < 0 || p.cfg.MinPlayers > MaxPlayers:
		return fmt.Errorf("match min_players must be in 0..%d", MaxPlayers)
	case p.cfg.Victory.TimeLimitTicks < 0:
		return fmt.Errorf("match victory.time_limit_ticks must be >= 0")
	case p.cfg.Scoring.IntervalTicks < 1:
		return fmt.Errorf("match scoring.interval_ticks must be >= 1")
	}
	p.byName = map[string]Metric{}
	for _, m := range p.metrics {
		if _, dup := p.byName[m.Name()]; dup {
			return fmt.Errorf("match: metric %q registered twice", m.Name())
		}
		p.byName[m.Name()] = m
	}
	for _, name := range slices.Sorted(maps.Keys(p.cfg.Scoring.Weights)) {
		m, ok := p.byName[name]
		if !ok {
			return fmt.Errorf("match scoring.weights: unknown metric %q (available: %v)", name, slices.Sorted(maps.Keys(p.byName)))
		}
		p.weights = append(p.weights, weight{m, p.cfg.Scoring.Weights[name]})
	}
	if err := r.Global(status.Def()); err != nil {
		return err
	}
	if err := r.Gate(p); err != nil {
		return err
	}
	if err := r.Phase(sim.PhaseDef{Name: Phase, After: []string{combat.Phase, cloning.Phase, salvage.Phase, logistics.Phase}}); err != nil {
		return err
	}
	if err := r.System(sim.SystemDef{Name: "match.evaluate", Phase: Phase, Run: p.evaluate}); err != nil {
		return err
	}
	r.Init(p.init)
	for _, c := range []sim.CommandDef{
		{Type: CommandReady, Execute: p.execReady},
		{Type: CommandStart, AdminOnly: true, Execute: p.execStart},
		{Type: CommandPause, AdminOnly: true, Execute: p.execPause},
		{Type: CommandResume, AdminOnly: true, Execute: p.execResume},
		{Type: CommandEnd, AdminOnly: true, Execute: p.execEnd},
		{Type: CommandEliminate, AdminOnly: true, Execute: p.execEliminate},
	} {
		if err := r.Command(c); err != nil {
			return err
		}
	}
	metricNames := slices.Sorted(maps.Keys(p.byName))
	return r.Describe(map[string]any{
		"config":     p.cfg,
		"conditions": p.ConditionNames(),
		"metrics":    metricNames,
		"precedence": "admin end > elimination > custom conditions (registration order) > time limit",
	})
}

func (p *Plugin) init(w *sim.World) error {
	switch n := len(p.players); {
	case n > MaxPlayers:
		return fmt.Errorf("match: at most %d players, got %d", MaxPlayers, n)
	case p.cfg.Start == "all_ready" && n == 0:
		return fmt.Errorf(`match: start "all_ready" needs at least one player`)
	case p.cfg.MinPlayers > n:
		return fmt.Errorf("match: min_players %d exceeds the %d configured players", p.cfg.MinPlayers, n)
	}
	st := Status{State: Ready, N: uint8(len(p.players))}
	copy(st.Players[:], p.players)
	if p.cfg.Start == "immediate" {
		st.State = Running
		st.StartTick = w.Tick()
	}
	status.Set(w, st)
	return nil
}

// ---- gate ---------------------------------------------------------------------------

// AllowCommand implements sim.Gate.
func (p *Plugin) AllowCommand(w *sim.World, cmd sim.Command) error {
	if cmd.Issuer.Admin || cmd.Type == CommandReady {
		return nil
	}
	st := status.Get(w)
	if st.State != Running {
		return sim.Fail(sim.CodeFailedPrecondition, "match is %s: gameplay commands are accepted only while it is running", st.State)
	}
	if i := st.Index(cmd.Issuer.Player); i >= 0 && st.Eliminated[i] {
		return sim.Fail(sim.CodeFailedPrecondition, "player %d has been eliminated", cmd.Issuer.Player)
	}
	return nil
}

// RunSystems implements sim.Gate.
func (p *Plugin) RunSystems(w *sim.World) bool { return status.Get(w).State == Running }

// ---- transitions -----------------------------------------------------------------------

func (p *Plugin) transition(ctx *sim.TickContext, st *Status, to State, reason string) {
	from := st.State
	st.State = to
	switch to {
	case Running:
		if from == Ready {
			st.StartTick = ctx.Tick
		}
	case Finished:
		st.FinishTick = ctx.Tick
		v := p.score(ctx.World, st)
		for i := range int(st.N) {
			st.Final[i] = v.Aggregate(st.Players[i])
		}
	}
	ctx.Emit(sim.Event{Type: EventState, Audience: sim.ToAll(), Fields: []sim.Field{
		{Key: "state", Value: sim.StringValue(to.String())},
		{Key: "previous", Value: sim.StringValue(from.String())},
		{Key: "reason", Value: sim.StringValue(reason)},
	}})
	if to == Finished {
		ctx.Emit(sim.Event{Type: EventFinished, Audience: sim.ToAll(), Fields: []sim.Field{
			{Key: "winner", Value: idv(uint64(st.Winner))},
			{Key: "draw", Value: sim.BoolValue(st.Draw)},
			{Key: "reason", Value: sim.StringValue(st.Reason)},
			{Key: "run_ticks", Value: sim.IntValue(st.RunTicks)},
		}})
	}
}

func (p *Plugin) finish(ctx *sim.TickContext, st *Status, o Outcome) {
	st.Winner, st.Draw, st.Reason = o.Winner, o.Draw || o.Winner == 0, o.Reason
	p.transition(ctx, st, Finished, o.Reason)
}

func idv(v uint64) sim.Value { return sim.StringValue(strconv.FormatUint(v, 10)) }

func (p *Plugin) execReady(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*matchv1.SetReadyCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetReadyCommand", cmd.Payload)
	}
	st := status.Get(ctx.World)
	i := st.Index(cmd.Issuer.Player)
	switch {
	case cmd.Issuer.Admin || i < 0:
		return sim.Fail(sim.CodeInvalidArgument, "only participating players can be ready")
	case st.State != Ready:
		return sim.Fail(sim.CodeFailedPrecondition, "match is %s: readiness can only change while it is ready", st.State)
	}
	st.Ready[i] = m.GetReady()
	n := 0
	for j := range int(st.N) {
		if st.Ready[j] {
			n++
		}
	}
	need := int(st.N)
	if p.cfg.MinPlayers > 0 {
		need = min(p.cfg.MinPlayers, need)
	}
	ctx.Emit(sim.Event{Type: EventReady, Audience: sim.ToAll(), Fields: []sim.Field{
		{Key: "ready_count", Value: sim.IntValue(int64(n))},
		{Key: "expected", Value: sim.IntValue(int64(need))},
	}})
	if p.cfg.Start == "all_ready" && n >= need && need > 0 {
		p.transition(ctx, &st, Running, "all_ready")
	}
	status.Set(ctx.World, st)
	return nil
}

func (p *Plugin) adminTransition(ctx *sim.TickContext, from []State, to State, reason string) error {
	st := status.Get(ctx.World)
	if !slices.Contains(from, st.State) {
		return sim.Fail(sim.CodeFailedPrecondition, "cannot go from %s to %s", st.State, to)
	}
	p.transition(ctx, &st, to, reason)
	status.Set(ctx.World, st)
	return nil
}

func (p *Plugin) execStart(ctx *sim.TickContext, _ sim.Command) error {
	return p.adminTransition(ctx, []State{Ready}, Running, "admin_start")
}

func (p *Plugin) execPause(ctx *sim.TickContext, _ sim.Command) error {
	return p.adminTransition(ctx, []State{Running}, Paused, "admin_pause")
}

func (p *Plugin) execResume(ctx *sim.TickContext, _ sim.Command) error {
	return p.adminTransition(ctx, []State{Paused}, Running, "admin_resume")
}

func (p *Plugin) execEnd(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*matchv1.EndMatchCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want EndMatchCommand", cmd.Payload)
	}
	st := status.Get(ctx.World)
	if st.State == Finished || st.State == Setup {
		return sim.Fail(sim.CodeFailedPrecondition, "match is %s", st.State)
	}
	winner := sim.PlayerID(m.GetWinnerId())
	if winner != 0 && st.Index(winner) < 0 {
		return sim.Fail(sim.CodeInvalidArgument, "unknown player %d", winner)
	}
	reason := m.GetReason()
	if reason == "" {
		reason = ReasonAdmin
	}
	p.finish(ctx, &st, Outcome{Winner: winner, Reason: reason})
	status.Set(ctx.World, st)
	return nil
}

func (p *Plugin) execEliminate(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*matchv1.EliminatePlayerCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want EliminatePlayerCommand", cmd.Payload)
	}
	st := status.Get(ctx.World)
	i := st.Index(sim.PlayerID(m.GetPlayerId()))
	switch {
	case i < 0:
		return sim.Fail(sim.CodeInvalidArgument, "unknown player %d", m.GetPlayerId())
	case st.State == Finished || st.State == Setup:
		return sim.Fail(sim.CodeFailedPrecondition, "match is %s", st.State)
	case st.Eliminated[i]:
		return sim.Fail(sim.CodeFailedPrecondition, "player %d is already eliminated", m.GetPlayerId())
	}
	w := ctx.World
	if m.GetRemoveUnits() {
		for _, id := range slices.Clone(w.EntityIDs()) {
			if e, _ := w.Entity(id); e.Kind == sim.KindUnit && e.Owner == st.Players[i] {
				_ = w.RemoveEntity(id)
			}
		}
	}
	p.eliminate(ctx, &st, i, ReasonAdmin)
	if st.State != Ready {
		p.checkElimination(ctx, &st)
	}
	status.Set(w, st)
	return nil
}

func (p *Plugin) eliminate(ctx *sim.TickContext, st *Status, i int, reason string) {
	st.Eliminated[i] = true
	ctx.Emit(sim.Event{Type: EventElim, Audience: sim.ToAll(), Fields: []sim.Field{
		{Key: "player", Value: idv(uint64(st.Players[i]))},
		{Key: "reason", Value: sim.StringValue(reason)},
	}})
}

// checkElimination ends the match when elimination victory is on and at
// most one of two or more players remains.
func (p *Plugin) checkElimination(ctx *sim.TickContext, st *Status) bool {
	if !p.cfg.Victory.Elimination || st.N < 2 || st.State == Finished {
		return false
	}
	alive, last := 0, sim.PlayerID(0)
	for i := range int(st.N) {
		if !st.Eliminated[i] {
			alive++
			last = st.Players[i]
		}
	}
	switch alive {
	case 0:
		p.finish(ctx, st, Outcome{Draw: true, Reason: ReasonAllGone})
	case 1:
		p.finish(ctx, st, Outcome{Winner: last, Reason: ReasonLastOne})
	default:
		return false
	}
	return true
}

// ---- per-tick evaluation (running only) ---------------------------------------------------

func (p *Plugin) evaluate(ctx *sim.TickContext) {
	w := ctx.World
	st := status.Get(w)
	if st.State != Running {
		return
	}
	st.RunTicks++
	// Cumulative statistics from this tick's events.
	for _, ev := range ctx.Events() {
		if ev.Type == energy.EventMined {
			if i := st.Index(ev.Audience.Player); i >= 0 {
				for _, f := range ev.Fields {
					if f.Key == "amount" {
						st.Stats[i].EnergyMined += f.Value.Int
					}
				}
			}
		}
	}
	// Elimination (only with elimination victory: it is public news that
	// a player has no units left, so it is not tracked otherwise).
	if p.cfg.Victory.Elimination {
		var units [MaxPlayers]int
		for _, id := range w.EntityIDs() {
			if e, _ := w.Entity(id); e.Kind == sim.KindUnit {
				if i := st.Index(e.Owner); i >= 0 {
					units[i]++
				}
			}
		}
		for i := range int(st.N) {
			if !st.Eliminated[i] && units[i] == 0 {
				p.eliminate(ctx, &st, i, ReasonNoUnits)
			}
		}
	}
	done := p.checkElimination(ctx, &st)
	if !done {
		v := p.view(w, &st)
		for _, c := range p.conditions {
			if o := c.Check(v); o != nil {
				p.finish(ctx, &st, *o)
				done = true
				break
			}
		}
	}
	if !done && p.cfg.Victory.TimeLimitTicks > 0 && st.RunTicks >= p.cfg.Victory.TimeLimitTicks {
		p.score(w, &st)
		best, winner, tie := int64(0), sim.PlayerID(0), false
		for i := range int(st.N) {
			switch s := st.Scores[i]; {
			case winner == 0 || s > best:
				best, winner, tie = s, st.Players[i], false
			case s == best:
				tie = true
			}
		}
		if tie {
			winner = 0
		}
		p.finish(ctx, &st, Outcome{Winner: winner, Draw: tie, Reason: ReasonTime})
		done = true
	}
	if !done && st.RunTicks%p.cfg.Scoring.IntervalTicks == 0 {
		p.score(w, &st)
		for i := range int(st.N) {
			aud := sim.ToPlayer(st.Players[i])
			if p.cfg.Scoring.PublicDuringMatch {
				aud = sim.ToAll()
			}
			ctx.Emit(sim.Event{Type: EventScore, Audience: aud, Fields: []sim.Field{
				{Key: "player", Value: idv(uint64(st.Players[i]))},
				{Key: "score", Value: sim.IntValue(st.Scores[i])},
			}})
		}
	}
	status.Set(w, st)
}

// destroyed is the combat destruction hook: statistics for the owner and
// every other player whose units hit the unit in the destroying tick.
func (p *Plugin) destroyed(ctx *sim.TickContext, d combat.Destroyed) {
	st := status.Get(ctx.World)
	if i := st.Index(d.Unit.Owner); i >= 0 {
		st.Stats[i].UnitsLost++
	}
	for _, a := range d.Audience {
		if a == d.Unit.Owner {
			continue
		}
		if i := st.Index(a); i >= 0 {
			st.Stats[i].Kills++
			st.Stats[i].InvestmentDestroyed += d.Invested
		}
	}
	status.Set(ctx.World, st)
}

// score recomputes every player's score into st.Scores.
func (p *Plugin) score(w *sim.World, st *Status) *View {
	v := p.view(w, st)
	for i := range int(st.N) {
		var s int64
		for _, wt := range p.weights {
			s += wt.w * wt.m.Value(v, st.Players[i])
		}
		st.Scores[i] = s
	}
	return v
}

// ---- view for conditions and metrics --------------------------------------------------------

// Aggregates are current per-player totals.
type Aggregates struct {
	Units, Embodied, Stored int64
}

// View is the read-only context passed to conditions and metrics.
type View struct {
	World  *sim.World
	Status *Status
	p      *Plugin
	aggs   map[sim.PlayerID]Aggregates
}

func (p *Plugin) view(w *sim.World, st *Status) *View { return &View{World: w, Status: st, p: p} }

// Aggregate returns a player's current unit count, embodied investment and
// stored energy (computed once per view).
func (v *View) Aggregate(pl sim.PlayerID) Aggregates {
	if v.aggs == nil {
		v.aggs = map[sim.PlayerID]Aggregates{}
		for _, id := range v.World.EntityIDs() {
			e, _ := v.World.Entity(id)
			if e.Kind != sim.KindUnit || e.Owner == 0 {
				continue
			}
			a := v.aggs[e.Owner]
			a.Units++
			if s, ok := energy.GetStore(v.World, id); ok {
				a.Stored += s.Energy
			}
			if v.p.attrs != nil {
				if u, ok := v.p.attrs.Get(v.World, id); ok {
					a.Embodied += v.p.attrs.Embodied(u)
				}
			}
			v.aggs[e.Owner] = a
		}
	}
	return v.aggs[pl]
}

// Stats returns a player's cumulative statistics.
func (v *View) Stats(pl sim.PlayerID) Stats {
	if i := v.Status.Index(pl); i >= 0 {
		return v.Status.Stats[i]
	}
	return Stats{}
}

// Score returns a player's score under the configured weights.
func (v *View) Score(pl sim.PlayerID) int64 {
	var s int64
	for _, wt := range v.p.weights {
		s += wt.w * wt.m.Value(v, pl)
	}
	return s
}

type metricFn struct {
	name string
	cum  bool
	fn   func(v *View, p sim.PlayerID) int64
}

func (m metricFn) Name() string                        { return m.name }
func (m metricFn) Cumulative() bool                    { return m.cum }
func (m metricFn) Value(v *View, p sim.PlayerID) int64 { return m.fn(v, p) }

// NewMetric builds a Metric from a function.
func NewMetric(name string, cumulative bool, fn func(v *View, p sim.PlayerID) int64) Metric {
	return metricFn{name, cumulative, fn}
}

func builtinMetrics() []Metric {
	return []Metric{
		NewMetric("units", false, func(v *View, p sim.PlayerID) int64 { return v.Aggregate(p).Units }),
		NewMetric("embodied", false, func(v *View, p sim.PlayerID) int64 { return v.Aggregate(p).Embodied }),
		NewMetric("stored_energy", false, func(v *View, p sim.PlayerID) int64 { return v.Aggregate(p).Stored }),
		NewMetric("energy_mined", true, func(v *View, p sim.PlayerID) int64 { return v.Stats(p).EnergyMined }),
		NewMetric("kills", true, func(v *View, p sim.PlayerID) int64 { return v.Stats(p).Kills }),
		NewMetric("investment_destroyed", true, func(v *View, p sim.PlayerID) int64 { return v.Stats(p).InvestmentDestroyed }),
		NewMetric("units_lost", true, func(v *View, p sim.PlayerID) int64 { return v.Stats(p).UnitsLost }),
	}
}
