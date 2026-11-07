// Command bot is a minimal autonomous Mono player using the public gRPC
// GameService. It connects with a join token, keeps a client-side copy of
// its view from the Observe stream, and plays a simple strategy:
//
//   - When it knows a well, it assigns idle units to mine-and-deliver
//     automation (well → one of its own units as the depot).
//   - Otherwise it sends idle units to explore toward a random tile.
//
// It respects the per-tick unit-command budget, logs command failures,
// and reconnects the stream on errors. See docs/api/bot-development.md.
//
//	go run ./examples/bot -addr 127.0.0.1:50051 -token red -ticks 300
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	matchv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/match/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
)

// state is the bot's client-side copy of what it observes.
type state struct {
	match    *rtsv1.MatchInfo // lifecycle, readiness, clock, result
	me       uint32
	tick     uint64
	width    int32
	height   int32
	budget   int                      // unit-commands per tick, 0 = unlimited
	entities map[uint64]*rtsv1.Entity // own entities and currently observed foreign ones
	lastSeen map[uint64]uint64        // foreign entity id -> tick last observed (memory under fog)
	pending  map[string]string        // command id -> description, until its result arrives
	assigned map[uint64]uint64        // miner id -> well id it was assigned to
	depleted map[uint64]bool          // wells known to be empty (they stay on the map)
}

// applyMatch tracks the match. Player IDs are per match: after a reset the
// same join token plays under a new ID, so adopt it.
func (s *state) applyMatch(m *rtsv1.MatchInfo) {
	if m == nil {
		return
	}
	if s.match != nil && s.match.GetMatchId() != m.GetMatchId() {
		log.Printf("new match %s (reset); forgetting the old world", m.GetMatchId())
		s.lastSeen, s.assigned, s.depleted = map[uint64]uint64{}, map[uint64]uint64{}, map[uint64]bool{}
	}
	if id := m.GetYourPlayerId(); id != 0 {
		s.me = id
	}
	if r := m.GetResult(); r != nil && (s.match == nil || s.match.GetResult() == nil) {
		log.Printf("match over: winner %d, draw %v, reason %s", r.GetWinnerId(), r.GetDraw(), r.GetReason())
	}
	s.match = m
}

func (s *state) applySnapshot(snap *rtsv1.WorldSnapshot) {
	s.applyMatch(snap.GetMatch())
	s.tick = snap.GetTick()
	s.entities = map[uint64]*rtsv1.Entity{}
	for _, e := range snap.GetEntities() {
		s.entities[e.GetId()] = e
	}
}

// applyUpdate merges one TickUpdate. Changed entities are full
// replacements; removed ones are gone from our view (under fog of war:
// destroyed or merely no longer detected — we cannot tell).
func (s *state) applyUpdate(u *rtsv1.TickUpdate) {
	s.applyMatch(u.GetMatch())
	s.tick = u.GetTick()
	for _, e := range u.GetChanged() {
		s.entities[e.GetId()] = e
		if e.GetOwnerId() != s.me {
			s.lastSeen[e.GetId()] = s.tick
		}
	}
	for _, id := range u.GetRemoved() {
		delete(s.entities, id)
	}
	for _, r := range u.GetCommandResults() {
		desc := s.pending[r.GetCommandId()]
		delete(s.pending, r.GetCommandId())
		if r.GetStatus() != rtsv1.CommandStatus_COMMAND_STATUS_SUCCEEDED {
			log.Printf("tick %d: %s %s: %s %s (%d unit results)", r.GetTick(), desc, r.GetStatus(), r.GetErrorCode(), r.GetErrorMessage(), len(r.GetUnitResults()))
		}
	}
	for _, ev := range u.GetEvents() {
		reason := ev.GetFields()["reason"].GetStringValue()
		switch {
		case ev.GetType() == "energy.well_depleted":
			s.depleted[ev.GetEntityId()] = true
		case ev.GetType() == "automation.failed" && reason == "well_depleted":
			s.depleted[s.assigned[ev.GetEntityId()]] = true
		}
		switch ev.GetType() {
		case "automation.failed", "automation.blocked", "combat.destroyed", "movement.cancelled":
			log.Printf("tick %d: event %s on %d %v", s.tick, ev.GetType(), ev.GetEntityId(), ev.GetFields())
		}
	}
}

func (s *state) own() []*rtsv1.Entity {
	var out []*rtsv1.Entity
	for _, e := range s.entities {
		if e.GetOwnerId() == s.me && e.GetKind() == rtsv1.EntityKind_ENTITY_KIND_UNIT {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *rtsv1.Entity) int { return compare(a.GetId(), b.GetId()) })
	return out
}

// wells are unowned, non-passable resources (wreckage is passable).
func (s *state) wells() []*rtsv1.Entity {
	var out []*rtsv1.Entity
	for _, e := range s.entities {
		if e.GetKind() == rtsv1.EntityKind_ENTITY_KIND_RESOURCE && !e.GetPassable() && !s.depleted[e.GetId()] && !empty(e) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *rtsv1.Entity) int { return compare(a.GetId(), b.GetId()) })
	return out
}

// empty reports whether a well's disclosed energy is known to be 0. Under
// fog of war the value may be an exact int, a range, or absent.
func empty(e *rtsv1.Entity) bool {
	v := e.GetAttributes()["wells.energy"]
	if r := v.GetRangeValue(); r != nil {
		return r.GetHigh() == 0
	}
	_, isInt := v.GetKind().(*rtsv1.Value_IntValue)
	return isInt && v.GetIntValue() == 0
}

func idle(e *rtsv1.Entity) bool {
	a := e.GetAttributes()
	return a["movement.destination"] == nil && a["automation.behavior"] == nil && a["energy.mining_well"] == nil
}

func compare(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func dist(a, b *rtsv1.Position) int32 {
	return max(abs(a.GetX()-b.GetX()), abs(a.GetY()-b.GetY()))
}

func abs(v int32) int32 { return max(v, -v) }

type bot struct {
	game    rtsv1.GameServiceClient
	session string
	s       *state
	seq     int
	nonce   uint32
	rnd     *rand.Rand
}

func (b *bot) ctx(parent context.Context) context.Context {
	return metadata.AppendToOutgoingContext(parent, "authorization", "Bearer "+b.session)
}

func (b *bot) submit(ctx context.Context, desc string, msg proto.Message) {
	payload, err := anypb.New(msg)
	if err != nil {
		log.Fatal(err)
	}
	b.seq++
	// Command IDs must be unique per player: resubmitting an ID returns the
	// original acknowledgment without scheduling it again. The per-run
	// nonce keeps a restarted bot from colliding with its earlier IDs.
	id := fmt.Sprintf("bot-%x-%d", b.nonce, b.seq)
	ack, err := b.game.SubmitCommand(b.ctx(ctx), &rtsv1.SubmitCommandRequest{CommandId: id, Payload: payload})
	switch {
	case err != nil:
		log.Printf("submit %s: %v", desc, err)
	case !ack.GetAccepted():
		log.Printf("submit %s rejected: %s", desc, ack.GetRejectionReason())
	default:
		b.s.pending[id] = desc
	}
}

// think issues this tick's commands, spending at most the unit budget.
func (b *bot) think(ctx context.Context) {
	switch b.s.match.GetState() {
	case rtsv1.MatchState_MATCH_STATE_READY:
		if !b.s.match.GetYouReady() && b.s.tick%10 == 0 { // retry until the server reports us ready
			b.submit(ctx, "ready", &matchv1.SetReadyCommand{Ready: true})
		}
		return
	case rtsv1.MatchState_MATCH_STATE_RUNNING:
	default: // paused or finished: gameplay commands would be rejected
		return
	}
	if b.s.tick%10 != 0 { // persistent orders do the work; re-plan every 10 ticks
		return
	}
	budget := b.s.budget
	if budget == 0 {
		budget = 1 << 30
	}
	units := b.s.own()
	var free []*rtsv1.Entity
	for _, u := range units {
		if idle(u) {
			free = append(free, u)
		}
	}
	if len(free) == 0 {
		return
	}
	if wells := b.s.wells(); len(wells) > 0 && len(units) >= 2 {
		// Mine the nearest known well with up to 3 idle units, delivering
		// to the unit closest to the well (the "depot").
		w := wells[0]
		slices.SortFunc(units, func(a, c *rtsv1.Entity) int {
			return int(dist(a.GetPosition(), w.GetPosition()) - dist(c.GetPosition(), w.GetPosition()))
		})
		depot := units[0]
		var miners []uint64
		for _, u := range free {
			if u.GetId() != depot.GetId() && len(miners) < min(3, budget) {
				miners = append(miners, u.GetId())
			}
		}
		if len(miners) > 0 {
			for _, m := range miners {
				b.s.assigned[m] = w.GetId()
			}
			b.submit(ctx, fmt.Sprintf("mine well %d with %d units", w.GetId(), len(miners)), &automationv1.AutomateCommand{
				UnitIds: miners,
				Behavior: &automationv1.AutomateCommand_MineAndDeliver{MineAndDeliver: &automationv1.MineAndDeliver{
					WellId: w.GetId(), ReceiverId: depot.GetId(),
				}},
			})
			return
		}
	}
	// Explore: one idle unit (cheap on budget) toward a random tile.
	u := free[b.rnd.IntN(len(free))]
	dest := &rtsv1.Position{X: b.rnd.Int32N(b.s.width), Y: b.rnd.Int32N(b.s.height)}
	b.submit(ctx, fmt.Sprintf("explore %d → (%d,%d)", u.GetId(), dest.GetX(), dest.GetY()), &movementv1.GroupMoveCommand{
		UnitIds: []uint64{u.GetId()}, Destination: dest,
	})
}

// connect exchanges the join token for a session token.
func (b *bot) connect(ctx context.Context, token string) error {
	resp, err := b.game.Connect(ctx, &rtsv1.ConnectRequest{JoinToken: token})
	if err != nil {
		return err
	}
	w := resp.GetWorld()
	b.session = resp.GetSessionToken()
	b.s.me = resp.GetPlayerId()
	b.s.width, b.s.height = w.GetWidth(), w.GetHeight()
	b.s.budget = int(w.GetUnitCommandBudget())
	log.Printf("connected as %q (player %d): %dx%d world at %d Hz, budget %d, plugins %v",
		resp.GetPlayerName(), b.s.me, w.GetWidth(), w.GetHeight(), w.GetTickRateHz(), b.s.budget, w.GetPlugins())
	return nil
}

// play observes until maxTicks ticks have been seen, reconnecting the
// stream whenever it breaks. Every (re)subscription starts with a full
// snapshot, so the client state is rebuilt, never patched blindly.
func (b *bot) play(ctx context.Context, token string, maxTicks int) error {
	seen := 0
	for {
		stream, err := b.game.Observe(b.ctx(ctx), &rtsv1.ObserveRequest{})
		if err == nil {
			for {
				msg, rerr := stream.Recv()
				if rerr != nil {
					err = rerr
					break
				}
				if snap := msg.GetSnapshot(); snap != nil {
					b.s.applySnapshot(snap)
					log.Printf("snapshot at tick %d: %d entities visible, fog=%v", snap.GetTick(), len(snap.GetEntities()), snap.GetFogOfWar())
					continue
				}
				b.s.applyUpdate(msg.GetUpdate())
				b.think(ctx)
				if seen++; maxTicks > 0 && seen >= maxTicks {
					log.Printf("done after %d ticks: %d own units, %d entities in view, %d foreign entities seen so far",
						seen, len(b.s.own()), len(b.s.entities), len(b.s.lastSeen))
					return nil
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status.Code(err) == codes.Unauthenticated { // e.g. server restarted: sessions are in memory
			if cerr := b.connect(ctx, token); cerr != nil {
				return cerr
			}
		}
		log.Printf("stream ended (%v); reconnecting", err)
		time.Sleep(time.Second)
	}
}

// newRand is the bot's own randomness. Client-side randomness is fine: the
// server decides every outcome deterministically.
func newRand() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "rts-server gRPC address")
	token := flag.String("token", "red", "join token")
	ticks := flag.Int("ticks", 0, "stop after observing this many ticks (0 = run forever)")
	flag.Parse()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	b := &bot{
		game: rtsv1.NewGameServiceClient(conn),
		s: &state{lastSeen: map[uint64]uint64{}, pending: map[string]string{},
			assigned: map[uint64]uint64{}, depleted: map[uint64]bool{}},
		rnd: newRand(),
	}
	ctx := context.Background()
	b.nonce = uint32(time.Now().UnixNano())
	if err := b.connect(ctx, *token); err != nil {
		log.Fatal(err)
	}
	if err := b.play(ctx, *token, *ticks); err != nil {
		log.Fatal(err)
	}
}
