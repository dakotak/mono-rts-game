// Package scale builds repeatable large-world scenarios and measures the
// authoritative simulation tick (Engine.Step: commands, every plugin phase
// and, with fog of war, per-player observation computation). It excludes
// networking and per-client serialization, which run outside the tick.
//
// Layout: units sit on a lattice with spacing 2 (25% density) at odd
// coordinates, owners alternating in a checkerboard (so every unit has an
// enemy two tiles away). Wells, when present, sit at (2+4a, 2+4b): every
// unit is diagonally adjacent to exactly one well.
//
// Everything is seeded: the same scenario, size and seed produce the same
// world, commands and state hashes.
package scale

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"time"

	automationv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/automation/v1"
	combatv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/combat/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	logisticsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/logistics/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/automation"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/combat"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/logistics"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/movement"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// Scenarios, in report order.
var Scenarios = []string{"idle", "movement", "mining", "sensors", "logistics", "combat", "mixed"}

// Result is one measured run.
type Result struct {
	Scenario         string
	Units            int
	Ticks            int
	Setup            time.Duration
	P50, P95, Max    time.Duration
	Mean             time.Duration
	AllocsPerTick    uint64
	BytesPerTick     uint64
	HeapMB           float64 // live heap after the run (after GC)
	EventsPerTick    float64
	ChangedPerTick   float64 // entity views changed per tick (owner deltas)
	ObservedPerTick  float64 // foreign-entity observation upserts per tick (fog)
	Hash             uint64
	BudgetFraction95 float64 // P95 / tick interval
}

// TickInterval is the tick budget the results are compared against (10 Hz).
const TickInterval = 100 * time.Millisecond

type world struct {
	g       *game.Game
	e       *sim.Engine
	side    int32
	units   []sim.EntityID // lattice order
	pos     []sim.Pos
	owner   []int // slot
	wells   bool
	r       *rand.Rand
	churn   int // random move orders per tick
	seq     int
	players []sim.PlayerID
}

func lattice(n int) (side int32, k int) {
	k = int(math.Ceil(math.Sqrt(float64(n))))
	return int32(2*k + 2), k
}

// build creates the world for a scenario.
func build(scenario string, n int, seed uint64) (*world, error) {
	side, k := lattice(n)
	fog := scenario == "sensors" || scenario == "mixed"
	wells := scenario == "mining" || scenario == "logistics" || scenario == "mixed"
	cfg := &game.Config{
		World:   game.WorldConfig{Width: side, Height: side, Seed: seed, TickRateHz: 10, IDKey: fmt.Sprintf("%032x", seed)},
		Players: []game.PlayerConfig{{Slot: 1, JoinToken: "a"}, {Slot: 2, JoinToken: "b"}},
		Plugins: map[string]json.RawMessage{
			"energy":     json.RawMessage(`{"unit_capacity": 200, "initial_energy": 50, "extraction_rate": 3, "transfer_rate": 5}`),
			"combat":     json.RawMessage(`{"attack": {"default": 1}, "max_health": {"default": 1000000}, "range": {"default": 2}, "cooldown": {"default": 3}}`),
			"logistics":  json.RawMessage(`{"range": {"default": 2}}`),
			"automation": json.RawMessage(`{"eval_interval": 5, "max_evaluations_per_tick": 5000}`),
		},
	}
	if fog {
		cfg.Plugins["visibility"] = json.RawMessage(`{
			"enabled": true, "vision": {"default": 6}, "sensor": {"default": 8}, "identification": {"default": 3},
			"detailed_threshold": 2,
			"disclosure": [{"key": "combat.health", "basic": 50, "estimated": 10, "detailed": 1}, {"key": "energy.stored", "estimated": 25}]
		}`)
	}
	w := &world{side: side, wells: wells, r: rand.New(rand.NewPCG(seed, 99))}
	for i := range n {
		x, y := int32(1+2*(i%k)), int32(1+2*(i/k))
		slot := 1 + (i%k+i/k)%2
		cfg.Units = append(cfg.Units, game.UnitBlock{Owner: slot, X: x, Y: y})
		w.pos = append(w.pos, sim.Pos{X: x, Y: y})
		w.owner = append(w.owner, slot)
	}
	if wells {
		var ws []map[string]any
		for y := int32(2); y < side-1; y += 4 {
			for x := int32(2); x < side-1; x += 4 {
				ws = append(ws, map[string]any{"x": x, "y": y, "unlimited": true, "extraction_rate": 12})
			}
		}
		b, _ := json.Marshal(map[string]any{"wells": ws})
		cfg.Plugins["wells"] = b
	}
	g, err := game.New(cfg)
	if err != nil {
		return nil, err
	}
	w.g, w.e = g, g.Engine
	for _, p := range g.Players {
		w.players = append(w.players, p.ID)
	}
	for _, p := range w.pos {
		id, ok := w.e.World().EntityAt(p)
		if !ok {
			return nil, fmt.Errorf("no unit at %v", p)
		}
		w.units = append(w.units, id)
	}
	return w, nil
}

func (w *world) submit(slot int, typ string, payload any) {
	w.seq++
	_, _ = w.e.Submit(sim.Command{ID: fmt.Sprint("s", w.seq), Issuer: sim.Principal{Player: w.players[slot-1]}, Type: typ, Payload: payload})
}

// bySlot groups lattice indices by owner (index slot-1), in lattice
// order. A slice, not a map: iteration order must be deterministic.
func (w *world) bySlot(idx []int) [][]int {
	m := make([][]int, len(w.players))
	for _, i := range idx {
		m[w.owner[i]-1] = append(m[w.owner[i]-1], i)
	}
	return m
}

func (w *world) ids(idx []int) []uint64 {
	out := make([]uint64, len(idx))
	for j, i := range idx {
		out[j] = uint64(w.units[i])
	}
	return out
}

func chunks(idx []int, n int, fn func([]int)) {
	for len(idx) > 0 {
		c := min(n, len(idx))
		fn(idx[:c])
		idx = idx[c:]
	}
}

// moveRandom orders units to random destinations within 12 tiles.
func (w *world) moveRandom(idx []int) {
	for si, is := range w.bySlot(idx) {
		slot := si + 1
		chunks(is, 1000, func(c []int) {
			m := &movementv1.MoveCommand{}
			for _, i := range c {
				e, ok := w.e.World().Entity(w.units[i])
				if !ok {
					continue
				}
				d := &rtsv1.Position{
					X: min(max(e.Pos.X+int32(w.r.IntN(25))-12, 0), w.side-1),
					Y: min(max(e.Pos.Y+int32(w.r.IntN(25))-12, 0), w.side-1),
				}
				m.Orders = append(m.Orders, &movementv1.UnitDestination{UnitId: uint64(w.units[i]), Destination: d})
			}
			w.submit(slot, movement.CommandMove, m)
		})
	}
}

// wellOf returns the well adjacent to lattice unit i.
func (w *world) wellOf(i int) (sim.EntityID, bool) {
	p := w.pos[i]
	// p is odd; the adjacent well column is the nearest x ≡ 2 (mod 4).
	wp := sim.Pos{X: nearest2mod4(p.X), Y: nearest2mod4(p.Y)}
	id, ok := w.e.World().EntityAt(wp)
	return id, ok
}

func nearest2mod4(v int32) int32 {
	if v%4 == 1 {
		return v + 1
	}
	return v - 1
}

func (w *world) mine(idx []int) {
	byWell := map[sim.EntityID][]int{}
	var order []sim.EntityID
	for _, i := range idx {
		if wl, ok := w.wellOf(i); ok {
			if _, seen := byWell[wl]; !seen {
				order = append(order, wl)
			}
			byWell[wl] = append(byWell[wl], i)
		}
	}
	for _, wl := range order {
		for si, is := range w.bySlot(byWell[wl]) {
			if len(is) == 0 {
				continue
			}
			slot := si + 1
			w.submit(slot, energy.CommandMine, &energyv1.MineCommand{UnitIds: w.ids(is), WellId: uint64(wl)})
		}
	}
}

// attack: each unit attacks the enemy two tiles to its right (or left).
func (w *world) attack(idx []int, k int) {
	for _, i := range idx {
		j := i + 1
		if i%k == k-1 || j >= len(w.units) {
			j = i - 1
		}
		if j < 0 || w.owner[j] == w.owner[i] {
			continue
		}
		w.submit(w.owner[i], combat.CommandAttack, &combatv1.AttackCommand{UnitIds: []uint64{uint64(w.units[i])}, TargetId: uint64(w.units[j])})
	}
}

// supplyRows links each unit to the next own unit in its row two lattice
// steps away (same owner in a checkerboard), forming relay chains.
func (w *world) supplyRows(idx []int, k int) {
	in := map[int]bool{}
	for _, i := range idx {
		in[i] = true
	}
	for _, i := range idx {
		j := i + 2
		if i%k >= k-2 || !in[j] {
			continue
		}
		w.submit(w.owner[i], logistics.CommandSupply, &logisticsv1.SupplyCommand{UnitIds: []uint64{uint64(w.units[i])}, ReceiverId: uint64(w.units[j]), Reserve: 10})
	}
}

func (w *world) automate(idx []int, k int) {
	for _, i := range idx {
		var b *automationv1.AutomateCommand
		switch i % 3 {
		case 0:
			p := w.pos[i]
			b = &automationv1.AutomateCommand{Behavior: &automationv1.AutomateCommand_Guard{Guard: &automationv1.Guard{Position: &rtsv1.Position{X: p.X, Y: p.Y}, Radius: 4}}}
		case 1:
			if wl, ok := w.wellOf(i); ok && i+2 < len(w.units) && i%k < k-2 {
				b = &automationv1.AutomateCommand{Behavior: &automationv1.AutomateCommand_MineAndDeliver{MineAndDeliver: &automationv1.MineAndDeliver{WellId: uint64(wl), ReceiverId: uint64(w.units[i+2])}}}
			}
		default:
			b = &automationv1.AutomateCommand{Behavior: &automationv1.AutomateCommand_AutoSalvage{AutoSalvage: &automationv1.AutoSalvage{Radius: 6}}}
		}
		if b != nil {
			b.UnitIds = []uint64{uint64(w.units[i])}
			w.submit(w.owner[i], automation.CommandAutomate, b)
		}
	}
}

// setup issues the scenario's standing orders (executed in the first,
// unmeasured tick) and sets per-tick churn.
func (w *world) setup(scenario string) {
	n := len(w.units)
	_, k := lattice(n)
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	part := func(m int) []int {
		var out []int
		for _, i := range all {
			if i%4 == m {
				out = append(out, i)
			}
		}
		return out
	}
	switch scenario {
	case "movement":
		w.moveRandom(all)
		w.churn = n / 20
	case "mining":
		w.mine(all)
	case "sensors":
		w.churn = n / 20
	case "logistics":
		w.mine(part(0))
		w.supplyRows(append(part(1), part(2)...), k)
		w.automate(part(3), k)
	case "combat":
		w.attack(all, k)
	case "mixed":
		w.churn = n / 40
		w.mine(part(0))
		w.attack(part(1), k)
		w.supplyRows(part(2), k)
		w.automate(part(3), k)
	}
}

// Run builds a scenario with n units, runs one setup tick and warm ticks,
// then measures ticks.
func Run(scenario string, n, ticks int, seed uint64) (Result, error) {
	res := Result{Scenario: scenario, Units: n, Ticks: ticks}
	start := time.Now()
	w, err := build(scenario, n, seed)
	if err != nil {
		return res, err
	}
	w.setup(scenario)
	w.e.Step()
	res.Setup = time.Since(start)
	step := func() *sim.Frame {
		if w.churn > 0 {
			idx := make([]int, w.churn)
			for j := range idx {
				idx[j] = w.r.IntN(len(w.units))
			}
			slices.Sort(idx)
			idx = slices.Compact(idx)
			w.moveRandom(idx)
		}
		return w.e.Step()
	}
	for range 5 {
		step()
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	durs := make([]time.Duration, 0, ticks)
	var events, changed, observed int
	for range ticks {
		t0 := time.Now()
		f := step()
		durs = append(durs, time.Since(t0))
		events += len(f.Events)
		changed += len(f.Changed)
		for _, o := range f.Observations {
			observed += len(o.Upserts)
		}
	}
	runtime.ReadMemStats(&after)
	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)
	slices.Sort(durs)
	var sum time.Duration
	for _, d := range durs {
		sum += d
	}
	res.P50 = durs[len(durs)/2]
	res.P95 = durs[min(len(durs)-1, len(durs)*95/100)]
	res.Max = durs[len(durs)-1]
	res.Mean = sum / time.Duration(ticks)
	res.AllocsPerTick = (after.Mallocs - before.Mallocs) / uint64(ticks)
	res.BytesPerTick = (after.TotalAlloc - before.TotalAlloc) / uint64(ticks)
	res.HeapMB = float64(live.HeapAlloc) / (1 << 20)
	res.EventsPerTick = float64(events) / float64(ticks)
	res.ChangedPerTick = float64(changed) / float64(ticks)
	res.ObservedPerTick = float64(observed) / float64(ticks)
	res.Hash = w.e.World().StateHash()
	res.BudgetFraction95 = float64(res.P95) / float64(TickInterval)
	return res, nil
}
