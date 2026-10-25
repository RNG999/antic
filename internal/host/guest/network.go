package guest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/egress"
	"github.com/RNG999/antic/internal/host/systemd"
)

const (
	// Guest-visible DNS address; passt forwards queries sent to it.
	// TEST-NET-1, so it never clashes with a real host.
	guestDNS = "192.0.2.53"
	// Inside the network namespace, pasta forwards DNS sent here to the
	// host's resolver.
	nsDNS = "192.0.2.54"
)

// Destinations no guest may reach: the namespace's loopback, link-local
// (including cloud metadata 169.254.169.254 and host fe80:: addresses),
// IPv4-mapped IPv6, multicast, and the host's own addresses.
// "internet" mode also blocks private/LAN ranges, CGNAT (Tailscale), NAT64,
// and any on-link subnet (including delegated global IPv6 /64 LAN prefixes).
var (
	alwaysBlocked4 = []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"}
	alwaysBlocked6 = []string{"::/128", "::1/128", "::ffff:0:0/96", "fe80::/10", "ff00::/8"}
	lanBlocked4    = []string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15"}
	lanBlocked6    = []string{"fc00::/7", "fec0::/10", "64:ff9b::/96", "64:ff9b:1::/48"}
)

// hasFilter reports whether the guest has an allow or deny filter.
func hasFilter(g *config.Guest) bool {
	return len(g.Network.Allow) > 0 || len(g.Network.Deny) > 0
}

func hasAllow(g *config.Guest) bool {
	return len(g.Network.Allow) > 0
}

// nftRules renders the egress filter for the guest's network namespace.
//
// Order matters: always-blocked destinations and host addresses are
// rejected first, then static deny entries are rejected, then the static allow
// list is accepted (so it may name LAN hosts in "internet" mode), then LAN
// ranges are rejected in "internet" mode (so DNS answers pointing into the LAN
// never open it), then addresses the DNS filter learned are accepted, and
// with an allow list everything else is rejected.
func nftRules(g *config.Guest, hostNets []*net.IPNet) string {
	filter := hasFilter(g)
	allow := hasAllow(g)
	internet := g.Network.Mode == config.NetworkInternet
	v4 := append([]string{}, alwaysBlocked4...)
	v6 := append([]string{}, alwaysBlocked6...)
	var lan4, lan6 []string
	if internet {
		lan4 = append(lan4, lanBlocked4...)
		lan6 = append(lan6, lanBlocked6...)
	}
	for _, n := range hostNets {
		ones, bits := n.Mask.Size()
		if ip4 := n.IP.To4(); ip4 != nil {
			v4 = append(v4, ip4.String())
			if internet && ones > 0 && ones < bits {
				lan4 = append(lan4, (&net.IPNet{IP: ip4.Mask(n.Mask), Mask: n.Mask}).String())
			}
		} else if ip6 := n.IP.To16(); ip6 != nil {
			v6 = append(v6, ip6.String())
			if internet && ones > 0 && ones < bits {
				lan6 = append(lan6, (&net.IPNet{IP: ip6.Mask(n.Mask), Mask: n.Mask}).String())
			}
		}
	}
	var deny4, deny6 []string
	for _, p := range g.DenyNets {
		if p.Addr().Is4() {
			deny4 = append(deny4, p.String())
		} else {
			deny6 = append(deny6, p.String())
		}
	}
	var static4, static6 []string
	for _, p := range g.AllowNets {
		if p.Addr().Is4() {
			static4 = append(static4, p.String())
		} else {
			static6 = append(static6, p.String())
		}
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	set := func(family string, items []string) string { return family + " daddr { " + strings.Join(items, ", ") + " }" }

	w("table inet antic {")
	if allow {
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
	w("    %s reject", set("ip", v4))
	w("    %s reject", set("ip6", v6))
	if len(deny4) > 0 {
		w("    %s reject", set("ip", deny4))
	}
	if len(deny6) > 0 {
		w("    %s reject", set("ip6", deny6))
	}
	if len(static4) > 0 {
		w("    %s accept", set("ip", static4))
	}
	if len(static6) > 0 {
		w("    %s accept", set("ip6", static6))
	}
	if len(lan4) > 0 {
		w("    %s reject", set("ip", lan4))
	}
	if len(lan6) > 0 {
		w("    %s reject", set("ip6", lan6))
	}
	if allow {
		w("    ip daddr @%s accept", egress.Set4)
		w("    ip6 daddr @%s accept", egress.Set6)
		w("    reject")
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
	helper := []string{g.m.Bins.Self, egress.HelperCmd, "--nft", g.m.Bins.Nft, "--rules", rules}
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
	props := systemd.Confine([]string{dir}, []string{g.m.Bins.Pasta, g.m.Bins.Passt, g.m.Bins.Nft, g.m.Bins.Self})
	return sock, g.helper(ctx, g.netUnit(), "antic network for "+g.Name(), props, sock, argv...)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
