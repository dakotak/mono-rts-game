package server

import (
	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/match"
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/worldgen"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

// matchStatic is what does not change during a match.
type matchStatic struct {
	id            string
	seed          uint64
	rulesetHash   string
	conditions    []string
	startMode     string
	timeLimit     int64
	scoresPublic  bool
	generator     string
	width, height int32
	names         map[sim.PlayerID]string
}

func newMatchStatic(m *matchRun) *matchStatic {
	st := &matchStatic{
		id: m.id, seed: m.game.Seed, rulesetHash: game.RulesetHash(m.engine.Rules()),
		width: m.cfg.World.Width, height: m.cfg.World.Height, names: map[sim.PlayerID]string{},
		generator: "none", startMode: "immediate",
	}
	if mp, ok := game.Plugin[*match.Plugin](m.game); ok {
		c := mp.Config()
		st.conditions, st.startMode, st.timeLimit, st.scoresPublic = mp.ConditionNames(), c.Start, c.Victory.TimeLimitTicks, c.Scoring.PublicDuringMatch
	}
	if wg, ok := game.Plugin[*worldgen.Plugin](m.game); ok {
		st.generator = wg.Generator()
	}
	for _, p := range m.players {
		st.names[p.ID] = p.Name
	}
	return st
}

func stateToProto(s match.State) rtsv1.MatchState {
	return rtsv1.MatchState(int32(s) + 1)
}

// matchInfo projects the status for a viewer. Players see their own
// readiness, elimination and score; everyone's scores only when public or
// finished; the seed only in the final result.
func matchInfo(static *matchStatic, st match.Status, tick sim.Tick, v viewer) *rtsv1.MatchInfo {
	info := &rtsv1.MatchInfo{
		MatchId: static.id, State: stateToProto(st.State), Tick: uint64(tick), StartTick: uint64(st.StartTick),
		RunTicks: st.RunTicks, TimeLimitTicks: static.timeLimit, VictoryConditions: static.conditions,
		StartMode: static.startMode, ExpectedPlayers: uint32(st.N), ScoresPublic: static.scoresPublic,
		Generator: static.generator, Width: static.width, Height: static.height,
	}
	if !v.admin {
		info.YourPlayerId = uint32(v.player)
	}
	if static.timeLimit > 0 {
		info.TicksRemaining = max(0, static.timeLimit-st.RunTicks)
	}
	finished := st.State == match.Finished
	for i := range int(st.N) {
		if st.Ready[i] {
			info.ReadyPlayers++
		}
		if st.Eliminated[i] {
			info.EliminatedPlayers++
		}
		p := st.Players[i]
		if !v.admin && p == v.player {
			info.YouReady, info.YouEliminated, info.YourScore = st.Ready[i], st.Eliminated[i], st.Scores[i]
		}
		if v.admin || static.scoresPublic || finished {
			info.Scores = append(info.Scores, &rtsv1.PlayerScore{PlayerId: uint32(p), Name: static.names[p],
				Score: st.Scores[i], Eliminated: st.Eliminated[i], Ready: st.Ready[i]})
		}
	}
	if finished {
		info.Result = resultFrom(static, st)
	}
	return info
}

// resultFrom builds the public result of a finished match.
func resultFrom(static *matchStatic, st match.Status) *rtsv1.MatchResult {
	r := &rtsv1.MatchResult{
		MatchId: static.id, Seed: static.seed, RulesetHash: static.rulesetHash, EngineVersion: game.EngineVersion,
		WinnerId: uint32(st.Winner), Draw: st.Draw, Reason: st.Reason, FinalTick: uint64(st.FinishTick),
		RunTicks: st.RunTicks, Generator: static.generator,
	}
	for i := range int(st.N) {
		p := st.Players[i]
		r.Players = append(r.Players, playerResult(static, st, i, p))
	}
	return r
}

func playerResult(static *matchStatic, st match.Status, i int, p sim.PlayerID) *rtsv1.PlayerResult {
	s, f := st.Stats[i], st.Final[i]
	return &rtsv1.PlayerResult{
		PlayerId: uint32(p), Name: static.names[p], Winner: p == st.Winner && !st.Draw, Eliminated: st.Eliminated[i],
		Score: st.Scores[i], Units: f.Units, Embodied: f.Embodied, StoredEnergy: f.Stored,
		EnergyMined: s.EnergyMined, Kills: s.Kills, UnitsLost: s.UnitsLost, InvestmentDestroyed: s.InvestmentDestroyed,
	}
}

// ---- hub integration -------------------------------------------------------------------

func (h *Hub) setMatch(static *matchStatic, st match.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.static, h.status = static, st
}

// MatchInfo returns the current match info for a viewer.
func (h *Hub) MatchInfo(v viewer) *rtsv1.MatchInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.matchInfoLocked(v)
}

func (h *Hub) matchInfoLocked(v viewer) *rtsv1.MatchInfo {
	if h.static == nil {
		return nil
	}
	return matchInfo(h.static, h.status, h.tick, v)
}

// Status returns the current match status (admin use).
func (h *Hub) Status() match.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Close drops every subscriber (they resubscribe and find the new match)
// and ignores later frames. Used when a match is replaced.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		close(s.ch)
		delete(h.subs, s)
	}
}
