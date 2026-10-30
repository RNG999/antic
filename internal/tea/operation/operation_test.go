package operation_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

func mustAbs(t *testing.T, s string) domain.AbsPath {
	t.Helper()
	p, err := domain.ParseAbsPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func paths(ps ...domain.AbsPath) operation.PathSet {
	return types.BuildSet[domain.AbsPath, domain.AbsPath, domain.AbsPathOrder](ps)
}

// Concrete resource states of the shadow host, independent of the core's
// own Version bookkeeping.
type cfgState uint8

const (
	cfgOld cfgState = iota // as before (or absent, for a new guest)
	cfgNew
	cfgTrashed
)

type instState uint8

const (
	instAbsent instState = iota
	instPresent
	instTrashed
)

type powerState uint8

const (
	powerOff powerState = iota
	powerOnOld
	powerOnNew
)

// shadow is the host as the effects leave it.
type shadow struct {
	config  cfgState
	inst    instState
	profile operation.Version // VersionOld or VersionNew
	system  operation.Version // the running system
	power   powerState
	mounts  string // hot-plugged shares
}

func mountKey(ps []domain.AbsPath) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.Get())
	}
	slices.Sort(s)
	return fmt.Sprint(s)
}

func initial(op operation.Op) shadow {
	s := shadow{mounts: mountKey(nil)}
	switch op.Phase {
	case operation.GuestAbsent, operation.GuestUncreated:
	case operation.GuestOff:
		s.inst = instPresent
	case operation.GuestRunning:
		s.inst, s.power, s.mounts = instPresent, powerOnOld, mountKey(op.Runtime.All())
	}
	return s
}

// apply is an independent semantics of each effect on the shadow host.
func apply(s shadow, e operation.Effect, mounts *[]domain.AbsPath) shadow {
	switch e := e.(type) {
	case operation.EffectWriteConfig:
		s.config = cfgOld
		if e.To == operation.VersionNew {
			s.config = cfgNew
		}
	case operation.EffectAssignInstance:
		s.inst = instPresent
	case operation.EffectRemoveInstance:
		s.inst = instAbsent
	case operation.EffectSetProfile:
		s.profile = e.To
	case operation.EffectSwitch:
		s.system = e.To
	case operation.EffectStartVM:
		s.power, s.system = powerOnOld, e.Config
		if e.Config == operation.VersionNew {
			s.power = powerOnNew
		}
		*mounts = e.Rehotplug.All()
	case operation.EffectStopVM:
		s.power = powerOff
		*mounts = nil
	case operation.EffectHotplug:
		*mounts = append(*mounts, e.Path)
	case operation.EffectHotunplug:
		*mounts = slices.DeleteFunc(slices.Clone(*mounts), func(p domain.AbsPath) bool { return p == e.Path })
	case operation.EffectTrash:
		if e.What == operation.TrashConfig {
			s.config = cfgTrashed
		} else {
			s.inst = instTrashed
		}
	case operation.EffectRestore:
		if e.What == operation.TrashConfig {
			s.config = cfgOld
		} else {
			s.inst = instPresent
		}
	case operation.EffectValidate, operation.EffectBuild, operation.EffectCheckPorts,
		operation.EffectGC, operation.EffectEmptyTrash, operation.EffectReport:
	}
	s.mounts = mountKey(*mounts)
	return s
}

// expected is the test's own oracle for a successful command, or ok=false
// for a command that must be rejected before any effect.
func expected(op operation.Op) (shadow, bool) {
	s := initial(op)
	ch := op.Change
	switch op.Kind {
	case operation.OpChangeConfig:
		if op.Phase == operation.GuestAbsent {
			return s, false
		}
		if ch.Config == operation.Changed {
			s.config = cfgNew
		}
		if ch.System == operation.Changed {
			s.inst, s.profile = instPresent, operation.VersionNew
		}
		if op.Phase == operation.GuestRunning {
			if ch.Restart == operation.Changed {
				s.power, s.system, s.mounts = powerOnNew, s.profile, mountKey(nil)
				break
			}
			if ch.System == operation.Changed {
				s.system = operation.VersionNew
			}
			var ms []domain.AbsPath
			for _, p := range op.Runtime.All() {
				if !ch.Removed.Contains(p) {
					ms = append(ms, p)
				}
			}
			s.mounts = mountKey(append(ms, ch.Added.All()...))
		}
	case operation.OpCreate:
		if op.Phase != operation.GuestAbsent && op.Phase != operation.GuestUncreated {
			return s, false
		}
		if op.Phase == operation.GuestAbsent {
			s.config = cfgNew
		}
		s.inst, s.profile, s.power, s.system = instPresent, operation.VersionNew, powerOnNew, operation.VersionNew
	case operation.OpStart:
		switch op.Phase {
		case operation.GuestAbsent, operation.GuestUncreated:
			return s, false
		case operation.GuestOff:
			s.power, s.system = powerOnNew, operation.VersionNew
		case operation.GuestRunning:
		}
	case operation.OpStop:
		if op.Phase == operation.GuestAbsent {
			return s, false
		}
		s.power, s.mounts = powerOff, mountKey(nil)
	case operation.OpRestart:
		if op.Phase == operation.GuestAbsent || op.Phase == operation.GuestUncreated {
			return s, false
		}
		s.power, s.system, s.mounts = powerOnNew, operation.VersionNew, mountKey(nil)
	case operation.OpRemove:
		if op.Phase == operation.GuestAbsent {
			return s, false
		}
		s.config, s.power, s.mounts = cfgTrashed, powerOff, mountKey(nil)
		if s.inst == instPresent {
			s.inst = instTrashed
		}
	}
	return s, true
}

// canon identifies states that are the same host: an unchanged config or
// system has identical old and new artifacts, and a guest without an
// instance has no profile or running system.
func canon(s shadow, op operation.Op) shadow {
	configSame := op.Change.Config == operation.Unchanged || op.Kind != operation.OpChangeConfig
	if op.Kind == operation.OpCreate {
		configSame = op.Phase != operation.GuestAbsent
	}
	systemSame := (op.Kind != operation.OpChangeConfig && op.Kind != operation.OpCreate) ||
		(op.Kind == operation.OpChangeConfig && op.Change.System == operation.Unchanged)
	if configSame && s.config == cfgNew {
		s.config = cfgOld
	}
	if systemSame {
		s.profile, s.system = operation.VersionOld, operation.VersionOld
	}
	if configSame && systemSame && s.power == powerOnNew {
		s.power = powerOnOld
	}
	if s.inst != instPresent {
		s.profile, s.system = operation.VersionOld, operation.VersionOld
	}
	if s.power == powerOff {
		s.system = operation.VersionOld
	}
	return s
}

// corpus enumerates commands over every phase and change combination.
func corpus(t *testing.T) []operation.Op {
	r := mustAbs(t, "/home/u/w/R")
	a := mustAbs(t, "/home/u/w/A")
	var ops []operation.Op
	kinds := []operation.OpKind{operation.OpChangeConfig, operation.OpCreate, operation.OpStart, operation.OpStop, operation.OpRestart, operation.OpRemove}
	phases := []operation.GuestPhase{operation.GuestAbsent, operation.GuestUncreated, operation.GuestOff, operation.GuestRunning}
	flags := []operation.ChangeKind{operation.Unchanged, operation.Changed}
	for _, k := range kinds {
		for _, ph := range phases {
			for _, cfg := range flags {
				for _, sys := range flags {
					for _, rs := range flags {
						for _, runtime := range []operation.PathSet{paths(), paths(r)} {
							for _, removed := range []operation.PathSet{paths(), paths(r)} {
								if (removed.Len() > 0 && runtime.Len() == 0) || (runtime.Len() > 0 && ph != operation.GuestRunning) {
									continue
								}
								for _, added := range []operation.PathSet{paths(), paths(a)} {
									ops = append(ops, operation.Op{
										Kind: k, Phase: ph, Runtime: runtime,
										Change: operation.Change{Config: cfg, System: sys, Restart: rs, Added: added, Removed: removed},
									})
								}
							}
						}
					}
				}
			}
		}
	}
	return ops
}

type run struct {
	status operation.ReportStatus
	final  shadow
	model  operation.Model
	trace  []operation.Effect
}

// simulate runs the saga for op, failing the failFwd-th forward effect and
// the failComp-th compensation (-1: none), and checks every step.
func simulate(t *testing.T, op operation.Op, failFwd, failComp int) run {
	t.Helper()
	name, _ := domain.ParseGuestName("e2e-a")
	m := operation.Init(name)
	if err := operation.Invariant(m); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mounts := op.Runtime.All()
	host := initial(op)
	var msg operation.Msg = operation.MsgCommand{Op: op}
	var r run
	fwd, comp := 0, 0
	for range 200 {
		next, effects := operation.Update(m, msg)
		if err := operation.Invariant(next); err != nil {
			t.Fatalf("%+v: %v", op, err)
		}
		if err := operation.FrameValid(m, msg, next); err != nil {
			t.Fatalf("%+v: %v", op, err)
		}
		if err := operation.StepValid(m, msg, next, effects); err != nil {
			t.Fatalf("%+v: %v", op, err)
		}
		m = next
		if len(effects) != 1 {
			t.Fatalf("%+v: saga stalled in stage %d", op, m.Stage)
		}
		e := effects[0]
		r.trace = append(r.trace, e)
		if rep, ok := e.(operation.EffectReport); ok {
			r.status, r.final, r.model = rep.Status, host, m
			return r
		}
		status := operation.ResultOk
		if m.Stage == operation.StageCompensate {
			if comp == failComp {
				status = operation.ResultErr
			}
			comp++
		} else {
			if fwd == failFwd {
				status = operation.ResultErr
			}
			fwd++
		}
		if status == operation.ResultOk {
			host = apply(host, e, &mounts)
		}
		msg = operation.MsgResult{Status: status, Reason: domain.NewText("injected")}
	}
	t.Fatalf("%+v: saga did not terminate", op)
	return r
}

// TestSagaAtomicityUnderFaultInjection is the L1a check: for every command
// in the corpus and every single forward failure, combined with every
// single compensation failure, the saga ends with the host either fully at
// the new state (success) or fully at the old state (failure, P1), and is
// Broken only when a compensation failed.
func TestSagaAtomicityUnderFaultInjection(t *testing.T) {
	runs := 0
	for _, op := range corpus(t) {
		want, accepted := expected(op)
		clean := simulate(t, op, -1, -1)
		runs++
		if !accepted {
			if clean.status != operation.ReportFailed || len(clean.trace) != 1 {
				t.Fatalf("%+v: must be rejected before any effect, got %v", op, clean.trace)
			}
			continue
		}
		if clean.status != operation.ReportOk || canon(clean.final, op) != canon(want, op) {
			t.Fatalf("%+v: success left %+v, want %+v", op, clean.final, want)
		}
		for res, v := range clean.model.Versions() {
			if v != operation.VersionNew {
				t.Fatalf("%+v: resource %+v ends at version %d after success", op, res, v)
			}
		}
		forwardEffects := len(clean.trace) - 1
		for k := range forwardEffects {
			failed := simulate(t, op, k, -1)
			runs++
			if failed.status == operation.ReportOk {
				// Only a Finalize failure may still succeed.
				if canon(failed.final, op) != canon(want, op) {
					t.Fatalf("%+v: success after finalize failure %d left %+v", op, k, failed.final)
				}
				continue
			}
			if failed.status != operation.ReportFailed || canon(failed.final, op) != canon(initial(op), op) {
				t.Fatalf("%+v: failure at %d ended %d with %+v, want the initial %+v\ntrace %v", op, k, failed.status, failed.final, initial(op), failed.trace)
			}
			if len(failed.model.Versions()) != 0 {
				t.Fatalf("%+v: failure at %d left committed resources", op, k)
			}
			for c := 0; ; c++ {
				broken := simulate(t, op, k, c)
				runs++
				if broken.status != operation.ReportBroken {
					if broken.status != operation.ReportFailed {
						t.Fatalf("%+v: compensation failure %d ended %d", op, c, broken.status)
					}
					break
				}
			}
		}
		// Finalize steps run only after every commit step succeeded.
		seenFinalize := false
		for _, e := range clean.trace {
			switch e.(type) {
			case operation.EffectGC, operation.EffectEmptyTrash:
				seenFinalize = true
			case operation.EffectWriteConfig, operation.EffectSetProfile, operation.EffectStopVM, operation.EffectStartVM,
				operation.EffectSwitch, operation.EffectHotplug, operation.EffectHotunplug, operation.EffectTrash,
				operation.EffectAssignInstance:
				if seenFinalize {
					t.Fatalf("%+v: commit step after finalize: %v", op, clean.trace)
				}
			}
		}
	}
	t.Logf("checked %d saga runs over %d commands", runs, len(corpus(t)))
}

// TestFinishedSagaIgnoresMessages checks that a finished or idle saga does
// not react to stray messages.
func TestFinishedSagaIgnoresMessages(t *testing.T) {
	name, _ := domain.ParseGuestName("e2e-a")
	idle := operation.Init(name)
	next, effects := operation.Update(idle, operation.MsgResult{Status: operation.ResultOk})
	if len(effects) != 0 || next.Stage != operation.StageIdle {
		t.Fatal("an idle saga reacted to a result")
	}
	done, _ := operation.Update(idle, operation.MsgCommand{Op: operation.Op{Kind: operation.OpStop, Phase: operation.GuestOff}})
	if done.Stage != operation.StageDone {
		t.Fatalf("stopping a stopped guest should succeed immediately, got stage %d", done.Stage)
	}
	for _, msg := range []operation.Msg{operation.MsgResult{}, operation.MsgCommand{Op: operation.Op{Kind: operation.OpRemove, Phase: operation.GuestOff}}} {
		after, effects := operation.Update(done, msg)
		if err := operation.StepValid(done, msg, after, effects); err != nil || len(effects) != 0 {
			t.Fatalf("a finished saga reacted to %T: %v", msg, err)
		}
	}
}
