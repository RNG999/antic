package nixbuild

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RNG999/antic"
)

func TestBuiltinKeyOrderIndependence(t *testing.T) {
	k1, norm1, err := BuiltinKey("alice", []string{"ripgrep", "git", "git"})
	if err != nil {
		t.Fatal(err)
	}
	k2, norm2, err := BuiltinKey("alice", []string{"git", "ripgrep"})
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("expected identical keys for reordered/duplicate packages, got %q vs %q", k1, k2)
	}
	if len(norm1) != 2 || norm1[0] != "git" || norm1[1] != "ripgrep" {
		t.Fatalf("unexpected normalized packages: %v (norm2=%v)", norm1, norm2)
	}
	if !validCacheKey(k1) {
		t.Fatalf("expected valid 12-hex key, got %q", k1)
	}

	kOtherUser, _, err := BuiltinKey("bob", []string{"git", "ripgrep"})
	if err != nil {
		t.Fatal(err)
	}
	if k1 == kOtherUser {
		t.Fatalf("expected different key for different user, got %q", k1)
	}
}

func TestCacheLifecycle(t *testing.T) {
	cacheDir := t.TempDir()
	key := "0123456789ab"
	entryDir := filepath.Join(cacheDir, key)
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := "/nix/store/00000000000000000000000000000000-nixos-system-antic"
	if err := os.Symlink(storePath, filepath.Join(entryDir, "result")); err != nil {
		t.Fatal(err)
	}
	meta := CacheEntry{
		Key:      key,
		OS:       "builtin [git]",
		Packages: []string{"git"},
		System:   storePath,
		Created:  time.Now().UTC().Truncate(time.Second),
	}
	if err := writeCacheMeta(entryDir, meta); err != nil {
		t.Fatal(err)
	}

	entries, err := ListCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != key || entries[0].System != storePath {
		t.Fatalf("unexpected cache list: %+v", entries)
	}

	if err := RemoveCache(cacheDir, key); err != nil {
		t.Fatal(err)
	}
	entries, err = ListCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty cache after RemoveCache, got %+v", entries)
	}
}

// TestGuestFSHasAgentImports checks that the embedded guest flake source
// contains every repository package antic-agent imports, directly or not;
// otherwise building a built-in guest fails.
func TestGuestFSHasAgentImports(t *testing.T) {
	const module = "github.com/RNG999/antic/"
	queue := []string{"cmd/antic-agent"}
	seen := map[string]bool{}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := fs.ReadDir(antic.GuestFS, dir)
		if err != nil {
			t.Errorf("package %s is imported by antic-agent but not embedded in antic.GuestFS (embed.go)", dir)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := fs.ReadFile(antic.GuestFS, dir+"/"+name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, module) {
					queue = append(queue, strings.TrimPrefix(p, module))
				}
			}
		}
	}
}
