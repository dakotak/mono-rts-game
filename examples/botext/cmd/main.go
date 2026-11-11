// Command botext runs Mono's computer player with the botext extensions:
//
//	go run ./examples/botext/cmd --token duel-red --difficulty 7
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dakotaknutson/unit-strategy-game/examples/botext"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/client"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

func main() {
	cfg := bot.Defaults()
	flag.StringVar(&cfg.Server, "server", cfg.Server, "rts-server gRPC address")
	flag.StringVar(&cfg.Token, "token", "", "join token")
	flag.IntVar(&cfg.Difficulty, "difficulty", cfg.Difficulty, "1..10")
	flag.StringVar(&cfg.Personality, "personality", cfg.Personality, "personality")
	flag.Parse()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	conn, err := grpc.NewClient(cfg.Server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	lg := cfg.Logger()
	r := client.New(conn, client.Options{Token: cfg.Token, Logger: lg, NewBot: func(k *state.Knowledge) *ai.Bot {
		return cfg.NewBot(k, lg, botext.Extensions())
	}})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := r.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
