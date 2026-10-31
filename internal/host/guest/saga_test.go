package guest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/tea/operation"
)

// TestJournalRoundTrip: a journal written by one process gives the next one
// the same command and artifacts to undo.
func TestJournalRoundTrip(t *testing.T) {
	added, _ := pathSet([]string{"/home/u/A"})
	runtime, _ := pathSet([]string{"/home/u/B", "/home/u/C"})
	tx := &txn{
		op: operation.Op{Kind: operation.OpChangeConfig, Phase: operation.GuestRunning, Runtime: runtime,
			Change: operation.Change{Config: operation.Changed, Added: added}},
		draft: config.Draft{Path: "/home/u/.config/antic/guests/e2e-a.toml", Name: "e2e-a", Old: []byte("old\n"), New: []byte("new\n")},
		modes: map[string]bool{"/home/u/A": true},
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
	got, err := back.txnOf("e2e-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.op.Kind != tx.op.Kind || got.op.Phase != tx.op.Phase || got.op.Change.Config != operation.Changed ||
		!slices.Equal(got.op.Change.Added.All(), added.All()) || !slices.Equal(got.op.Runtime.All(), runtime.All()) ||
		got.op.Change.Removed.Len() != 0 || string(got.draft.Old) != "old\n" || string(got.draft.New) != "new\n" ||
		got.draft.Path != tx.draft.Path || !got.modes["/home/u/A"] || !slices.Equal(back.Intents, []int{2}) {
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
		added, removed []string
		live           bool
	}{
		{"unchanged but one dropped", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}), nil, []string{"/home/u/old"}, true},
		{"new share", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}, config.Share{Path: "/home/u/old"}, config.Share{Path: "/home/u/N"}), []string{"/home/u/N"}, nil, true},
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
