// Package metadata implements player-owned unit metadata, deterministic
// unit selectors, and batch commands.
//
// Metadata is a small, bounded key/value collection per unit that only the
// owner can read or change. It is pure bookkeeping for players and their
// AI agents (squads, roles, notes): no plugin reads it for gameplay.
//
// A selector picks units by ownership, explicit IDs and metadata
// conditions. It is evaluated when its batch command executes — at the
// command's scheduled tick, in deterministic command order, after any
// earlier commands of that tick — and yields units in ascending entity-ID
// order. A batch command applies a template command to each selected unit
// through the kernel's per-unit command hook. Each unit is validated and
// charged (one unit of the issuer's per-tick command budget) separately,
// so a batch can partly succeed; the result lists every unit's outcome.
package metadata

import (
	"errors"
	"fmt"
	"regexp"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Name is the plugin name.
const Name = "metadata"

// MaxEntriesLimit is the hard upper bound on entries per unit.
const MaxEntriesLimit = 8

// AttrPrefix prefixes observable metadata keys: "metadata.squad".
const AttrPrefix = "metadata."

// Command types.
var (
	CommandSet    = string((&metadatav1.SetMetadataCommand{}).ProtoReflect().Descriptor().FullName())
	CommandRemove = string((&metadatav1.RemoveMetadataCommand{}).ProtoReflect().Descriptor().FullName())
	CommandBatch  = string((&metadatav1.BatchCommand{}).ProtoReflect().Descriptor().FullName())
)

var keyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// Config is the plugin's configuration schema (the "metadata" section).
type Config struct {
	MaxEntries     int `json:"max_entries"`
	MaxKeyLength   int `json:"max_key_length"`
	MaxValueLength int `json:"max_value_length"`
	// MaxBatchUnits caps how many units one batch command may select.
	MaxBatchUnits int `json:"max_batch_units"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{MaxEntries: MaxEntriesLimit, MaxKeyLength: 32, MaxValueLength: 64, MaxBatchUnits: 4096}
}

type entry struct{ Key, Value string }

// Entries is a unit's metadata, kept sorted by key.
type Entries struct {
	N    int
	Item [MaxEntriesLimit]entry
}

// Get returns the value for key.
func (m Entries) Get(key string) (string, bool) {
	for _, e := range m.Item[:m.N] {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

func (m *Entries) set(key, value string) bool {
	i := 0
	for ; i < m.N && m.Item[i].Key < key; i++ {
	}
	if i < m.N && m.Item[i].Key == key {
		m.Item[i].Value = value
		return true
	}
	if m.N == MaxEntriesLimit {
		return false
	}
	copy(m.Item[i+1:m.N+1], m.Item[i:m.N])
	m.Item[i] = entry{key, value}
	m.N++
	return true
}

func (m *Entries) remove(key string) {
	for i := 0; i < m.N; i++ {
		if m.Item[i].Key == key {
			copy(m.Item[i:m.N-1], m.Item[i+1:m.N])
			m.N--
			m.Item[m.N] = entry{}
			return
		}
	}
}

var store = sim.NewComponent[Entries]("metadata.entries")

// Get returns a unit's metadata.
func Get(w *sim.World, id sim.EntityID) Entries {
	m, _ := store.Get(w, id)
	return m
}

// Plugin implements sim.Plugin.
type Plugin struct {
	cfg Config
}

// New returns the metadata plugin.
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return Name }
func (*Plugin) DependsOn() []string { return nil }

// Register implements sim.Plugin.
func (p *Plugin) Register(r *sim.Registrar) error {
	p.cfg = DefaultConfig()
	if err := r.DecodeConfig(&p.cfg); err != nil {
		return err
	}
	c := p.cfg
	if c.MaxEntries < 1 || c.MaxEntries > MaxEntriesLimit || c.MaxKeyLength < 1 || c.MaxKeyLength > 64 ||
		c.MaxValueLength < 0 || c.MaxValueLength > 256 || c.MaxBatchUnits < 1 {
		return fmt.Errorf("metadata limits out of range: %+v (max_entries <= %d, key <= 64, value <= 256)", c, MaxEntriesLimit)
	}
	if err := r.Component(store.Def(sim.VisibleOwner, func(m Entries) []sim.Field {
		out := make([]sim.Field, m.N)
		for i, e := range m.Item[:m.N] {
			out[i] = sim.Field{Key: AttrPrefix + e.Key, Value: sim.StringValue(e.Value)}
		}
		return out
	})); err != nil {
		return err
	}
	if err := r.Describe(p.cfg); err != nil {
		return err
	}
	for _, d := range []sim.CommandDef{
		{Type: CommandSet, Execute: p.execSet, ApplyToUnit: p.applySet},
		{Type: CommandRemove, Execute: p.execRemove, ApplyToUnit: p.applyRemove},
		{Type: CommandBatch, Execute: p.execBatch},
	} {
		if err := r.Command(d); err != nil {
			return err
		}
	}
	return nil
}

// ---- metadata commands ------------------------------------------------------

func (p *Plugin) checkKey(key string) error {
	if len(key) == 0 || len(key) > p.cfg.MaxKeyLength || !keyRE.MatchString(key) {
		return sim.Fail(sim.CodeInvalidArgument, "invalid metadata key %q: 1..%d chars of [a-z0-9_.-], starting with a letter or digit", key, p.cfg.MaxKeyLength)
	}
	return nil
}

func (p *Plugin) checkValue(v string) error {
	if len(v) > p.cfg.MaxValueLength {
		return sim.Fail(sim.CodeInvalidArgument, "metadata value longer than %d bytes", p.cfg.MaxValueLength)
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return sim.Fail(sim.CodeInvalidArgument, "metadata values must be printable ASCII")
		}
	}
	return nil
}

func ownedUnit(w *sim.World, issuer sim.Principal, id sim.EntityID) error {
	e, ok := w.Entity(id)
	if !ok || e.Kind != sim.KindUnit {
		return sim.Fail(sim.CodeNotFound, "unit %d not found", id)
	}
	if !issuer.Admin && e.Owner != issuer.Player {
		return sim.Fail(sim.CodePermissionDenied, "unit %d is not yours", id)
	}
	return nil
}

func (p *Plugin) canSet(w *sim.World, id sim.EntityID, key string) error {
	m := Get(w, id)
	if _, exists := m.Get(key); !exists && m.N >= p.cfg.MaxEntries {
		return sim.Fail(sim.CodeFailedPrecondition, "unit %d already has %d metadata entries", id, p.cfg.MaxEntries)
	}
	return nil
}

func (p *Plugin) put(w *sim.World, id sim.EntityID, key, value string) {
	m := Get(w, id)
	m.set(key, value)
	store.Set(w, id, m)
}

func (p *Plugin) drop(w *sim.World, id sim.EntityID, key string) {
	m := Get(w, id)
	m.remove(key)
	if m.N == 0 {
		store.Remove(w, id)
	} else {
		store.Set(w, id, m)
	}
}

// eachUnit validates a unit list atomically, charges it, then applies fn.
func eachUnit(ctx *sim.TickContext, cmd sim.Command, ids []uint64, check func(sim.EntityID) error, apply func(sim.EntityID)) error {
	if len(ids) == 0 || len(ids) > 4096 {
		return sim.Fail(sim.CodeInvalidArgument, "need 1..4096 units, got %d", len(ids))
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return sim.Fail(sim.CodeInvalidArgument, "unit %d listed more than once", id)
		}
		seen[id] = true
		if err := ownedUnit(ctx.World, cmd.Issuer, sim.EntityID(id)); err != nil {
			return err
		}
		if err := check(sim.EntityID(id)); err != nil {
			return err
		}
	}
	if err := ctx.Charge(cmd.Issuer, len(ids)); err != nil {
		return err
	}
	for _, id := range ids {
		apply(sim.EntityID(id))
	}
	return nil
}

func (p *Plugin) execSet(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*metadatav1.SetMetadataCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetMetadataCommand", cmd.Payload)
	}
	if err := firstErr(p.checkKey(m.GetKey()), p.checkValue(m.GetValue())); err != nil {
		return err
	}
	return eachUnit(ctx, cmd, m.GetUnitIds(),
		func(id sim.EntityID) error { return p.canSet(ctx.World, id, m.GetKey()) },
		func(id sim.EntityID) { p.put(ctx.World, id, m.GetKey(), m.GetValue()) })
}

func (p *Plugin) applySet(ctx *sim.TickContext, cmd sim.Command, id sim.EntityID) error {
	m, ok := cmd.Payload.(*metadatav1.SetMetadataCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want SetMetadataCommand", cmd.Payload)
	}
	if err := firstErr(p.checkKey(m.GetKey()), p.checkValue(m.GetValue()), ownedUnit(ctx.World, cmd.Issuer, id)); err != nil {
		return err
	}
	// Charge only after every check passed.
	if err := firstErr(p.canSet(ctx.World, id, m.GetKey())); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	p.put(ctx.World, id, m.GetKey(), m.GetValue())
	return nil
}

func (p *Plugin) execRemove(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*metadatav1.RemoveMetadataCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want RemoveMetadataCommand", cmd.Payload)
	}
	if err := p.checkKey(m.GetKey()); err != nil {
		return err
	}
	return eachUnit(ctx, cmd, m.GetUnitIds(),
		func(sim.EntityID) error { return nil },
		func(id sim.EntityID) { p.drop(ctx.World, id, m.GetKey()) })
}

func (p *Plugin) applyRemove(ctx *sim.TickContext, cmd sim.Command, id sim.EntityID) error {
	m, ok := cmd.Payload.(*metadatav1.RemoveMetadataCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want RemoveMetadataCommand", cmd.Payload)
	}
	if err := firstErr(p.checkKey(m.GetKey()), ownedUnit(ctx.World, cmd.Issuer, id)); err != nil {
		return err
	}
	if err := ctx.Charge(cmd.Issuer, 1); err != nil {
		return err
	}
	p.drop(ctx.World, id, m.GetKey())
	return nil
}

// protoOpts resolves batch templates against the global registry, which
// holds every compiled-in plugin's command messages.
var protoOpts = proto.UnmarshalOptions{}

// firstErr returns the first non-nil error. Its arguments are evaluated
// eagerly, so only pass side-effect-free checks (never Charge).
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- selectors and batches --------------------------------------------------

// Select evaluates a selector for an issuer. It returns matching units in
// ascending ID order.
func (p *Plugin) Select(w *sim.World, issuer sim.Principal, s *metadatav1.Selector) ([]sim.EntityID, error) {
	for _, c := range s.GetConditions() {
		if err := p.checkKey(c.GetKey()); err != nil {
			return nil, err
		}
		if c.GetMatch() == metadatav1.MatchKind_MATCH_KIND_UNSPECIFIED || c.GetMatch() > metadatav1.MatchKind_MATCH_KIND_ABSENT {
			return nil, sim.Fail(sim.CodeInvalidArgument, "condition on %q: match kind required", c.GetKey())
		}
	}
	if s.GetOwnerId() != 0 && !issuer.Admin {
		return nil, sim.Fail(sim.CodePermissionDenied, "owner_id is admin only; selectors only match your own units")
	}
	limit := p.cfg.MaxBatchUnits
	if l := int(s.GetLimit()); l > 0 && l < limit {
		limit = l
	}
	var only map[sim.EntityID]bool
	if len(s.GetUnitIds()) > 0 {
		only = make(map[sim.EntityID]bool, len(s.GetUnitIds()))
		for _, id := range s.GetUnitIds() {
			only[sim.EntityID(id)] = true
		}
	}
	var out []sim.EntityID
	for _, id := range w.EntityIDs() {
		if len(out) == limit {
			break
		}
		if only != nil && !only[id] {
			continue
		}
		e, _ := w.Entity(id)
		if e.Kind != sim.KindUnit {
			continue
		}
		switch {
		case !issuer.Admin && e.Owner != issuer.Player:
			continue
		case issuer.Admin && s.GetOwnerId() != 0 && e.Owner != sim.PlayerID(s.GetOwnerId()):
			continue
		}
		if matches(Get(w, id), s.GetConditions()) {
			out = append(out, id)
		}
	}
	return out, nil
}

func matches(m Entries, conds []*metadatav1.MetadataCondition) bool {
	for _, c := range conds {
		v, ok := m.Get(c.GetKey())
		switch c.GetMatch() {
		case metadatav1.MatchKind_MATCH_KIND_EQUALS:
			if !ok || v != c.GetValue() {
				return false
			}
		case metadatav1.MatchKind_MATCH_KIND_PRESENT:
			if !ok {
				return false
			}
		case metadatav1.MatchKind_MATCH_KIND_ABSENT:
			if ok {
				return false
			}
		}
	}
	return true
}

func (p *Plugin) execBatch(ctx *sim.TickContext, cmd sim.Command) error {
	m, ok := cmd.Payload.(*metadatav1.BatchCommand)
	if !ok {
		return sim.Fail(sim.CodeInvalidArgument, "payload is %T, want BatchCommand", cmd.Payload)
	}
	if m.GetCommand() == nil {
		return sim.Fail(sim.CodeInvalidArgument, "batch command template missing")
	}
	inner, err := anypb.UnmarshalNew(m.GetCommand(), protoOpts)
	if err != nil {
		return sim.Fail(sim.CodeInvalidArgument, "cannot decode batch template %q: %v", m.GetCommand().GetTypeUrl(), err)
	}
	template := sim.Command{
		ID:      cmd.ID,
		Issuer:  cmd.Issuer,
		Type:    string(inner.ProtoReflect().Descriptor().FullName()),
		Payload: inner,
		Tick:    cmd.Tick,
		Seq:     cmd.Seq,
	}
	units, err := p.Select(ctx.World, cmd.Issuer, m.GetSelector())
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return sim.Fail(sim.CodeNotFound, "selector matched no units")
	}
	var firstFail error
	succeeded := 0
	for _, u := range units {
		err := ctx.ApplyToUnit(template, u)
		ctx.ReportUnit(u, err)
		if err == nil {
			succeeded++
		} else if firstFail == nil {
			firstFail = err
		}
	}
	if succeeded == 0 {
		code, msg := sim.CodeInternal, firstFail.Error()
		if ce := (*sim.CommandError)(nil); errors.As(firstFail, &ce) {
			code, msg = ce.Code, ce.Message
		}
		return sim.Fail(code, "no selected unit succeeded (%d tried); first error: %s", len(units), msg)
	}
	return nil
}
