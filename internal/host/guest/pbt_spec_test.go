package guest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

// Property-based differential tests of the shell's own logic against the
// specification, the part neither the formal models nor the core cover:
// the config file edits (config.Draft on real TOML files, behind the share
// policy) and the decision whether a running guest takes an edit live
// (liveShares). Random command sequences run on both, and after each the
// shell must accept exactly what the specification accepts and leave a
// config file that means the specification's config.

type fixture struct {
	home string
	env  spec.Env
	cfg  *config.Config
	name domain.GuestName
}

func abs(t interface{ Fatal(...any) }, p string) domain.AbsPath {
	a, err := domain.ParseAbsPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// newFixture makes a home with directories A, A/sub and B, a link LB to B
// and a file F, and an empty config directory.
func newFixture(t *testing.T) fixture {
	home := t.TempDir()
	for _, d := range []string{"A/sub", "B", ".config/antic/guests"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "B"), filepath.Join(home, "LB")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "F"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := spec.Env{
		Home:  abs(t, home),
		Dirs:  []domain.AbsPath{abs(t, home+"/A"), abs(t, home+"/A/sub"), abs(t, home+"/B")},
		Links: []spec.Link{{From: abs(t, home+"/LB"), To: abs(t, home+"/B")}},
	}
	name, _ := domain.ParseGuestName("e2e-a")
	return fixture{home: home, env: env, name: name}
}

// specConfig is what a loaded config file means in the specification.
func (f fixture) specConfig(g *config.Guest) (spec.Config, error) {
	raw := spec.RawConfig{Packages: g.Packages, CPUs: g.CPUs, Memory: g.Memory, Network: spec.RawNetwork{
		Mode: g.Network.Mode, Default: g.Network.Default, Allow: g.Network.Allow, Deny: g.Network.Deny}}
	for _, p := range g.Ports {
		raw.Ports = append(raw.Ports, spec.RawPort{Host: p.Host, Guest: p.Guest, Proto: p.Proto})
	}
	for _, s := range g.Shares {
		mode := "rw"
		if s.ReadOnly {
			mode = "ro"
		}
		raw.Shares = append(raw.Shares, spec.RawShare{Path: realPath(s.Path), Mode: mode})
	}
	return spec.ParseConfig(f.env, raw)
}

// shellCmd runs c on the config file as the shell does for a stopped guest.
func (f fixture) shellCmd(c spec.Cmd) error {
	g := f.cfg.Guests[f.name.Get()]
	d, err := config.ReadDraft(g, f.home)
	if err != nil {
		return err
	}
	switch c := c.(type) {
	case spec.CmdMount:
		for _, dir := range c.Dirs {
			real, err := share.Policy{Home: f.home}.Resolve(dir)
			if err != nil {
				return err
			}
			if d, err = d.AddShare(real, c.ReadOnly); err != nil {
				return err
			}
		}
	case spec.CmdUnmount:
		for _, dir := range c.Dirs {
			if d, err = d.RemoveShare(realPath(dir)); err != nil {
				return err
			}
		}
	case spec.CmdForward:
		var ports []config.Port
		for _, s := range c.Ports {
			p, err := config.ParsePort(s)
			if err != nil {
				return err
			}
			if p.Proto == "" {
				p.Proto = c.Proto
			}
			ports = append(ports, p)
		}
		if d, err = d.AddPorts(ports, nil); err != nil {
			return err
		}
	case spec.CmdUnforward:
		var hosts []int
		for _, s := range c.Ports {
			p, err := config.ParsePort(s)
			if err != nil {
				return err
			}
			hosts = append(hosts, p.Host)
		}
		if d, err = d.RemovePorts(hosts); err != nil {
			return err
		}
	case spec.CmdSet:
		var u config.ResourceUpdates
		if v, ok := c.CPUs.Get(); ok {
			u.CPUs = &v
		}
		if v, ok := c.Memory.Get(); ok {
			u.Memory = &v
		}
		if v, ok := c.Network.Get(); ok {
			u.Network = &v
		}
		if d, err = d.SetResources(u); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported %T", c)
	}
	return d.Write(false)
}

func genEdit(t *rapid.T, home string) spec.Cmd {
	n := "e2e-a"
	dir := func(label string) string {
		return filepath.Join(home, rapid.SampledFrom([]string{"A", "A/sub", "B", "LB", "F", "missing", "."}).Draw(t, label))
	}
	port := func(label string) string {
		return rapid.SampledFrom([]string{"8001", "8002", "8001:80", "53/udp", "70000", "x"}).Draw(t, label)
	}
	switch rapid.IntRange(0, 4).Draw(t, "kind") {
	case 0:
		return spec.CmdMount{Target: n, Dirs: []string{dir("dir")}, ReadOnly: rapid.Bool().Draw(t, "ro")}
	case 1:
		return spec.CmdUnmount{Target: n, Dirs: []string{dir("dir")}}
	case 2:
		return spec.CmdForward{Target: n, Ports: []string{port("port")}, Proto: rapid.SampledFrom([]string{"", "udp", "tcp,udp"}).Draw(t, "proto")}
	case 3:
		return spec.CmdUnforward{Target: n, Ports: []string{port("port")}}
	}
	var c spec.CmdSet
	c.Target = n
	if rapid.Bool().Draw(t, "setCPUs") {
		c.CPUs = types.Some(rapid.SampledFrom([]int{1, 2, 4, 0, 999}).Draw(t, "cpus"))
	}
	if rapid.Bool().Draw(t, "setMemory") {
		c.Memory = types.Some(rapid.SampledFrom([]string{"1G", "2G", "512M", "lots"}).Draw(t, "memory"))
	}
	if rapid.Bool().Draw(t, "setNetwork") {
		c.Network = types.Some(rapid.SampledFrom([]string{"internet", "lan", "none", "wifi"}).Draw(t, "network"))
	}
	return c
}

// TestPBTDraftEditsRefineSpec: on a stopped guest, the shell's config edits
// accept and reject as the specification does and leave the config it says.
func TestPBTDraftEditsRefineSpec(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".config/antic/guests")
	rapid.Check(t, func(rt *rapid.T) {
		os.RemoveAll(filepath.Join(dir, "e2e-a.toml"))
		c, err := config.Load(dir, f.home)
		if err != nil {
			rt.Fatal(err)
		}
		if err := c.CreateGuest("e2e-a", config.Guest{Packages: []string{"git"}, Memory: "1G"}, f.home); err != nil {
			rt.Fatal(err)
		}
		if f.cfg, err = config.Load(dir, f.home); err != nil {
			rt.Fatal(err)
		}
		start, err := f.specConfig(f.cfg.Guests["e2e-a"])
		if err != nil {
			rt.Fatal(err)
		}
		w := spec.World{Guests: types.BuildSet[spec.Guest, domain.GuestName, spec.GuestByName](
			[]spec.Guest{{Name: f.name, Config: start, Phase: spec.PhaseOff}})}
		for i := range rapid.IntRange(1, 12).Draw(rt, "length") {
			cmd := genEdit(rt, f.home)
			outs := spec.Post(w, f.env, cmd)
			if len(outs) != 1 {
				rt.Fatalf("step %d %#v: %d outcomes for a stopped guest", i, cmd, len(outs))
			}
			shellErr := f.shellCmd(cmd)
			if (shellErr == nil) != (outs[0].Kind == spec.OutcomeOk) {
				rt.Fatalf("step %d %#v: shell error %v, spec %v (%s)", i, cmd, shellErr, outs[0].Kind, outs[0].Reason)
			}
			w = outs[0].World
			if f.cfg, err = config.Load(dir, f.home); err != nil {
				rt.Fatal(err)
			}
			got, err := f.specConfig(f.cfg.Guests["e2e-a"])
			if err != nil {
				rt.Fatalf("step %d: the file the shell left does not load in the spec: %v", i, err)
			}
			want, _ := w.Guests.Lookup(f.name)
			if !got.Equal(want.Config) {
				rt.Fatalf("step %d %#v: the file means %+v, the spec says %+v", i, cmd, got, want.Config)
			}
		}
	})
}

// TestPBTLiveSharesRefineSpec: for a running guest with random boot and
// hot-plugged mounts, an edit of its shares is taken live (and with which
// hot-plugs and detaches) exactly when the specification's edit keeps the
// guest running with those mounts instead of restarting it.
func TestPBTLiveSharesRefineSpec(t *testing.T) {
	f := newFixture(t)
	paths := []string{f.home + "/A", f.home + "/A/sub", f.home + "/B"}
	rapid.Check(t, func(rt *rapid.T) {
		var cfgShares, newShares []spec.RawShare
		var mounts []spec.Mount
		bt := &boot{}
		mode := func(label string) string { return rapid.SampledFrom([]string{"rw", "ro"}).Draw(rt, label) }
		for _, p := range paths {
			if rapid.Bool().Draw(rt, "configured") {
				cfgShares = append(cfgShares, spec.RawShare{Path: p, Mode: mode("cmode")})
			}
			if rapid.Bool().Draw(rt, "wanted") {
				newShares = append(newShares, spec.RawShare{Path: p, Mode: mode("nmode")})
			}
		}
		base, err := spec.ParseConfig(f.env, spec.RawConfig{Packages: []string{"git"}, Shares: cfgShares})
		if err != nil {
			rt.Skip("overlapping configured shares")
		}
		next, err := spec.ParseConfig(f.env, spec.RawConfig{Packages: []string{"git"}, Shares: newShares})
		if err != nil {
			rt.Skip("overlapping new shares")
		}
		// Mounts: every configured share is mounted (at boot or hot-plugged),
		// plus possibly a lingering boot mount of an unconfigured one.
		for _, s := range base.Shares.All() {
			origin := rapid.SampledFrom([]spec.MountOrigin{spec.OriginBoot, spec.OriginRuntime}).Draw(rt, "origin")
			mounts = append(mounts, spec.Mount{Path: s.Path, Mode: s.Mode, Origin: origin})
		}
		for _, p := range paths {
			pa := abs(rt, p)
			if base.Shares.Contains(pa) || !rapid.Bool().Draw(rt, "lingering") {
				continue
			}
			overlaps := false
			for _, m := range mounts {
				overlaps = overlaps || domain.PathsOverlap(m.Path, pa)
			}
			if !overlaps {
				mounts = append(mounts, spec.Mount{Path: pa, Mode: spec.ModeRW, Origin: spec.OriginBoot})
			}
		}
		for _, m := range mounts {
			bt.Shares = append(bt.Shares, bootShare{Share: config.Share{Path: m.Path.Get(), ReadOnly: m.Mode == spec.ModeRO},
				Tag: "t", Runtime: m.Origin == spec.OriginRuntime})
		}
		g := spec.Guest{Name: f.name, Config: base, Phase: spec.PhaseRunning,
			Mounts: types.BuildSet[spec.Mount, domain.AbsPath, spec.MountByPath](mounts)}
		w := spec.World{Guests: types.BuildSet[spec.Guest, domain.GuestName, spec.GuestByName]([]spec.Guest{g})}
		if err := spec.Invariant(w); err != nil {
			rt.Skip(err.Error())
		}
		outs := spec.Post(w, f.env, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Packages: []string{"git"}, Shares: newShares}})
		var after spec.Guest
		for _, o := range outs {
			if o.Kind == spec.OutcomeOk {
				after, _ = o.World.Guests.Lookup(f.name)
			}
		}
		if after.Name != f.name {
			rt.Fatalf("the spec rejects the edit: %+v", outs)
		}
		cg := &config.Guest{}
		for _, s := range next.Shares.All() {
			cg.Shares = append(cg.Shares, config.Share{Path: s.Path.Get(), ReadOnly: s.Mode == spec.ModeRO})
		}
		added, removed, live := liveShares(bt, cg)
		// The mounts the shell leaves: live, the old ones minus the detached
		// plus the hot-plugged; otherwise it restarts with the new shares
		// mounted at boot.
		var got []spec.Mount
		if live {
			for _, m := range mounts {
				if !slices.Contains(removed, m.Path.Get()) {
					got = append(got, m)
				}
			}
			for _, p := range added {
				s, _ := next.Shares.Lookup(abs(rt, p))
				got = append(got, spec.Mount{Path: s.Path, Mode: s.Mode, Origin: spec.OriginRuntime})
			}
		} else {
			for _, s := range next.Shares.All() {
				got = append(got, spec.Mount{Path: s.Path, Mode: s.Mode, Origin: spec.OriginBoot})
			}
		}
		gotSet := types.BuildSet[spec.Mount, domain.AbsPath, spec.MountByPath](got)
		if !slices.Equal(gotSet.All(), after.Mounts.All()) {
			rt.Fatalf("mounts %+v, new shares %+v: the shell (live=%v) leaves %+v, the spec %+v",
				mounts, newShares, live, gotSet.All(), after.Mounts.All())
		}
	})
}
