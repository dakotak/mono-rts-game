// Package host runs computer players on request: the small, explicit
// orchestration boundary between "a human asks for a computer opponent" and
// "a bot joins the match".
//
// The host holds the join tokens of the player slots it may fill (from the
// server's configuration or flags) and nothing else: no admin token, no
// access to the simulation. A bot it starts is an ordinary GameService
// client using that slot's join token, exactly like `mono-bot --token ...`.
// Slots that are not configured as bot slots can never be taken, so a
// player cannot hijack another human's seat through the host.
//
// HTTP API (JSON):
//
//	GET    /api/bots           -> {"slots":[Slot...], "difficulties":[...], "personalities":[...]}
//	POST   /api/bots           {"slot":2,"difficulty":6,"personality":"aggressive"} -> Slot
//	DELETE /api/bots/{slot}    -> Slot (stopped)
//	GET    /api/bots/{slot}    -> Slot with the bot's diagnostics
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/ai"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/client"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/difficulty"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// SlotConfig is a player slot the host may fill.
type SlotConfig struct {
	Slot  int    `json:"slot"`
	Name  string `json:"name"`
	Token string `json:"-"` // never sent to clients
}

// Slot is a slot's public state.
type Slot struct {
	Slot        int             `json:"slot"`
	Name        string          `json:"name"`
	Running     bool            `json:"running"`
	Difficulty  int             `json:"difficulty,omitempty"`
	Personality string          `json:"personality,omitempty"`
	Started     *time.Time      `json:"started,omitempty"`
	Error       string          `json:"error,omitempty"`
	Status      json.RawMessage `json:"status,omitempty"`
}

type running struct {
	cfg     bot.Config
	cancel  context.CancelFunc
	done    chan struct{}
	runner  *client.Runner
	mu      sync.Mutex
	status  json.RawMessage // diagnostics snapshot, refreshed by the bot
	started time.Time
	err     error
}

// Host manages bots for a set of slots.
type Host struct {
	conn  grpc.ClientConnInterface
	slots []SlotConfig
	log   *slog.Logger
	mu    sync.Mutex
	bots  map[int]*running
}

// New returns a host connecting bots through conn.
func New(conn grpc.ClientConnInterface, slots []SlotConfig, lg *slog.Logger) *Host {
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	slots = slices.Clone(slots)
	slices.SortFunc(slots, func(a, b SlotConfig) int { return a.Slot - b.Slot })
	return &Host{conn: conn, slots: slots, log: lg, bots: map[int]*running{}}
}

func (h *Host) slot(n int) (SlotConfig, bool) {
	for _, s := range h.slots {
		if s.Slot == n {
			return s, true
		}
	}
	return SlotConfig{}, false
}

// Start runs a bot in a slot.
func (h *Host) Start(slot, level int, personality string) (Slot, error) {
	sc, ok := h.slot(slot)
	if !ok {
		return Slot{}, fmt.Errorf("slot %d is not available for computer players", slot)
	}
	cfg := bot.Defaults()
	cfg.Token, cfg.Difficulty, cfg.PlayerName = sc.Token, level, sc.Name
	if personality != "" {
		cfg.Personality = personality
	}
	if err := cfg.Validate(); err != nil {
		return Slot{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.bots[slot]; r != nil && r.err == nil && !closed(r.done) {
		return Slot{}, fmt.Errorf("slot %d already has a computer player", slot)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cfg: cfg, cancel: cancel, done: make(chan struct{}), started: time.Now()}
	lg := h.log.With("slot", slot, "difficulty", level, "personality", cfg.Personality)
	r.runner = client.New(h.conn, client.Options{
		Token:  cfg.Token,
		Logger: lg,
		NewBot: func(k *state.Knowledge) *ai.Bot { return cfg.NewBot(k, lg) },
		OnTick: func(tick uint64) {
			if tick%10 == 0 && r.runner.Bot != nil {
				data, _ := json.Marshal(r.runner.Bot.Status)
				r.mu.Lock()
				r.status = data
				r.mu.Unlock()
			}
		},
	})
	h.bots[slot] = r
	go func() {
		err := r.runner.Run(ctx)
		h.mu.Lock()
		r.err = err
		h.mu.Unlock()
		close(r.done)
		lg.Info("computer player stopped", "err", err)
	}()
	lg.Info("computer player started")
	return h.describe(sc, r, false), nil
}

// Stop stops a slot's bot (graceful: it finishes its tick).
func (h *Host) Stop(slot int) (Slot, error) {
	sc, ok := h.slot(slot)
	if !ok {
		return Slot{}, fmt.Errorf("slot %d is not available for computer players", slot)
	}
	h.mu.Lock()
	r := h.bots[slot]
	delete(h.bots, slot)
	h.mu.Unlock()
	if r == nil {
		return Slot{}, fmt.Errorf("slot %d has no computer player", slot)
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
	return Slot{Slot: sc.Slot, Name: sc.Name}, nil
}

// StopAll stops every bot (shutdown).
func (h *Host) StopAll() {
	for _, s := range h.slots {
		_, _ = h.Stop(s.Slot)
	}
}

// Slots lists the slots and their bots.
func (h *Host) Slots(detail bool) []Slot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Slot, 0, len(h.slots))
	for _, sc := range h.slots {
		out = append(out, h.describe(sc, h.bots[sc.Slot], detail))
	}
	return out
}

func closed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func (h *Host) describe(sc SlotConfig, r *running, detail bool) Slot {
	s := Slot{Slot: sc.Slot, Name: sc.Name}
	if r == nil {
		return s
	}
	t := r.started
	s.Running, s.Difficulty, s.Personality, s.Started = !closed(r.done), r.cfg.Difficulty, r.cfg.Personality, &t
	if r.err != nil {
		s.Error = r.err.Error()
	}
	if detail {
		r.mu.Lock()
		s.Status = r.status
		r.mu.Unlock()
	}
	return s
}

// Handler returns the HTTP API.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bots", func(w http.ResponseWriter, _ *http.Request) {
		var levels []map[string]any
		for _, p := range difficulty.Levels() {
			levels = append(levels, map[string]any{"level": p.Level, "name": p.Name})
		}
		names := make([]string, 0, len(difficulty.Personalities))
		for n := range difficulty.Personalities {
			names = append(names, n)
		}
		slices.Sort(names)
		reply(w, http.StatusOK, map[string]any{"slots": h.Slots(false), "difficulties": levels, "personalities": names})
	})
	mux.HandleFunc("POST /api/bots", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Slot        int    `json:"slot"`
			Difficulty  int    `json:"difficulty"`
			Personality string `json:"personality"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s, err := h.Start(req.Slot, req.Difficulty, req.Personality)
		if err != nil {
			reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /api/bots/{slot}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("slot"))
		for _, s := range h.Slots(true) {
			if s.Slot == n {
				reply(w, http.StatusOK, s)
				return
			}
		}
		reply(w, http.StatusNotFound, map[string]string{"error": "no such bot slot"})
	})
	mux.HandleFunc("DELETE /api/bots/{slot}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("slot"))
		s, err := h.Stop(n)
		if err != nil {
			reply(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, s)
	})
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrNoSlots is returned when a host is configured without bot slots.
var ErrNoSlots = errors.New("no player slots are configured for computer players")
