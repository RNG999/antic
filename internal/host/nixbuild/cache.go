package nixbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/RNG999/antic"
)

// CacheEntry describes one cached guest system build under ~/.local/state/antic/cache/<key>.
type CacheEntry struct {
	Key      string    `json:"key"`
	OS       string    `json:"os"`
	Packages []string  `json:"packages,omitempty"`
	Flake    string    `json:"flake,omitempty"`
	System   string    `json:"system"`
	Created  time.Time `json:"created"`
}

var cacheKeyRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidCacheKey reports whether k is a valid 12-hex-digit cache key.
func ValidCacheKey(k string) bool { return cacheKeyRE.MatchString(k) }

// BuiltinKey returns a deterministic 12-hex-character cache key for the embedded
// guest flake, target architecture, user, and normalized package list.
func BuiltinKey(user string, packages []string) (string, []string, error) {
	sys, err := nixSystem()
	if err != nil {
		return "", nil, err
	}
	norm := slices.Clone(packages)
	slices.Sort(norm)
	norm = slices.Compact(norm)

	h := sha256.New()
	fmt.Fprintf(h, "builtin\x00%s\x00%s\x00%s\x00", sys, user, strings.Join(norm, "\n"))
	err = fs.WalkDir(antic.GuestFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." || d.IsDir() {
			return err
		}
		b, err := antic.GuestFS.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:6]), norm, nil
}

// FlakeKey returns a deterministic 12-hex-character cache key for a custom flake#attr.
func FlakeKey(flakePath, flakeAttr string) string {
	sum := sha256.Sum256([]byte("flake\x00" + flakePath + "#" + flakeAttr))
	return hex.EncodeToString(sum[:6])
}

func builtinOSDesc(pkgs []string) string {
	if len(pkgs) == 0 {
		return "builtin"
	}
	return "builtin [" + strings.Join(pkgs, " ") + "]"
}

func readResultLink(link string) (string, error) {
	p := link
	for i := 0; i < 8; i++ {
		dst, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(dst, "/nix/store/") {
			return dst, nil
		}
		if !filepath.IsAbs(dst) {
			dst = filepath.Join(filepath.Dir(p), dst)
		}
		p = dst
	}
	return "", fmt.Errorf("%s: too many links", link)
}

// BuildBuiltinCached returns a cached NixOS toplevel store path if already
// built in cacheDir (and present in the host Nix store), or builds and caches
// it in cacheDir/<key>/.
func (n *Nix) BuildBuiltinCached(ctx context.Context, cacheDir, user string, packages []string, onBuild func()) (string, bool, error) {
	key, normPkgs, err := BuiltinKey(user, packages)
	if err != nil {
		return "", false, err
	}
	entryDir := filepath.Join(cacheDir, key)
	resultLink := filepath.Join(entryDir, "result")
	if sys, err := readResultLink(resultLink); err == nil {
		if _, serr := os.Stat(n.Real(sys)); serr == nil {
			return sys, true, nil
		}
	}

	if onBuild != nil {
		onBuild()
	}
	flakeDir := filepath.Join(entryDir, "flake")
	if err := WriteBuiltinFlakeTo(flakeDir, user, normPkgs); err != nil {
		return "", false, err
	}
	system, err := n.BuildLink(ctx, flakeDir, "nixosConfigurations.antic.config.system.build.toplevel", resultLink)
	if err != nil {
		return "", false, err
	}
	meta := CacheEntry{
		Key:      key,
		OS:       builtinOSDesc(normPkgs),
		Packages: normPkgs,
		System:   system,
		Created:  time.Now().UTC(),
	}
	_ = writeCacheMeta(entryDir, meta)
	return system, false, nil
}

// BuildFlakeCached builds flakePath#nixosConfigurations.flakeAttr and records a
// GC root and metadata in cacheDir/<key>/.
func (n *Nix) BuildFlakeCached(ctx context.Context, cacheDir, flakePath, flakeAttr string) (string, error) {
	key := FlakeKey(flakePath, flakeAttr)
	entryDir := filepath.Join(cacheDir, key)
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		return "", err
	}
	resultLink := filepath.Join(entryDir, "result")
	system, err := n.BuildLink(ctx, flakePath, "nixosConfigurations."+flakeAttr+".config.system.build.toplevel", resultLink)
	if err != nil {
		return "", err
	}
	spec := flakePath + "#" + flakeAttr
	meta := CacheEntry{
		Key:     key,
		OS:      spec,
		Flake:   spec,
		System:  system,
		Created: time.Now().UTC(),
	}
	_ = writeCacheMeta(entryDir, meta)
	return system, nil
}

func writeCacheMeta(entryDir string, meta CacheEntry) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(entryDir, ".meta.json.new")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(entryDir, "meta.json"))
}

// ListCache returns all valid cache entries in cacheDir, sorted newest first.
func ListCache(cacheDir string) ([]CacheEntry, error) {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []CacheEntry
	for _, e := range entries {
		if !e.IsDir() || !ValidCacheKey(e.Name()) {
			continue
		}
		dir := filepath.Join(cacheDir, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var m CacheEntry
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		if sys, err := readResultLink(filepath.Join(dir, "result")); err == nil {
			m.System = sys
		}
		if m.System == "" {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b CacheEntry) int {
		if !a.Created.Equal(b.Created) {
			return b.Created.Compare(a.Created)
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

// RemoveCache deletes one cache entry by key.
func RemoveCache(cacheDir, key string) error {
	if !ValidCacheKey(key) {
		return fmt.Errorf("invalid cache key %q", key)
	}
	dir := filepath.Join(cacheDir, key)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no cache entry %q", key)
	}
	return removeCacheDir(dir)
}

// RemoveAllCache deletes all cache entries in cacheDir and returns the removed keys.
func RemoveAllCache(cacheDir string) ([]string, error) {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || !ValidCacheKey(e.Name()) {
			continue
		}
		if err := removeCacheDir(filepath.Join(cacheDir, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

func removeCacheDir(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// GC runs "nix store gc" on the host Nix store to reclaim unreferenced paths.
func (n *Nix) GC(ctx context.Context) error {
	if _, err := n.output(ctx, "store", "gc"); err != nil {
		return fmt.Errorf("nix store gc: %w", err)
	}
	return nil
}
