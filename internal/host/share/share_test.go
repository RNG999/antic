package share

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/domain"
)

func mustAbs(t *testing.T, raw string) domain.AbsPath {
	t.Helper()
	abs, err := domain.ParseAbsPath(raw)
	if err != nil {
		t.Fatalf("mustAbs(%q): %v", raw, err)
	}
	return abs
}

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
	got, err := Protected(mustAbs(t, r))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.AbsPath{
		mustAbs(t, r+"/a/.git"),
		mustAbs(t, r+"/b/.claude"),
		mustAbs(t, r+"/b/CLAUDE.md"),
		mustAbs(t, r+"/bare"),
	}
	slices.SortFunc(got, func(a, b domain.AbsPath) int {
		return strings.Compare(a.Get(), b.Get())
	})
	if !slices.Equal(got, want) {
		t.Fatalf("Protected = %v, want %v", got, want)
	}
}

func TestPolicy(t *testing.T) {
	home := t.TempDir()
	homeAbs := mustAbs(t, home)
	mk(t, home+"/.ssh/", home+"/Work/proj/", home+"/dotfiles/bashrc", home+"/bin/")
	os.Symlink(home+"/dotfiles/bashrc", home+"/.bashrc")
	t.Setenv("PATH", home+"/bin:/usr/bin")
	p := NewPolicy(homeAbs, []domain.AbsPath{mustAbs(t, home+"/Work/proj/flake")}, []domain.AbsPath{mustAbs(t, home+"/Work")})
	if _, err := p.ValidateShareDirectory(mustAbs(t, home+"/Work")); err == nil {
		t.Error("shared a directory containing a guest flake")
	}
	if _, err := p.ValidateShareDirectory(mustAbs(t, home+"/Work/proj/sub")); err == nil {
		t.Error("shared a missing directory")
	}
	for _, bad := range []string{home, home + "/.ssh", home + "/dotfiles", home + "/bin"} {
		if _, err := p.ValidateShareDirectory(mustAbs(t, bad)); err == nil {
			t.Errorf("ValidateShareDirectory(%s) succeeded", bad)
		}
	}
	mk(t, home+"/Projects/")
	if got, err := p.ValidateShareDirectory(mustAbs(t, home+"/Projects")); err != nil || got.Get() != home+"/Projects" {
		t.Errorf("ValidateShareDirectory(Projects) = %q, %v", got, err)
	}
	if p.InRoots(mustAbs(t, home+"/Projects")) || !p.InRoots(mustAbs(t, home+"/Work/x")) {
		t.Error("InRoots wrong")
	}

	tag := Tag(mustAbs(t, home+"/Projects"))
	if !tag.Valid() {
		t.Errorf("Tag(%s) produced invalid ShareTag %q", home+"/Projects", tag.Get())
	}
}
