package guest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/egress"
	"github.com/RNG999/antic/internal/host/systemd"
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
func nftRules(g *config.Guest, hostNets []*net.IPNet) (domain.NFTRuleset, error) {
	filter := hasFilter(g)
	policy := egressPolicy(g)

	var b strings.Builder
	fmt.Fprintf(&b, "table %s {\n", egress.Table)
	if policy.Default == teaegress.DefaultDeny {
		fmt.Fprintf(&b, "  set %s { type ipv4_addr; flags timeout; size 65536; }\n", egress.Set4)
		fmt.Fprintf(&b, "  set %s { type ipv6_addr; flags timeout; size 65536; }\n", egress.Set6)
	}
	b.WriteString("  chain output {\n")
	b.WriteString("    type filter hook output priority filter; policy accept;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } ip6 hoplimit 255 accept\n")
	for _, p := range g.Ports {
		if p.TCP() {
			fmt.Fprintf(&b, "    oifname \"lo\" ip saddr 127.0.0.1 ip daddr 127.0.0.1 tcp dport %d accept\n", p.Host)
		}
		if p.UDP() {
			fmt.Fprintf(&b, "    oifname \"lo\" ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport %d accept\n", p.Host)
		}
	}
	if filter {
		// passt -> DNS filter, and only the filter's marked sockets upstream.
		l := egress.Listen
		fmt.Fprintf(&b, "    oifname \"lo\" ip saddr 127.0.0.1 ip daddr %s udp dport %d accept\n", l.Addr(), l.Port())
		fmt.Fprintf(&b, "    oifname \"lo\" ip saddr 127.0.0.1 ip daddr %s tcp dport %d accept\n", l.Addr(), l.Port())
		fmt.Fprintf(&b, "    meta mark %#x ip daddr %s udp dport 53 accept\n", egress.Mark, nsDNS)
		fmt.Fprintf(&b, "    meta mark %#x ip daddr %s tcp dport 53 accept\n", egress.Mark, nsDNS)
	} else {
		fmt.Fprintf(&b, "    ip daddr %s udp dport 53 accept\n", nsDNS)
		fmt.Fprintf(&b, "    ip daddr %s tcp dport 53 accept\n", nsDNS)
	}
	fmt.Fprintf(&b, "    ip daddr %s reject\n", nsDNS)
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
				fmt.Fprintf(&b, "    ip daddr { %s } %s\n", strings.Join(v4, ", "), verdict)
			}
			if len(v6) > 0 {
				fmt.Fprintf(&b, "    ip6 daddr { %s } %s\n", strings.Join(v6, ", "), verdict)
			}
		case teaegress.MatchLearned:
			fmt.Fprintf(&b, "    ip daddr @%s %s\n", egress.Set4, verdict)
			fmt.Fprintf(&b, "    ip6 daddr @%s %s\n", egress.Set6, verdict)
		case teaegress.MatchAll:
			fmt.Fprintf(&b, "    %s\n", verdict)
		}
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return domain.ParseNFTRuleset(b.String())
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
func (g *Guest) startNet(ctx context.Context) (domain.AbsPath, error) {
	dir := g.netSockDir()
	if err := os.MkdirAll(dir.Get(), 0o700); err != nil {
		return domain.AbsPath{}, err
	}
	sock := domain.JoinAbsPath(dir, "passt.sock")
	rules := domain.JoinAbsPath(dir, "egress.nft")
	ports := g.Cfg.Ports
	renderedRules, err := nftRules(g.Cfg, hostNetworks())
	if err != nil {
		return domain.AbsPath{}, err
	}
	if err := os.WriteFile(rules.Get(), []byte(renderedRules.Get()), 0o600); err != nil {
		return domain.AbsPath{}, err
	}
	dnsHost := nsDNS
	netnsCmd := []string{g.m.Bins.Self.Get(), egress.HelperCmd, "--nft", g.m.Bins.NFT.Get(), "--rules", rules.Get()}
	if hasFilter(g.Cfg) {
		if len(g.Cfg.AllowDomains) > 0 {
			domains := domain.JoinAbsPath(dir, "allow-domains")
			if err := os.WriteFile(domains.Get(), []byte(strings.Join(g.Cfg.AllowDomains, "\n")+"\n"), 0o600); err != nil {
				return domain.AbsPath{}, err
			}
			netnsCmd = append(netnsCmd, "--allow-domains", domains.Get())
		}
		if len(g.Cfg.DenyDomains) > 0 {
			denyFile := domain.JoinAbsPath(dir, "deny-domains")
			if err := os.WriteFile(denyFile.Get(), []byte(strings.Join(g.Cfg.DenyDomains, "\n")+"\n"), 0o600); err != nil {
				return domain.AbsPath{}, err
			}
			netnsCmd = append(netnsCmd, "--deny-domains", denyFile.Get())
		}
		if g.Cfg.Network.DefaultDeny() {
			netnsCmd = append(netnsCmd, "--default-deny")
		}
		netnsCmd = append(netnsCmd, "--upstream", nsDNS)
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

	passt := []string{g.m.Bins.Passt.Get(), "--vhost-user", "--socket", sock.Get(), "--foreground", "--quiet", "--one-off",
		"--no-map-gw", "--map-host-loopback", "none",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", dnsHost}
	pasta := []string{g.m.Bins.Pasta.Get(), "--config-net", "--quiet",
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

	argv := append(append(append(pasta, "--"), netnsCmd...), append([]string{"--"}, passt...)...)
	// No NoNewPrivileges: it stops passt from setting up its own user
	// namespace inside pasta's. passt and pasta drop privileges and apply
	// seccomp themselves.
	var roBins []domain.AbsPath
	for _, b := range []domain.AbsPath{g.m.Bins.Pasta, g.m.Bins.Passt, g.m.Bins.NFT, g.m.Bins.Self} {
		if b.Valid() {
			roBins = append(roBins, b)
		}
	}
	props := g.m.confine([]domain.AbsPath{dir}, roBins)
	desc, _ := domain.ParsePrintableText("antic network for " + g.GuestName().Get())
	return sock, g.startBoundUnitAndWaitSocket(ctx, systemd.Unit{
		Name:        g.netUnit(),
		Description: desc,
		Props:       props,
		Argv:        argv,
	}, sock)
}
