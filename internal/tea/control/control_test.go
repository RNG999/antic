package control_test

import (
	"fmt"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/control"
)

func TestExhaustiveStateSpaceAndIncorrectnessReachability(t *testing.T) {
	u, err := domain.ParseUserName("alice")
	if err != nil {
		t.Fatal(err)
	}
	initModel := control.Init(u)
	if err := control.Invariant(initModel); err != nil {
		t.Fatalf("Init invariant failed: %v", err)
	}

	validTagA, _ := domain.ParseShareTag("s-001122334455")
	validPathA, _ := domain.ParseAbsPath("/home/alice/work")
	validFDA, _ := domain.ParseMountFD(7)

	validTagB, _ := domain.ParseShareTag("s-66778899aabb")
	validPathB, _ := domain.ParseAbsPath("/home/alice/docs")
	validFDB, _ := domain.ParseMountFD(8)

	alphabet := []control.Msg{
		// Spoofed guest CID (must always be rejected):
		control.ParseRequest(3, proto.Request{Op: proto.OpHello}, false),
		control.ParseRequest(3, proto.Request{Op: proto.OpMount, Tag: "s-001122334455", Path: "/home/alice/work"}, false),
		// Valid host CID with traversal / invalid inputs:
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpMount, Tag: "s-001122334455", Path: "/etc/shadow"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpMount, Tag: "s-001122334455", Path: "/home/alice"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpMount, Tag: "bad-tag", Path: "/home/alice/work"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpApply, Toplevel: "/nix/store/invalid"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpKill, Session: "sshd.service"}, false),
		// Valid host CID & valid inputs:
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpHello}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpMount, Tag: "s-001122334455", Path: "/home/alice/work", ReadOnly: true}, false),
		control.MsgMountCompleted{Tag: validTagA, Path: validPathA, FD: validFDA, ReadOnly: true},
		control.MsgMountCompleted{Tag: validTagB, Path: validPathB, FD: validFDB, ReadOnly: false},
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpUnmount, Tag: "s-001122334455"}, false),
		control.MsgUnmountCompleted{Tag: validTagA},
		control.MsgUnmountCompleted{Tag: validTagB},
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpApply, Toplevel: "/nix/store/0123456789abcdef0123456789abcdef-nixos-system"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpSessions}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpKill, Session: "antic-session@1.service"}, false),
		control.ParseRequest(proto.HostCID, proto.Request{Op: proto.OpJournal, Units: []string{"sshd.service"}, Lines: 100}, false),
	}

	stateKey := func(m control.Model) string {
		return fmt.Sprintf("%#v", m)
	}

	visited := map[string]bool{stateKey(initModel): true}
	frontier := []control.Model{initModel}
	transitions := 0
	diameter := 0

	// Incorrectness Logic (O'Hearn POPL 2020) under-approximate reachability witnesses:
	var (
		reachedMsgRequest    bool
		reachedMountComp     bool
		reachedUnmountComp   bool
		reachedReject        bool
		reachedReplyHello    bool
		reachedMount         bool
		reachedUnmount       bool
		reachedExecSwitch    bool
		reachedListSessions  bool
		reachedStopSession   bool
		reachedStreamJournal bool
	)

	for len(frontier) > 0 {
		var nextFrontier []control.Model
		for _, s := range frontier {
			for _, msg := range alphabet {
				transitions++
				switch msg.(type) {
				case control.MsgRequest:
					reachedMsgRequest = true
				case control.MsgMountCompleted:
					reachedMountComp = true
				case control.MsgUnmountCompleted:
					reachedUnmountComp = true
				}

				next, effs := control.Update(s, msg)

				// 1. Hoare Logic Over-Approximate Safety:
				if err := control.Invariant(next); err != nil {
					t.Fatalf("Invariant failed on %+v: %v", msg, err)
				}
				// 2. Separation Logic Frame Rule:
				if err := control.FrameValid(s, msg, next); err != nil {
					t.Fatalf("FrameValid failed on %+v: %v", msg, err)
				}
				// 3. IronFleet / TLA+ Two-State Transition Predicate:
				if err := control.StepValid(s, msg, next, effs); err != nil {
					t.Fatalf("StepValid failed on %+v: %v", msg, err)
				}

				for _, eff := range effs {
					switch eff.(type) {
					case control.EffectReject:
						reachedReject = true
					case control.EffectReplyHello:
						reachedReplyHello = true
					case control.EffectMountVirtiofs:
						reachedMount = true
					case control.EffectUnmountVirtiofs:
						reachedUnmount = true
					case control.EffectExecSwitch:
						reachedExecSwitch = true
					case control.EffectListSessions:
						reachedListSessions = true
					case control.EffectStopSession:
						reachedStopSession = true
					case control.EffectStreamJournal:
						reachedStreamJournal = true
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

	const maxAllowedStates = 32
	t.Logf("control reachable state space: states=%d (bound=%d), transitions=%d, diameter=%d",
		len(visited), maxAllowedStates, transitions, diameter)
	if len(visited) > maxAllowedStates {
		t.Fatalf("state-space explosion gate: reachable states %d > bound %d", len(visited), maxAllowedStates)
	}

	// Incorrectness Logic: every Msg and Effect variant must be reachable.
	if !reachedMsgRequest || !reachedMountComp || !reachedUnmountComp {
		t.Fatalf("Incorrectness Logic reachability failure on Msg: req=%v mount=%v unmount=%v",
			reachedMsgRequest, reachedMountComp, reachedUnmountComp)
	}
	if !reachedReject || !reachedReplyHello || !reachedMount || !reachedUnmount ||
		!reachedExecSwitch || !reachedListSessions || !reachedStopSession || !reachedStreamJournal {
		t.Fatalf("Incorrectness Logic reachability failure on Effect: reject=%v hello=%v mount=%v unmount=%v switch=%v sessions=%v stop=%v journal=%v",
			reachedReject, reachedReplyHello, reachedMount, reachedUnmount,
			reachedExecSwitch, reachedListSessions, reachedStopSession, reachedStreamJournal)
	}
}
