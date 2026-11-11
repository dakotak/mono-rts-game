package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/gateway"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
)

var testStatic = fstest.MapFS{
	"player/index.html": {Data: []byte("player")},
	"admin/index.html":  {Data: []byte("admin")},
	"shared/board.js":   {Data: []byte("shared")},
}

func TestGatewayEndToEnd(t *testing.T) {
	cfg := &game.Config{
		World:   game.WorldConfig{Width: 8, Height: 8, Seed: 1, TickRateHz: 50},
		Players: []game.PlayerConfig{{Slot: 1, Name: "Red", JoinToken: "red"}, {Slot: 2, Name: "Blue", JoinToken: "blue"}},
		Units:   []game.UnitBlock{{Owner: 1, X: 0, Y: 0}, {Owner: 2, X: 7, Y: 7}},
	}
	gm, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := gm.Engine
	srv := server.New(cfg, gm)
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = engine.Run(ctx, 20*time.Millisecond) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	web := httptest.NewServer(gateway.New(conn, testStatic))
	defer web.Close()

	post := func(path, token, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequestWithContext(ctx, "POST", web.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}

	if resp, _ := post("/api/connect", "", `{"joinToken":"nope"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status %d", resp.StatusCode)
	}
	_, conn1 := post("/api/connect", "", `{"joinToken":"red"}`)
	token, _ := conn1["sessionToken"].(string)
	playerID, _ := conn1["playerId"].(float64)
	if token == "" || playerID == 0 || conn1["playerName"] != "Red" {
		t.Fatalf("connect = %v", conn1)
	}

	// Open the observation stream before commanding, as a client would.
	req, _ := http.NewRequestWithContext(ctx, "GET", web.URL+"/api/observe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("observe: %v %v", stream, err)
	}
	defer stream.Body.Close()
	lines := bufio.NewScanner(stream.Body)
	lines.Buffer(nil, 1<<20)
	if !lines.Scan() || !strings.Contains(lines.Text(), `"snapshot"`) {
		t.Fatalf("first line = %q", lines.Text())
	}
	// Find our unit; IDs are opaque, so look it up by owner.
	var first struct {
		Snapshot struct {
			Entities []struct {
				ID      string  `json:"id"`
				OwnerID float64 `json:"ownerId"`
			} `json:"entities"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(lines.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	var unitID string
	for _, en := range first.Snapshot.Entities {
		if en.OwnerID == playerID {
			unitID = en.ID
		}
	}
	if unitID == "" {
		t.Fatalf("no unit owned by %v in %s", playerID, lines.Text())
	}

	_, ack := post("/api/command", token, `{"commandId":"m1","payload":{"@type":"type.googleapis.com/rts.movement.v1.MoveCommand","orders":[{"unitId":"`+unitID+`","destination":{"x":2,"y":0}}]}}`)
	if ack["accepted"] != true {
		t.Fatalf("ack = %v", ack)
	}
	// Read updates until the unit is reported at its destination.
	for lines.Scan() {
		var msg struct {
			Update struct {
				Changed []struct {
					ID       string `json:"id"`
					Position struct{ X, Y int }
				} `json:"changed"`
				CommandResults []struct {
					CommandID string `json:"commandId"`
					Status    string `json:"status"`
				} `json:"commandResults"`
			} `json:"update"`
		}
		if err := json.Unmarshal(lines.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		for _, r := range msg.Update.CommandResults {
			if r.CommandID == "m1" && r.Status != "COMMAND_STATUS_SUCCEEDED" {
				t.Fatalf("result = %+v", r)
			}
		}
		for _, c := range msg.Update.Changed {
			if c.ID == unitID && c.Position.X == 2 {
				// Static files are served too: the player app at the root,
				// shared assets under /shared/, and never the admin app.
				for path, want := range map[string]int{"/index.html": 200, "/shared/board.js": 200, "/admin/index.html": 404} {
					resp, err := http.Get(web.URL + path)
					if err != nil || resp.StatusCode != want {
						t.Fatalf("GET %s: %v %v, want %d", path, resp, err, want)
					}
					resp.Body.Close()
				}
				return
			}
		}
	}
	t.Fatalf("stream ended before arrival: %v", lines.Err())
}

func TestGatewayRejectsUnauthenticatedObserve(t *testing.T) {
	cfg := &game.Config{World: game.WorldConfig{Width: 4, Height: 4, TickRateHz: 10}}
	gm, _ := game.New(cfg)
	srv := server.New(cfg, gm)
	lis := bufconn.Listen(1 << 16)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, _ := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	web := httptest.NewServer(gateway.New(conn, fstest.MapFS{}))
	defer web.Close()
	resp, err := http.Get(web.URL + "/api/observe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAdminGateway(t *testing.T) {
	cfg := &game.Config{
		World:      game.WorldConfig{Width: 6, Height: 6, TickRateHz: 10},
		Players:    []game.PlayerConfig{{Slot: 1, Name: "Red", JoinToken: "red"}},
		AdminToken: "secret-admin",
	}
	gm, err := game.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, gm)
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, _ := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()

	admin := httptest.NewServer(gateway.NewAdmin(conn, testStatic))
	defer admin.Close()
	player := httptest.NewServer(gateway.New(conn, testStatic))
	defer player.Close()

	do := func(base, method, path, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	if code, _ := do(admin.URL, "GET", "/api/snapshot", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := do(admin.URL, "GET", "/api/snapshot", "red", ""); code != http.StatusUnauthorized {
		t.Fatalf("join token accepted by admin gateway: %d", code)
	}
	code, snap := do(admin.URL, "GET", "/api/snapshot", "secret-admin", "")
	if code != 200 || snap["world"].(map[string]any)["width"].(float64) != 6 || len(snap["players"].([]any)) != 1 {
		t.Fatalf("snapshot: %d %v", code, snap)
	}
	code, ack := do(admin.URL, "POST", "/api/command", "secret-admin",
		`{"payload":{"@type":"type.googleapis.com/rts.wells.v1.CreateWellCommand","position":{"x":2,"y":2},"unlimited":true}}`)
	if code != 200 || ack["accepted"] != true {
		t.Fatalf("create well: %d %v", code, ack)
	}
	if code, _ := do(admin.URL, "POST", "/api/step", "secret-admin", `{"count":1}`); code != http.StatusConflict {
		t.Fatalf("step while running: %d", code)
	}
	// The admin API is not reachable through the player gateway.
	if code, _ := do(player.URL, "POST", "/api/spawn", "secret-admin", `{}`); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("admin route on player gateway: %d", code)
	}
	// Static: admin app at root.
	if code, _ := do(admin.URL, "GET", "/index.html", "", ""); code != 200 {
		t.Fatalf("admin static: %d", code)
	}
}

// TestStaticFilesRevalidate: browsers must revalidate client files, so an
// updated client never runs stale cached scripts against a new page.
func TestStaticFilesRevalidate(t *testing.T) {
	for _, h := range []http.Handler{gateway.New(nil, testStatic), gateway.NewAdmin(nil, testStatic)} {
		for _, path := range []string{"/", "/shared/board.js"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("GET %s: %d, Cache-Control %q", path, rec.Code, rec.Header().Get("Cache-Control"))
			}
		}
	}
}
