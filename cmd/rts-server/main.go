// Command rts-server hosts one game world and serves the GameService and
// AdminService gRPC APIs.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
)

func main() {
	configPath := flag.String("config", "configs/dev.json", "game configuration file")
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC listen address")
	flag.Parse()

	cfg, err := game.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := game.NewEngine(cfg)
	if err != nil {
		log.Fatal(err)
	}
	srv := server.New(cfg, engine)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	g := grpc.NewServer()
	srv.Register(g)
	reflection.Register(g) // lets grpcurl and other tools discover the API

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("world %dx%d, %d entities, %d Hz, plugins %v",
		cfg.World.Width, cfg.World.Height, engine.World().EntityCount(), cfg.World.TickRateHz, engine.Rules().Plugins())
	log.Printf("tick schedule: %s", strings.Join(engine.Rules().Schedule(), "; "))

	simDone := make(chan error, 1)
	go func() { simDone <- engine.Run(ctx, cfg.TickInterval()) }()
	go func() {
		<-ctx.Done()
		// Observe streams never end on their own; give unary calls a moment
		// to finish, then cut the streams.
		force := time.AfterFunc(2*time.Second, g.Stop)
		g.GracefulStop()
		force.Stop()
	}()

	log.Printf("gRPC listening on %s", lis.Addr())
	if err := g.Serve(lis); err != nil {
		log.Fatal(err)
	}
	if err := <-simDone; err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Print("shut down")
}
