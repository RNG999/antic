// Package systemd starts and inspects antic's transient units in the
// user's systemd instance, over D-Bus.
package systemd

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// Property is a unit property.
type Property = sd.Property

// Unit describes a transient service.
type Unit struct {
	Name        string
	Slice       string
	Description string
	Argv        []string
	Props       []Property
}

// State is a unit's state. A unit that is not loaded is "inactive".
type State struct {
	Active     string // ActiveState: active, activating, deactivating, inactive, failed, ...
	Invocation string // InvocationID, hex; changes on every start
	Since      time.Time
}

// Running reports whether the unit is (being) started.
func (s State) Running() bool {
	return s.Active == "active" || s.Active == "activating" || s.Active == "reloading"
}

// Manager manages units; DBus is the real one.
type Manager interface {
	Start(ctx context.Context, u Unit) error
	Stop(ctx context.Context, name string) error
	State(ctx context.Context, name string) (State, error)
}

// DBus talks to the user's systemd instance.
type DBus struct{ c *sd.Conn }

// Connect connects to the user's systemd instance.
func Connect(ctx context.Context) (*DBus, error) {
	c, err := sd.NewUserConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to the user systemd instance: %w", err)
	}
	return &DBus{c}, nil
}

// Start starts u and waits for the start job. Units run with Type=exec, so
// a binary that cannot be executed (or a sandbox that cannot be set up)
// fails the job instead of looking started.
func (m *DBus) Start(ctx context.Context, u Unit) error {
	props := append([]Property{
		sd.PropDescription(u.Description),
		sd.PropExecStart(u.Argv, false),
		sd.PropSlice(u.Slice),
		sd.PropType("exec"),
		Prop("CollectMode", "inactive-or-failed"),
	}, u.Props...)
	done := make(chan string, 1)
	if _, err := m.c.StartTransientUnitContext(ctx, u.Name, "fail", props, done); err != nil {
		return fmt.Errorf("start %s: %w", u.Name, err)
	}
	select {
	case res := <-done:
		if res != "done" {
			return fmt.Errorf("start %s: %s (see: journalctl --user -u %s)", u.Name, res, u.Name)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop stops a unit (a slice: everything in it) and waits for the job.
// Units that are not loaded are not an error.
func (m *DBus) Stop(ctx context.Context, name string) error {
	done := make(chan string, 1)
	if _, err := m.c.StopUnitContext(ctx, name, "replace", done); err != nil {
		if strings.Contains(err.Error(), "not loaded") {
			return nil
		}
		return fmt.Errorf("stop %s: %w", name, err)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State returns a unit's state.
func (m *DBus) State(ctx context.Context, name string) (State, error) {
	p, err := m.c.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		return State{}, fmt.Errorf("state of %s: %w", name, err)
	}
	s := State{}
	s.Active, _ = p["ActiveState"].(string)
	if id, ok := p["InvocationID"].([]byte); ok {
		s.Invocation = hex.EncodeToString(id)
	}
	if us, ok := p["ActiveEnterTimestamp"].(uint64); ok && us > 0 {
		s.Since = time.UnixMicro(int64(us))
	}
	if s.Active == "" {
		return State{}, fmt.Errorf("state of %s: no ActiveState", name)
	}
	return s, nil
}

// Prop builds a property.
func Prop(name string, value any) Property {
	return Property{Name: name, Value: dbus.MakeVariant(value)}
}

// bindPath is one BindPaths= entry in D-Bus form, a(ssbt): source,
// destination, ignore if missing, mount flags.
type bindPath struct {
	Source, Destination string
	IgnoreMissing       bool
	Flags               uint64
}

const msRec = 0x4000 // MS_REC

func binds(name string, paths []string) Property {
	b := make([]bindPath, len(paths))
	for i, p := range paths {
		b[i] = bindPath{p, p, false, msRec}
	}
	return Prop(name, b)
}

// Confine returns properties that hide $HOME except rw and ro (bound at the
// same paths), make the rest of the file system read-only, and give the
// unit its own /tmp, IPC and process view.
func Confine(rw, ro []string) []Property {
	props := []Property{
		Prop("ProtectHome", "tmpfs"),
		Prop("ProtectSystem", "strict"),
		Prop("PrivateTmp", true),
		Prop("PrivateIPC", true),
		Prop("PrivatePIDs", "yes"),
		Prop("ProtectProc", "invisible"),
		Prop("ProtectClock", true),
		Prop("ProtectKernelModules", true),
		Prop("ProtectControlGroups", true),
		Prop("LockPersonality", true),
		Prop("RestrictRealtime", true),
		Prop("RestrictSUIDSGID", true),
		Prop("SystemCallArchitectures", []string{"native"}),
		Prop("LimitCORE", uint64(0)),
	}
	if len(rw) > 0 {
		props = append(props, binds("BindPaths", rw))
	}
	if len(ro) > 0 {
		props = append(props, binds("BindReadOnlyPaths", ro))
	}
	return props
}

// ProtectProcFS makes /proc/sys (including the hostname) read-only and the
// kernel log inaccessible. Not for virtiofsd and pasta: covering parts of
// /proc stops them from mounting a fresh /proc in their own namespaces.
func ProtectProcFS() []Property {
	return []Property{Prop("ProtectKernelTunables", true), Prop("ProtectKernelLogs", true), Prop("ProtectHostname", true)}
}

// UnixOnly allows only AF_UNIX sockets (RestrictAddressFamilies=, (bas):
// true means allow list).
func UnixOnly() Property {
	return Prop("RestrictAddressFamilies", struct {
		Allow    bool
		Families []string
	}{true, []string{"AF_UNIX"}})
}

// BindsTo ties a unit's lifetime to another (stopped when it stops).
func BindsTo(unit string) []Property {
	return []Property{Prop("BindsTo", []string{unit}), Prop("After", []string{unit})}
}
