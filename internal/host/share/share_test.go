package share

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mk(t *testing.T, paths ...string) {
	for _, p := range paths {
		if p[len(p)-1] == '/' {
			os.MkdirAll(p, 0o755)
		} else {
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, nil, 0o644)
		}
	}
}

func TestProtected(t *testing.T) {
	r := t.TempDir()
	mk(t, r+"/a/.git/hooks/", r+"/a/src/x.go", r+"/b/CLAUDE.md", r+"/b/.claude/settings.json",
		r+"/bare/HEAD", r+"/bare/objects/", r+"/bare/refs/", r+"/node_modules/x/.git/", r+"/deep/1/2/3/4/5/6/7/8/9/.envrc")
	os.Symlink("/", r+"/.envrc")
	got, err := Protected(r, []string{".git", ".claude", "CLAUDE.md", ".envrc"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{r + "/a/.git", r + "/b/.claude", r + "/b/CLAUDE.md", r + "/bare"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("Protected = %v, want %v", got, want)
	}
}

func TestPolicy(t *testing.T) {
	home := t.TempDir()
	mk(t, home+"/.ssh/", home+"/Work/proj/", home+"/dotfiles/bashrc", home+"/bin/")
	os.Symlink(home+"/dotfiles/bashrc", home+"/.bashrc")
	t.Setenv("PATH", home+"/bin:/usr/bin")
	p := NewPolicy(home, []string{home + "/Work/proj/flake"}, []string{home + "/Work"})
	if _, err := p.Resolve(home + "/Work"); err == nil {
		t.Error("shared a directory containing a guest flake")
	}
	if _, err := p.Resolve(home + "/Work/proj/sub"); err == nil {
		t.Error("shared a missing directory")
	}
	for _, bad := range []string{home, home + "/.ssh", home + "/dotfiles", home + "/bin"} {
		if _, err := p.Resolve(bad); err == nil {
			t.Errorf("Resolve(%s) succeeded", bad)
		}
	}
	mk(t, home+"/Projects/")
	if got, err := p.Resolve(home + "/Projects"); err != nil || got != home+"/Projects" {
		t.Errorf("Resolve(Projects) = %q, %v", got, err)
	}
	if p.InRoots(home+"/Projects") || !p.InRoots(home+"/Work/x") {
		t.Error("InRoots wrong")
	}
}
