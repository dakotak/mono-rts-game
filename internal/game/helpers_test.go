package game_test

import (
	"github.com/dakotaknutson/unit-strategy-game/internal/plugins/energy"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func energySet(w *sim.World, u sim.EntityID, v int64) { energy.SetStored(w, u, v) }

func energyOf(w *sim.World, u sim.EntityID) int64 {
	v, _ := energy.Stored(w, u)
	return v
}
