package metadata_test

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	wellsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/wells/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/metadata"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

const store = `{"unit_capacity": 1000, "initial_energy": 0}`

func set(key, value string, units ...sim.EntityID) *metadatav1.SetMetadataCommand {
	m := &metadatav1.SetMetadataCommand{Key: key, Value: value}
	for _, u := range units {
		m.UnitIds = append(m.UnitIds, uint64(u))
	}
	return m
}

func eq(key, value string) *metadatav1.MetadataCondition {
	return &metadatav1.MetadataCondition{Key: key, Value: value, Match: metadatav1.MatchKind_MATCH_KIND_EQUALS}
}

func batch(t *testing.T, sel *metadatav1.Selector, tmpl proto.Message) *metadatav1.BatchCommand {
	t.Helper()
	a, err := anypb.New(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	return &metadatav1.BatchCommand{Selector: sel, Command: a}
}

func upgradeBy(attr string, by int64) *upgradesv1.UpgradeCommand {
	return &upgradesv1.UpgradeCommand{Attribute: attr, Amount: &upgradesv1.UpgradeCommand_Increase{Increase: by}}
}

func TestSetRemoveAndLimits(t *testing.T) {
	h := pt.New(t, map[string]string{"energy": store, "metadata": `{"max_entries": 3, "max_value_length": 8}`})
	u := h.Unit(1, 0, 0, 0)
	v := h.Unit(1, 1, 0, 0)
	theirs := h.Unit(2, 5, 5, 0)
	h.Step()
	ok := func(id string, f *sim.Frame) {
		t.Helper()
		if r, _ := pt.Result(f, id); !r.OK {
			t.Fatalf("%s: %+v", id, r)
		}
	}
	ok(h.Submit(1, metadata.CommandSet, set("squad", "alpha", u, v)), h.Step())
	ok(h.Submit(1, metadata.CommandSet, set("role", "miner", u)), h.Step())
	m := metadata.Get(h.W(), u)
	if s, _ := m.Get("squad"); s != "alpha" || m.N != 2 {
		t.Fatalf("metadata = %+v", m)
	}
	// Owner-visible attributes, sorted by key.
	h.Submit(1, metadata.CommandSet, set("squad", "bravo", u)) // overwrite
	f := h.Step()
	var keys []string
	for _, a := range f.Changed[0].Attrs {
		if strings.HasPrefix(a.Key, metadata.AttrPrefix) {
			keys = append(keys, a.Key+"="+a.Value.Str)
			if a.Visibility != sim.VisibleOwner {
				t.Fatal("metadata not owner-only")
			}
		}
	}
	if strings.Join(keys, ",") != "metadata.role=miner,metadata.squad=bravo" {
		t.Fatalf("observed = %v", keys)
	}

	cases := []struct {
		name string
		slot int
		typ  string
		cmd  proto.Message
		code string
	}{
		{"not yours", 1, metadata.CommandSet, set("squad", "x", theirs), sim.CodeNotFound},
		{"bad key", 1, metadata.CommandSet, set("Squad!", "x", u), sim.CodeInvalidArgument},
		{"long value", 1, metadata.CommandSet, set("k", "123456789", u), sim.CodeInvalidArgument},
		{"non-printable", 1, metadata.CommandSet, set("k", "a\x01", u), sim.CodeInvalidArgument},
		{"full", 1, metadata.CommandSet, set("k4", "x", u), sim.CodeFailedPrecondition}, // u has 2, +k3 below makes 3
		{"remove not yours", 1, metadata.CommandRemove, &metadatav1.RemoveMetadataCommand{UnitIds: []uint64{uint64(theirs)}, Key: "squad"}, sim.CodeNotFound},
	}
	ok(h.Submit(1, metadata.CommandSet, set("k3", "x", u)), h.Step())
	for _, c := range cases {
		id := h.Submit(c.slot, c.typ, c.cmd)
		if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != c.code {
			t.Errorf("%s: %+v, want %s", c.name, r, c.code)
		}
	}
	// Removal frees a slot; removing an absent key is fine.
	ok(h.Submit(1, metadata.CommandRemove, &metadatav1.RemoveMetadataCommand{UnitIds: []uint64{uint64(u), uint64(v)}, Key: "role"}), h.Step())
	ok(h.Submit(1, metadata.CommandSet, set("k4", "x", u)), h.Step())
	// Metadata never changes gameplay attributes, even with clashing names.
	ok(h.Submit(1, metadata.CommandSet, set("armor", "999", v)), h.Step())
	if h.Prop(v, "armor") != 0 {
		t.Fatal("metadata changed an attribute")
	}
}

func TestSelectorsAreDeterministic(t *testing.T) {
	h := pt.New(t, map[string]string{"energy": store})
	var mine []sim.EntityID
	for i := range 12 {
		u := h.Unit(1, int32(i), 0, 0)
		mine = append(mine, u)
		tag := []string{"alpha", "bravo", "alpha"}[i%3]
		h.Submit(1, metadata.CommandSet, set("squad", tag, u))
		if i%4 == 0 {
			h.Submit(1, metadata.CommandSet, set("role", "scout", u))
		}
	}
	other := h.Unit(2, 0, 5, 0)
	h.Submit(2, metadata.CommandSet, set("squad", "alpha", other))
	h.Step()
	sel := func(s *metadatav1.Selector) []sim.EntityID {
		t.Helper()
		ids, err := pluginOf(h).Select(h.W(), sim.Principal{Player: h.P(1)}, s)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	want := func(pred func(i int) bool) []sim.EntityID {
		var out []sim.EntityID
		for i, u := range mine {
			if pred(i) {
				out = append(out, u)
			}
		}
		return sorted(out)
	}
	check := func(name string, got, exp []sim.EntityID) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(exp) {
			t.Errorf("%s: got %v want %v", name, got, exp)
		}
	}
	check("squad=alpha", sel(&metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{eq("squad", "alpha")}}), want(func(i int) bool { return i%3 != 1 }))
	check("alpha and scout", sel(&metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{eq("squad", "alpha"), {Key: "role", Match: metadatav1.MatchKind_MATCH_KIND_PRESENT}}}), want(func(i int) bool { return i%3 != 1 && i%4 == 0 }))
	check("no role", sel(&metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{{Key: "role", Match: metadatav1.MatchKind_MATCH_KIND_ABSENT}}}), want(func(i int) bool { return i%4 != 0 }))
	all := sel(&metadatav1.Selector{})
	check("everything mine (never the opponent's)", all, want(func(int) bool { return true }))
	check("limit takes lowest IDs", sel(&metadatav1.Selector{Limit: 3}), all[:3])
	check("explicit ids", sel(&metadatav1.Selector{UnitIds: []uint64{uint64(mine[5]), uint64(other), uint64(mine[2])}}), sorted([]sim.EntityID{mine[5], mine[2]}))
	if _, err := pluginOf(h).Select(h.W(), sim.Principal{Player: h.P(1)}, &metadatav1.Selector{OwnerId: uint32(h.P(2))}); err == nil {
		t.Error("player used owner_id")
	}
	if _, err := pluginOf(h).Select(h.W(), sim.Principal{Player: h.P(1)}, &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{{Key: "squad"}}}); err == nil {
		t.Error("unspecified match accepted")
	}
	adminSel, _ := pluginOf(h).Select(h.W(), sim.Principal{Admin: true}, &metadatav1.Selector{OwnerId: uint32(h.P(2))})
	check("admin owner filter", adminSel, []sim.EntityID{other})
}

func TestBatchEvaluatedAtExecutionTick(t *testing.T) {
	h := pt.New(t, map[string]string{"energy": store})
	a := h.Unit(1, 0, 0, 500)
	b := h.Unit(1, 2, 0, 500)
	h.Step()
	// Tag a, then batch-upgrade "squad=x", in the same tick: the selector
	// runs after the earlier command and sees the tag.
	h.Submit(1, metadata.CommandSet, set("squad", "x", a))
	id := h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{eq("squad", "x")}}, upgradeBy("armor", 1)))
	f := h.Step()
	r, _ := pt.Result(f, id)
	if !r.OK || r.Partial || len(r.Units) != 1 || r.Units[0].Unit != a {
		t.Fatalf("result = %+v", r)
	}
	if _, busy := h.Attrs().Busy(h.W(), b); busy {
		t.Fatal("untagged unit upgraded")
	}
}

func TestBatchPartialSuccess(t *testing.T) {
	h := pt.New(t, map[string]string{"energy": store})
	for i := range 6 {
		energyAmount := int64(100)
		if i%2 == 1 {
			energyAmount = 5 // cannot afford armor +1 (15)
		}
		h.Unit(1, int32(i)*2, 0, energyAmount)
	}
	h.Step()
	id := h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{}, upgradeBy("armor", 1)))
	f := h.Step()
	r, _ := pt.Result(f, id)
	if !r.OK || !r.Partial || len(r.Units) != 6 {
		t.Fatalf("result = %+v", r)
	}
	okCount := 0
	for _, ur := range r.Units {
		if ur.OK {
			okCount++
			if h.Energy(ur.Unit) != 85 {
				t.Errorf("unit %d energy %d", ur.Unit, h.Energy(ur.Unit))
			}
		} else if ur.Code != energy.CodeInsufficientEnergy || h.Energy(ur.Unit) != 5 {
			t.Errorf("unit %+v energy %d", ur, h.Energy(ur.Unit))
		}
	}
	if okCount != 3 {
		t.Fatalf("%d succeeded", okCount)
	}
	// Unit results are in ascending ID order.
	for i := 1; i < len(r.Units); i++ {
		if r.Units[i-1].Unit >= r.Units[i].Unit {
			t.Fatal("unit results not ordered")
		}
	}
	// When nobody succeeds the batch fails, still listing every unit.
	id = h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{}, upgradeBy("armor", 20)))
	r, _ = pt.Result(h.Step(), id)
	if r.OK || len(r.Units) != 6 {
		t.Fatalf("all-fail result = %+v", r)
	}
	// Empty selection fails.
	id = h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{eq("squad", "none")}}, upgradeBy("armor", 1)))
	if r, _ := pt.Result(h.Step(), id); r.OK || r.Code != sim.CodeNotFound {
		t.Fatalf("empty selection = %+v", r)
	}
}

func TestBatchTemplates(t *testing.T) {
	h := pt.New(t, map[string]string{"energy": store, "cloning": `{"base_cost": 5, "cost": "base", "duration": "1", "offspring_energy": 0}`})
	a := h.Unit(1, 10, 10, 100)
	b := h.Unit(1, 20, 10, 100)
	h.Step()
	run := func(tmpl proto.Message) sim.CommandResult {
		t.Helper()
		id := h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{UnitIds: []uint64{uint64(a), uint64(b)}}, tmpl))
		r, _ := pt.Result(h.Step(), id)
		return r
	}
	if r := run(set("squad", "z")); !r.OK || r.Partial {
		t.Fatalf("set via batch: %+v", r)
	}
	if s, _ := metadata.Get(h.W(), b).Get("squad"); s != "z" {
		t.Fatal("batch set did not apply")
	}
	if r := run(&cloningv1.CloneCommand{}); !r.OK {
		t.Fatalf("clone via batch: %+v", r)
	}
	if len(h.W().EntityIDs()) != 4 {
		t.Fatalf("clones = %d entities", len(h.W().EntityIDs()))
	}
	if r := run(&movementv1.StopCommand{}); !r.OK {
		t.Fatalf("stop via batch: %+v", r)
	}
	// Commands without per-unit support, admin commands and nested batches
	// are rejected per unit.
	for _, tmpl := range []proto.Message{
		&movementv1.MoveCommand{},
		&wellsv1.CreateWellCommand{},
		batch(t, &metadatav1.Selector{}, set("a", "b")),
		&rtsv1.Position{},
	} {
		if r := run(tmpl); r.OK {
			t.Errorf("%T accepted as batch template", tmpl)
		}
	}
}

func TestCommandBudgetEnforcement(t *testing.T) {
	h := pt.NewWithBudget(t, 10, map[string]string{"energy": store})
	var units []sim.EntityID
	for i := range 15 {
		units = append(units, h.Unit(1, int32(i), 0, 100))
	}
	other := h.Unit(2, 0, 9, 100)
	h.Step()
	move := func(us []sim.EntityID) *movementv1.MoveCommand {
		m := &movementv1.MoveCommand{}
		for _, u := range us {
			e, _ := h.W().Entity(u)
			m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(u), Destination: &rtsv1.Position{X: e.Pos.X, Y: 20}})
		}
		return m
	}
	// 8 + 5 > 10: the second command fails atomically; the opponent has
	// its own budget.
	first := h.Submit(1, movement.CommandMove, move(units[:8]))
	second := h.Submit(1, movement.CommandMove, move(units[8:13]))
	theirs := h.Submit(2, movement.CommandMove, move([]sim.EntityID{other}))
	f := h.Step()
	r1, _ := pt.Result(f, first)
	r2, _ := pt.Result(f, second)
	r3, _ := pt.Result(f, theirs)
	if !r1.OK || r2.OK || r2.Code != sim.CodeResourceExhausted || !r3.OK {
		t.Fatalf("results %+v / %+v / %+v", r1, r2, r3)
	}
	// Budgets reset every tick.
	again := h.Submit(1, movement.CommandMove, move(units[8:13]))
	if r, _ := pt.Result(h.Step(), again); !r.OK {
		t.Fatalf("next tick: %+v", r)
	}
	// A batch over 15 units: the first 10 succeed, the rest are exhausted.
	id := h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{}, set("squad", "b")))
	r, _ := pt.Result(h.Step(), id)
	if !r.Partial || len(r.Units) != 15 {
		t.Fatalf("batch = %+v", r)
	}
	for i, ur := range r.Units {
		if (i < 10) != ur.OK || (!ur.OK && ur.Code != sim.CodeResourceExhausted) {
			t.Fatalf("unit %d: %+v", i, ur)
		}
	}
	// Invalid units do not consume budget: 5 foreign IDs + 10 valid.
	ids := []uint64{uint64(other)}
	for _, u := range units[:10] {
		ids = append(ids, uint64(u))
	}
	id = h.Submit(1, metadata.CommandSet, set("x", "y")) // invalid: no units, costs nothing
	id2 := h.Submit(1, metadata.CommandBatch, batch(t, &metadatav1.Selector{UnitIds: ids}, set("squad", "c")))
	f = h.Step()
	if r, _ := pt.Result(f, id); r.OK {
		t.Fatal("empty set accepted")
	}
	if r, _ := pt.Result(f, id2); !r.OK || r.Partial || len(r.Units) != 10 {
		t.Fatalf("batch after invalid command = %+v", r)
	}
	// Admins are never charged.
	adm := h.Submit(0, movement.CommandMove, move(units))
	if r, _ := pt.Result(h.Step(), adm); !r.OK {
		t.Fatalf("admin charged: %+v", r)
	}
}

func pluginOf(h *pt.Harness) *metadata.Plugin {
	for _, p := range pluginsOf(h) {
		if m, ok := p.(*metadata.Plugin); ok {
			return m
		}
	}
	h.T.Fatal("no metadata plugin")
	return nil
}

func sorted(ids []sim.EntityID) []sim.EntityID {
	out := append([]sim.EntityID(nil), ids...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
