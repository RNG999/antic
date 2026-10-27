// Package lifecycle implements the pure Sans-I/O & TEA (The Elm Architecture)
// state machine for an antic guest VM's lifecycle, configuration switching, and
// hot-plugged virtiofs shares.
//
// Design references:
//   - Sans-I/O protocol pattern (Benfield): pure state machine isolated from sockets/processes/timers.
//   - Functional Core, Imperative Shell (Bernhardt) & TEA: Update(Model, Msg) -> (Model, []Effect).
//   - IronFleet (Hawblitzel et al., SOSP 2015) & Verdi (Wilcox et al., PLDI 2015):
//     separates verified state-machine transitions (Init, Invariant, StepValid, Update)
//     from the untrusted I/O runtime shim.
package lifecycle

import (
	"errors"
	"fmt"
	"slices"
)

// Phase represents the lifecycle state of a guest VM.
type Phase uint8

const (
	// PhaseUncreated means the guest exists only in configuration (no instance ID or store).
	PhaseUncreated Phase = iota
	// PhaseStopped means the guest has an instance ID and installed system, but the VM is not running.
	PhaseStopped
	// PhaseRunning means the VM is booted and running BootSystem.
	PhaseRunning
	// PhaseSwitching means the running VM is currently switching to CandidateSystem.
	PhaseSwitching
)

func (p Phase) String() string {
	switch p {
	case PhaseUncreated:
		return "-"
	case PhaseStopped:
		return "stopped"
	case PhaseRunning:
		return "running"
	case PhaseSwitching:
		return "switching"
	default:
		return "unknown"
	}
}

// Share describes a configured or mounted host directory share.
type Share struct {
	Path     string
	Tag      string
	ReadOnly bool
	Mounted  bool
	Runtime  bool // true if hot-plugged while running
}

// Model is the complete, immutable state of one guest's lifecycle.
// All fields use decidable value types and sorted slices (never maps, floats,
// or pointers) to guarantee deterministic state transitions and canonical equality.
type Model struct {
	Name            string
	Phase           Phase
	InstalledSystem string
	BootSystem      string
	CandidateSystem string
	BootKernelHash  string
	Shares          []Share // strictly sorted by Path
}

// Init constructs the canonical initial state for a named guest.
func Init(name string) Model {
	return Model{
		Name:  name,
		Phase: PhaseUncreated,
	}
}

// Invariant checks the inductive state invariants I(s) of Model.
// Formal verification and bounded model checking assert:
//  1. Base case:      ∀ name != "": Invariant(Init(name)) == nil
//  2. Inductive step: ∀ s, msg: Invariant(s) == nil => Invariant(Update(s, msg).Model) == nil
func Invariant(m Model) error {
	if m.Name == "" {
		return errors.New("invariant violation: guest Name must not be empty")
	}
	switch m.Phase {
	case PhaseUncreated:
		if m.InstalledSystem != "" || m.BootSystem != "" || m.CandidateSystem != "" {
			return errors.New("invariant violation: uncreated guest cannot have installed, boot, or candidate system")
		}
	case PhaseStopped:
		if m.InstalledSystem == "" {
			return errors.New("invariant violation: stopped guest must have an InstalledSystem")
		}
		if m.BootSystem != "" || m.CandidateSystem != "" {
			return errors.New("invariant violation: stopped guest cannot have active BootSystem or CandidateSystem")
		}
	case PhaseRunning:
		if m.InstalledSystem == "" || m.BootSystem == "" {
			return errors.New("invariant violation: running guest must have InstalledSystem and BootSystem")
		}
		if m.CandidateSystem != "" {
			return errors.New("invariant violation: running guest outside switching phase cannot have CandidateSystem")
		}
	case PhaseSwitching:
		if m.InstalledSystem == "" || m.BootSystem == "" || m.CandidateSystem == "" {
			return errors.New("invariant violation: switching guest must have InstalledSystem, BootSystem, and CandidateSystem")
		}
	default:
		return fmt.Errorf("invariant violation: invalid phase %d", m.Phase)
	}

	for i, s := range m.Shares {
		if s.Path == "" || s.Tag == "" {
			return errors.New("invariant violation: share Path and Tag must not be empty")
		}
		if i > 0 && m.Shares[i-1].Path >= s.Path {
			return fmt.Errorf("invariant violation: shares not strictly sorted at %q", s.Path)
		}
		if m.Phase != PhaseRunning && m.Phase != PhaseSwitching {
			if s.Mounted || s.Runtime {
				return fmt.Errorf("invariant violation: share %q is mounted/runtime while guest is in phase %s", s.Path, m.Phase)
			}
		}
		if s.Runtime && !s.Mounted {
			return fmt.Errorf("invariant violation: runtime hot-plugged share %q must be mounted", s.Path)
		}
	}
	return nil
}

// StepValid checks the IronFleet / TLA+ action invariant Next(prev, msg, next, effects)
// across a single state transition (prev, msg) -> (next, effects).
func StepValid(prev Model, msg Msg, next Model, effects []Effect) error {
	// Guest identity is immutable across all transitions.
	if prev.Name != next.Name {
		return fmt.Errorf("step violation: guest Name changed from %q to %q", prev.Name, next.Name)
	}
	// Once a guest is created (Phase != PhaseUncreated), it never reverts to PhaseUncreated.
	if prev.Phase != PhaseUncreated && next.Phase == PhaseUncreated {
		return errors.New("step violation: created guest reverted to PhaseUncreated")
	}
	// IfMsgSwitchFailed occurred in PhaseSwitching, a rollback EffectAgentSwitch must be emitted
	// and InstalledSystem must equal BootSystem.
	if _, ok := msg.(MsgSwitchFailed); ok && prev.Phase == PhaseSwitching {
		if next.InstalledSystem != prev.BootSystem {
			return errors.New("step violation: failed switch must restore InstalledSystem to BootSystem")
		}
		hasRollback := false
		for _, eff := range effects {
			if sw, ok := eff.(EffectAgentSwitch); ok && sw.Rollback && sw.Toplevel == prev.BootSystem {
				hasRollback = true
			}
		}
		if !hasRollback {
			return errors.New("step violation: failed switch must emit rollback EffectAgentSwitch")
		}
	}
	return nil
}

// Msg is the closed sum type of events/inputs driving the guest state machine.
type Msg interface {
	isMsg()
}

// MsgApplyBuilt reports that a NixOS system closure has been built and requests
// installing it into the guest store and switching to it if running.
type MsgApplyBuilt struct {
	System     string
	KernelHash string
}

func (MsgApplyBuilt) isMsg() {}

// MsgSwitchSucceeded reports that the guest agent succeeded in switching to CandidateSystem.
type MsgSwitchSucceeded struct {
	KernelHash string
}

func (MsgSwitchSucceeded) isMsg() {}

// MsgSwitchFailed reports that switching to CandidateSystem failed in the guest agent.
type MsgSwitchFailed struct {
	Reason string
}

func (MsgSwitchFailed) isMsg() {}

// MsgStart reports that the VM is being booted with InstalledSystem.
type MsgStart struct {
	KernelHash string
}

func (MsgStart) isMsg() {}

// MsgStop reports that the VM is being stopped.
type MsgStop struct{}

func (MsgStop) isMsg() {}

// MsgMountShare requests mounting a share (hot-plugging if running).
type MsgMountShare struct {
	Path     string
	Tag      string
	ReadOnly bool
}

func (MsgMountShare) isMsg() {}

// MsgUnmountShare requests removing a share (hot-unplugging if runtime-mounted).
type MsgUnmountShare struct {
	Path string
}

func (MsgUnmountShare) isMsg() {}

// Effect is the closed sum type of side effects to be executed by the imperative shell.
type Effect interface {
	isEffect()
}

// EffectAssignInstance instructs the runtime to allocate a new guest instance ID.
type EffectAssignInstance struct {
	Guest string
}

func (EffectAssignInstance) isEffect() {}

// EffectInstallStore instructs the runtime to copy System into the guest's Nix store.
type EffectInstallStore struct {
	System string
}

func (EffectInstallStore) isEffect() {}

// EffectAgentSwitch instructs the runtime to ask antic-agent to activate Toplevel.
type EffectAgentSwitch struct {
	Toplevel string
	Rollback bool
}

func (EffectAgentSwitch) isEffect() {}

// EffectWriteBoot instructs the runtime to persist boot.json for the running VM.
type EffectWriteBoot struct {
	System string
}

func (EffectWriteBoot) isEffect() {}

// EffectStartVM instructs the runtime to start the systemd units and boot the VM.
type EffectStartVM struct {
	System string
}

func (EffectStartVM) isEffect() {}

// EffectStopVM instructs the runtime to stop the VM and its helper units.
type EffectStopVM struct{}

func (EffectStopVM) isEffect() {}

// EffectHotplugShare instructs the runtime to start virtiofsd, attach the device
// to cloud-hypervisor, and mount it in the guest.
type EffectHotplugShare struct {
	Path     string
	Tag      string
	ReadOnly bool
}

func (EffectHotplugShare) isEffect() {}

// EffectHotunplugShare instructs the runtime to unmount the share in the guest,
// detach the device from cloud-hypervisor, and stop the virtiofsd unit.
type EffectHotunplugShare struct {
	Path string
	Tag  string
}

func (EffectHotunplugShare) isEffect() {}

// EffectLog emits an informational message to the user.
type EffectLog struct {
	Message string
}

func (EffectLog) isEffect() {}

// Update is the pure, total, deterministic Sans-I/O & TEA transition function:
// (Model, Msg) -> (Model, []Effect).
func Update(m Model, msg Msg) (Model, []Effect) {
	next := cloneModel(m)
	var effects []Effect

	switch ev := msg.(type) {
	case MsgApplyBuilt:
		if ev.System == "" {
			return next, nil
		}
		if next.Phase == PhaseUncreated {
			effects = append(effects, EffectAssignInstance{Guest: next.Name})
			next.Phase = PhaseStopped
		}
		if next.InstalledSystem != ev.System {
			effects = append(effects, EffectInstallStore{System: ev.System})
			next.InstalledSystem = ev.System
		}
		if next.Phase == PhaseRunning && next.BootSystem != ev.System {
			next.Phase = PhaseSwitching
			next.CandidateSystem = ev.System
			effects = append(effects, EffectAgentSwitch{Toplevel: ev.System, Rollback: false})
		}

	case MsgSwitchSucceeded:
		if next.Phase != PhaseSwitching {
			return next, nil
		}
		cand := next.CandidateSystem
		next.CandidateSystem = ""
		next.Phase = PhaseRunning
		if next.BootKernelHash == ev.KernelHash {
			next.BootSystem = cand
			effects = append(effects, EffectWriteBoot{System: cand})
		} else {
			effects = append(effects, EffectLog{
				Message: fmt.Sprintf("the kernel changed; restart the guest to boot it (antic guest restart %s)", next.Name),
			})
		}

	case MsgSwitchFailed:
		if next.Phase != PhaseSwitching {
			return next, nil
		}
		old := next.BootSystem
		next.InstalledSystem = old
		next.CandidateSystem = ""
		next.Phase = PhaseRunning
		effects = append(effects, EffectAgentSwitch{Toplevel: old, Rollback: true})

	case MsgStart:
		if next.Phase != PhaseStopped || next.InstalledSystem == "" {
			return next, nil
		}
		next.Phase = PhaseRunning
		next.BootSystem = next.InstalledSystem
		next.BootKernelHash = ev.KernelHash
		for i := range next.Shares {
			next.Shares[i].Mounted = true
			next.Shares[i].Runtime = false
		}
		effects = append(effects,
			EffectStartVM{System: next.BootSystem},
			EffectWriteBoot{System: next.BootSystem},
		)

	case MsgStop:
		if next.Phase != PhaseRunning && next.Phase != PhaseSwitching {
			return next, nil
		}
		next.Phase = PhaseStopped
		next.BootSystem = ""
		next.CandidateSystem = ""
		next.BootKernelHash = ""
		var kept []Share
		for _, s := range next.Shares {
			s.Mounted = false
			s.Runtime = false
			kept = append(kept, s)
		}
		next.Shares = kept
		effects = append(effects, EffectStopVM{})

	case MsgMountShare:
		if ev.Path == "" || ev.Tag == "" {
			return next, nil
		}
		idx := findShare(next.Shares, ev.Path)
		running := next.Phase == PhaseRunning || next.Phase == PhaseSwitching
		if idx >= 0 {
			next.Shares[idx].ReadOnly = ev.ReadOnly
			if running && !next.Shares[idx].Mounted {
				next.Shares[idx].Mounted = true
				next.Shares[idx].Runtime = true
				effects = append(effects, EffectHotplugShare{Path: ev.Path, Tag: ev.Tag, ReadOnly: ev.ReadOnly})
			}
			return next, effects
		}
		sh := Share{
			Path:     ev.Path,
			Tag:      ev.Tag,
			ReadOnly: ev.ReadOnly,
			Mounted:  running,
			Runtime:  running,
		}
		next.Shares = insertShareSorted(next.Shares, sh)
		if running {
			effects = append(effects, EffectHotplugShare{Path: ev.Path, Tag: ev.Tag, ReadOnly: ev.ReadOnly})
		}

	case MsgUnmountShare:
		idx := findShare(next.Shares, ev.Path)
		if idx < 0 {
			return next, nil
		}
		sh := next.Shares[idx]
		next.Shares = slices.Delete(next.Shares, idx, idx+1)
		if (next.Phase == PhaseRunning || next.Phase == PhaseSwitching) && sh.Mounted && sh.Runtime {
			effects = append(effects, EffectHotunplugShare{Path: sh.Path, Tag: sh.Tag})
		}
	}

	return next, effects
}

func cloneModel(m Model) Model {
	out := m
	if len(m.Shares) > 0 {
		out.Shares = slices.Clone(m.Shares)
	}
	return out
}

func findShare(shares []Share, path string) int {
	for i, s := range shares {
		if s.Path == path {
			return i
		}
	}
	return -1
}

func insertShareSorted(shares []Share, sh Share) []Share {
	out := make([]Share, 0, len(shares)+1)
	inserted := false
	for _, s := range shares {
		if !inserted && sh.Path < s.Path {
			out = append(out, sh)
			inserted = true
		}
		out = append(out, s)
	}
	if !inserted {
		out = append(out, sh)
	}
	return out
}
