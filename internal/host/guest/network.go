package guest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/RNG999/antic/internal/host/config"
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

// Destinations no guest may reach: the namespace's loopback, the host's own
// addresses, and special-purpose ranges. "filtered" adds private/LAN, CGNAT
// (Tailscale), link-local (cloud metadata), benchmark/fake-IP and NAT64
// ranges, i.e. everything but the internet.
var (
	alwaysBlocked4 = []string{"0.0.0.0/8", "127.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4"}
	alwaysBlocked6 = []string{"::/128", "::1/128", "ff00::/8"}
	lanBlocked4    = []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15"}
	lanBlocked6    = []string{"fc00::/7", "fe80::/10", "fec0::/10", "64:ff9b::/96", "64:ff9b:1::/48"}
)

// nftRules renders the egress filter for the guest's network namespace.
func nftRules(mode string, hostAddrs []net.IP) string {
	v4 := append([]string{}, alwaysBlocked4...)
	v6 := append([]string{}, alwaysBlocked6...)
	if mode == config.NetworkFiltered {
		v4 = append(v4, lanBlocked4...)
		v6 = append(v6, lanBlocked6...)
	}
	for _, ip := range hostAddrs {
		if ip.To4() != nil {
			v4 = append(v4, ip.String())
		} else {
			v6 = append(v6, ip.String())
		}
	}
	return fmt.Sprintf(`table inet antic {
  chain output {
    type filter hook output priority filter; policy accept;
    ip daddr %s udp dport 53 accept
    ip daddr %s tcp dport 53 accept
    ip daddr %s reject
    ip daddr { %s } reject
    ip6 daddr { %s } reject
  }
}
`, nsDNS, nsDNS, nsDNS, strings.Join(v4, ", "), strings.Join(v6, ", "))
}

// hostAddresses returns the host's own unicast addresses (reachable from
// the namespace through pasta's routing otherwise).
func hostAddresses() []net.IP {
	addrs, _ := net.InterfaceAddrs()
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP)
		}
	}
	return out
}

// startNet runs passt (vhost-user) inside a rootless network namespace from
// pasta, after nftables there installs the egress filter. The filter lives
// outside the guest, so guest root cannot change it. Neither maps the
// host's loopback, and pasta forwards no ports.
func (g *Guest) startNet(ctx context.Context) (string, error) {
	dir := g.sockDir("net")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "passt.sock")
	rules := filepath.Join(dir, "egress.nft")
	if err := os.WriteFile(rules, []byte(nftRules(g.Cfg.Network, hostAddresses())), 0o600); err != nil {
		return "", err
	}
	passt := []string{g.m.Bins.Passt, "--vhost-user", "--socket", sock, "--foreground", "--quiet", "--one-off",
		"--no-map-gw", "--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", nsDNS}
	quoted := make([]string, len(passt))
	for i, a := range passt {
		quoted[i] = shellQuote(a)
	}
	argv := []string{g.m.Bins.Pasta, "--config-net", "--quiet",
		"--no-map-gw", "--map-host-loopback", "none", "-t", "none", "-u", "none", "-T", "none", "-U", "none",
		"--dns-forward", nsDNS, "--",
		"/bin/sh", "-c", shellQuote(g.m.Bins.Nft) + " -f " + shellQuote(rules) + " && exec " + strings.Join(quoted, " ")}
	// No NoNewPrivileges: it stops passt from setting up its own user
	// namespace inside pasta's. passt and pasta drop privileges and apply
	// seccomp themselves.
	props := systemd.Confine([]string{dir}, []string{g.m.Bins.Pasta, g.m.Bins.Passt, g.m.Bins.Nft})
	return sock, g.helper(ctx, g.netUnit(), "antic network for "+g.Name(), props, sock, argv...)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
