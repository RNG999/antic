package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/proto"
)

var storePath = regexp.MustCompile(`^/nix/store/[0-9a-z]{32}-[^/]+$`)

// apply switches the running system to toplevel, a NixOS system the host
// built and copied into the guest's read-only store. Output lines go to
// line.
func apply(toplevel string, line func(string)) error {
	if !storePath.MatchString(toplevel) {
		return fmt.Errorf("invalid system path %q", toplevel)
	}
	stc := filepath.Join(toplevel, "bin/switch-to-configuration")
	if _, err := os.Stat(stc); err != nil {
		return fmt.Errorf("%s is not a NixOS system: %w", toplevel, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, stc, "switch")
	cmd.Env = append(os.Environ(), "NIXOS_INSTALL_BOOTLOADER=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line(sc.Text())
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("switch-to-configuration: %w", err)
	}
	return nil
}

// Session units: one antic-agent session instance per "antic run" (see nix/module.nix).
var sessionUnit = regexp.MustCompile(`^antic-session@[A-Za-z0-9:_.\-]+\.service$`)

// sessions lists running session units with their command and processes.
func sessions() ([]proto.Session, error) {
	var out []proto.Session
	var dirs []string
	for _, slice := range proto.SessionSlices {
		entries, _ := os.ReadDir(slice)
		for _, e := range entries {
			dirs = append(dirs, filepath.Join(slice, e.Name()))
		}
	}
	for _, d := range dirs {
		unit := filepath.Base(d)
		if !sessionUnit.MatchString(unit) {
			continue
		}
		s := proto.Session{Unit: unit}
		if fi, err := os.Stat(d); err == nil {
			s.Started = fi.ModTime().Unix()
		}
		b, _ := os.ReadFile(filepath.Join(d, "cgroup.procs"))
		for _, f := range strings.Fields(string(b)) {
			var pid int
			fmt.Sscan(f, &pid)
			s.PIDs = append(s.PIDs, pid)
			if s.Command != "" {
				continue
			}
			if comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); strings.HasPrefix(string(comm), "antic-agent") {
				continue
			}
			if cl, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
				s.Command = strings.TrimSpace(strings.ReplaceAll(string(cl), "\x00", " "))
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// kill stops a session unit.
func kill(unit string) error {
	if !sessionUnit.MatchString(unit) {
		return fmt.Errorf("invalid session %q", unit)
	}
	cmd := exec.Command("systemctl", "stop", "--no-block", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop %s: %v: %s", unit, err, out)
	}
	return nil
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+$`)

// journal streams the guest's journal for this boot.
func journal(units []string, lines int, line func(string)) error {
	if lines <= 0 {
		lines = 200
	}
	args := []string{"-b", "--no-pager", "-o", "short-monotonic", "-n", fmt.Sprint(lines)}
	for _, u := range units {
		if !unitName.MatchString(u) {
			return fmt.Errorf("invalid unit %q", u)
		}
		args = append(args, "-u", u)
	}
	cmd := exec.Command("journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line(sc.Text())
	}
	return cmd.Wait()
}
