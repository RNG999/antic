// Package control implements the pure Sans-I/O & TEA state machine and
// security reference monitor for guest control operations.
//
// All fields in Model, Msg, and Effect use formal type-theory combinators from
// internal/types and domain vocabulary types from internal/domain (SSOT):
//   - Refinement Types (domain.UserName, domain.ShareTag, domain.AbsPath, domain.StorePath,
//     domain.SessionUnit, domain.UnitName, domain.JournalLines, domain.MountFD)
//   - Phantom Types (domain.PeerCID, domain.Text)
//   - Ghosts of Departed Proofs (domain.HostAuthProof on all privileged effects;
//     domain.WithinHomeProof on EffectMountVirtiofs)
//   - Canonical Quotient Containers (types.SortedSet[MountEntry, domain.ShareTag, MountByTag])
//   - SSOT state normalization (Home directory derived from Model.User).
package control

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/types"
)

// MountEntry records one active virtiofs share mounted by the guest agent.
type MountEntry struct {
	Tag      domain.ShareTag
	Path     domain.AbsPath
	FD       domain.MountFD
	ReadOnly bool
}

// MountByTag defines the canonical total order on MountEntry by its unique ShareTag key.
type MountByTag struct{}

// Key extracts the unique ShareTag of a MountEntry.
func (MountByTag) Key(m MountEntry) domain.ShareTag { return m.Tag }

// Less orders two ShareTags lexicographically.
func (MountByTag) Less(a, b domain.ShareTag) bool { return a.Get() < b.Get() }

// MountSet is a canonical SortedSet of MountEntries keyed and ordered by Tag.
type MountSet = types.SortedSet[MountEntry, domain.ShareTag, MountByTag]

// Model is the immutable, normalized (SSOT) state of the guest control plane.
// The user's home directory (/home/<User>) is derived via Home() rather than
// stored redundantly.
type Model struct {
	User   domain.UserName
	Mounts MountSet
}

// Home derives the canonical /home/<User> directory from Model.User (SSOT).
func (m Model) Home() domain.AbsPath {
	h, _ := domain.HomeDir(m.User)
	return h
}

// Init constructs the initial guest control model for a validated UserName.
func Init(user domain.UserName) Model {
	return Model{
		User: user,
	}
}

// Invariant checks the inductive safety invariants of Model.
func Invariant(m Model) error {
	if !m.User.Valid() {
		return errors.New("invariant violation: Model.User is not a valid UserName")
	}
	home := m.Home()
	if !home.Valid() {
		return errors.New("invariant violation: derived Home is not a valid AbsPath")
	}
	if !m.Mounts.Canonical() {
		return errors.New("invariant violation: Mounts SortedSet is not in canonical order")
	}
	allMounts := m.Mounts.All()
	for i, mnt := range allMounts {
		if !mnt.Tag.Valid() {
			return fmt.Errorf("invariant violation: invalid share Tag %q", mnt.Tag)
		}
		if !mnt.Path.Valid() || !domain.WithinSubtree(home, mnt.Path) {
			return fmt.Errorf("invariant violation: mount Path %q is not strictly beneath %q", mnt.Path, home)
		}
		if !mnt.FD.Valid() {
			return fmt.Errorf("invariant violation: invalid mount FD %d for tag %q", mnt.FD.Get(), mnt.Tag)
		}
		// Separation Logic (Separating Conjunction M_i * M_j): active mounts
		// must have disjoint open file descriptors and distinct mount paths.
		for _, other := range allMounts[i+1:] {
			if mnt.FD == other.FD {
				return fmt.Errorf("separation violation: mounts %q and %q alias FD %d", mnt.Tag, other.Tag, mnt.FD.Get())
			}
			if mnt.Path == other.Path {
				return fmt.Errorf("separation violation: mounts %q and %q alias path %q", mnt.Tag, other.Tag, mnt.Path)
			}
		}
	}
	return nil
}

// FrameValid enforces the Separation Logic Frame Rule ({P} C {Q} => {P * R} C {Q * R}):
// MsgRequest is a pure read/validation step that never mutates Mounts, and
// MsgMountCompleted / MsgUnmountCompleted only modify the single target Tag's entry,
// leaving all other mounts M_k (k != Tag) unchanged.
func FrameValid(prev Model, msg Msg, next Model) error {
	if prev.User != next.User {
		return errors.New("frame violation: control User is immutable")
	}
	switch ev := msg.(type) {
	case MsgRequest:
		if !slices.Equal(prev.Mounts.All(), next.Mounts.All()) {
			return errors.New("frame violation: MsgRequest mutated Mounts state before completion")
		}
	case MsgMountCompleted:
		for _, m := range prev.Mounts.All() {
			if m.Tag != ev.Tag {
				if nm, ok := next.Mounts.Lookup(m.Tag); !ok || nm != m {
					return fmt.Errorf("frame violation: MsgMountCompleted(%s) mutated unrelated mount %s", ev.Tag, m.Tag)
				}
			}
		}
	case MsgUnmountCompleted:
		for _, m := range prev.Mounts.All() {
			if m.Tag != ev.Tag {
				if nm, ok := next.Mounts.Lookup(m.Tag); !ok || nm != m {
					return fmt.Errorf("frame violation: MsgUnmountCompleted(%s) mutated unrelated mount %s", ev.Tag, m.Tag)
				}
			}
		}
	}
	return nil
}

// StepValid checks the IronFleet / TLA+ transition relation predicate across
// a single step (prev, msg) -> (next, effects).
func StepValid(prev Model, msg Msg, next Model, effects []Effect) error {
	if prev.User != next.User {
		return errors.New("step violation: control User mutated")
	}
	if req, ok := msg.(MsgRequest); ok && req.PeerCID.Get() != proto.HostCID {
		if len(effects) != 1 {
			return errors.New("step violation: non-host CID must produce exactly one EffectReject")
		}
		if _, isRej := effects[0].(EffectReject); !isRej {
			return errors.New("step violation: non-host CID produced non-reject effect")
		}
	}
	home := prev.Home()
	for _, eff := range effects {
		switch e := eff.(type) {
		case EffectReplyHello:
			if !e.Auth.Valid() || !e.User.Valid() {
				return errors.New("step violation: EffectReplyHello has invalid Auth or User")
			}
		case EffectMountVirtiofs:
			if !e.Auth.Valid() || !e.Subtree.Valid() || !e.Tag.Valid() || !e.Path.Valid() || !domain.WithinSubtree(home, e.Path) {
				return fmt.Errorf("step violation: EffectMountVirtiofs with invalid proof, tag %q, or path %q", e.Tag, e.Path)
			}
			if prev.Mounts.Contains(e.Tag) {
				return fmt.Errorf("step violation: EffectMountVirtiofs emitted for already-mounted tag %q", e.Tag)
			}
		case EffectUnmountVirtiofs:
			if !e.Auth.Valid() || !e.Tag.Valid() || !e.FD.Valid() {
				return errors.New("step violation: EffectUnmountVirtiofs has invalid Auth, Tag, or FD")
			}
		case EffectExecSwitch:
			if !e.Auth.Valid() || !e.Toplevel.Valid() {
				return fmt.Errorf("step violation: EffectExecSwitch with invalid Auth or store path %q", e.Toplevel)
			}
		case EffectListSessions:
			if !e.Auth.Valid() {
				return errors.New("step violation: EffectListSessions missing valid HostAuthProof")
			}
		case EffectStopSession:
			if !e.Auth.Valid() || !e.Unit.Valid() {
				return fmt.Errorf("step violation: EffectStopSession with invalid Auth or unit %q", e.Unit)
			}
		case EffectStreamJournal:
			if !e.Auth.Valid() || !e.Lines.Valid() {
				return errors.New("step violation: EffectStreamJournal has invalid Auth or Lines")
			}
			for _, u := range e.Units {
				if !u.Valid() {
					return fmt.Errorf("step violation: EffectStreamJournal with invalid unit %q", u)
				}
			}
		}
	}
	return nil
}

// MountInfoState describes whether a share tag is already present in /proc/self/mountinfo.
type MountInfoState uint8

const (
	// MountInfoAbsent means the tag is not currently mounted in /proc/self/mountinfo.
	MountInfoAbsent MountInfoState = iota
	// MountInfoPresent means the tag is already mounted in /proc/self/mountinfo.
	MountInfoPresent
)

// Msg is the closed sum type of inputs to the guest control state machine.
type Msg interface {
	isMsg()
}

// MsgRequest represents an incoming request on the vsock agent port.
// Raw strings from proto.Request are parsed into types.Option[domain.*]
// at the boundary; any invalid field is represented as None with ParseErr set.
type MsgRequest struct {
	PeerCID   domain.PeerCID
	Op        proto.Op
	Toplevel  types.Option[domain.StorePath]
	Tag       types.Option[domain.ShareTag]
	Path      types.Option[domain.AbsPath]
	Session   types.Option[domain.SessionUnit]
	Units     []domain.UnitName
	Lines     domain.JournalLines
	MountInfo MountInfoState
	ReadOnly  bool
	ParseErr  types.Option[domain.Text]
}

func (MsgRequest) isMsg() {}

// MsgMountCompleted records that the imperative shell mounted Tag and obtained FD.
type MsgMountCompleted struct {
	Tag      domain.ShareTag
	Path     domain.AbsPath
	FD       domain.MountFD
	ReadOnly bool
}

func (MsgMountCompleted) isMsg() {}

// MsgUnmountCompleted records that the imperative shell unmounted and closed Tag.
type MsgUnmountCompleted struct {
	Tag domain.ShareTag
}

func (MsgUnmountCompleted) isMsg() {}

// Effect is the closed sum type of side effects executed by the guest agent shell.
type Effect interface {
	isEffect()
}

// EffectReject rejects the request with an error message.
type EffectReject struct {
	Reason domain.Text
}

func (EffectReject) isEffect() {}

// EffectReplyHello instructs the runtime to check antic-ready.target and send Hello.
type EffectReplyHello struct {
	Auth domain.HostAuthProof
	User domain.UserName
}

func (EffectReplyHello) isEffect() {}

// EffectMountVirtiofs instructs the runtime to open the target directory beneath
// Home and mount the virtiofs tag. Carries both HostAuthProof and WithinHomeProof
// (Ghosts of Departed Proofs).
type EffectMountVirtiofs struct {
	Auth     domain.HostAuthProof
	Subtree  domain.WithinHomeProof
	Tag      domain.ShareTag
	Path     domain.AbsPath
	ReadOnly bool
}

func (EffectMountVirtiofs) isEffect() {}

// EffectUnmountVirtiofs instructs the runtime to detach the mounted FD for Tag.
type EffectUnmountVirtiofs struct {
	Auth domain.HostAuthProof
	Tag  domain.ShareTag
	Path domain.AbsPath
	FD   domain.MountFD
}

func (EffectUnmountVirtiofs) isEffect() {}

// EffectExecSwitch instructs the runtime to run switch-to-configuration on Toplevel.
type EffectExecSwitch struct {
	Auth     domain.HostAuthProof
	Toplevel domain.StorePath
}

func (EffectExecSwitch) isEffect() {}

// EffectListSessions instructs the runtime to read active session cgroups and reply.
type EffectListSessions struct {
	Auth domain.HostAuthProof
}

func (EffectListSessions) isEffect() {}

// EffectStopSession instructs the runtime to stop the validated session systemd unit.
type EffectStopSession struct {
	Auth domain.HostAuthProof
	Unit domain.SessionUnit
}

func (EffectStopSession) isEffect() {}

// EffectStreamJournal instructs the runtime to stream journalctl for Units and Lines.
type EffectStreamJournal struct {
	Auth  domain.HostAuthProof
	Units []domain.UnitName
	Lines domain.JournalLines
}

func (EffectStreamJournal) isEffect() {}

// ParseRequest parses a raw wire proto.Request into a strongly-typed MsgRequest
// at the I/O boundary ("Parse, don't validate").
func ParseRequest(peerCID uint32, req proto.Request, inMountinfo bool) MsgRequest {
	lines, _ := domain.ParseJournalLines(req.Lines)
	minfo := MountInfoAbsent
	if inMountinfo {
		minfo = MountInfoPresent
	}
	msg := MsgRequest{
		PeerCID:   domain.NewPeerCID(peerCID),
		Op:        req.Op,
		Lines:     lines,
		MountInfo: minfo,
		ReadOnly:  req.ReadOnly,
	}
	switch req.Op {
	case proto.OpMount:
		tag, err := domain.ParseShareTag(req.Tag)
		if err != nil {
			msg.ParseErr = types.Some(domain.NewText(err.Error()))
			return msg
		}
		p, err := domain.ParseAbsPath(req.Path)
		if err != nil {
			msg.ParseErr = types.Some(domain.NewText(fmt.Sprintf("mount point %q is not a clean absolute path", req.Path)))
			return msg
		}
		msg.Tag = types.Some(tag)
		msg.Path = types.Some(p)

	case proto.OpUnmount:
		tag, err := domain.ParseShareTag(req.Tag)
		if err != nil {
			msg.ParseErr = types.Some(domain.NewText(fmt.Sprintf("share %s was not mounted by this agent", req.Tag)))
			return msg
		}
		msg.Tag = types.Some(tag)

	case proto.OpApply:
		sp, err := domain.ParseStorePath(req.Toplevel)
		if err != nil {
			msg.ParseErr = types.Some(domain.NewText(err.Error()))
			return msg
		}
		msg.Toplevel = types.Some(sp)

	case proto.OpKill:
		su, err := domain.ParseSessionUnit(req.Session)
		if err != nil {
			msg.ParseErr = types.Some(domain.NewText(err.Error()))
			return msg
		}
		msg.Session = types.Some(su)

	case proto.OpHello, proto.OpSessions:
		// No arguments to parse.

	case proto.OpJournal:
		for _, rawU := range req.Units {
			u, err := domain.ParseUnitName(rawU)
			if err != nil {
				msg.ParseErr = types.Some(domain.NewText(err.Error()))
				return msg
			}
			msg.Units = append(msg.Units, u)
		}
	}
	return msg
}

// Update is the pure, total, deterministic transition function:
// (Model, Msg) -> (Model, []Effect).
func Update(m Model, msg Msg) (Model, []Effect) {
	next := m
	next.Mounts = m.Mounts.Clone()
	home := next.Home()

	switch ev := msg.(type) {
	case MsgRequest:
		auth, err := domain.ProveHostCID(ev.PeerCID)
		if err != nil {
			return next, []Effect{EffectReject{Reason: domain.NewText(err.Error())}}
		}
		if perr, ok := ev.ParseErr.Get(); ok {
			return next, []Effect{EffectReject{Reason: perr}}
		}

		switch ev.Op {
		case proto.OpHello:
			return next, []Effect{EffectReplyHello{Auth: auth, User: next.User}}

		case proto.OpMount:
			tag, okT := ev.Tag.Get()
			p, okP := ev.Path.Get()
			if !okT || !tag.Valid() || !okP || !p.Valid() {
				return next, []Effect{EffectReject{Reason: domain.NewText("invalid mount request")}}
			}
			if next.Mounts.Contains(tag) || ev.MountInfo == MountInfoPresent {
				return next, []Effect{EffectReject{Reason: domain.NewText(fmt.Sprintf("share %s is already mounted", tag.Get()))}}
			}
			subProof, err := domain.ProveWithinHome(home, p)
			if err != nil {
				return next, []Effect{EffectReject{Reason: domain.NewText(err.Error())}}
			}
			return next, []Effect{EffectMountVirtiofs{Auth: auth, Subtree: subProof, Tag: tag, Path: p, ReadOnly: ev.ReadOnly}}

		case proto.OpUnmount:
			tag, okT := ev.Tag.Get()
			if !okT || !tag.Valid() {
				return next, []Effect{EffectReject{Reason: domain.NewText("share was not mounted by this agent")}}
			}
			mnt, ok := next.Mounts.Lookup(tag)
			if !ok {
				return next, []Effect{EffectReject{Reason: domain.NewText(fmt.Sprintf("share %s was not mounted by this agent", tag.Get()))}}
			}
			return next, []Effect{EffectUnmountVirtiofs{Auth: auth, Tag: mnt.Tag, Path: mnt.Path, FD: mnt.FD}}

		case proto.OpApply:
			sp, okS := ev.Toplevel.Get()
			if !okS || !sp.Valid() {
				return next, []Effect{EffectReject{Reason: domain.NewText("invalid system path")}}
			}
			return next, []Effect{EffectExecSwitch{Auth: auth, Toplevel: sp}}

		case proto.OpSessions:
			return next, []Effect{EffectListSessions{Auth: auth}}

		case proto.OpKill:
			su, okU := ev.Session.Get()
			if !okU || !su.Valid() {
				return next, []Effect{EffectReject{Reason: domain.NewText("invalid session")}}
			}
			return next, []Effect{EffectStopSession{Auth: auth, Unit: su}}

		case proto.OpJournal:
			lines := ev.Lines
			if !lines.Valid() {
				lines, _ = domain.ParseJournalLines(200)
			}
			for _, u := range ev.Units {
				if !u.Valid() {
					return next, []Effect{EffectReject{Reason: domain.NewText(fmt.Sprintf("invalid unit %q", u.Get()))}}
				}
			}
			return next, []Effect{EffectStreamJournal{Auth: auth, Units: slices.Clone(ev.Units), Lines: lines}}

		default:
			return next, []Effect{EffectReject{Reason: domain.NewText(fmt.Sprintf("unknown op %q", ev.Op))}}
		}

	case MsgMountCompleted:
		if !ev.Tag.Valid() || !ev.Path.Valid() || !domain.WithinSubtree(home, ev.Path) || !ev.FD.Valid() {
			return next, nil
		}
		if next.Mounts.Contains(ev.Tag) {
			return next, nil
		}
		for _, other := range next.Mounts.All() {
			if other.FD == ev.FD || other.Path == ev.Path {
				return next, nil
			}
		}
		//nolint:staticcheck // S1016: a literal keeps the message and the model's entry independent types
		next.Mounts = next.Mounts.Upsert(MountEntry{
			Tag:      ev.Tag,
			Path:     ev.Path,
			FD:       ev.FD,
			ReadOnly: ev.ReadOnly,
		})
		return next, nil

	case MsgUnmountCompleted:
		if !ev.Tag.Valid() {
			return next, nil
		}
		next.Mounts = next.Mounts.Remove(ev.Tag)
		return next, nil
	}

	return next, nil
}
