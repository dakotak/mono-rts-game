// Command mono-bothost starts and stops computer players on request, for
// the player client's "Add computer opponent" panel (proxied by
// rts-gateway -bothost) or any HTTP client.
//
//	mono-bothost -config configs/presets/duel.json -slots 2
//	mono-bothost -slot-token 2=duel-blue -slot-token 3=bot-green
//
// It needs the join tokens of the slots it may fill, read from the server's
// configuration file (only the players section; the admin token is never
// read or used) or given with -slot-token. Bots are ordinary GameService
// clients; see internal/bot/host for the API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/host"
)

type tokens []string

func (t *tokens) String() string     { return strings.Join(*t, ",") }
func (t *tokens) Set(v string) error { *t = append(*t, v); return nil }

func main() {
	server := flag.String("server", "127.0.0.1:50051", "rts-server gRPC address")
	addr := flag.String("addr", "127.0.0.1:8090", "HTTP listen address (keep it local or behind the gateway)")
	configPath := flag.String("config", "", "server configuration to read player slots and join tokens from")
	slotList := flag.String("slots", "", "comma-separated slots computer players may take (default: every configured slot)")
	var slotTokens tokens
	flag.Var(&slotTokens, "slot-token", "SLOT=JOIN_TOKEN for a slot computer players may take (repeatable)")
	level := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Parse()

	lgCfg := bot.Config{LogLevel: *level}
	lg := lgCfg.Logger()
	slots, err := loadSlots(*configPath, *slotList, slotTokens)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mono-bothost:", err)
		os.Exit(2)
	}
	conn, err := grpc.NewClient(*server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		lg.Error("dial", "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	h := host.New(conn, slots, lg)
	srv := &http.Server{Addr: *addr, Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		h.StopAll()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	var names []string
	for _, s := range slots {
		names = append(names, fmt.Sprintf("%d (%s)", s.Slot, s.Name))
	}
	lg.Info("bot host", "addr", "http://"+*addr+"/api/bots", "server", *server, "slots", strings.Join(names, ", "))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		lg.Error("listen", "err", err)
		os.Exit(1)
	}
}

func loadSlots(path, list string, extra []string) ([]host.SlotConfig, error) {
	var out []host.SlotConfig
	if path != "" {
		// Only the players section is read: no admin token, no rules.
		var cfg struct {
			Players []struct {
				Slot      int    `json:"slot"`
				Name      string `json:"name"`
				JoinToken string `json:"join_token"`
			} `json:"players"`
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, p := range cfg.Players {
			out = append(out, host.SlotConfig{Slot: p.Slot, Name: p.Name, Token: p.JoinToken})
		}
	}
	for _, st := range extra {
		n, tok, ok := strings.Cut(st, "=")
		slot, err := strconv.Atoi(n)
		if !ok || err != nil || tok == "" {
			return nil, fmt.Errorf("bad -slot-token %q (want SLOT=TOKEN)", st)
		}
		out = slices.DeleteFunc(out, func(s host.SlotConfig) bool { return s.Slot == slot })
		out = append(out, host.SlotConfig{Slot: slot, Name: fmt.Sprintf("Slot %d", slot), Token: tok})
	}
	if list != "" {
		allowed := map[int]bool{}
		for _, f := range strings.Split(list, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				return nil, fmt.Errorf("bad -slots %q", list)
			}
			allowed[n] = true
		}
		out = slices.DeleteFunc(out, func(s host.SlotConfig) bool { return !allowed[s.Slot] })
	}
	if len(out) == 0 {
		return nil, host.ErrNoSlots
	}
	return out, nil
}
