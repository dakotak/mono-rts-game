package ai

import (
	"cmp"
	"fmt"
	"slices"

	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

// MonitorInterval is the minimum number of ticks between re-scouting trips
// to the enemy base once the map is explored.
const MonitorInterval = 1200

// sectorSize is the edge of an exploration sector, in tiles.
const sectorSize = 8

// planScouts sends scouts to valuable unexplored sectors and back home to
// refuel. Skilled bots prefer sectors toward the inferred enemy base and
// far, unseen regions (likely resources), and re-scout the enemy base to
// monitor it; unskilled bots take the nearest unexplored sector.
func (b *Bot) planScouts() {
	taken := map[state.Pos]bool{}
	for _, t := range b.scouting {
		taken[sectorOf(t)] = true
	}
	for _, u := range b.unitsByRole(RoleScout) {
		e, capacity := energy(u)
		// Head home while the energy still covers the trip (terrain can cost
		// several steps per tile).
		if step := b.K.Rules.EnergyPerStep; step > 0 && e < int64(state.Cheb(u.Pos, b.home))*step*3+step*10 && capacity > 0 {
			delete(b.scouting, u.ID)
			if state.Cheb(u.Pos, b.home) > 2 {
				b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioExplore, Essential: true, Desc: "scout refuel",
					Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(b.home)}})
			}
			continue
		}
		if t, ok := b.scouting[u.ID]; ok {
			if state.Cheb(u.Pos, t) > 2 && b.unexploredIn(sectorOf(t)) > 0 {
				continue
			}
			delete(b.scouting, u.ID)
		}
		reach := int32(1 << 20)
		if step := b.K.Rules.EnergyPerStep; step > 0 {
			reach = int32((e - step*10) / (step * 2)) // there and back with some terrain
		}
		t, ok := b.chooseSector(u.Pos, taken, reach)
		if !ok {
			continue
		}
		taken[sectorOf(t)] = true
		b.scouting[u.ID] = t
		b.issue(Order{Key: fmt.Sprintf("move:%d", u.ID), Units: 1, Priority: PrioExplore, Desc: "scout " + fmt.Sprint(t),
			Payload: &movementv1.GroupMoveCommand{UnitIds: []uint64{u.ID}, Destination: pos(t)}})
	}
}

func sectorOf(p state.Pos) state.Pos { return state.Pos{X: p.X / sectorSize, Y: p.Y / sectorSize} }

// unexploredIn counts unexplored tiles in a sector (sampled every 2 tiles).
func (b *Bot) unexploredIn(s state.Pos) int {
	n := 0
	for y := s.Y * sectorSize; y < (s.Y+1)*sectorSize; y += 2 {
		for x := s.X * sectorSize; x < (s.X+1)*sectorSize; x += 2 {
			if p := (state.Pos{X: x, Y: y}); b.K.InBounds(p) && !b.K.IsExplored(p) {
				n++
			}
		}
	}
	return n
}

// chooseSector picks the next scouting destination.
func (b *Bot) chooseSector(from state.Pos, taken map[state.Pos]bool, reach int32) (state.Pos, bool) {
	sw, sh := (b.K.Width+sectorSize-1)/sectorSize, (b.K.Height+sectorSize-1)/sectorSize
	guess := b.enemyBaseGuess()
	type cand struct {
		center state.Pos
		score  int64
	}
	var cands []cand
	for sy := range sh {
		for sx := range sw {
			s := state.Pos{X: sx, Y: sy}
			if taken[s] || !b.spend(1) {
				continue
			}
			un := b.unexploredIn(s)
			if un == 0 {
				continue
			}
			center := state.Pos{X: min(sx*sectorSize+sectorSize/2, b.K.Width-1), Y: min(sy*sectorSize+sectorSize/2, b.K.Height-1)}
			d := int64(state.Cheb(center, from))
			if state.Cheb(center, from)+state.Cheb(center, b.home) > reach {
				continue
			}
			score := int64(un)*10 - d*int64(1100-b.P.ExplorationAggression)/100
			if b.P.ExplorationAggression >= 450 {
				// Toward the inferred enemy and away from home: where the
				// unknown (enemies, contested wells) is.
				score += int64(state.Cheb(center, b.home))*3 - int64(state.Cheb(center, guess))*2
			}
			cands = append(cands, cand{center, score})
		}
	}
	if len(cands) == 0 {
		// Re-scout the enemy base now and then (adaptive bots); walking
		// back and forth constantly would burn more energy than the
		// information is worth.
		if b.P.Adaptation >= 600 && b.K.Tick >= b.monitorAt && state.Cheb(guess, from)+state.Cheb(guess, b.home) <= reach {
			b.monitorAt = b.K.Tick + MonitorInterval
			return guess, true
		}
		return state.Pos{}, false
	}
	slices.SortStableFunc(cands, func(a, c cand) int { return cmp.Compare(c.score, a.score) })
	scores := make([]int64, len(cands))
	for i, c := range cands {
		scores[i] = c.score
	}
	return cands[b.pick(scores)].center, true
}
