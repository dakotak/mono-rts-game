package ai

import (
	"slices"

	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// Long-term intelligence about each opponent, built only from the bot's own
// observations. Remembered enemies (state.Knowledge) fade after
// state.MemoryTicks because units move; a base does not, so where each
// opponent has been seen is kept for the whole match. With several
// opponents, every one keeps its own estimate: averaging all sightings
// would point at the empty ground between them.

// enemyIntel is what the bot knows about one opponent.
type enemyIntel struct {
	owner    uint32
	base     state.Pos // smoothed position of their sightings (their territory)
	lastSeen uint64
	lastPos  state.Pos // centroid of their units at the last sighting
	peak     int       // most of their units seen at once
	samples  int
}

// updateIntel records the opponents visible this tick.
func (b *Bot) updateIntel() {
	type agg struct {
		x, y int64
		n    int
	}
	seen := map[uint32]*agg{}
	for _, e := range b.K.Enemies() {
		// Units near our base are raiders or scouts, far from their home:
		// they say who is out there, not where they live.
		if state.Cheb(e.Pos, b.home) <= homeRadius {
			if _, known := b.intel[e.Owner]; !known {
				b.log.Info("enemy found", "tick", b.K.Tick, "player", e.Owner, "at", e.Pos.String(), "near", "home")
				b.intel[e.Owner] = &enemyIntel{owner: e.Owner, base: b.mirror(e.Pos), lastSeen: b.K.Tick, lastPos: e.Pos}
			}
			continue
		}
		a := seen[e.Owner]
		if a == nil {
			a = &agg{}
			seen[e.Owner] = a
		}
		a.x, a.y, a.n = a.x+int64(e.Pos.X), a.y+int64(e.Pos.Y), a.n+1
	}
	for owner, a := range seen {
		c := state.Pos{X: int32(a.x / int64(a.n)), Y: int32(a.y / int64(a.n))}
		in := b.intel[owner]
		if in != nil && in.samples == 0 {
			in.base = c // the first sighting away from our base replaces the guess
		}
		if in == nil {
			in = &enemyIntel{owner: owner, base: c}
			b.intel[owner] = in
			b.log.Info("enemy found", "tick", b.K.Tick, "player", owner, "at", c.String(), "units", a.n)
		}
		// Larger groups say more about where a player lives than a lone
		// scout does: weight each sighting by its size.
		w := int32(min(a.n, 8))
		in.base = state.Pos{X: (in.base.X*8 + c.X*w) / (8 + w), Y: (in.base.Y*8 + c.Y*w) / (8 + w)}
		in.lastSeen, in.lastPos = b.K.Tick, c
		in.peak = max(in.peak, a.n)
		in.samples++
	}
}

// homeRadius: sightings this close to our base do not locate the enemy's.
const homeRadius = 12

// mirror is a first guess at the base of a player only seen near ours:
// further along the line from our base through the sighting.
func (b *Bot) mirror(p state.Pos) state.Pos {
	dx, dy := p.X-b.home.X, p.Y-b.home.Y
	// Scale the direction so the guess lands well away from home.
	for range 4 {
		if q := (state.Pos{X: b.home.X + dx*2, Y: b.home.Y + dy*2}); b.K.InBounds(q) && state.Cheb(q, b.home) <= b.K.Width*3/4 {
			dx, dy = dx*2, dy*2
		}
	}
	return state.Pos{X: clamp(b.home.X+dx, 0, b.K.Width-1), Y: clamp(b.home.Y+dy, 0, b.K.Height-1)}
}

// knownOpponents returns the opponents seen so far, in owner order. An
// opponent whose territory an army found empty is forgotten (see
// clearOpponentAt) until it is seen again.
func (b *Bot) knownOpponents() []*enemyIntel {
	out := make([]*enemyIntel, 0, len(b.intel))
	for _, in := range b.intel {
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, c *enemyIntel) int { return int(a.owner) - int(c.owner) })
	return out
}

// clearOpponentAt forgets opponents believed to live at p once an army
// standing there sees none of their units: they moved, or are gone.
func (b *Bot) clearOpponentAt(p state.Pos) {
	visible := map[uint32]bool{}
	for _, e := range b.K.Enemies() {
		visible[e.Owner] = true
	}
	for owner, in := range b.intel {
		if !visible[owner] && state.Cheb(in.base, p) <= 4 {
			b.log.Info("enemy territory empty", "tick", b.K.Tick, "player", owner, "at", in.base.String())
			delete(b.intel, owner)
		}
	}
}

// targetOpponent chooses whom to fight: the opponent whose territory is
// closest to home, discounted when they have shown a big army (go for the
// weaker neighbor when there is a choice). Stable while nothing changes.
func (b *Bot) targetOpponent() (*enemyIntel, bool) {
	var best *enemyIntel
	var bestScore int64
	for _, in := range b.knownOpponents() {
		score := int64(state.Cheb(in.base, b.home))*10 + int64(in.peak)*15
		if best == nil || score < bestScore {
			best, bestScore = in, score
		}
	}
	return best, best != nil
}

// probeTarget picks where to look for opponents nobody has seen yet: the
// sector with the most unexplored ground, preferring distance from home
// (other players start away from us). It is recomputed at most every
// probeInterval ticks so the bot does not dither between two targets.
func (b *Bot) probeTarget() (state.Pos, bool) {
	if b.probe.valid && b.K.Tick < b.probe.at+probeInterval && b.unexploredIn(sectorOf(b.probe.pos)) > 0 {
		return b.probe.pos, true
	}
	sw, sh := (b.K.Width+sectorSize-1)/sectorSize, (b.K.Height+sectorSize-1)/sectorSize
	best, bestScore, found := state.Pos{}, int64(0), false
	for sy := range sh {
		for sx := range sw {
			s := state.Pos{X: sx, Y: sy}
			un := int64(b.unexploredIn(s))
			if un == 0 {
				continue
			}
			center := state.Pos{X: min(sx*sectorSize+sectorSize/2, b.K.Width-1), Y: min(sy*sectorSize+sectorSize/2, b.K.Height-1)}
			// Unexplored ground near other unexplored ground (a region, not
			// a stray corner) at a distance an army can still march to.
			score := un*4 + int64(state.Cheb(center, b.home))*3
			for _, d := range [][2]int32{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				if n := (state.Pos{X: sx + d[0], Y: sy + d[1]}); n.X >= 0 && n.Y >= 0 && n.X < sw && n.Y < sh {
					score += int64(b.unexploredIn(n))
				}
			}
			if !found || score > bestScore {
				best, bestScore, found = center, score, true
			}
		}
	}
	b.probe = probeState{pos: best, at: b.K.Tick, valid: found}
	return best, found
}

// probeInterval is how long a chosen search area is kept.
const probeInterval = 600

type probeState struct {
	pos   state.Pos
	at    uint64
	valid bool
}
