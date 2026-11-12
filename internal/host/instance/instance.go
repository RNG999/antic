// Package instance manages guest instances on the host: their IDs (a name
// maps to a random ID when the guest is created), their directories and a
// per-guest lock. Where things live:
//
//	~/.config/antic/guests/<name>.toml  config per guest
//	~/.local/state/antic/
//	  guests/<ID>/                      one guest: store/ (its /nix), log/
//	  names/<NAME> -> ../guests/<ID>    which ID a name has
//	  locks/<NAME>.lock                 serializes lifecycle operations
//	$XDG_RUNTIME_DIR/antic/<ID>/        vm/ (cloud-hypervisor), sock/<helper>/, boot.json
package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
	"golang.org/x/sys/unix"
)

// Dirs is the set of host directories antic uses.
type Dirs struct {
	Home      domain.AbsPath
	ConfigDir domain.AbsPath
	StateRoot domain.AbsPath
	RunRoot   domain.AbsPath
	BinDir    domain.AbsPath
	Libexec   domain.AbsPath
}

// Default returns the directories for the current user.
func Default() (Dirs, error) {
	rawHome, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	home, err := domain.ParseAbsPath(filepath.Clean(rawHome))
	if err != nil {
		return Dirs{}, err
	}
	rawRun := os.Getenv("XDG_RUNTIME_DIR")
	if rawRun == "" {
		rawRun = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	runDir, err := domain.ParseAbsPath(filepath.Clean(rawRun))
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{
		Home:      home,
		ConfigDir: domain.JoinAbsPath(home, ".config/antic"),
		StateRoot: domain.JoinAbsPath(home, ".local/state/antic"),
		RunRoot:   domain.JoinAbsPath(runDir, "antic"),
		BinDir:    domain.JoinAbsPath(home, ".local/bin"),
		Libexec:   domain.JoinAbsPath(home, ".local/libexec/antic"),
	}, nil
}

func (d Dirs) GuestsConfigDir() domain.AbsPath { return domain.JoinAbsPath(d.ConfigDir, "guests") }
func (d Dirs) CacheDir() domain.AbsPath        { return domain.JoinAbsPath(d.StateRoot, "cache") }
func (d Dirs) guestsDir() domain.AbsPath       { return domain.JoinAbsPath(d.StateRoot, "guests") }
func (d Dirs) namesDir() domain.AbsPath        { return domain.JoinAbsPath(d.StateRoot, "names") }

// Instance is a guest's identity and its directories.
type Instance struct {
	Name domain.GuestName
	ID   domain.InstanceID
	d    Dirs
}

func (i Instance) StateDir() domain.AbsPath   { return domain.JoinAbsPath(i.d.guestsDir(), i.ID.Get()) }
func (i Instance) StoreDir() domain.AbsPath   { return domain.JoinAbsPath(i.StateDir(), "store") } // the guest's /nix lives in store/nix
func (i Instance) LogDir() domain.AbsPath     { return domain.JoinAbsPath(i.StateDir(), "log") }
func (i Instance) Console() domain.AbsPath    { return domain.JoinAbsPath(i.LogDir(), "console.log") }
func (i Instance) OriginFile() domain.AbsPath { return domain.JoinAbsPath(i.StateDir(), "origin") }
func (i Instance) RunDir() domain.AbsPath     { return domain.JoinAbsPath(i.d.RunRoot, i.ID.Get()) }
func (i Instance) VMDir() domain.AbsPath      { return domain.JoinAbsPath(i.RunDir(), "vm") }   // cloud-hypervisor's own sockets
func (i Instance) SockDir() domain.AbsPath    { return domain.JoinAbsPath(i.RunDir(), "sock") } // helpers' sockets, one directory each
func (i Instance) BootFile() domain.AbsPath   { return domain.JoinAbsPath(i.RunDir(), "boot.json") }

// Lookup returns the instance of name; ok is false if it has no ID yet. A
// names/ link that is not exactly "../guests/<12 hex digits>" is an error,
// never something to act on.
func (d Dirs) Lookup(name domain.GuestName) (Instance, bool, error) {
	link := domain.JoinAbsPath(d.namesDir(), name.Get())
	dst, err := os.Readlink(link.Get())
	if errors.Is(err, os.ErrNotExist) {
		return Instance{Name: name, d: d}, false, nil
	}
	if err != nil {
		return Instance{}, false, fmt.Errorf("%s: %w", link.Get(), err)
	}
	idStr := filepath.Base(dst)
	if dst != filepath.Join("..", "guests", idStr) {
		return Instance{}, false, fmt.Errorf("%s points to %q, not ../guests/<id>; fix or remove it", link.Get(), dst)
	}
	id, err := domain.ParseInstanceID(idStr)
	if err != nil {
		return Instance{}, false, fmt.Errorf("%s points to %q, not ../guests/<id>; fix or remove it", link.Get(), dst)
	}
	return Instance{Name: name, ID: id, d: d}, true, nil
}

// Assign gives name a new ID (replacing an old link, whose state is left
// for "guest prune").
func (d Dirs) Assign(name domain.GuestName) (Instance, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return Instance{}, err
	}
	id, err := domain.ParseInstanceID(hex.EncodeToString(b))
	if err != nil {
		return Instance{}, err
	}
	inst := Instance{Name: name, ID: id, d: d}
	if err := os.MkdirAll(inst.StateDir().Get(), 0o700); err != nil {
		return Instance{}, err
	}
	if err := os.MkdirAll(d.namesDir().Get(), 0o700); err != nil {
		return Instance{}, err
	}
	tmp := domain.JoinAbsPath(d.namesDir(), "."+name.Get()+".new")
	os.Remove(tmp.Get())
	if err := os.Symlink(filepath.Join("..", "guests", inst.ID.Get()), tmp.Get()); err != nil {
		return Instance{}, err
	}
	return inst, os.Rename(tmp.Get(), domain.JoinAbsPath(d.namesDir(), name.Get()).Get())
}

// Remove deletes the instance's state and its name link.
func (d Dirs) Remove(inst Instance) error {
	if !inst.ID.Valid() {
		return fmt.Errorf("refusing to remove guest state with invalid ID %q", inst.ID.Get())
	}
	if err := removeAll(inst.StateDir()); err != nil {
		return err
	}
	os.RemoveAll(inst.RunDir().Get())
	link := domain.JoinAbsPath(d.namesDir(), inst.Name.Get())
	if dst, err := os.Readlink(link.Get()); err == nil && filepath.Base(dst) == inst.ID.Get() {
		return os.Remove(link.Get())
	}
	return nil
}

// InstanceOf returns the instance of name with a known ID (a journal
// records it while the name link is in the trash).
func (d Dirs) InstanceOf(name domain.GuestName, id domain.InstanceID) Instance {
	return Instance{Name: name, ID: id, d: d}
}

// TrashDir holds what "guest rm" moved aside until it succeeds.
func (d Dirs) TrashDir(name domain.GuestName) domain.AbsPath {
	return domain.JoinAbsPath(d.StateRoot, "trash", name.Get())
}

// TrashInstance moves the instance's state and name link to the trash.
// Moving an instance that is already there does nothing.
func (d Dirs) TrashInstance(inst Instance) error {
	if !inst.ID.Valid() {
		return fmt.Errorf("refusing to move guest state with invalid ID %q", inst.ID.Get())
	}
	trash := d.TrashDir(inst.Name)
	if err := os.MkdirAll(trash.Get(), 0o700); err != nil {
		return err
	}
	if err := renameIfExists(inst.StateDir(), domain.JoinAbsPath(trash, inst.ID.Get())); err != nil {
		return err
	}
	link := domain.JoinAbsPath(d.namesDir(), inst.Name.Get())
	if dst, err := os.Readlink(link.Get()); err == nil && filepath.Base(dst) == inst.ID.Get() {
		if err := os.Rename(link.Get(), domain.JoinAbsPath(trash, "name").Get()); err != nil {
			return err
		}
	}
	os.RemoveAll(inst.RunDir().Get())
	return nil
}

// RestoreInstance moves a trashed instance back. Restoring an instance
// that is not in the trash does nothing.
func (d Dirs) RestoreInstance(inst Instance) error {
	trash := d.TrashDir(inst.Name)
	if err := renameIfExists(domain.JoinAbsPath(trash, inst.ID.Get()), inst.StateDir()); err != nil {
		return err
	}
	if err := os.MkdirAll(d.namesDir().Get(), 0o700); err != nil {
		return err
	}
	return renameIfExists(domain.JoinAbsPath(trash, "name"), domain.JoinAbsPath(d.namesDir(), inst.Name.Get()))
}

// TrashConfig moves guest name's config file at configPath to the trash; moving
// a file that is already there does nothing.
func (d Dirs) TrashConfig(name domain.GuestName, configPath domain.AbsPath) error {
	return renameIfExists(configPath, domain.JoinAbsPath(d.TrashDir(name), "config.toml"))
}

// RestoreConfig moves a trashed config file back to configPath; restoring a file
// that is not in the trash does nothing.
func (d Dirs) RestoreConfig(name domain.GuestName, configPath domain.AbsPath) error {
	return renameIfExists(domain.JoinAbsPath(d.TrashDir(name), "config.toml"), configPath)
}

// EmptyTrash deletes what "guest rm" moved aside for name.
func (d Dirs) EmptyTrash(name domain.GuestName) error { return removeAll(d.TrashDir(name)) }

func renameIfExists(from, to domain.AbsPath) error {
	if _, err := os.Lstat(from.Get()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to.Get()), 0o700); err != nil {
		return err
	}
	return os.Rename(from.Get(), to.Get())
}

// removeAll deletes dir, including the guest store, whose directories Nix
// makes read-only.
func removeAll(dir domain.AbsPath) error {
	filepath.WalkDir(dir.Get(), func(walkEntryPath string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(walkEntryPath, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir.Get())
}

// RemoveDir deletes a guest state directory (see Orphans).
func RemoveDir(dir domain.AbsPath) error { return removeAll(dir) }

// Orphans lists state directories no name links to.
func (d Dirs) Orphans() ([]domain.AbsPath, error) {
	used := map[domain.InstanceID]bool{}
	links, _ := os.ReadDir(d.namesDir().Get())
	for _, e := range links {
		if dst, err := os.Readlink(domain.JoinAbsPath(d.namesDir(), e.Name()).Get()); err == nil {
			if id, idErr := domain.ParseInstanceID(filepath.Base(dst)); idErr == nil {
				used[id] = true
			}
		}
	}
	dirs, err := os.ReadDir(d.guestsDir().Get())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []domain.AbsPath
	for _, e := range dirs {
		if id, parseErr := domain.ParseInstanceID(e.Name()); parseErr == nil && !used[id] {
			out = append(out, domain.JoinAbsPath(d.guestsDir(), id.Get()))
		}
	}
	return out, err
}

const (
	defaultLockTimeout     = 60 * time.Second
	defaultNixBuildTimeout = 20 * time.Minute
)

func defaultTimeout(fallback time.Duration) time.Duration {
	if s := os.Getenv("ANTIC_LOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// Lock serializes lifecycle operations on guest name across antic
// processes (AI agents run tools in parallel). Unlock by calling the
// returned function. Times out after defaultLockTimeout.
func (d Dirs) Lock(name domain.GuestName) (func(), error) {
	return d.LockWithTimeout(name, defaultTimeout(defaultLockTimeout))
}

// LockNixBuild serializes Nix builds across antic processes so that
// concurrent builds don't collide on the Nix eval-cache or store lock.
// Times out after defaultNixBuildTimeout.
func (d Dirs) LockNixBuild() (func(), error) {
	timeout := defaultTimeout(defaultNixBuildTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.lockNamed(ctx, "nixbuild", "Nix build", timeout)
}

// LockUpdate serializes self-update operations across antic processes.
// Times out after defaultLockTimeout.
func (d Dirs) LockUpdate() (func(), error) {
	timeout := defaultTimeout(defaultLockTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.lockNamed(ctx, "update", "antic update", timeout)
}

// LockWithTimeout takes an exclusive lock for guest name under StateRoot/locks/, waiting up to timeout.
func (d Dirs) LockWithTimeout(name domain.GuestName, timeout time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.LockWithContext(ctx, name, timeout)
}

// LockWithContext takes an exclusive lock for guest name under StateRoot/locks/.
// If the lock is held by another process, it notifies the user on stderr
// and polls until acquired or ctx is cancelled / timeout expires.
func (d Dirs) LockWithContext(ctx context.Context, name domain.GuestName, timeout time.Duration) (func(), error) {
	return d.lockNamed(ctx, name.Get(), fmt.Sprintf("guest %q", name.Get()), timeout)
}

func (d Dirs) lockNamed(ctx context.Context, lockKey, description string, _ time.Duration) (func(), error) {
	locksDir := domain.JoinAbsPath(d.StateRoot, "locks")
	if err := os.MkdirAll(locksDir.Get(), 0o700); err != nil {
		return nil, err
	}
	lockFile := domain.JoinAbsPath(locksDir, lockKey+".lock")
	f, err := os.OpenFile(lockFile.Get(), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}

	// Try non-blocking first to detect contention and inform the user.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		writeLockPID(f)
		return lockCleanup(f), nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		f.Close()
		return nil, err
	}

	holderMsg := "another process"
	if pid, ok := readLockPID(lockFile).Get(); ok {
		holderMsg = fmt.Sprintf("PID %d", pid.Get())
	}
	fmt.Fprintf(os.Stderr, "antic: waiting for lock on %s (held by %s)...\n", description, holderMsg)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			f.Close()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if pid, ok := readLockPID(lockFile).Get(); ok {
					holderMsg = fmt.Sprintf("PID %d", pid.Get())
				}
				elapsed := time.Since(start).Round(time.Millisecond)
				if elapsed >= time.Second {
					elapsed = elapsed.Round(time.Second)
				}
				return nil, fmt.Errorf("timed out waiting for lock on %s after %v (held by %s)", description, elapsed, holderMsg)
			}
			return nil, fmt.Errorf("interrupted waiting for lock on %s (held by %s): %w", description, holderMsg, ctx.Err())
		case <-ticker.C:
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
				writeLockPID(f)
				return lockCleanup(f), nil
			} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				f.Close()
				return nil, err
			}
		}
	}
}

func writeLockPID(f *os.File) {
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Sync()
}

func lockCleanup(f *os.File) func() {
	return func() {
		_ = f.Truncate(0)
		_ = f.Sync()
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}
}

func readLockPID(lockFile domain.AbsPath) types.Option[domain.ProcessID] {
	b, err := os.ReadFile(lockFile.Get())
	if err != nil {
		return types.None[domain.ProcessID]()
	}
	rawPID, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return types.None[domain.ProcessID]()
	}
	pid, err := domain.ParseProcessID(rawPID)
	if err != nil {
		return types.None[domain.ProcessID]()
	}
	return types.Some(pid)
}
