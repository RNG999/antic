// Package lifecycle implements the pure Sans-I/O & TEA (The Elm Architecture)
// state machine for an antic guest VM's lifecycle, configuration switching, and
// hot-plugged virtiofs shares.
//
// All fields in Model, Msg, and Effect use formal type-theory combinators from
// internal/typetheory and domain vocabulary types from internal/domain (SSOT):
//   - Refinement Types (domain.GuestName, domain.StorePath, domain.AbsPath, domain.ShareTag)
//   - Typestate-Indexed Types (domain.InstalledStorePath, domain.CandidateStorePath)
//   - Phantom Types (domain.KernelSig, domain.Text)
//   - Canonical Quotient Containers (types.SortedSet[Share, domain.AbsPath, ShareByPath])
package lifecycle

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// Phase is the derived lifecycle state of a guest VM.
type Phase uint8

const (
	// PhaseUncreated means the guest exists only in configuration (no installed system).
	PhaseUncreated Phase = iota
	// PhaseStopped means the guest has an installed system, but the VM is not booted.
	PhaseStopped
	// PhaseRunning means the VM is booted and running Boot.System.
	PhaseRunning
	// PhaseSwitching means the running VM is currently switching to Candidate.
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

// MountState represents the single source of truth for a share's mount status,
// eliminating the illegal state combination (Mounted=false, Runtime=true).
type MountState uint8

const (
	// MountNone means the share is configured on disk but not currently mounted.
	MountNone MountState = iota
	// MountBoot means the share was mounted when the VM booted.
	MountBoot
	// MountRuntime means the share was hot-plugged while the VM was running.
	MountRuntime
)

// BootedVM holds the active boot state of a running VM.
type BootedVM struct {
	System    domain.StorePath
	KernelSig domain.KernelSig
}

// Share describes a configured or mounted host directory share using refined types
// and a normalized MountState enum (SSOT).
type Share struct {
	Path     domain.AbsPath
	Tag      domain.ShareTag
	Mount    MountState
	ReadOnly bool
}

// Mounted reports whether the share is currently mounted in the guest.
func (s Share) Mounted() bool {
	return s.Mount == MountBoot || s.Mount == MountRuntime
}

// ShareByPath defines the canonical total order on Share by its unique AbsPath key.
type ShareByPath struct{}

// Key extracts the unique AbsPath key of a Share.
func (ShareByPath) Key(s Share) domain.AbsPath { return s.Path }

// Less orders two AbsPath keys lexicographically.
func (ShareByPath) Less(a, b domain.AbsPath) bool { return a.Get() < b.Get() }

// ShareSet is a canonical SortedSet of Shares keyed and strictly ordered by Path.
type ShareSet = types.SortedSet[Share, domain.AbsPath, ShareByPath]

// Model is the complete, immutable, normalized (SSOT) state of one guest's lifecycle.
// Phase is derived from (Installed, Boot, Candidate) rather than stored redundantly,
// and Installed / Candidate carry distinct Typestate indices (InstalledState vs CandidateState).
type Model struct {
	Name      domain.GuestName
	Installed types.Option[domain.InstalledStorePath]
	Boot      types.Option[BootedVM]
	Candidate types.Option[domain.CandidateStorePath]
	Shares    ShareSet
}

// Phase derives the guest's current lifecycle Phase from the normalized Model state (SSOT).
func (m Model) Phase() Phase {
	if !m.Installed.IsSome() {
		return PhaseUncreated
	}
	if !m.Boot.IsSome() {
		return PhaseStopped
	}
	if !m.Candidate.IsSome() {
		return PhaseRunning
	}
	return PhaseSwitching
}

// Init constructs the canonical initial state for a validated GuestName.
func Init(name domain.GuestName) Model {
	return Model{
		Name: name,
	}
}

// Invariant checks the inductive state invariants I(s) of Model, including all
// refinement predicates, typestate consistency, and canonical SortedSet invariants.
func Invariant(m Model) error {
	if !m.Name.Valid() {
		return errors.New("invariant violation: guest Name is not a valid GuestName")
	}
	if inst, ok := m.Installed.Get(); ok {
		if !inst.Get().Valid() {
			return errors.New("invariant violation: Installed system is not a valid StorePath")
		}
	} else {
		if m.Boot.IsSome() || m.Candidate.IsSome() {
			return errors.New("invariant violation: uncreated guest cannot have Boot or Candidate state")
		}
	}
	if bt, ok := m.Boot.Get(); ok {
		if !bt.System.Valid() {
			return errors.New("invariant violation: Boot.System is not a valid StorePath")
		}
	} else if m.Candidate.IsSome() {
		return errors.New("invariant violation: stopped guest cannot have Candidate system")
	}
	if cand, ok := m.Candidate.Get(); ok {
		if !cand.Get().Valid() {
			return errors.New("invariant violation: Candidate is not a valid StorePath")
		}
	}
	if !m.Shares.Canonical() {
		return errors.New("invariant violation: Shares SortedSet is not in canonical order")
	}

	phase := m.Phase()
	allShares := m.Shares.All()
	for i, s := range allShares {
		if !s.Path.Valid() || !s.Tag.Valid() {
			return errors.New("invariant violation: share Path or Tag does not satisfy refinement predicate")
		}
		if s.Mount > MountRuntime {
			return fmt.Errorf("invariant violation: invalid MountState %d", s.Mount)
		}
		if phase != PhaseRunning && phase != PhaseSwitching && s.Mounted() {
			return fmt.Errorf("invariant violation: share %q is mounted while guest is in phase %s", s.Path.Get(), phase)
		}
		// Separation Logic (Separating Conjunction S_i * S_j): distinct shares
		// must have disjoint path subtrees and distinct virtiofs tags.
		for _, other := range allShares[i+1:] {
			if domain.PathsOverlap(s.Path, other.Path) {
				return fmt.Errorf("separation violation: shares %q and %q overlap", s.Path, other.Path)
			}
			if s.Tag == other.Tag {
				return fmt.Errorf("separation violation: shares %q and %q share tag %q", s.Path, other.Path, s.Tag)
			}
		}
	}
	return nil
}

// FrameValid enforces the Separation Logic Frame Rule ({P} C {Q} => {P * R} C {Q * R}):
// every Msg transition may only modify its designated state footprint, leaving
// the orthogonal frame R untouched.
func FrameValid(prev Model, msg Msg, next Model) error {
	if prev.Name != next.Name {
		return errors.New("frame violation: guest Name is immutable")
	}
	switch msg.(type) {
	case MsgApplyBuilt, MsgSwitchSucceeded, MsgSwitchFailed:
		// Configuration build/switch footprint: (Installed, Boot, Candidate).
		// Frame: Shares must remain unchanged.
		if !slices.Equal(prev.Shares.All(), next.Shares.All()) {
			return fmt.Errorf("frame violation: %T modified Shares frame", msg)
		}
	case MsgStart, MsgStop:
		// VM power footprint: (Boot, Candidate, Share.Mount).
		// Frame: Installed system must remain unchanged.
		if prev.Installed != next.Installed {
			return fmt.Errorf("frame violation: %T modified Installed frame", msg)
		}
	case MsgMountShare, MsgUnmountShare:
		// Share hotplug footprint: (Shares).
		// Frame: (Installed, Boot, Candidate) must remain unchanged.
		if prev.Installed != next.Installed || prev.Boot != next.Boot || prev.Candidate != next.Candidate {
			return fmt.Errorf("frame violation: %T modified VM system/boot frame", msg)
		}
	}
	return nil
}

// StepValid checks the IronFleet / TLA+ action invariant Next(prev, msg, next, effects)
// across a single state transition (prev, msg) -> (next, effects).
func StepValid(prev Model, msg Msg, next Model, effects []Effect) error {
	if prev.Name != next.Name {
		return fmt.Errorf("step violation: guest Name changed from %q to %q", prev.Name, next.Name)
	}
	if prev.Phase() != PhaseUncreated && next.Phase() == PhaseUncreated {
		return errors.New("step violation: created guest reverted to PhaseUncreated")
	}
	if _, ok := msg.(MsgSwitchFailed); ok && prev.Phase() == PhaseSwitching {
		bt := prev.Boot.Value()
		if next.Installed.Value().Get() != bt.System {
			return errors.New("step violation: failed switch must restore Installed to Boot.System")
		}
		hasRollback := false
		for _, eff := range effects {
			if sw, ok := eff.(EffectAgentSwitch); ok && sw.Mode == SwitchRollback && sw.Toplevel == bt.System {
				hasRollback = true
			}
		}
		if !hasRollback {
			return errors.New("step violation: failed switch must emit rollback EffectAgentSwitch")
		}
	}
	return nil
}

// SwitchMode distinguishes normal activation from rollback in EffectAgentSwitch.
type SwitchMode uint8

const (
	// SwitchActivate activates a newly built configuration.
	SwitchActivate SwitchMode = iota
	// SwitchRollback rolls back to the previous working boot configuration.
	SwitchRollback
)

// Msg is the closed sum type of events/inputs driving the guest state machine.
type Msg interface {
	isMsg()
}

// MsgApplyBuilt reports that a NixOS system closure has been built and requests
// installing it into the guest store and switching to it if running.
type MsgApplyBuilt struct {
	System    domain.StorePath
	KernelSig domain.KernelSig
}

func (MsgApplyBuilt) isMsg() {}

// MsgSwitchSucceeded reports that the guest agent succeeded in switching to Candidate.
type MsgSwitchSucceeded struct {
	KernelSig domain.KernelSig
}

func (MsgSwitchSucceeded) isMsg() {}

// MsgSwitchFailed reports that switching to Candidate failed in the guest agent.
type MsgSwitchFailed struct {
	Reason domain.Text
}

func (MsgSwitchFailed) isMsg() {}

// MsgStart reports that the VM is being booted with Installed.
type MsgStart struct {
	KernelSig domain.KernelSig
}

func (MsgStart) isMsg() {}

// MsgStop reports that the VM is being stopped.
type MsgStop struct{}

func (MsgStop) isMsg() {}

// MsgMountShare requests mounting a share (hot-plugging if running).
type MsgMountShare struct {
	Path     domain.AbsPath
	Tag      domain.ShareTag
	ReadOnly bool
}

func (MsgMountShare) isMsg() {}

// MsgUnmountShare requests removing a share (hot-unplugging if runtime-mounted).
type MsgUnmountShare struct {
	Path domain.AbsPath
}

func (MsgUnmountShare) isMsg() {}

// Effect is the closed sum type of side effects to be executed by the imperative shell.
type Effect interface {
	isEffect()
}

// EffectAssignInstance instructs the runtime to allocate a new guest instance ID.
type EffectAssignInstance struct {
	Guest domain.GuestName
}

func (EffectAssignInstance) isEffect() {}

// EffectInstallStore instructs the runtime to copy System into the guest's Nix store.
type EffectInstallStore struct {
	System domain.InstalledStorePath
}

func (EffectInstallStore) isEffect() {}

// EffectAgentSwitch instructs the runtime to ask antic-agent to activate Toplevel.
type EffectAgentSwitch struct {
	Toplevel domain.StorePath
	Mode     SwitchMode
}

func (EffectAgentSwitch) isEffect() {}

// EffectWriteBoot instructs the runtime to persist boot.json for the running VM.
type EffectWriteBoot struct {
	System domain.StorePath
}

func (EffectWriteBoot) isEffect() {}

// EffectStartVM instructs the runtime to start the systemd units and boot the VM.
type EffectStartVM struct {
	System domain.StorePath
}

func (EffectStartVM) isEffect() {}

// EffectStopVM instructs the runtime to stop the VM and its helper units.
type EffectStopVM struct{}

func (EffectStopVM) isEffect() {}

// EffectHotplugShare instructs the runtime to start virtiofsd, attach the device
// to cloud-hypervisor, and mount it in the guest.
type EffectHotplugShare struct {
	Path     domain.AbsPath
	Tag      domain.ShareTag
	ReadOnly bool
}

func (EffectHotplugShare) isEffect() {}

// EffectHotunplugShare instructs the runtime to unmount the share in the guest,
// detach the device from cloud-hypervisor, and stop the virtiofsd unit.
type EffectHotunplugShare struct {
	Path domain.AbsPath
	Tag  domain.ShareTag
}

func (EffectHotunplugShare) isEffect() {}

// EffectLog emits an informational message to the user.
type EffectLog struct {
	Message domain.Text
}

func (EffectLog) isEffect() {}

// Update is the pure, total, deterministic Sans-I/O & TEA transition function:
// (Model, Msg) -> (Model, []Effect).
func Update(m Model, msg Msg) (Model, []Effect) {
	next := m
	next.Shares = m.Shares.Clone()
	var effects []Effect

	switch ev := msg.(type) {
	case MsgApplyBuilt:
		if !ev.System.Valid() {
			return next, nil
		}
		wasUncreated := next.Phase() == PhaseUncreated
		if wasUncreated {
			effects = append(effects, EffectAssignInstance{Guest: next.Name})
		}
		instSys := types.NewIndexed[domain.StorePath, domain.InstalledState](ev.System)
		if cur, ok := next.Installed.Get(); !ok || cur != instSys {
			effects = append(effects, EffectInstallStore{System: instSys})
			next.Installed = types.Some(instSys)
		}
		switch next.Phase() {
		case PhaseStopped:
			effects = append(effects, EffectLog{
				Message: domain.NewText(fmt.Sprintf("%s is ready; it boots on the next \"antic guest start %s\"", ev.System.Get(), next.Name.Get())),
			})
		case PhaseRunning:
			bt := next.Boot.Value()
			if bt.System != ev.System {
				candSys := types.TransitionIndexed[domain.StorePath, domain.InstalledState, domain.CandidateState](instSys)
				next.Candidate = types.Some(candSys)
				effects = append(effects, EffectAgentSwitch{Toplevel: candSys.Get(), Mode: SwitchActivate})
			}
		}

	case MsgSwitchSucceeded:
		if next.Phase() != PhaseSwitching {
			return next, nil
		}
		cand := next.Candidate.Value().Get()
		bt := next.Boot.Value()
		next.Candidate = types.None[domain.CandidateStorePath]()
		if bt.KernelSig == ev.KernelSig {
			next.Boot = types.Some(BootedVM{System: cand, KernelSig: ev.KernelSig})
			effects = append(effects, EffectWriteBoot{System: cand})
		} else {
			effects = append(effects, EffectLog{
				Message: domain.NewText(fmt.Sprintf("the kernel changed; restart the guest to boot it (antic guest restart %s)", next.Name.Get())),
			})
		}

	case MsgSwitchFailed:
		if next.Phase() != PhaseSwitching {
			return next, nil
		}
		bt := next.Boot.Value()
		next.Installed = types.Some(types.NewIndexed[domain.StorePath, domain.InstalledState](bt.System))
		next.Candidate = types.None[domain.CandidateStorePath]()
		effects = append(effects, EffectAgentSwitch{Toplevel: bt.System, Mode: SwitchRollback})

	case MsgStart:
		if next.Phase() != PhaseStopped {
			return next, nil
		}
		inst := next.Installed.Value().Get()
		next.Boot = types.Some(BootedVM{System: inst, KernelSig: ev.KernelSig})
		var updated []Share
		for _, s := range next.Shares.All() {
			s.Mount = MountBoot
			updated = append(updated, s)
		}
		next.Shares = types.BuildSet[Share, domain.AbsPath, ShareByPath](updated)
		effects = append(effects,
			EffectStartVM{System: inst},
			EffectWriteBoot{System: inst},
		)

	case MsgStop:
		if next.Phase() != PhaseRunning && next.Phase() != PhaseSwitching {
			return next, nil
		}
		next.Boot = types.None[BootedVM]()
		next.Candidate = types.None[domain.CandidateStorePath]()
		var updated []Share
		for _, s := range next.Shares.All() {
			s.Mount = MountNone
			updated = append(updated, s)
		}
		next.Shares = types.BuildSet[Share, domain.AbsPath, ShareByPath](updated)
		effects = append(effects, EffectStopVM{})

	case MsgMountShare:
		if !ev.Path.Valid() || !ev.Tag.Valid() {
			return next, nil
		}
		running := next.Phase() == PhaseRunning || next.Phase() == PhaseSwitching
		if existing, ok := next.Shares.Lookup(ev.Path); ok {
			if existing.Tag != ev.Tag {
				return next, nil
			}
			existing.ReadOnly = ev.ReadOnly
			if running && !existing.Mounted() {
				existing.Mount = MountRuntime
				effects = append(effects, EffectHotplugShare{Path: ev.Path, Tag: ev.Tag, ReadOnly: ev.ReadOnly})
			}
			next.Shares = next.Shares.Upsert(existing)
			return next, effects
		}
		for _, other := range next.Shares.All() {
			if domain.PathsOverlap(ev.Path, other.Path) || other.Tag == ev.Tag {
				return next, nil
			}
		}
		mnt := MountNone
		if running {
			mnt = MountRuntime
		}
		next.Shares = next.Shares.Upsert(Share{
			Path:     ev.Path,
			Tag:      ev.Tag,
			Mount:    mnt,
			ReadOnly: ev.ReadOnly,
		})
		if running {
			effects = append(effects, EffectHotplugShare{Path: ev.Path, Tag: ev.Tag, ReadOnly: ev.ReadOnly})
		}

	case MsgUnmountShare:
		if !ev.Path.Valid() {
			return next, nil
		}
		sh, ok := next.Shares.Lookup(ev.Path)
		if !ok {
			return next, nil
		}
		next.Shares = next.Shares.Remove(ev.Path)
		if next.Phase() == PhaseRunning || next.Phase() == PhaseSwitching {
			switch sh.Mount {
			case MountRuntime:
				effects = append(effects, EffectHotunplugShare{Path: sh.Path, Tag: sh.Tag})
			case MountBoot:
				effects = append(effects, EffectLog{
					Message: domain.NewText(fmt.Sprintf("removed %s from %s.toml; restart guest %s to unmount it", sh.Path.Get(), next.Name.Get(), next.Name.Get())),
				})
			}
		}
	}

	return next, effects
}
