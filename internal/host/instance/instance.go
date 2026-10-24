// Package instance manages guest instances on the host: their IDs (a name
// maps to a random ID when the guest is created), their directories and a
// per-guest lock. Where things live:
//
//	~/.config/antic/antic.toml          config
//	~/.local/state/antic/
//	  guests/<ID>/                      one guest: store/ (its /nix), log/
//	  names/<NAME> -> ../guests/<ID>    which ID a name has
//	  locks/<NAME>.lock                 serializes lifecycle operations
//	$XDG_RUNTIME_DIR/antic/<ID>/        vm/ (cloud-hypervisor), sock/<helper>/, boot.json
package instance

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

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
	SrcDir    string
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
		SrcDir:    filepath.Join(home, ".local/src/antic"),
	}, nil
}

func (d Dirs) ConfigFile() string { return filepath.Join(d.ConfigDir, "antic.toml") }
func (d Dirs) guestsDir() string  { return filepath.Join(d.StateRoot, "guests") }
func (d Dirs) namesDir() string   { return filepath.Join(d.StateRoot, "names") }

// Instance is a guest's identity and its directories.
type Instance struct {
	Name, ID string
	d        Dirs
}

func (i Instance) StateDir() string { return filepath.Join(i.d.guestsDir(), i.ID) }
func (i Instance) StoreDir() string { return filepath.Join(i.StateDir(), "store") } // the guest's /nix lives in store/nix
func (i Instance) LogDir() string   { return filepath.Join(i.StateDir(), "log") }
func (i Instance) Console() string  { return filepath.Join(i.LogDir(), "console.log") }
func (i Instance) RunDir() string   { return filepath.Join(i.d.RunRoot, i.ID) }
func (i Instance) VMDir() string    { return filepath.Join(i.RunDir(), "vm") }   // cloud-hypervisor's own sockets
func (i Instance) SockDir() string  { return filepath.Join(i.RunDir(), "sock") } // helpers' sockets, one directory each
func (i Instance) BootFile() string { return filepath.Join(i.RunDir(), "boot.json") }

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

// Lock serializes lifecycle operations on guest name across antic
// processes (AI agents run tools in parallel). Unlock by calling the
// returned function.
func (d Dirs) Lock(name string) (func(), error) {
	dir := filepath.Join(d.StateRoot, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
