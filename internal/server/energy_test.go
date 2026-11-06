package server_test

import (
	"encoding/json"
	"testing"

	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"google.golang.org/protobuf/proto"
)

// TestEnergySnapshotAndEventConsistency checks, from a player's point of
// view, that: the player sees its own energy but not the opponent's; mined
// and transferred events exactly account for the observed energy changes;
// and snapshot + updates equals a later snapshot.
func TestEnergySnapshotAndEventConsistency(t *testing.T) {
	e := newEnvWith(t, func(c *game.Config) {
		c.Plugins = map[string]json.RawMessage{
			"wells":  json.RawMessage(`{"wells":[{"x":1,"y":1,"energy":50,"capacity":50,"extraction_rate":3}]}`),
			"energy": json.RawMessage(`{"unit_capacity":100,"initial_energy":20,"extraction_rate":2,"transfer_rate":5}`),
		}
		c.Units = append(c.Units, game.UnitBlock{Owner: 1, X: 0, Y: 1}) // second red unit, adjacent to the first
	})
	red, blue := e.connect("red"), e.connect("blue")
	ro, bo := red.observe(), blue.observe()

	var well, red2 uint64
	for _, en := range ro.snap.GetEntities() {
		switch {
		case en.GetKind() == rtsv1.EntityKind_ENTITY_KIND_RESOURCE:
			well = en.GetId()
		case en.GetOwnerId() == red.id && en.GetId() != e.redUnit:
			red2 = en.GetId()
		}
	}
	energyOf := func(es []*rtsv1.Entity, id uint64) (int64, bool) {
		v := find(es, id).GetAttributes()[energy.AttrStored]
		return v.GetIntValue(), v != nil
	}
	if v, ok := energyOf(ro.snap.GetEntities(), e.redUnit); !ok || v != 20 {
		t.Fatalf("red sees own energy %v %v", v, ok)
	}
	if _, ok := energyOf(ro.snap.GetEntities(), e.blueUnit); ok {
		t.Fatal("red sees blue's energy")
	}
	if _, ok := energyOf(bo.snap.GetEntities(), e.redUnit); ok {
		t.Fatal("blue sees red's energy")
	}

	red.submit("mine", &energyv1.MineCommand{UnitIds: []uint64{e.redUnit, red2}, WellId: well})
	red.submit("give", &energyv1.TransferCommand{FromUnitId: e.redUnit, ToUnitId: red2, Amount: 7})

	state := ro.snap.GetEntities()
	start := map[uint64]int64{}
	for _, id := range []uint64{e.redUnit, red2} {
		start[id], _ = energyOf(state, id)
	}
	gained := map[uint64]int64{}
	wellStart := find(state, well).GetAttributes()["wells.energy"].GetIntValue()
	var minedTotal int64
	for range 30 {
		e.engine.Step()
		u := ro.next()
		bu := bo.next()
		for _, ev := range bu.GetEvents() {
			if ev.GetType() != "energy.well_depleted" {
				t.Fatalf("blue received red's private event %v", ev)
			}
		}
		state = applyUpdate(state, u)
		for _, ev := range u.GetEvents() {
			amount := ev.GetFields()["amount"].GetIntValue()
			switch ev.GetType() {
			case energy.EventMined:
				gained[ev.GetEntityId()] += amount
				minedTotal += amount
			case energy.EventTransferred:
				gained[ev.GetEntityId()] -= amount
				gained[red2] += amount
			}
		}
	}
	for _, id := range []uint64{e.redUnit, red2} {
		now, _ := energyOf(state, id)
		if now-start[id] != gained[id] {
			t.Errorf("unit %d: observed change %d, events say %d", id, now-start[id], gained[id])
		}
	}
	wellNow := find(state, well).GetAttributes()["wells.energy"].GetIntValue()
	if wellStart-wellNow != minedTotal || minedTotal == 0 {
		t.Errorf("well lost %d, mined events total %d", wellStart-wellNow, minedTotal)
	}
	snap, err := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(snap.GetSnapshot(), &rtsv1.WorldSnapshot{Tick: 30, Entities: state}) {
		t.Fatal("incremental state diverged from snapshot")
	}
}
