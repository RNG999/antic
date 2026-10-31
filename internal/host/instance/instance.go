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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Dirs is the set of host directories antic uses.
type Dirs struct {
	Home      string
	ConfigDir string
	StateRoot string
	RunRoot   string
	BinDir    string
	Libexec   string
}

// Default returns the directories for the current user.
func Default() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		run = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	return Dirs{
		Home:      home,
		ConfigDir: filepath.Join(home, ".config/antic"),
		StateRoot: filepath.Join(home, ".local/state/antic"),
		RunRoot:   filepath.Join(run, "antic"),
		BinDir:    filepath.Join(home, ".local/bin"),
		Libexec:   filepath.Join(home, ".local/libexec/antic"),
	}, nil
}

func (d Dirs) GuestsConfigDir() string { return filepath.Join(d.ConfigDir, "guests") }
func (d Dirs) CacheDir() string        { return filepath.Join(d.StateRoot, "cache") }
func (d Dirs) guestsDir() string       { return filepath.Join(d.StateRoot, "guests") }
func (d Dirs) namesDir() string        { return filepath.Join(d.StateRoot, "names") }

// Instance is a guest's identity and its directories.
type Instance struct {
	Name, ID string
	d        Dirs
}

func (i Instance) StateDir() string   { return filepath.Join(i.d.guestsDir(), i.ID) }
func (i Instance) StoreDir() string   { return filepath.Join(i.StateDir(), "store") } // the guest's /nix lives in store/nix
func (i Instance) LogDir() string     { return filepath.Join(i.StateDir(), "log") }
func (i Instance) Console() string    { return filepath.Join(i.LogDir(), "console.log") }
func (i Instance) OriginFile() string { return filepath.Join(i.StateDir(), "origin") }
func (i Instance) RunDir() string     { return filepath.Join(i.d.RunRoot, i.ID) }
func (i Instance) VMDir() string      { return filepath.Join(i.RunDir(), "vm") }   // cloud-hypervisor's own sockets
func (i Instance) SockDir() string    { return filepath.Join(i.RunDir(), "sock") } // helpers' sockets, one directory each
func (i Instance) BootFile() string   { return filepath.Join(i.RunDir(), "boot.json") }

var idRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Lookup returns the instance of name; ok is false if it has no ID yet. A
// names/ link that is not exactly "../guests/<12 hex digits>" is an error,
// never something to act on.
func (d Dirs) Lookup(name string) (Instance, bool, error) {
	link := filepath.Join(d.namesDir(), name)
	dst, err := os.Readlink(link)
	if errors.Is(err, os.ErrNotExist) {
		return Instance{Name: name, d: d}, false, nil
	}
	if err != nil {
		return Instance{}, false, fmt.Errorf("%s: %w", link, err)
	}
	id := filepath.Base(dst)
	if dst != filepath.Join("..", "guests", id) || !idRE.MatchString(id) {
		return Instance{}, false, fmt.Errorf("%s points to %q, not ../guests/<id>; fix or remove it", link, dst)
	}
	return Instance{Name: name, ID: id, d: d}, true, nil
}

// Assign gives name a new ID (replacing an old link, whose state is left
// for "guest prune").
func (d Dirs) Assign(name string) (Instance, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return Instance{}, err
	}
	inst := Instance{Name: name, ID: hex.EncodeToString(b), d: d}
	if err := os.MkdirAll(inst.StateDir(), 0o700); err != nil {
		return Instance{}, err
	}
	if err := os.MkdirAll(d.namesDir(), 0o700); err != nil {
		return Instance{}, err
	}
	tmp := filepath.Join(d.namesDir(), "."+name+".new")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("..", "guests", inst.ID), tmp); err != nil {
		return Instance{}, err
	}
	return inst, os.Rename(tmp, filepath.Join(d.namesDir(), name))
}

// Remove deletes the instance's state and its name link.
func (d Dirs) Remove(inst Instance) error {
	if !idRE.MatchString(inst.ID) {
		return fmt.Errorf("refusing to remove guest state with ID %q", inst.ID)
	}
	if err := removeAll(inst.StateDir()); err != nil {
		return err
	}
	os.RemoveAll(inst.RunDir())
	link := filepath.Join(d.namesDir(), inst.Name)
	if dst, err := os.Readlink(link); err == nil && filepath.Base(dst) == inst.ID {
		return os.Remove(link)
	}
	return nil
}

// InstanceOf returns the instance of name with a known ID (a journal
// records it while the name link is in the trash).
func (d Dirs) InstanceOf(name, id string) Instance { return Instance{Name: name, ID: id, d: d} }

// TrashDir holds what "guest rm" moved aside until it succeeds.
func (d Dirs) TrashDir(name string) string { return filepath.Join(d.StateRoot, "trash", name) }

// TrashInstance moves the instance's state and name link to the trash.
// Moving an instance that is already there does nothing.
func (d Dirs) TrashInstance(inst Instance) error {
	if !idRE.MatchString(inst.ID) {
		return fmt.Errorf("refusing to move guest state with ID %q", inst.ID)
	}
	trash := d.TrashDir(inst.Name)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	if err := renameIfExists(inst.StateDir(), filepath.Join(trash, inst.ID)); err != nil {
		return err
	}
	link := filepath.Join(d.namesDir(), inst.Name)
	if dst, err := os.Readlink(link); err == nil && filepath.Base(dst) == inst.ID {
		if err := os.Rename(link, filepath.Join(trash, "name")); err != nil {
			return err
		}
	}
	os.RemoveAll(inst.RunDir())
	return nil
}

// RestoreInstance moves a trashed instance back. Restoring an instance
// that is not in the trash does nothing.
func (d Dirs) RestoreInstance(inst Instance) error {
	trash := d.TrashDir(inst.Name)
	if err := renameIfExists(filepath.Join(trash, inst.ID), inst.StateDir()); err != nil {
		return err
	}
	if err := os.MkdirAll(d.namesDir(), 0o700); err != nil {
		return err
	}
	return renameIfExists(filepath.Join(trash, "name"), filepath.Join(d.namesDir(), inst.Name))
}

// EmptyTrash deletes what "guest rm" moved aside for name.
func (d Dirs) EmptyTrash(name string) error { return removeAll(d.TrashDir(name)) }

func renameIfExists(from, to string) error {
	if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// removeAll deletes dir, including the guest store, whose directories Nix
// makes read-only.
func removeAll(dir string) error {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// RemoveDir deletes a guest state directory (see Orphans).
func RemoveDir(dir string) error { return removeAll(dir) }

// Orphans lists state directories no name links to.
func (d Dirs) Orphans() ([]string, error) {
	used := map[string]bool{}
	links, _ := os.ReadDir(d.namesDir())
	for _, e := range links {
		if dst, err := os.Readlink(filepath.Join(d.namesDir(), e.Name())); err == nil {
			used[filepath.Base(dst)] = true
		}
	}
	dirs, err := os.ReadDir(d.guestsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, e := range dirs {
		if idRE.MatchString(e.Name()) && !used[e.Name()] {
			out = append(out, filepath.Join(d.guestsDir(), e.Name()))
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
func (d Dirs) Lock(name string) (func(), error) {
	return d.LockWithTimeout(name, fmt.Sprintf("guest %q", name), defaultTimeout(defaultLockTimeout))
}

// LockNixBuild serializes Nix builds across antic processes so that
// concurrent builds don't collide on the Nix eval-cache or store lock.
// Times out after defaultNixBuildTimeout.
func (d Dirs) LockNixBuild() (func(), error) {
	return d.LockWithTimeout("nixbuild", "Nix build", defaultTimeout(defaultNixBuildTimeout))
}

// LockWithReason takes an exclusive lock under StateRoot/locks/.
// If the lock is held by another process, it notifies the user on stderr
// and waits until the lock is acquired or defaultLockTimeout expires.
func (d Dirs) LockWithReason(name, description string) (func(), error) {
	return d.LockWithTimeout(name, description, defaultTimeout(defaultLockTimeout))
}

// LockWithTimeout takes an exclusive lock under StateRoot/locks/, waiting up to timeout.
func (d Dirs) LockWithTimeout(name, description string, timeout time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.LockWithContext(ctx, name, description, timeout)
}

// LockWithContext takes an exclusive lock under StateRoot/locks/.
// If the lock is held by another process, it notifies the user on stderr
// and polls until acquired or ctx is cancelled / timeout expires.
func (d Dirs) LockWithContext(ctx context.Context, name, description string, timeout time.Duration) (func(), error) {
	dir := filepath.Join(d.StateRoot, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
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

	holder := readLockPID(lockPath)
	holderMsg := "another process"
	if holder != "" {
		holderMsg = holder
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
				if h := readLockPID(lockPath); h != "" {
					holderMsg = h
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

func readLockPID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	pid, err := strconv.Atoi(s)
	if err == nil && pid > 0 {
		return fmt.Sprintf("PID %d", pid)
	}
	return s
}
