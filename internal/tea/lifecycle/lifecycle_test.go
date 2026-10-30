package lifecycle_test

import (
	"fmt"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/lifecycle"
)

func mustGuestName(t *testing.T, s string) domain.GuestName {
	t.Helper()
	g, err := domain.ParseGuestName(s)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func mustStorePath(t *testing.T, s string) domain.StorePath {
	t.Helper()
	p, err := domain.ParseStorePath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustAbsPath(t *testing.T, s string) domain.AbsPath {
	t.Helper()
	p, err := domain.ParseAbsPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustShareTag(t *testing.T, s string) domain.ShareTag {
	t.Helper()
	tag, err := domain.ParseShareTag(s)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func TestExhaustiveStateSpaceAndIncorrectnessReachability(t *testing.T) {
	name := mustGuestName(t, "dev")
	sysA := mustStorePath(t, "/nix/store/0000000000000000000000000000000a-nixos-system")
	sysB := mustStorePath(t, "/nix/store/0000000000000000000000000000000b-nixos-system")
	k1 := domain.NewKernelSig("kernel-v1")
	k2 := domain.NewKernelSig("kernel-v2")
	shareA := mustAbsPath(t, "/home/alice/proj-a")
	tagA := mustShareTag(t, "s-001122334455")
	shareB := mustAbsPath(t, "/home/alice/proj-b")
	tagB := mustShareTag(t, "s-66778899aabb")

	initModel := lifecycle.Init(name)
	if err := lifecycle.Invariant(initModel); err != nil {
		t.Fatalf("Init invariant failed: %v", err)
	}

	alphabet := []lifecycle.Msg{
		lifecycle.MsgApplyBuilt{System: sysA, KernelSig: k1},
		lifecycle.MsgApplyBuilt{System: sysB, KernelSig: k1},
		lifecycle.MsgApplyBuilt{System: sysB, KernelSig: k2},
		lifecycle.MsgStart{KernelSig: k1},
		lifecycle.MsgSwitchSucceeded{KernelSig: k1},
		lifecycle.MsgSwitchSucceeded{KernelSig: k2},
		lifecycle.MsgSwitchFailed{Reason: domain.NewText("unit failed")},
		lifecycle.MsgMountShare{Path: shareB, Tag: tagB, ReadOnly: false},
		lifecycle.MsgMountShare{Path: shareA, Tag: tagA, ReadOnly: true},
		lifecycle.MsgUnmountShare{Path: shareA},
		lifecycle.MsgUnmountShare{Path: shareB},
		lifecycle.MsgStop{},
	}

	stateKey := func(m lifecycle.Model) string {
		return fmt.Sprintf("%#v", m)
	}

	visited := map[string]bool{stateKey(initModel): true}
	frontier := []lifecycle.Model{initModel}
	transitions := 0
	diameter := 0

	// Incorrectness Logic (O'Hearn POPL 2020) under-approximate reachability witnesses:
	var (
		reachedAssignInstance bool
		reachedInstallStore   bool
		reachedAgentSwitch    bool
		reachedSwitchActivate bool
		reachedSwitchRollback bool
		reachedWriteBoot      bool
		reachedStartVM        bool
		reachedStopVM         bool
		reachedHotplugShare   bool
		reachedHotunplugShare bool
		reachedLog            bool
	)

	for len(frontier) > 0 {
		var nextFrontier []lifecycle.Model
		for _, s := range frontier {
			for _, msg := range alphabet {
				transitions++
				next, effs := lifecycle.Update(s, msg)

				// 1. Hoare Logic Over-Approximate Safety:
				if err := lifecycle.Invariant(next); err != nil {
					t.Fatalf("Invariant failed on %T: %v", msg, err)
				}
				// 2. Separation Logic Frame Rule:
				if err := lifecycle.FrameValid(s, msg, next); err != nil {
					t.Fatalf("FrameValid failed on %T: %v", msg, err)
				}
				// 3. IronFleet / TLA+ Two-State Transition Predicate:
				if err := lifecycle.StepValid(s, msg, next, effs); err != nil {
					t.Fatalf("StepValid failed on %T: %v", msg, err)
				}

				for _, eff := range effs {
					switch e := eff.(type) {
					case lifecycle.EffectAssignInstance:
						reachedAssignInstance = true
					case lifecycle.EffectInstallStore:
						reachedInstallStore = true
					case lifecycle.EffectAgentSwitch:
						reachedAgentSwitch = true
						if e.Mode == lifecycle.SwitchActivate {
							reachedSwitchActivate = true
						} else if e.Mode == lifecycle.SwitchRollback {
							reachedSwitchRollback = true
						}
					case lifecycle.EffectWriteBoot:
						reachedWriteBoot = true
					case lifecycle.EffectStartVM:
						reachedStartVM = true
					case lifecycle.EffectStopVM:
						reachedStopVM = true
					case lifecycle.EffectHotplugShare:
						reachedHotplugShare = true
					case lifecycle.EffectHotunplugShare:
						reachedHotunplugShare = true
					case lifecycle.EffectLog:
						reachedLog = true
					}
				}

				k := stateKey(next)
				if !visited[k] {
					visited[k] = true
					nextFrontier = append(nextFrontier, next)
				}
			}
		}
		if len(nextFrontier) > 0 {
			diameter++
		}
		frontier = nextFrontier
	}

	const maxAllowedStates = 512
	t.Logf("lifecycle reachable state space: states=%d (bound=%d), transitions=%d, diameter=%d",
		len(visited), maxAllowedStates, transitions, diameter)
	if len(visited) > maxAllowedStates {
		t.Fatalf("state-space explosion gate: reachable states %d > bound %d", len(visited), maxAllowedStates)
	}

	// Incorrectness Logic: every postcondition / effect branch must be reachable.
	if !reachedAssignInstance || !reachedInstallStore || !reachedAgentSwitch ||
		!reachedSwitchActivate || !reachedSwitchRollback || !reachedWriteBoot ||
		!reachedStartVM || !reachedStopVM || !reachedHotplugShare ||
		!reachedHotunplugShare || !reachedLog {
		t.Fatalf("Incorrectness Logic reachability failure: assign=%v install=%v switch=%v (act=%v roll=%v) writeBoot=%v start=%v stop=%v hotplug=%v hotunplug=%v log=%v",
			reachedAssignInstance, reachedInstallStore, reachedAgentSwitch,
			reachedSwitchActivate, reachedSwitchRollback, reachedWriteBoot,
			reachedStartVM, reachedStopVM, reachedHotplugShare, reachedHotunplugShare, reachedLog)
	}
}
