//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

// step runs c for real and checks it against the specification:
//
//	O0  antic finishes (cmdTimeout) and its JSON parses (observe)
//	O1  its exit status is one a permitted outcome allows
//	O2  the observed World (CLI output and config files) matches that outcome
//	O3  running guests see the vCPU count the outcome says (probeCPUs)
//
// It returns "" on success, or a description of the mismatch.
func (h *harness) step(c spec.Cmd) string {
	outs := spec.Post(h.model, h.env, c)
	if outs == nil {
		h.t.Fatalf("%#v is outside the model", c)
	}
	var args []string
	var code int
	var stderr string
	switch c := c.(type) {
	case spec.CmdSpawn:
		args, code, stderr = h.spawn(c)
	case spec.CmdKill:
		args, code, stderr = h.kill(c)
	case spec.CmdShell:
		// An interactive shell reading its commands from standard input.
		args = append([]string{"shell"}, c.Target)
		_, code, stderr = h.anticWith(h.work, fmt.Sprintf("exit %d\n", c.Code), nil, args...)
	default:
		var env []string
		args, env = h.render(c)
		_, code, stderr = h.antic(env, args...)
	}
	cmdline := "antic " + strings.Join(args, " ")
	obs, err := h.observe()
	if err != nil {
		return fmt.Sprintf("%s (exit %d): observation failed: %v\nstderr: %s", cmdline, code, err, stderr)
	}
	var why []string
	for _, o := range outs {
		if o.Exit() != code {
			why = append(why, fmt.Sprintf("  outcome %s: exit %d, not %d", kind(o), o.Exit(), code))
			continue
		}
		if d := diff(o.World, obs); d != "" {
			why = append(why, fmt.Sprintf("  outcome %s:\n    %s", kind(o), strings.ReplaceAll(d, "\n", "\n    ")))
			continue
		}
		if o.Kind == spec.OutcomeEnvFail {
			return fmt.Sprintf("%s: environmental failure (permitted, but must be investigated without fault injection)\nstderr: %s", cmdline, stderr)
		}
		h.model = o.World
		return ""
	}
	return fmt.Sprintf("%s: exit %d matches no permitted outcome\n%s\nstderr: %s", cmdline, code, strings.Join(why, "\n"), stderr)
}

func kind(o spec.Outcome) string {
	switch o.Kind {
	case spec.OutcomeOk:
		return "ok"
	case spec.OutcomeReject:
		return "reject (" + o.Reason + ")"
	case spec.OutcomeEnvFail:
	}
	return "envfail"
}

// scenario is a command sequence. A scenario with known set reproduces a
// documented divergence of the current implementation from the
// specification (docs/design/spec.md §9) with its last command: it is
// skipped while that command still diverges, and fails once the divergence
// is fixed so the marker gets removed.
type scenario struct {
	name       string
	known      string
	editorArgs bool
	setup      func(h *harness)
	cmds       func(h *harness) []spec.Cmd
}

const (
	a = "e2e-a"
	b = "e2e-b"
)

// create makes a small guest (1 GiB keeps several guests affordable).
func create(name string, ports ...string) spec.CmdCreate {
	return spec.CmdCreate{Name: name, Memory: types.Some("1G"), Ports: ports}
}

func scenarios() []scenario {
	return []scenario{
		{
			name: "lifecycle",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdCreate{Name: "Bad_Name"},
					spec.CmdStop{Names: []string{a}},
					spec.CmdStart{Names: []string{a}},
					spec.CmdRun{Target: a, Code: 42},
					spec.CmdRestart{Names: []string{a}},
					spec.CmdInstall{Target: a, Packages: []string{"jq"}},
					spec.CmdUninstall{Target: a, Packages: []string{"jq"}},
					spec.CmdStop{Names: []string{a}},
					spec.CmdApply{Target: a},
					spec.CmdRun{Target: a, Code: 0},
					spec.CmdRm{Names: []string{a}},
					spec.CmdRm{Names: []string{a}, Yes: true},
				}
			},
		},
		{
			name: "shares",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("A/sub")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("LB")}, ReadOnly: true},
					spec.CmdUnmount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdRestart{Names: []string{a}},
					spec.CmdUnmount{Target: a, Dirs: []string{h.path("LB")}},
					spec.CmdRestart{Names: []string{a}},
					spec.CmdMount{Target: a, Dirs: []string{h.home + "/.local"}},
					spec.CmdMount{Target: a, Dirs: []string{h.home}},
				}
			},
		},
		{
			name: "sessions",
			cmds: func(h *harness) []spec.Cmd {
				spawn := func(cmd string) spec.Cmd { return spec.CmdSpawn{Target: a, Command: cmd} }
				kill := func(cmd string) spec.Cmd { return spec.CmdKill{Target: a, Commands: []string{cmd}} }
				return []spec.Cmd{
					create(a),
					spawn("sleep 6001"),
					spawn("sleep 6002"),
					kill("sleep 6001"),
					kill("sleep 6001"),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdSet{Target: a, CPUs: types.Some(1)},
					spawn("sleep 6003"),
					spec.CmdStop{Names: []string{a}},
					spawn("sleep 6004"),
					spec.CmdRestart{Names: []string{a}},
				}
			},
		},
		{
			name: "inspection changes nothing, shell passes its status",
			cmds: func(*harness) []spec.Cmd {
				inspect := func(what spec.Inspection) spec.Cmd { return spec.CmdInspect{What: what, Target: a} }
				all := func() []spec.Cmd {
					return []spec.Cmd{inspect(spec.InspectLs), inspect(spec.InspectShow), inspect(spec.InspectStats),
						inspect(spec.InspectLogs), inspect(spec.InspectDoctor), inspect(spec.InspectCacheLs)}
				}
				var cmds []spec.Cmd
				cmds = append(cmds, inspect(spec.InspectShow)) // no such guest: rejected
				cmds = append(cmds, create(a))
				cmds = append(cmds, all()...)
				cmds = append(cmds, spec.CmdShell{Target: a, Code: 7}, spec.CmdStop{Names: []string{a}})
				cmds = append(cmds, all()...)
				cmds = append(cmds, spec.CmdShell{Target: a, Code: 0}) // starts the stopped guest
				return cmds
			},
		},
		{
			name: "D2 mount with another mode",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}, ReadOnly: true},
				}
			},
		},
		{
			name: "D16 mount the same directory again",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
				}
			},
		},
		{
			name: "mount several directories stops at the first failure",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A"), h.path("missing"), h.path("B")}},
					spec.CmdUnmount{Target: a, Dirs: []string{h.path("A"), h.path("B")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}, ReadOnly: true},
				}
			},
		},
		{
			name:  "D3 create flags on an existing config",
			setup: func(h *harness) { h.fixtureConfig(b, spec.RawConfig{Packages: []string{"git"}, Memory: "1G"}) },
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{spec.CmdCreate{Name: b, Packages: []string{"jq"}}}
			},
		},
		{
			name:  "D6 uncreated guest",
			setup: func(h *harness) { h.fixtureConfig(b, spec.RawConfig{Packages: []string{"git"}, Memory: "1G"}) },
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					spec.CmdStop{Names: []string{b}},
					spec.CmdStart{Names: []string{b}},
					spec.CmdInspect{What: spec.InspectLogs, Target: b},
					spec.CmdInspect{What: spec.InspectShow, Target: b},
					spec.CmdApply{Target: b},
					spec.CmdStart{Names: []string{b}},
					spec.CmdRm{Names: []string{b}, Yes: true},
				}
			},
		},
		{
			name: "D5 unbuildable package",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdInstall{Target: a, Packages: []string{"e2e-no-such-package"}},
				}
			},
		},
		{
			name: "D7 host port used by another guest",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a, "8001"),
					create(b),
					spec.CmdForward{Target: b, Ports: []string{"8001"}},
				}
			},
		},
		{
			name: "reconfigure a running guest atomically",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a, "8001"),
					spec.CmdForward{Target: a, Ports: []string{fmt.Sprint(busyPort)}},
					spec.CmdForward{Target: a, Ports: []string{"8001"}},
					spec.CmdSet{Target: a, CPUs: types.Some(2)},
					spec.CmdUnforward{Target: a, Ports: []string{"8001"}},
					spec.CmdRun{Target: a, Code: 0},
				}
			},
		},
		{
			name: "D19 set zero cpus",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{create(a), spec.CmdSet{Target: a, CPUs: types.Some(0)}}
			},
		},
		{
			name: "D8 multi-guest start stops at the first failure",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					create(b),
					spec.CmdStop{Names: []string{a, b}},
					spec.CmdForward{Target: a, Ports: []string{fmt.Sprint(busyPort)}},
					spec.CmdStart{Names: []string{a, b}},
				}
			},
		},
		{
			name: "D9 edit of a running guest",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 4, Memory: "1G"}},
				}
			},
		},
		{
			name: "D10 invalid edit",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 999, Memory: "1G"}},
				}
			},
		},
		{
			name: "network default deny boots",
			setup: func(h *harness) {
				h.fixtureConfig(b, spec.RawConfig{Packages: []string{"git"}, Memory: "1G", Network: spec.RawNetwork{
					Default: "deny", Allow: []string{"one.one.one.one", "192.168.1.1/32"}, Deny: []string{"*.evil.example"}}})
			},
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{spec.CmdApply{Target: b}, spec.CmdStart{Names: []string{b}}, spec.CmdRun{Target: b, Code: 0}}
			},
		},
		{
			name: "network lan with exceptions boots",
			setup: func(h *harness) {
				h.fixtureConfig(b, spec.RawConfig{Packages: []string{"git"}, Memory: "1G", Network: spec.RawNetwork{
					Mode: "lan", Default: "allow", Allow: []string{"10.1.2.0/24"}, Deny: []string{"192.168.1.0/24", "*.evil.example"}}})
			},
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{spec.CmdApply{Target: b}, spec.CmdStart{Names: []string{b}}, spec.CmdRun{Target: b, Code: 0}}
			},
		},
		{
			name: "D11 network default",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, Memory: "1G",
						Network: spec.RawNetwork{Default: "deny", Allow: []string{"one.one.one.one"}}}},
				}
			},
		},
		{
			name:       "D12 EDITOR with arguments",
			editorArgs: true,
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, Memory: "1G"}},
				}
			},
		},
	}
}

func TestE2E(t *testing.T) {
	h := newHarness(t)
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model, h.editorArgs = h.base, sc.editorArgs
			defer h.cleanup()
			if sc.setup != nil {
				sc.setup(h)
			}
			cmds := sc.cmds(h)
			for i, c := range cmds {
				msg := h.step(c)
				if msg == "" {
					continue
				}
				// A known divergence is demonstrated by the last command; an
				// earlier failure is a real one.
				if sc.known != "" && i == len(cmds)-1 {
					t.Skipf("known divergence %s reproduced at step %d:\n%s", sc.known, i+1, msg)
				}
				t.Fatalf("step %d: %s", i+1, msg)
			}
			if sc.known != "" {
				t.Errorf("%s no longer reproduces; remove its known marker", sc.known)
			}
		})
	}
	h.t = t
}

// TestE2ECrashRecovery kills antic (ANTIC_SAGA_CRASH_AFTER) right after
// chosen effects of a command's saga and checks P1 across the crash: the
// next command on the guest recovers its journal, after which the guest is
// observed exactly as before the killed command; the command then works.
func TestE2ECrashRecovery(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name   string
		cmd    func(*harness) spec.Cmd
		points []int // effects executed before the crash
	}{
		// Validate, JournalBegin, Intent, WriteConfig, Intent, Hotplug, JournalClear.
		{"mount on a running guest", func(h *harness) spec.Cmd {
			return spec.CmdMount{Target: a, Dirs: []string{h.path("A")}}
		}, []int{2, 4, 6}},
		// Validate, CheckPorts, JournalBegin, Intent, WriteConfig, Intent,
		// StopVM, Intent, StartVM, JournalClear.
		{"set restarting a running guest", func(*harness) spec.Cmd {
			return spec.CmdSet{Target: a, CPUs: types.Some(3)}
		}, []int{5, 7, 9}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model = h.base
			defer h.cleanup()
			if msg := h.step(create(a)); msg != "" {
				t.Fatal(msg)
			}
			for _, k := range c.points {
				args, env := h.render(c.cmd(h))
				_, code, stderr := h.antic(append(env, fmt.Sprintf("ANTIC_SAGA_CRASH_AFTER=%d", k)), args...)
				if code != 137 {
					t.Fatalf("crash after effect %d: exit %d, want 137\nstderr: %s", k, code, stderr)
				}
				if msg := h.step(spec.CmdStart{Names: []string{a}}); msg != "" {
					t.Fatalf("recovery after a crash after effect %d: %s", k, msg)
				}
			}
			if msg := h.step(c.cmd(h)); msg != "" {
				t.Fatalf("after recovery: %s", msg)
			}
		})
	}
	h.t = t
}

// TestE2EFaultInjection fails one effect of a command's saga
// (ANTIC_SAGA_FAIL) on a running guest and checks P1 on the real host: the
// command exits 125 and the guest is observed exactly as before, including
// the system it runs and the system its profile installs (D1).
func TestE2EFaultInjection(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name string
		cmd  func(*harness) spec.Cmd
		fail string
	}{
		{"install whose switch fails (D1)", func(*harness) spec.Cmd {
			return spec.CmdInstall{Target: a, Packages: []string{"jq"}}
		}, "Switch"},
		{"set whose restart fails to boot", func(*harness) spec.Cmd {
			return spec.CmdSet{Target: a, CPUs: types.Some(3)}
		}, "StartVM"},
		{"mount whose hot-plug fails", func(h *harness) spec.Cmd {
			return spec.CmdMount{Target: a, Dirs: []string{h.path("A")}}
		}, "Hotplug"},
		{"forward whose journal cannot be cleared", func(*harness) spec.Cmd {
			return spec.CmdForward{Target: a, Ports: []string{"8001"}}
		}, "JournalClear"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model = h.base
			defer h.cleanup()
			if msg := h.step(create(a)); msg != "" {
				t.Fatal(msg)
			}
			before, err := h.show(a)
			if err != nil {
				t.Fatal(err)
			}
			args, env := h.render(c.cmd(h))
			_, code, stderr := h.antic(append(env, "ANTIC_SAGA_FAIL="+c.fail), args...)
			if code != 125 || !strings.Contains(stderr, "injected failure") {
				t.Fatalf("exit %d, want 125 with the injected failure\nstderr: %s", code, stderr)
			}
			obs, err := h.observe()
			if err != nil {
				t.Fatal(err)
			}
			if d := diff(h.model, obs); d != "" {
				t.Fatalf("the failed command changed the guest (P1):\n%s\nstderr: %s", d, stderr)
			}
			after, err := h.show(a)
			if err != nil {
				t.Fatal(err)
			}
			if after.System != before.System {
				t.Fatalf("the running system changed from %s to %s", before.System, after.System)
			}
			if msg := h.step(spec.CmdStop{Names: []string{a}}); msg != "" {
				t.Fatal(msg)
			}
			if stopped, err := h.show(a); err != nil || stopped.System != before.System {
				t.Fatalf("the installed profile is %s, not %s (D1): %v", stopped.System, before.System, err)
			}
			// The command works without the injected failure.
			if msg := h.step(spec.CmdStart{Names: []string{a}}); msg != "" {
				t.Fatal(msg)
			}
			if msg := h.step(c.cmd(h)); msg != "" {
				t.Fatal(msg)
			}
		})
	}
	h.t = t
}

// TestE2EBrokenConfig: a running guest whose config file no longer loads
// is listed (with its error) by "guest ls" and removed by "guest rm",
// after which its name can be created again (D17; outside the model).
func TestE2EBrokenConfig(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if msg := h.step(create(a)); msg != "" {
		t.Fatal(msg)
	}
	path := filepath.Join(h.cfg, a+".toml")
	if err := os.WriteFile(path, []byte("packages = [\"git\"]\n[spec]\ncpus = \"many\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code, stderr := h.antic(nil, "guest", "ls")
	if code != 0 || !strings.Contains(out, a) || !strings.Contains(out, "error") {
		t.Fatalf("guest ls with a broken config: exit %d\n%s%s", code, out, stderr)
	}
	if _, code, stderr := h.antic(nil, "guest", "rm", a, "-y"); code != 0 {
		t.Fatalf("guest rm of a broken config: exit %d\n%s", code, stderr)
	}
	if out, _, _ := h.antic(nil, "guest", "ls", "-q"); strings.Contains(out, a) {
		t.Fatalf("guest %s is still listed after rm:\n%s", a, out)
	}
	h.model = h.base
	if msg := h.step(create(a)); msg != "" {
		t.Fatalf("creating the name again: %s", msg)
	}
}
