package spec

import (
	"errors"
	"fmt"

	"github.com/RNG999/antic/internal/domain"
)

// Invariant checks the state invariants every reachable World satisfies.
func Invariant(w World) error {
	if !w.Guests.Canonical() || !w.External.Canonical() {
		return errors.New("guest sets are not canonical")
	}
	for _, e := range w.External.All() {
		if w.Guests.Contains(e.Name) {
			return fmt.Errorf("guest %s is both managed and external", e.Name.Get())
		}
	}
	for _, g := range w.Guests.All() {
		if err := guestInvariant(g); err != nil {
			return fmt.Errorf("guest %s: %w", g.Name.Get(), err)
		}
	}
	// Host ports are unique across all guests, managed and external.
	var seen []Port
	for _, g := range w.Guests.All() {
		seen = append(seen, g.Config.Ports.All()...)
	}
	for _, e := range w.External.All() {
		seen = append(seen, e.Ports.All()...)
	}
	for i, p := range seen {
		for _, q := range seen[i+1:] {
			if p.Host == q.Host && p.Proto.overlaps(q.Proto) {
				return fmt.Errorf("host port %d is forwarded twice", p.Host.Get())
			}
		}
	}
	return nil
}

func guestInvariant(g Guest) error {
	c := g.Config
	if !g.Name.Valid() || g.Phase > PhaseRunning {
		return errors.New("invalid name or phase")
	}
	if !c.Packages.Canonical() || !c.Shares.Canonical() || !c.Ports.Canonical() || !g.Mounts.Canonical() {
		return errors.New("sets are not canonical")
	}
	if !c.Res.CPUs.Valid() || !c.Res.Memory.Valid() {
		return errors.New("invalid resources")
	}
	net := c.Res.Net
	if net.Mode == NetNone && (c.Ports.Len() > 0 || net.Allow.Len() > 0 || net.Deny.Len() > 0 || net.Default != DefaultAllow) {
		return errors.New("network mode none with ports, default, allow or deny")
	}
	if net.Default == DefaultAllow {
		for _, e := range net.Allow.All() {
			if e.Kind == EntryDomain {
				return errors.New("domain allow entry with default allow")
			}
		}
	}
	shares := c.Shares.All()
	for i, s := range shares {
		for _, t := range shares[i+1:] {
			if domain.PathsOverlap(s.Path, t.Path) {
				return fmt.Errorf("shares %s and %s overlap", s.Path.Get(), t.Path.Get())
			}
		}
	}
	ports := c.Ports.All()
	for i, p := range ports {
		for _, q := range ports[i+1:] {
			if p.Host == q.Host && p.Proto.overlaps(q.Proto) {
				return fmt.Errorf("host port %d is forwarded twice", p.Host.Get())
			}
		}
	}
	if g.Phase != PhaseRunning {
		if g.Mounts.Len() > 0 {
			return errors.New("mounts on a guest that is not running")
		}
		return nil
	}
	mounts := g.Mounts.All()
	for i, m := range mounts {
		for _, n := range mounts[i+1:] {
			if domain.PathsOverlap(m.Path, n.Path) {
				return fmt.Errorf("mounts %s and %s overlap", m.Path.Get(), n.Path.Get())
			}
		}
		if s, ok := c.Shares.Lookup(m.Path); ok && s.Mode != m.Mode {
			return fmt.Errorf("mount %s has a different mode than its share", m.Path.Get())
		}
		if !c.Shares.Contains(m.Path) && m.Origin == OriginRuntime {
			return fmt.Errorf("runtime mount %s is not a configured share", m.Path.Get())
		}
	}
	// A running guest has every configured share mounted.
	for _, s := range shares {
		if !g.Mounts.Contains(s.Path) {
			return fmt.Errorf("share %s is not mounted", s.Path.Get())
		}
	}
	return nil
}
