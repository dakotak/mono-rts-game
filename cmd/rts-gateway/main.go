// Command rts-gateway serves the browser client and bridges browser HTTP
// requests to the server's public GameService over gRPC.
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
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	webDir := flag.String("web", "", "serve the client from this directory instead of the embedded copy (for live editing)")
	flag.Parse()

	conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	var static fs.FS = web.Static()
	if *webDir != "" {
		static = os.DirFS(*webDir)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           gateway.New(conn, static),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("gateway on http://%s -> gRPC %s", *addr, *serverAddr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
