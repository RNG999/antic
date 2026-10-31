//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EProjectGuest: a guest defined by a project's antic.toml (outside
// the model, which does not say where a config lives) works from inside
// the project: its relative share resolves against the file, commands
// edit that file in place (and it still loads), and "guest rm" deletes the
// guest's state but leaves the user's project file, so the guest is
// uncreated again.
func TestE2EProjectGuest(t *testing.T) {
	h := newHarness(t)
	const name = prefix + "p"
	proj := h.path("P")
	if err := os.MkdirAll(filepath.Join(proj, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "more"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, "antic.toml")
	const content = `# the project's guest
name = "` + name + `"
packages = ["git"]

[spec]
memory = "1G"

[[shares]]
source = "./data"
mode = "rw"
`
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int, string) { return h.anticIn(proj, nil, args...) }
	defer func() {
		run("guest", "rm", name, "-y")
		os.RemoveAll(proj)
	}()
	type info struct {
		State  string `json:"state"`
		Shares []struct {
			Path    string `json:"path"`
			Mounted bool   `json:"mounted"`
		} `json:"shares"`
	}
	show := func() info {
		out, code, stderr := run("guest", "show", name, "--json")
		if code != 0 {
			t.Fatalf("guest show: exit %d: %s", code, stderr)
		}
		var i info
		if err := json.Unmarshal([]byte(out), &i); err != nil {
			t.Fatal(err)
		}
		return i
	}
	mounted := func(i info, p string) bool {
		for _, s := range i.Shares {
			if s.Path == p && s.Mounted {
				return true
			}
		}
		return false
	}

	if _, code, stderr := run("guest", "create", name); code != 0 {
		t.Fatalf("create: exit %d: %s", code, stderr)
	}
	if i := show(); i.State != "running" || !mounted(i, filepath.Join(proj, "data")) {
		t.Fatalf("after create: %+v (the relative share must resolve against antic.toml)", i)
	}
	if _, code, stderr := run("guest", "mount", name, filepath.Join(proj, "more")); code != 0 {
		t.Fatalf("mount: exit %d: %s", code, stderr)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# the project's guest") || !strings.Contains(string(b), "more") {
		t.Fatalf("mount did not edit antic.toml in place:\n%s", b)
	}
	if i := show(); !mounted(i, filepath.Join(proj, "more")) {
		t.Fatalf("after mount: %+v", i)
	}
	for _, c := range [][]string{{"guest", "stop", name}, {"guest", "start", name}} {
		if _, code, stderr := run(c...); code != 0 {
			t.Fatalf("%v: exit %d: %s", c, code, stderr)
		}
	}
	if i := show(); i.State != "running" || !mounted(i, filepath.Join(proj, "more")) {
		t.Fatalf("after a restart the edited antic.toml must still load and mount both: %+v", i)
	}
	if _, code, stderr := run("guest", "rm", name, "-y"); code != 0 {
		t.Fatalf("rm: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("rm removed the user's project file: %v", err)
	}
	if i := show(); i.State != "-" {
		t.Fatalf("after rm the guest must be uncreated, got %+v", i)
	}
}
