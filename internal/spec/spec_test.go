package spec_test

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

const (
	home = "/home/u"
	dirA = "/home/u/w/A"
	dirB = "/home/u/w/B"
	sub  = "/home/u/w/A/sub"
	link = "/home/u/w/LB" // -> dirB
)

func mustAbs(t testing.TB, s string) domain.AbsPath {
	t.Helper()
	p, err := domain.ParseAbsPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustName(t testing.TB, s string) domain.GuestName {
	t.Helper()
	n, err := domain.ParseGuestName(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustPort(t testing.TB, n int) domain.PortNumber {
	t.Helper()
	p, err := domain.ParsePortNumber(n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustPackage(t testing.TB, s string) domain.PackageName {
	t.Helper()
	p, err := domain.ParsePackageName(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureEnv(t testing.TB) spec.Env {
	return spec.Env{
		Home:        mustAbs(t, home),
		Dirs:        []domain.AbsPath{mustAbs(t, dirA), mustAbs(t, dirB), mustAbs(t, sub), mustAbs(t, "/home/u/.ssh"), mustAbs(t, "/srv")},
		Links:       []spec.Link{{From: mustAbs(t, link), To: mustAbs(t, dirB)}},
		Denied:      []domain.AbsPath{mustAbs(t, "/home/u/.ssh")},
		Busy:        []spec.PortKey{{Host: mustPort(t, 9999), Proto: spec.ProtoTCP}},
		Unbuildable: []domain.PackageName{mustPackage(t, "no-such-pkg")},
	}
}

// fixtureWorld has one external guest ("dev", forwarding 7000/tcp) and no
// managed guests.
func fixtureWorld(t testing.TB) spec.World {
	ports := types.BuildSet[spec.Port, spec.PortKey, spec.PortByHost]([]spec.Port{
		{Host: mustPort(t, 7000), Guest: mustPort(t, 7000), Proto: spec.ProtoTCP},
	})
	return spec.World{
		External: types.BuildSet[spec.External, domain.GuestName, spec.ExternalByName]([]spec.External{
			{Name: mustName(t, "dev"), Phase: spec.PhaseOff, Ports: ports},
		}),
	}
}

// alphabet is a small but adversarial set of commands over the fixture.
func alphabet() []spec.Cmd {
	var cs []spec.Cmd
	for _, n := range []string{"e2e-a", "e2e-b"} {
		cs = append(cs,
			spec.CmdCreate{Name: n},
			spec.CmdCreate{Name: n, Packages: []string{"jq"}, Ports: []string{"8001"}},
			spec.CmdApply{Target: n},
			spec.CmdInstall{Target: n, Packages: []string{"jq"}},
			spec.CmdUninstall{Target: n, Packages: []string{"jq"}},
			spec.CmdSet{Target: n, CPUs: types.Some(4)},
			spec.CmdSet{Target: n, Network: types.Some("none")},
			spec.CmdForward{Target: n, Ports: []string{"8001"}},
			spec.CmdForward{Target: n, Ports: []string{"8002:80/udp"}},
			spec.CmdUnforward{Target: n, Ports: []string{"8001"}},
			spec.CmdStart{Names: []string{n}},
			spec.CmdStop{Names: []string{n}},
			spec.CmdRestart{Names: []string{n}},
			spec.CmdRm{Names: []string{n}, Yes: true},
			spec.CmdMount{Target: n, Dirs: []string{dirA}},
			spec.CmdMount{Target: n, Dirs: []string{dirA}, ReadOnly: true},
			spec.CmdMount{Target: n, Dirs: []string{sub, link}},
			spec.CmdUnmount{Target: n, Dirs: []string{dirA}},
			spec.CmdUnmount{Target: n, Dirs: []string{link}},
			spec.CmdEdit{Target: n, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 2, Shares: []spec.RawShare{{Path: dirB, Mode: "ro"}}}},
			spec.CmdEdit{Target: n, New: spec.RawConfig{Network: spec.RawNetwork{Default: "deny", Allow: []string{"one.one.one.one", "192.168.1.0/24"}}}},
			spec.CmdRun{Target: n, Code: 42},
			spec.CmdSpawn{Target: n, Command: "sleep 600"},
			spec.CmdShell{Target: n, Code: 7},
			spec.CmdInspect{What: spec.InspectShow, Target: n},
			spec.CmdInspect{What: spec.InspectLogs, Target: n},
			spec.CmdKill{Target: n, Commands: []string{"sleep 600"}},
		)
	}
	return append(cs,
		spec.CmdInspect{What: spec.InspectLs},
		spec.CmdStart{Names: []string{"e2e-a", "e2e-b"}},
		spec.CmdStop{},
		spec.CmdCreate{Name: "Bad_Name"},
		spec.CmdInstall{Target: "e2e-a", Packages: []string{"bad pkg"}},
		spec.CmdInstall{Target: "e2e-a", Packages: []string{"no-such-pkg"}},
		spec.CmdForward{Target: "e2e-a", Ports: []string{"7000"}},
		spec.CmdForward{Target: "e2e-a", Ports: []string{"9999"}},
		spec.CmdMount{Target: "e2e-a", Dirs: []string{"/home/u/.ssh"}},
		spec.CmdMount{Target: "e2e-a", Dirs: []string{"/srv"}},
		spec.CmdRm{Names: []string{"e2e-a"}},
	)
}

// targets returns the managed guests c may change, or nil for "any".
func targets(c spec.Cmd) []string {
	switch c := c.(type) {
	case spec.CmdCreate:
		return []string{c.Name}
	case spec.CmdApply:
		return []string{c.Target}
	case spec.CmdInstall:
		return []string{c.Target}
	case spec.CmdUninstall:
		return []string{c.Target}
	case spec.CmdSet:
		return []string{c.Target}
	case spec.CmdForward:
		return []string{c.Target}
	case spec.CmdUnforward:
		return []string{c.Target}
	case spec.CmdMount:
		return []string{c.Target}
	case spec.CmdUnmount:
		return []string{c.Target}
	case spec.CmdEdit:
		return []string{c.Target}
	case spec.CmdRun:
		return []string{c.Target}
	case spec.CmdSpawn:
		return []string{c.Target}
	case spec.CmdShell:
		return []string{c.Target}
	case spec.CmdInspect:
		return []string{c.Target}
	case spec.CmdKill:
		return []string{c.Target}
	case spec.CmdStart:
		return c.Names
	case spec.CmdStop:
		return c.Names
	case spec.CmdRestart:
		return c.Names
	case spec.CmdRm:
		return c.Names
	}
	return nil
}

func key(w spec.World) string { return fmt.Sprintf("%v", w) }

// checkStep verifies the properties every Post result must have.
func checkStep(t *testing.T, w spec.World, c spec.Cmd, outs []spec.Outcome) {
	t.Helper()
	if len(outs) == 0 {
		t.Fatalf("Post is empty for in-model command %#v in %v", c, w)
	}
	last := outs[len(outs)-1]
	for i, o := range outs {
		if err := spec.Invariant(o.World); err != nil {
			t.Fatalf("%#v: outcome %d violates Invariant: %v", c, i, err)
		}
		// P5: external guests never change.
		if !(spec.World{External: o.World.External}).Equal(spec.World{External: w.External}) {
			t.Fatalf("%#v: outcome %d changed an external guest", c, i)
		}
		// P5: guests the command does not name never change.
		if ts := targets(c); len(ts) > 0 && ts[0] != "" {
			for _, g := range w.Guests.All() {
				named := false
				for _, n := range ts {
					named = named || n == g.Name.Get()
				}
				if after, ok := o.World.Guests.Lookup(g.Name); !named && (!ok || !after.Equal(g)) {
					t.Fatalf("%#v: outcome %d changed guest %s it does not name", c, i, g.Name.Get())
				}
			}
		}
		// A rejection is deterministic and final; environmental failures come
		// before the successes, of which there may be several (a running
		// guest may switch or restart, newSystem).
		if o.Kind == spec.OutcomeReject && i != len(outs)-1 {
			t.Fatalf("%#v: rejection is not the last outcome", c)
		}
		if o.Kind != spec.OutcomeOk && i > 0 && outs[i-1].Kind == spec.OutcomeOk {
			t.Fatalf("%#v: a failure follows a success", c)
		}
		if o.Kind != spec.OutcomeOk && o.Exit() != 125 {
			t.Fatalf("%#v: failure exits %d, not 125", c, o.Exit())
		}
	}
	// P1: a single-element command that fails leaves the World unchanged.
	if single(c) {
		for _, o := range outs {
			if o.Kind != spec.OutcomeOk && !o.World.Equal(w) {
				t.Fatalf("%#v: failed outcome changed the World (P1)", c)
			}
		}
	}
	if last.Kind == spec.OutcomeEnvFail {
		t.Fatalf("%#v: last outcome must be success or rejection", c)
	}
	// P5: an inspection changes nothing, whatever it reports.
	if _, inspect := c.(spec.CmdInspect); inspect {
		for _, o := range outs {
			if !o.World.Equal(w) {
				t.Fatalf("%#v changed the World", c)
			}
		}
	}
}

func single(c spec.Cmd) bool {
	switch c := c.(type) {
	case spec.CmdStart:
		return len(c.Names) <= 1
	case spec.CmdStop:
		return len(c.Names) <= 1
	case spec.CmdRestart:
		return len(c.Names) <= 1
	case spec.CmdRm:
		return len(c.Names) <= 1
	case spec.CmdMount:
		return len(c.Dirs) <= 1
	case spec.CmdUnmount:
		return len(c.Dirs) <= 1
	}
	return true
}

// TestExploreReachableWorlds explores every World reachable from the fixture
// in a bounded number of successful steps and checks each transition.
func TestExploreReachableWorlds(t *testing.T) {
	env := fixtureEnv(t)
	start := fixtureWorld(t)
	if err := spec.Invariant(start); err != nil {
		t.Fatal(err)
	}
	cmds := alphabet()
	seen := map[string]bool{key(start): true}
	frontier := []spec.World{start}
	steps := 0
	for _, depth := range []int{1, 2, 3, 4} {
		var next []spec.World
		for _, w := range frontier {
			for _, c := range cmds {
				outs := spec.Post(w, env, c)
				if outs == nil {
					continue
				}
				checkStep(t, w, c, outs)
				steps++
				for _, o := range outs {
					if k := key(o.World); !seen[k] {
						seen[k] = true
						next = append(next, o.World)
					}
				}
			}
		}
		t.Logf("depth %d: %d new worlds", depth, len(next))
		frontier = next
	}
	t.Logf("checked %d transitions over %d worlds", steps, len(seen))
	if len(seen) < 100 {
		t.Fatalf("exploration reached only %d worlds; the alphabet is too weak", len(seen))
	}
}

// run applies c and requires a single successful (or rejected) final outcome.
func run(t *testing.T, w spec.World, c spec.Cmd, want spec.OutcomeKind) spec.World {
	t.Helper()
	outs := spec.Post(w, fixtureEnv(t), c)
	checkStep(t, w, c, outs)
	last := outs[len(outs)-1]
	if last.Kind != want {
		t.Fatalf("%#v: got outcome %d (%s), want %d", c, last.Kind, last.Reason, want)
	}
	return last.World
}

func guest(t *testing.T, w spec.World, name string) spec.Guest {
	t.Helper()
	g, ok := w.Guests.Lookup(mustName(t, name))
	if !ok {
		t.Fatalf("guest %s does not exist", name)
	}
	return g
}

func TestScenarios(t *testing.T) {
	w0 := fixtureWorld(t)
	running := run(t, w0, spec.CmdCreate{Name: "e2e-a"}, spec.OutcomeOk)

	t.Run("create boots with defaults", func(t *testing.T) {
		g := guest(t, running, "e2e-a")
		if g.Phase != spec.PhaseRunning || g.Config.Packages.Len() != 1 || g.Config.Res.CPUs.Get() != 2 {
			t.Fatalf("unexpected guest %v", g)
		}
	})

	t.Run("D3 create with flags on an existing config is rejected", func(t *testing.T) {
		g := guest(t, running, "e2e-a")
		g.Phase, g.Mounts = spec.PhaseUncreated, spec.MountSet{}
		uncreated := running
		uncreated.Guests = uncreated.Guests.Upsert(g)
		run(t, uncreated, spec.CmdCreate{Name: "e2e-a", Packages: []string{"jq"}}, spec.OutcomeReject)
		after := run(t, uncreated, spec.CmdCreate{Name: "e2e-a"}, spec.OutcomeOk)
		if guest(t, after, "e2e-a").Phase != spec.PhaseRunning {
			t.Fatal("create without flags should build and boot the existing config")
		}
		// D6: stopping an uncreated guest succeeds and changes nothing.
		if !run(t, uncreated, spec.CmdStop{Names: []string{"e2e-a"}}, spec.OutcomeOk).Equal(uncreated) {
			t.Fatal("stop changed an uncreated guest")
		}
	})

	t.Run("D2 mount with a different mode is rejected", func(t *testing.T) {
		w := run(t, running, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk)
		run(t, w, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}, ReadOnly: true}, spec.OutcomeReject)
		if !run(t, w, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk).Equal(w) {
			t.Fatal("mounting again with the same mode must be a no-op (P4)")
		}
		run(t, w, spec.CmdMount{Target: "e2e-a", Dirs: []string{sub}}, spec.OutcomeReject)
	})

	t.Run("unmounting a boot-time share keeps it mounted until restart", func(t *testing.T) {
		w := run(t, running, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk)
		w = run(t, w, spec.CmdRestart{Names: []string{"e2e-a"}}, spec.OutcomeOk)
		w = run(t, w, spec.CmdUnmount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk)
		g := guest(t, w, "e2e-a")
		if g.Config.Shares.Len() != 0 || !g.Mounts.Contains(mustAbs(t, dirA)) {
			t.Fatalf("boot mount should linger: %v", g)
		}
		// Remounting with another mode must wait for the restart.
		run(t, w, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}, ReadOnly: true}, spec.OutcomeReject)
		w = run(t, w, spec.CmdRestart{Names: []string{"e2e-a"}}, spec.OutcomeOk)
		if guest(t, w, "e2e-a").Mounts.Len() != 0 {
			t.Fatal("restart should drop the unconfigured mount")
		}
	})

	t.Run("D5 a package that cannot be built changes nothing", func(t *testing.T) {
		run(t, running, spec.CmdInstall{Target: "e2e-a", Packages: []string{"jq", "no-such-pkg"}}, spec.OutcomeReject)
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Packages: []string{"no-such-pkg"}}}, spec.OutcomeReject)
	})

	t.Run("D7 host ports are unique across guests", func(t *testing.T) {
		run(t, running, spec.CmdForward{Target: "e2e-a", Ports: []string{"7000"}}, spec.OutcomeReject)
		run(t, running, spec.CmdForward{Target: "e2e-a", Ports: []string{"7000/udp"}}, spec.OutcomeOk)
		run(t, running, spec.CmdForward{Target: "e2e-a", Ports: []string{"9999"}}, spec.OutcomeReject)
	})

	t.Run("D9 edit applies to a running guest immediately", func(t *testing.T) {
		w := run(t, running, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk)
		w = run(t, w, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{CPUs: 4, Shares: []spec.RawShare{{Path: dirA}}}}, spec.OutcomeOk)
		g := guest(t, w, "e2e-a")
		m, _ := g.Mounts.Lookup(mustAbs(t, dirA))
		if g.Config.Res.CPUs.Get() != 4 || m.Origin != spec.OriginBoot {
			t.Fatalf("edit of resources must restart the guest: %v", g)
		}
	})

	t.Run("D10 invalid edit changes nothing", func(t *testing.T) {
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{CPUs: 999}}, spec.OutcomeReject)
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Shares: []spec.RawShare{{Path: dirA}, {Path: sub}}}}, spec.OutcomeReject)
	})

	t.Run("D11 allow without default is a migration error", func(t *testing.T) {
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Network: spec.RawNetwork{Allow: []string{"10.0.0.0/8"}}}}, spec.OutcomeReject)
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Network: spec.RawNetwork{Default: "allow", Allow: []string{"example.com"}}}}, spec.OutcomeReject)
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Network: spec.RawNetwork{Default: "allow", Allow: []string{"10.0.0.0/8"}}}}, spec.OutcomeOk)
		run(t, running, spec.CmdEdit{Target: "e2e-a", New: spec.RawConfig{Network: spec.RawNetwork{Default: "deny", Allow: []string{"127.0.0.0/8"}}}}, spec.OutcomeReject)
	})

	t.Run("P2 multi-guest start stops at the first failure", func(t *testing.T) {
		w := run(t, running, spec.CmdStop{Names: []string{"e2e-a"}}, spec.OutcomeOk)
		outs := spec.Post(w, fixtureEnv(t), spec.CmdStart{Names: []string{"e2e-a", "e2e-b", "e2e-a"}})
		last := outs[len(outs)-1]
		if last.Kind != spec.OutcomeReject || guest(t, last.World, "e2e-a").Phase != spec.PhaseRunning {
			t.Fatalf("e2e-a should stay started when e2e-b is rejected: %v", last)
		}
	})

	t.Run("sessions end when the guest stops or restarts, and kill ends one", func(t *testing.T) {
		w := run(t, running, spec.CmdSpawn{Target: "e2e-a", Command: "sleep 1"}, spec.OutcomeOk)
		w = run(t, w, spec.CmdSpawn{Target: "e2e-a", Command: "sleep 2"}, spec.OutcomeOk)
		if n := guest(t, w, "e2e-a").Sessions.Len(); n != 2 {
			t.Fatalf("%d sessions, want 2", n)
		}
		killed := run(t, w, spec.CmdKill{Target: "e2e-a", Commands: []string{"sleep 1"}}, spec.OutcomeOk)
		if n := guest(t, killed, "e2e-a").Sessions.Len(); n != 1 {
			t.Fatalf("%d sessions after kill, want 1", n)
		}
		run(t, killed, spec.CmdKill{Target: "e2e-a", Commands: []string{"sleep 1"}}, spec.OutcomeReject)
		for _, c := range []spec.Cmd{spec.CmdRestart{Names: []string{"e2e-a"}}, spec.CmdStop{Names: []string{"e2e-a"}}, spec.CmdSet{Target: "e2e-a", CPUs: types.Some(4)}} {
			if n := guest(t, run(t, w, c, spec.OutcomeOk), "e2e-a").Sessions.Len(); n != 0 {
				t.Fatalf("%#v left %d sessions", c, n)
			}
		}
		if n := guest(t, run(t, w, spec.CmdMount{Target: "e2e-a", Dirs: []string{dirA}}, spec.OutcomeOk), "e2e-a").Sessions.Len(); n != 2 {
			t.Fatalf("a live mount ended sessions: %d left", n)
		}
	})

	t.Run("run starts the guest and passes the exit status through", func(t *testing.T) {
		w := run(t, running, spec.CmdStop{Names: []string{"e2e-a"}}, spec.OutcomeOk)
		outs := spec.Post(w, fixtureEnv(t), spec.CmdRun{Target: "e2e-a", Code: 42})
		last := outs[len(outs)-1]
		if last.Exit() != 42 || guest(t, last.World, "e2e-a").Phase != spec.PhaseRunning {
			t.Fatalf("unexpected run outcome %v", last)
		}
	})

	t.Run("implicit NAME never selects a guest when two are defined", func(t *testing.T) {
		run(t, running, spec.CmdStop{}, spec.OutcomeReject)
		if spec.Post(spec.World{External: w0.External}, fixtureEnv(t), spec.CmdStop{}) != nil {
			t.Fatal("selecting the only (external) guest implicitly is outside the model")
		}
	})

	t.Run("rm without --yes is rejected", func(t *testing.T) {
		run(t, running, spec.CmdRm{Names: []string{"e2e-a"}}, spec.OutcomeReject)
		w := run(t, running, spec.CmdRm{Names: []string{"e2e-a"}, Yes: true}, spec.OutcomeOk)
		if w.Guests.Len() != 0 {
			t.Fatal("rm did not remove the guest")
		}
	})
}

func TestReach(t *testing.T) {
	h := spec.HostNet{
		Addrs:  []netip.Addr{netip.MustParseAddr("192.168.1.10")},
		OnLink: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
	}
	lan := netip.MustParseAddr("192.168.1.1")
	public := netip.MustParseAddr("1.1.1.1")
	self := netip.MustParseAddr("192.168.1.10")
	loopback := netip.MustParseAddr("127.0.0.1")
	metadata := netip.MustParseAddr("169.254.169.254")
	mapped := netip.MustParseAddr("::ffff:1.1.1.1")

	entries := func(raw ...string) spec.EntrySet {
		var es []spec.Entry
		for _, s := range raw {
			if p, err := netip.ParsePrefix(s); err == nil {
				es = append(es, spec.Entry{Kind: spec.EntryCIDR, Prefix: p})
				continue
			}
			d, err := domain.ParseDomainPattern(s)
			if err != nil {
				t.Fatal(err)
			}
			es = append(es, spec.Entry{Kind: spec.EntryDomain, Pattern: d})
		}
		return types.BuildSet[spec.Entry, spec.Entry, spec.EntryOrder](es)
	}

	cases := []struct {
		name    string
		policy  spec.NetPolicy
		dst     netip.Addr
		learned bool
		want    bool
	}{
		{"internet reaches public", spec.NetPolicy{Mode: spec.NetInternet}, public, false, true},
		{"internet blocks LAN", spec.NetPolicy{Mode: spec.NetInternet}, lan, false, false},
		{"lan reaches LAN", spec.NetPolicy{Mode: spec.NetLAN}, lan, false, true},
		{"none reaches nothing", spec.NetPolicy{Mode: spec.NetNone}, public, false, false},
		{"host itself is never reachable", spec.NetPolicy{Mode: spec.NetLAN}, self, false, false},
		{"loopback is never reachable", spec.NetPolicy{Mode: spec.NetLAN, Allow: entries("1.0.0.0/8")}, loopback, false, false},
		{"metadata is never reachable", spec.NetPolicy{Mode: spec.NetLAN}, metadata, false, false},
		{"IPv4-mapped public is treated as IPv4", spec.NetPolicy{Mode: spec.NetInternet}, mapped, false, true},
		{"static allow opens LAN in internet mode", spec.NetPolicy{Mode: spec.NetInternet, Allow: entries("192.168.1.1/32")}, lan, false, true},
		{"deny beats allow", spec.NetPolicy{Mode: spec.NetLAN, Allow: entries("1.1.1.1/32"), Deny: entries("1.1.1.0/24")}, public, false, false},
		{"default deny blocks unlearned public", spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny}, public, false, false},
		{"default deny reaches learned public", spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny}, public, true, true},
		{"learned LAN answer does not open LAN (rebinding)", spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny}, lan, true, false},
		{"default deny still honours static allow", spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny, Allow: entries("192.168.1.0/24")}, lan, false, true},
	}
	for _, c := range cases {
		if got := spec.Reach(c.policy, h, c.dst, c.learned); got != c.want {
			t.Errorf("%s: Reach = %v, want %v", c.name, got, c.want)
		}
	}

	name, _ := domain.ParseDomainName("api.example.com")
	open := spec.NetPolicy{Mode: spec.NetInternet}
	closed := spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny, Allow: entries("*.example.com")}
	denied := spec.NetPolicy{Mode: spec.NetInternet, Default: spec.DefaultDeny, Allow: entries("*.example.com"), Deny: entries("api.example.com")}
	if !spec.Resolvable(open, name) || !spec.Resolvable(closed, name) || spec.Resolvable(denied, name) {
		t.Error("Resolvable does not follow deny > allow > default")
	}
	other, _ := domain.ParseDomainName("other.org")
	if spec.Resolvable(closed, other) {
		t.Error("default deny must refuse names outside the allow list")
	}
}
