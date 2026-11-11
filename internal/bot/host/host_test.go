package host

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
)

func TestHostStartsBotsOnlyInConfiguredSlots(t *testing.T) {
	cfg, err := game.LoadConfig("../../../configs/presets/duel.json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, g)
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()
	conn, err := grpc.NewClient("passthrough:///h", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	h := New(conn, []SlotConfig{{Slot: 2, Name: "Blue", Token: "duel-blue"}}, nil)
	defer h.StopAll()
	api := httptest.NewServer(h.Handler())
	defer api.Close()

	post := func(body string) *http.Response {
		res, err := http.Post(api.URL+"/api/bots", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := post(`{"slot":1,"difficulty":5}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("unconfigured slot: %d", res.StatusCode)
	}
	if res := post(`{"slot":2,"difficulty":11}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("bad difficulty: %d", res.StatusCode)
	}
	if res := post(`{"slot":2,"difficulty":6,"personality":"aggressive"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("start: %d", res.StatusCode)
	}
	if res := post(`{"slot":2,"difficulty":6}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("double start: %d", res.StatusCode)
	}
	// The bot connects as an ordinary player and readies up.
	deadline := time.Now().Add(5 * time.Second)
	for srv.Hub().Status().Ready[1] == false && time.Now().Before(deadline) {
		srv.Engine().Step()
		time.Sleep(5 * time.Millisecond)
	}
	if !srv.Hub().Status().Ready[1] {
		t.Fatal("the computer player never readied up in slot 2")
	}
	res, err := http.Get(api.URL + "/api/bots")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Slots []Slot `json:"slots"`
	}
	body, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "duel-blue") {
		t.Fatal("the join token is exposed by the API")
	}
	if len(list.Slots) != 1 || !list.Slots[0].Running || list.Slots[0].Difficulty != 6 || list.Slots[0].Personality != "aggressive" {
		t.Fatalf("slots = %+v", list.Slots)
	}
	req, _ := http.NewRequest(http.MethodDelete, api.URL+"/api/bots/2", nil)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("stop: %v %v", err, res.StatusCode)
	}
	if s := h.Slots(false); s[0].Running {
		t.Fatal("still running after stop")
	}
}
