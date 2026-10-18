package host

import (
	"context"
	"fmt"
	"sync"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// All units hanare starts are transient services of the user's systemd
// instance, managed over D-Bus.

var (
	userBus     *sd.Conn
	userBusErr  error
	userBusOnce sync.Once
)

func systemd() (*sd.Conn, error) {
	userBusOnce.Do(func() {
		userBus, userBusErr = sd.NewUserConnectionContext(context.Background())
	})
	return userBus, userBusErr
}

// unitActive reports whether a unit is active (or activating).
func unitActive(name string) bool {
	c, err := systemd()
	if err != nil {
		return false
	}
	p, err := c.GetUnitPropertyContext(context.Background(), name, "ActiveState")
	if err != nil {
		return false
	}
	s, _ := p.Value.Value().(string)
	return s == "active" || s == "activating" || s == "reloading"
}

// unitActiveSince returns when a unit last became active.
func unitActiveSince(name string) (time.Time, error) {
	c, err := systemd()
	if err != nil {
		return time.Time{}, err
	}
	p, err := c.GetUnitPropertyContext(context.Background(), name, "ActiveEnterTimestamp")
	if err != nil {
		return time.Time{}, err
	}
	us, _ := p.Value.Value().(uint64)
	if us == 0 {
		return time.Time{}, fmt.Errorf("%s: no activation time", name)
	}
	return time.UnixMicro(int64(us)), nil
}

// stopUnit stops a unit (or a slice with everything in it) and waits for the
// job; units that are not loaded are ignored.
func stopUnit(name string) {
	c, err := systemd()
	if err != nil {
		return
	}
	done := make(chan string, 1)
	if _, err := c.StopUnitContext(context.Background(), name, "replace", done); err == nil {
		<-done
	}
}

// startTransient starts a transient service running argv in slice and waits
// until systemd reports the start job as done.
func startTransient(name, slice, description string, props []sd.Property, argv ...string) error {
	c, err := systemd()
	if err != nil {
		return fmt.Errorf("connect to the user systemd: %w", err)
	}
	all := append([]sd.Property{
		sd.PropDescription(description),
		sd.PropExecStart(argv, false),
		sd.PropSlice(slice),
		prop("CollectMode", "inactive-or-failed"),
	}, props...)
	done := make(chan string, 1)
	if _, err := c.StartTransientUnitContext(context.Background(), name, "fail", all, done); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	if res := <-done; res != "done" {
		return fmt.Errorf("start %s: %s (see: journalctl --user -u %s)", name, res, name)
	}
	return nil
}

func prop(name string, value any) sd.Property {
	return sd.Property{Name: name, Value: dbus.MakeVariant(value)}
}

// bindPath is the D-Bus form of one BindPaths= entry, a(ssbt):
// source, destination, ignore if missing, mount flags.
type bindPath struct {
	Source, Destination string
	IgnoreMissing       bool
	Flags               uint64
}

const msRec = 0x4000 // MS_REC: recursive bind mount

func bindPaths(name string, paths []string) sd.Property {
	var b []bindPath
	for _, p := range paths {
		b = append(b, bindPath{p, p, false, msRec})
	}
	return prop(name, b)
}

// sandboxProps confines a unit to the given $HOME paths: a compromised QEMU
// or helper sees nothing else from $HOME and cannot write outside.
func sandboxProps(rw, ro []string) []sd.Property {
	props := []sd.Property{
		prop("ProtectHome", "tmpfs"),
		prop("ProtectSystem", "strict"),
		prop("PrivateTmp", true),
	}
	if len(rw) > 0 {
		props = append(props, bindPaths("BindPaths", rw))
	}
	if len(ro) > 0 {
		props = append(props, bindPaths("BindReadOnlyPaths", ro))
	}
	return props
}

// unixOnly allows only AF_UNIX sockets: RestrictAddressFamilies=, (bas) with
// true meaning "allow list".
var unixOnly = prop("RestrictAddressFamilies", struct {
	Allow    bool
	Families []string
}{true, []string{"AF_UNIX"}})

func partOf(unit string) sd.Property { return prop("PartOf", []string{unit}) }
