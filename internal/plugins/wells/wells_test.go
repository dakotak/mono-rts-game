package wells_test

import (
	"encoding/json"
	"strings"
	"testing"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	wellsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/wells/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/wells"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func engine(t *testing.T, cfg string) *sim.Engine {
	t.Helper()
	var c map[string]json.RawMessage
	if cfg != "" {
		c = map[string]json.RawMessage{wells.Name: json.RawMessage(cfg)}
	}
	rs, err := sim.NewRuleset([]sim.Plugin{wells.New()}, c)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sim.NewEngine(sim.Config{Width: 10, Height: 10, IDKey: []byte("wells-test-key-0")}, rs)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func ptr[T any](v T) *T { return &v }

func TestInitialWellsFromConfig(t *testing.T) {
	e := engine(t, `{"defaults":{"energy":100,"capacity":200,"extraction_rate":3},
		"wells":[{"x":1,"y":1},{"x":5,"y":5,"unlimited":true,"extraction_rate":7},{"x":2,"y":2,"capacity":50}]}`)
	if err := e.Init(); err != nil {
		t.Fatal(err)
	}
	w := e.World()
	got := map[sim.Pos]wells.Well{}
	for _, id := range wells.IDs(w) {
		ent, _ := w.Entity(id)
		v, _ := wells.Get(w, id)
		if ent.Kind != sim.KindResource || ent.Owner != 0 {
			t.Fatalf("well entity = %+v", ent)
		}
		got[ent.Pos] = v
	}
	want := map[sim.Pos]wells.Well{
		{X: 1, Y: 1}: {Energy: 100, Capacity: 200, ExtractionRate: 3},
		{X: 5, Y: 5}: {Energy: 100, Capacity: 200, ExtractionRate: 7, Unlimited: true},
		{X: 2, Y: 2}: {Energy: 50, Capacity: 50, ExtractionRate: 3}, // energy clamped to new capacity
	}
	if len(got) != len(want) {
		t.Fatalf("wells = %v", got)
	}
	for p, wv := range want {
		if got[p] != wv {
			t.Errorf("well at %v = %+v, want %+v", p, got[p], wv)
		}
	}
	if err := e.Init(); err == nil {
		t.Error("second Init accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []string{
		`{"defaults":{"energy":10,"capacity":5}}`,
		`{"defaults":{"extraction_rate":-1}}`,
		`{"wells":[{"x":0,"y":0,"energy":-1}]}`,
		`{"bogus":true}`,
	} {
		if _, err := sim.NewRuleset([]sim.Plugin{wells.New()}, map[string]json.RawMessage{wells.Name: json.RawMessage(cfg)}); err == nil {
			t.Errorf("accepted %s", cfg)
		}
	}
	// Overlapping initial wells fail at Init.
	e := engine(t, `{"wells":[{"x":1,"y":1},{"x":1,"y":1}]}`)
	if err := e.Init(); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Errorf("overlap: %v", err)
	}
}

func TestCreateAndConfigureCommands(t *testing.T) {
	e := engine(t, "")
	admin := sim.Principal{Admin: true}
	submit := func(id string, payload any, typ string) {
		t.Helper()
		if _, err := e.Submit(sim.Command{ID: id, Issuer: admin, Type: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Submit(sim.Command{Issuer: sim.Principal{Player: 1}, Type: wells.CommandCreate, Payload: &wellsv1.CreateWellCommand{}}); err == nil {
		t.Fatal("player may create wells")
	}
	submit("ok", &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 3, Y: 3}, Capacity: ptr[int64](20)}, wells.CommandCreate)
	submit("dup", &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 3, Y: 3}}, wells.CommandCreate)
	submit("oob", &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 30, Y: 3}}, wells.CommandCreate)
	submit("neg", &wellsv1.CreateWellCommand{Position: &rtsv1.Position{X: 4, Y: 3}, ExtractionRate: ptr[int64](-2)}, wells.CommandCreate)
	submit("nopos", &wellsv1.CreateWellCommand{}, wells.CommandCreate)
	f := e.Step()
	codes := []string{}
	for _, r := range f.Results {
		codes = append(codes, r.Code)
	}
	if strings.Join(codes, ",") != ","+sim.CodeFailedPrecondition+","+sim.CodeInvalidArgument+","+sim.CodeInvalidArgument+","+sim.CodeInvalidArgument {
		t.Fatalf("result codes = %v", codes)
	}
	id := wells.IDs(e.World())[0]
	if v, _ := wells.Get(e.World(), id); v != (wells.Well{Energy: 20, Capacity: 20, ExtractionRate: 5}) {
		t.Fatalf("created = %+v", v)
	}

	submit("cfg", &wellsv1.ConfigureWellCommand{WellId: uint64(id), Energy: ptr[int64](7), Unlimited: ptr(true)}, wells.CommandConfigure)
	submit("missing", &wellsv1.ConfigureWellCommand{WellId: 12345, Energy: ptr[int64](1)}, wells.CommandConfigure)
	submit("over", &wellsv1.ConfigureWellCommand{WellId: uint64(id), Capacity: ptr[int64](5), Energy: ptr[int64](6)}, wells.CommandConfigure)
	f = e.Step()
	if !f.Results[0].OK || f.Results[1].Code != sim.CodeNotFound || f.Results[2].Code != sim.CodeInvalidArgument {
		t.Fatalf("results = %+v", f.Results)
	}
	if v, _ := wells.Get(e.World(), id); v != (wells.Well{Energy: 7, Capacity: 20, ExtractionRate: 5, Unlimited: true}) {
		t.Fatalf("configured = %+v", v)
	}
	// Attributes are public.
	for _, a := range f.Changed[0].Attrs {
		if a.Visibility != sim.VisiblePublic {
			t.Fatalf("attr %s not public", a.Key)
		}
	}
}
