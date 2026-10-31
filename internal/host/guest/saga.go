package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

// The imperative shell of internal/tea/operation: a command plans an
// operation.Op and a txn of concrete artifacts under the guest's lock, and
// runSaga executes the core's effects one at a time (docs/design/spec.md
// §11). Inverse effects are idempotent ("ensure Old"), as recovery requires.

// txn holds the concrete old and new artifacts of one command.
type txn struct {
	op    operation.Op
	draft config.Draft
	// modes records whether each share the saga hot-plugs is read-only.
	modes map[string]bool
	// newSystem is the system Build built; oldProfile is the system the
	// guest store's profile pointed at ("" if none); oldBooted is the
	// system the running guest ran.
	newSystem, oldProfile, oldBooted string
}

// journalRecord is the durable journal of an interrupted command.
type journalRecord struct {
	Op struct {
		Kind    operation.OpKind     `json:"kind"`
		Phase   operation.GuestPhase `json:"phase"`
		Config  operation.ChangeKind `json:"config"`
		System  operation.ChangeKind `json:"system"`
		Restart operation.ChangeKind `json:"restart"`
		Added   []string             `json:"added,omitempty"`
		Removed []string             `json:"removed,omitempty"`
		Runtime []string             `json:"runtime,omitempty"`
	} `json:"op"`
	ConfigPath string          `json:"config_path"`
	NewFile    bool            `json:"new_file,omitempty"` // the command creates the config file
	InstanceID string          `json:"instance_id,omitempty"`
	OldConfig  []byte          `json:"old_config"`
	NewConfig  []byte          `json:"new_config"`
	Modes      map[string]bool `json:"modes,omitempty"`
	NewSystem  string          `json:"new_system,omitempty"`
	OldProfile string          `json:"old_profile,omitempty"`
	OldBooted  string          `json:"old_booted,omitempty"`
	Intents    []int           `json:"intents"`
}

func pathStrings(s operation.PathSet) []string {
	var out []string
	for _, p := range s.All() {
		out = append(out, p.Get())
	}
	return out
}

func pathSet(ps []string) (operation.PathSet, error) {
	var out []domain.AbsPath
	for _, p := range ps {
		a, err := domain.ParseAbsPath(p)
		if err != nil {
			return operation.PathSet{}, err
		}
		out = append(out, a)
	}
	return types.BuildSet[domain.AbsPath, domain.AbsPath, domain.AbsPathOrder](out), nil
}

func newJournalRecord(tx *txn) journalRecord {
	var r journalRecord
	r.Op.Kind, r.Op.Phase = tx.op.Kind, tx.op.Phase
	r.Op.Config, r.Op.System, r.Op.Restart = tx.op.Change.Config, tx.op.Change.System, tx.op.Change.Restart
	r.Op.Added, r.Op.Removed, r.Op.Runtime = pathStrings(tx.op.Change.Added), pathStrings(tx.op.Change.Removed), pathStrings(tx.op.Runtime)
	r.ConfigPath, r.OldConfig, r.NewConfig, r.NewFile = tx.draft.Path, tx.draft.Old, tx.draft.New, tx.draft.NewFile
	r.Modes = tx.modes
	r.NewSystem, r.OldProfile, r.OldBooted = tx.newSystem, tx.oldProfile, tx.oldBooted
	return r
}

// txnOf reconstructs the command a journal records.
func (r journalRecord) txnOf(name string) (*txn, error) {
	op := operation.Op{Kind: r.Op.Kind, Phase: r.Op.Phase, Change: operation.Change{
		Config: r.Op.Config, System: r.Op.System, Restart: r.Op.Restart}}
	var err error
	if op.Change.Added, err = pathSet(r.Op.Added); err != nil {
		return nil, err
	}
	if op.Change.Removed, err = pathSet(r.Op.Removed); err != nil {
		return nil, err
	}
	if op.Runtime, err = pathSet(r.Op.Runtime); err != nil {
		return nil, err
	}
	return &txn{op: op, modes: r.Modes, draft: config.Draft{Path: r.ConfigPath, Name: name, Old: r.OldConfig, New: r.NewConfig},
		newSystem: r.NewSystem, oldProfile: r.OldProfile, oldBooted: r.OldBooted}, nil
}

func (g *Guest) journalPath() string {
	return filepath.Join(g.m.Dirs.StateRoot, "journals", g.Name()+".json")
}

func (g *Guest) writeJournal(r journalRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := g.journalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (g *Guest) readJournal() (journalRecord, bool, error) {
	var r journalRecord
	b, err := os.ReadFile(g.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, false, fmt.Errorf("%s: %w", g.journalPath(), err)
	}
	return r, true, nil
}

// traceEnv names an NDJSON file the shell appends its saga events to:
// taking and releasing the guest's lock, planning (with the plan's number
// of commit steps), and each executed effect's stage and outcome.
// internal/spec/refine checks these traces against formal/saga/Saga.tla.
const traceEnv = "ANTIC_SAGA_TRACE"

// traceEvent is one line of a saga trace.
type traceEvent struct {
	PID     int             `json:"pid"`
	Guest   string          `json:"guest"`
	Event   string          `json:"event"` // acquire, plan, step, release
	Commits int             `json:"commits,omitempty"`
	Stage   operation.Stage `json:"stage,omitempty"`
	OK      bool            `json:"ok,omitempty"`
}

func (g *Guest) trace(ev traceEvent) {
	path := os.Getenv(traceEnv)
	if path == "" {
		return
	}
	ev.PID, ev.Guest = os.Getpid(), g.Name()
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n')) // one write: lines of concurrent processes do not mix
}

// lockAndRecover takes the guest's lock and, if a crashed command left a
// journal, undoes that command before the caller plans its own.
func (g *Guest) lockAndRecover(ctx context.Context) (func(), error) {
	locked, err := g.lock()
	if err != nil {
		return nil, err
	}
	g.trace(traceEvent{Event: "acquire"})
	unlock := func() {
		g.trace(traceEvent{Event: "release"})
		locked()
	}
	r, pending, err := g.readJournal()
	if err == nil && pending {
		err = g.recoverJournal(ctx, r)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (g *Guest) recoverJournal(ctx context.Context, r journalRecord) error {
	g.m.logf("undoing an interrupted command on guest %s...", g.Name())
	tx, err := r.txnOf(g.Name())
	if err != nil {
		return fmt.Errorf("guest %s: unreadable journal %s: %w", g.Name(), g.journalPath(), err)
	}
	if r.InstanceID != "" && !g.Created {
		g.Inst, g.Created = g.m.Dirs.InstanceOf(g.Name(), r.InstanceID), true
	}
	tx.draft = config.RestoreDraft(r.ConfigPath, g.Name(), g.m.Dirs.Home, r.NewFile, r.OldConfig, r.NewConfig)
	last := types.None[domain.StepIndex]()
	if n := len(r.Intents); n > 0 {
		i, err := domain.ParseStepIndex(r.Intents[n-1])
		if err != nil {
			return err
		}
		last = types.Some(i)
	}
	if err := g.runSaga(ctx, tx, operation.MsgRecover{Op: tx.op, Last: last}); err != nil {
		return fmt.Errorf("guest %s: undoing an interrupted command: %w", g.Name(), err)
	}
	return nil
}

// crashAfterEnv is a fault-injection hook for the E2E tests: with
// ANTIC_SAGA_CRASH_AFTER=k a saga exits right after its k-th effect, as a
// killed process would, so that the next command must recover.
const crashAfterEnv = "ANTIC_SAGA_CRASH_AFTER"

// failEnv is the other E2E hook: with ANTIC_SAGA_FAIL=NAME[:n] the n-th
// effect (default the first) of type EffectNAME fails without running, so
// that the saga must undo what it committed.
const failEnv = "ANTIC_SAGA_FAIL"

// injectedFailure reports whether the effect is the one failEnv names;
// seen counts the effects of each type so far.
func injectedFailure(eff operation.Effect, seen map[string]int) bool {
	spec := os.Getenv(failEnv)
	if spec == "" {
		return false
	}
	name, nth, _ := strings.Cut(spec, ":")
	n, err := strconv.Atoi(nth)
	if err != nil || n < 1 {
		n = 1
	}
	typ := strings.TrimPrefix(fmt.Sprintf("%T", eff), "operation.Effect")
	seen[typ]++
	return typ == name && seen[typ] == n
}

// runSaga feeds msg to a new saga and executes its effects until it reports.
func (g *Guest) runSaga(ctx context.Context, tx *txn, msg operation.Msg) error {
	name, err := domain.ParseGuestName(g.Name())
	if err != nil {
		return err
	}
	m := operation.Init(name)
	var jr journalRecord
	crashAfter, _ := strconv.Atoi(os.Getenv(crashAfterEnv))
	seen := map[string]int{}
	for executed := 0; ; executed++ {
		next, effects := operation.Update(m, msg)
		if len(effects) != 1 {
			return fmt.Errorf("internal error: saga stalled in stage %d", next.Stage)
		}
		if _, command := msg.(operation.MsgCommand); command && m.Stage == operation.StageIdle {
			commits := 0
			for _, s := range next.Plan.All() {
				if s.Phase == operation.PhaseCommit {
					commits++
				}
			}
			g.trace(traceEvent{Event: "plan", Commits: commits})
		}
		if rep, ok := effects[0].(operation.EffectReport); ok {
			switch rep.Status {
			case operation.ReportOk:
				return nil
			case operation.ReportBroken:
				return fmt.Errorf("%s (guest %s may be inconsistent; the next command on it retries the undo)", rep.Reason.Get(), g.Name())
			case operation.ReportFailed:
			}
			return errors.New(rep.Reason.Get())
		}
		m = next
		var err error
		if injectedFailure(effects[0], seen) {
			err = fmt.Errorf("injected failure (%s)", failEnv)
		} else {
			err = g.runEffect(ctx, tx, &jr, effects[0])
		}
		g.trace(traceEvent{Event: "step", Stage: m.Stage, OK: err == nil})
		if crashAfter > 0 && executed+1 == crashAfter {
			os.Exit(137) // as if killed (the kernel releases the lock)
		}
		msg = operation.MsgResult{Status: operation.ResultOk}
		if err != nil {
			msg = operation.MsgResult{Status: operation.ResultErr, Reason: domain.NewText(err.Error())}
		}
	}
}

// runEffect runs one effect.
func (g *Guest) runEffect(ctx context.Context, tx *txn, jr *journalRecord, eff operation.Effect) error {
	switch e := eff.(type) {
	case operation.EffectJournalBegin:
		*jr = newJournalRecord(tx)
		jr.InstanceID = g.Inst.ID
		return g.writeJournal(*jr)
	case operation.EffectJournalIntent:
		jr.Intents = append(jr.Intents, e.Pos.Get())
		return g.writeJournal(*jr)
	case operation.EffectJournalClear:
		if err := os.Remove(g.journalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case operation.EffectValidate:
		// The draft loads (config.Draft checked it); new shares must also be
		// allowed by the share policy.
		for _, s := range tx.draft.After.Shares {
			if tx.draft.Before != nil {
				if _, had := tx.draft.Before.ShareAt(config.RealPath(s.Path)); had {
					continue
				}
			}
			if _, err := g.m.policy().Resolve(s.Path); err != nil {
				return err
			}
		}
		return nil
	case operation.EffectWriteConfig:
		return tx.draft.Write(e.To == operation.VersionOld)
	case operation.EffectBuild:
		if tx.newSystem != "" {
			return nil // built ahead of the saga (prebuild)
		}
		sys, err := g.build(ctx, tx.draft.After)
		tx.newSystem = sys
		return err
	case operation.EffectAssignInstance:
		if err := g.ensureInstance(); err != nil {
			return err
		}
		// Recovery must find the instance to remove it.
		jr.InstanceID = g.Inst.ID
		return g.writeJournal(*jr)
	case operation.EffectTrash:
		if e.What == operation.TrashInstance {
			return g.m.Dirs.TrashInstance(g.Inst)
		}
		return g.m.Dirs.TrashConfig(g.Name(), g.m.Config.GuestPath(g.Name()))
	case operation.EffectRestore:
		if e.What == operation.TrashInstance {
			return g.m.Dirs.RestoreInstance(g.Inst)
		}
		return g.m.Dirs.RestoreConfig(g.Name(), g.m.Config.GuestPath(g.Name()))
	case operation.EffectEmptyTrash:
		if err := g.m.Dirs.EmptyTrash(g.Name()); err != nil {
			return err
		}
		if orphans, err := g.m.Dirs.Orphans(); err == nil {
			for _, p := range orphans {
				instance.RemoveDir(p)
			}
		}
		return nil
	case operation.EffectRemoveInstance:
		if !g.Created {
			return nil
		}
		if err := g.m.Dirs.Remove(g.Inst); err != nil {
			return err
		}
		g.Inst, g.Created = instance.Instance{}, false
		return nil
	case operation.EffectSetProfile:
		nix, err := g.m.nixBuilder()
		if err != nil {
			return err
		}
		system := tx.newSystem
		if e.To == operation.VersionOld {
			system = tx.oldProfile
		}
		if system == "" {
			return nix.RemoveProfile(g.store())
		}
		g.m.logf("installing %s into the guest store...", system)
		return nix.SetProfile(ctx, g.store(), system)
	case operation.EffectSwitch:
		if e.To == operation.VersionOld {
			return g.switchTo(ctx, tx.oldBooted)
		}
		return g.switchTo(ctx, tx.newSystem)
	case operation.EffectGC:
		nix, err := g.m.nixBuilder()
		if err != nil {
			return err
		}
		return nix.GCGuest(ctx, g.store())
	case operation.EffectCheckPorts:
		return g.checkPorts(ctx, tx)
	case operation.EffectStopVM:
		if tx.op.Kind == operation.OpChangeConfig {
			g.m.logf("restarting guest %s to apply the change...", g.Name())
		}
		return g.stopLocked(ctx)
	case operation.EffectStartVM:
		c, system := tx.draft.After, tx.newSystem
		if e.Config == operation.VersionOld {
			c, system = tx.draft.Before, tx.oldBooted
		}
		if c == nil {
			return errors.New("internal error: the config to boot is unknown")
		}
		if system == "" {
			system, _ = g.store().Current()
		}
		g.Cfg = c
		if err := g.startSystem(ctx, system); err != nil {
			return err
		}
		for _, p := range e.Rehotplug.All() {
			if err := g.ensureMounted(ctx, p.Get(), tx.modes[p.Get()]); err != nil {
				return err
			}
		}
		return nil
	case operation.EffectHotplug:
		return g.ensureMounted(ctx, e.Path.Get(), tx.modes[e.Path.Get()])
	case operation.EffectHotunplug:
		return g.ensureUnmounted(ctx, e.Path.Get())
	}
	return fmt.Errorf("internal error: %T is not implemented by this command yet", eff)
}

// checkPorts checks that the host ports the new config forwards are free,
// except those the running guest forwards already.
func (g *Guest) checkPorts(ctx context.Context, tx *txn) error {
	var own []config.Port
	if tx.op.Phase == operation.GuestRunning && tx.draft.Before != nil {
		own = tx.draft.Before.Ports
	}
	for _, p := range tx.draft.After.Ports {
		if slices.ContainsFunc(own, func(q config.Port) bool { return q.Host == p.Host }) {
			continue
		}
		addr := fmt.Sprintf("127.0.0.1:%d", p.Host)
		if p.TCP() {
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
			if err != nil {
				return fmt.Errorf("host port %d/tcp is in use", p.Host)
			}
			l.Close()
		}
		if p.UDP() {
			l, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
			if err != nil {
				return fmt.Errorf("host port %d/udp is in use", p.Host)
			}
			l.Close()
		}
	}
	return nil
}

// RecoverOrphan undoes a command a crashed process left in the journal of
// a guest whose config file is gone (a create or rm interrupted while the
// file was moved), so that the guest can be used or removed again.
func (m *Manager) RecoverOrphan(ctx context.Context, name string) error {
	g := &Guest{m: m, Cfg: &config.Guest{Name: name, Path: m.Config.GuestPath(name)}}
	r, pending, err := g.readJournal()
	if err != nil || !pending {
		return err
	}
	if r.InstanceID != "" {
		g.Inst, g.Created = m.Dirs.InstanceOf(name, r.InstanceID), true
	}
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	unlock()
	c, err := config.Load(m.Config.Dir(), m.Dirs.Home)
	if err != nil {
		return err
	}
	m.Config.Guests, m.Config.Errors = c.Guests, c.Errors
	return nil
}

// ensureInstance allocates the guest's instance unless it has one.
func (g *Guest) ensureInstance() error {
	if !g.Created {
		inst, err := g.m.Dirs.Assign(g.Name())
		if err != nil {
			return err
		}
		g.Inst, g.Created = inst, true
		g.m.logf("guest %s has ID %s", g.Name(), inst.ID)
	}
	return os.WriteFile(g.Inst.OriginFile(), []byte(g.Cfg.Path+"\n"), 0o600)
}

// ensureMounted hot-plugs path unless the running guest already has it.
func (g *Guest) ensureMounted(ctx context.Context, path string, ro bool) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(bt.Shares, func(s bootShare) bool { return s.Path == path }) {
		return nil
	}
	return g.hotplug(ctx, bt, path, ro)
}

// ensureUnmounted detaches the hot-plugged share at path unless it is gone.
// It undoes its own partial work on failure.
func (g *Guest) ensureUnmounted(ctx context.Context, path string) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(bt.Shares, func(s bootShare) bool { return s.Path == path && s.Runtime })
	if i < 0 {
		return nil
	}
	s := bt.Shares[i]
	if err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpUnmount, Tag: s.Tag}, nil); err != nil {
		return err
	}
	if err := g.api().RemoveDevice(ctx, "fs-"+s.Tag); err != nil {
		remount := g.vsock().Agent(context.WithoutCancel(ctx), proto.Request{Op: proto.OpMount, Tag: s.Tag, Path: s.Path, ReadOnly: s.ReadOnly}, nil)
		return errors.Join(err, remount)
	}
	time.Sleep(2 * time.Second) // see hotplug
	// The device is gone, so the share is unmounted whatever happens to its
	// virtiofsd; record that before stopping it.
	bt.Shares = slices.Delete(bt.Shares, i, i+1)
	if err := g.writeBoot(bt); err != nil {
		return err
	}
	if err := g.m.Units.Stop(ctx, g.shareUnit(s.Tag)); err != nil {
		g.m.logf("warning: %s is unmounted, but stopping its virtiofsd failed: %v", path, err)
	}
	return nil
}
