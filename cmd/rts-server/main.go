// Command rts-server hosts one game world and serves the GameService and
// AdminService gRPC APIs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func main() {
	configPath := flag.String("config", "configs/dev.json", "game configuration file")
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC listen address")
	check := flag.Bool("check", false, "validate the configuration (including world generation), print a summary and exit")
	hashEvery := flag.Uint64("replay-hash-interval", server.ReplayHashInterval, "ticks between full state hashes in recorded replays (0 = start and end only)")
	flag.Parse()

	cfg, err := game.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	gm, err := game.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	engine := gm.Engine
	if *check {
		summarize(cfg, gm)
		return
	}
	server.ReplayHashInterval = *hashEvery
	srv := server.New(cfg, gm)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	// Replays (ExportReplay, VerifyReplay) can exceed gRPC's 4 MiB default.
	g := grpc.NewServer(grpc.MaxRecvMsgSize(64 << 20))
	srv.Register(g)
	reflection.Register(g) // lets grpcurl and other tools discover the API

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("world %dx%d, %d entities, %d Hz, plugins %v",
		cfg.World.Width, cfg.World.Height, engine.World().EntityCount(), cfg.World.TickRateHz, engine.Rules().Plugins())
	log.Printf("tick schedule: %s", strings.Join(engine.Rules().Schedule(), "; "))
	st := match.Get(engine.World())
	log.Printf("match %s: %s, seed %d, %d players", srv.MatchID(), st.State, gm.Seed, st.N)

	simDone := make(chan error, 1)
	go func() { simDone <- srv.Run(ctx) }()
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

// summarize prints what -check validated.
func summarize(cfg *game.Config, g *game.Game) {
	w := g.Engine.World()
	kinds := map[sim.Kind]int{}
	owned := map[sim.PlayerID]int{}
	for _, id := range w.EntityIDs() {
		e, _ := w.Entity(id)
		kinds[e.Kind]++
		if e.Kind == sim.KindUnit {
			owned[e.Owner]++
		}
	}
	st := match.Get(w)
	fmt.Printf("configuration OK\n")
	fmt.Printf("world      %dx%d at %d Hz, seed %d (random_seed=%v)\n", cfg.World.Width, cfg.World.Height, cfg.World.TickRateHz, g.Seed, cfg.World.RandomSeed)
	fmt.Printf("entities   %d units, %d resources, %d terrain\n", kinds[sim.KindUnit], kinds[sim.KindResource], kinds[sim.KindTerrain])
	for _, p := range g.Players {
		fmt.Printf("player     slot %d %q: %d units\n", p.Slot, p.Name, owned[p.ID])
	}
	fmt.Printf("match      %s after setup\n", st.State)
	fmt.Printf("plugins    %s\n", strings.Join(g.Engine.Rules().Plugins(), " "))
	fmt.Printf("ruleset    %s (%s)\n", game.RulesetHash(g.Engine.Rules()), game.EngineVersion)
	for _, line := range g.Engine.Rules().Schedule() {
		fmt.Printf("schedule   %s\n", line)
	}
}
