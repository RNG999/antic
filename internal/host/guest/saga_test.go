package guest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/tea/operation"
)

func mustTestAbsPath(t *testing.T, raw string) domain.AbsPath {
	t.Helper()
	abs, err := domain.ParseAbsPath(raw)
	if err != nil {
		t.Fatalf("ParseAbsPath(%q): %v", raw, err)
	}
	return abs
}

// TestJournalRoundTrip: a journal written by one process gives the next one
// the same command and artifacts to undo.
func TestJournalRoundTrip(t *testing.T) {
	pathA := mustTestAbsPath(t, "/home/u/A")
	pathB := mustTestAbsPath(t, "/home/u/B")
	pathC := mustTestAbsPath(t, "/home/u/C")
	added := buildPathSet(pathA)
	runtime := buildPathSet(pathB, pathC)
	tx := &txn{
		op: operation.Op{Kind: operation.OpChangeConfig, Phase: operation.GuestRunning, Runtime: runtime,
			Change: operation.Change{Config: operation.Changed, Added: added}},
		draft: config.Draft{Path: "/home/u/.config/antic/guests/e2e-a.toml", Name: "e2e-a", Old: []byte("old\n"), New: []byte("new\n")},
		modes: map[domain.AbsPath]bool{pathA: true},
	}
	r := newJournalRecord(tx)
	r.Intents = []int{2}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back journalRecord
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	guestName, err := domain.ParseGuestName("e2e-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.txnOf(guestName)
	if err != nil {
		t.Fatal(err)
	}
	if got.op.Kind != tx.op.Kind || got.op.Phase != tx.op.Phase || got.op.Change.Config != operation.Changed ||
		!slices.Equal(got.op.Change.Added.All(), added.All()) || !slices.Equal(got.op.Runtime.All(), runtime.All()) ||
		got.op.Change.Removed.Len() != 0 || string(got.draft.Old) != "old\n" || string(got.draft.New) != "new\n" ||
		got.draft.Path != tx.draft.Path || !got.modes[pathA] || !slices.Equal(back.Intents, []int{2}) {
		t.Fatalf("journal round trip changed the command: %+v", got)
	}
}

// TestLiveShares: an edit of a running guest's shares is made live unless a
// mounted share changes mode or a new share overlaps a mount.
func TestLiveShares(t *testing.T) {
	bt := &boot{Shares: []bootShare{
		{Share: config.Share{Path: "/home/u/A"}, Tag: "a"},                  // mounted at boot
		{Share: config.Share{Path: "/home/u/R"}, Tag: "r", Runtime: true},   // hot-plugged
		{Share: config.Share{Path: "/home/u/old"}, Tag: "o", Runtime: true}, // hot-plugged, dropped below
	}}
	cfg := func(ss ...config.Share) *config.Guest { return &config.Guest{Shares: ss} }
	for _, c := range []struct {
		name           string
		c              *config.Guest
		added, removed []domain.AbsPath
		live           bool
	}{
		{"unchanged but one dropped", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}), nil, []domain.AbsPath{mustTestAbsPath(t, "/home/u/old")}, true},
		{"new share", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}, config.Share{Path: "/home/u/old"}, config.Share{Path: "/home/u/N"}), []domain.AbsPath{mustTestAbsPath(t, "/home/u/N")}, nil, true},
		{"boot share removed stays mounted", cfg(config.Share{Path: "/home/u/R"}, config.Share{Path: "/home/u/old"}), nil, nil, true},
		{"mode change", cfg(config.Share{Path: "/home/u/A", ReadOnly: true}), nil, nil, false},
		{"overlap with a mount", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/A/sub"}), nil, nil, false},
	} {
		added, removed, live := liveShares(bt, c.c)
		if live != c.live || (live && (!slices.Equal(added, c.added) || !slices.Equal(removed, c.removed))) {
			t.Errorf("%s: added %v, removed %v, live %v", c.name, added, removed, live)
		}
	}
}

// TestBootSignature: systems differ in what they boot exactly when their
// kernel, initrd or kernel parameters differ (a switch cannot change them,
// so such a change restarts a running guest).
func TestBootSignature(t *testing.T) {
	root := t.TempDir()
	store := nixbuild.GuestStore(root)
	system := func(name, kernel, initrd, params string) domain.StorePath {
		dir, err := domain.ParseStorePath("/nix/store/00000000000000000000000000000000-" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(store.Real(dir.Get()), 0o755); err != nil {
			t.Fatal(err)
		}
		for link, target := range map[string]string{"kernel": kernel, "initrd": initrd} {
			if err := os.Symlink(target, filepath.Join(store.Real(dir.Get()), link)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(store.Real(dir.Get()), "kernel-params"), []byte(params), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	base := system("a-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=ttyS0")
	same := system("b-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=ttyS0")
	if bootSignature(store, base) != bootSignature(store, same) {
		t.Fatal("systems with the same kernel, initrd and parameters boot differently")
	}
	for _, other := range []domain.StorePath{
		system("c-system", "/nix/store/k2/bzImage", "/nix/store/i1/initrd", "console=ttyS0"),
		system("d-system", "/nix/store/k1/bzImage", "/nix/store/i2/initrd", "console=ttyS0"),
		system("e-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=hvc0"),
	} {
		if bootSignature(store, base) == bootSignature(store, other) {
			t.Fatalf("%s boots like %s", other, base)
		}
	}
}
