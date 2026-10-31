//go:build e2e

package e2e

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

// Generated command sequences (§6 of the design): a random walk over an
// alphabet of commands, each checked against the specification as the
// scenarios are. A failing sequence is shrunk (greedily dropping commands
// while it still fails) and pinned as a txtar file in testdata/regress,
// which every E2E run replays.
//
//	ANTIC_E2E_RANDOM=N   run N generated sequences (default: none)
//	ANTIC_E2E_SEED=S     the first sequence's seed (default: random)
//	ANTIC_E2E_LENGTH=L   commands per sequence (default 8)

// alphabet names every command a sequence may use; the names are what
// regression files record, so they must stay stable.
func alphabet(h *harness) map[string]spec.Cmd {
	return map[string]spec.Cmd{
		"create":         create(a),
		"create-port":    create(a, "8001"),
		"apply":          spec.CmdApply{Target: a},
		"install-jq":     spec.CmdInstall{Target: a, Packages: []string{"jq"}},
		"uninstall-jq":   spec.CmdUninstall{Target: a, Packages: []string{"jq"}},
		"set-cpus-1":     spec.CmdSet{Target: a, CPUs: types.Some(1)},
		"set-cpus-2":     spec.CmdSet{Target: a, CPUs: types.Some(2)},
		"set-cpus-0":     spec.CmdSet{Target: a, CPUs: types.Some(0)},
		"forward-8001":   spec.CmdForward{Target: a, Ports: []string{"8001"}},
		"forward-busy":   spec.CmdForward{Target: a, Ports: []string{strconv.Itoa(busyPort)}},
		"unforward-8001": spec.CmdUnforward{Target: a, Ports: []string{"8001"}},
		"mount-A":        spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
		"mount-A-ro":     spec.CmdMount{Target: a, Dirs: []string{h.path("A")}, ReadOnly: true},
		"mount-sub":      spec.CmdMount{Target: a, Dirs: []string{h.path("A/sub")}},
		"mount-LB-ro":    spec.CmdMount{Target: a, Dirs: []string{h.path("LB")}, ReadOnly: true},
		"unmount-A":      spec.CmdUnmount{Target: a, Dirs: []string{h.path("A")}},
		"unmount-LB":     spec.CmdUnmount{Target: a, Dirs: []string{h.path("LB")}},
		"edit-shares":    spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, Memory: "1G", Shares: []spec.RawShare{{Path: h.path("B"), Mode: "ro"}}}},
		"edit-cpus":      spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 1, Memory: "1G"}},
		"edit-invalid":   spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 999, Memory: "1G"}},
		"start":          spec.CmdStart{Names: []string{a}},
		"stop":           spec.CmdStop{Names: []string{a}},
		"restart":        spec.CmdRestart{Names: []string{a}},
		"run-0":          spec.CmdRun{Target: a, Code: 0},
		"run-3":          spec.CmdRun{Target: a, Code: 3},
		"rm":             spec.CmdRm{Names: []string{a}, Yes: true},
	}
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

// generate draws a sequence of n command names from seed; the first one
// creates the guest, so that most of the walk exercises a guest.
func generate(h *harness, seed uint64, n int) []string {
	names := make([]string, 0, len(alphabet(h)))
	for k := range alphabet(h) {
		names = append(names, k)
	}
	sort.Strings(names)
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	seq := []string{"create"}
	for len(seq) < n {
		seq = append(seq, names[r.IntN(len(names))])
	}
	return seq
}

// runSequence runs seq from a clean host and returns the first failing
// step's message, or "".
func (h *harness) runSequence(seq []string) string {
	h.cleanup()
	h.model = h.base
	defer h.cleanup()
	alpha := alphabet(h)
	for i, name := range seq {
		c, ok := alpha[name]
		if !ok {
			h.t.Fatalf("unknown command %q (the alphabet changed?)", name)
		}
		if msg := h.step(c); msg != "" {
			return fmt.Sprintf("step %d (%s): %s", i+1, name, msg)
		}
	}
	return ""
}

// shrink drops commands from a failing seq while it still fails, within a
// budget of runs (each is a real run of antic).
func (h *harness) shrink(seq []string, budget int) []string {
	for changed := true; changed && budget > 0; {
		changed = false
		for i := 1; i < len(seq) && budget > 0; i++ { // keep "create" first
			cand := append(append([]string{}, seq[:i]...), seq[i+1:]...)
			budget--
			if h.runSequence(cand) != "" {
				seq, changed = cand, true
				i--
			}
		}
	}
	return seq
}

// A regression file is a txtar archive: a comment saying where it came
// from and why it failed, and a "commands" file with one alphabet name per
// line.
func writeRegression(path, comment string, seq []string) error {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(comment), "\n") {
		b.WriteString("# " + l + "\n")
	}
	b.WriteString("-- commands --\n")
	for _, c := range seq {
		b.WriteString(c + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func readRegression(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var seq []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "-- ") && strings.HasSuffix(l, " --"):
			in = l == "-- commands --"
		case in && strings.TrimSpace(l) != "":
			seq = append(seq, strings.TrimSpace(l))
		}
	}
	if len(seq) == 0 {
		return nil, fmt.Errorf("%s: no commands", path)
	}
	return seq, nil
}

// TestE2ERegressions replays every pinned sequence.
func TestE2ERegressions(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "regress", "*.txtar"))
	if len(files) == 0 {
		t.Skip("no pinned sequences in testdata/regress")
	}
	h := newHarness(t)
	h.base = h.model
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			h.t = t
			seq, err := readRegression(f)
			if err != nil {
				t.Fatal(err)
			}
			if msg := h.runSequence(seq); msg != "" {
				t.Fatalf("%s\n%s", strings.Join(seq, " "), msg)
			}
		})
	}
	h.t = t
}

// TestE2ERandomSequences runs generated sequences (ANTIC_E2E_RANDOM).
func TestE2ERandomSequences(t *testing.T) {
	n := envInt("ANTIC_E2E_RANDOM", 0)
	if n == 0 {
		t.Skip("ANTIC_E2E_RANDOM is not set")
	}
	h := newHarness(t)
	h.base = h.model
	seed := uint64(envInt("ANTIC_E2E_SEED", int(rand.Uint32())))
	length := envInt("ANTIC_E2E_LENGTH", 8)
	for i := range n {
		s := seed + uint64(i)
		t.Run(fmt.Sprintf("seed-%d", s), func(t *testing.T) {
			h.t = t
			seq := generate(h, s, length)
			msg := h.runSequence(seq)
			if msg == "" {
				t.Logf("%s: ok", strings.Join(seq, " "))
				return
			}
			small := h.shrink(seq, 3*len(seq))
			final := h.runSequence(small)
			path := filepath.Join("testdata", "regress", fmt.Sprintf("seed-%d.txtar", s))
			comment := fmt.Sprintf("Generated with ANTIC_E2E_SEED=%d ANTIC_E2E_LENGTH=%d and shrunk from:\n  %s\nIt failed with:\n%s",
				s, length, strings.Join(seq, " "), final)
			if err := writeRegression(path, comment, small); err != nil {
				t.Error(err)
			}
			t.Fatalf("%s\n%s\n(shrunk to %s, pinned as %s)", strings.Join(seq, " "), msg, strings.Join(small, " "), path)
		})
	}
	h.t = t
}
