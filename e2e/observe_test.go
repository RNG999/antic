//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/types"
)

// mountView is the observable part of a mount (its origin is not observable).
type mountView struct {
	Path string
	Mode spec.ShareMode
}

// guestView is the observable projection of a managed guest.
type guestView struct {
	Phase   spec.Phase
	Config  spec.Config
	Mounted []mountView
	// CPUs is the vCPU count seen inside a running guest (O3), or 0.
	CPUs int
}

// observation is α_obs's input: what the CLI reports.
type observation struct {
	guests   map[string]guestView
	external spec.ExternalSet
}

// view projects a model World onto what can be observed.
func view(w spec.World) map[string]guestView {
	out := map[string]guestView{}
	for _, g := range w.Guests.All() {
		v := guestView{Phase: g.Phase, Config: g.Config}
		for _, m := range g.Mounts.All() {
			v.Mounted = append(v.Mounted, mountView{Path: m.Path.Get(), Mode: m.Mode})
		}
		if g.Phase == spec.PhaseRunning {
			v.CPUs = g.Config.Res.CPUs.Get()
		}
		out[g.Name.Get()] = v
	}
	return out
}

// diff describes how an observation differs from a model World ("" if not).
func diff(w spec.World, o observation) string {
	var out []string
	want := view(w)
	var names []string
	for n := range want {
		names = append(names, n)
	}
	for n := range o.guests {
		if _, ok := want[n]; !ok {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		a, aok := want[n]
		b, bok := o.guests[n]
		switch {
		case !aok:
			out = append(out, fmt.Sprintf("%s: exists but the model has no such guest", n))
		case !bok:
			out = append(out, fmt.Sprintf("%s: missing (model phase %d)", n, a.Phase))
		default:
			if a.Phase != b.Phase {
				out = append(out, fmt.Sprintf("%s: phase %d, model %d", n, b.Phase, a.Phase))
			}
			if !a.Config.Equal(b.Config) {
				out = append(out, fmt.Sprintf("%s: config\n    observed %s\n    model    %s", n, describe(b.Config), describe(a.Config)))
			}
			if !slices.Equal(a.Mounted, b.Mounted) {
				out = append(out, fmt.Sprintf("%s: mounted %v, model %v", n, b.Mounted, a.Mounted))
			}
			if b.CPUs != 0 && a.CPUs != b.CPUs {
				out = append(out, fmt.Sprintf("%s: %d vCPUs inside the guest, model %d", n, b.CPUs, a.CPUs))
			}
		}
	}
	if !(spec.World{External: w.External}).Equal(spec.World{External: o.external}) {
		out = append(out, fmt.Sprintf("external guests changed: %v, model %v", o.external.All(), w.External.All()))
	}
	return strings.Join(out, "\n")
}

func describe(c spec.Config) string {
	var pkgs, shares, ports []string
	for _, p := range c.Packages.All() {
		pkgs = append(pkgs, p.Get())
	}
	for _, s := range c.Shares.All() {
		shares = append(shares, fmt.Sprintf("%s(%d)", s.Path.Get(), s.Mode))
	}
	for _, p := range c.Ports.All() {
		ports = append(ports, fmt.Sprintf("%d:%d/%d", p.Host.Get(), p.Guest.Get(), p.Proto))
	}
	n := c.Res.Net
	return fmt.Sprintf("pkgs=%v cpus=%d mem=%d net={mode %d default %d allow %d deny %d} ports=%v shares=%v",
		pkgs, c.Res.CPUs.Get(), c.Res.Memory.Get(), n.Mode, n.Default, n.Allow.Len(), n.Deny.Len(), ports, shares)
}

type lsEntry struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type showInfo struct {
	State  string `json:"state"`
	System string `json:"system"`
	Shares []struct {
		Path     string `json:"path"`
		ReadOnly bool   `json:"read_only"`
		Mounted  bool   `json:"mounted"`
	} `json:"shares"`
	Ports []struct {
		Host  int    `json:"host"`
		Guest int    `json:"guest"`
		Proto string `json:"proto"`
	} `json:"ports"`
}

// tomlConfig is the subset of a guest config file the model covers.
type tomlConfig struct {
	Packages []string `toml:"packages"`
	Spec     struct {
		CPUs   int    `toml:"cpus"`
		Memory string `toml:"memory"`
	} `toml:"spec"`
	Network struct {
		Mode    string   `toml:"mode"`
		Default string   `toml:"default"`
		Allow   []string `toml:"allow"`
		Deny    []string `toml:"deny"`
		Ports   []struct {
			Host  int    `toml:"host"`
			Guest int    `toml:"guest"`
			Proto string `toml:"proto"`
		} `toml:"ports"`
	} `toml:"network"`
	Shares []struct {
		Source string `toml:"source"`
		Mode   string `toml:"mode"`
	} `toml:"shares"`
}

func phaseOf(state string) (spec.Phase, error) {
	switch state {
	case "-":
		return spec.PhaseUncreated, nil
	case "stopped":
		return spec.PhaseOff, nil
	case "running":
		return spec.PhaseRunning, nil
	}
	return 0, fmt.Errorf("state %q", state)
}

// observe is α_obs: it reads the CLI's view of every guest.
func (h *harness) observe() (observation, error) {
	o := observation{guests: map[string]guestView{}}
	out, code, stderr := h.antic(nil, "guest", "ls", "--json")
	if code != 0 {
		return o, fmt.Errorf("guest ls: exit %d: %s", code, stderr)
	}
	var ls []lsEntry
	if err := json.Unmarshal([]byte(out), &ls); err != nil {
		return o, fmt.Errorf("guest ls --json: %w", err)
	}
	var ext []spec.External
	for _, e := range ls {
		phase, err := phaseOf(e.State)
		if err != nil {
			return o, fmt.Errorf("guest %s: %w", e.Name, err)
		}
		info, err := h.show(e.Name)
		if err != nil {
			return o, err
		}
		name, err := domain.ParseGuestName(e.Name)
		if err != nil {
			return o, err
		}
		if !strings.HasPrefix(e.Name, prefix) {
			var ports []spec.Port
			for _, p := range info.Ports {
				sp, err := portOf(p.Host, p.Guest, p.Proto)
				if err != nil {
					return o, err
				}
				ports = append(ports, sp)
			}
			ext = append(ext, spec.External{Name: name, Phase: phase, Ports: types.BuildSet[spec.Port, spec.PortKey, spec.PortByHost](ports)})
			continue
		}
		cfg, err := h.readConfig(e.Name)
		if err != nil {
			return o, err
		}
		v := guestView{Phase: phase, Config: cfg}
		for _, s := range info.Shares {
			if s.Mounted {
				m := spec.ModeRW
				if s.ReadOnly {
					m = spec.ModeRO
				}
				v.Mounted = append(v.Mounted, mountView{Path: s.Path, Mode: m})
			}
		}
		slices.SortFunc(v.Mounted, func(a, b mountView) int { return strings.Compare(a.Path, b.Path) })
		if phase == spec.PhaseRunning {
			if v.CPUs, err = h.probeCPUs(e.Name); err != nil {
				return o, err
			}
		}
		o.guests[e.Name] = v
	}
	o.external = types.BuildSet[spec.External, domain.GuestName, spec.ExternalByName](ext)
	return o, nil
}

func (h *harness) show(name string) (showInfo, error) {
	var info showInfo
	out, code, stderr := h.antic(nil, "guest", "show", name, "--json")
	if code != 0 {
		return info, fmt.Errorf("guest show %s: exit %d: %s", name, code, stderr)
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return info, fmt.Errorf("guest show %s --json: %w", name, err)
	}
	return info, nil
}

func portOf(host, guest int, proto string) (spec.Port, error) {
	h, err := domain.ParsePortNumber(host)
	if err != nil {
		return spec.Port{}, err
	}
	if guest == 0 {
		guest = host
	}
	g, err := domain.ParsePortNumber(guest)
	if err != nil {
		return spec.Port{}, err
	}
	p := spec.ProtoTCP
	switch proto {
	case "udp":
		p = spec.ProtoUDP
	case "tcp,udp", "udp,tcp":
		p = spec.ProtoBoth
	}
	return spec.Port{Host: h, Guest: g, Proto: p}, nil
}

// readConfig maps the guest's config file ("guest show --config") to a
// spec.Config.
func (h *harness) readConfig(name string) (spec.Config, error) {
	out, code, stderr := h.antic(nil, "guest", "show", name, "--config")
	if code != 0 {
		return spec.Config{}, fmt.Errorf("guest show %s --config: exit %d: %s", name, code, stderr)
	}
	var tc tomlConfig
	if _, err := toml.Decode(out, &tc); err != nil {
		return spec.Config{}, fmt.Errorf("%s.toml: %w", name, err)
	}
	raw := spec.RawConfig{
		Packages: tc.Packages,
		CPUs:     tc.Spec.CPUs,
		Memory:   tc.Spec.Memory,
		Network: spec.RawNetwork{
			Mode: tc.Network.Mode, Default: tc.Network.Default,
			Allow: tc.Network.Allow, Deny: tc.Network.Deny,
		},
	}
	for _, p := range tc.Network.Ports {
		guest := p.Guest
		if guest == 0 {
			guest = p.Host
		}
		raw.Ports = append(raw.Ports, spec.RawPort{Host: p.Host, Guest: guest, Proto: p.Proto})
	}
	for _, s := range tc.Shares {
		src := s.Source
		if src == "~" || strings.HasPrefix(src, "~/") {
			src = filepath.Join(h.home, strings.TrimPrefix(src, "~"))
		}
		raw.Shares = append(raw.Shares, spec.RawShare{Path: src, Mode: s.Mode})
	}
	cfg, err := spec.ParseConfig(h.env, raw)
	if err != nil {
		return cfg, fmt.Errorf("%s.toml does not satisfy the specification: %w", name, err)
	}
	return cfg, nil
}

// probeCPUs counts the vCPUs a running guest sees (O3): it reads
// /sys/devices/system/cpu/online, which nproc's affinity rules do not affect.
func (h *harness) probeCPUs(name string) (int, error) {
	out, code, stderr := h.antic(nil, "run", name, "-T", "--", "cat", "/sys/devices/system/cpu/online")
	if code != 0 {
		return 0, fmt.Errorf("probe cpus in %s: exit %d: %s", name, code, stderr)
	}
	n := 0
	for _, r := range strings.Split(strings.TrimSpace(out), ",") {
		lo, hi, isRange := strings.Cut(r, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return 0, fmt.Errorf("probe cpus in %s: %q", name, out)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return 0, fmt.Errorf("probe cpus in %s: %q", name, out)
			}
		}
		n += b - a + 1
	}
	return n, nil
}
