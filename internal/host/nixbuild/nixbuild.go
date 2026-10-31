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

// findNix locates the nix binary on PATH or in ~/.local/bin/nix.
func findNix() (string, error) {
	bin, err := exec.LookPath("nix")
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, ".local/bin/nix")
			if _, serr := os.Stat(cand); serr == nil {
				bin, err = cand, nil
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("nix not found: %w", err)
	}
	return bin, nil
}

// withNixConfig ensures nix-command and flakes are enabled even if nix.conf
// has not configured experimental-features yet.
func withNixConfig(cmd *exec.Cmd) *exec.Cmd {
	env := os.Environ()
	cfg := os.Getenv("NIX_CONFIG")
	if cfg != "" {
		cfg += "\n"
	}
	cfg += "extra-experimental-features = nix-command flakes\n"
	cmd.Env = append(env, "NIX_CONFIG="+cfg)
	return cmd
}

// New finds nix and its store root (rootless "local?root=..." stores).
func New() (*Nix, error) {
	bin, err := findNix()
	if err != nil {
		return nil, err
	}
	n := &Nix{bin: bin}
	out, err := withNixConfig(exec.Command(bin, "config", "show", "store")).Output()
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

// Root returns the host directory backing /nix ("" when /nix is the real root store).
func (n *Nix) Root() string { return n.root }

// Real maps a store path to where it is on the host.
func (n *Nix) Real(storePath string) string { return filepath.Join(n.root, storePath) }

// Build builds flake#attr and returns its store path.
func (n *Nix) Build(ctx context.Context, flake, attr string) (string, error) {
	return n.BuildLink(ctx, flake, attr, "")
}

// BuildLink builds flake#attr, optionally registering outLink as a GC root
// symlink (or --no-link if outLink is empty), and returns its store path.
func (n *Nix) BuildLink(ctx context.Context, flake, attr, outLink string) (string, error) {
	ref := flake
	args := []string{"build", "--print-out-paths"}
	if outLink != "" {
		args = append(args, "--out-link", outLink)
	} else {
		args = append(args, "--no-link")
	}
	if filepath.IsAbs(flake) {
		ref = "path:" + flake
	} else {
		args = append(args, "--refresh")
	}
	// "^out": only the main output (packages may also have man/doc outputs).
	args = append(args, ref+"#"+attr+"^out")
	out, err := n.output(ctx, args...)
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

// SetProfile copies the closure of system into the guest store and makes
// it the current "system" profile. Setting the current system again is
// harmless.
func (n *Nix) SetProfile(ctx context.Context, g GuestStore, system string) error {
	if err := os.MkdirAll(string(g), 0o700); err != nil {
		return err
	}
	if cur, err := g.Current(); err == nil && cur == system {
		return nil
	}
	if _, err := n.output(ctx, "copy", "--no-check-sigs", "--to", g.uri(), system); err != nil {
		return fmt.Errorf("copy %s to the guest store: %w", system, err)
	}
	if err := n.env(ctx, "--store", g.uri(), "-p", g.profile(), "--set", system); err != nil {
		return fmt.Errorf("set the guest's system profile: %w", err)
	}
	return nil
}

// RemoveProfile makes the guest store have no current system (the state of
// a guest that was never built).
func (n *Nix) RemoveProfile(g GuestStore) error {
	if err := os.Remove(g.profile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// GCGuest keeps the last 3 generations of the system profile and collects the
// guest store's garbage.
func (n *Nix) GCGuest(ctx context.Context, g GuestStore) error {
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
	cmd := withNixConfig(exec.CommandContext(ctx, n.bin, args...))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// env runs nix-env (the static nix binary dispatches on argv[0]).
func (n *Nix) env(ctx context.Context, args ...string) error {
	cmd := withNixConfig(exec.CommandContext(ctx, n.bin, args...))
	cmd.Args[0] = "nix-env"
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
