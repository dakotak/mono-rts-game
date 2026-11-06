// Package gateway translates between browsers and the public GameService.
//
// It is a thin protocol adapter with no game logic and no access to the
// simulation: it is just another gRPC client. Messages are the GameService
// protobuf messages in their canonical protojson form:
//
//	POST /api/connect   ConnectRequest        -> ConnectResponse
//	POST /api/command   SubmitCommandRequest  -> SubmitCommandResponse
//	GET  /api/snapshot                        -> GetSnapshotResponse
//	GET  /api/observe                         -> newline-delimited ObserveResponse stream
//
// The session token travels in the "Authorization: Bearer <token>" header and
// is forwarded as gRPC metadata. Everything else is served from the static
// file system (the browser client).
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	// Command payloads are carried in google.protobuf.Any; protojson needs
	// their types registered to translate them. Each plugin's command
	// package must be imported here.
	_ "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
)

const maxBody = 1 << 20

var (
	marshal   = protojson.MarshalOptions{EmitUnpopulated: true}
	unmarshal = protojson.UnmarshalOptions{}
)

// New returns the gateway handler.
func New(conn grpc.ClientConnInterface, static fs.FS) http.Handler {
	g := &gateway{client: rtsv1.NewGameServiceClient(conn)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/connect", g.connect)
	mux.HandleFunc("POST /api/command", g.command)
	mux.HandleFunc("GET /api/snapshot", g.snapshot)
	mux.HandleFunc("GET /api/observe", g.observe)
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

type gateway struct {
	client rtsv1.GameServiceClient
}

// outgoing forwards the caller's Authorization header to the server.
func outgoing(r *http.Request) context.Context {
	ctx := r.Context()
	if a := r.Header.Get("Authorization"); a != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", a)
	}
	return ctx
}

func readJSON(w http.ResponseWriter, r *http.Request, m proto.Message) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil {
		err = unmarshal.Unmarshal(body, m)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, m proto.Message) {
	b, err := marshal.Marshal(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, httpStatus int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

func writeGRPCError(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	httpStatus := http.StatusBadGateway
	switch st.Code() {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
	case codes.PermissionDenied:
		httpStatus = http.StatusForbidden
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
	}
	writeError(w, httpStatus, st.Code().String(), st.Message())
}

func (g *gateway) connect(w http.ResponseWriter, r *http.Request) {
	var req rtsv1.ConnectRequest
	if !readJSON(w, r, &req) {
		return
	}
	resp, err := g.client.Connect(r.Context(), &req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *gateway) command(w http.ResponseWriter, r *http.Request) {
	var req rtsv1.SubmitCommandRequest
	if !readJSON(w, r, &req) {
		return
	}
	resp, err := g.client.SubmitCommand(outgoing(r), &req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *gateway) snapshot(w http.ResponseWriter, r *http.Request) {
	resp, err := g.client.GetSnapshot(outgoing(r), &rtsv1.GetSnapshotRequest{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *gateway) observe(w http.ResponseWriter, r *http.Request) {
	stream, err := g.client.Observe(outgoing(r), &rtsv1.ObserveRequest{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	// Wait for the first message so authentication errors become proper
	// HTTP statuses instead of a broken stream.
	first, err := stream.Recv()
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	for msg := first; ; {
		b, err := marshal.Marshal(msg)
		if err != nil {
			log.Printf("observe: marshal: %v", err)
			return
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return // browser went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		if msg, err = stream.Recv(); err != nil {
			if r.Context().Err() == nil {
				log.Printf("observe: upstream ended: %v", err)
			}
			return
		}
	}
}
