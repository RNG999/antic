package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

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
