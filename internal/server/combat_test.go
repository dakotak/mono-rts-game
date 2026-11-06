package server_test

import (
	"encoding/json"
	"testing"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	salvagev1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/salvage/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// TestCombatOverGRPC checks combat through the public API, including what
// each player is (not) allowed to observe.
func TestCombatOverGRPC(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) {
		c.Plugins = map[string]json.RawMessage{
			"energy":  json.RawMessage(`{"unit_capacity": 500, "initial_energy": 100}`),
			"combat":  json.RawMessage(`{"attack": {"default": 15}, "max_health": {"default": 40}, "cooldown": {"default": 1}, "range": {"default": 12}, "energy_cost": "2"}`),
			"salvage": json.RawMessage(`{"stored_recovery_permille": 500}`),
		}
	})
	red, blue := e.connect("red"), e.connect("blue")
	ro, bo := red.observe(), blue.observe()

	// Health is public; attack stats are private.
	if find(bo.snap.GetEntities(), e.redUnit).GetAttributes()["combat.health"].GetIntValue() != 40 {
		t.Fatal("blue cannot see red's health")
	}
	if find(bo.snap.GetEntities(), e.redUnit).GetAttributes()["combat.attack"] != nil {
		t.Fatal("blue can see red's attack strength")
	}
	if find(ro.snap.GetEntities(), e.redUnit).GetAttributes()["combat.attack"].GetIntValue() != 15 {
		t.Fatal("red cannot see its own attack strength")
	}

	// Friendly fire is rejected at execution.
	if ack := red.submit("ff", &combatv1.AttackCommand{UnitIds: []uint64{e.redUnit}, TargetId: e.redUnit}); !ack.GetAccepted() {
		t.Fatal("envelope rejected")
	}
	red.submit("atk", &combatv1.AttackCommand{UnitIds: []uint64{e.redUnit}, TargetId: e.blueUnit})
	e.engine.Step()
	ru, bu := ro.next(), bo.next()
	res := map[string]*rtsv1.CommandResult{}
	for _, r := range ru.GetCommandResults() {
		res[r.GetCommandId()] = r
	}
	if res["ff"].GetErrorCode() != "permission_denied" || res["atk"].GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED {
		t.Fatalf("results = %v", res)
	}
	// Red sees its attack event; blue sees being damaged; neither sees the
	// other's event.
	types := func(u *rtsv1.TickUpdate) map[string]int {
		m := map[string]int{}
		for _, ev := range u.GetEvents() {
			m[ev.GetType()]++
		}
		return m
	}
	if rt, bt := types(ru), types(bu); rt["combat.attack"] != 1 || rt["combat.damaged"] != 0 || bt["combat.damaged"] != 1 || bt["combat.attack"] != 0 {
		t.Fatalf("red events %v, blue events %v", rt, bt)
	}
	if find(bu.GetChanged(), e.blueUnit).GetAttributes()["combat.health"].GetIntValue() != 25 {
		t.Fatal("blue health not updated")
	}
	// The attack order is visible to its owner only.
	if find(ru.GetChanged(), e.redUnit).GetAttributes()["combat.target"] == nil {
		t.Fatal("red cannot see its attack order")
	}
	if en := find(bu.GetChanged(), e.redUnit); en != nil && en.GetAttributes()["combat.target"] != nil {
		t.Fatal("blue can see red's attack order")
	}

	// Two more hits destroy blue's unit; both players learn of it and see
	// the wreck appear (with half of blue's stored energy).
	var wreck *rtsv1.Entity
	destroyedFor := map[string]bool{}
	for range 2 {
		e.engine.Step()
		ru, bu = ro.next(), bo.next()
		for who, u := range map[string]*rtsv1.TickUpdate{"red": ru, "blue": bu} {
			for _, ev := range u.GetEvents() {
				if ev.GetType() == "combat.destroyed" {
					destroyedFor[who] = true
				}
			}
			for _, en := range u.GetChanged() {
				if en.GetAttributes()["salvage.energy"] != nil {
					wreck = en
				}
			}
		}
	}
	if !destroyedFor["red"] || !destroyedFor["blue"] {
		t.Fatalf("destroyed events delivered to %v", destroyedFor)
	}
	if wreck == nil || !wreck.GetPassable() || wreck.GetAttributes()["salvage.energy"].GetIntValue() != 50 {
		t.Fatalf("wreck = %v", wreck)
	}

	// Red orders salvage of the enemy wreck from a distance (the order
	// waits), then the admin moves red next to it.
	red.submit("salv", &salvagev1.SalvageCommand{UnitIds: []uint64{e.redUnit}, WreckId: wreck.GetId()})
	if _, err := e.admin.MoveEntity(authed(e.ctx, "admin"), &rtsv1.MoveEntityRequest{EntityId: e.redUnit, Position: &rtsv1.Position{X: wreck.GetPosition().GetX() - 1, Y: wreck.GetPosition().GetY()}}); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		e.engine.Step()
		ro.next()
		bo.next()
	}
	snap, _ := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if find(snap.GetSnapshot().GetEntities(), wreck.GetId()) != nil {
		t.Fatal("wreck not emptied")
	}
	// 100 - 3 attacks x 2 + 50 salvaged.
	if got := find(snap.GetSnapshot().GetEntities(), e.redUnit).GetAttributes()["energy.stored"].GetIntValue(); got != 144 {
		t.Fatalf("red energy = %d, want 144", got)
	}
}
