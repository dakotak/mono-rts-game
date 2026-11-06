package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	logisticsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/logistics/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// Automation and supply over the GameService: group assignment through a
// metadata selector, owner-only status, admin-only work reports.
func TestAutomationAndSupplyOverGRPC(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) {
		c.Plugins = map[string]json.RawMessage{
			"energy":    json.RawMessage(`{"unit_capacity": 100, "initial_energy": 50}`),
			"logistics": json.RawMessage(`{"range": {"default": 4}}`),
		}
		c.Units = append(c.Units, game.UnitBlock{Owner: 1, X: 3, Y: 3}, game.UnitBlock{Owner: 1, X: 5, Y: 3})
	})
	red, blue := e.connect("red"), e.connect("blue")
	ro, bo := red.observe(), blue.observe()
	ao := e.observeAdmin()
	var squad []uint64
	for _, en := range ro.snap.GetEntities() {
		if en.GetOwnerId() == red.id && en.GetId() != e.redUnit {
			squad = append(squad, en.GetId())
		}
	}
	red.submit("tag", &metadatav1.SetMetadataCommand{UnitIds: squad, Key: "role", Value: "guard"})
	e.engine.Step()
	ro.next()
	bo.next()

	tmpl, _ := anypb.New(&automationv1.AutomateCommand{Behavior: &automationv1.AutomateCommand_Guard{Guard: &automationv1.Guard{Position: &rtsv1.Position{X: 4, Y: 3}, Radius: 3}}})
	red.submit("guard", &metadatav1.BatchCommand{
		Selector: &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{{Key: "role", Value: "guard", Match: metadatav1.MatchKind_MATCH_KIND_EQUALS}}},
		Command:  tmpl,
	})
	red.submit("supply", &logisticsv1.SupplyCommand{UnitIds: []uint64{e.redUnit}, ReceiverId: squad[0], Reserve: 10})
	blue.submit("steal", &logisticsv1.SupplyCommand{UnitIds: []uint64{e.blueUnit}, ReceiverId: e.redUnit})
	e.engine.Step()
	u := ro.next()
	if r := resultFor(u, "guard"); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED || len(r.GetUnitResults()) != 2 {
		t.Fatalf("guard batch = %v", r)
	}
	if r := resultFor(u, "supply"); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED {
		t.Fatalf("supply = %v", r)
	}
	bu := bo.next()
	if r := resultFor(bu, "steal"); r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_FAILED || r.GetErrorCode() != "not_found" {
		t.Fatalf("cross-player supply = %v", r)
	}
	for _, id := range squad {
		if b := find(u.GetChanged(), id).GetAttributes()["automation.behavior"].GetStringValue(); b != "guard" {
			t.Fatalf("owner sees behavior %q on %d", b, id)
		}
	}
	if to := find(u.GetChanged(), e.redUnit).GetAttributes()["logistics.supply.0.to"]; to == nil {
		t.Fatal("owner does not see the supply link")
	}
	// The opponent sees neither status attributes nor events.
	for _, en := range bu.GetChanged() {
		for k := range en.GetAttributes() {
			if en.GetOwnerId() == red.id && (strings.HasPrefix(k, "automation.") || strings.HasPrefix(k, "logistics.")) {
				t.Fatalf("opponent sees %s", k)
			}
		}
	}
	for _, ev := range bu.GetEvents() {
		if strings.HasPrefix(ev.GetType(), "automation.") || strings.HasPrefix(ev.GetType(), "logistics.") {
			t.Fatalf("opponent got %s", ev.GetType())
		}
	}
	// Admins get the per-tick work reports; players never do.
	ao.until(func(u *rtsv1.TickUpdate) bool {
		for _, ev := range u.GetEvents() {
			if ev.GetType() == "automation.work" {
				return true
			}
		}
		return false
	})
	for _, ev := range u.GetEvents() {
		if strings.HasSuffix(ev.GetType(), ".work") {
			t.Fatalf("player got %s", ev.GetType())
		}
	}

	// While automation and supply run, snapshot + updates stays equal to a
	// fresh snapshot (status attributes included).
	fresh := red.observe()
	state := fresh.snap.GetEntities()
	for range 20 {
		e.engine.Step()
		state = applyUpdate(state, fresh.next())
	}
	snap, err := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&rtsv1.WorldSnapshot{Tick: snap.GetSnapshot().GetTick(), Entities: state}, snap.GetSnapshot()) {
		t.Fatal("snapshot + updates diverged from a fresh snapshot under automation")
	}
}
