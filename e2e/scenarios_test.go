//go:build e2e

package e2e

import (
	"fmt"
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
	args, env := h.render(c)
	_, code, stderr := h.antic(env, args...)
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
// specification (docs/design/e2e-spec.md §9) with its last command: it is
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
			name:  "D16 mount the same directory again",
			known: "D16",
			cmds: func(h *harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
					spec.CmdMount{Target: a, Dirs: []string{h.path("A")}},
				}
			},
		},
		{
			name:  "D3 create flags on an existing config",
			known: "D3",
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
					spec.CmdApply{Target: b},
					spec.CmdStart{Names: []string{b}},
					spec.CmdRm{Names: []string{b}, Yes: true},
				}
			},
		},
		{
			name:  "D5 unbuildable package",
			known: "D5",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdInstall{Target: a, Packages: []string{"e2e-no-such-package"}},
				}
			},
		},
		{
			name:  "D7 host port used by another guest",
			known: "D7",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a, "8001"),
					create(b),
					spec.CmdForward{Target: b, Ports: []string{"8001"}},
				}
			},
		},
		{
			name:  "D8 multi-guest start stops at the first failure",
			known: "D8",
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
			name:  "D9 edit of a running guest",
			known: "D9",
			cmds: func(*harness) []spec.Cmd {
				return []spec.Cmd{
					create(a),
					spec.CmdEdit{Target: a, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 4, Memory: "1G"}},
				}
			},
		},
		{
			name:  "D10 invalid edit",
			known: "D10",
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
			known:      "D12",
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
	base := h.model
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			h.t = t
			h.cleanup()
			h.model, h.editorArgs = base, sc.editorArgs
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
