package sim

import (
	"strings"
	"testing"
)

type gateFn struct {
	allow func(w *World, cmd Command) error
	run   func(w *World) bool
}

func (g gateFn) AllowCommand(w *World, cmd Command) error { return g.allow(w, cmd) }
func (g gateFn) RunSystems(w *World) bool                 { return g.run(w) }

type counter struct{ N int }

func TestGlobalsAreHashedReportedAndValidated(t *testing.T) {
	g := NewGlobal("p.count", counter{})
	rs := mustRules(t, testPlugin{name: "p", register: func(r *Registrar) error {
		if err := r.Global(g.Def()); err != nil {
			return err
		}
		if err := r.Phase(PhaseDef{Name: "p"}); err != nil {
			return err
		}
		return r.System(SystemDef{Name: "p.inc", Phase: "p", Run: func(ctx *TickContext) {
			if ctx.Tick%2 == 0 {
				c := g.Get(ctx.World)
				c.N++
				g.Set(ctx.World, c)
			}
		}})
	}})
	e := mustEngine(t, rs)
	if f1 := e.Step(); f1.Globals != nil { // no global changed on tick 1
		t.Fatalf("tick 1 globals = %v", f1.Globals)
	}
	h1 := e.World().StateHash()
	f2 := e.Step()
	if got, ok := f2.Globals["p.count"].(counter); !ok || got.N != 1 {
		t.Fatalf("tick 2 globals = %v", f2.Globals)
	}
	if e.World().Globals()["p.count"].(counter).N != 1 || e.World().StateHash() == h1 {
		t.Fatal("global not stored or not hashed")
	}

	if _, err := NewRuleset([]Plugin{testPlugin{name: "p", register: func(r *Registrar) error {
		return r.Global(NewGlobal("q.x", 0).Def())
	}}}, nil); err == nil || !strings.Contains(err.Error(), "prefixed") {
		t.Fatalf("unprefixed global: %v", err)
	}
}

func TestGateBlocksCommandsAndSystems(t *testing.T) {
	open := NewGlobal("p.open", false)
	ran := 0
	rs := mustRules(t, testPlugin{name: "p", register: func(r *Registrar) error {
		_ = r.Global(open.Def())
		_ = r.Phase(PhaseDef{Name: "p"})
		_ = r.System(SystemDef{Name: "p.s", Phase: "p", Run: func(*TickContext) { ran++ }})
		_ = r.Command(CommandDef{Type: "p.open", Execute: func(ctx *TickContext, cmd Command) error {
			open.Set(ctx.World, true)
			return nil
		}})
		_ = r.Command(CommandDef{Type: "p.play", Execute: func(*TickContext, Command) error { return nil }})
		return r.Gate(gateFn{
			allow: func(w *World, cmd Command) error {
				if cmd.Type == "p.play" && !open.Get(w) {
					return Fail(CodeFailedPrecondition, "closed")
				}
				return nil
			},
			run: func(w *World) bool { return open.Get(w) },
		})
	}})
	e := mustEngine(t, rs)
	_, _ = e.Submit(Command{ID: "a", Issuer: Principal{Player: 1}, Type: "p.play"})
	f := e.Step()
	if f.Results[0].OK || ran != 0 {
		t.Fatalf("gate did not block: %+v ran=%d", f.Results[0], ran)
	}
	_, _ = e.Submit(Command{ID: "b", Issuer: Principal{Admin: true}, Type: "p.open"})
	_, _ = e.Submit(Command{ID: "c", Issuer: Principal{Player: 1}, Type: "p.play"})
	f = e.Step()
	if !f.Results[0].OK || !f.Results[1].OK || ran != 1 {
		t.Fatalf("gate did not open: %+v ran=%d", f.Results, ran)
	}
	if _, err := NewRuleset([]Plugin{
		testPlugin{name: "a", register: func(r *Registrar) error { return r.Gate(gateFn{}) }},
		testPlugin{name: "b", register: func(r *Registrar) error { return r.Gate(gateFn{}) }},
	}, nil); err == nil {
		t.Fatal("two gates accepted")
	}
}

func TestTickContextEvents(t *testing.T) {
	var seen int
	rs := mustRules(t, testPlugin{name: "p", register: func(r *Registrar) error {
		_ = r.Phase(PhaseDef{Name: "a"})
		_ = r.Phase(PhaseDef{Name: "b", After: []string{"a"}})
		_ = r.System(SystemDef{Name: "p.emit", Phase: "a", Run: func(ctx *TickContext) { ctx.Emit(Event{Type: "p.x"}) }})
		return r.System(SystemDef{Name: "p.read", Phase: "b", Run: func(ctx *TickContext) { seen = len(ctx.Events()) }})
	}})
	mustEngine(t, rs).Step()
	if seen != 1 {
		t.Fatalf("later system saw %d events", seen)
	}
}
