package config

import (
	"net/netip"
	"slices"
	"testing"
)

func TestLoadAllow(t *testing.T) {
	dir := t.TempDir()
	writeGuest(t, dir, "dev", `allow = ["GitHub.com", "*.npmjs.org", "pypi.org.", "github.com", "192.168.11.50", "10.1.2.3/16", "2001:db8::1"]`+"\n")
	c, err := Load(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Guests["dev"]
	if !slices.Equal(g.AllowDomains, []string{"github.com", "npmjs.org", "pypi.org"}) {
		t.Errorf("domains = %v", g.AllowDomains)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("192.168.11.50/32"),
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("2001:db8::1/128"),
	}
	if !slices.Equal(g.AllowNets, want) {
		t.Errorf("nets = %v", g.AllowNets)
	}
}
