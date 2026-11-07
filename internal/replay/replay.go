// Package replay records matches and replays them without clients.
//
// A replay (format "mono-replay/1", JSON) holds everything needed to
// rebuild a match deterministically: the engine version and ruleset hash,
// the resolved configuration (seed and ID key filled in; join and admin
// tokens redacted), the players, and every executed command with its tick,
// issuer and per-issuer sequence number. To detect and locate divergence it
// also stores a digest of every tick's frame (results, events, changed and
// removed entities) and full state hashes at checkpoints and at the end.
//
// Verification rebuilds the game from the configuration and steps every
// tick with exactly the recorded commands (Engine.StepWith), comparing each
// frame digest and checkpoint hash. The first mismatching tick is reported.
//
// Compatibility: a replay reproduces only under the same engine version and
// ruleset (plugins, phases, effective configuration). Mismatches are
// reported; verification is still attempted.
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	_ "github.com/dakotaknutson/unit-strategy-game/internal/protoreg" // command message types
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Format is the format identifier.
const Format = "mono-replay/1"

// Replay is a recorded match.
type Replay struct {
	Format        string          `json:"format"`
	EngineVersion string          `json:"engine_version"`
	RulesetHash   string          `json:"ruleset_hash"`
	MatchID       string          `json:"match_id"`
	Seed          uint64          `json:"seed"`
	Config        json.RawMessage `json:"config"`
	Players       []Player        `json:"players"`
	FinalTick     uint64          `json:"final_tick"`
	Ticks         []Tick          `json:"ticks"`   // ticks with commands only
	Digests       []string        `json:"digests"` // frame digest per tick 1..FinalTick (hex)
	Checkpoints   []Checkpoint    `json:"checkpoints"`
	Result        json.RawMessage `json:"result,omitempty"`
}

// Player is a participant.
type Player struct {
	Slot int    `json:"slot"`
	Name string `json:"name"`
	ID   uint32 `json:"id"`
}

// Tick is the commands executed in one tick, in recorded order.
type Tick struct {
	Tick     uint64    `json:"tick"`
	Commands []Command `json:"commands"`
}

// Command is one executed command.
type Command struct {
	ID      string          `json:"id"`
	Player  uint32          `json:"player,omitempty"`
	Admin   bool            `json:"admin,omitempty"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Checkpoint is a full state hash.
type Checkpoint struct {
	Tick uint64 `json:"tick"`
	Hash string `json:"hash"`
}

// Digest summarizes a frame: results, events, changed entities (ID,
// position, attribute count) and removals. It is cheap enough to compute
// every tick and changes whenever observable outcomes change.
func Digest(f *sim.Frame) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "t%d|", f.Tick)
	for _, r := range f.Results {
		fmt.Fprintf(h, "r%s:%t:%s:%d|", r.CommandID, r.OK, r.Code, len(r.Units))
	}
	for _, e := range f.Events {
		fmt.Fprintf(h, "e%s:%d:%v|", e.Type, e.Entity, e.Fields)
	}
	for _, v := range f.Changed {
		fmt.Fprintf(h, "c%d:%d,%d:%d|", v.ID, v.Pos.X, v.Pos.Y, len(v.Attrs))
	}
	for _, id := range f.Removed {
		fmt.Fprintf(h, "x%d|", id)
	}
	return h.Sum64()
}

func hex64(v uint64) string { return fmt.Sprintf("%016x", v) }

// EncodePayload serializes a command payload: protobuf messages as
// protojson, kernel commands as JSON.
func EncodePayload(p any) (json.RawMessage, error) {
	if m, ok := p.(proto.Message); ok {
		return protojson.Marshal(m)
	}
	switch p.(type) {
	case sim.CreateEntity, sim.RemoveEntity, sim.MoveEntity:
		return json.Marshal(p)
	}
	return nil, fmt.Errorf("cannot encode payload %T", p)
}

// DecodePayload reverses EncodePayload for a command type.
func DecodePayload(typ string, raw json.RawMessage) (any, error) {
	switch typ {
	case sim.CmdCreateEntity:
		var v sim.CreateEntity
		return v, json.Unmarshal(raw, &v)
	case sim.CmdRemoveEntity:
		var v sim.RemoveEntity
		return v, json.Unmarshal(raw, &v)
	case sim.CmdMoveEntity:
		var v sim.MoveEntity
		return v, json.Unmarshal(raw, &v)
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(typ))
	if err != nil {
		return nil, fmt.Errorf("unknown command type %q: %w", typ, err)
	}
	m := mt.New().Interface()
	if err := protojson.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", typ, err)
	}
	return m, nil
}

// ---- recording -----------------------------------------------------------------------

// Recorder accumulates a match's replay from its frames. Record must be
// called with every frame, on the simulation goroutine (it reads the world
// for checkpoint hashes); Snapshot may be called from any goroutine.
type Recorder struct {
	mu       sync.Mutex
	r        Replay
	interval uint64
	err      error
}

// NewRecorder starts recording a freshly built game. interval is the
// number of ticks between full state hashes (0 = only the initial one).
func NewRecorder(g *game.Game, matchID string, interval uint64) (*Recorder, error) {
	cfg := *g.Resolved
	cfg.AdminToken = ""
	cfg.Players = append([]game.PlayerConfig(nil), cfg.Players...)
	for i := range cfg.Players {
		cfg.Players[i].JoinToken = "replay-slot-" + strconv.Itoa(cfg.Players[i].Slot)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	rec := &Recorder{interval: interval, r: Replay{
		Format: Format, EngineVersion: game.EngineVersion, RulesetHash: game.RulesetHash(g.Engine.Rules()),
		MatchID: matchID, Seed: g.Seed, Config: raw,
		Checkpoints: []Checkpoint{{Tick: uint64(g.Engine.World().Tick()), Hash: hex64(g.Engine.World().StateHash())}},
	}}
	for _, p := range g.Players {
		rec.r.Players = append(rec.r.Players, Player{Slot: p.Slot, Name: p.Name, ID: uint32(p.ID)})
	}
	return rec, nil
}

func (r *Recorder) lock()   { r.mu.Lock() }
func (r *Recorder) unlock() { r.mu.Unlock() }

// Record adds one frame. final forces a checkpoint (e.g. the match ended).
func (r *Recorder) Record(f *sim.Frame, w *sim.World, final bool) {
	var t Tick
	for _, c := range f.Commands {
		raw, err := EncodePayload(c.Payload)
		if err != nil {
			r.lock()
			r.err = errors.Join(r.err, fmt.Errorf("tick %d command %s: %w", f.Tick, c.ID, err))
			r.unlock()
			continue
		}
		t.Commands = append(t.Commands, Command{ID: c.ID, Player: uint32(c.Issuer.Player), Admin: c.Issuer.Admin, Seq: c.Seq, Type: c.Type, Payload: raw})
	}
	var cp *Checkpoint
	if final || (r.interval > 0 && uint64(f.Tick)%r.interval == 0) {
		cp = &Checkpoint{Tick: uint64(f.Tick), Hash: hex64(w.StateHash())}
	}
	d := hex64(Digest(f))
	r.lock()
	defer r.unlock()
	if len(t.Commands) > 0 {
		t.Tick = uint64(f.Tick)
		r.r.Ticks = append(r.r.Ticks, t)
	}
	r.r.Digests = append(r.r.Digests, d)
	r.r.FinalTick = uint64(f.Tick)
	if cp != nil && (len(r.r.Checkpoints) == 0 || r.r.Checkpoints[len(r.r.Checkpoints)-1].Tick != cp.Tick) {
		r.r.Checkpoints = append(r.r.Checkpoints, *cp)
	}
}

// SetResult attaches the match result (any JSON-serializable value).
func (r *Recorder) SetResult(v any) {
	raw, _ := json.Marshal(v)
	r.lock()
	r.r.Result = raw
	r.unlock()
}

// Snapshot returns a copy of the replay recorded so far, and any recording
// error (e.g. a payload that could not be encoded).
func (r *Recorder) Snapshot() (Replay, error) {
	r.lock()
	defer r.unlock()
	out := r.r
	out.Ticks = append([]Tick(nil), r.r.Ticks...)
	out.Digests = append([]string(nil), r.r.Digests...)
	out.Checkpoints = append([]Checkpoint(nil), r.r.Checkpoints...)
	return out, r.err
}

// ---- verification --------------------------------------------------------------------

// Report is a verification outcome.
type Report struct {
	OK                 bool   `json:"ok"`
	TicksReplayed      uint64 `json:"ticks_replayed"`
	EngineMatches      bool   `json:"engine_matches"`
	RulesetMatches     bool   `json:"ruleset_matches"`
	FirstDivergentTick uint64 `json:"first_divergent_tick,omitempty"`
	Detail             string `json:"detail,omitempty"`
	FinalHash          string `json:"final_hash"`
	CheckpointsChecked int    `json:"checkpoints_checked"`
}

// Parse reads a replay document.
func Parse(data []byte) (*Replay, error) {
	var r Replay
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse replay: %w", err)
	}
	if r.Format != Format {
		return nil, fmt.Errorf("unsupported replay format %q (want %q)", r.Format, Format)
	}
	if uint64(len(r.Digests)) != r.FinalTick {
		return nil, fmt.Errorf("replay has %d digests for %d ticks", len(r.Digests), r.FinalTick)
	}
	return &r, nil
}

// Verify rebuilds the match and replays every tick, comparing digests and
// checkpoint hashes. It returns the first divergence, if any. Errors are
// reserved for replays that cannot be run at all.
func Verify(r *Replay) (*Report, error) {
	dec := json.NewDecoder(bytes.NewReader(r.Config))
	dec.DisallowUnknownFields()
	var cfg game.Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("replay config: %w", err)
	}
	g, err := game.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("rebuild match: %w", err)
	}
	e := g.Engine
	rep := &Report{EngineMatches: r.EngineVersion == game.EngineVersion, RulesetMatches: r.RulesetHash == game.RulesetHash(e.Rules())}
	checks := map[uint64]string{}
	for _, c := range r.Checkpoints {
		checks[c.Tick] = c.Hash
	}
	diverge := func(t uint64, format string, args ...any) (*Report, error) {
		rep.FirstDivergentTick, rep.Detail = t, fmt.Sprintf(format, args...)
		rep.FinalHash = hex64(e.World().StateHash())
		return rep, nil
	}
	if h, ok := checks[0]; ok {
		rep.CheckpointsChecked++
		if got := hex64(e.World().StateHash()); got != h {
			return diverge(0, "initial state hash %s, recorded %s (configuration or generation differs)", got, h)
		}
	}
	next := 0
	for t := uint64(1); t <= r.FinalTick; t++ {
		var cmds []sim.Command
		if next < len(r.Ticks) && r.Ticks[next].Tick == t {
			for _, c := range r.Ticks[next].Commands {
				payload, err := DecodePayload(c.Type, c.Payload)
				if err != nil {
					return nil, fmt.Errorf("tick %d command %s: %w", t, c.ID, err)
				}
				cmds = append(cmds, sim.Command{ID: c.ID, Issuer: sim.Principal{Player: sim.PlayerID(c.Player), Admin: c.Admin},
					Type: c.Type, Payload: payload, Seq: c.Seq, Tick: sim.Tick(t)})
			}
			next++
		}
		f := e.StepWith(cmds)
		rep.TicksReplayed = t
		if got := hex64(Digest(f)); got != r.Digests[t-1] {
			return diverge(t, "frame digest %s, recorded %s", got, r.Digests[t-1])
		}
		if h, ok := checks[t]; ok {
			rep.CheckpointsChecked++
			if got := hex64(e.World().StateHash()); got != h {
				return diverge(t, "state hash %s, recorded %s", got, h)
			}
		}
	}
	rep.OK = true
	rep.FinalHash = hex64(e.World().StateHash())
	return rep, nil
}
