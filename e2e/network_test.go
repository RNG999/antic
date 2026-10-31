//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
)

// O3 for the network: what a running guest can reach and resolve, probed
// from inside it with base-system tools only (bash's /dev/tcp, getent), is
// what the specification's Reach and Resolvable say for its policy on this
// host's network. The expectations are computed, not written down.

// hostNet is this host's network as the specification describes it: its
// unicast addresses and on-link subnets, found independently of antic.
func hostNet(t *testing.T) (spec.HostNet, netip.Addr) {
	t.Helper()
	var h spec.HostNet
	var lan netip.Addr
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil || p.Addr().IsLinkLocalUnicast() {
				continue
			}
			h.Addrs = append(h.Addrs, p.Addr())
			h.OnLink = append(h.OnLink, p.Masked())
			if p.Addr().Is4() && p.Addr().IsPrivate() && !lan.IsValid() {
				lan = p.Addr()
			}
		}
	}
	return h, lan
}

// probeConnect reports whether guest name can open a TCP connection to d:port.
func (h *harness) probeConnect(name string, d netip.Addr, port int) bool {
	_, code, _ := h.antic(nil, "run", name, "-T", "--", "bash", "-c",
		fmt.Sprintf("timeout 5 bash -c '</dev/tcp/%s/%d'", d, port))
	return code == 0
}

// probeResolve returns the addresses guest name resolves host to (nil if none).
func (h *harness) probeResolve(name, host string) []netip.Addr {
	out, code, _ := h.antic(nil, "run", name, "-T", "--", "getent", "ahosts", host)
	if code != 0 {
		return nil
	}
	var as []netip.Addr
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			if a, err := netip.ParseAddr(f[0]); err == nil {
				as = append(as, a)
			}
		}
	}
	return as
}

func TestE2ENetworkReach(t *testing.T) {
	h := newHarness(t)
	base := h.model
	host, lan := hostNet(t)
	public := netip.MustParseAddr("1.1.1.1")
	conn, err := net.DialTimeout("tcp", "1.1.1.1:443", 5e9)
	if err != nil {
		t.Skipf("this host cannot reach %v:443 itself: %v", public, err)
	}
	conn.Close()
	// A listener on the host's own LAN address: something a guest could
	// reach if the filter let it (the host's addresses are never reachable).
	var selfPort int
	if lan.IsValid() {
		l, err := net.Listen("tcp", netip.AddrPortFrom(lan, 0).String())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		selfPort = l.Addr().(*net.TCPAddr).Port
	}
	type probe struct {
		resolve string     // resolve this name first (it may teach the filter its addresses)
		connect netip.Addr // then connect here
	}
	for _, c := range []struct {
		name   string
		net    spec.RawNetwork
		probes []probe
	}{
		{"internet", spec.RawNetwork{}, []probe{{connect: public}, {resolve: "example.org"}}},
		{"internet deny a CIDR", spec.RawNetwork{Default: "allow", Deny: []string{"1.1.1.0/24"}},
			[]probe{{connect: public}, {connect: netip.MustParseAddr("1.0.0.1")}}},
		{"default deny learns from an allowed name", spec.RawNetwork{Default: "deny", Allow: []string{"one.one.one.one"}},
			[]probe{{connect: public}, {resolve: "example.org"}, {resolve: "one.one.one.one", connect: public}}},
		{"none", spec.RawNetwork{Mode: "none"}, []probe{{resolve: "example.org"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model = base
			defer h.cleanup()
			h.fixtureConfig(b, spec.RawConfig{Packages: []string{"git"}, Memory: "1G", Network: c.net})
			for _, cmd := range []spec.Cmd{spec.CmdApply{Target: b}, spec.CmdStart{Names: []string{b}}} {
				if msg := h.step(cmd); msg != "" {
					t.Fatal(msg)
				}
			}
			name, _ := domain.ParseGuestName(b)
			g, _ := h.model.Guests.Lookup(name)
			policy := g.Config.Res.Net
			learned := map[netip.Addr]bool{}
			probes := c.probes
			if lan.IsValid() {
				probes = append(probes, probe{connect: lan})
			}
			for _, p := range probes {
				if p.resolve != "" {
					n, err := domain.ParseDomainName(p.resolve)
					if err != nil {
						t.Fatal(err)
					}
					got := h.probeResolve(b, p.resolve)
					want := spec.Resolvable(policy, n)
					if (len(got) > 0) != want {
						t.Errorf("resolve %s: got %v, want resolvable=%v", p.resolve, got, want)
					}
					t.Logf("resolve %s: %v (spec: %v)", p.resolve, got, want)
					for _, a := range got {
						learned[a] = true
					}
				}
				if p.connect.IsValid() {
					port := 443
					if p.connect == lan {
						port = selfPort
					}
					got := h.probeConnect(b, p.connect, port)
					want := spec.Reach(policy, host, p.connect, learned[p.connect])
					if got != want {
						t.Errorf("connect %v:%d (learned %v): got %v, want %v", p.connect, port, learned[p.connect], got, want)
					}
					t.Logf("connect %v:%d (learned %v): %v (spec: %v)", p.connect, port, learned[p.connect], got, want)
				}
			}
		})
	}
	h.t = t
}
