package metadata_test

import (
	"github.com/dakotaknutson/unit-strategy-game/internal/game"
	pt "github.com/dakotaknutson/unit-strategy-game/internal/plugintest"
	"github.com/dakotaknutson/unit-strategy-game/internal/sim"
)

func pluginsOf(h *pt.Harness) []sim.Plugin { return game.PluginsOf(h.Game) }
