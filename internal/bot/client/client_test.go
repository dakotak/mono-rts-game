package client

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// harness is a real server reachable over an in-memory network whose
// transport can be killed and restarted.
type harness struct {
	t   *testing.T
	g   *game.Game
	srv *server.Server
	lis atomic.Pointer[bufconn.Listener]
	gs  *grpc.Server
}

func newHarness(t *testing.T) *harness {
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, g: g, srv: server.New(cfg, g)}
	h.start()
	t.Cleanup(func() { h.gs.Stop() })
	return h
}

func (h *harness) start() {
	lis := bufconn.Listen(1 << 20)
	h.lis.Store(lis)
	h.gs = grpc.NewServer()
	h.srv.Register(h.gs)
	go func() { _ = h.gs.Serve(lis) }()
}

func (h *harness) conn() *grpc.ClientConn {
	c, err := grpc.NewClient("passthrough:///test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return h.lis.Load().DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	return c
}

func (h *harness) player(slot int) sim.PlayerID {
	for _, p := range h.g.Players {
		if p.Slot == slot {
			return p.ID
		}
	}
	h.t.Fatalf("no slot %d", slot)
	return 0
}

func runBot(t *testing.T, h *harness, token string, ticks chan uint64) (*Runner, chan error) {
	p, _ := difficulty.Level(8)
	pers, _ := difficulty.PersonalityByName("balanced")
	r := New(h.conn(), Options{
		Token: token,
		NewBot: func(k *state.Knowledge) *ai.Bot {
			return ai.New(k, ai.Config{Difficulty: p, Personality: pers, Seed: 1, Nonce: "c"})
		},
		OnTick:  func(tk uint64) { ticks <- tk },
		Backoff: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return r, done
}

func waitTick(t *testing.T, ticks chan uint64, want uint64) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case tk := <-ticks:
			if tk >= want {
				return
			}
		case <-deadline:
			t.Fatalf("bot never processed tick %d", want)
		}
	}
}

// TestFogCompliance checks that everything the bot knows about foreign
// entities right now is exactly what the server shows that player: the
// computer player has no hidden-world access.
func TestFogCompliance(t *testing.T) {
	h := newHarness(t)
	ticks := make(chan uint64, 16)
	r, _ := runBot(t, h, "duel-red", ticks)
	other := make(chan uint64, 16)
	runBot(t, h, "duel-blue", other)
	waitTick(t, ticks, 0)
	waitTick(t, other, 0)
	me := h.player(1)
	checked := 0
	for range 1500 {
		f := h.srv.Engine().Step()
		waitTick(t, ticks, uint64(f.Tick))
		waitTick(t, other, uint64(f.Tick))
		view, _ := h.srv.Hub().PlayerView(me)
		visible := map[uint64]bool{}
		for _, e := range view.GetEntities() {
			visible[e.GetId()] = true
		}
		for id, e := range r.K.Foreign {
			if !visible[id] {
				t.Fatalf("tick %d: bot knows foreign entity %d (%v) that the player cannot observe", f.Tick, id, e.Pos)
			}
			checked++
		}
		for id := range r.K.Own {
			if !visible[id] {
				t.Fatalf("tick %d: own entity %d unknown to the server view", f.Tick, id)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no foreign entity was ever observed (test is vacuous)")
	}
	if len(r.K.RememberedEnemies()) == 0 && len(r.K.Enemies()) == 0 {
		t.Log("note: no enemy seen in 1500 ticks")
	}
}

// TestReconnect kills the transport mid-match and restarts it: the bot
// reconnects (its session survives on the server) and keeps playing.
func TestReconnect(t *testing.T) {
	h := newHarness(t)
	ticks := make(chan uint64, 16)
	r, _ := runBot(t, h, "duel-red", ticks)
	other := make(chan uint64, 1024)
	runBot(t, h, "duel-blue", other) // duel starts when both are ready
	go func() {
		for range other {
		}
	}()
	waitTick(t, ticks, 0)
	for range 50 {
		f := h.srv.Engine().Step()
		waitTick(t, ticks, uint64(f.Tick))
	}
	before := r.Bot.Status.Commands.Submitted
	h.gs.Stop() // the network goes away
	h.start()   // and comes back
	var last uint64
	for range 100 {
		f := h.srv.Engine().Step()
		last = uint64(f.Tick)
	}
	waitTick(t, ticks, last)
	decisions := r.Bot.Status.Decisions
	for range 600 {
		f := h.srv.Engine().Step()
		waitTick(t, ticks, uint64(f.Tick))
	}
	if r.Bot.Status.Decisions < decisions+600 {
		t.Fatalf("decisions %d -> %d over 600 ticks", decisions, r.Bot.Status.Decisions)
	}
	if r.Reconnects == 0 {
		t.Fatal("no reconnection recorded")
	}
	if st := r.Bot.Status.Commands; st.Rejected > 0 {
		t.Fatalf("%d submissions rejected around the reconnection (%d before)", st.Rejected, before)
	}
	if r.K.Tick < last+600 {
		t.Fatalf("knowledge at tick %d, server at %d", r.K.Tick, last+600)
	}
}

func TestBadTokenStops(t *testing.T) {
	h := newHarness(t)
	_, done := runBot(t, h, "nope", make(chan uint64, 1))
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("err = %v, want Unauthenticated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bot kept retrying a rejected join token")
	}
}
