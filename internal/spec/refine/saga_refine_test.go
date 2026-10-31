package refine_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

// The refinement check between formal/saga/Saga.tla and the Go core
// internal/tea/operation: TLC dumps the model's complete state graph for
// SagaJournal.cfg; a harness runs the Go core (operation.Update) for the
// same processes, commands and crashes on a host of integer versions, and
// every Go transition, mapped by the abstraction function alpha, must be a
// stutter or an edge of the TLC graph with the action the mapping table
// (action) names; conversely every TLC state and edge must be reached. The
// Go core and the model are thus equivalent up to stuttering on this
// finite instance (a bisimulation of the abstraction).
//
// Assumptions the model makes and the harness therefore enforces: an
// inverse effect (compensation, recovery) does not fail, and an effect is
// atomic (a crash falls between effects). Mutants of the Go side check
// that the comparison can fail.

// The commands processes run, by plan length n (Saga.tla's PlanCmd chooses
// 0..NRes commit steps): starting a running guest (none), stopping it
// (StopVM), mounting a directory on it (WriteConfig, Hotplug), and a config
// and package change (WriteConfig, SetProfile, Switch). The model numbers
// a plan's resources by position, so the i-th commit step moves resource i.
func sagaOps() []operation.Op {
	added := types.BuildSet[domain.AbsPath, domain.AbsPath, domain.AbsPathOrder]([]domain.AbsPath{abs(dirA)})
	running := func(k operation.OpKind, ch operation.Change) operation.Op {
		return operation.Op{Kind: k, Phase: operation.GuestRunning, Change: ch}
	}
	return []operation.Op{
		running(operation.OpStart, operation.Change{}),
		running(operation.OpStop, operation.Change{}),
		running(operation.OpChangeConfig, operation.Change{Config: operation.Changed, Added: added}),
		running(operation.OpChangeConfig, operation.Change{Config: operation.Changed, System: operation.Changed}),
	}
}

// planOf returns the saga's plan for op.
func planOf(op operation.Op) operation.Model {
	m, _ := operation.Update(operation.Init(guest()), operation.MsgCommand{Op: op})
	return m
}

// The bounds of SagaJournal.cfg (checked against it by readTLCGraph).
const (
	nRes       = 3
	nProcs     = 2
	maxCmds    = 3
	maxCrashes = 2
)

type versions [nRes]int

// sagaProc is one antic process; base and target are the concrete old and new
// artifacts its shell holds (versions here), plen its plan's length.
type sagaProc struct {
	lock       bool // holds the guest's lock and has not reported
	toPlan     bool // holds the lock, recovered, and has not planned yet
	recovering bool
	model      operation.Model
	pending    operation.Effect // nil without a saga
	base       versions
	target     int
	plen       int
}

type sagaJournal struct {
	open    bool
	plen    int // the command recorded (sagaOps()[plen])
	base    versions
	intents []domain.StepIndex
}

type sagaHost struct {
	val     versions
	journal sagaJournal
	procs   [nProcs]sagaProc
	cmds    int
	crashes int
	lastOk  versions
}

func (h sagaHost) clone() sagaHost {
	h.journal.intents = slices.Clone(h.journal.intents)
	return h
}

func (h sagaHost) key() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v %+v %d %d %v", h.val, h.journal, h.cmds, h.crashes, h.lastOk)
	for _, p := range h.procs {
		fmt.Fprintf(&b, "|%v %v %v %v %d %d %d %d %s %#v", p.lock, p.toPlan, p.recovering, p.base, p.target, p.plen,
			p.model.Stage, p.model.Pos.Get(), p.model.Reason.Get(), p.pending)
	}
	return b.String()
}

// mutant perturbs the Go side to check that the refinement check fails.
type mutant uint8

const (
	mutNone         mutant = iota
	mutNoBegin             // the journal is never opened
	mutNoIntent            // intents are not recorded
	mutUndoToNew           // inverse effects restore the new version
	mutRecoverShort        // recovery skips the last recorded intent
	mutStaleBase           // the process plans from the state it read before the lock
	mutSucceedEarly        // the command counts as succeeded before its journal is cleared
)

func idle(p sagaProc) bool { return !p.lock && p.pending == nil && !p.toPlan }

func guest() domain.GuestName {
	n, _ := domain.ParseGuestName("e2e-a")
	return n
}

type transition struct {
	action string // the TLA+ action, or "" for a stutter
	next   sagaHost
}

// update feeds msg to process i's saga and checks the core's own predicates.
func update(t *testing.T, p *sagaProc, msg operation.Msg) {
	t.Helper()
	next, effects := operation.Update(p.model, msg)
	if err := operation.Invariant(next); err != nil {
		t.Fatal(err)
	}
	if err := operation.FrameValid(p.model, msg, next); err != nil {
		t.Fatal(err)
	}
	if err := operation.StepValid(p.model, msg, next, effects); err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 {
		t.Fatalf("saga stalled in stage %d", next.Stage)
	}
	p.model, p.pending = next, effects[0]
}

// sagaAction is the mapping table from a Go transition (the stage whose
// effect ran and its outcome) to the TLA+ action it implements ("" for a
// stutter). The shell's traces are checked through it too.
func sagaAction(stage operation.Stage, ok bool) string {
	switch stage {
	case operation.StagePrepare:
		if !ok {
			return "PrepareFail"
		}
		return "" // a check or build: nothing observable
	case operation.StageBegin:
		if !ok {
			return "PrepareFail"
		}
		return "Prepare"
	case operation.StageIntent:
		if !ok {
			return "IntentFail"
		}
		return "Intent"
	case operation.StageCommit:
		if !ok {
			return "ApplyFail"
		}
		return "Apply"
	case operation.StageClose:
		if !ok {
			return "FinishFail"
		}
		return "Finish"
	case operation.StageCompensate:
		return "Compensate"
	case operation.StageAbort:
		if !ok {
			return "CompensateDoneFail"
		}
		return "CompensateDone"
	case operation.StageRecover:
		return "Recover"
	case operation.StageRecoverClose:
		if !ok {
			return "RecoverFail"
		}
		return "RecoverDone"
	case operation.StageFinalize:
		return "" // irreversible clean-up
	case operation.StageIdle, operation.StageDone, operation.StageFailed, operation.StageRecovered, operation.StageBroken:
	}
	return "?"
}

// slotAt is the resource (0-based) the commit step at pos of m moves.
func slotAt(m operation.Model, pos int) int { return pos - firstCommitOf(m) }

// execute runs process i's pending effect with the given outcome on h: a
// forward commit step sets its resource to the command's target, an
// inverse one back to the version the command (or the journal) started
// from. operation.StepValid has checked that each effect is the planned
// step or its inverse.
func execute(t *testing.T, h sagaHost, i int, ok bool, mut mutant) transition {
	t.Helper()
	h = h.clone()
	p := &h.procs[i]
	stage := p.model.Stage
	if _, report := p.pending.(operation.EffectReport); report {
		h.procs[i] = sagaProc{base: p.base, target: p.target}
		return transition{"Release", h}
	}
	act := sagaAction(stage, ok)
	pos := p.model.Pos.Get()
	if ok {
		switch stage {
		case operation.StageCommit:
			h.val[slotAt(p.model, pos)] = p.target
		case operation.StageCompensate, operation.StageRecover:
			r := slotAt(p.model, pos-1)
			switch {
			case mut == mutUndoToNew:
				h.val[r] = p.target
			case p.recovering:
				h.val[r] = h.journal.base[r]
			default:
				h.val[r] = p.base[r]
			}
		case operation.StageBegin:
			if mut != mutNoBegin {
				h.journal = sagaJournal{open: true, plen: p.plen, base: p.base}
			}
		case operation.StageIntent:
			if h.journal.open && mut != mutNoIntent {
				h.journal.intents = append(h.journal.intents, p.model.Pos)
			}
		case operation.StageClose, operation.StageAbort, operation.StageRecoverClose:
			h.journal = sagaJournal{}
			if stage == operation.StageClose && mut != mutSucceedEarly {
				for r := range p.plen {
					h.lastOk[r] = p.target
				}
			}
		case operation.StageIdle, operation.StagePrepare, operation.StageFinalize, operation.StageDone,
			operation.StageFailed, operation.StageRecovered, operation.StageBroken:
		}
		if mut == mutSucceedEarly && stage == operation.StageCommit && slotAt(p.model, pos)+1 == p.plen {
			for r := range p.plen {
				h.lastOk[r] = p.target
			}
		}
	}
	status := operation.ResultOk
	if !ok {
		status = operation.ResultErr
	}
	update(t, p, operation.MsgResult{Status: status, Reason: domain.NewText("injected")})
	if p.recovering && p.model.Stage == operation.StageRecovered {
		// Recovered: the process now plans its own command.
		h.procs[i] = sagaProc{lock: true, toPlan: true, base: p.base, target: p.target}
	}
	return transition{act, h}
}

// successors enumerates every transition of h.
func successors(t *testing.T, h sagaHost, mut mutant) []transition {
	t.Helper()
	var out []transition
	held := slices.ContainsFunc(h.procs[:], func(p sagaProc) bool { return p.lock })
	for i, p := range h.procs {
		switch {
		case idle(p):
			if held || h.cmds >= maxCmds {
				break
			}
			n := h.clone()
			q := &n.procs[i]
			n.cmds++
			*q = sagaProc{lock: true, base: p.base, target: n.cmds}
			if n.journal.open {
				last := types.None[domain.StepIndex]()
				intents := n.journal.intents
				if mut == mutRecoverShort && len(intents) > 0 {
					intents = intents[:len(intents)-1]
				}
				if len(intents) > 0 {
					last = types.Some(intents[len(intents)-1])
				}
				q.recovering, q.model = true, operation.Init(guest())
				update(t, q, operation.MsgRecover{Op: sagaOps()[n.journal.plen], Last: last})
			} else {
				q.toPlan = true
			}
			out = append(out, transition{"Acquire", n})
		case p.toPlan:
			for plen, op := range sagaOps() {
				n := h.clone()
				q := &n.procs[i]
				q.toPlan, q.plen = false, plen
				if mut != mutStaleBase {
					q.base = n.val
				}
				q.model = operation.Init(guest())
				update(t, q, operation.MsgCommand{Op: op})
				out = append(out, transition{"PlanCmd", n})
			}
		default:
			out = append(out, execute(t, h, i, true, mut))
			_, report := p.pending.(operation.EffectReport)
			undo := p.model.Stage == operation.StageCompensate || p.model.Stage == operation.StageRecover
			if !report && !undo {
				out = append(out, execute(t, h, i, false, mut))
			}
		}
		if !idle(p) && h.crashes < maxCrashes {
			n := h.clone()
			n.crashes++
			n.procs[i] = sagaProc{base: p.base, target: p.target}
			out = append(out, transition{"Crash", n})
		}
	}
	return out
}

// Canonical TLA+ values: functions and records are printed with sorted keys.
func tlaSeq(xs []string) string { return "<<" + strings.Join(xs, ",") + ">>" }

func tlaFn(keys, vals []string) string {
	kv := make([]string, len(keys))
	for i := range keys {
		kv[i] = keys[i] + ":>" + vals[i]
	}
	sort.Strings(kv)
	return "(" + strings.Join(kv, "@@") + ")"
}

func tlaRec(kv map[string]string) string {
	var fs []string
	for k, v := range kv {
		fs = append(fs, k+"|->"+v)
	}
	sort.Strings(fs)
	return "[" + strings.Join(fs, ",") + "]"
}

func tlaState(vars map[string]string) string {
	var fs []string
	for k, v := range vars {
		fs = append(fs, k+"="+v)
	}
	sort.Strings(fs)
	return strings.Join(fs, " ")
}

func tlaVersions(v versions) string {
	var xs []string
	for _, x := range v {
		xs = append(xs, fmt.Sprint(x))
	}
	return tlaSeq(xs)
}

// resSeq is the model's resources of the commit steps at positions [from, to).
func resSeq(m operation.Model, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		if m.Plan.All()[i].Phase == operation.PhaseCommit {
			out = append(out, fmt.Sprint(slotAt(m, i)+1))
		}
	}
	return out
}

func firstCommitOf(m operation.Model) int {
	for _, s := range m.Plan.All() {
		if s.Phase != operation.PhasePrepare {
			return s.Index.Get()
		}
	}
	return m.Plan.Len()
}

func procName(i int) string { return fmt.Sprintf("p%d", i+1) }

func procList() string {
	var ps []string
	for i := range nProcs {
		ps = append(ps, procName(i))
	}
	return strings.Join(ps, ", ")
}

// alpha is the abstraction function from the Go host to a Saga.tla state.
func alpha(h sagaHost) string {
	var names, pcs, bases, targets, todos, dones, plens []string
	lock := "None"
	for i, p := range h.procs {
		names = append(names, procName(i))
		if p.lock {
			lock = procName(i)
		}
		bases = append(bases, tlaVersions(p.base))
		targets = append(targets, fmt.Sprint(p.target))
		plens = append(plens, fmt.Sprint(p.plen))
		m := p.model
		pos, first := m.Pos.Get(), firstCommitOf(m)
		pc, todo, done := "idle", []string{}, []string{}
		_, report := p.pending.(operation.EffectReport)
		switch {
		case idle(p):
		case p.toPlan:
			pc = "plan"
		case report || m.Stage == operation.StageFinalize:
			pc = "done"
		case m.Stage == operation.StageRecover || m.Stage == operation.StageRecoverClose:
			pc, done = "recover", resSeq(m, first, pos)
		case m.Stage == operation.StagePrepare || m.Stage == operation.StageBegin:
			pc = "prepare"
		case m.Stage == operation.StageIntent || m.Stage == operation.StageCommit || m.Stage == operation.StageClose:
			pc, todo, done = "commit", resSeq(m, pos, m.Plan.Len()), resSeq(m, first, pos)
		case m.Stage == operation.StageCompensate || m.Stage == operation.StageAbort:
			pc, done = "compensate", resSeq(m, first, pos)
		default:
			pc = fmt.Sprintf("?%d", m.Stage)
		}
		pcs = append(pcs, `"`+pc+`"`)
		todos = append(todos, tlaSeq(todo))
		dones = append(dones, tlaSeq(done))
	}
	jr := "None"
	if h.journal.open {
		var is []string
		m := planOf(sagaOps()[h.journal.plen])
		for _, pos := range h.journal.intents {
			is = append(is, fmt.Sprint(slotAt(m, pos.Get())+1))
		}
		jr = tlaRec(map[string]string{"base": tlaVersions(h.journal.base), "intents": tlaSeq(is)})
	}
	return tlaState(map[string]string{
		"val": tlaVersions(h.val), "lock": lock, "pc": tlaFn(names, pcs), "base": tlaFn(names, bases),
		"target": tlaFn(names, targets), "todo": tlaFn(names, todos), "done": tlaFn(names, dones),
		"plen": tlaFn(names, plens), "journal": jr, "cmds": fmt.Sprint(h.cmds), "crashes": fmt.Sprint(h.crashes),
		"lastOk": tlaVersions(h.lastOk),
	})
}

// checkHarnessBounds checks that cfg sets the bounds the replay harness runs.
func checkHarnessBounds(t *testing.T, cfg string) {
	t.Helper()
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"NRes": fmt.Sprint(nRes), "MaxCmds": fmt.Sprint(maxCmds), "MaxCrashes": fmt.Sprint(maxCrashes),
		"Journaling": "TRUE", "PlanUnderLock": "TRUE",
	} {
		if !strings.Contains(string(raw), name+" = "+want+"\n") {
			t.Fatalf("%s must set %s = %s, as the harness does", cfg, name, want)
		}
	}
	for n, op := range sagaOps() {
		m := planOf(op)
		if got := len(resSeq(m, 0, m.Plan.Len())); got != n {
			t.Fatalf("sagaOps()[%d] plans %d commit steps", n, got)
		}
	}
	if len(sagaOps()) != nRes+1 || !strings.Contains(string(raw), fmt.Sprintf("Procs = {%s}\n", procList())) {
		t.Fatalf("%s: the harness runs %d processes with plans of 0..%d steps", cfg, nProcs, len(sagaOps())-1)
	}
}

// tlcGraph is a TLC state graph: canonical states and labelled edges.
type tlcGraph struct {
	init   string
	states map[string]bool
	edges  map[[3]string]bool // from, to, action
}

// tlaValue parses one TLC-printed value into its canonical form.
type tlaParser struct {
	s   string
	pos int
}

func (p *tlaParser) ws() {
	for p.pos < len(p.s) && strings.ContainsRune(" \t\n", rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *tlaParser) eat(tok string) bool {
	p.ws()
	if strings.HasPrefix(p.s[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *tlaParser) value() (string, error) {
	p.ws()
	switch {
	case p.eat("<<"):
		var xs []string
		for !p.eat(">>") {
			x, err := p.value()
			if err != nil {
				return "", err
			}
			xs = append(xs, x)
			p.eat(",")
		}
		return tlaSeq(xs), nil
	case p.eat("["):
		kv := map[string]string{}
		for !p.eat("]") {
			k := p.atom()
			if !p.eat("|->") {
				return "", fmt.Errorf("record without |-> at %d in %q", p.pos, p.s)
			}
			v, err := p.value()
			if err != nil {
				return "", err
			}
			kv[k] = v
			p.eat(",")
		}
		return tlaRec(kv), nil
	case p.eat("("):
		var ks, vs []string
		for !p.eat(")") {
			k, err := p.value()
			if err != nil {
				return "", err
			}
			if !p.eat(":>") {
				return "", fmt.Errorf("function without :> at %d in %q", p.pos, p.s)
			}
			v, err := p.value()
			if err != nil {
				return "", err
			}
			ks, vs = append(ks, k), append(vs, v)
			p.eat("@@")
		}
		return tlaFn(ks, vs), nil
	}
	if a := p.atom(); a != "" {
		return a, nil
	}
	return "", fmt.Errorf("unexpected %q at %d", p.s[p.pos:], p.pos)
}

func (p *tlaParser) atom() string {
	p.ws()
	start := p.pos
	if p.pos < len(p.s) && p.s[p.pos] == '"' {
		end := strings.IndexByte(p.s[p.pos+1:], '"')
		p.pos += end + 2
		return p.s[start:p.pos]
	}
	for p.pos < len(p.s) && (p.s[p.pos] == '_' || ('a' <= p.s[p.pos] && p.s[p.pos] <= 'z') ||
		('A' <= p.s[p.pos] && p.s[p.pos] <= 'Z') || ('0' <= p.s[p.pos] && p.s[p.pos] <= '9')) {
		p.pos++
	}
	return p.s[start:p.pos]
}

// parseState parses a TLC state ("/\ x = v" lines) into canonical form.
func parseState(label string) (string, error) {
	vars := map[string]string{}
	for _, line := range strings.Split(label, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), `/\ `)
		if line == "" {
			continue
		}
		name, val, ok := strings.Cut(line, " = ")
		if !ok {
			return "", fmt.Errorf("bad state line %q", line)
		}
		p := &tlaParser{s: val}
		v, err := p.value()
		if err != nil {
			return "", err
		}
		if p.ws(); p.pos != len(p.s) {
			return "", fmt.Errorf("trailing %q", p.s[p.pos:])
		}
		vars[name] = v
	}
	return tlaState(vars), nil
}

// unquote decodes a dot string literal.
func unquote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// attr returns the quoted value of attribute name in a dot line.
func attr(line, name string) (string, bool) {
	i := strings.Index(line, name+`="`)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(name)+2:]
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] == '"' {
			return unquote(rest[:j]), true
		}
	}
	return "", false
}

func readTLCGraph(t *testing.T, cfg string) tlcGraph {
	t.Helper()
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
	if cfg == "SagaJournal.cfg" {
		checkHarnessBounds(t, filepath.Join(dir, cfg))
	}
	tmp := t.TempDir()
	dot := filepath.Join(tmp, "graph.dot")
	cmd := exec.Command(tlc, "-workers", "1", "-metadir", tmp, "-dump", "dot,actionlabels", dot, "-config", cfg, "Saga.tla")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "No error has been found") {
		t.Fatalf("TLC: %v\n%s", err, out)
	}
	f, err := os.Open(dot)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g := tlcGraph{states: map[string]bool{}, edges: map[[3]string]bool{}}
	ids := map[string]string{}
	type edge struct{ from, to, label string }
	var edges []edge
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "->" {
			label, ok := attr(line, "label")
			if !ok {
				continue // a stuttering step
			}
			edges = append(edges, edge{fields[0], fields[2], label})
			continue
		}
		label, ok := attr(line, "label")
		if !ok || !strings.HasPrefix(label, `/\`) {
			continue
		}
		s, err := parseState(label)
		if err != nil {
			t.Fatalf("parsing TLC state: %v", err)
		}
		ids[fields[0]] = s
		g.states[s] = true
		if strings.Contains(line, "style = filled") {
			g.init = s
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		from, to := ids[e.from], ids[e.to]
		if from == "" || to == "" {
			t.Fatalf("edge between unknown states %s -> %s", e.from, e.to)
		}
		if from != to {
			g.edges[[3]string{from, to, e.label}] = true
		}
	}
	return g
}

// compare explores the Go host and returns the refinement discrepancies.
func compare(t *testing.T, g tlcGraph, mut mutant) (problems []string, states, transitions int) {
	t.Helper()
	start := sagaHost{}
	if a := alpha(start); a != g.init {
		return []string{fmt.Sprintf("initial states differ:\n go  %s\n tla %s", a, g.init)}, 0, 0
	}
	seen := map[string]bool{start.key(): true}
	reached := map[string]bool{alpha(start): true}
	covered := map[[3]string]bool{}
	queue := []sagaHost{start}
	for len(queue) > 0 && len(problems) < 5 {
		h := queue[0]
		queue = queue[1:]
		a := alpha(h)
		for _, tr := range successors(t, h, mut) {
			transitions++
			b := alpha(tr.next)
			switch {
			case tr.action == "" && a == b:
			case tr.action == "":
				problems = append(problems, fmt.Sprintf("a stutter changed the state:\n from %s\n to   %s", a, b))
			case !g.edges[[3]string{a, b, tr.action}]:
				problems = append(problems, fmt.Sprintf("%s is not a TLC edge:\n from %s\n to   %s", tr.action, a, b))
			default:
				covered[[3]string{a, b, tr.action}] = true
			}
			reached[b] = true
			if k := tr.next.key(); !seen[k] {
				seen[k] = true
				queue = append(queue, tr.next)
			}
		}
	}
	states = len(seen)
	if len(problems) > 0 {
		return problems, states, transitions
	}
	for s := range g.states {
		if !reached[s] {
			problems = append(problems, "TLC state not reached by the Go core: "+s)
			break
		}
	}
	for e := range g.edges {
		if !covered[e] {
			problems = append(problems, fmt.Sprintf("TLC edge %s not taken by the Go core:\n from %s\n to   %s", e[2], e[0], e[1]))
			break
		}
	}
	return problems, states, transitions
}

// TestGoCoreRefinesSagaModel replays the TLC state graph of the journaling
// saga against operation.Update, and checks that every mutant is caught.
func TestGoCoreRefinesSagaModel(t *testing.T) {
	g := readTLCGraph(t, "SagaJournal.cfg")
	problems, states, transitions := compare(t, g, mutNone)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("TLC: %d states, %d edges; Go core: %d states, %d transitions", len(g.states), len(g.edges), states, transitions)
	for _, m := range []struct {
		name string
		mut  mutant
	}{
		{"no journal begin", mutNoBegin},
		{"no intents", mutNoIntent},
		{"undo restores new", mutUndoToNew},
		{"recovery skips the last intent", mutRecoverShort},
		{"plan from a stale read", mutStaleBase},
		{"succeed before the journal is cleared", mutSucceedEarly},
	} {
		ps, _, _ := compare(t, g, m.mut)
		if len(ps) > 0 {
			t.Logf("mutant %q: %s", m.name, strings.SplitN(ps[0], "\n", 2)[0])
		}
		if len(ps) == 0 {
			t.Errorf("mutant %q: the refinement check did not detect it", m.name)
		}
	}
}
