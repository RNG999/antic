package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/tea/operation"
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
