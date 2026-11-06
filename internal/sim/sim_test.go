package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// testPlugin is a configurable plugin for exercising the registry.
type testPlugin struct {
	name     string
	deps     []string
	register func(r *Registrar) error
}

func (p testPlugin) Name() string        { return p.name }
func (p testPlugin) DependsOn() []string { return p.deps }
func (p testPlugin) Register(r *Registrar) error {
	if p.register == nil {
		return nil
	}
	return p.register(r)
}

var testKey = []byte("0123456789abcdef")

func mustRules(t *testing.T, plugins ...Plugin) *Ruleset {
	t.Helper()
	rs, err := NewRuleset(plugins, nil)
	if err != nil {
		t.Fatalf("NewRuleset: %v", err)
	}
	return rs
}

func mustEngine(t *testing.T, rs *Ruleset) *Engine {
	t.Helper()
	e, err := NewEngine(Config{Width: 10, Height: 8, Seed: 1, IDKey: testKey}, rs)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestRulesetPluginValidation(t *testing.T) {
	tests := []struct {
		name    string
		plugins []Plugin
		config  map[string]json.RawMessage
		wantErr string
	}{
		{"missing dependency", []Plugin{testPlugin{name: "a", deps: []string{"b"}}}, nil, "missing plugin"},
		{"duplicate plugin", []Plugin{testPlugin{name: "a"}, testPlugin{name: "a"}}, nil, "registered twice"},
		{"dependency cycle", []Plugin{testPlugin{name: "a", deps: []string{"b"}}, testPlugin{name: "b", deps: []string{"a"}}}, nil, "cycle"},
		{"reserved name", []Plugin{testPlugin{name: "sim"}}, nil, "reserved"},
		{"bad name", []Plugin{testPlugin{name: "a.b"}}, nil, "invalid plugin name"},
		{"config for unknown plugin", []Plugin{testPlugin{name: "a"}}, map[string]json.RawMessage{"zzz": []byte(`{}`)}, "unknown plugin"},
		{"unprefixed system", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			return r.System(SystemDef{Name: "step", Phase: "p", Run: func(*TickContext) {}})
		}}}, nil, "must be prefixed"},
		{"unknown phase", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			return r.System(SystemDef{Name: "a.step", Phase: "nope", Run: func(*TickContext) {}})
		}}}, nil, "unknown phase"},
		{"duplicate command", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			return r.Command(CommandDef{Type: CmdCreateEntity, Execute: func(*TickContext, Command) error { return nil }})
		}}}, nil, "already declared"},
		{"phase cycle", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			_ = r.Phase(PhaseDef{Name: "x", After: []string{"y"}})
			return r.Phase(PhaseDef{Name: "y", After: []string{"x"}})
		}}}, nil, "cycle"},
		{"cross-phase system order", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			_ = r.Phase(PhaseDef{Name: "x"})
			_ = r.Phase(PhaseDef{Name: "y"})
			_ = r.System(SystemDef{Name: "a.one", Phase: "x", Run: func(*TickContext) {}})
			return r.System(SystemDef{Name: "a.two", Phase: "y", After: []string{"a.one"}, Run: func(*TickContext) {}})
		}}}, nil, "another phase"},
		{"plugin register error", []Plugin{testPlugin{name: "a", register: func(r *Registrar) error {
			return errors.New("boom")
		}}}, nil, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRuleset(tt.plugins, tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRulesetOrdering(t *testing.T) {
	var regOrder []string
	var runOrder []string
	sys := func(r *Registrar, name, phase string, after ...string) {
		if err := r.System(SystemDef{Name: name, Phase: phase, After: after, Run: func(*TickContext) {
			runOrder = append(runOrder, name)
		}}); err != nil {
			t.Fatal(err)
		}
	}
	// "zeta" is listed first but depends on "alpha", so alpha registers first.
	zeta := testPlugin{name: "zeta", deps: []string{"alpha"}, register: func(r *Registrar) error {
		regOrder = append(regOrder, "zeta")
		if err := r.Phase(PhaseDef{Name: "late", After: []string{"early"}}); err != nil {
			return err
		}
		sys(r, "zeta.b", "late")
		sys(r, "zeta.a", "late", "zeta.b")
		sys(r, "zeta.c", "early")
		return nil
	}}
	alpha := testPlugin{name: "alpha", register: func(r *Registrar) error {
		regOrder = append(regOrder, "alpha")
		if err := r.Phase(PhaseDef{Name: "early"}); err != nil {
			return err
		}
		sys(r, "alpha.z", "early")
		return nil
	}}
	rs := mustRules(t, zeta, alpha)
	if got := strings.Join(regOrder, ","); got != "alpha,zeta" {
		t.Errorf("registration order = %s", got)
	}
	e := mustEngine(t, rs)
	e.Step()
	// early: alpha.z, zeta.c (name order); late: zeta.b then zeta.a (After).
	if got := strings.Join(runOrder, ","); got != "alpha.z,zeta.c,zeta.b,zeta.a" {
		t.Errorf("run order = %s", got)
	}
	if got := strings.Join(rs.Schedule(), "; "); got != "early: alpha.z, zeta.c; late: zeta.b, zeta.a" {
		t.Errorf("schedule = %s", got)
	}
}

func TestDecodeConfigStrict(t *testing.T) {
	type cfg struct {
		Speed int `json:"speed"`
	}
	var got cfg
	p := testPlugin{name: "a", register: func(r *Registrar) error {
		got = cfg{Speed: 7}
		return r.DecodeConfig(&got)
	}}
	if _, err := NewRuleset([]Plugin{p}, nil); err != nil || got.Speed != 7 {
		t.Fatalf("defaults: err=%v speed=%d", err, got.Speed)
	}
	if _, err := NewRuleset([]Plugin{p}, map[string]json.RawMessage{"a": []byte(`{"speed":3}`)}); err != nil || got.Speed != 3 {
		t.Fatalf("override: err=%v speed=%d", err, got.Speed)
	}
	if _, err := NewRuleset([]Plugin{p}, map[string]json.RawMessage{"a": []byte(`{"sped":3}`)}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// recorder registers a command that appends its payload to a log.
func recorder(log *[]string) Plugin {
	return testPlugin{name: "rec", register: func(r *Registrar) error {
		return r.Command(CommandDef{Type: "rec.Note", Execute: func(ctx *TickContext, c Command) error {
			s, _ := c.Payload.(string)
			if s == "bad" {
				return Fail(CodeInvalidArgument, "bad note")
			}
			*log = append(*log, fmt.Sprintf("t%d %s %s", ctx.Tick, c.Issuer, s))
			return nil
		}})
	}}
}

func TestCommandTickAssignmentAndOrdering(t *testing.T) {
	var log []string
	e := mustEngine(t, mustRules(t, recorder(&log)))

	submit := func(p Principal, note string) Tick {
		t.Helper()
		tick, err := e.Submit(Command{ID: note, Issuer: p, Type: "rec.Note", Payload: note})
		if err != nil {
			t.Fatal(err)
		}
		return tick
	}
	p1, p2, admin := Principal{Player: 1}, Principal{Player: 2}, Principal{Admin: true}
	// Arrival order: p2, p1, admin, p2. Execution order must be admin, p1,
	// then p2 in its own submission order.
	for _, tick := range []Tick{submit(p2, "b1"), submit(p1, "a1"), submit(admin, "x"), submit(p2, "b2")} {
		if tick != 1 {
			t.Fatalf("scheduled tick = %d, want 1", tick)
		}
	}
	f := e.Step()
	if tick := submit(p1, "a2"); tick != 2 {
		t.Fatalf("scheduled tick = %d, want 2", tick)
	}
	e.Step()

	want := "t1 admin x|t1 player 1 a1|t1 player 2 b1|t1 player 2 b2|t2 player 1 a2"
	if got := strings.Join(log, "|"); got != want {
		t.Errorf("execution log\n got %s\nwant %s", got, want)
	}
	if len(f.Results) != 4 || f.Results[0].CommandID != "x" || !f.Results[0].OK {
		t.Errorf("results = %+v", f.Results)
	}
}

func TestCommandSubmitValidation(t *testing.T) {
	var log []string
	e := mustEngine(t, mustRules(t, recorder(&log)))
	if _, err := e.Submit(Command{Type: "nope"}); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown type: err = %v", err)
	}
	if _, err := e.Submit(Command{Type: CmdCreateEntity, Issuer: Principal{Player: 1}}); !errors.Is(err, ErrAdminOnly) {
		t.Errorf("admin-only: err = %v", err)
	}
	if _, err := e.Submit(Command{ID: "c", Type: "rec.Note", Issuer: Principal{Player: 1}, Payload: "bad"}); err != nil {
		t.Fatal(err)
	}
	f := e.Step()
	if len(f.Results) != 1 || f.Results[0].OK || f.Results[0].Code != CodeInvalidArgument {
		t.Errorf("results = %+v", f.Results)
	}
}

func TestConcurrentSubmitIsDeterministicPerTick(t *testing.T) {
	// However goroutines interleave, commands from distinct players in the
	// same tick execute in player order.
	for range 20 {
		var log []string
		e := mustEngine(t, mustRules(t, recorder(&log)))
		var wg sync.WaitGroup
		for p := 1; p <= 8; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = e.Submit(Command{Issuer: Principal{Player: PlayerID(p)}, Type: "rec.Note", Payload: fmt.Sprint(p)})
			}()
		}
		wg.Wait()
		e.Step()
		if got := strings.Join(log, "|"); !strings.HasPrefix(got, "t1 player 1 1|t1 player 2 2|") || len(log) != 8 {
			t.Fatalf("log = %s", got)
		}
	}
}

func TestCreateEntityCommand(t *testing.T) {
	e := mustEngine(t, mustRules(t))
	admin := Principal{Admin: true}
	_, _ = e.Submit(Command{ID: "ok", Issuer: admin, Type: CmdCreateEntity, Payload: CreateEntity{Kind: KindUnit, Owner: 1, Pos: Pos{1, 1}}})
	_, _ = e.Submit(Command{ID: "dup", Issuer: admin, Type: CmdCreateEntity, Payload: CreateEntity{Kind: KindUnit, Owner: 2, Pos: Pos{1, 1}}})
	_, _ = e.Submit(Command{ID: "oob", Issuer: admin, Type: CmdCreateEntity, Payload: CreateEntity{Kind: KindUnit, Owner: 2, Pos: Pos{10, 0}}})
	f := e.Step()
	if !f.Results[0].OK || f.Results[1].OK || f.Results[2].OK {
		t.Fatalf("results = %+v", f.Results)
	}
	if len(f.Changed) != 1 || f.Changed[0].Owner != 1 || f.Changed[0].Pos != (Pos{1, 1}) {
		t.Fatalf("changed = %+v", f.Changed)
	}
}

func TestWorldOccupancyAndMoves(t *testing.T) {
	e := mustEngine(t, mustRules(t))
	w := e.World()
	a, _ := w.CreateEntity(KindUnit, 1, Pos{0, 0})
	b, _ := w.CreateEntity(KindUnit, 1, Pos{1, 0})
	if _, err := w.CreateEntity(KindUnit, 2, Pos{0, 0}); !errors.Is(err, ErrOccupied) {
		t.Errorf("occupied create: %v", err)
	}
	if _, err := w.CreateEntity(KindUnit, 2, Pos{-1, 0}); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("oob create: %v", err)
	}
	// Into an occupied tile whose occupant is not moving: rejected, atomic.
	if err := w.MoveEntities([]Move{{a, Pos{0, 1}}, {b, Pos{0, 1}}}); err == nil {
		t.Error("two entities into one tile accepted")
	}
	if err := w.MoveEntities([]Move{{a, Pos{1, 0}}}); !errors.Is(err, ErrOccupied) {
		t.Errorf("into occupied: %v", err)
	}
	if got, _ := w.Entity(a); got.Pos != (Pos{0, 0}) {
		t.Errorf("failed move changed state: %v", got.Pos)
	}
	// A follows B, simultaneously: allowed.
	if err := w.MoveEntities([]Move{{a, Pos{1, 0}}, {b, Pos{2, 0}}}); err != nil {
		t.Fatal(err)
	}
	if id, _ := w.EntityAt(Pos{1, 0}); id != a {
		t.Errorf("EntityAt(1,0) = %d", id)
	}
	if _, ok := w.EntityAt(Pos{0, 0}); ok {
		t.Error("vacated tile still occupied")
	}
	if err := w.RemoveEntity(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.EntityAt(Pos{1, 0}); ok || w.EntityCount() != 1 {
		t.Error("remove did not clean up")
	}
	_, removed := w.takeChanges()
	if len(removed) != 1 || removed[0] != a {
		t.Errorf("removed = %v", removed)
	}
}

func TestComponentsAndChangeTracking(t *testing.T) {
	counter := NewComponent[int64]("rec.counter")
	p := testPlugin{name: "rec", register: func(r *Registrar) error {
		return r.Component(counter.Def(VisibleOwner, func(v int64) []Field {
			return []Field{{Key: "rec.counter", Value: IntValue(v)}}
		}))
	}}
	e := mustEngine(t, mustRules(t, p))
	w := e.World()
	a, _ := w.CreateEntity(KindUnit, 1, Pos{0, 0})
	b, _ := w.CreateEntity(KindUnit, 1, Pos{1, 0})
	counter.Set(w, b, 5)
	counter.Set(w, a, 3)
	if got := counter.Entities(w); len(got) != 2 || got[0] != min(a, b) || got[1] != max(a, b) {
		t.Errorf("Entities = %v, want sorted [%d %d]", got, min(a, b), max(a, b))
	}
	changed, _ := w.takeChanges()
	if len(changed) != 2 {
		t.Fatalf("changed = %+v", changed)
	}
	for _, v := range changed {
		want := map[EntityID]int64{a: 3, b: 5}[v.ID]
		if len(v.Attrs) != 1 || v.Attrs[0].Value.Int != want || v.Attrs[0].Visibility != VisibleOwner {
			t.Fatalf("view of %d = %+v", v.ID, v)
		}
	}
	counter.Set(w, a, 3) // unchanged value: not dirty
	if changed, _ := w.takeChanges(); len(changed) != 0 {
		t.Errorf("no-op set marked dirty: %+v", changed)
	}
}

func TestRandDeterministic(t *testing.T) {
	a, b := NewRand(42), NewRand(42)
	for range 1000 {
		if a.Intn(7) != b.Intn(7) {
			t.Fatal("diverged")
		}
	}
	// Pin the sequence so accidental algorithm changes are caught.
	if got := NewRand(0).Uint64(); got != 0xe220a8397b1dcdaf {
		t.Errorf("first value = %#x", got)
	}
}

func TestEntityIDsAreUnpredictable(t *testing.T) {
	newWorld := func(key []byte) *World {
		e, err := NewEngine(Config{Width: 100, Height: 100, IDKey: key}, mustRules(t))
		if err != nil {
			t.Fatal(err)
		}
		return e.World()
	}
	w := newWorld(testKey)
	var created []EntityID
	for i := range 1000 {
		id, err := w.CreateEntity(KindUnit, 1, Pos{int32(i % 100), int32(i / 100)})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, id)
	}
	ascending, small := 0, 0
	seen := map[EntityID]bool{}
	for i, id := range created {
		if id == 0 || seen[id] {
			t.Fatalf("id %d is zero or duplicated", id)
		}
		seen[id] = true
		if id < 1<<32 {
			small++
		}
		if i > 0 && id > created[i-1] {
			ascending++
		}
	}
	// Random 64-bit values: essentially none below 2^32, and consecutive
	// IDs increase about half the time (sequential IDs: always).
	if small > 2 || ascending < 400 || ascending > 600 {
		t.Errorf("IDs look structured: %d small, %d/999 ascending", small, ascending)
	}
	// The ID index stays sorted regardless of creation order.
	ids := w.EntityIDs()
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatal("EntityIDs not sorted")
		}
	}

	// Same key: identical sequence (replays work). Different key: unrelated.
	same, other := newWorld(testKey), newWorld([]byte("another-secret-key"))
	for i := range 10 {
		a, _ := same.CreateEntity(KindUnit, 1, Pos{int32(i), 0})
		b, _ := other.CreateEntity(KindUnit, 1, Pos{int32(i), 0})
		if a != created[i] {
			t.Fatalf("same key produced different id at %d", i)
		}
		if b == created[i] {
			t.Fatalf("different key produced same id at %d", i)
		}
	}
}

func TestEngineRequiresIDKey(t *testing.T) {
	if _, err := NewEngine(Config{Width: 1, Height: 1, IDKey: []byte("short")}, mustRules(t)); err == nil {
		t.Fatal("short key accepted")
	}
}
