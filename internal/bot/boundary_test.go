package bot

import (
	"go/build"
	"strings"
	"testing"
)

// TestImportBoundary enforces that the computer player is a client: its
// packages (and everything they import from this module) must not reach the
// simulation, the plugins, the game assembly or the server. Only generated
// API types (gen/...) and the bot's own packages are allowed. The arena
// (an orchestrator hosting a server for evaluation) and tests are exempt.
func TestImportBoundary(t *testing.T) {
	const mod = "github.com/dakotaknutson/unit-strategy-game/"
	roots := []string{"internal/bot", "internal/bot/ai", "internal/bot/state", "internal/bot/client", "internal/bot/difficulty", "internal/bot/host", "cmd/mono-bot", "cmd/mono-bothost"}
	seen := map[string]bool{}
	var visit func(path, from string)
	visit = func(path, from string) {
		if seen[path] || !strings.HasPrefix(path, mod) {
			return
		}
		seen[path] = true
		rel := strings.TrimPrefix(path, mod)
		if !strings.HasPrefix(rel, "gen/") && !strings.HasPrefix(rel, "internal/bot") && !strings.HasPrefix(rel, "cmd/mono-bot") {
			t.Errorf("%s imports %s: the computer player may only use the public API", from, rel)
			return
		}
		if strings.HasPrefix(rel, "internal/bot/arena") {
			t.Errorf("%s imports the arena (which hosts a server)", from)
			return
		}
		pkg, err := build.Import(path, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pkg.Imports { // non-test imports only
			visit(imp, rel)
		}
	}
	for _, r := range roots {
		visit(mod+r, "(root)")
	}
	if len(seen) < len(roots) {
		t.Fatal("nothing checked")
	}
}
