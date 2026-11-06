package server_test

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	attributesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/attributes/v1"
	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
)

// TestEvolutionOverGRPC exercises Milestone 3 through the public API.
func TestEvolutionOverGRPC(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) {
		c.Plugins = map[string]json.RawMessage{
			"attributes": json.RawMessage(plugintest.Rules),
			"energy":     json.RawMessage(`{"unit_capacity": 1000, "initial_energy": 100}`),
			"cloning":    json.RawMessage(`{"base_cost": 10, "cost": "base + embodied", "duration": "2", "offspring_energy": 5}`),
		}
		c.Units = append(c.Units, game.UnitBlock{Owner: 1, X: 3, Y: 3}) // second red unit
		c.World.UnitCommandBudget = 100
	})
	red, blue := e.connect("red"), e.connect("blue")

	// Rules are published to players, including cost tables.
	resp, err := e.game.Connect(e.ctx, &rtsv1.ConnectRequest{JoinToken: "red"})
	if err != nil {
		t.Fatal(err)
	}
	var rules map[string]json.RawMessage
	if err := json.Unmarshal([]byte(resp.GetWorld().GetRulesJson()), &rules); err != nil {
		t.Fatal(err)
	}
	var attrRules struct {
		Attributes []struct {
			Name      string  `json:"name"`
			CostTable []int64 `json:"cost_table"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(rules["attributes"], &attrRules); err != nil || len(attrRules.Attributes) != 4 || attrRules.Attributes[0].CostTable[3] != 75 {
		t.Fatalf("attribute rules = %+v (%v)", attrRules, err)
	}
	if len(rules["cloning"]) == 0 || resp.GetWorld().GetUnitCommandBudget() != 100 || len(resp.GetWorld().GetSchedule()) == 0 {
		t.Fatal("rules, budget or schedule missing")
	}

	ro, bo := red.observe(), blue.observe()
	var red2 uint64
	for _, en := range ro.snap.GetEntities() {
		if en.GetOwnerId() == red.id && en.GetId() != e.redUnit {
			red2 = en.GetId()
		}
	}
	// Attributes are visible to the owner only.
	if find(ro.snap.GetEntities(), e.redUnit).GetAttributes()["attributes.mass"] == nil {
		t.Fatal("owner cannot see attributes")
	}
	if find(bo.snap.GetEntities(), e.redUnit).GetAttributes()["attributes.mass"] != nil {
		t.Fatal("opponent can see attributes")
	}

	// Tag one unit, then batch-upgrade everything tagged — plus a unit
	// that cannot afford it, by explicit selection of both.
	red.submit("tag", &metadatav1.SetMetadataCommand{UnitIds: []uint64{e.redUnit}, Key: "squad", Value: "alpha"})
	tmpl, _ := anypb.New(&upgradesv1.UpgradeCommand{Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 4}}) // costs 120 > 100
	red.submit("b1", &metadatav1.BatchCommand{
		Selector: &metadatav1.Selector{Conditions: []*metadatav1.MetadataCondition{{Key: "squad", Value: "alpha", Match: metadatav1.MatchKind_MATCH_KIND_EQUALS}}},
		Command:  tmpl,
	})
	tmpl2, _ := anypb.New(&upgradesv1.UpgradeCommand{Attribute: "armor", Amount: &upgradesv1.UpgradeCommand_Increase{Increase: 1}}) // costs 15
	if ack, err := e.admin.SubmitAdminCommand(authed(e.ctx, "admin"), &rtsv1.SubmitAdminCommandRequest{Payload: mustAny(t, &attributesv1.SetAttributeCommand{UnitId: red2, Attribute: "armor", Value: 20})}); err != nil || !ack.GetAccepted() {
		t.Fatalf("admin set: %v %v", ack, err)
	}
	red.submit("b2", &metadatav1.BatchCommand{Selector: &metadatav1.Selector{UnitIds: []uint64{e.redUnit, red2}}, Command: tmpl2})
	e.engine.Step()
	u := ro.next()
	results := map[string]*rtsv1.CommandResult{}
	for _, r := range u.GetCommandResults() {
		results[r.GetCommandId()] = r
	}
	if r := results["b1"]; r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_FAILED || len(r.GetUnitResults()) != 1 || r.GetUnitResults()[0].GetErrorCode() != "insufficient_energy" {
		t.Fatalf("b1 = %v", r)
	}
	// red2 is already at max armor (admin), the tagged unit can afford +1.
	r := results["b2"]
	if r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_PARTIAL || len(r.GetUnitResults()) != 2 {
		t.Fatalf("b2 = %v", r)
	}
	if find(u.GetChanged(), e.redUnit).GetAttributes()["metadata.squad"].GetStringValue() != "alpha" {
		t.Fatal("owner does not see metadata")
	}
	bu := bo.next()
	if en := find(bu.GetChanged(), e.redUnit); en != nil && en.GetAttributes()["metadata.squad"] != nil {
		t.Fatal("opponent sees metadata")
	}
	for _, ev := range bu.GetEvents() {
		t.Fatalf("opponent received %v", ev)
	}

	// Cloning through the API. red2 (armor 20 via admin) embodies 2200
	// energy, so cloning it is unaffordable; the upgraded unit (embodied 15)
	// can clone once its 4-tick upgrade has finished.
	for range 3 {
		e.engine.Step()
		ro.next()
		bo.next()
	}
	red.submit("expensive", &cloningv1.CloneCommand{UnitIds: []uint64{red2}})
	red.submit("clone", &cloningv1.CloneCommand{UnitIds: []uint64{e.redUnit}})
	results = map[string]*rtsv1.CommandResult{}
	for range 2 {
		e.engine.Step()
		for _, r := range ro.next().GetCommandResults() {
			results[r.GetCommandId()] = r
		}
		bo.next()
	}
	if results["expensive"].GetErrorCode() != "insufficient_energy" || results["clone"].GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED {
		t.Fatalf("clone results = %v", results)
	}
	snap, _ := e.game.GetSnapshot(blue.ctx, &rtsv1.GetSnapshotRequest{})
	reds := 0
	for _, en := range snap.GetSnapshot().GetEntities() {
		if en.GetOwnerId() == red.id {
			reds++
		}
	}
	if reds != 3 {
		t.Fatalf("blue sees %d red units, want 3", reds)
	}
}

func mustAny(t *testing.T, m proto.Message) *anypb.Any {
	t.Helper()
	a, err := anypb.New(m)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
