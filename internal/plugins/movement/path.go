package movement

import (
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/terrain"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// PathLen is how many upcoming steps an order stores. Longer routes are
// planned again when the stored steps run out.
const PathLen = 32

// neighbours in fixed order (N, NE, E, SE, S, SW, W, NW): part of the
// deterministic tie-breaking.
var neighbours = [8]sim.Pos{{X: 0, Y: -1}, {X: 1, Y: -1}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0}, {X: -1, Y: -1}}

type node struct {
	f, h, d2 int64
	idx      int32
}

func (a node) less(b node) bool {
	switch {
	case a.f != b.f:
		return a.f < b.f
	case a.h != b.h:
		return a.h < b.h
	case a.d2 != b.d2:
		return a.d2 < b.d2 // prefer straighter routes among equals
	}
	return a.idx < b.idx
}

// planner is A* over the grid with reusable scratch buffers (no per-search
// allocation for the grid-sized arrays). It is not simulation state: the
// same inputs always give the same path.
type planner struct {
	width, height int32
	g             []int64
	came          []int32
	seen, closed  []uint32
	stamp         uint32
	heap          []node
}

func (pl *planner) reset(w *sim.World) {
	n := int(w.Width()) * int(w.Height())
	if pl.width != w.Width() || pl.height != w.Height() || len(pl.g) != n {
		pl.width, pl.height = w.Width(), w.Height()
		pl.g, pl.came = make([]int64, n), make([]int32, n)
		pl.seen, pl.closed = make([]uint32, n), make([]uint32, n)
		pl.stamp = 0
	}
	pl.stamp++
	if pl.stamp == 0 { // wrapped: clear
		clear(pl.seen)
		clear(pl.closed)
		pl.stamp = 1
	}
	pl.heap = pl.heap[:0]
}

func (pl *planner) push(n node) {
	pl.heap = append(pl.heap, n)
	i := len(pl.heap) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !pl.heap[i].less(pl.heap[parent]) {
			break
		}
		pl.heap[i], pl.heap[parent] = pl.heap[parent], pl.heap[i]
		i = parent
	}
}

func (pl *planner) pop() node {
	top := pl.heap[0]
	last := len(pl.heap) - 1
	pl.heap[0] = pl.heap[last]
	pl.heap = pl.heap[:last]
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(pl.heap) && pl.heap[l].less(pl.heap[m]) {
			m = l
		}
		if r < len(pl.heap) && pl.heap[r].less(pl.heap[m]) {
			m = r
		}
		if m == i {
			return top
		}
		pl.heap[i], pl.heap[m] = pl.heap[m], pl.heap[i]
		i = m
	}
}

func cheb(a, b sim.Pos) int64 {
	dx, dy := int64(a.X-b.X), int64(a.Y-b.Y)
	return max(dx, -dx, dy, -dy)
}

// plan finds a cheapest path from -> to (step costs from terrain.StepCost;
// static obstacles impassable; avoid marks extra tiles to route around,
// e.g. units that keep blocking). It expands at most budget nodes. If the
// destination cannot be reached it returns the path to the explored tile
// closest to it (ties: lower cost, then tile index), and reached=false.
// The returned steps exclude from and are capped at PathLen.
func (pl *planner) plan(w *sim.World, from, to sim.Pos, avoid func(sim.Pos) bool, budget int) (steps []sim.Pos, reached bool) {
	pl.reset(w)
	W := int64(pl.width)
	idx := func(p sim.Pos) int32 { return int32(int64(p.Y)*W + int64(p.X)) }
	pos := func(i int32) sim.Pos { return sim.Pos{X: int32(int64(i) % W), Y: int32(int64(i) / W)} }
	h := func(p sim.Pos) (int64, int64) {
		dx, dy := int64(p.X-to.X), int64(p.Y-to.Y)
		return max(dx, -dx, dy, -dy), dx*dx + dy*dy
	}
	open := func(p sim.Pos) bool {
		if !w.InBounds(p) {
			return false
		}
		if _, ok := terrain.StepCost(w, p); !ok {
			return false
		}
		return p == to || avoid == nil || !avoid(p)
	}
	start := idx(from)
	pl.g[start], pl.came[start], pl.seen[start] = 0, -1, pl.stamp
	h0, d0 := h(from)
	pl.push(node{f: h0, h: h0, d2: d0, idx: start})
	best, bestH, bestG := start, h0, int64(0)
	goal := idx(to)
	expanded := 0
	for len(pl.heap) > 0 && expanded < budget {
		n := pl.pop()
		if pl.closed[n.idx] == pl.stamp {
			continue
		}
		pl.closed[n.idx] = pl.stamp
		expanded++
		if n.idx == goal {
			best, reached = goal, true
			break
		}
		if n.h < bestH || (n.h == bestH && (pl.g[n.idx] < bestG || (pl.g[n.idx] == bestG && n.idx < best))) {
			best, bestH, bestG = n.idx, n.h, pl.g[n.idx]
		}
		cur := pos(n.idx)
		for _, d := range neighbours {
			np := sim.Pos{X: cur.X + d.X, Y: cur.Y + d.Y}
			if !open(np) {
				continue
			}
			if d.X != 0 && d.Y != 0 {
				// No corner cutting past static obstacles.
				if _, ok := terrain.StepCost(w, sim.Pos{X: cur.X + d.X, Y: cur.Y}); !ok {
					continue
				}
				if _, ok := terrain.StepCost(w, sim.Pos{X: cur.X, Y: cur.Y + d.Y}); !ok {
					continue
				}
			}
			cost, _ := terrain.StepCost(w, np)
			ni := idx(np)
			ng := pl.g[n.idx] + cost
			if pl.seen[ni] == pl.stamp && ng >= pl.g[ni] {
				continue
			}
			pl.seen[ni], pl.g[ni], pl.came[ni] = pl.stamp, ng, n.idx
			nh, nd := h(np)
			pl.push(node{f: ng + nh, h: nh, d2: nd, idx: ni})
		}
	}
	// Reconstruct from best back to start.
	var rev []sim.Pos
	for i := best; i != start && i >= 0; i = pl.came[i] {
		rev = append(rev, pos(i))
	}
	n := min(len(rev), PathLen)
	steps = make([]sim.Pos, 0, n)
	for k := len(rev) - 1; k >= len(rev)-n; k-- {
		steps = append(steps, rev[k])
	}
	return steps, reached
}

// slots finds up to n free tiles around center for a group, in breadth-
// first order over enterable tiles (ties: neighbour order), skipping tiles
// held by units outside the group. It visits at most limit tiles.
func (pl *planner) slots(w *sim.World, center sim.Pos, n int, inGroup func(sim.EntityID) bool, limit int) []sim.Pos {
	pl.reset(w)
	W := int64(pl.width)
	idx := func(p sim.Pos) int32 { return int32(int64(p.Y)*W + int64(p.X)) }
	enterable := func(p sim.Pos) bool {
		if _, ok := terrain.StepCost(w, p); !ok {
			return false
		}
		if id, taken := w.EntityAt(p); taken && !inGroup(id) {
			return false
		}
		return true
	}
	var out []sim.Pos
	if !w.InBounds(center) {
		return nil
	}
	queue := []sim.Pos{center}
	pl.seen[idx(center)] = pl.stamp
	for head := 0; head < len(queue) && len(out) < n && head < limit; head++ {
		p := queue[head]
		if enterable(p) {
			out = append(out, p)
		} else if _, ok := terrain.StepCost(w, p); !ok && p != center {
			continue // do not expand through walls
		}
		for _, d := range neighbours {
			np := sim.Pos{X: p.X + d.X, Y: p.Y + d.Y}
			if !w.InBounds(np) || pl.seen[idx(np)] == pl.stamp {
				continue
			}
			pl.seen[idx(np)] = pl.stamp
			queue = append(queue, np)
		}
	}
	return out
}
