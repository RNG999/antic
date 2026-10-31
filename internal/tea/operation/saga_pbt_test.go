package operation_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

// A stateful property-based test of the saga core beyond the bounds of the
// TLA+ model (formal/saga): two to four processes run any commands of the
// corpus on one guest, interleaved at random with failures of any effect
// (including the inverse effects the model assumes never fail) and with
// crashes. A host of resource versions executes the effects, and Saga.tla's
// Consistent is checked after every step: whenever no process holds the
// lock and no journal is pending, every resource is at the version the
// last successful command left it at.

type pbtProc struct {
	locked, toPlan, recovering bool
	model                      operation.Model
	pending                    operation.Effect
	base                       map[operation.Resource]int
	target                     int
}

type pbtJournal struct {
	open    bool
	op      operation.Op
	base    map[operation.Resource]int
	target  int
	intents []domain.StepIndex
}

type sagaMachine struct {
	ops     []operation.Op
	procs   []pbtProc
	val     map[operation.Resource]int
	lastOk  map[operation.Resource]int
	journal pbtJournal
	cmds    int
}

func clone(m map[operation.Resource]int) map[operation.Resource]int {
	out := map[operation.Resource]int{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// version is the value a forward step leaves its resource at: the
// command's target, or a distinct value for Mid (a stopped VM).
func version(s operation.Step, target int) int {
	if s.To == operation.VersionMid {
		return -target
	}
	return target
}

// before is the value of step i's resource before step i ran: the version
// an earlier commit step on it left, or the command's base.
func before(plan []operation.Step, i, target int, base map[operation.Resource]int) int {
	for j := i - 1; j >= 0; j-- {
		if plan[j].Phase == operation.PhaseCommit && plan[j].Res == plan[i].Res {
			return version(plan[j], target)
		}
	}
	return base[plan[i].Res]
}

func (sm *sagaMachine) update(t *rapid.T, p *pbtProc, msg operation.Msg) {
	next, effects := operation.Update(p.model, msg)
	for _, err := range []error{operation.Invariant(next), operation.FrameValid(p.model, msg, next), operation.StepValid(p.model, msg, next, effects)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(effects) != 1 {
		t.Fatalf("saga stalled in stage %d", next.Stage)
	}
	p.model, p.pending = next, effects[0]
}

func (sm *sagaMachine) holder() int {
	for i, p := range sm.procs {
		if p.locked {
			return i
		}
	}
	return -1
}

func (sm *sagaMachine) acquire(t *rapid.T) {
	i := rapid.IntRange(0, len(sm.procs)-1).Draw(t, "proc")
	p := &sm.procs[i]
	sm.cmds++
	*p = pbtProc{locked: true, target: sm.cmds, model: operation.Init(guestName())}
	if !sm.journal.open {
		p.toPlan = true
		return
	}
	last := types.None[domain.StepIndex]()
	if n := len(sm.journal.intents); n > 0 {
		last = types.Some(sm.journal.intents[n-1])
	}
	p.recovering = true
	sm.update(t, p, operation.MsgRecover{Op: sm.journal.op, Last: last})
}

func (sm *sagaMachine) plan(t *rapid.T) {
	p := &sm.procs[sm.holder()]
	op := rapid.SampledFrom(sm.ops).Draw(t, "op")
	p.toPlan, p.base = false, clone(sm.val)
	sm.update(t, p, operation.MsgCommand{Op: op})
}

// step runs the holder's pending effect, succeeding or failing.
func (sm *sagaMachine) step(t *rapid.T) {
	i := sm.holder()
	p := &sm.procs[i]
	if _, report := p.pending.(operation.EffectReport); report {
		sm.procs[i] = pbtProc{}
		return
	}
	ok := rapid.Bool().Draw(t, "ok")
	stage, pos, plan := p.model.Stage, p.model.Pos.Get(), p.model.Plan.All()
	if ok {
		switch stage {
		case operation.StageCommit:
			sm.val[plan[pos].Res] = version(plan[pos], p.target)
		case operation.StageCompensate:
			sm.val[plan[pos-1].Res] = before(plan, pos-1, p.target, p.base)
		case operation.StageRecover:
			sm.val[plan[pos-1].Res] = before(plan, pos-1, sm.journal.target, sm.journal.base)
		case operation.StageBegin:
			sm.journal = pbtJournal{open: true, op: p.model.Op, base: clone(p.base), target: p.target}
		case operation.StageIntent:
			sm.journal.intents = append(sm.journal.intents, p.model.Pos)
		case operation.StageClose, operation.StageAbort, operation.StageRecoverClose:
			sm.journal = pbtJournal{}
			if stage == operation.StageClose {
				for _, s := range plan {
					if s.Phase == operation.PhaseCommit {
						sm.lastOk[s.Res] = sm.val[s.Res]
					}
				}
			}
		case operation.StageIdle, operation.StagePrepare, operation.StageFinalize, operation.StageDone,
			operation.StageFailed, operation.StageRecovered, operation.StageBroken:
		}
	}
	status := operation.ResultOk
	if !ok {
		status = operation.ResultErr
	}
	sm.update(t, p, operation.MsgResult{Status: status, Reason: domain.NewText("injected")})
	if p.recovering && p.model.Stage == operation.StageRecovered {
		*p = pbtProc{locked: true, toPlan: true, target: p.target, model: operation.Init(guestName())}
	}
}

func (sm *sagaMachine) crash(t *rapid.T) {
	sm.procs[sm.holder()] = pbtProc{}
}

// check is Saga.tla's Consistent.
func (sm *sagaMachine) check(t *rapid.T) {
	if sm.holder() >= 0 || sm.journal.open {
		return
	}
	for r, v := range sm.val {
		if sm.lastOk[r] != v {
			t.Fatalf("resource %+v is at %d, but the last successful command left it at %d (P1)", r, v, sm.lastOk[r])
		}
	}
}

func guestName() domain.GuestName {
	n, _ := domain.ParseGuestName("e2e-a")
	return n
}

func TestSagaConsistentUnderRandomFaultsAndCrashes(t *testing.T) {
	var ops []operation.Op
	for _, op := range corpus(t) {
		if _, ok := expected(op); ok {
			ops = append(ops, op)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		sm := &sagaMachine{
			ops:    ops,
			procs:  make([]pbtProc, rapid.IntRange(2, 4).Draw(t, "procs")),
			val:    map[operation.Resource]int{},
			lastOk: map[operation.Resource]int{},
		}
		// Only the lock holder acts (the others wait for the lock or are
		// idle), so the enabled actions are: take the lock, or the holder
		// plans, runs its next effect, or crashes (rarely).
		for range rapid.IntRange(50, 400).Draw(t, "length") {
			h := sm.holder()
			switch {
			case h < 0:
				sm.acquire(t)
			case rapid.IntRange(0, 9).Draw(t, "crash?") == 0:
				sm.crash(t)
			case sm.procs[h].toPlan:
				sm.plan(t)
			default:
				sm.step(t)
			}
			sm.check(t)
		}
	})
}
