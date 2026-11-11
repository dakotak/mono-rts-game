// Command rts-gateway serves the browser clients and bridges browser HTTP
// requests to the server's gRPC APIs. The player client and API listen on
// -addr; the admin client and API listen on a separate -admin-addr so that
// administrative access can be firewalled independently.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dakotaknutson/unit-strategy-game/internal/gateway"
	"github.com/dakotaknutson/unit-strategy-game/web"
)

func main() {
	serverAddr := flag.String("server", "127.0.0.1:50051", "rts-server gRPC address")
	addr := flag.String("addr", "127.0.0.1:8080", "player HTTP listen address")
	adminAddr := flag.String("admin-addr", "127.0.0.1:8081", "admin HTTP listen address (empty disables the admin gateway)")
	botHost := flag.String("bothost", "", "computer-player host (mono-bothost) URL to expose at /api/bots for the player client's \"Add computer opponent\" panel")
	webDir := flag.String("web", "", "serve the client from this directory instead of the embedded copy (for live editing)")
	flag.Parse()

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	var static fs.FS = web.Static()
	if *webDir != "" {
		static = os.DirFS(*webDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	player := gateway.New(conn, static)
	if *botHost != "" {
		if player, err = gateway.WithBotHost(player, *botHost); err != nil {
			log.Fatal(err)
		}
		log.Printf("computer players via %s at /api/bots", *botHost)
	}
	servers := []*http.Server{{Addr: *addr, Handler: player, ReadHeaderTimeout: 5 * time.Second}}
	log.Printf("player gateway on http://%s -> gRPC %s", *addr, *serverAddr)
	if *adminAddr != "" {
		servers = append(servers, &http.Server{Addr: *adminAddr, Handler: gateway.NewAdmin(conn, static), ReadHeaderTimeout: 5 * time.Second})
		log.Printf("admin gateway on http://%s -> gRPC %s", *adminAddr, *serverAddr)
	}

	errs := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				return
			}
			errs <- nil
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil {
			log.Fatal(err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
}
