package guest

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/host/config"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

func hostNets(t *testing.T) []*net.IPNet {
	return []*net.IPNet{
		mustCIDR(t, "192.168.11.12/24"),
		mustCIDR(t, "2404:7a81:f80:4400:a578:cbae:3ab4:9bc9/64"),
	}
}

func contains(t *testing.T, rules string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(rules, w) {
			t.Errorf("rules lack %q:\n%s", w, rules)
		}
	}
}

func lacks(t *testing.T, rules string, notWant ...string) {
	t.Helper()
	for _, w := range notWant {
		if strings.Contains(rules, w) {
			t.Errorf("rules must not contain %q:\n%s", w, rules)
		}
	}
}

// index returns the position of s in rules, failing if absent.
func index(t *testing.T, rules, s string) int {
	t.Helper()
	i := strings.Index(rules, s)
	if i < 0 {
		t.Fatalf("rules lack %q:\n%s", s, rules)
	}
	return i
}

func TestNFTRulesInternet(t *testing.T) {
	g := &config.Guest{Network: config.NetworkConfig{Mode: config.NetworkInternet}, Ports: []config.Port{{Host: 8080, Guest: 80}}}
	rules := nftRules(g, hostNets(t))
	contains(t, rules,
		"icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } ip6 hoplimit 255 accept",
		`oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 tcp dport 8080 accept`,
		"ip daddr 192.0.2.54 udp dport 53 accept",
		"169.254.0.0/16", "::ffff:0.0.0.0/96", "fe80::/10",
		"192.168.11.12/32", "192.168.11.0/24",
		"2404:7a81:f80:4400:a578:cbae:3ab4:9bc9", "2404:7a81:f80:4400::/64",
	)
	// No allow list: no default deny, no DNS filter.
	lacks(t, rules, "@dyn4", "meta mark", "    reject\n")
}

func TestNFTRulesLAN(t *testing.T) {
	g := &config.Guest{Network: config.NetworkConfig{Mode: config.NetworkLAN}}
	rules := nftRules(g, hostNets(t))
	contains(t, rules, "169.254.0.0/16", "::ffff:0.0.0.0/96", "fe80::/10", "192.168.11.12/32", "2404:7a81:f80:4400:a578:cbae:3ab4:9bc9/128")
	lacks(t, rules, "192.168.0.0/16", "192.168.11.0/24", "2404:7a81:f80:4400::/64")
}

func TestNFTRulesAllowList(t *testing.T) {
	g := &config.Guest{
		Network: config.NetworkConfig{
			Mode:    config.NetworkInternet,
			Default: "deny",
			Allow:   []string{"github.com", "192.168.11.50/32", "2001:db8::/32"},
		},
		AllowDomains: []string{"github.com"},
		AllowNets:    []netip.Prefix{netip.MustParsePrefix("192.168.11.50/32"), netip.MustParsePrefix("2001:db8::/32")},
	}
	rules := nftRules(g, hostNets(t))
	contains(t, rules,
		"set dyn4 { type ipv4_addr; flags timeout; size 65536; }",
		"set dyn6 { type ipv6_addr; flags timeout; size 65536; }",
		`oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport 53 accept`,
		"meta mark 0x616e7463 ip daddr 192.0.2.54 udp dport 53 accept",
		"meta mark 0x616e7463 ip daddr 192.0.2.54 tcp dport 53 accept",
	)
	// Only the filter's marked sockets may reach the upstream resolver.
	lacks(t, rules, "    ip daddr 192.0.2.54 udp dport 53 accept", "    ip daddr 192.0.2.54 tcp dport 53 accept")

	nsReject := index(t, rules, "ip daddr 192.0.2.54 reject")
	always := index(t, rules, "ip daddr { 0.0.0.0/8")
	static4 := index(t, rules, "ip daddr { 192.168.11.50/32 } accept")
	static6 := index(t, rules, "ip6 daddr { 2001:db8::/32 } accept")
	lan := index(t, rules, "ip daddr { 10.0.0.0/8")
	dyn := index(t, rules, "ip daddr @dyn4 accept")
	final := index(t, rules, "    reject\n  }")
	if !(nsReject < always && always < static4 && static4 < static6 && static6 < lan && lan < dyn && dyn < final) {
		t.Errorf("rule order wrong:\n%s", rules)
	}
	// The host address is rejected before the static allow list.
	if index(t, rules, "192.168.11.12") > static4 {
		t.Errorf("host address must be rejected before static allow:\n%s", rules)
	}
}

func TestNFTRulesDenyList(t *testing.T) {
	g := &config.Guest{
		Network: config.NetworkConfig{
			Mode: config.NetworkInternet,
			Deny: []string{"bad.com", "198.51.100.0/24"},
		},
		DenyDomains: []string{"bad.com"},
		DenyNets:    []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	}
	rules := nftRules(g, hostNets(t))
	contains(t, rules,
		`oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport 53 accept`,
		"meta mark 0x616e7463 ip daddr 192.0.2.54 udp dport 53 accept",
		"ip daddr { 198.51.100.0/24 } reject",
	)
	// Deny-only is default allow (no dyn4, no final reject)
	lacks(t, rules, "@dyn4", "    reject\n  }")
}

func TestNFTRulesAllowExceptionsWithDefaultAllow(t *testing.T) {
	g := &config.Guest{
		Network:   config.NetworkConfig{Mode: config.NetworkInternet, Default: "allow", Allow: []string{"192.168.11.50/32"}},
		AllowNets: []netip.Prefix{netip.MustParsePrefix("192.168.11.50/32")},
	}
	rules := nftRules(g, hostNets(t))
	// A static exception opens one LAN host; everything else keeps the mode.
	if index(t, rules, "ip daddr { 192.168.11.50/32 } accept") > index(t, rules, "ip daddr { 10.0.0.0/8") {
		t.Errorf("the exception must come before the LAN block:\n%s", rules)
	}
	// Default allow: no DNS filter, no learned set, no final reject.
	lacks(t, rules, "@dyn4", "meta mark", "    reject\n  }")
}
