// Command mono-bot is Mono's autonomous computer player. It joins a match
// through the public GameService gRPC API with a player's join token, like
// any human client, and plays it: economy, specialization, logistics,
// exploration, combat and strategy, at difficulty 1..10.
//
//	mono-bot --server 127.0.0.1:50051 --token blue --difficulty 7 --personality aggressive
//
// It never uses admin access, receives only what the player may observe,
// and is bound by the same command budget as everyone.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/client"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

func main() {
	cfg := bot.Defaults()
	configPath := flag.String("config", "", "JSON configuration file (flags given explicitly override it)")
	server := flag.String("server", cfg.Server, "rts-server gRPC address")
	token := flag.String("token", "", "the player's join token")
	name := flag.String("player-name", "", "label for logs (the server's player name comes from its configuration)")
	diff := flag.Int("difficulty", cfg.Difficulty, "difficulty 1 (beginner) .. 10 (maximum)")
	pers := flag.String("personality", cfg.Personality, "balanced, aggressive, defensive, economic or exploratory")
	seed := flag.Uint64("seed", 0, "seed for the bot's decisions")
	det := flag.Bool("deterministic", false, "deterministic decisions and command IDs (same inputs, same outputs)")
	lvl := flag.String("log-level", cfg.LogLevel, "debug, info, warn or error")
	debug := flag.Bool("debug", false, "log utility scores and candidate evaluations")
	statusAddr := flag.String("status-addr", "", "serve the bot's diagnostics as JSON at http://ADDR/status (local only)")
	ticks := flag.Int("ticks", 0, "stop after this many ticks (0 = run until interrupted)")
	flag.Parse()

	if *configPath != "" {
		if err := cfg.LoadFile(*configPath); err != nil {
			log.Fatal(err)
		}
	}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "server":
			cfg.Server = *server
		case "token":
			cfg.Token = *token
		case "player-name":
			cfg.PlayerName = *name
		case "difficulty":
			cfg.Difficulty = *diff
		case "personality":
			cfg.Personality = *pers
		case "seed":
			cfg.Seed = *seed
		case "deterministic":
			cfg.Deterministic = *det
		case "log-level":
			cfg.LogLevel = *lvl
		case "debug":
			cfg.Debug = *debug
		case "status-addr":
			cfg.StatusAddr = *statusAddr
		}
	})
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "mono-bot:", err)
		os.Exit(2)
	}
	lg := cfg.Logger()
	conn, err := grpc.NewClient(cfg.Server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	var (
		mu     sync.Mutex
		status = []byte("{}") // diagnostics snapshot, refreshed between ticks
		r      *client.Runner
	)
	r = client.New(conn, client.Options{
		Token:    cfg.Token,
		Logger:   lg,
		MaxTicks: *ticks,
		NewBot: func(k *state.Knowledge) *ai.Bot {
			return cfg.NewBot(k, lg)
		},
		OnTick: func(tick uint64) {
			if cfg.StatusAddr != "" && tick%10 == 0 && r.Bot != nil {
				data, _ := json.Marshal(r.Bot.Status)
				mu.Lock()
				status = data
				mu.Unlock()
			}
		},
	})
	if cfg.StatusAddr != "" {
		go func() {
			http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				data := status
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(data)
			})
			lg.Info("diagnostics", "url", "http://"+cfg.StatusAddr+"/status")
			if err := http.ListenAndServe(cfg.StatusAddr, nil); err != nil {
				lg.Error("diagnostics server", "err", err)
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lg.Info("starting", "server", cfg.Server, "difficulty", cfg.Difficulty, "personality", cfg.Personality, "seed", cfg.Seed, "deterministic", cfg.Deterministic)
	if err := r.Run(ctx); err != nil {
		lg.Error("stopped", "err", err)
		os.Exit(1)
	}
	if r.Bot != nil {
		s := r.Bot.Status
		lg.Info("finished", "tick", s.Tick, "strategy", s.Strategy, "units", s.Units, "commands", s.Commands.Submitted,
			"failed", s.Commands.Failed, "reconnects", r.Reconnects)
	}
}
