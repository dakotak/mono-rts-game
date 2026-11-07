package replay_test

import (
	"encoding/json"
	"math/rand/v2"
	"testing"

	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/replay"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func cfg() *game.Config {
	return &game.Config{
		World:      game.WorldConfig{Width: 40, Height: 30, TickRateHz: 10, RandomSeed: true},
		Players:    []game.PlayerConfig{{Slot: 1, Name: "A", JoinToken: "a"}, {Slot: 2, Name: "B", JoinToken: "b"}},
		AdminToken: "secret",
		Plugins: map[string]json.RawMessage{
			"terrain":  json.RawMessage(`{"types": {"rock": {"passable": false}}}`),
			"worldgen": json.RawMessage(`{"generator": "procedural", "spawns": {"layout": "corners"}, "start": {"units": 4}}`),
			"match":    json.RawMessage(`{"start": "all_ready", "victory": {"time_limit_ticks": 60}}`),
		},
	}
}

// play runs a match with random moves, recording it.
func play(t *testing.T) (*game.Game, *replay.Recorder) {
	g, err := game.New(cfg())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := replay.NewRecorder(g, "test-match", 10)
	if err != nil {
		t.Fatal(err)
	}
	e := g.Engine
	r := rand.New(rand.NewPCG(1, 1))
	submit := func(issuer sim.Principal, typ string, payload any) {
		if _, err := e.Submit(sim.Command{ID: typ + "-" + string(rune('a'+r.IntN(26))), Issuer: issuer, Type: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	for tick := 1; ; tick++ {
		switch tick {
		case 3:
			submit(sim.Principal{Player: g.Players[0].ID}, match.CommandReady, &matchv1.SetReadyCommand{Ready: true})
		case 5:
			submit(sim.Principal{Player: g.Players[1].ID}, match.CommandReady, &matchv1.SetReadyCommand{Ready: true})
		case 30:
			submit(sim.Principal{Admin: true}, sim.CmdCreateEntity, sim.CreateEntity{Kind: sim.KindUnit, Owner: g.Players[0].ID, Pos: sim.Pos{X: 20, Y: 15}})
		}
		if tick > 5 {
			for _, id := range e.World().EntityIDs() {
				if u, _ := e.World().Entity(id); u.Kind == sim.KindUnit && r.IntN(8) == 0 {
					submit(sim.Principal{Player: u.Owner}, movement.CommandMove, &movementv1.MoveCommand{Orders: []*movementv1.UnitDestination{
						{UnitId: uint64(id), Destination: &rtsv1.Position{X: int32(r.IntN(40)), Y: int32(r.IntN(30))}}}})
				}
			}
		}
		f := e.Step()
		done := match.Get(e.World()).State == match.Finished
		rec.Record(f, e.World(), done)
		if done {
			return g, rec
		}
	}
}

func TestRecordedMatchVerifies(t *testing.T) {
	g, rec := play(t)
	rp, err := rec.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if rp.Seed != g.Seed || rp.FinalTick == 0 || len(rp.Ticks) == 0 {
		t.Fatalf("replay header %+v", rp.Seed)
	}
	data, _ := json.Marshal(rp)
	parsed, err := replay.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	report, err := replay.Verify(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || !report.EngineMatches || !report.RulesetMatches || report.TicksReplayed != rp.FinalTick || report.CheckpointsChecked < 3 {
		t.Fatalf("report = %+v", report)
	}
	if report.FinalHash != rp.Checkpoints[len(rp.Checkpoints)-1].Hash {
		t.Fatal("final hash differs")
	}
	// Secrets are not exported.
	var c game.Config
	_ = json.Unmarshal(rp.Config, &c)
	if c.AdminToken != "" || c.Players[0].JoinToken == "a" || c.World.RandomSeed || c.World.Seed != g.Seed {
		t.Fatalf("config not resolved/redacted: %+v", c.World)
	}
}

func TestDivergenceIsLocated(t *testing.T) {
	_, rec := play(t)
	rp, _ := rec.Snapshot()
	// Tamper with a move command in the middle of the match.
	target := -1
	for i, tk := range rp.Ticks {
		if tk.Tick > 20 && tk.Commands[0].Type == movement.CommandMove {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("no move to tamper with")
	}
	c := &rp.Ticks[target].Commands[0]
	var m movementv1.MoveCommand
	p, _ := replay.DecodePayload(c.Type, c.Payload)
	m.Orders = p.(*movementv1.MoveCommand).Orders
	m.Orders[0].Destination = &rtsv1.Position{X: 0, Y: 0}
	c.Payload, _ = replay.EncodePayload(&m)
	report, err := replay.Verify(&rp)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.FirstDivergentTick < rp.Ticks[target].Tick || report.FirstDivergentTick > rp.Ticks[target].Tick+5 {
		t.Fatalf("report = %+v (tampered tick %d)", report, rp.Ticks[target].Tick)
	}
}

func TestRulesetMismatchReported(t *testing.T) {
	_, rec := play(t)
	rp, _ := rec.Snapshot()
	rp.EngineVersion = "mono-m0"
	report, err := replay.Verify(&rp)
	if err != nil {
		t.Fatal(err)
	}
	if report.EngineMatches || !report.OK {
		t.Fatalf("report = %+v", report)
	}
}
