package guest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/egress"
	teaegress "github.com/RNG999/antic/internal/tea/egress"
)

const (
	// Guest-visible DNS address; passt forwards queries sent to it.
	// TEST-NET-1, so it never clashes with a real host.
	guestDNS = "192.0.2.53"
	// Inside the network namespace, pasta forwards DNS sent here to the
	// host's resolver.
	nsDNS = "192.0.2.54"
)

// hasFilter reports whether the guest needs the DNS filter: it has domain
// entries, or default "deny" (which only reaches what it resolves).
func hasFilter(g *config.Guest) bool {
	return g.Network.DefaultDeny() || len(g.AllowDomains) > 0 || len(g.DenyDomains) > 0
}

// egressPolicy is the IP-level network policy of g.
func egressPolicy(g *config.Guest) teaegress.Policy {
	p := teaegress.Policy{Mode: teaegress.ModeInternet, AllowNets: g.AllowNets, DenyNets: g.DenyNets}
	if g.Network.Mode == config.NetworkLAN {
		p.Mode = teaegress.ModeLAN
	}
	if g.Network.DefaultDeny() {
		p.Default = teaegress.DefaultDeny
	}
	return p
}

// hostNet describes the host's addresses and on-link subnets.
func hostNet(hostNets []*net.IPNet) teaegress.HostNet {
	var h teaegress.HostNet
	for _, n := range hostNets {
		a, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		a = a.Unmap()
		h.Addrs = append(h.Addrs, a)
		if ones, bits := n.Mask.Size(); ones > 0 && ones < bits {
			h.OnLink = append(h.OnLink, netip.PrefixFrom(a, ones).Masked())
		}
	}
	return h
}

// nftRules renders the egress filter for the guest's network namespace: the
// plumbing (forwarded ports, the DNS path) and then the pure egress rules
// of internal/tea/egress in order.
func nftRules(g *config.Guest, hostNets []*net.IPNet) string {
	filter := hasFilter(g)
	policy := egressPolicy(g)

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	set := func(family string, items []string) string {
		return family + " daddr { " + strings.Join(items, ", ") + " }"
	}

	w("table %s {", egress.Table)
	if policy.Default == teaegress.DefaultDeny {
		w("  set %s { type ipv4_addr; flags timeout; size 65536; }", egress.Set4)
		w("  set %s { type ipv6_addr; flags timeout; size 65536; }", egress.Set6)
	}
	w("  chain output {")
	w("    type filter hook output priority filter; policy accept;")
	w("    ct state established,related accept")
	w("    icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } ip6 hoplimit 255 accept")
	for _, p := range g.Ports {
		if p.TCP() {
			w(`    oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 tcp dport %d accept`, p.Host)
		}
		if p.UDP() {
			w(`    oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport %d accept`, p.Host)
		}
	}
	if filter {
		// passt -> DNS filter, and only the filter's marked sockets upstream.
		l := egress.Listen
		w(`    oifname "lo" ip saddr 127.0.0.1 ip daddr %s udp dport %d accept`, l.Addr(), l.Port())
		w(`    oifname "lo" ip saddr 127.0.0.1 ip daddr %s tcp dport %d accept`, l.Addr(), l.Port())
		w("    meta mark %#x ip daddr %s udp dport 53 accept", egress.Mark, nsDNS)
		w("    meta mark %#x ip daddr %s tcp dport 53 accept", egress.Mark, nsDNS)
	} else {
		w("    ip daddr %s udp dport 53 accept", nsDNS)
		w("    ip daddr %s tcp dport 53 accept", nsDNS)
	}
	w("    ip daddr %s reject", nsDNS)
	for _, r := range teaegress.Rules(policy, hostNet(hostNets)) {
		verdict := "accept"
		if r.Verdict == teaegress.VerdictReject {
			verdict = "reject"
		}
		switch r.Match {
		case teaegress.MatchPrefixes:
			var v4, v6 []string
			for _, p := range r.Prefixes {
				if p.Addr().Is4() {
					v4 = append(v4, p.String())
				} else {
					v6 = append(v6, p.String())
				}
			}
			if len(v4) > 0 {
				w("    %s %s", set("ip", v4), verdict)
			}
			if len(v6) > 0 {
				w("    %s %s", set("ip6", v6), verdict)
			}
		case teaegress.MatchLearned:
			w("    ip daddr @%s %s", egress.Set4, verdict)
			w("    ip6 daddr @%s %s", egress.Set6, verdict)
		case teaegress.MatchAll:
			w("    %s", verdict)
		}
	}
	w("  }")
	w("}")
	return b.String()
}

// hostNetworks returns the host's own unicast interface addresses and masks.
func hostNetworks() []*net.IPNet {
	addrs, _ := net.InterfaceAddrs()
	var out []*net.IPNet
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n)
		}
	}
	return out
}

// startNet runs passt (vhost-user) inside a rootless network namespace from
// pasta. antic itself is the namespace's first process: it installs the
// egress filter, runs the DNS filter for guests with an allow list, and then
// runs passt. The filter lives outside the guest, so guest root cannot change
// it.
func (g *Guest) startNet(ctx context.Context) (string, error) {
	dir := g.sockDir("net")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "passt.sock")
	rules := filepath.Join(dir, "egress.nft")
	ports := g.Cfg.Ports
	if err := os.WriteFile(rules, []byte(nftRules(g.Cfg, hostNetworks())), 0o600); err != nil {
		return "", err
	}
	dnsHost := nsDNS
	helper := []string{g.m.Bins.Self, egress.HelperCmd, "--nft", g.m.Bins.NFT, "--rules", rules}
	if hasFilter(g.Cfg) {
		if len(g.Cfg.AllowDomains) > 0 {
			domains := filepath.Join(dir, "allow-domains")
			if err := os.WriteFile(domains, []byte(strings.Join(g.Cfg.AllowDomains, "\n")+"\n"), 0o600); err != nil {
				return "", err
			}
			helper = append(helper, "--allow-domains", domains)
		}
		if len(g.Cfg.DenyDomains) > 0 {
			denyFile := filepath.Join(dir, "deny-domains")
			if err := os.WriteFile(denyFile, []byte(strings.Join(g.Cfg.DenyDomains, "\n")+"\n"), 0o600); err != nil {
				return "", err
			}
			helper = append(helper, "--deny-domains", denyFile)
		}
		if g.Cfg.Network.DefaultDeny() {
			helper = append(helper, "--default-deny")
		}
		helper = append(helper, "--upstream", nsDNS)
		dnsHost = egress.Listen.Addr().String()
	}
	hasTCP := false
	hasUDP := false
	for _, p := range ports {
		if p.TCP() {
			hasTCP = true
		}
		if p.UDP() {
			hasUDP = true
		}
	}

	passt := []string{g.m.Bins.Passt, "--vhost-user", "--socket", sock, "--foreground", "--quiet", "--one-off",
		"--no-map-gw", "--map-host-loopback", "none",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", dnsHost}
	pasta := []string{g.m.Bins.Pasta, "--config-net", "--quiet",
		"--no-map-gw", "--map-host-loopback", "none", "-T", "none", "-U", "none",
		"--dns-forward", nsDNS}

	if !hasTCP {
		pasta = append(pasta, "-t", "none")
		passt = append(passt, "-t", "none")
	} else {
		for _, p := range ports {
			if p.TCP() {
				pasta = append(pasta, "-t", fmt.Sprintf("127.0.0.1/%d:%d", p.Host, p.Host))
				passt = append(passt, "-t", fmt.Sprintf("127.0.0.1/%d:%d", p.Host, p.Guest))
			}
		}
	}

	if !hasUDP {
		pasta = append(pasta, "-u", "none")
		passt = append(passt, "-u", "none")
	} else {
		for _, p := range ports {
			if p.UDP() {
				pasta = append(pasta, "-u", fmt.Sprintf("127.0.0.1/%d:%d", p.Host, p.Host))
				passt = append(passt, "-u", fmt.Sprintf("127.0.0.1/%d:%d", p.Host, p.Guest))
			}
		}
	}

	argv := append(append(append(pasta, "--"), helper...), append([]string{"--"}, passt...)...)
	// No NoNewPrivileges: it stops passt from setting up its own user
	// namespace inside pasta's. passt and pasta drop privileges and apply
	// seccomp themselves.
	props := g.m.confine([]string{dir}, []string{g.m.Bins.Pasta, g.m.Bins.Passt, g.m.Bins.NFT, g.m.Bins.Self})
	return sock, g.helper(ctx, g.netUnit(), "antic network for "+g.Name(), props, sock, argv...)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
