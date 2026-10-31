package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/RNG999/antic/internal/domain"
)

// Draft is a validated new content of one guest's config file that has not
// been written yet: commands read the file under the guest's lock, plan
// from it, and write Old or New as steps of their saga (EffectWriteConfig).
type Draft struct {
	// Path is the config file (a guests/<name>.toml or a project antic.toml).
	Path string
	Name string
	Old  []byte
	New  []byte
	// Absent means Old is "no file" (a guest being created).
	Absent bool
	// Before and After are Old and New parsed.
	Before *Guest
	After  *Guest
	home   string
}

// Changed reports whether the draft rewrites the file.
func (d Draft) Changed() bool { return d.Absent || !bytes.Equal(d.Old, d.New) }

// Write atomically writes New, or Old when old is set. Writing either twice
// is harmless (compensation and recovery may repeat it).
func (d Draft) Write(old bool) error {
	content := d.New
	if old {
		if d.Absent {
			if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		content = d.Old
	}
	return writeFileAtomic(RealPath(d.Path), content)
}

// ReadDraft reads the current config file of guest g, to edit with the
// Draft methods. It must be called with the guest's lock held.
func ReadDraft(g *Guest, home string) (Draft, error) {
	real, err := filepath.EvalSymlinks(g.Path)
	if err != nil {
		return Draft{}, err
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return Draft{}, err
	}
	d := Draft{Path: g.Path, Name: g.Name, Old: b, New: b, home: home}
	if d.Before, err = d.parse(b); err != nil {
		return Draft{}, err
	}
	d.After = d.Before
	return d, nil
}

// RestoreDraft rebuilds the draft a journal recorded. A content that no
// longer loads (or an absent file) has no parsed guest.
func RestoreDraft(path, name, home string, absent bool, old, new []byte) Draft {
	d := Draft{Path: path, Name: name, Old: old, New: new, Absent: absent, home: home}
	if !absent {
		d.Before, _ = d.parse(old)
	}
	d.After, _ = d.parse(new)
	return d
}

// ReadRawDraft reads a config file's bytes without loading it (an unchanged
// draft, for commands that move the file rather than edit it).
func ReadRawDraft(path, name, home string) (Draft, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Draft{}, err
	}
	return Draft{Path: path, Name: name, Old: b, New: b, home: home}, nil
}

// project reports whether the draft is a project antic.toml (whose name is
// in the file and whose relative paths are relative to it).
func (d Draft) project() bool { return filepath.Base(d.Path) == "antic.toml" }

// parse validates content as this draft's file by loading it from a
// temporary file next to it (so relative paths resolve the same way).
func (d Draft) parse(content []byte) (*Guest, error) {
	tmp, err := os.CreateTemp(filepath.Dir(RealPath(d.Path)), "."+d.Name+".toml.*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(content)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return nil, err
	}
	var g *Guest
	if d.project() {
		g, err = loadProjectGuest(tmp.Name(), d.home)
	} else {
		g, err = loadGuest(tmp.Name(), d.Name, d.home)
	}
	if err != nil {
		return nil, fmt.Errorf("refusing to write %s, the result would not load: %w", d.Path, err)
	}
	g.Path = d.Path
	return g, nil
}

// edit returns the draft with change applied to its new content.
func (d Draft) edit(change func([]string) ([]string, error)) (Draft, error) {
	lines, err := change(strings.Split(strings.TrimRight(string(d.New), "\n"), "\n"))
	if err != nil {
		return d, err
	}
	content := []byte(strings.Join(lines, "\n") + "\n")
	after, err := d.parse(content)
	if err != nil {
		return d, err
	}
	d.New, d.After = content, after
	return d, nil
}

// RealPath returns p with its symlinks resolved, or p itself when they
// cannot be (p does not exist yet, say). Share paths and config paths are
// compared and stored resolved.
func RealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// ShareAt returns the configured share whose directory is real (a resolved
// path), comparing resolved paths rather than how the file spells them.
func (g *Guest) ShareAt(real string) (Share, bool) {
	for _, s := range g.Shares {
		if s.Path != "" && RealPath(s.Path) == real {
			return s, true
		}
	}
	return Share{}, false
}

// AddShare adds a [[shares]] entry for the resolved directory real. Sharing
// a directory again with the same mode changes nothing; with another mode
// it is an error (the mount would keep its old mode until a restart).
func (d Draft) AddShare(real string, ro bool) (Draft, error) {
	if have, ok := d.After.ShareAt(real); ok {
		if have.ReadOnly != ro {
			return d, fmt.Errorf("%s is already shared %s with %s", real, modeName(have.ReadOnly), d.Name)
		}
		return d, nil
	}
	for _, s := range d.After.Shares {
		if s.Path != "" && (within(real, RealPath(s.Path)) || within(RealPath(s.Path), real)) {
			return d, fmt.Errorf("%s overlaps share %s", real, s.Path)
		}
	}
	mode := "rw"
	if ro {
		mode = "ro"
	}
	return d.edit(func(lines []string) ([]string, error) {
		return append(lines, "", "[[shares]]",
			"source = "+tomlString(tildePath(real, d.home)),
			"mode = "+tomlString(mode)), nil
	})
}

// RemoveShare removes the [[shares]] entry for the resolved directory real.
func (d Draft) RemoveShare(real string) (Draft, error) {
	if _, ok := d.After.ShareAt(real); !ok {
		return d, fmt.Errorf("%s is not a configured share of %s", real, d.Name)
	}
	base := filepath.Dir(d.Path)
	return d.edit(func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "shares"
		}, func(body []string) bool {
			var s Share
			if _, err := toml.Decode(strings.Join(body, "\n"), &s); err != nil {
				return false
			}
			dir := ""
			if d.project() {
				dir = base
			}
			p, _ := resolvePath(s.Source, d.home, dir, "", nil)
			return RealPath(p) == real
		})
		if removed == 0 {
			return nil, fmt.Errorf("%s is not a configured share of %s", real, d.Name)
		}
		return out, nil
	})
}

// Replace replaces the whole file with content, which must load.
func (d Draft) Replace(content []byte) (Draft, error) {
	after, err := d.parse(content)
	if err != nil {
		return d, err
	}
	d.New, d.After = content, after
	return d, nil
}

// SetPackages sets the guest's packages.
func (d Draft) SetPackages(pkgs []string) (Draft, error) {
	return d.edit(func(lines []string) ([]string, error) {
		return setRootKey(lines, "packages", tomlArray(pkgs)), nil
	})
}

// AddPorts adds port forwards, rejecting conflicts with the guest's own
// forwards, with each other and with others (other guests' forwards). An
// identical forward changes nothing.
func (d Draft) AddPorts(ports []Port, others []Port) (Draft, error) {
	have := slices.Clone(d.After.Ports)
	var add []Port
	for _, p := range ports {
		same := func(q Port) bool { return q.Host == p.Host && q.Guest == p.Guest && q.ProtoStr() == p.ProtoStr() }
		if slices.ContainsFunc(have, same) {
			continue
		}
		for _, q := range append(slices.Clone(have), others...) {
			if q.Host == p.Host && ((p.TCP() && q.TCP()) || (p.UDP() && q.UDP())) {
				return d, fmt.Errorf("host port %d is already forwarded", p.Host)
			}
		}
		have, add = append(have, p), append(add, p)
	}
	if len(add) == 0 {
		return d, nil
	}
	return d.edit(func(lines []string) ([]string, error) {
		for _, p := range add {
			lines = append(lines, "", "[[network.ports]]",
				fmt.Sprintf("host = %d", p.Host),
				fmt.Sprintf("guest = %d", p.Guest))
			if p.Proto != "" && p.Proto != "tcp" {
				lines = append(lines, "proto = "+tomlString(p.Proto))
			}
			if p.Description != "" {
				lines = append(lines, "description = "+tomlString(p.Description))
			}
		}
		return lines, nil
	})
}

// RemovePorts removes the forwards of the given host ports.
func (d Draft) RemovePorts(hosts []int) (Draft, error) {
	for _, h := range hosts {
		if !slices.ContainsFunc(d.After.Ports, func(p Port) bool { return p.Host == h }) {
			return d, fmt.Errorf("port %d is not configured on %s", h, d.Name)
		}
	}
	return d.edit(func(lines []string) ([]string, error) {
		out, _ := removeBlocks(lines, func(h string) bool {
			return h == "ports" || h == "network.ports"
		}, func(body []string) bool {
			var p Port
			if _, err := toml.Decode(strings.Join(body, "\n"), &p); err != nil {
				return false
			}
			return slices.Contains(hosts, p.Host)
		})
		return out, nil
	})
}

// SetResources updates the guest's cpus, memory, and network mode.
func (d Draft) SetResources(u ResourceUpdates) (Draft, error) {
	if u.CPUs == nil && u.Memory == nil && u.Network == nil {
		return d, errors.New("specify at least one of --cpus, --memory, or --network")
	}
	// A requested count is checked as given: the file's "cpus = 0" means
	// the default, so writing 0 would report success for 2 CPUs (D19).
	if u.CPUs != nil {
		if _, err := domain.ParseCPUCount(*u.CPUs); err != nil {
			return d, fmt.Errorf("cpus must be between 1 and 256: %w", err)
		}
	}
	return d.edit(func(lines []string) ([]string, error) {
		if u.CPUs != nil {
			lines = setTableKey(lines, "spec", "cpus", strconv.Itoa(*u.CPUs))
		}
		if u.Memory != nil {
			lines = setTableKey(lines, "spec", "memory", tomlString(*u.Memory))
		}
		if u.Network != nil {
			lines = setTableKey(lines, "network", "mode", tomlString(*u.Network))
		}
		return lines, nil
	})
}

// NeedsRestart reports whether a running guest must reboot to take the
// change from a to b: its resources, network policy or port forwards differ.
func NeedsRestart(a, b *Guest) bool {
	return a.CPUs != b.CPUs || a.MemoryBytes != b.MemoryBytes ||
		a.Network.Mode != b.Network.Mode || a.Network.Default != b.Network.Default ||
		!slices.Equal(a.AllowDomains, b.AllowDomains) || !slices.Equal(a.AllowNets, b.AllowNets) ||
		!slices.Equal(a.DenyDomains, b.DenyDomains) || !slices.Equal(a.DenyNets, b.DenyNets) ||
		!slices.EqualFunc(a.Ports, b.Ports, func(p, q Port) bool {
			return p.Host == q.Host && p.Guest == q.Guest && p.ProtoStr() == q.ProtoStr()
		})
}

func modeName(ro bool) string {
	if ro {
		return "read-only"
	}
	return "read-write"
}

// writeFileAtomic replaces real with content, keeping its permissions.
func writeFileAtomic(real string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(real), "."+filepath.Base(real)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(content)
	if err := errors.Join(werr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), real)
}
