package web

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAdminLogic runs the admin workspace's JavaScript unit tests
// (web/admintest) with Node.js when it is installed; the project does not
// otherwise need Node.
func TestAdminLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: run `node --test web/admintest/*.test.js` to test the admin client logic")
	}
	files, _ := filepath.Glob("admintest/*.test.js")
	if len(files) == 0 {
		t.Fatal("no admin client tests")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("admin client tests failed: %v\n%s", err, out)
	}
}

// TestAdminPage checks the admin client's wiring without a browser: every
// script and stylesheet the page loads is embedded, every element id the
// scripts look up exists in the page (so no control silently disappears
// from a panel), and every panel has a title and a place in both default
// layouts.
func TestAdminPage(t *testing.T) {
	static := Static()
	page := read(t, static, "admin/index.html")
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		ref := m[1]
		p := "admin/" + ref
		if strings.HasPrefix(ref, "/") {
			p = strings.TrimPrefix(ref, "/")
		}
		if _, err := fs.Stat(static, p); err != nil {
			t.Errorf("index.html references %s, which is not embedded", ref)
		}
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		ids[m[1]] = true
	}
	scripts, _ := fs.Glob(static, "admin/*.js")
	lookup := regexp.MustCompile(`(?:\$|\$l|Store\.el|getElementById)\("([A-Za-z0-9_-]+)"\)`)
	for _, f := range scripts {
		for _, m := range lookup.FindAllStringSubmatch(read(t, static, f), -1) {
			id := m[1]
			if strings.HasPrefix(id, "cw-") { // created by the inspector's edit forms
				continue
			}
			if !ids[id] {
				t.Errorf("%s looks up #%s, which is not in index.html", f, id)
			}
		}
	}
	logic := read(t, static, "admin/logic.js")
	sections := regexp.MustCompile(`<section data-panel="([a-z]+)" data-title="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(sections) < 10 {
		t.Fatalf("found %d panels", len(sections))
	}
	for _, s := range sections {
		if n := strings.Count(logic, `{ id: "`+s[1]+`"`); n != 2 {
			t.Errorf("panel %s appears %d times in the default layouts, want once in each", s[1], n)
		}
	}
}

func read(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
