package game_test

import (
	"fmt"
	"math/rand/v2"
	"testing"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func testConfig() *game.Config {
	return &game.Config{
		World:   game.WorldConfig{Width: 40, Height: 30, Seed: 99, TickRateHz: 10, IDKey: "00112233445566778899aabbccddeeff"},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "a"}, {Slot: 2, JoinToken: "b"}, {Slot: 3, JoinToken: "c"}},
		Units: []game.UnitBlock{
			{Owner: 1, X: 1, Y: 1, W: 6, H: 6},
			{Owner: 2, X: 30, Y: 20, W: 6, H: 6},
			{Owner: 3, X: 15, Y: 10, W: 8, H: 8},
		},
	}
}

// scriptedRun drives an engine for n ticks, submitting pseudo-random move
// commands (including invalid ones) from the given script seed, and returns
// the per-tick state hashes and the recorded command log.
func scriptedRun(t testing.TB, scriptSeed uint64, n int) ([]uint64, [][]sim.Command) {
	t.Helper()
	g, err := game.New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	e := g.Engine
	// The script generator lives outside the simulation; its randomness
	// stands in for nondeterministic players.
	r := rand.New(rand.NewPCG(scriptSeed, 0))
	ids := e.World().EntityIDs()
	var hashes []uint64
	var log [][]sim.Command
	for tick := range n {
		for c := range r.IntN(4) {
			player := g.Players[r.IntN(len(g.Players))].ID
			m := &movementv1.MoveCommand{}
			for range 1 + r.IntN(10) {
				m.Orders = append(m.Orders, &movementv1.UnitDestination{
					UnitId:      uint64(ids[r.IntN(len(ids))]),
					Destination: &rtsv1.Position{X: int32(r.IntN(42)) - 1, Y: int32(r.IntN(32)) - 1},
				})
			}
			_, err := e.Submit(sim.Command{ID: fmt.Sprintf("%d-%d", tick, c), Issuer: sim.Principal{Player: player}, Type: movement.CommandMove, Payload: m})
			if err != nil {
				t.Fatal(err)
			}
		}
		f := e.Step()
		log = append(log, f.Commands)
		hashes = append(hashes, e.World().StateHash())
	}
	return hashes, log
}

func TestDeterministicAcrossRuns(t *testing.T) {
	const ticks = 400
	h1, _ := scriptedRun(t, 1, ticks)
	h2, _ := scriptedRun(t, 1, ticks)
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("runs diverged at tick %d", i+1)
		}
	}
	h3, _ := scriptedRun(t, 2, ticks)
	if h1[ticks-1] == h3[ticks-1] {
		t.Fatal("different inputs produced identical state; hash is not sensitive")
	}
}

func TestReplayFromRecordedCommands(t *testing.T) {
	const ticks = 400
	want, log := scriptedRun(t, 3, ticks)

	rg, err := game.New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	replay := rg.Engine
	for i, cmds := range log {
		replay.StepWith(cmds)
		if got := replay.World().StateHash(); got != want[i] {
			t.Fatalf("replay diverged at tick %d", i+1)
		}
	}
}

func TestReplayIgnoresSubmissionOrderWithinTick(t *testing.T) {
	_, log := scriptedRun(t, 4, 200)
	run := func(reverse bool) uint64 {
		g, err := game.New(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		e := g.Engine
		for _, cmds := range log {
			c := append([]sim.Command(nil), cmds...)
			if reverse {
				for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
					c[i], c[j] = c[j], c[i]
				}
			}
			e.StepWith(c)
		}
		return e.World().StateHash()
	}
	if run(false) != run(true) {
		t.Fatal("arrival order within a tick changed the outcome")
	}
}

func TestOccupancyInvariantHolds(t *testing.T) {
	g, err := game.New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	e := g.Engine
	ids := e.World().EntityIDs()
	r := rand.New(rand.NewPCG(5, 5))
	for range 300 {
		if r.IntN(3) == 0 {
			m := &movementv1.MoveCommand{}
			for _, id := range ids {
				m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(id), Destination: &rtsv1.Position{X: int32(r.IntN(40)), Y: int32(r.IntN(30))}})
			}
			_, _ = e.Submit(sim.Command{ID: "x", Issuer: sim.Principal{Admin: true}, Type: movement.CommandMove, Payload: m})
		}
		e.Step()
		seen := map[sim.Pos]bool{}
		for _, id := range e.World().EntityIDs() {
			ent, _ := e.World().Entity(id)
			if seen[ent.Pos] || !e.World().InBounds(ent.Pos) {
				t.Fatalf("tick %d: invalid position %v", e.World().Tick(), ent.Pos)
			}
			seen[ent.Pos] = true
			if at, _ := e.World().EntityAt(ent.Pos); at != id {
				t.Fatalf("occupancy index out of sync for %d", id)
			}
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []func(c *game.Config){
		func(c *game.Config) { c.World.Width = 0 },
		func(c *game.Config) { c.World.TickRateHz = 0 },
		func(c *game.Config) { c.Players[1].Slot = 1 },
		func(c *game.Config) { c.Players[1].Slot = 0 },
		func(c *game.Config) { c.World.IDKey = "abcd" },
		func(c *game.Config) { c.World.IDKey = "not hex at all, not hex at all!!" },
		func(c *game.Config) { c.Players[1].JoinToken = "a" },
		func(c *game.Config) { c.AdminToken = "a" },
		func(c *game.Config) { c.Units[0].Owner = 9 },
	}
	for i, mutate := range bad {
		c := testConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	c := testConfig()
	c.Units = append(c.Units, game.UnitBlock{Owner: 1, X: 1, Y: 1})
	if _, err := game.New(c); err == nil {
		t.Error("overlapping units accepted")
	}
}

func TestPlayerIDsAreOpaque(t *testing.T) {
	g, err := game.New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[sim.PlayerID]bool{}
	for _, p := range g.Players {
		if p.ID == 0 || seen[p.ID] || sim.PlayerID(p.Slot) == p.ID {
			t.Fatalf("player %+v: id is zero, duplicated or equals its slot", p)
		}
		seen[p.ID] = true
	}
	// Units are owned by the derived IDs, not by slot numbers.
	for _, id := range g.Engine.World().EntityIDs() {
		e, _ := g.Engine.World().Entity(id)
		if !seen[e.Owner] {
			t.Fatalf("unit %d owned by %d, not a player id", id, e.Owner)
		}
	}
	// Deterministic with a fixed key...
	g2, _ := game.New(testConfig())
	if g2.Players[0].ID != g.Players[0].ID || g2.Engine.World().EntityIDs()[0] != g.Engine.World().EntityIDs()[0] {
		t.Fatal("same key, different ids")
	}
	// ...and random per process without one.
	c := testConfig()
	c.World.IDKey = ""
	g3, _ := game.New(c)
	g4, _ := game.New(c)
	if g3.Players[0].ID == g4.Players[0].ID {
		t.Fatal("generated keys produced identical player ids")
	}
}
