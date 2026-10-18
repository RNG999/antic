// Package nixbuild builds guest systems with the host's Nix and maintains
// each guest's store: a separate Nix store (store/nix) holding only that
// guest's closures, which the guest mounts read-only as /nix.
package nixbuild

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Nix runs the host's nix.
type Nix struct {
	bin  string
	root string // host directory backing /nix/store ("" for a real /nix)
}

// New finds nix and its store root (rootless "local?root=..." stores).
func New() (*Nix, error) {
	bin, err := exec.LookPath("nix")
	if err != nil {
		return nil, fmt.Errorf("nix not found: %w", err)
	}
	n := &Nix{bin: bin}
	out, err := exec.Command(bin, "config", "show", "store").Output()
	if err != nil {
		return nil, fmt.Errorf("nix config show store: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '?'); i >= 0 {
		q, err := url.ParseQuery(s[i+1:])
		if err != nil {
			return nil, err
		}
		n.root = q.Get("root")
	}
	return n, nil
}

// Real maps a store path to where it is on the host.
func (n *Nix) Real(storePath string) string { return filepath.Join(n.root, storePath) }

// Build builds flake#attr and returns its store path.
func (n *Nix) Build(ctx context.Context, flake, attr string) (string, error) {
	// "^out": only the main output (packages may also have man/doc outputs).
	out, err := n.output(ctx, "build", "--no-link", "--print-out-paths", "path:"+flake+"#"+attr+"^out")
	if err != nil {
		return "", fmt.Errorf("nix build %s#%s: %w", flake, attr, err)
	}
	return strings.TrimSpace(out), nil
}

// GuestStore is a guest's own store (dir/nix/{store,var}).
type GuestStore string

func (g GuestStore) uri() string     { return "local?root=" + string(g) }
func (g GuestStore) profile() string { return filepath.Join(string(g), "nix/var/nix/profiles/system") }

// Real maps a store path to its location in the guest store.
func (g GuestStore) Real(storePath string) string { return filepath.Join(string(g), storePath) }

// Install copies the closure of system into the guest store, makes it the
// current "system" profile (keeping the last 3 generations) and collects
// garbage.
func (n *Nix) Install(ctx context.Context, g GuestStore, system string) error {
	if err := os.MkdirAll(string(g), 0o700); err != nil {
		return err
	}
	if _, err := n.output(ctx, "copy", "--no-check-sigs", "--to", g.uri(), system); err != nil {
		return fmt.Errorf("copy %s to the guest store: %w", system, err)
	}
	if err := n.env(ctx, "--store", g.uri(), "-p", g.profile(), "--set", system); err != nil {
		return fmt.Errorf("set the guest's system profile: %w", err)
	}
	if err := n.env(ctx, "--store", g.uri(), "-p", g.profile(), "--delete-generations", "+3"); err != nil {
		return err
	}
	if _, err := n.output(ctx, "store", "gc", "--store", g.uri()); err != nil {
		return fmt.Errorf("garbage-collect the guest store: %w", err)
	}
	return nil
}

// Current returns the guest store's current system ("" if none).
func (g GuestStore) Current() (string, error) {
	p := g.profile()
	for i := 0; i < 8; i++ {
		dst, err := os.Readlink(p)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(dst, "/nix/store/") {
			return dst, nil
		}
		p = filepath.Join(filepath.Dir(p), dst)
	}
	return "", fmt.Errorf("%s: too many links", g.profile())
}

func (n *Nix) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, n.bin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// env runs nix-env (the static nix binary dispatches on argv[0]).
func (n *Nix) env(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, n.bin, args...)
	cmd.Args[0] = "nix-env"
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
