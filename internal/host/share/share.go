// Package share decides what may be shared with a guest and what inside a
// share the host exports read-only.
package share

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Tag derives a share's virtiofs tag from its host path.
func Tag(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "s-" + hex.EncodeToString(sum[:6])
}

// Policy decides which directories may be shared.
type Policy struct {
	Home  string
	Deny  []string // never shared, nor any directory containing them
	Roots []string // runtime shares outside these need confirmation
}

// NewPolicy builds the policy. Denied: every hidden entry at the top of
// $HOME and whatever symlinks there (one level into hidden directories as
// well) point to, $PATH directories in $HOME, and extra (antic's own
// directories, guest flakes, policy.deny). A guest that could write any of
// these would get code run on the host.
func NewPolicy(home string, extra, roots []string) Policy {
	p := Policy{Home: home, Roots: roots}
	add := func(path string) {
		p.Deny = append(p.Deny, filepath.Clean(path))
		if real, err := filepath.EvalSymlinks(path); err == nil {
			p.Deny = append(p.Deny, real)
		}
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		top := filepath.Join(home, e.Name())
		add(top)
		if fi, err := os.Lstat(top); err == nil && fi.IsDir() {
			inner, _ := os.ReadDir(top)
			for _, f := range inner {
				if f.Type()&fs.ModeSymlink != 0 {
					add(filepath.Join(top, f.Name()))
				}
			}
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if within(dir, home) {
			add(dir)
		}
	}
	for _, d := range extra {
		add(d)
	}
	slices.Sort(p.Deny)
	p.Deny = slices.Compact(p.Deny)
	return p
}

// Resolve validates a share path and returns its real absolute path.
func (p Policy) Resolve(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", real)
	}
	if !within(real, p.Home) || real == p.Home {
		return "", fmt.Errorf("%s: only directories below %s can be shared", real, p.Home)
	}
	if strings.ContainsFunc(real, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%q: control characters in share paths are not supported", real)
	}
	for _, d := range p.Deny {
		if within(real, d) || within(d, real) {
			return "", fmt.Errorf("%s: refusing to share (overlaps %s)", real, d)
		}
	}
	return real, nil
}

// InRoots reports whether path lies in one of the policy's share roots (or
// there are none).
func (p Policy) InRoots(path string) bool {
	if len(p.Roots) == 0 {
		return true
	}
	for _, r := range p.Roots {
		if within(path, r) {
			return true
		}
	}
	return false
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// maxDepth limits how deep Protected looks for protected names.
const maxDepth = 8

// Protected lists the directories and files under root that host-side tools
// execute or trust (names), plus bare-repository layouts, so the host can
// export them read-only. Symlinks are never followed or returned; node_modules
// is not searched.
func Protected(root string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if path == root {
			return nil
		}
		depth := strings.Count(strings.TrimPrefix(path, root), "/")
		if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return nil
		}
		if want[d.Name()] {
			out = append(out, path)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || depth >= maxDepth {
				return fs.SkipDir
			}
			if bareRepo(path) {
				out = append(out, path)
				return fs.SkipDir
			}
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, err
}

// bareRepo reports whether dir looks like a git repository git would pick
// up without a .git (HEAD file, objects/ and refs/ directories).
func bareRepo(dir string) bool {
	for name, isDir := range map[string]bool{"HEAD": false, "objects": true, "refs": true} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || fi.IsDir() != isDir {
			return false
		}
	}
	return true
}
