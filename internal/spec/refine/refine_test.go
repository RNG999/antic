// Package refine_test checks that the implementation's command core
// (internal/tea/operation) refines the top-level specification
// (internal/spec): for every transition of the specification, the saga the
// core plans for the corresponding command touches exactly the resources the
// specification changes, and rejects exactly the commands the specification
// rejects for the guest's phase. Atomicity under faults is checked in
// internal/tea/operation itself.
package refine_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

const (
	dirA = "/home/u/w/A"
	dirB = "/home/u/w/B"
)

func abs(s string) domain.AbsPath {
	p, _ := domain.ParseAbsPath(s)
	return p
}

func env() spec.Env {
	return spec.Env{Home: abs("/home/u"), Dirs: []domain.AbsPath{abs(dirA), abs(dirB)}}
}

func alphabet() []spec.Cmd {
	n := "e2e-a"
	return []spec.Cmd{
		spec.CmdCreate{Name: n},
		spec.CmdApply{Target: n},
		spec.CmdInstall{Target: n, Packages: []string{"jq"}},
		spec.CmdUninstall{Target: n, Packages: []string{"jq"}},
		spec.CmdSet{Target: n, CPUs: types.Some(4)},
		spec.CmdForward{Target: n, Ports: []string{"8001"}},
		spec.CmdUnforward{Target: n, Ports: []string{"8001"}},
		spec.CmdStart{Names: []string{n}},
		spec.CmdStop{Names: []string{n}},
		spec.CmdRestart{Names: []string{n}},
		spec.CmdRm{Names: []string{n}, Yes: true},
		spec.CmdMount{Target: n, Dirs: []string{dirA}},
		spec.CmdMount{Target: n, Dirs: []string{dirB}, ReadOnly: true},
		spec.CmdUnmount{Target: n, Dirs: []string{dirA}},
		spec.CmdEdit{Target: n, New: spec.RawConfig{Packages: []string{"git", "jq"}, Shares: []spec.RawShare{{Path: dirB, Mode: "ro"}}}},
		spec.CmdEdit{Target: n, New: spec.RawConfig{Packages: []string{"git"}, CPUs: 4, Shares: []spec.RawShare{{Path: dirA}}}},
		spec.CmdRun{Target: n, Code: 0},
	}
}

// uncreated returns a world whose only guest has a config but no build.
func uncreated(t *testing.T) spec.World {
	outs := spec.Post(spec.World{}, env(), spec.CmdCreate{Name: "e2e-a"})
	w := outs[len(outs)-1].World
	g := w.Guests.All()[0]
	g.Phase, g.Mounts = spec.PhaseUncreated, spec.MountSet{}
	w.Guests = w.Guests.Upsert(g)
	if err := spec.Invariant(w); err != nil {
		t.Fatal(err)
	}
	return w
}

func phaseOf(w spec.World) (spec.Guest, operation.GuestPhase) {
	gs := w.Guests.All()
	if len(gs) == 0 {
		return spec.Guest{}, operation.GuestAbsent
	}
	switch gs[0].Phase {
	case spec.PhaseUncreated:
		return gs[0], operation.GuestUncreated
	case spec.PhaseOff:
		return gs[0], operation.GuestOff
	case spec.PhaseRunning:
	}
	return gs[0], operation.GuestRunning
}

func pathSet(ms []spec.Mount, keep func(spec.Mount) bool) operation.PathSet {
	var ps []domain.AbsPath
	for _, m := range ms {
		if keep(m) {
			ps = append(ps, m.Path)
		}
	}
	return types.BuildSet[domain.AbsPath, domain.AbsPath, domain.AbsPathOrder](ps)
}

func flag(b bool) operation.ChangeKind {
	if b {
		return operation.Changed
	}
	return operation.Unchanged
}

// opFor is the abstraction the shell implements: the core's input for
// running c in w, given the specification's successful outcome w2.
func opFor(c spec.Cmd, w, w2 spec.World) operation.Op {
	g, phase := phaseOf(w)
	g2, _ := phaseOf(w2)
	op := operation.Op{Phase: phase, Runtime: pathSet(g.Mounts.All(), func(m spec.Mount) bool { return m.Origin == spec.OriginRuntime })}
	switch c.(type) {
	case spec.CmdCreate:
		op.Kind = operation.OpCreate
		return op
	case spec.CmdStart, spec.CmdRun:
		op.Kind = operation.OpStart
		return op
	case spec.CmdStop:
		op.Kind = operation.OpStop
		return op
	case spec.CmdRestart:
		op.Kind = operation.OpRestart
		return op
	case spec.CmdRm:
		op.Kind = operation.OpRemove
		return op
	}
	op.Kind = operation.OpChangeConfig
	built := g.Phase != spec.PhaseUncreated
	restart := g.Phase == spec.PhaseRunning && (!g.Config.Res.Equal(g2.Config.Res) ||
		!slices.Equal(g.Config.Ports.All(), g2.Config.Ports.All()) || rebooted(g, g2))
	op.Change = operation.Change{
		Config:  flag(!g.Config.Equal(g2.Config)),
		System:  flag(!slices.Equal(g.Config.Packages.All(), g2.Config.Packages.All()) && built || !built && g2.Phase != spec.PhaseUncreated),
		Restart: flag(restart),
	}
	if g.Phase == spec.PhaseRunning && !restart {
		old := g.Mounts
		op.Change.Added = pathSet(g2.Mounts.All(), func(m spec.Mount) bool { return !old.Contains(m.Path) })
		op.Change.Removed = pathSet(g.Mounts.All(), func(m spec.Mount) bool { return !g2.Mounts.Contains(m.Path) })
	}
	return op
}

// rebooted reports whether the running guest's mounts were reset to its
// config's boot mounts (a runtime mount became a boot mount, or a lingering
// boot mount disappeared).
func rebooted(g, g2 spec.Guest) bool {
	for _, m := range g.Mounts.All() {
		m2, ok := g2.Mounts.Lookup(m.Path)
		if m.Origin == spec.OriginRuntime && ok && m2.Origin == spec.OriginBoot {
			return true
		}
		if m.Origin == spec.OriginBoot && !g.Config.Shares.Contains(m.Path) && !ok {
			return true
		}
	}
	return false
}

// footprint is the set of resources the specification says c changes.
func footprint(c spec.Cmd, w, w2 spec.World) []string {
	g, phase := phaseOf(w)
	g2, phase2 := phaseOf(w2)
	var out []string
	add := func(s string) { out = append(out, s) }
	if _, rm := c.(spec.CmdRm); rm {
		if phase == operation.GuestRunning {
			add("power")
		}
		add("config")
		if phase != operation.GuestUncreated {
			add("instance")
		}
		slices.Sort(out)
		return out
	}
	if phase == operation.GuestAbsent || !g.Config.Equal(g2.Config) {
		add("config")
	}
	built := phase != operation.GuestAbsent && phase != operation.GuestUncreated
	if !built && phase2 != operation.GuestUncreated && phase2 != operation.GuestAbsent {
		add("instance")
		add("profile")
	} else if built && !slices.Equal(g.Config.Packages.All(), g2.Config.Packages.All()) {
		add("profile")
	}
	op := opFor(c, w, w2)
	restart := op.Kind == operation.OpRestart || op.Change.Restart == operation.Changed
	if (phase == operation.GuestRunning) != (phase2 == operation.GuestRunning) || (phase == operation.GuestRunning && restart) {
		add("power")
	} else if phase == operation.GuestRunning {
		if !slices.Equal(g.Config.Packages.All(), g2.Config.Packages.All()) {
			add("system")
		}
		for _, p := range op.Change.Added.All() {
			add("mount " + p.Get())
		}
		for _, p := range op.Change.Removed.All() {
			add("mount " + p.Get())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func resourceName(r operation.Resource) string {
	switch r.Kind {
	case operation.ResConfig:
		return "config"
	case operation.ResInstance:
		return "instance"
	case operation.ResProfile:
		return "profile"
	case operation.ResSystem:
		return "system"
	case operation.ResPower:
		return "power"
	case operation.ResMount:
	}
	return "mount " + r.Path.Value().Get()
}

// runCore runs op's saga without faults and returns its status and the
// resources it changed.
func runCore(t *testing.T, op operation.Op) (operation.ReportStatus, []string) {
	t.Helper()
	name, _ := domain.ParseGuestName("e2e-a")
	m, effects := operation.Update(operation.Init(name), operation.MsgCommand{Op: op})
	for range 100 {
		if rep, ok := effects[0].(operation.EffectReport); ok {
			var touched []string
			for r := range m.Versions() {
				touched = append(touched, resourceName(r))
			}
			slices.Sort(touched)
			return rep.Status, touched
		}
		m, effects = operation.Update(m, operation.MsgResult{Status: operation.ResultOk})
	}
	t.Fatalf("%+v: saga did not terminate", op)
	return 0, nil
}

func TestCoreRefinesSpec(t *testing.T) {
	starts := []spec.World{{}, uncreated(t)}
	seen := map[string]bool{}
	frontier := starts
	checked := 0
	for range 3 {
		var next []spec.World
		for _, w := range frontier {
			for _, c := range alphabet() {
				outs := spec.Post(w, env(), c)
				last := outs[len(outs)-1]
				if last.Kind == spec.OutcomeReject {
					// Rejections the core decides from the phase alone must
					// agree; the rest are found by its Prepare steps.
					op := opFor(c, w, w)
					if status, touched := runCore(t, op); status == operation.ReportOk && len(touched) > 0 {
						t.Fatalf("%#v in %v: the specification rejects it, the core changed %v", c, w, touched)
					}
					continue
				}
				// Every permitted success (a running guest may switch or,
				// should the kernel change, restart) is one the core can do.
				for _, o := range outs {
					if o.Kind != spec.OutcomeOk {
						continue
					}
					op := opFor(c, w, o.World)
					status, touched := runCore(t, op)
					if status != operation.ReportOk {
						t.Fatalf("%#v: the specification accepts it, the core rejected %+v", c, op)
					}
					if want := footprint(c, w, o.World); !slices.Equal(touched, want) {
						t.Fatalf("%#v from %v:\n  core changed %v\n  spec changed %v\n  op %+v", c, w, touched, want, op)
					}
					checked++
					if k := fmt.Sprintf("%v", o.World); !seen[k] {
						seen[k] = true
						next = append(next, o.World)
					}
				}
			}
		}
		frontier = next
	}
	t.Logf("checked %d accepted transitions over %d worlds", checked, len(seen))
}
