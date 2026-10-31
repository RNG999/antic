package guest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/host/transport"
	"github.com/RNG999/antic/internal/proto"
)

// WorkDir returns the current directory, which must lie in one of the guest's
// shares (the guest sees them at the same path).
func (g *Guest) WorkDir(ctx context.Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wd = config.RealPath(wd)
	var shares []string
	if bt, err := g.readBoot(ctx); err == nil {
		for _, s := range bt.Shares {
			shares = append(shares, s.Path)
		}
	} else {
		for _, s := range g.Cfg.Shares {
			if p, err := filepath.EvalSymlinks(s.Path); err == nil {
				shares = append(shares, p)
			}
		}
	}
	for _, s := range shares {
		if wd == s || strings.HasPrefix(wd, s+"/") {
			return wd, nil
		}
	}
	return "", fmt.Errorf("%s is not inside a directory shared with guest %s (%s); see \"antic share add\"",
		wd, g.Name(), strings.Join(shares, ", "))
}

// Exec runs a command or shell in the guest (starting it if needed).
func (g *Guest) Exec(ctx context.Context, c transport.Cmd) (int, error) {
	if err := g.requireCreated(); err != nil {
		return 0, err
	}
	if err := g.EnsureRunning(ctx); err != nil {
		return 0, err
	}
	if len(g.Cfg.Env) > 0 {
		var envKeys []string
		for k := range g.Cfg.Env {
			envKeys = append(envKeys, k)
		}
		slices.Sort(envKeys)
		for _, k := range envKeys {
			c.Env = append(c.Env, k+"="+g.Cfg.Env[k])
		}
	}
	return g.vsock().Exec(ctx, c)
}

// Sessions lists the guest's running sessions.
func (g *Guest) Sessions(ctx context.Context) ([]proto.Session, error) {
	if _, err := g.readBoot(ctx); err != nil {
		return nil, err
	}
	var out []proto.Session
	err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpSessions}, func(r proto.Response) {
		if r.Sessions != nil {
			out = r.Sessions
		}
	})
	return out, err
}

// Kill stops a session.
func (g *Guest) Kill(ctx context.Context, session string) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpKill, Session: session}, nil)
}

func (g *Guest) osDesc() string {
	if g.Cfg.FlakePath != "" {
		return g.Cfg.FlakePath + "#" + g.Cfg.FlakeAttr
	}
	if len(g.Cfg.Packages) == 0 {
		return "builtin"
	}
	return "builtin [" + strings.Join(g.Cfg.Packages, " ") + "]"
}

// Info describes a guest for "guest ls" and "guest show".
type Info struct {
	Name        string        `json:"name"`
	ID          string        `json:"id,omitempty"`
	Description string        `json:"description,omitempty"`
	State       string        `json:"state"` // -, stopped, running
	Since       *time.Time    `json:"since,omitempty"`
	OS          string        `json:"os"` // builtin [...] or flake#attr
	Packages    []string      `json:"packages,omitempty"`
	System      string        `json:"system,omitempty"`
	CPUs        int           `json:"cpus"`
	Memory      string        `json:"memory"`
	Network     string        `json:"network"`
	Ports       []config.Port `json:"ports,omitempty"`
	Shares      []ShareInfo   `json:"shares"`
	Units       []string      `json:"units,omitempty"`
}

// Inspect returns the guest's state.
func (g *Guest) Inspect(ctx context.Context) (Info, error) {
	i := Info{
		Name: g.Name(), ID: g.Inst.ID,
		Description: g.Cfg.Description, State: "-",
		OS: g.osDesc(), Packages: g.Cfg.Packages,
		CPUs: g.Cfg.CPUs, Memory: g.Cfg.Memory, Network: g.Cfg.Network.Mode, Ports: g.Cfg.Ports,
		Shares: g.Shares(ctx),
	}
	if !g.Created {
		return i, nil
	}
	i.State = "stopped"
	i.System, _ = g.store().Current()
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return i, err
	}
	if st.Running() {
		i.State, i.Since = "running", &st.Since
		if bt, err := g.readBoot(ctx); err == nil {
			i.System = bt.System
		}
		i.Units = []string{g.vmUnit()}
		if g.Cfg.Network.Mode != config.NetworkNone {
			i.Units = append(i.Units, g.netUnit())
		}
		i.Units = append(i.Units, g.shareUnit("store"))
		for _, s := range i.Shares {
			if s.Mounted {
				i.Units = append(i.Units, g.shareUnit(share.Tag(s.Path)))
			}
		}
	}
	return i, nil
}

// Journal streams the running guest's journal (via antic-agent; root's view).
func (g *Guest) Journal(ctx context.Context, units []string, lines int, line func(string)) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpJournal, Units: units, Lines: lines}, func(r proto.Response) {
		if r.Line != "" {
			line(r.Line) // the CLI strips control sequences (sanitizer)
		}
	})
}

// Console is the guest's console log.
func (g *Guest) Console() string { return g.Inst.Console() }
