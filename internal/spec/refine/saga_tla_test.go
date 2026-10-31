package refine_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTLCSagaModels model-checks formal/saga/Saga.tla with TLC in three
// configurations and requires the expected verdicts: with journaling the
// saga keeps P1 (Consistent) and terminates under crashes; without a
// journal, or when a command reads the state it plans from before taking
// the guest's lock, TLC must find a P1 counterexample.
func TestTLCSagaModels(t *testing.T) {
	tlc, err := exec.LookPath("tlc")
	if err != nil {
		t.Skip("tlc is not installed (it is in the dev shell: nix develop)")
	}
	dir, err := filepath.Abs("../../../formal/saga")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Saga.tla")); err != nil {
		t.Skip("formal/saga is not in this source tree")
	}
	cases := []struct {
		cfg  string
		want string
	}{
		{"SagaJournal.cfg", "Model checking completed. No error has been found."},
		{"SagaTrace.cfg", "Model checking completed. No error has been found."},
		{"SagaNoJournal.cfg", "Invariant Consistent is violated."},
		{"SagaStaleRead.cfg", "Invariant Consistent is violated."},
	}
	for _, c := range cases {
		t.Run(c.cfg, func(t *testing.T) {
			cmd := exec.Command(tlc, "-workers", "auto", "-metadir", t.TempDir(), "-config", c.cfg, "Saga.tla")
			cmd.Dir = dir
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), c.want) {
				t.Fatalf("TLC did not report %q:\n%s", c.want, out)
			}
		})
	}
}
