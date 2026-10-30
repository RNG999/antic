//go:build e2e

// Package e2e runs antic for real (this host, this user, real systemd, KVM,
// Nix and cloud-hypervisor; no mocks) and checks every command against the
// top-level specification in internal/spec. See docs/design/e2e-spec.md.
//
// Build static binaries inside the dev shell, then run them from the user's
// ordinary environment: with a rootless Nix store, "nix develop" runs in a
// namespace where Nix cannot sandbox guest builds, and dynamically linked
// binaries built there need that namespace's /nix:
//
//	nix develop -c sh -c 'export CGO_ENABLED=0 && go build -o ~/.cache/antic-e2e/bin/antic ./cmd/antic &&
//	  go test -c -tags e2e -o ~/.cache/antic-e2e/e2e.test ./e2e'
//	ANTIC_E2E_BIN=~/.cache/antic-e2e/bin/antic ~/.cache/antic-e2e/e2e.test -test.v -test.timeout 3h
//
// Guests are named e2e-*; every other guest is external and must not change.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

const (
	prefix     = "e2e-"
	busyPort   = 9999
	cmdTimeout = 20 * time.Minute
)

// harness drives the real antic binary and keeps the model in step.
type harness struct {
	t     *testing.T
	bin   string // antic under test
	home  string
	work  string // fixture directories and antic's working directory
	cfg   string // antic's guest config directory
	edit  string // EDITOR script
	env   spec.Env
	model spec.World
	// editorArgs runs EDITOR as "sh SCRIPT" (an EDITOR value with arguments, D12).
	editorArgs bool
	busy       net.Listener
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, home: home, work: filepath.Join(home, "e2e-work")}
	h.bin = buildAntic(t, home)
	h.prepareFixture()
	if out, code, _ := h.antic(nil, "guest", "show", "--path"); code == 0 {
		h.cfg = strings.TrimSpace(out)
	} else {
		t.Fatal("cannot find antic's guest config directory")
	}
	h.env = h.fixtureEnv()
	h.cleanup()
	obs, err := h.observe()
	if err != nil {
		t.Fatal(err)
	}
	h.model = spec.World{External: obs.external}
	t.Cleanup(h.cleanup)
	return h
}

// buildAntic returns $ANTIC_E2E_BIN, or builds the working tree's antic under
// $HOME: antic binds its own binary into sandboxed units with a private /tmp,
// so it must not live there.
func buildAntic(t *testing.T, home string) string {
	t.Helper()
	if bin := os.Getenv("ANTIC_E2E_BIN"); bin != "" {
		return bin
	}
	bin := filepath.Join(home, ".cache", "antic-e2e", "bin", "antic")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/antic")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building antic: %v\n%s", err, out)
	}
	return bin
}

// prepareFixture creates the directories guests share and holds busyPort.
func (h *harness) prepareFixture() {
	t := h.t
	for _, d := range []string{"A/sub", "A/.git", "B"} {
		if err := os.MkdirAll(filepath.Join(h.work, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.work, "A", "CLAUDE.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.work, "LB")
	if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(filepath.Join(h.work, "B"), link); err != nil {
			t.Fatal(err)
		}
	}
	h.edit = filepath.Join(h.home, ".cache", "antic-e2e", "edit.sh")
	if err := os.WriteFile(h.edit, []byte("#!/bin/sh\ncp \"$E2E_EDIT_SRC\" \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{8001, 8002} {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Skipf("port %d is in use on this host; the fixture needs it free", p)
		}
		l.Close()
	}
	if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(busyPort)); err == nil {
		h.busy = l
		t.Cleanup(func() { l.Close() })
	}
}

func (h *harness) path(rel string) string { return filepath.Join(h.work, rel) }

func mustAbs(t *testing.T, s string) domain.AbsPath {
	t.Helper()
	p, err := domain.ParseAbsPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fixtureEnv describes the host facts the specification depends on.
func (h *harness) fixtureEnv() spec.Env {
	t := h.t
	env := spec.Env{Home: mustAbs(t, h.home)}
	for _, d := range []string{h.path("A"), h.path("A/sub"), h.path("B"), filepath.Join(h.home, ".local")} {
		env.Dirs = append(env.Dirs, mustAbs(t, d))
	}
	env.Links = []spec.Link{{From: mustAbs(t, h.path("LB")), To: mustAbs(t, h.path("B"))}}
	// antic never shares hidden entries at the top of $HOME (nor what they
	// link to) or $PATH directories in $HOME.
	entries, _ := os.ReadDir(h.home)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			p := filepath.Join(h.home, e.Name())
			env.Denied = append(env.Denied, mustAbs(t, p))
			if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
				if a, err := domain.ParseAbsPath(real); err == nil {
					env.Denied = append(env.Denied, a)
				}
			}
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.HasPrefix(d, h.home+"/") {
			if a, err := domain.ParseAbsPath(filepath.Clean(d)); err == nil {
				env.Denied = append(env.Denied, a)
			}
		}
	}
	busy, _ := domain.ParsePortNumber(busyPort)
	env.Busy = []spec.PortKey{{Host: busy, Proto: spec.ProtoTCP}}
	missing, _ := domain.ParsePackageName("e2e-no-such-package")
	env.Unbuildable = []domain.PackageName{missing}
	return env
}

// antic runs the binary under test in the fixture directory without a
// terminal and returns its stdout and exit status.
func (h *harness) antic(extraEnv []string, args ...string) (stdout string, code int, stderr string) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = h.work
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		h.t.Fatalf("antic %v: %v", args, err)
	}
	if ctx.Err() != nil {
		h.t.Fatalf("antic %v did not finish within %v (quiescence)", args, cmdTimeout)
	}
	return out.String(), code, errb.String()
}

// cleanup removes every e2e-* guest and config file. A guest whose config
// does not load cannot be removed with "guest rm" (D17), so every e2e-*
// config is first replaced with a minimal valid one.
func (h *harness) cleanup() {
	if h.cfg != "" {
		files, _ := filepath.Glob(filepath.Join(h.cfg, prefix+"*.toml"))
		for _, f := range files {
			os.WriteFile(f, []byte("packages = [\"git\"]\n"), 0o644)
		}
	}
	out, _, _ := h.antic(nil, "guest", "ls", "-q")
	for _, n := range strings.Fields(out) {
		if strings.HasPrefix(n, prefix) {
			h.antic(nil, "guest", "rm", "-y", n)
		}
	}
	if h.cfg != "" {
		files, _ := filepath.Glob(filepath.Join(h.cfg, prefix+"*.toml"))
		for _, f := range files {
			os.Remove(f)
		}
	}
}

// render turns a command into antic's argv and environment.
func (h *harness) render(c spec.Cmd) ([]string, []string) {
	opt := func(args []string, s string) []string {
		if s != "" {
			args = append(args, s)
		}
		return args
	}
	switch c := c.(type) {
	case spec.CmdCreate:
		args := []string{"guest", "create", c.Name}
		for _, p := range c.Packages {
			args = append(args, "-p", p)
		}
		if v, ok := c.CPUs.Get(); ok {
			args = append(args, "--cpus", strconv.Itoa(v))
		}
		if v, ok := c.Memory.Get(); ok {
			args = append(args, "--memory", v)
		}
		if v, ok := c.Network.Get(); ok {
			args = append(args, "--network", v)
		}
		for _, p := range c.Ports {
			args = append(args, "--port", p)
		}
		return args, nil
	case spec.CmdApply:
		return opt([]string{"guest", "apply"}, c.Target), nil
	case spec.CmdInstall:
		return append(opt([]string{"guest", "install"}, c.Target), c.Packages...), nil
	case spec.CmdUninstall:
		return append(opt([]string{"guest", "uninstall"}, c.Target), c.Packages...), nil
	case spec.CmdSet:
		args := opt([]string{"guest", "set"}, c.Target)
		if v, ok := c.CPUs.Get(); ok {
			args = append(args, "--cpus", strconv.Itoa(v))
		}
		if v, ok := c.Memory.Get(); ok {
			args = append(args, "--memory", v)
		}
		if v, ok := c.Network.Get(); ok {
			args = append(args, "--network", v)
		}
		return args, nil
	case spec.CmdForward:
		args := append(opt([]string{"guest", "forward"}, c.Target), c.Ports...)
		if c.Proto != "" {
			args = append(args, "-p", c.Proto)
		}
		return args, nil
	case spec.CmdUnforward:
		return append(opt([]string{"guest", "unforward"}, c.Target), c.Ports...), nil
	case spec.CmdStart:
		return append([]string{"guest", "start"}, c.Names...), nil
	case spec.CmdStop:
		return append([]string{"guest", "stop"}, c.Names...), nil
	case spec.CmdRestart:
		return append([]string{"guest", "restart"}, c.Names...), nil
	case spec.CmdRm:
		args := []string{"guest", "rm"}
		if c.Yes {
			args = append(args, "-y")
		}
		return append(args, c.Names...), nil
	case spec.CmdMount:
		args := opt([]string{"guest", "mount"}, c.Target)
		if c.ReadOnly {
			args = append(args, "--ro")
		}
		return append(args, c.Dirs...), nil
	case spec.CmdUnmount:
		return append(opt([]string{"guest", "unmount"}, c.Target), c.Dirs...), nil
	case spec.CmdEdit:
		src := filepath.Join(h.home, ".cache", "antic-e2e", "new.toml")
		if err := os.WriteFile(src, []byte(renderTOML(c.New)), 0o644); err != nil {
			h.t.Fatal(err)
		}
		editor := h.edit
		if h.editorArgs {
			editor = "sh " + h.edit
		}
		return opt([]string{"guest", "edit"}, c.Target), []string{"EDITOR=" + editor, "VISUAL=", "E2E_EDIT_SRC=" + src}
	case spec.CmdRun:
		return append(opt([]string{"run"}, c.Target), "-T", "--", "sh", "-c", fmt.Sprintf("exit %d", c.Code)), nil
	}
	h.t.Fatalf("cannot render %#v", c)
	return nil, nil
}

func tomlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// renderTOML writes raw as a guest config file.
func renderTOML(raw spec.RawConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "packages = %s\n", tomlList(raw.Packages))
	b.WriteString("\n[spec]\n")
	if raw.CPUs != 0 {
		fmt.Fprintf(&b, "cpus = %d\n", raw.CPUs)
	}
	if raw.Memory != "" {
		fmt.Fprintf(&b, "memory = %q\n", raw.Memory)
	}
	b.WriteString("\n[network]\n")
	if raw.Network.Mode != "" {
		fmt.Fprintf(&b, "mode = %q\n", raw.Network.Mode)
	}
	if raw.Network.Default != "" {
		fmt.Fprintf(&b, "default = %q\n", raw.Network.Default)
	}
	if len(raw.Network.Allow) > 0 {
		fmt.Fprintf(&b, "allow = %s\n", tomlList(raw.Network.Allow))
	}
	if len(raw.Network.Deny) > 0 {
		fmt.Fprintf(&b, "deny = %s\n", tomlList(raw.Network.Deny))
	}
	for _, p := range raw.Ports {
		fmt.Fprintf(&b, "\n[[network.ports]]\nhost = %d\nguest = %d\n", p.Host, p.Guest)
		if p.Proto != "" {
			fmt.Fprintf(&b, "proto = %q\n", p.Proto)
		}
	}
	for _, s := range raw.Shares {
		fmt.Fprintf(&b, "\n[[shares]]\nsource = %q\n", s.Path)
		if s.Mode != "" {
			fmt.Fprintf(&b, "mode = %q\n", s.Mode)
		}
	}
	return b.String()
}

// fixtureConfig writes a guest config file directly (a guest the user
// configured by hand and never built: PhaseUncreated) and adds it to the model.
func (h *harness) fixtureConfig(name string, raw spec.RawConfig) {
	t := h.t
	n, err := domain.ParseGuestName(name)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.ParseConfig(h.env, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.cfg, name+".toml"), []byte(renderTOML(raw)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.model.Guests = h.model.Guests.Upsert(spec.Guest{Name: n, Config: cfg, Mounts: types.EmptySet[spec.Mount, domain.AbsPath, spec.MountByPath]()})
}
