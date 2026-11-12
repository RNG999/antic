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

	"github.com/RNG999/antic/internal/domain"
)

// Tag derives a share's virtiofs tag from its host path.
func Tag(path string) domain.ShareTag {
	sum := sha256.Sum256([]byte(path))
	tag, err := domain.ParseShareTag("s-" + hex.EncodeToString(sum[:6]))
	if err != nil {
		panic("share.Tag: invalid generated tag: " + err.Error())
	}
	return tag
}

// Policy decides which directories may be shared.
type Policy struct {
	Home  domain.AbsPath
	Deny  []domain.AbsPath // never shared, nor any directory containing them
	Roots []domain.AbsPath // runtime shares outside these need confirmation
}

// NewPolicy builds the policy. Denied: every hidden entry at the top of
// $HOME and whatever symlinks there (one level into hidden directories as
// well) point to, $PATH directories in $HOME, and extra (antic's own
// directories, guest flakes, policy.deny). A guest that could write any of
// these would get code run on the host.
func NewPolicy(home string, extra, roots []string) Policy {
	var p Policy
	if h, err := domain.ParseAbsPath(filepath.Clean(home)); err == nil {
		p.Home = h
	}
	for _, r := range roots {
		if abs, err := domain.ParseAbsPath(filepath.Clean(r)); err == nil {
			p.Roots = append(p.Roots, abs)
		}
	}
	add := func(path string) {
		clean := filepath.Clean(path)
		if abs, err := domain.ParseAbsPath(clean); err == nil {
			p.Deny = append(p.Deny, abs)
		}
		if real, err := filepath.EvalSymlinks(clean); err == nil {
			if abs, err := domain.ParseAbsPath(real); err == nil {
				p.Deny = append(p.Deny, abs)
			}
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
		if cleanDir, err := domain.ParseAbsPath(filepath.Clean(dir)); err == nil && p.Home.Valid() {
			if cleanDir == p.Home || domain.WithinSubtree(p.Home, cleanDir) {
				add(dir)
			}
		}
	}
	for _, d := range extra {
		add(d)
	}
	slices.SortFunc(p.Deny, func(a, b domain.AbsPath) int {
		return strings.Compare(a.Get(), b.Get())
	})
	p.Deny = slices.Compact(p.Deny)
	return p
}

// Resolve validates a share path and returns its real absolute path.
func (p Policy) Resolve(path string) (domain.AbsPath, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return domain.AbsPath{}, err
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return domain.AbsPath{}, fmt.Errorf("%s is not a directory", real)
	}
	abs, err := domain.ParseAbsPath(real)
	if err != nil {
		return domain.AbsPath{}, fmt.Errorf("%s: %w", real, err)
	}
	if !p.Home.Valid() || !domain.WithinSubtree(p.Home, abs) {
		return domain.AbsPath{}, fmt.Errorf("%s: only directories below %s can be shared", real, p.Home.Get())
	}
	if strings.ContainsFunc(real, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return domain.AbsPath{}, fmt.Errorf("%q: control characters in share paths are not supported", real)
	}
	for _, d := range p.Deny {
		if domain.PathsOverlap(abs, d) {
			return domain.AbsPath{}, fmt.Errorf("%s: refusing to share (overlaps %s)", real, d.Get())
		}
	}
	return abs, nil
}

// InRoots reports whether path lies in one of the policy's share roots (or
// there are none).
func (p Policy) InRoots(path domain.AbsPath) bool {
	if len(p.Roots) == 0 {
		return true
	}
	for _, r := range p.Roots {
		if path == r || domain.WithinSubtree(r, path) {
			return true
		}
	}
	return false
}

// InRootsString is a convenience helper for string paths.
func (p Policy) InRootsString(path string) bool {
	abs, err := domain.ParseAbsPath(filepath.Clean(path))
	if err != nil {
		return false
	}
	return p.InRoots(abs)
}

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
