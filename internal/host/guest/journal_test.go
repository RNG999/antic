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
