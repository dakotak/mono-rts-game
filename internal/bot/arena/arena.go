// Package arena runs computer players against each other for evaluation.
// It hosts a Mono server in process and connects each bot through the
// public GameService gRPC API (over an in-memory connection), exactly as a
// remote bot would. The arena is the orchestrator, not a player: it uses the
// server directly only to step ticks and read the final result.
//
// Matches run in lockstep: the arena executes a tick only after every bot
// has processed the previous one and submitted its commands. That makes
// evaluation fast (no wall clock) and reproducible: with the same map seed
// and bot seeds, a match replays identically.
package arena

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/client"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/server"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// BotSpec is one computer player in a match.
type BotSpec struct {
	Slot        int
	Difficulty  int
	Personality string
	Seed        uint64
	// Overrides adjusts individual difficulty parameters (JSON object of
	// difficulty.Params fields), for tuning experiments.
	Overrides json.RawMessage
	// Extensions (to test a new strategy idea against the built-ins).
	Extensions bot.Extensions
}

// Spec is one match.
type Spec struct {
	Config   *game.Config // must define the bots' slots and join tokens
	MapSeed  uint64
	Bots     []BotSpec
	MaxTicks int // safety cap (the configuration's time limit normally ends matches)
	// Logger receives the bots' structured logs (nil = discard).
	Logger *slog.Logger
	// Debug enables the bots' utility-score logging (with a debug Logger).
	Debug bool
	// Inspect, when set, is called by each bot's goroutine after every tick
	// it processed (diagnostics and tests).
	Inspect func(slot int, b *ai.Bot)
}

// BotResult is one bot's outcome.
type BotResult struct {
	Slot                int            `json:"slot"`
	Difficulty          int            `json:"difficulty"`
	Personality         string         `json:"personality"`
	Won                 bool           `json:"won"`
	Score               int64          `json:"score"`
	Units               int64          `json:"units"`
	Embodied            int64          `json:"embodied"`
	Stored              int64          `json:"stored_energy"`
	EnergyMined         int64          `json:"energy_mined"`
	Kills               int64          `json:"kills"`
	UnitsLost           int64          `json:"units_lost"`
	InvestmentDestroyed int64          `json:"investment_destroyed"`
	Commands            int            `json:"commands"`
	UnitCommands        int            `json:"unit_commands"`
	FailedCommands      int            `json:"failed_commands"`
	AvgDecisionUs       float64        `json:"avg_decision_us"`
	MaxDecisionUs       int64          `json:"max_decision_us"`
	FinalStrategy       string         `json:"final_strategy"`
	OrdersByKind        map[string]int `json:"orders_by_kind,omitempty"`
	StrategySwitches    int            `json:"strategy_switches"`
}

// Result is a match outcome.
type Result struct {
	MapSeed    uint64        `json:"map_seed"`
	WinnerSlot int           `json:"winner_slot"` // 0 = draw / no winner
	Reason     string        `json:"reason"`
	Ticks      int64         `json:"ticks"`
	Bots       []BotResult   `json:"bots"`
	Elapsed    time.Duration `json:"elapsed_ns"`
}

type player struct {
	spec   BotSpec
	runner *client.Runner
	ticks  chan uint64   // each processed tick (lockstep handoff)
	done   chan struct{} // closed when the runner stops
	last   uint64
	seen   bool // processed its first observation
}

// Play runs one match to completion.
func Play(ctx context.Context, spec Spec) (Result, error) {
	start := time.Now()
	cfg := *spec.Config
	cfg.World.Seed, cfg.World.RandomSeed = spec.MapSeed, false
	// IDs derive from a secret key, random per match by default; a key
	// derived from the map seed makes matches reproducible.
	key := sha256.Sum256(fmt.Appendf(nil, "mono-arena/%d", spec.MapSeed))
	cfg.World.IDKey = hex.EncodeToString(key[:])
	g, err := game.New(&cfg)
	if err != nil {
		return Result{}, err
	}
	srv := server.New(&cfg, g)
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tokens := map[int]string{}
	for _, p := range cfg.Players {
		tokens[p.Slot] = p.JoinToken
	}
	var players []*player
	var wg sync.WaitGroup
	for _, bs := range spec.Bots {
		tok, ok := tokens[bs.Slot]
		if !ok {
			return Result{}, fmt.Errorf("no player slot %d in the configuration", bs.Slot)
		}
		conn, err := grpc.NewClient("passthrough:///arena",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return Result{}, err
		}
		defer conn.Close()
		p := &player{spec: bs, ticks: make(chan uint64, 4), done: make(chan struct{})}
		bc := bot.Config{Token: tok, Difficulty: bs.Difficulty, Personality: bs.Personality, Seed: bs.Seed, Deterministic: true, Debug: spec.Debug,
			DifficultyOverrides: bs.Overrides}
		if err := bc.Validate(); err != nil {
			return Result{}, err
		}
		p.runner = client.New(conn, client.Options{
			Token: tok,
			NewBot: func(k *state.Knowledge) *ai.Bot {
				var lg *slog.Logger
				if spec.Logger != nil {
					lg = spec.Logger.With("slot", bs.Slot, "level", bs.Difficulty)
				}
				return bc.NewBot(k, lg, bs.Extensions)
			},
			OnTick: func(t uint64) {
				if spec.Inspect != nil && p.runner.Bot != nil {
					spec.Inspect(bs.Slot, p.runner.Bot)
				}
				select {
				case p.ticks <- t:
				case <-ctx.Done():
				}
			},
			Backoff: 10 * time.Millisecond,
			Logger:  spec.Logger,
		})
		players = append(players, p)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.runner.Run(ctx); err != nil && spec.Logger != nil {
				spec.Logger.Error("bot stopped", "slot", bs.Slot, "err", err)
			}
			close(p.done)
		}()
	}
	// waitAll blocks until every bot processed tick t (or stopped, or got
	// stuck for 10s: a failing bot must not hang the evaluation).
	waitAll := func(t uint64) {
		for _, p := range players {
			for !p.seen || p.last < t {
				select {
				case v := <-p.ticks:
					p.last, p.seen = v, true
				case <-p.done:
					p.seen, p.last = true, ^uint64(0)
				case <-time.After(10 * time.Second):
					p.seen, p.last = true, t
				}
			}
		}
	}
	engine := srv.Engine()
	waitAll(0) // initial snapshots processed
	maxTicks := spec.MaxTicks
	if maxTicks == 0 {
		maxTicks = 20000
	}
	var st match.Status
	for i := 0; i < maxTicks; i++ {
		f := engine.Step()
		waitAll(uint64(f.Tick))
		st = srv.Hub().Status()
		if st.State == match.Finished {
			break
		}
	}
	cancel()
	wg.Wait()

	res := Result{MapSeed: spec.MapSeed, Reason: st.Reason, Ticks: st.RunTicks, Elapsed: time.Since(start)}
	if st.State != match.Finished {
		res.Reason = "max_ticks"
	}
	for _, p := range players {
		var pid uint32
		for _, pl := range g.Players {
			if pl.Slot == p.spec.Slot {
				pid = uint32(pl.ID)
			}
		}
		br := BotResult{Slot: p.spec.Slot, Difficulty: p.spec.Difficulty, Personality: p.spec.Personality}
		if i := st.Index(sim.PlayerID(pid)); i >= 0 {
			s, fin := st.Stats[i], st.Final[i]
			br.Score, br.Units, br.Embodied, br.Stored = st.Scores[i], fin.Units, fin.Embodied, fin.Stored
			br.EnergyMined, br.Kills, br.UnitsLost, br.InvestmentDestroyed = s.EnergyMined, s.Kills, s.UnitsLost, s.InvestmentDestroyed
			if st.State == match.Finished && !st.Draw && uint32(st.Winner) == pid {
				br.Won = true
				res.WinnerSlot = p.spec.Slot
			}
		}
		if b := p.runner.Bot; b != nil {
			s := b.Status
			br.Commands, br.UnitCommands, br.FailedCommands = s.Commands.Submitted, s.Commands.UnitsCharged, s.Commands.Failed
			br.AvgDecisionUs = float64(s.TotalDecision.Microseconds()) / float64(max(s.Decisions, 1))
			br.MaxDecisionUs = s.MaxDecision.Microseconds()
			br.FinalStrategy, br.StrategySwitches = s.Strategy, s.StrategySwitches
			br.OrdersByKind = s.Commands.ByKind
			if st.State != match.Finished {
				br.Units = int64(len(b.K.OwnUnits())) // the server freezes totals only at the end
			}
		}
		res.Bots = append(res.Bots, br)
	}
	return res, nil
}
