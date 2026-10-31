package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Metamorphic properties of the config file edits (config.Draft), which
// the specification cannot state: it knows what a config means, not how the
// file is written. Edits keep the user's comments outside the blocks they
// remove, undoing an edit restores the meaning, and edits of different
// shares commute.

const commented = `# my dev guest
description = "dev"   # trailing comment
packages = ["git"]

[spec]
# sized for builds
cpus = 2
memory = "2G"

[network]
mode = "internet"
`

// meaning renders what g means, independently of how its file is written.
func meaning(g *Guest) string {
	var shares, ports []string
	for _, s := range g.Shares {
		shares = append(shares, fmt.Sprintf("%s:%v", s.Path, s.ReadOnly))
	}
	for _, p := range g.Ports {
		ports = append(ports, fmt.Sprintf("%d:%d/%s", p.Host, p.Guest, p.ProtoStr()))
	}
	slices.Sort(shares)
	slices.Sort(ports)
	pkgs := slices.Clone(g.Packages)
	slices.Sort(pkgs)
	return fmt.Sprintf("pkgs=%v cpus=%d mem=%d net=%s/%s shares=%v ports=%v",
		pkgs, g.CPUs, g.MemoryBytes, g.Network.Mode, g.Network.Default, shares, ports)
}

func draftFixture(t *testing.T) (Draft, string) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.toml")
	if err := os.WriteFile(path, []byte(commented), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGuest(path, "dev", home)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ReadDraft(g, home)
	if err != nil {
		t.Fatal(err)
	}
	return d, path
}

var pbtShares = []string{home + "/a", home + "/b", home + "/c/d", home + "/x y"}

// genDraftEdit applies one random edit (shares, ports, resources,
// packages) to d; edits the draft rejects leave it unchanged.
func genDraftEdit(t *rapid.T, d Draft) Draft {
	var next Draft
	var err error
	switch rapid.IntRange(0, 5).Draw(t, "edit") {
	case 0:
		next, err = d.AddShare(rapid.SampledFrom(pbtShares).Draw(t, "share"), rapid.Bool().Draw(t, "ro"))
	case 1:
		next, err = d.RemoveShare(rapid.SampledFrom(pbtShares).Draw(t, "share"))
	case 2:
		h := rapid.IntRange(8000, 8003).Draw(t, "host")
		next, err = d.AddPorts([]Port{{Host: h, Guest: h, Proto: rapid.SampledFrom([]string{"", "udp", "tcp,udp"}).Draw(t, "proto")}}, nil)
	case 3:
		next, err = d.RemovePorts([]int{rapid.IntRange(8000, 8003).Draw(t, "host")})
	case 4:
		cpus := rapid.IntRange(1, 8).Draw(t, "cpus")
		next, err = d.SetResources(ResourceUpdates{CPUs: &cpus})
	default:
		next, err = d.SetPackages(rapid.SliceOfDistinct(rapid.SampledFrom([]string{"git", "jq", "ripgrep"}), rapid.ID[string]).Draw(t, "pkgs"))
	}
	if err != nil {
		return d
	}
	return next
}

func TestPBTDraftKeepsComments(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		d, _ := draftFixture(t)
		for range rapid.IntRange(1, 15).Draw(rt, "edits") {
			d = genDraftEdit(rt, d)
		}
		for _, c := range []string{"# my dev guest", "# trailing comment", "# sized for builds"} {
			if !strings.Contains(string(d.New), c) {
				rt.Fatalf("the edits lost %q:\n%s", c, d.New)
			}
		}
	})
}

func TestPBTDraftInverseEdits(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		d, _ := draftFixture(t)
		for range rapid.IntRange(0, 6).Draw(rt, "setup") {
			d = genDraftEdit(rt, d)
		}
		before := meaning(d.After)
		share := rapid.SampledFrom(pbtShares).Draw(rt, "share")
		if _, had := d.After.ShareAt(share); !had {
			if added, err := d.AddShare(share, rapid.Bool().Draw(rt, "ro")); err == nil {
				back, err := added.RemoveShare(share)
				if err != nil || meaning(back.After) != before {
					rt.Fatalf("sharing and unsharing %s changed %s to %s (%v)", share, before, meaning(back.After), err)
				}
			}
		}
		h := rapid.IntRange(8000, 8003).Draw(rt, "host")
		if !slices.ContainsFunc(d.After.Ports, func(p Port) bool { return p.Host == h }) {
			if added, err := d.AddPorts([]Port{{Host: h, Guest: 80}}, nil); err == nil {
				back, err := added.RemovePorts([]int{h})
				if err != nil || meaning(back.After) != before {
					rt.Fatalf("forwarding and unforwarding %d changed %s to %s (%v)", h, before, meaning(back.After), err)
				}
			}
		}
	})
}

func TestPBTDraftEditsCommute(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		d, _ := draftFixture(t)
		x := rapid.SampledFrom(pbtShares).Draw(rt, "x")
		y := rapid.SampledFrom(pbtShares).Draw(rt, "y")
		rx, ry := rapid.Bool().Draw(rt, "rox"), rapid.Bool().Draw(rt, "roy")
		apply := func(a string, ra bool, b string, rb bool) (string, error) {
			d1, err := d.AddShare(a, ra)
			if err != nil {
				return "", err
			}
			d2, err := d1.AddShare(b, rb)
			if err != nil {
				return "", err
			}
			return meaning(d2.After), nil
		}
		xy, err1 := apply(x, rx, y, ry)
		yx, err2 := apply(y, ry, x, rx)
		if (err1 == nil) != (err2 == nil) || xy != yx {
			rt.Fatalf("sharing %s then %s gives %q (%v), the other order %q (%v)", x, y, xy, err1, yx, err2)
		}
	})
}
