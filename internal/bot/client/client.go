// Package client runs a computer player against a Mono server through the
// public GameService gRPC API: connect with a join token, observe, keep
// knowledge current, let the bot decide every tick, submit its commands,
// and reconnect after interruptions.
package client

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// Options configure a Runner.
type Options struct {
	// Token is the player's join token.
	Token string
	// NewBot builds the bot once the world (and its rules) is known.
	NewBot func(k *state.Knowledge) *ai.Bot
	Logger *slog.Logger
	// OnTick is called after each tick's decisions were submitted (the
	// arena uses it to step in lockstep).
	OnTick func(tick uint64)
	// MaxTicks stops after observing this many ticks (0 = run forever).
	MaxTicks int
	// Backoff between reconnection attempts (default 1s).
	Backoff time.Duration
}

// Runner connects one bot to a server.
type Runner struct {
	game    rtsv1.GameServiceClient
	opts    Options
	K       *state.Knowledge
	Bot     *ai.Bot
	session string
	ticks   int
	// Reconnects counts observe-stream and session re-establishments.
	Reconnects int
}

// New returns a runner over a gRPC connection.
func New(conn grpc.ClientConnInterface, opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Backoff == 0 {
		opts.Backoff = time.Second
	}
	return &Runner{game: rtsv1.NewGameServiceClient(conn), opts: opts, K: state.New()}
}

func (r *Runner) authed(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+r.session)
}

func (r *Runner) connect(ctx context.Context) error {
	resp, err := r.game.Connect(ctx, &rtsv1.ConnectRequest{JoinToken: r.opts.Token})
	if err != nil {
		return err
	}
	r.session = resp.GetSessionToken()
	if err := r.K.SetWorld(resp); err != nil {
		return err
	}
	if r.Bot == nil {
		r.Bot = r.opts.NewBot(r.K)
	}
	r.opts.Logger.Info("connected", "player", resp.GetPlayerName(), "player_id", resp.GetPlayerId(),
		"world", resp.GetWorld().GetWidth(), "height", resp.GetWorld().GetHeight(), "budget", resp.GetWorld().GetUnitCommandBudget(),
		"match", resp.GetMatch().GetMatchId(), "state", resp.GetMatch().GetState().String())
	return nil
}

// Run plays until ctx is cancelled (graceful shutdown, returns nil),
// MaxTicks is reached, or the join token is rejected.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if r.session == "" {
			if err := r.connect(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if status.Code(err) == codes.Unauthenticated {
					return err // a wrong join token will not fix itself
				}
				r.opts.Logger.Warn("connect failed; retrying", "err", err)
				if !sleep(ctx, r.opts.Backoff) {
					return nil
				}
				continue
			}
		}
		done, err := r.observe(ctx)
		if done || ctx.Err() != nil {
			return nil
		}
		r.Reconnects++
		if status.Code(err) == codes.Unauthenticated {
			r.session = "" // the server restarted: sessions are in memory
		}
		r.opts.Logger.Warn("stream ended; reconnecting", "err", err)
		if !sleep(ctx, r.opts.Backoff) {
			return nil
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// observe consumes one Observe stream. done reports MaxTicks reached.
func (r *Runner) observe(ctx context.Context) (done bool, err error) {
	stream, err := r.game.Observe(r.authed(ctx), &rtsv1.ObserveRequest{})
	if err != nil {
		return false, err
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return false, err
		}
		r.K.BeginTick()
		if s := msg.GetSnapshot(); s != nil {
			r.K.ApplySnapshot(s)
		} else {
			r.K.ApplyUpdate(msg.GetUpdate())
		}
		r.submit(ctx, r.Bot.Step())
		if r.opts.OnTick != nil {
			r.opts.OnTick(r.K.Tick)
		}
		if msg.GetUpdate() != nil {
			r.ticks++
			if r.opts.MaxTicks > 0 && r.ticks >= r.opts.MaxTicks {
				return true, nil
			}
		}
	}
}

func (r *Runner) submit(ctx context.Context, outs []ai.Outgoing) {
	for _, o := range outs {
		payload, err := anypb.New(o.Payload)
		if err != nil {
			r.Bot.Cmd.Rejected(o.ID)
			continue
		}
		ack, err := r.game.SubmitCommand(r.authed(ctx), &rtsv1.SubmitCommandRequest{CommandId: o.ID, Payload: payload})
		switch {
		case err != nil:
			r.Bot.Cmd.Rejected(o.ID)
			if !errors.Is(ctx.Err(), context.Canceled) {
				r.opts.Logger.Warn("submit failed", "order", o.Order.Desc, "err", err)
			}
		case !ack.GetAccepted():
			r.Bot.Cmd.Rejected(o.ID)
			r.opts.Logger.Info("command rejected", "order", o.Order.Desc, "reason", ack.GetRejectionReason())
		}
	}
}
