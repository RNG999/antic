//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/spec"
)

// TestE2EFlakeKernelChange: a guest built from its own flake (outside the
// model, which only knows package lists) runs, and applying a new system
// that boots differently (other kernel parameters) restarts it instead of
// switching: the guest runs the new parameters, its sessions end and its
// shares are mounted again. Applying a system that boots the same switches
// and keeps them.
//
// The flake takes antic's guest module from the pushed branch: a local path
// would be a flake input inside a directory another guest shares, which
// antic refuses to build.
func TestE2EFlakeKernelChange(t *testing.T) {
	h := newHarness(t)
	const name = prefix + "k"
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ref := os.Getenv("ANTIC_E2E_FLAKE_REF")
	if ref == "" {
		ref = "github:RNG999/antic/feat/e2e-spec"
	}
	dir := h.path("F")
	writeFlake := func(param string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		flake := fmt.Sprintf(`{
  inputs.antic.url = %q;
  inputs.nixpkgs.follows = "antic/nixpkgs";
  outputs = { antic, nixpkgs, ... }: {
    nixosConfigurations.%s = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        antic.nixosModules.guest
        { antic.user = %q; boot.kernelParams = [ %q ]; }
      ];
    };
  };
}
`, ref, name, u.Username, param)
		if err := os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(flake), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(h.cfg, name+".toml")
	defer func() {
		h.cleanup()
		os.RemoveAll(dir)
	}()
	writeFlake("antic.e2e=1")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("flake = %q\n\n[spec]\nmemory = \"1G\"\n", dir+"#"+name)), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string) {
		out, code, stderr := h.antic(nil, args...)
		if code != 0 {
			t.Fatalf("antic %v: exit %d\n%s", args, code, stderr)
		}
		return out, stderr
	}
	cmdline := func() string {
		out, _ := run("run", name, "-T", "--", "cat", "/proc/cmdline")
		return out
	}
	mounted := func(p string) bool {
		info, err := h.show(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range info.Shares {
			if s.Path == p && s.Mounted {
				return true
			}
		}
		return false
	}
	session := func(cmd string) bool {
		ss, err := h.sessions(name)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := ss[cmd]
		return ok
	}

	run("guest", "create", name)
	if !strings.Contains(cmdline(), "antic.e2e=1") {
		t.Fatalf("the flake's kernel parameter is not booted: %q", cmdline())
	}
	run("guest", "mount", name, h.path("A"))
	if _, code, stderr := h.spawn(spec.CmdSpawn{Target: name, Command: "sleep 6100"}); code != 0 {
		t.Fatalf("spawn: exit %d: %s", code, stderr)
	}

	// The same boot: a switch keeps the session and the hot-plugged share.
	if _, stderr := run("guest", "apply", name); strings.Contains(stderr, "restarts") {
		t.Fatalf("an apply that boots the same restarted the guest:\n%s", stderr)
	}
	if !session("sleep 6100") || !mounted(h.path("A")) {
		t.Fatal("an apply that boots the same lost the session or the share")
	}

	// Other kernel parameters: a restart.
	writeFlake("antic.e2e=2")
	_, stderr := run("guest", "apply", name)
	if !strings.Contains(stderr, "restarts to take it") {
		t.Fatalf("an apply that boots differently did not restart:\n%s", stderr)
	}
	if got := cmdline(); !strings.Contains(got, "antic.e2e=2") {
		t.Fatalf("the guest does not run the new kernel parameters: %q", got)
	}
	if session("sleep 6100") {
		t.Fatal("the restart left the session running")
	}
	if !mounted(h.path("A")) {
		t.Fatal("the restart lost the share")
	}
}
