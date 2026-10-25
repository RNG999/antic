package config

import (
	"net/netip"
	"slices"
	"testing"
)

func TestLoadNetworkAllowDeny(t *testing.T) {
	dir := t.TempDir()
	toml := `[network]
mode = "internet"
allow = ["GitHub.com", "*.npmjs.org", "pypi.org.", "github.com", "192.168.11.50/32", "10.1.2.3/16", "2001:db8::1/128"]
deny = ["evil.com", "198.51.100.0/24"]
`
	writeGuest(t, dir, "dev", toml)
	c, err := Load(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Guests["dev"]
	if !slices.Equal(g.AllowDomains, []string{"github.com", "*.npmjs.org", "pypi.org"}) {
		t.Errorf("domains = %v", g.AllowDomains)
	}
	wantAllowNets := []netip.Prefix{
		netip.MustParsePrefix("192.168.11.50/32"),
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("2001:db8::1/128"),
	}
	if !slices.Equal(g.AllowNets, wantAllowNets) {
		t.Errorf("allow nets = %v", g.AllowNets)
	}
	if !slices.Equal(g.DenyDomains, []string{"evil.com"}) {
		t.Errorf("deny domains = %v", g.DenyDomains)
	}
	wantDenyNets := []netip.Prefix{
		netip.MustParsePrefix("198.51.100.0/24"),
	}
	if !slices.Equal(g.DenyNets, wantDenyNets) {
		t.Errorf("deny nets = %v", g.DenyNets)
	}
}
