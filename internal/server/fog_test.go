package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
)

// fogEnv: 12x8 world, red at (0,0), blue at (11,7), vision 3 (chebyshev),
// everything in sight is detected; identification estimated within 1 tile,
// detailed never.
func fogEnv(t *testing.T) *env {
	return newEnvWith(t, func(c *game.Config) {
		c.Plugins = map[string]json.RawMessage{
			"energy": json.RawMessage(`{"unit_capacity": 500, "initial_energy": 120}`),
			"combat": json.RawMessage(`{"attack": {"default": 5}, "range": {"default": 3}, "cooldown": {"default": 1}}`),
			"visibility": json.RawMessage(`{
				"enabled": true,
				"vision": {"default": 3}, "sensor": {"default": 10}, "identification": {"default": 1},
				"detailed_threshold": 50,
				"disclosure": [
					{"key": "combat.health", "basic": 50, "estimated": 10},
					{"key": "energy.stored", "estimated": 100}
				]
			}`),
		}
	})
}

func moveTo(unit uint64, x, y int32) *movementv1.MoveCommand {
	return &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{{UnitId: unit, Destination: &rtsv1.Position{X: x, Y: y}}}}
}

func countTiles(runs []*rtsv1.TileRun) int {
	n := 0
	for _, r := range runs {
		n += int(r.GetLength())
	}
	return n
}

func TestFogSnapshotsAndUpdates(t *testing.T) {
	e := fogEnv(t)
	red, blue := e.connect("red"), e.connect("blue")
	e.engine.Step() // first observation
	ro, bo := red.observe(), blue.observe()

	// Red sees only itself and its 4x4 corner of the map.
	if !ro.snap.GetFogOfWar() || len(ro.snap.GetEntities()) != 1 || ro.snap.GetEntities()[0].GetId() != e.redUnit {
		t.Fatalf("red snapshot = %v", ro.snap)
	}
	if got := countTiles(ro.snap.GetVisibleTiles()); got != 16 {
		t.Fatalf("red sees %d tiles", got)
	}
	if ro.snap.GetEntities()[0].GetIdentification() != rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_OWN {
		t.Fatal("own unit not marked OWN")
	}

	// Blue walks toward red with a private destination and metadata.
	blue.submit("tag", &metadatav1.SetMetadataCommand{UnitIds: []uint64{e.blueUnit}, Key: "plan", Value: "flank"})
	blue.submit("go", moveTo(e.blueUnit, 2, 1))
	var seen *rtsv1.Entity
	var tilesIn int
	for range 14 {
		e.engine.Step()
		u := ro.next()
		bo.next()
		tilesIn += len(u.GetTilesEntered())
		for _, en := range u.GetChanged() {
			if en.GetId() == e.blueUnit {
				seen = en
			}
		}
		for _, id := range u.GetRemoved() {
			if id == e.blueUnit {
				seen = nil
			}
		}
	}
	if seen == nil {
		t.Fatal("approaching blue unit never observed")
	}
	if tilesIn != 0 {
		t.Fatalf("red's vision changed without red moving: %d tiles", tilesIn)
	}
	// Disclosed: position, owner, kind, and an estimate of health only.
	for k := range seen.GetAttributes() {
		if k != "combat.health" && k != "energy.stored" {
			t.Fatalf("undisclosed attribute %q sent", k)
		}
	}
	if seen.GetOwnerId() != blue.id || seen.GetIdentification() == rtsv1.IdentificationLevel_IDENTIFICATION_LEVEL_OWN {
		t.Fatalf("observed blue = %v", seen)
	}
	if hv := seen.GetAttributes()["combat.health"]; hv.GetRangeValue() == nil {
		t.Fatalf("health should be an estimate, got %v", hv)
	}

	// Snapshot + updates equals a fresh snapshot (reconnect consistency).
	snap, _ := e.game.GetSnapshot(red.ctx, &rtsv1.GetSnapshotRequest{})
	again := red.observe()
	if !proto.Equal(snap.GetSnapshot(), again.snap) {
		t.Fatal("GetSnapshot and Observe snapshot differ")
	}
	if find(again.snap.GetEntities(), e.blueUnit) == nil {
		t.Fatal("reconnect snapshot lacks the currently detected enemy")
	}

	// Admin preview shows exactly red's view, plus why.
	pv, err := e.admin.GetPlayerView(authed(e.ctx, "admin"), &rtsv1.GetPlayerViewRequest{PlayerId: red.id})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(pv.GetSnapshot(), again.snap) {
		t.Fatal("admin preview differs from the player's snapshot")
	}
	if len(pv.GetDetections()) != 1 || pv.GetDetections()[0].GetObserverId() != e.redUnit {
		t.Fatalf("detections = %v", pv.GetDetections())
	}
	if _, err := e.admin.GetPlayerView(red.ctx, &rtsv1.GetPlayerViewRequest{PlayerId: red.id}); err == nil {
		t.Fatal("player used the admin preview")
	}
}

func TestFogHidesDestructionAndProbing(t *testing.T) {
	e := fogEnv(t)
	red := e.connect("red")
	e.engine.Step()
	ro := red.observe()
	// Blue's unit is destroyed far outside red's vision.
	if _, err := e.admin.SubmitAdminCommand(authed(e.ctx, "admin"), &rtsv1.SubmitAdminCommandRequest{Payload: mustAny(t, &combatv1.SetHealthCommand{UnitId: e.blueUnit, Health: 0})}); err != nil {
		t.Fatal(err)
	}
	e.engine.Step()
	u := ro.next()
	if len(u.GetRemoved()) != 0 || len(u.GetEvents()) != 0 || len(u.GetChanged()) != 0 {
		t.Fatalf("hidden destruction leaked: %v", u)
	}
	// Attacking the (now dead, previously hidden) unit and a made-up ID
	// produce indistinguishable results.
	red.submit("a1", &combatv1.AttackCommand{UnitIds: []uint64{e.redUnit}, TargetId: e.blueUnit})
	red.submit("a2", &combatv1.AttackCommand{UnitIds: []uint64{e.redUnit}, TargetId: 123456789})
	e.engine.Step()
	res := map[string]*rtsv1.CommandResult{}
	for _, r := range ro.next().GetCommandResults() {
		res[r.GetCommandId()] = r
	}
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return -1
			}
			return r
		}, s)
	}
	if res["a1"].GetErrorCode() != res["a2"].GetErrorCode() || norm(res["a1"].GetErrorMessage()) != norm(res["a2"].GetErrorMessage()) {
		t.Fatalf("probe distinguishable: %v vs %v", res["a1"], res["a2"])
	}
}
