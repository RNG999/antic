// Package operation implements the pure Sans-I/O & TEA core that runs one
// host-side antic command on one guest as a saga: a Prepare phase of checks
// that change nothing, a Commit phase whose every step records its inverse,
// a Compensate phase that undoes committed steps in reverse when one fails,
// and a Finalize phase for irreversible clean-up after every commit succeeded
// (docs/design/e2e-spec.md §11).
//
// The core reasons about the resources a command touches (config file,
// instance, system profile, running system, power state, hot-plugged
// mounts) and the version each is at relative to the command: Old (before),
// New (after) or Mid (a VM that a restart has stopped). The imperative shell
// owns the concrete old and new artifacts and executes one effect at a time,
// feeding the result back as MsgResult. A finished saga leaves every
// resource at New (success) or at Old (failure, P1); a compensation that
// fails is reported as Broken.
//
// All fields in Model, Msg, and Effect use formal type-theory combinators from
// internal/types and domain vocabulary types from internal/domain (SSOT):
//   - Refinement Types (domain.GuestName, domain.StepIndex, domain.AbsPath)
//   - Phantom Types (domain.Text)
//   - Canonical Quotient Containers (types.SortedSet for the plan and path sets)
//   - SSOT state normalization (resource versions are derived from Plan and Pos).
package operation

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// GuestPhase is the guest's lifecycle phase when the command starts.
type GuestPhase uint8

const (
	// GuestAbsent means no config file exists (only "guest create" applies).
	GuestAbsent GuestPhase = iota
	// GuestUncreated means the config exists but the guest was never built.
	GuestUncreated
	// GuestOff means the guest is built but not running.
	GuestOff
	// GuestRunning means the guest is running.
	GuestRunning
)

// OpKind is the shape of a command.
type OpKind uint8

const (
	// OpChangeConfig replaces the config and makes it take effect
	// (apply, install, uninstall, set, forward, unforward, mount, unmount, edit).
	OpChangeConfig OpKind = iota
	// OpCreate builds and boots a guest ("guest create").
	OpCreate
	// OpStart boots a stopped guest ("guest start", and "run" before it executes).
	OpStart
	// OpStop shuts a running guest down.
	OpStop
	// OpRestart reboots a guest with its current config.
	OpRestart
	// OpRemove deletes a guest ("guest rm").
	OpRemove
)

// ChangeKind says whether one aspect of the config changes.
type ChangeKind uint8

const (
	// Unchanged means the aspect stays as it is.
	Unchanged ChangeKind = iota
	// Changed means the aspect changes.
	Changed
)

// PathSet is a canonical set of share paths.
type PathSet = types.SortedSet[domain.AbsPath, domain.AbsPath, domain.AbsPathOrder]

// Change describes what a new config changes, as the shell's diff of the
// old and new config files found it.
type Change struct {
	// Config is Changed when the config file is rewritten.
	Config ChangeKind
	// System is Changed when the guest's system must be (re)built and
	// installed: its packages changed, or it has never been built.
	System ChangeKind
	// Restart is Changed when a running guest must reboot to take the change
	// (resources, network policy, port forwards, or shares it cannot change live).
	Restart ChangeKind
	// Added are shares a running guest must hot-plug.
	Added PathSet
	// Removed are hot-plugged shares a running guest must detach.
	Removed PathSet
}

// Op is one command on one guest, with the facts the shell observed.
type Op struct {
	Kind   OpKind
	Phase  GuestPhase
	Change Change
	// Runtime are the guest's hot-plugged shares; undoing a stop reboots the
	// old config and hot-plugs them again.
	Runtime PathSet
}

// StepPhase is the saga phase a step belongs to.
type StepPhase uint8

const (
	// PhasePrepare steps check or build without changing anything observable.
	PhasePrepare StepPhase = iota
	// PhaseCommit steps change one resource and have an inverse.
	PhaseCommit
	// PhaseFinalize steps clean up irreversibly once every commit succeeded.
	PhaseFinalize
)

// Action is what a step does.
type Action uint8

const (
	// ActValidate checks the new config (parse, policy, overlaps, migration rules).
	ActValidate Action = iota
	// ActBuild builds the new system.
	ActBuild
	// ActCheckPorts checks that the forwarded host ports are free.
	ActCheckPorts
	// ActWriteConfig writes the new config file.
	ActWriteConfig
	// ActAssignInstance allocates the guest's instance (ID and state directory).
	ActAssignInstance
	// ActSetProfile points the guest store's system profile at the new system.
	ActSetProfile
	// ActSwitch switches the running guest to the new system.
	ActSwitch
	// ActStopVM shuts the VM down.
	ActStopVM
	// ActStartVM boots the VM with the new config.
	ActStartVM
	// ActHotplug hot-plugs one share.
	ActHotplug
	// ActHotunplug detaches one hot-plugged share.
	ActHotunplug
	// ActTrashConfig moves the config file to the trash.
	ActTrashConfig
	// ActTrashInstance moves the instance to the trash.
	ActTrashInstance
	// ActGC collects the guest store's garbage.
	ActGC
	// ActEmptyTrash deletes the trash.
	ActEmptyTrash
)

// ResourceKind is a resource a commit step changes.
type ResourceKind uint8

const (
	// ResConfig is the guest's config file.
	ResConfig ResourceKind = iota
	// ResInstance is the guest's instance (ID and state directory).
	ResInstance
	// ResProfile is the guest store's system profile (the installed system).
	ResProfile
	// ResSystem is the system the running VM runs.
	ResSystem
	// ResPower is whether the VM runs and with which config.
	ResPower
	// ResMount is one hot-plugged share (Resource.Path names it).
	ResMount
)

// Version is where a resource stands relative to the command.
type Version uint8

const (
	// VersionOld is the resource as it was before the command.
	VersionOld Version = iota
	// VersionMid is a VM that a restart has stopped but not yet booted.
	VersionMid
	// VersionNew is the resource as the command leaves it.
	VersionNew
)

// Resource identifies one resource.
type Resource struct {
	Kind ResourceKind
	Path types.Option[domain.AbsPath]
}

// Step is one planned step.
type Step struct {
	Index  domain.StepIndex
	Phase  StepPhase
	Action Action
	// Res and To say which resource a commit step moves to which version.
	Res Resource
	To  Version
}

// StepByIndex orders steps by their position in the plan.
type StepByIndex struct{}

// Key returns the step's position.
func (StepByIndex) Key(s Step) domain.StepIndex { return s.Index }

// Less orders positions numerically.
func (StepByIndex) Less(a, b domain.StepIndex) bool { return a.Get() < b.Get() }

// StepSet is a canonical plan, keyed and ordered by position.
type StepSet = types.SortedSet[Step, domain.StepIndex, StepByIndex]

// Stage is where the saga is.
type Stage uint8

const (
	// StageIdle waits for MsgCommand.
	StageIdle Stage = iota
	// StagePrepare runs the Prepare step at Pos.
	StagePrepare
	// StageCommit runs the Commit step at Pos.
	StageCommit
	// StageCompensate undoes the committed step at Pos-1.
	StageCompensate
	// StageFinalize runs the Finalize step at Pos.
	StageFinalize
	// StageDone means the command succeeded: every resource is at New.
	StageDone
	// StageFailed means the command failed: every resource is at Old.
	StageFailed
	// StageBroken means a compensation failed: resources are mixed (a P1 violation).
	StageBroken
)

// Model is the complete, immutable, normalized (SSOT) state of one saga.
// Resource versions are derived from Plan and Pos (Versions).
type Model struct {
	Guest  domain.GuestName
	Op     Op
	Plan   StepSet
	Pos    domain.StepIndex
	Stage  Stage
	Reason domain.Text
}

// Msg is the closed sum type of inputs to the saga.
type Msg interface {
	isMsg()
}

// MsgCommand starts the saga for op.
type MsgCommand struct {
	Op Op
}

func (MsgCommand) isMsg() {}

// ResultStatus is the outcome of one effect.
type ResultStatus uint8

const (
	// ResultOk means the effect succeeded.
	ResultOk ResultStatus = iota
	// ResultErr means the effect failed and changed nothing.
	ResultErr
)

// MsgResult reports the outcome of the last effect. Compound effects
// (StartVM, StopVM, Hotplug, Hotunplug, Switch) must be atomic in the shell:
// on ResultErr they changed nothing.
type MsgResult struct {
	Status ResultStatus
	Reason domain.Text
}

func (MsgResult) isMsg() {}

// Effect is the closed sum type of side effects the shell executes.
type Effect interface {
	isEffect()
}

// EffectValidate checks the new config.
type EffectValidate struct{}

// EffectBuild builds the new system.
type EffectBuild struct{}

// EffectCheckPorts checks that the new config's forwarded host ports are free.
type EffectCheckPorts struct{}

// EffectWriteConfig writes the config file at version To (Old may mean "absent").
type EffectWriteConfig struct{ To Version }

// EffectAssignInstance allocates the guest's instance.
type EffectAssignInstance struct{}

// EffectRemoveInstance deletes an instance EffectAssignInstance allocated.
type EffectRemoveInstance struct{}

// EffectSetProfile points the system profile at the system of version To.
type EffectSetProfile struct{ To Version }

// EffectSwitch switches the running guest to the system of version To and
// records it as booted.
type EffectSwitch struct{ To Version }

// EffectStartVM boots the VM with the config and the system of version
// Config, then hot-plugs Rehotplug (restoring a guest a compensated stop shut
// down). It names the system explicitly rather than booting the profile: a
// compensation reboots the old system before the profile is restored.
type EffectStartVM struct {
	Config    Version
	Rehotplug PathSet
}

// EffectStopVM shuts the VM down.
type EffectStopVM struct{}

// EffectHotplug hot-plugs the share at Path.
type EffectHotplug struct{ Path domain.AbsPath }

// EffectHotunplug detaches the hot-plugged share at Path.
type EffectHotunplug struct{ Path domain.AbsPath }

// TrashTarget is what EffectTrash moves.
type TrashTarget uint8

const (
	// TrashConfig is the config file.
	TrashConfig TrashTarget = iota
	// TrashInstance is the instance.
	TrashInstance
)

// EffectTrash moves What to the trash.
type EffectTrash struct{ What TrashTarget }

// EffectRestore moves What back from the trash.
type EffectRestore struct{ What TrashTarget }

// EffectGC collects the guest store's garbage.
type EffectGC struct{}

// EffectEmptyTrash deletes the trash.
type EffectEmptyTrash struct{}

// ReportStatus is how a saga ended.
type ReportStatus uint8

const (
	// ReportOk means the command succeeded.
	ReportOk ReportStatus = iota
	// ReportFailed means the command failed and changed nothing.
	ReportFailed
	// ReportBroken means a compensation failed.
	ReportBroken
)

// EffectReport ends the saga: the shell exits 0 for ReportOk and 125 otherwise.
type EffectReport struct {
	Status ReportStatus
	Reason domain.Text
}

func (EffectValidate) isEffect()       {}
func (EffectBuild) isEffect()          {}
func (EffectCheckPorts) isEffect()     {}
func (EffectWriteConfig) isEffect()    {}
func (EffectAssignInstance) isEffect() {}
func (EffectRemoveInstance) isEffect() {}
func (EffectSetProfile) isEffect()     {}
func (EffectSwitch) isEffect()         {}
func (EffectStartVM) isEffect()        {}
func (EffectStopVM) isEffect()         {}
func (EffectHotplug) isEffect()        {}
func (EffectHotunplug) isEffect()      {}
func (EffectTrash) isEffect()          {}
func (EffectRestore) isEffect()        {}
func (EffectGC) isEffect()             {}
func (EffectEmptyTrash) isEffect()     {}
func (EffectReport) isEffect()         {}

// Init constructs the idle saga for a validated GuestName.
func Init(name domain.GuestName) Model {
	return Model{Guest: name, Pos: position(0)}
}

// position converts n to a StepIndex; plans never exceed maxSteps.
func position(n int) domain.StepIndex {
	i, _ := domain.ParseStepIndex(n)
	return i
}

// maxSteps bounds a plan (a StepIndex also names the position after it).
const maxSteps = 64

// planner accumulates a plan.
type planner struct {
	steps []Step
}

func (p planner) add(phase StepPhase, act Action, res Resource, to Version) planner {
	p.steps = append(slices.Clone(p.steps), Step{Index: position(len(p.steps)), Phase: phase, Action: act, Res: res, To: to})
	return p
}

func (p planner) prepare(act Action) planner {
	return p.add(PhasePrepare, act, Resource{}, VersionOld)
}

func (p planner) commit(act Action, kind ResourceKind, to Version) planner {
	return p.add(PhaseCommit, act, Resource{Kind: kind}, to)
}

func (p planner) mount(act Action, path domain.AbsPath) planner {
	return p.add(PhaseCommit, act, Resource{Kind: ResMount, Path: types.Some(path)}, VersionNew)
}

func (p planner) finalize(act Action) planner {
	return p.add(PhaseFinalize, act, Resource{}, VersionOld)
}

// plan returns the saga for op, or an error for a command that is rejected
// before any effect (a precondition decidable from op alone).
func plan(op Op) ([]Step, error) {
	var p planner
	running := op.Phase == GuestRunning
	switch op.Kind {
	case OpChangeConfig:
		if op.Phase == GuestAbsent {
			return nil, errors.New("no such guest")
		}
		ch := op.Change
		if ch.Config == Changed {
			p = p.prepare(ActValidate)
		}
		if ch.System == Changed {
			p = p.prepare(ActBuild)
		}
		restart := running && ch.Restart == Changed
		if restart {
			p = p.prepare(ActCheckPorts)
		}
		if ch.Config == Changed {
			p = p.commit(ActWriteConfig, ResConfig, VersionNew)
		}
		if ch.System == Changed {
			if op.Phase == GuestUncreated {
				p = p.commit(ActAssignInstance, ResInstance, VersionNew)
			}
			p = p.commit(ActSetProfile, ResProfile, VersionNew)
		}
		switch {
		case restart:
			p = p.commit(ActStopVM, ResPower, VersionMid).commit(ActStartVM, ResPower, VersionNew)
		case running:
			if ch.System == Changed {
				p = p.commit(ActSwitch, ResSystem, VersionNew)
			}
			for _, path := range ch.Removed.All() {
				p = p.mount(ActHotunplug, path)
			}
			for _, path := range ch.Added.All() {
				p = p.mount(ActHotplug, path)
			}
		}
		if ch.System == Changed {
			p = p.finalize(ActGC)
		}
	case OpCreate:
		if op.Phase != GuestAbsent && op.Phase != GuestUncreated {
			return nil, errors.New("guest already exists")
		}
		if op.Phase == GuestAbsent {
			p = p.prepare(ActValidate)
		}
		p = p.prepare(ActBuild).prepare(ActCheckPorts)
		if op.Phase == GuestAbsent {
			p = p.commit(ActWriteConfig, ResConfig, VersionNew)
		}
		p = p.commit(ActAssignInstance, ResInstance, VersionNew).
			commit(ActSetProfile, ResProfile, VersionNew).
			commit(ActStartVM, ResPower, VersionNew)
	case OpStart:
		switch op.Phase {
		case GuestAbsent:
			return nil, errors.New("no such guest")
		case GuestUncreated:
			return nil, errors.New("guest has not been built yet")
		case GuestOff:
			p = p.prepare(ActCheckPorts).commit(ActStartVM, ResPower, VersionNew)
		case GuestRunning:
		}
	case OpStop:
		if op.Phase == GuestAbsent {
			return nil, errors.New("no such guest")
		}
		if running {
			p = p.commit(ActStopVM, ResPower, VersionNew)
		}
	case OpRestart:
		switch op.Phase {
		case GuestAbsent:
			return nil, errors.New("no such guest")
		case GuestUncreated:
			return nil, errors.New("guest has not been built yet")
		case GuestOff:
			p = p.prepare(ActCheckPorts).commit(ActStartVM, ResPower, VersionNew)
		case GuestRunning:
			p = p.prepare(ActCheckPorts).commit(ActStopVM, ResPower, VersionMid).commit(ActStartVM, ResPower, VersionNew)
		}
	case OpRemove:
		if op.Phase == GuestAbsent {
			return nil, errors.New("no such guest")
		}
		if running {
			p = p.commit(ActStopVM, ResPower, VersionNew)
		}
		p = p.commit(ActTrashConfig, ResConfig, VersionNew)
		if op.Phase != GuestUncreated {
			p = p.commit(ActTrashInstance, ResInstance, VersionNew)
		}
		p = p.finalize(ActEmptyTrash)
	}
	if len(p.steps) >= maxSteps {
		return nil, fmt.Errorf("too many changes in one command (%d steps)", len(p.steps))
	}
	return p.steps, nil
}

// forward returns the effect that performs s.
func forward(s Step) Effect {
	switch s.Action {
	case ActValidate:
		return EffectValidate{}
	case ActBuild:
		return EffectBuild{}
	case ActCheckPorts:
		return EffectCheckPorts{}
	case ActWriteConfig:
		return EffectWriteConfig{To: VersionNew}
	case ActAssignInstance:
		return EffectAssignInstance{}
	case ActSetProfile:
		return EffectSetProfile{To: VersionNew}
	case ActSwitch:
		return EffectSwitch{To: VersionNew}
	case ActStopVM:
		return EffectStopVM{}
	case ActStartVM:
		return EffectStartVM{Config: VersionNew}
	case ActHotplug:
		return EffectHotplug{Path: s.Res.Path.Value()}
	case ActHotunplug:
		return EffectHotunplug{Path: s.Res.Path.Value()}
	case ActTrashConfig:
		return EffectTrash{What: TrashConfig}
	case ActTrashInstance:
		return EffectTrash{What: TrashInstance}
	case ActGC:
		return EffectGC{}
	case ActEmptyTrash:
	}
	return EffectEmptyTrash{}
}

// inverse returns the effect that undoes the committed step s of op.
func inverse(op Op, s Step) Effect {
	switch s.Action {
	case ActWriteConfig:
		return EffectWriteConfig{To: VersionOld}
	case ActAssignInstance:
		return EffectRemoveInstance{}
	case ActSetProfile:
		return EffectSetProfile{To: VersionOld}
	case ActSwitch:
		return EffectSwitch{To: VersionOld}
	case ActStopVM:
		return EffectStartVM{Config: VersionOld, Rehotplug: op.Runtime.Clone()}
	case ActHotplug:
		return EffectHotunplug{Path: s.Res.Path.Value()}
	case ActHotunplug:
		return EffectHotplug{Path: s.Res.Path.Value()}
	case ActTrashConfig:
		return EffectRestore{What: TrashConfig}
	case ActTrashInstance:
		return EffectRestore{What: TrashInstance}
	case ActValidate, ActBuild, ActCheckPorts, ActStartVM, ActGC, ActEmptyTrash:
	}
	return EffectStopVM{}
}

// stageOf returns the stage that runs a step of phase ph.
func stageOf(ph StepPhase) Stage {
	switch ph {
	case PhasePrepare:
		return StagePrepare
	case PhaseCommit:
		return StageCommit
	case PhaseFinalize:
	}
	return StageFinalize
}

// firstCommit is the position of the first commit step (or of the first
// finalize step, or the plan's end, when there is none).
func firstCommit(plan StepSet) int {
	for _, s := range plan.All() {
		if s.Phase != PhasePrepare {
			return s.Index.Get()
		}
	}
	return plan.Len()
}

// Versions derives every committed resource's version from Plan and Pos:
// a resource is at the target version of the last committed step on it.
func (m Model) Versions() map[Resource]Version {
	out := map[Resource]Version{}
	if m.Stage == StageIdle || m.Stage == StagePrepare {
		return out
	}
	for _, s := range m.Plan.All() {
		if s.Phase == PhaseCommit && s.Index.Get() < m.Pos.Get() {
			out[s.Res] = s.To
		}
	}
	return out
}

// advance runs the step at m.Pos, or ends the saga successfully.
func advance(m Model) (Model, []Effect) {
	steps := m.Plan.All()
	if m.Pos.Get() >= len(steps) {
		m.Stage = StageDone
		return m, []Effect{EffectReport{Status: ReportOk}}
	}
	s := steps[m.Pos.Get()]
	m.Stage = stageOf(s.Phase)
	return m, []Effect{forward(s)}
}

// fail ends the saga after nothing (or everything committed was undone).
func fail(m Model) (Model, []Effect) {
	m.Stage = StageFailed
	return m, []Effect{EffectReport{Status: ReportFailed, Reason: m.Reason}}
}

// Update is the pure, total, deterministic Sans-I/O & TEA transition function.
func Update(m Model, msg Msg) (Model, []Effect) {
	next := m
	next.Plan = m.Plan.Clone()
	next.Op.Runtime = m.Op.Runtime.Clone()

	switch ev := msg.(type) {
	case MsgCommand:
		if m.Stage != StageIdle {
			return next, nil
		}
		next.Op = Op{
			Kind:    ev.Op.Kind,
			Phase:   ev.Op.Phase,
			Runtime: ev.Op.Runtime.Clone(),
			Change: Change{
				Config:  ev.Op.Change.Config,
				System:  ev.Op.Change.System,
				Restart: ev.Op.Change.Restart,
				Added:   ev.Op.Change.Added.Clone(),
				Removed: ev.Op.Change.Removed.Clone(),
			},
		}
		steps, err := plan(next.Op)
		if err != nil {
			next.Reason = domain.NewText(err.Error())
			return fail(next)
		}
		next.Plan = types.BuildSet[Step, domain.StepIndex, StepByIndex](steps)
		next.Pos = position(0)
		return advance(next)

	case MsgResult:
		pos := m.Pos.Get()
		switch m.Stage {
		case StagePrepare, StageCommit:
			if ev.Status == ResultOk {
				next.Pos = position(pos + 1)
				return advance(next)
			}
			next.Reason = ev.Reason
			if pos == firstCommit(m.Plan) || m.Stage == StagePrepare {
				return fail(next)
			}
			next.Stage = StageCompensate
			return next, []Effect{inverse(m.Op, m.Plan.All()[pos-1])}
		case StageCompensate:
			if ev.Status == ResultErr {
				next.Stage = StageBroken
				return next, []Effect{EffectReport{Status: ReportBroken, Reason: domain.NewText(fmt.Sprintf("%s; undoing it also failed: %s", m.Reason.Get(), ev.Reason.Get()))}}
			}
			next.Pos = position(pos - 1)
			if pos-1 == firstCommit(m.Plan) {
				return fail(next)
			}
			return next, []Effect{inverse(m.Op, m.Plan.All()[pos-2])}
		case StageFinalize:
			// Finalize steps are irreversible clean-up; a failure changes
			// nothing the user observes, so the saga still succeeds.
			next.Pos = position(pos + 1)
			return advance(next)
		case StageIdle, StageDone, StageFailed, StageBroken:
		}
		return next, nil
	}
	return next, nil
}

// Invariant checks the inductive invariants of Model.
func Invariant(m Model) error {
	if !m.Guest.Valid() || !m.Pos.Valid() {
		return errors.New("invariant violation: invalid guest name or position")
	}
	if !m.Plan.Canonical() || !m.Op.Runtime.Canonical() || !m.Op.Change.Added.Canonical() || !m.Op.Change.Removed.Canonical() {
		return errors.New("invariant violation: sets are not canonical")
	}
	steps := m.Plan.All()
	for i, s := range steps {
		if s.Index.Get() != i {
			return fmt.Errorf("invariant violation: step %d is at position %d", s.Index.Get(), i)
		}
		if i > 0 && s.Phase < steps[i-1].Phase {
			return fmt.Errorf("invariant violation: %d-phase step %d follows a later phase", s.Phase, i)
		}
		if s.Res.Path.IsSome() != (s.Phase == PhaseCommit && s.Res.Kind == ResMount) {
			return fmt.Errorf("invariant violation: step %d names a path if and only if it is a mount step", i)
		}
	}
	pos, n, first := m.Pos.Get(), len(steps), firstCommit(m.Plan)
	if pos > n {
		return fmt.Errorf("invariant violation: position %d beyond plan of %d steps", pos, n)
	}
	at := func(ph StepPhase) bool { return pos < n && steps[pos].Phase == ph }
	switch m.Stage {
	case StageIdle:
		if n != 0 || pos != 0 {
			return errors.New("invariant violation: idle saga has a plan")
		}
	case StagePrepare:
		if !at(PhasePrepare) {
			return errors.New("invariant violation: preparing outside the prepare phase")
		}
	case StageCommit:
		if !at(PhaseCommit) {
			return errors.New("invariant violation: committing outside the commit phase")
		}
	case StageCompensate:
		if pos <= first || pos > n || steps[pos-1].Phase != PhaseCommit {
			return errors.New("invariant violation: compensating without a committed step")
		}
	case StageFinalize:
		if !at(PhaseFinalize) {
			return errors.New("invariant violation: finalizing outside the finalize phase")
		}
	case StageDone:
		if pos != n {
			return errors.New("invariant violation: done before the end of the plan")
		}
	case StageFailed:
		if pos > first {
			return errors.New("invariant violation: failed with committed steps (P1)")
		}
	case StageBroken:
	default:
		return fmt.Errorf("invariant violation: invalid stage %d", m.Stage)
	}
	return nil
}

func sameModel(a, b Model) bool {
	return a.Guest == b.Guest && a.Pos == b.Pos && a.Stage == b.Stage && a.Reason == b.Reason &&
		sameOp(a.Op, b.Op) && slices.Equal(a.Plan.All(), b.Plan.All())
}

func sameOp(a, b Op) bool {
	return a.Kind == b.Kind && a.Phase == b.Phase &&
		a.Change.Config == b.Change.Config && a.Change.System == b.Change.System && a.Change.Restart == b.Change.Restart &&
		slices.Equal(a.Change.Added.All(), b.Change.Added.All()) &&
		slices.Equal(a.Change.Removed.All(), b.Change.Removed.All()) &&
		slices.Equal(a.Runtime.All(), b.Runtime.All())
}

// FrameValid enforces the Separation Logic Frame Rule: the guest never
// changes, a result never changes the command or its plan, and a command
// only starts an idle saga.
func FrameValid(prev Model, msg Msg, next Model) error {
	if prev.Guest != next.Guest {
		return errors.New("frame violation: Guest is immutable")
	}
	switch msg.(type) {
	case MsgResult:
		if !sameOp(prev.Op, next.Op) || !slices.Equal(prev.Plan.All(), next.Plan.All()) {
			return errors.New("frame violation: MsgResult changed the command or its plan")
		}
	case MsgCommand:
		if prev.Stage != StageIdle && !sameModel(prev, next) {
			return errors.New("frame violation: MsgCommand changed a saga that already started")
		}
	}
	return nil
}

func terminal(s Stage) bool { return s == StageDone || s == StageFailed || s == StageBroken }

// reportFor is the report status a terminal stage ends with.
func reportFor(s Stage) ReportStatus {
	switch s {
	case StageFailed:
		return ReportFailed
	case StageBroken:
		return ReportBroken
	case StageIdle, StagePrepare, StageCommit, StageCompensate, StageFinalize, StageDone:
	}
	return ReportOk
}

// StepValid checks the transition relation across one step.
func StepValid(prev Model, msg Msg, next Model, effects []Effect) error {
	if terminal(prev.Stage) {
		if len(effects) != 0 || !sameModel(prev, next) {
			return errors.New("step violation: a finished saga changed")
		}
		return nil
	}
	if len(effects) > 1 {
		return errors.New("step violation: more than one effect per step")
	}
	if len(effects) == 0 {
		return nil
	}
	rep, isReport := effects[0].(EffectReport)
	if terminal(next.Stage) != isReport {
		return errors.New("step violation: a report must end the saga, and only a report")
	}
	if isReport {
		if rep.Status != reportFor(next.Stage) {
			return fmt.Errorf("step violation: stage %d reported status %d", next.Stage, rep.Status)
		}
		if next.Stage == StageFailed && len(next.Versions()) != 0 {
			return errors.New("step violation: failed with committed resources (P1)")
		}
		return nil
	}
	steps := next.Plan.All()
	pos := next.Pos.Get()
	switch next.Stage {
	case StagePrepare, StageCommit, StageFinalize:
		if pos >= len(steps) || !sameEffect(effects[0], forward(steps[pos])) {
			return errors.New("step violation: effect is not the planned step at the position")
		}
	case StageCompensate:
		if pos == 0 || !sameEffect(effects[0], inverse(next.Op, steps[pos-1])) {
			return errors.New("step violation: compensation is not the inverse of the last committed step")
		}
	case StageIdle, StageDone, StageFailed, StageBroken:
		return fmt.Errorf("step violation: effect emitted in stage %d", next.Stage)
	}
	return nil
}

// sameEffect compares effects structurally (EffectStartVM holds a set).
func sameEffect(a, b Effect) bool {
	sa, okA := a.(EffectStartVM)
	sb, okB := b.(EffectStartVM)
	if okA || okB {
		return okA && okB && sa.Config == sb.Config && slices.Equal(sa.Rehotplug.All(), sb.Rehotplug.All())
	}
	return a == b
}
