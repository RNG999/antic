package spec

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// OutcomeKind classifies one permitted result of a command.
type OutcomeKind uint8

const (
	// OutcomeOk means the command succeeded.
	OutcomeOk OutcomeKind = iota
	// OutcomeReject means a precondition was false: exit 125 and, for the
	// element that failed, nothing changed (P1). Rejections are
	// deterministic, so a run that should be rejected has no other outcome.
	OutcomeReject
	// OutcomeEnvFail means the element did real work (build, boot, hotplug)
	// that failed for environmental reasons: exit 125 and nothing changed for
	// that element (P1). Without fault injection, a run that lands here
	// must be investigated.
	OutcomeEnvFail
)

// Outcome is one permitted result of a command.
type Outcome struct {
	World World
	Kind  OutcomeKind
	// Code is the exit status of the guest command (CmdRun only).
	Code int
	// Reason explains a rejection.
	Reason string
}

// Exit returns the antic process's exit status.
func (o Outcome) Exit() int {
	if o.Kind == OutcomeOk {
		return o.Code
	}
	return 125
}

// step is the effect of one element of a command.
type step struct {
	w   World
	err error
	// risky means the element does real work that can fail for
	// environmental reasons.
	risky bool
	// also are further permitted results: a running guest whose new system
	// changes the kernel restarts instead of switching (the specification
	// does not model kernels, so either may happen).
	also []World
}

// fold applies elements in order and stops at the first rejection (P2).
// Elements that already succeeded stay applied.
// An element with alternative results (step.also) branches the fold.
func fold[E any](w World, elems []E, f func(World, E) step) []Outcome {
	var out []Outcome
	curs := []World{w}
	for _, e := range elems {
		var next []World
		for _, cur := range curs {
			s := f(cur, e)
			if s.err != nil {
				out = append(out, Outcome{World: cur, Kind: OutcomeReject, Reason: s.err.Error()})
				continue
			}
			if s.risky {
				out = append(out, Outcome{World: cur, Kind: OutcomeEnvFail})
			}
			next = append(append(next, s.w), s.also...)
		}
		curs = next
	}
	for _, cur := range curs {
		out = append(out, Outcome{World: cur, Kind: OutcomeOk})
	}
	return out
}

// newSystem is the step that installs g's (changed) config with a new
// system: a running guest switches to it, or restarts when its kernel
// changes.
func newSystem(w World, g Guest) step {
	s := step{w: w.with(g), risky: true}
	if g.Phase == PhaseRunning {
		s.also = []World{w.with(boot(g))}
	}
	return s
}

func reject(err error) step { return step{err: err} }

func (w World) with(g Guest) World {
	w.Guests = w.Guests.Upsert(g)
	return w
}

// resolveName resolves an optional NAME argument to a defined guest name.
func (w World) resolveName(t string) (domain.GuestName, error) {
	if t == "" {
		ns := w.names()
		switch len(ns) {
		case 0:
			return domain.GuestName{}, errors.New("no guests defined")
		case 1:
			return ns[0], nil
		}
		return domain.GuestName{}, errors.New("guest NAME is required")
	}
	name, err := domain.ParseGuestName(t)
	if err != nil {
		return name, fmt.Errorf("no guest named %q", t)
	}
	return name, nil
}

// lookup returns the managed guest an optional NAME argument selects.
func (w World) lookup(t string) (Guest, error) {
	name, err := w.resolveName(t)
	if err != nil {
		return Guest{}, err
	}
	g, ok := w.Guests.Lookup(name)
	if !ok {
		return g, fmt.Errorf("no guest named %q", name.Get())
	}
	return g, nil
}

// external reports whether the NAME arguments select an external guest; such
// commands are outside the model (the test never issues them).
func (w World) external(ts ...string) bool {
	for _, t := range ts {
		if name, err := w.resolveName(t); err == nil && w.External.Contains(name) {
			return true
		}
	}
	return false
}

// boot returns g freshly (re)booted with its config.
func boot(g Guest) Guest {
	g.Phase = PhaseRunning
	g.Mounts = bootMounts(g.Config)
	g.Sessions = types.EmptySet[Session, string, SessionByCommand]()
	return g
}

// Post returns every permitted outcome of running c in w. It returns nil
// only for commands outside the model (those that select an external guest).
func Post(w World, env Env, c Cmd) []Outcome {
	switch c := c.(type) {
	case CmdCreate:
		return postCreate(w, env, c)
	case CmdApply:
		return postApply(w, env, c)
	case CmdInstall:
		return postPackages(w, env, c.Target, c.Packages, true)
	case CmdUninstall:
		return postPackages(w, env, c.Target, c.Packages, false)
	case CmdSet:
		return postSet(w, env, c)
	case CmdForward:
		return postForward(w, env, c)
	case CmdUnforward:
		return postUnforward(w, env, c)
	case CmdStart:
		return postPower(w, env, c.Names, startGuest)
	case CmdStop:
		return postPower(w, env, c.Names, stopGuest)
	case CmdRestart:
		return postPower(w, env, c.Names, restartGuest)
	case CmdRm:
		return postRm(w, c)
	case CmdMount:
		return postMount(w, env, c)
	case CmdUnmount:
		return postUnmount(w, env, c)
	case CmdEdit:
		return postEdit(w, env, c)
	case CmdRun:
		return postRun(w, env, c)
	case CmdShell:
		return postRun(w, env, CmdRun(c))
	case CmdInspect:
		return postInspect(w, c)
	case CmdSpawn:
		return postSpawn(w, env, c)
	case CmdKill:
		return postKill(w, c)
	}
	return nil
}

func postCreate(w World, env Env, c CmdCreate) []Outcome {
	if name, err := domain.ParseGuestName(c.Name); err == nil && w.External.Contains(name) {
		return nil
	}
	return fold(w, []CmdCreate{c}, func(w World, c CmdCreate) step {
		name, err := domain.ParseGuestName(c.Name)
		if err != nil {
			return reject(err)
		}
		flags := len(c.Packages) > 0 || c.CPUs.IsSome() || c.Memory.IsSome() || c.Network.IsSome() || len(c.Ports) > 0
		g, exists := w.Guests.Lookup(name)
		switch {
		case exists && g.Phase != PhaseUncreated:
			return reject(fmt.Errorf("guest %s already exists", name.Get()))
		case exists && flags:
			// D3: the config exists, so the flags would be silently ignored (P3).
			return reject(fmt.Errorf("guest %s already has a config; edit it instead of passing flags", name.Get()))
		case !exists:
			raw := defaults()
			if len(c.Packages) > 0 {
				raw.Packages = c.Packages
			}
			if v, ok := c.CPUs.Get(); ok {
				raw.CPUs = v
			}
			if v, ok := c.Memory.Get(); ok {
				raw.Memory = v
			}
			raw.Network.Mode = c.Network.Value()
			cfg, err := parseConfig(w, env, name, raw)
			if err != nil {
				return reject(err)
			}
			var ports []Port
			for _, spec := range c.Ports {
				p, err := parsePortSpec(spec, "")
				if err != nil {
					return reject(err)
				}
				ports = append(ports, p)
			}
			if len(ports) > 0 && cfg.Res.Net.Mode == NetNone {
				return reject(errors.New("port forwards cannot be used when network.mode is \"none\""))
			}
			if cfg.Ports, err = addPorts(cfg.Ports, ports, w.otherPorts(name)); err != nil {
				return reject(err)
			}
			g = Guest{Name: name, Config: cfg}
		}
		if err := buildable(env, g.Config.Packages); err != nil {
			return reject(err)
		}
		if busy(env, g.Config.Ports) {
			return reject(errors.New("a forwarded host port is in use"))
		}
		return step{w: w.with(boot(g)), risky: true}
	})
}

func postApply(w World, env Env, c CmdApply) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []string{c.Target}, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		if err := buildable(env, g.Config.Packages); err != nil {
			return reject(err)
		}
		if g.Phase == PhaseUncreated {
			g.Phase = PhaseOff
		}
		return newSystem(w, g)
	})
}

func postPackages(w World, env Env, target string, items []string, add bool) []Outcome {
	if w.external(target) {
		return nil
	}
	return fold(w, []string{target}, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		if len(items) == 0 {
			return reject(errors.New("PKG is required"))
		}
		pkgs, err := parsePackages(items)
		if err != nil {
			return reject(err)
		}
		next := g.Config.Packages.Clone()
		for _, p := range pkgs.All() {
			if add {
				next = next.Upsert(p)
			} else {
				next = next.Remove(p)
			}
		}
		if slices.Equal(next.All(), g.Config.Packages.All()) && g.Phase != PhaseUncreated {
			return step{w: w} // P4
		}
		if err := buildable(env, next); err != nil {
			return reject(err)
		}
		g.Config.Packages = next
		if g.Phase == PhaseUncreated {
			g.Phase = PhaseOff
		}
		return newSystem(w, g)
	})
}

// reconfigure replaces g's config with cfg, restarting g if it is running.
func reconfigure(w World, env Env, g Guest, cfg Config) step {
	if cfg.Equal(g.Config) {
		return step{w: w} // P4
	}
	g.Config = cfg
	if g.Phase != PhaseRunning {
		return step{w: w.with(g)}
	}
	if busy(env, cfg.Ports) {
		return reject(errors.New("a forwarded host port is in use"))
	}
	return step{w: w.with(boot(g)), risky: true}
}

func postSet(w World, env Env, c CmdSet) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []CmdSet{c}, func(w World, c CmdSet) step {
		g, err := w.lookup(c.Target)
		if err != nil {
			return reject(err)
		}
		if !c.CPUs.IsSome() && !c.Memory.IsSome() && !c.Network.IsSome() {
			return reject(errors.New("specify at least one of --cpus, --memory, or --network"))
		}
		cfg := g.Config
		if v, ok := c.CPUs.Get(); ok {
			if cfg.Res.CPUs, err = domain.ParseCPUCount(v); err != nil {
				return reject(err)
			}
		}
		if v, ok := c.Memory.Get(); ok {
			n, err := parseSize(v)
			if err != nil {
				return reject(err)
			}
			if cfg.Res.Memory, err = domain.ParseMemorySize(n); err != nil {
				return reject(err)
			}
		}
		if v, ok := c.Network.Get(); ok {
			if v == "" {
				return reject(errors.New("network mode must not be empty"))
			}
			if cfg.Res.Net.Mode, err = parseNetMode(v); err != nil {
				return reject(err)
			}
			net := cfg.Res.Net
			if net.Mode == NetNone && (cfg.Ports.Len() > 0 || net.Allow.Len() > 0 || net.Deny.Len() > 0 || net.Default != DefaultAllow) {
				return reject(errors.New("network.mode \"none\" cannot be combined with ports, default, allow or deny"))
			}
		}
		return reconfigure(w, env, g, cfg)
	})
}

func postForward(w World, env Env, c CmdForward) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []CmdForward{c}, func(w World, c CmdForward) step {
		g, err := w.lookup(c.Target)
		if err != nil {
			return reject(err)
		}
		if len(c.Ports) == 0 {
			return reject(errors.New("PORT is required"))
		}
		if _, err := parseProto(c.Proto); err != nil {
			return reject(err)
		}
		var ports []Port
		for _, spec := range c.Ports {
			p, err := parsePortSpec(spec, c.Proto)
			if err != nil {
				return reject(err)
			}
			ports = append(ports, p)
		}
		if g.Config.Res.Net.Mode == NetNone {
			return reject(errors.New("port forwards cannot be used when network.mode is \"none\""))
		}
		cfg := g.Config
		if cfg.Ports, err = addPorts(cfg.Ports, ports, w.otherPorts(g.Name)); err != nil {
			return reject(err)
		}
		return reconfigure(w, env, g, cfg)
	})
}

func postUnforward(w World, env Env, c CmdUnforward) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []CmdUnforward{c}, func(w World, c CmdUnforward) step {
		g, err := w.lookup(c.Target)
		if err != nil {
			return reject(err)
		}
		if len(c.Ports) == 0 {
			return reject(errors.New("PORT is required"))
		}
		cfg := g.Config
		for _, spec := range c.Ports {
			p, err := parsePortSpec(spec, "")
			if err != nil {
				return reject(err)
			}
			var keep []Port
			for _, q := range cfg.Ports.All() {
				if q.Host != p.Host {
					keep = append(keep, q)
				}
			}
			if len(keep) == cfg.Ports.Len() {
				return reject(fmt.Errorf("port %d is not configured on %s", p.Host.Get(), g.Name.Get()))
			}
			cfg.Ports = types.BuildSet[Port, PortKey, PortByHost](keep)
		}
		return reconfigure(w, env, g, cfg)
	})
}

func startGuest(w World, env Env, g Guest) step {
	switch g.Phase {
	case PhaseUncreated:
		return reject(fmt.Errorf("guest %s has not been built yet", g.Name.Get()))
	case PhaseRunning:
		return step{w: w} // P4
	case PhaseOff:
	}
	if busy(env, g.Config.Ports) {
		return reject(errors.New("a forwarded host port is in use"))
	}
	return step{w: w.with(boot(g)), risky: true}
}

func stopGuest(w World, _ Env, g Guest) step {
	if g.Phase != PhaseRunning {
		return step{w: w} // P4 (D6: stopping an uncreated guest succeeds)
	}
	g.Phase = PhaseOff
	g.Mounts = types.EmptySet[Mount, domain.AbsPath, MountByPath]()
	g.Sessions = types.EmptySet[Session, string, SessionByCommand]()
	return step{w: w.with(g), risky: true}
}

func restartGuest(w World, env Env, g Guest) step {
	if g.Phase == PhaseUncreated {
		return reject(fmt.Errorf("guest %s has not been built yet", g.Name.Get()))
	}
	if busy(env, g.Config.Ports) {
		return reject(errors.New("a forwarded host port is in use"))
	}
	return step{w: w.with(boot(g)), risky: true}
}

// postPower runs start, stop or restart over NAME... (all guests in order,
// stopping at the first failure, P2).
func postPower(w World, env Env, names []string, f func(World, Env, Guest) step) []Outcome {
	if len(names) == 0 {
		names = []string{""}
	}
	if w.external(names...) {
		return nil
	}
	return fold(w, names, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		return f(w, env, g)
	})
}

func postRm(w World, c CmdRm) []Outcome {
	if w.external(c.Names...) {
		return nil
	}
	if len(c.Names) == 0 || !c.Yes {
		reason := "guest NAME is required"
		if len(c.Names) > 0 {
			reason = "refusing without --yes (stdin is not a terminal)"
		}
		return []Outcome{{World: w, Kind: OutcomeReject, Reason: reason}}
	}
	return fold(w, c.Names, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		w.Guests = w.Guests.Remove(g.Name)
		return step{w: w, risky: true}
	})
}

func postMount(w World, env Env, c CmdMount) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	if _, err := w.lookup(c.Target); err != nil || len(c.Dirs) == 0 {
		if err == nil {
			err = errors.New("DIR is required")
		}
		return []Outcome{{World: w, Kind: OutcomeReject, Reason: err.Error()}}
	}
	mode := ModeRW
	if c.ReadOnly {
		mode = ModeRO
	}
	return fold(w, c.Dirs, func(w World, dir string) step {
		g, _ := w.lookup(c.Target)
		p, err := sharePath(env, dir)
		if err != nil {
			return reject(err)
		}
		if have, ok := g.Config.Shares.Lookup(p); ok {
			if have.Mode != mode {
				// D2: changing the mode needs a restart; reporting success would lie (P3).
				return reject(fmt.Errorf("%s is already shared %s", p.Get(), modeName(have.Mode)))
			}
			return step{w: w} // P4
		}
		for _, s := range g.Config.Shares.All() {
			if domain.PathsOverlap(s.Path, p) {
				return reject(fmt.Errorf("%s overlaps share %s", p.Get(), s.Path.Get()))
			}
		}
		for _, m := range g.Mounts.All() {
			if m.Path != p && domain.PathsOverlap(m.Path, p) {
				return reject(fmt.Errorf("%s overlaps mounted share %s", p.Get(), m.Path.Get()))
			}
		}
		if m, ok := g.Mounts.Lookup(p); ok && m.Mode != mode {
			return reject(fmt.Errorf("%s is still mounted %s until the guest restarts", p.Get(), modeName(m.Mode)))
		}
		g.Config.Shares = g.Config.Shares.Upsert(Share{Path: p, Mode: mode})
		if g.Phase != PhaseRunning || g.Mounts.Contains(p) {
			return step{w: w.with(g)}
		}
		g.Mounts = g.Mounts.Upsert(Mount{Path: p, Mode: mode, Origin: OriginRuntime})
		return step{w: w.with(g), risky: true}
	})
}

func modeName(m ShareMode) string {
	if m == ModeRO {
		return "read-only"
	}
	return "read-write"
}

func postUnmount(w World, env Env, c CmdUnmount) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	if _, err := w.lookup(c.Target); err != nil || len(c.Dirs) == 0 {
		if err == nil {
			err = errors.New("DIR is required")
		}
		return []Outcome{{World: w, Kind: OutcomeReject, Reason: err.Error()}}
	}
	return fold(w, c.Dirs, func(w World, dir string) step {
		g, _ := w.lookup(c.Target)
		p, err := resolve(env, dir)
		if err != nil {
			return reject(err)
		}
		if !g.Config.Shares.Contains(p) {
			return reject(fmt.Errorf("%s is not a configured share of %s", p.Get(), g.Name.Get()))
		}
		g.Config.Shares = g.Config.Shares.Remove(p)
		if m, ok := g.Mounts.Lookup(p); ok && m.Origin == OriginRuntime {
			g.Mounts = g.Mounts.Remove(p)
			return step{w: w.with(g), risky: true}
		}
		// A boot-time mount stays until the guest restarts.
		return step{w: w.with(g)}
	})
}

// liveMounts returns the mounts of running guest g after its shares change
// to next without a restart, or false if that needs a restart.
func liveMounts(g Guest, next ShareSet) (MountSet, bool) {
	ms := g.Mounts.Clone()
	for _, m := range g.Mounts.All() {
		if !next.Contains(m.Path) && m.Origin == OriginRuntime {
			ms = ms.Remove(m.Path)
		}
	}
	for _, s := range next.All() {
		if m, ok := ms.Lookup(s.Path); ok {
			if m.Mode != s.Mode {
				return ms, false
			}
			continue
		}
		for _, m := range ms.All() {
			if domain.PathsOverlap(m.Path, s.Path) {
				return ms, false
			}
		}
		ms = ms.Upsert(Mount{Path: s.Path, Mode: s.Mode, Origin: OriginRuntime})
	}
	return ms, true
}

func postEdit(w World, env Env, c CmdEdit) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []CmdEdit{c}, func(w World, c CmdEdit) step {
		g, err := w.lookup(c.Target)
		if err != nil {
			return reject(err)
		}
		cfg, err := parseConfig(w, env, g.Name, c.New)
		if err != nil {
			return reject(err) // D10: the file keeps its previous content
		}
		if cfg.Equal(g.Config) {
			return step{w: w} // P4
		}
		switch g.Phase {
		case PhaseUncreated:
			g.Config = cfg
			return step{w: w.with(g)}
		case PhaseOff, PhaseRunning:
			if err := buildable(env, cfg.Packages); err != nil {
				return reject(err)
			}
		}
		if g.Phase == PhaseOff {
			g.Config = cfg
			return step{w: w.with(g), risky: true}
		}
		// D9: a running guest takes the new config immediately.
		ms, live := liveMounts(g, cfg.Shares)
		if !live || !cfg.Res.Equal(g.Config.Res) || !slices.Equal(cfg.Ports.All(), g.Config.Ports.All()) {
			return reconfigure(w, env, g, cfg)
		}
		g.Config = cfg
		switched := g
		switched.Mounts = ms
		s := newSystem(w, g)
		s.w = w.with(switched)
		return s
	})
}

// postInspect changes nothing; it rejects a guest that does not exist,
// and the console log of a guest that never booted.
func postInspect(w World, c CmdInspect) []Outcome {
	switch c.What {
	case InspectLs, InspectDoctor, InspectCacheLs:
		return []Outcome{{World: w, Kind: OutcomeOk}}
	case InspectShow, InspectStats, InspectLogs:
	}
	if w.external(c.Target) {
		return nil
	}
	g, err := w.lookup(c.Target)
	if err == nil && c.What == InspectLogs && g.Phase == PhaseUncreated {
		err = fmt.Errorf("guest %s has not been built yet", g.Name.Get())
	}
	if err != nil {
		return []Outcome{{World: w, Kind: OutcomeReject, Reason: err.Error()}}
	}
	return []Outcome{{World: w, Kind: OutcomeOk}}
}

// postSpawn starts the guest if needed (as run does) and leaves a session
// running COMMAND.
func postSpawn(w World, env Env, c CmdSpawn) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	return fold(w, []string{c.Target}, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		if g.Sessions.Contains(c.Command) {
			return reject(fmt.Errorf("%q is already running (the model keeps one session per command)", c.Command))
		}
		s := startGuest(w, env, g)
		if s.err != nil {
			return s
		}
		g, _ = s.w.Guests.Lookup(g.Name)
		g.Sessions = g.Sessions.Upsert(Session{Command: c.Command})
		return step{w: s.w.with(g), risky: true}
	})
}

// postKill ends the named sessions in order, stopping at the first one that
// is not running (P2).
func postKill(w World, c CmdKill) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	if _, err := w.lookup(c.Target); err != nil || len(c.Commands) == 0 {
		if err == nil {
			err = errors.New("SESSION is required")
		}
		return []Outcome{{World: w, Kind: OutcomeReject, Reason: err.Error()}}
	}
	return fold(w, c.Commands, func(w World, cmd string) step {
		g, _ := w.lookup(c.Target)
		if !g.Sessions.Contains(cmd) {
			return reject(fmt.Errorf("no session %q in %s", cmd, g.Name.Get()))
		}
		g.Sessions = g.Sessions.Remove(cmd)
		return step{w: w.with(g), risky: true}
	})
}

func postRun(w World, env Env, c CmdRun) []Outcome {
	if w.external(c.Target) {
		return nil
	}
	out := fold(w, []string{c.Target}, func(w World, t string) step {
		g, err := w.lookup(t)
		if err != nil {
			return reject(err)
		}
		return startGuest(w, env, g)
	})
	for i := range out {
		if out[i].Kind == OutcomeOk {
			out[i].Code = c.Code
		}
	}
	return out
}
