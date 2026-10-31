//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

// Concurrent commands on one guest (what formal/saga/Saga.tla models):
// several antic processes start at once, and the guest's lock serializes
// them. The order they took the lock is read from their saga traces; the
// run must be linearizable in that order: applying the specification's Post
// in it gives every command's exit status and the final observation. One
// process may be killed mid-saga, in which case it changed nothing (P1)
// and the next to take the lock recovers its journal. The traces are kept
// for trace validation against the model (internal/spec/refine).

type concurrentRun struct {
	cmd    spec.Cmd
	crash  bool
	pid    int
	code   int
	stderr string
}

// runConcurrently starts every command at once and waits for all of them.
func (h *harness) runConcurrently(cmds []spec.Cmd, crashAt int, trace string) ([]concurrentRun, error) {
	out := make([]concurrentRun, len(cmds))
	var wg sync.WaitGroup
	var startErr error
	var mu sync.Mutex
	for i, c := range cmds {
		args, env := h.render(c)
		env = append(env, "ANTIC_SAGA_TRACE="+trace, "ANTIC_LOCK_TIMEOUT=15m")
		out[i] = concurrentRun{cmd: c}
		if i == 0 && crashAt > 0 {
			env = append(env, fmt.Sprintf("ANTIC_SAGA_CRASH_AFTER=%d", crashAt))
			out[i].crash = true
		}
		cmd := exec.CommandContext(context.Background(), h.bin, args...)
		cmd.Dir = h.work
		cmd.Env = append(os.Environ(), env...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		out[i].pid = cmd.Process.Pid
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cmd.Wait()
			var ee *exec.ExitError
			code := 0
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				mu.Lock()
				startErr = err
				mu.Unlock()
			}
			out[i].code, out[i].stderr = code, errb.String()
		}()
	}
	wg.Wait()
	return out, startErr
}

// lockOrder returns the pids in the order they first took a guest's lock.
func lockOrder(path, guest string) ([]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var order []int
	seen := map[int]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev struct {
			PID   int    `json:"pid"`
			Guest string `json:"guest"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, err
		}
		if ev.Guest == guest && ev.Event == "acquire" && !seen[ev.PID] {
			seen[ev.PID] = true
			order = append(order, ev.PID)
		}
	}
	return order, sc.Err()
}

// linearize applies the commands in lock order to the model and returns the
// worlds the specification permits after them, or why none is.
func (h *harness) linearize(runs []concurrentRun, order []int) ([]spec.World, error) {
	byPID := map[int]concurrentRun{}
	for _, r := range runs {
		byPID[r.pid] = r
	}
	worlds := []spec.World{h.model}
	for _, pid := range order {
		r, ok := byPID[pid]
		if !ok {
			return nil, fmt.Errorf("pid %d in the trace is none of the commands", pid)
		}
		if r.crash {
			if r.code != 137 {
				return nil, fmt.Errorf("the killed command %#v exited %d", r.cmd, r.code)
			}
			continue // P1: a killed command changes nothing, once recovered
		}
		var next []spec.World
		var why []string
		for _, w := range worlds {
			for _, o := range spec.Post(w, h.env, r.cmd) {
				if o.Exit() == r.code {
					next = append(next, o.World)
				} else {
					why = append(why, fmt.Sprintf("%s exits %d", kind(o), o.Exit()))
				}
			}
		}
		if len(next) == 0 {
			return nil, fmt.Errorf("%#v exited %d, which the specification does not permit here (%s)\nstderr: %s",
				r.cmd, r.code, strings.Join(why, ", "), r.stderr)
		}
		worlds = next
	}
	return worlds, nil
}

func TestE2EConcurrentCommands(t *testing.T) {
	h := newHarness(t)
	traces := os.Getenv("ANTIC_E2E_TRACES")
	if traces == "" {
		traces = t.TempDir()
	}
	pool := func(h *harness) []spec.Cmd {
		return []spec.Cmd{
			spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
			spec.CmdMount{Target: a, Dirs: []string{h.path("A")}, ReadOnly: true},
			spec.CmdUnmount{Target: a, Dirs: []string{h.path("A")}},
			spec.CmdForward{Target: a, Ports: []string{"8001"}},
			spec.CmdUnforward{Target: a, Ports: []string{"8001"}},
			spec.CmdSet{Target: a, CPUs: types.Some(1)},
			spec.CmdSet{Target: a, CPUs: types.Some(2)},
			spec.CmdRestart{Names: []string{a}},
			spec.CmdStop{Names: []string{a}},
			spec.CmdStart{Names: []string{a}},
		}
	}
	seed := uint64(envInt("ANTIC_E2E_SEED", 7))
	rounds := envInt("ANTIC_E2E_ROUNDS", 4)
	for round := range rounds {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model = h.base
			defer h.cleanup()
			if msg := h.step(create(a)); msg != "" {
				t.Fatal(msg)
			}
			r := rand.New(rand.NewPCG(seed, uint64(round)))
			all := pool(h)
			r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
			cmds := all[:3+r.IntN(2)]
			// Every other round kills the first command after its journal
			// is open (JournalBegin, Intent, the first commit step).
			crashAt := 0
			if round%2 == 1 {
				crashAt = 3
			}
			trace := filepath.Join(traces, fmt.Sprintf("TestE2EConcurrentCommands_round-%d.ndjson", round))
			os.Remove(trace)
			runs, err := h.runConcurrently(cmds, crashAt, trace)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range runs {
				t.Logf("pid %d crash=%v exit %d: %#v", r.pid, r.crash, r.code, r.cmd)
			}
			order, err := lockOrder(trace, a)
			if err != nil {
				t.Fatal(err)
			}
			if crashAt > 0 && runs[0].code != 137 {
				// The first command finished before its third effect (a
				// no-op or a rejection): nothing was killed.
				runs[0].crash = false
			}
			// A command recovers a killed one's journal before its own; one
			// more start makes sure the recovery ran even if the killed
			// command took the lock last (it is a no-op on a running guest).
			_, startCode, startErr := h.antic(nil, "guest", "start", a)
			obs, err := h.observe()
			if err != nil {
				t.Fatal(err)
			}
			worlds, err := h.linearize(runs, order)
			if err != nil {
				t.Fatalf("lock order %v: %v", order, err)
			}
			var why []string
			for _, w := range worlds {
				for _, o := range spec.Post(w, h.env, spec.CmdStart{Names: []string{a}}) {
					if o.Exit() != startCode {
						continue
					}
					if d := diff(o.World, obs); d != "" {
						why = append(why, d)
						continue
					}
					h.model = o.World
					return
				}
			}
			why = append(why, "final start: exit "+fmt.Sprint(startCode)+": "+startErr)
			t.Fatalf("lock order %v: the guest is in no state the specification permits:\n%s", order, strings.Join(why, "\n---\n"))
		})
	}
	h.t = t
}

// TestE2EConcurrentPortClaims: two guests claim the same host port at the
// same time (D26). The ports a guest may forward depend on the other
// guests' configs, so the claims must be serialized across guests: exactly
// one succeeds, and the host port is forwarded once (Invariant 3).
func TestE2EConcurrentPortClaims(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	for _, c := range []spec.Cmd{create(a), create(b), spec.CmdStop{Names: []string{a, b}}} {
		if msg := h.step(c); msg != "" {
			t.Fatal(msg)
		}
	}
	for round := range 3 {
		port := fmt.Sprint(8002)
		runs, err := h.runConcurrently([]spec.Cmd{
			spec.CmdForward{Target: a, Ports: []string{port}},
			spec.CmdForward{Target: b, Ports: []string{port}},
		}, 0, filepath.Join(t.TempDir(), "trace.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		ok := 0
		for _, r := range runs {
			if r.code == 0 {
				ok++
			}
		}
		owners := 0
		for _, g := range []string{a, b} {
			cfg, err := h.readConfig(g)
			if err != nil {
				t.Fatal(err)
			}
			owners += cfg.Ports.Len()
		}
		if ok != 1 || owners != 1 {
			t.Fatalf("round %d: %d of the two claims succeeded and %d guests forward port %s, want 1 and 1\n%+v", round, ok, owners, port, runs)
		}
		// Release the port for the next round.
		for _, g := range []string{a, b} {
			h.antic(nil, "guest", "unforward", g, port)
		}
	}
}
