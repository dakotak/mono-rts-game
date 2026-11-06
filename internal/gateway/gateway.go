// Package gateway translates between browsers and the server's gRPC APIs.
//
// It is a thin protocol adapter with no game logic and no access to the
// simulation: it is just another gRPC client. Messages are the protobuf
// request/response messages in their canonical protojson form, and streams
// are newline-delimited JSON. Credentials travel in the
// "Authorization: Bearer <token>" header and are forwarded as gRPC metadata.
//
// Player and admin traffic are served by separate handlers, meant for
// separate listeners: the player handler only ever calls GameService, the
// admin handler only ever calls AdminService.
//
// Player API (New):
//
//	POST /api/connect   ConnectRequest        -> ConnectResponse
//	POST /api/command   SubmitCommandRequest  -> SubmitCommandResponse
//	GET  /api/snapshot                        -> GetSnapshotResponse
//	GET  /api/observe                         -> ObserveResponse stream
//
// Admin API (NewAdmin):
//
//	GET  /api/snapshot                           -> GetWorldSnapshotResponse
//	GET  /api/status                             -> GetStatusResponse
//	GET  /api/observe                            -> ObserveWorldResponse stream
//	POST /api/spawn     SpawnUnitRequest          -> SpawnUnitResponse
//	POST /api/delete    DeleteEntityRequest       -> DeleteEntityResponse
//	POST /api/move      MoveEntityRequest         -> MoveEntityResponse
//	POST /api/command   SubmitAdminCommandRequest -> SubmitAdminCommandResponse
//	POST /api/pause     SetPausedRequest          -> SetPausedResponse
//	POST /api/step      StepRequest               -> StepResponse
//
// Static files: the player handler serves web/static/player at "/", the
// admin handler serves web/static/admin at "/"; both serve
// web/static/shared at "/shared/".
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

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	// Registers every plugin's command messages so protojson can translate
	// google.protobuf.Any payloads.
	_ "github.com/dakotaknutson/unit-strategy-game/internal/protoreg"
)

const maxBody = 1 << 20

var (
	marshal   = protojson.MarshalOptions{EmitUnpopulated: true}
	unmarshal = protojson.UnmarshalOptions{}
)

// New returns the player gateway handler. static is the web/static tree.
func New(conn grpc.ClientConnInterface, static fs.FS) http.Handler {
	c := rtsv1.NewGameServiceClient(conn)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/connect", unary(c.Connect))
	mux.HandleFunc("POST /api/command", unary(c.SubmitCommand))
	mux.HandleFunc("GET /api/snapshot", unary(c.GetSnapshot))
	mux.HandleFunc("GET /api/observe", streaming(c.Observe))
	serveStatic(mux, static, "player")
	return mux
}

// NewAdmin returns the admin gateway handler. static is the web/static tree.
func NewAdmin(conn grpc.ClientConnInterface, static fs.FS) http.Handler {
	c := rtsv1.NewAdminServiceClient(conn)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", unary(c.GetWorldSnapshot))
	mux.HandleFunc("GET /api/status", unary(c.GetStatus))
	mux.HandleFunc("GET /api/observe", streaming(c.ObserveWorld))
	mux.HandleFunc("POST /api/spawn", unary(c.SpawnUnit))
	mux.HandleFunc("POST /api/delete", unary(c.DeleteEntity))
	mux.HandleFunc("POST /api/move", unary(c.MoveEntity))
	mux.HandleFunc("POST /api/command", unary(c.SubmitAdminCommand))
	mux.HandleFunc("POST /api/pause", unary(c.SetPaused))
	mux.HandleFunc("POST /api/step", unary(c.Step))
	serveStatic(mux, static, "admin")
	return mux
}

func serveStatic(mux *http.ServeMux, static fs.FS, app string) {
	appFS, err := fs.Sub(static, app)
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /shared/", http.FileServerFS(static))
	mux.Handle("GET /", http.FileServerFS(appFS))
}

// outgoing forwards the caller's Authorization header to the server.
func outgoing(r *http.Request) context.Context {
	ctx := r.Context()
	if a := r.Header.Get("Authorization"); a != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", a)
	}
	return ctx
}

// unary adapts a unary gRPC client method. GET requests send an empty
// request message; POST requests decode the body as protojson.
func unary[Req any, Resp proto.Message, PReq interface {
	*Req
	proto.Message
}](call func(context.Context, PReq, ...grpc.CallOption) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := PReq(new(Req))
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err == nil {
				err = unmarshal.Unmarshal(body, req)
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
				return
			}
		}
		resp, err := call(outgoing(r), req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		b, err := marshal.Marshal(resp)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

// streaming adapts a server-streaming gRPC client method to NDJSON.
func streaming[Req any, Resp any, PReq interface {
	*Req
	proto.Message
}](call func(context.Context, PReq, ...grpc.CallOption) (grpc.ServerStreamingClient[Resp], error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stream, err := call(outgoing(r), PReq(new(Req)))
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		// Wait for the first message so authentication errors become proper
		// HTTP statuses instead of a broken stream.
		msg, err := stream.Recv()
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		for {
			b, err := marshal.Marshal(any(msg).(proto.Message))
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
	case codes.FailedPrecondition:
		httpStatus = http.StatusConflict
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
	}
	writeError(w, httpStatus, st.Code().String(), st.Message())
}
