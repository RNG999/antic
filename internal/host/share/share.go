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
	"github.com/RNG999/antic/internal/proto"
)

// Tag derives a share's virtiofs tag from its host path.
func Tag(path domain.AbsPath) domain.ShareTag {
	sum := sha256.Sum256([]byte(path.Get()))
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
func NewPolicy(home domain.AbsPath, extra, roots []domain.AbsPath) Policy {
	p := Policy{Home: home, Roots: slices.Clone(roots)}
	var rawCandidates []string
	entries, _ := os.ReadDir(home.Get())
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		top := filepath.Join(home.Get(), e.Name())
		rawCandidates = append(rawCandidates, top)
		if fi, err := os.Lstat(top); err == nil && fi.IsDir() {
			inner, _ := os.ReadDir(top)
			for _, f := range inner {
				if f.Type()&fs.ModeSymlink != 0 {
					rawCandidates = append(rawCandidates, filepath.Join(top, f.Name()))
				}
			}
		}
	}
	for _, rawEnvDir := range filepath.SplitList(os.Getenv("PATH")) {
		if cleanDir, err := domain.ParseAbsPath(filepath.Clean(rawEnvDir)); err == nil && p.Home.Valid() {
			if cleanDir == p.Home || domain.WithinSubtree(p.Home, cleanDir) {
				rawCandidates = append(rawCandidates, rawEnvDir)
			}
		}
	}
	for _, d := range extra {
		rawCandidates = append(rawCandidates, d.Get())
	}
	for _, rawCandidate := range rawCandidates {
		clean := filepath.Clean(rawCandidate)
		if abs, err := domain.ParseAbsPath(clean); err == nil {
			p.Deny = append(p.Deny, abs)
		}
		if real, err := filepath.EvalSymlinks(clean); err == nil {
			if abs, err := domain.ParseAbsPath(real); err == nil {
				p.Deny = append(p.Deny, abs)
			}
		}
	}
	slices.SortFunc(p.Deny, func(a, b domain.AbsPath) int {
		return strings.Compare(a.Get(), b.Get())
	})
	p.Deny = slices.Compact(p.Deny)
	return p
}

// ValidateShareDirectory validates a candidate share directory against the policy
// and returns its symlink-evaluated canonical absolute path.
func (p Policy) ValidateShareDirectory(candidate domain.AbsPath) (domain.AbsPath, error) {
	real, err := filepath.EvalSymlinks(candidate.Get())
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

// maxDepth limits how deep Protected looks for protected names.
const maxDepth = 8

// Protected lists the directories and files under root that host-side tools
// execute or trust (proto.ProtectedNames), plus bare-repository layouts, so the
// host can export them read-only. Symlinks are never followed or returned;
// node_modules is not searched.
func Protected(root domain.AbsPath) ([]domain.AbsPath, error) {
	want := map[string]bool{}
	for _, n := range proto.ProtectedNames {
		want[n] = true
	}
	var out []domain.AbsPath
	err := filepath.WalkDir(root.Get(), func(walkEntryPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			if walkEntryPath == root.Get() {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if walkEntryPath == root.Get() {
			return nil
		}
		depth := strings.Count(strings.TrimPrefix(walkEntryPath, root.Get()), "/")
		if entry.Type()&fs.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return nil
		}
		absEntry, parseErr := domain.ParseAbsPath(walkEntryPath)
		if parseErr != nil {
			return nil
		}
		if want[entry.Name()] {
			out = append(out, absEntry)
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" || depth >= maxDepth {
				return fs.SkipDir
			}
			if bareRepo(absEntry) {
				out = append(out, absEntry)
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
func bareRepo(dir domain.AbsPath) bool {
	for name, isDir := range map[string]bool{"HEAD": false, "objects": true, "refs": true} {
		fi, err := os.Lstat(filepath.Join(dir.Get(), name))
		if err != nil || fi.IsDir() != isDir {
			return false
		}
	}
	return true
}
