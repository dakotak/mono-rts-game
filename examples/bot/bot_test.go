package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
)

// TestBotAgainstInProcessServer runs the bot against a real server over an
// in-memory connection: a small world with one well near the bot's units.
func TestBotAgainstInProcessServer(t *testing.T) {
	cfg := &game.Config{
		World:      game.WorldConfig{Width: 20, Height: 12, Seed: 1, TickRateHz: 100},
		Players:    []game.PlayerConfig{{Slot: 1, Name: "Bot", JoinToken: "bot"}, {Slot: 2, Name: "Idle", JoinToken: "idle"}},
		AdminToken: "admin",
		Units:      []game.UnitBlock{{Owner: 1, X: 1, Y: 1, W: 3, H: 2}, {Owner: 2, X: 17, Y: 9}},
		Plugins: map[string]json.RawMessage{
			"wells":  json.RawMessage(`{"wells": [{"x": 6, "y": 2, "unlimited": true}]}`),
			"energy": json.RawMessage(`{"unit_capacity": 100, "initial_energy": 50}`),
		},
	}
	gm, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	server.New(cfg, gm).Register(g)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = gm.Engine.Run(ctx, 10*time.Millisecond) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	b := &bot{
		game: rtsv1.NewGameServiceClient(conn),
		s: &state{lastSeen: map[uint64]uint64{}, pending: map[string]string{},
			assigned: map[uint64]uint64{}, depleted: map[uint64]bool{}},
	}
	b.rnd = newRand()
	if err := b.connect(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := b.play(ctx, "bot", 80); err != nil {
		t.Fatal(err)
	}
	automated := 0
	for _, u := range b.s.own() {
		if u.GetAttributes()["automation.behavior"].GetStringValue() == "mine_and_deliver" {
			automated++
		}
	}
	if automated == 0 {
		t.Fatal("bot never put units to work on the visible well")
	}
	if len(b.s.pending) > 10 {
		t.Fatalf("%d commands never got a result", len(b.s.pending))
	}
}
