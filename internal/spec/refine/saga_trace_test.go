package refine_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/tea/operation"
)

// traceEvent is one line of a saga trace, as internal/host/guest writes it
// (its traceEvent; the JSON format is the contract between the two).
type traceEvent struct {
	PID     int             `json:"pid"`
	Guest   string          `json:"guest"`
	Event   string          `json:"event"`
	Commits int             `json:"commits"`
	Stage   operation.Stage `json:"stage"`
	OK      bool            `json:"ok"`
}

// Trace validation of the imperative shell against formal/saga/Saga.tla:
// the E2E tests run antic with ANTIC_SAGA_TRACE, so every real command
// records when it takes and releases a guest's lock, what it plans and how
// each effect of its saga ends. Each guest's events, mapped to TLA+
// actions by the same table as the Go core's replay (sagaAction), must be
// a behavior of the model: they are replayed on TLC's state graph as on a
// nondeterministic automaton (the model's process identities and plan
// choices are what it guesses). A process whose events end without a
// release was killed, which the model takes as Crash.
//
// Between commands the model is reset to its initial state whenever it is
// quiescent (no lock holder, no journal): from there its behavior is the
// initial one up to renaming versions, so a trace of any length is checked
// against the bounded model. A segment between such points must fit the
// model's bounds (SagaTrace.cfg: three commands, two crashes, plans of up to
// five commit steps).

// traceGraph is a TLC graph indexed for replay.
type traceGraph struct {
	init string
	vars map[string]map[string]string // state -> variable -> value
	next map[string][][2]string       // state -> (successor, action)
}

func indexGraph(g tlcGraph) traceGraph {
	tg := traceGraph{init: g.init, vars: map[string]map[string]string{}, next: map[string][][2]string{}}
	for s := range g.states {
		vs := map[string]string{}
		for _, f := range strings.Fields(s) {
			k, v, _ := strings.Cut(f, "=")
			vs[k] = v
		}
		tg.vars[s] = vs
	}
	for e := range g.edges {
		tg.next[e[0]] = append(tg.next[e[0]], [2]string{e[1], e[2]})
	}
	return tg
}

// fnAt returns f[key] of a canonical TLA+ function "(k:>v@@...)".
func fnAt(f, key string) string {
	for _, kv := range strings.Split(strings.Trim(f, "()"), "@@") {
		if k, v, ok := strings.Cut(kv, ":>"); ok && k == key {
			return v
		}
	}
	return ""
}

func (tg traceGraph) quiescent(s string) bool {
	vs := tg.vars[s]
	return vs["lock"] == "None" && vs["journal"] == "None" && strings.Count(vs["pc"], `"idle"`) == nProcs
}

// step follows action from every state of set; with plan set, only to
// states where the lock holder plans that many commit steps.
func (tg traceGraph) step(set map[string]bool, action string, plan int) map[string]bool {
	out := map[string]bool{}
	for s := range set {
		for _, e := range tg.next[s] {
			if e[1] != action {
				continue
			}
			if plan >= 0 {
				vs := tg.vars[e[0]]
				if fnAt(vs["plen"], vs["lock"]) != fmt.Sprint(plan) {
					continue
				}
			}
			out[e[0]] = true
		}
	}
	return out
}

// traceAction is one model action a trace implies.
type traceAction struct {
	action string
	plan   int // the plan length for PlanCmd, else -1
	event  int // the trace line it comes from
}

// actionsOf maps one guest's events to model actions, adding a Crash after
// the last event of every process that did not release the lock.
func actionsOf(events []traceEvent, lines []int) []traceAction {
	last := map[int]int{}
	for i, ev := range events {
		last[ev.PID] = i
	}
	var out []traceAction
	for i, ev := range events {
		a := traceAction{plan: -1, event: lines[i]}
		switch ev.Event {
		case "acquire":
			a.action = "Acquire"
		case "plan":
			a.action, a.plan = "PlanCmd", ev.Commits
		case "step":
			a.action = sagaAction(ev.Stage, ev.OK)
		case "release":
			a.action = "Release"
		default:
			a.action = "?" + ev.Event
		}
		if a.action != "" {
			out = append(out, a)
		}
		if last[ev.PID] == i && ev.Event != "release" {
			out = append(out, traceAction{action: "Crash", plan: -1, event: lines[i]})
		}
	}
	return out
}

// validate replays actions on the model and returns the first deviation.
func (tg traceGraph) validate(actions []traceAction) error {
	set := map[string]bool{tg.init: true}
	for _, a := range actions {
		next := tg.step(set, a.action, a.plan)
		if len(next) == 0 && a.action == "Release" {
			// A command that returned before planning (nothing to do, or
			// rejected) is a no-op: PlanCmd with no commit step.
			next = tg.step(tg.step(set, "PlanCmd", 0), "Release", -1)
		}
		if len(next) == 0 {
			return fmt.Errorf("line %d: %s is not a behavior of Saga.tla here (or the segment exceeds its bounds)", a.event, a.action)
		}
		set = next
		all := true
		for s := range set {
			all = all && tg.quiescent(s)
		}
		if all {
			set = map[string]bool{tg.init: true}
		}
	}
	return nil
}

// TestShellTracesRefineSagaModel validates the traces in $ANTIC_SAGA_TRACES
// (the E2E tests write them; see formal/README.md).
func TestShellTracesRefineSagaModel(t *testing.T) {
	dir := os.Getenv("ANTIC_SAGA_TRACES")
	if dir == "" {
		t.Skip("ANTIC_SAGA_TRACES is not set (run the E2E tests with traces first)")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no traces in %s: %v", dir, err)
	}
	tg := indexGraph(readTLCGraph(t, "SagaTrace.cfg"))
	total := 0
	for _, f := range files {
		byGuest := map[string][]traceEvent{}
		lineNo := map[string][]int{}
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for n := 1; sc.Scan(); n++ {
			var ev traceEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("%s:%d: %v", f, n, err)
			}
			byGuest[ev.Guest] = append(byGuest[ev.Guest], ev)
			lineNo[ev.Guest] = append(lineNo[ev.Guest], n)
			total++
		}
		fh.Close()
		for g, evs := range byGuest {
			if err := tg.validate(actionsOf(evs, lineNo[g])); err != nil {
				t.Errorf("%s (guest %s): %v", filepath.Base(f), g, err)
			}
		}
	}
	t.Logf("validated %d events in %d traces", total, len(files))
}

// TestTraceValidationRejects checks that the validator can fail: a shell
// that plans without recovering a pending journal, or that writes before
// recording the intent, is not a behavior of the model.
func TestTraceValidationRejects(t *testing.T) {
	tg := indexGraph(readTLCGraph(t, "SagaTrace.cfg"))
	act := func(as ...string) []traceAction {
		var out []traceAction
		for i, a := range as {
			ta := traceAction{action: a, plan: -1, event: i + 1}
			if n, ok := strings.CutPrefix(a, "PlanCmd"); ok {
				ta.action, ta.plan = "PlanCmd", int(n[0]-'0')
			}
			out = append(out, ta)
		}
		return out
	}
	good := act("Acquire", "PlanCmd1", "Prepare", "Intent", "Crash", "Acquire", "Recover", "RecoverDone", "PlanCmd0", "Release")
	if err := tg.validate(good); err != nil {
		t.Fatalf("a crash and its recovery: %v", err)
	}
	for name, bad := range map[string][]traceAction{
		"plan without recovering":  act("Acquire", "PlanCmd1", "Prepare", "Intent", "Crash", "Acquire", "PlanCmd0"),
		"apply without intent":     act("Acquire", "PlanCmd1", "Prepare", "Apply"),
		"release while committing": act("Acquire", "PlanCmd2", "Prepare", "Intent", "Apply", "Release"),
		"more steps than planned":  act("Acquire", "PlanCmd1", "Prepare", "Intent", "Apply", "Intent"),
	} {
		if err := tg.validate(bad); err == nil {
			t.Errorf("%s: the validator accepted it", name)
		}
	}
}
