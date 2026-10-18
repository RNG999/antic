// Command hanare manages the hanare NixOS VM and runs commands inside it.
//
// Invoked as "hanare <subcommand>", or through a symlink named after a tool
// (see "hanare link"), in which case that tool runs inside the VM in the
// current directory.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const usage = `usage: hanare <command> [args...]

  run CMD [ARGS...]   run CMD in the VM, in the current directory (starts the VM if needed)
  shell               interactive login shell in the VM
  up                  start the VM and wait until it accepts SSH
  down                shut the VM down
  status              show VM state
  logs                print the VM console log
  switch              apply ~/.config/hanare to the running VM (nixos-rebuild switch)
  reinstall [-y]      build a fresh disk image from ~/.config/hanare and replace the VM disk
  update              build this tool from ~/.config/hanare and install it to ~/.local/bin

  share               list shared directories
  share [--ro] [--save] DIR...
                      share DIR with the running VM at the same path (--save: also add to hanare.json)
  unshare DIR...      remove a share added with "share"

  link                sync ~/.local/bin links with "links" in hanare.json
  link TOOL...        create ~/.local/bin/TOOL links that run TOOL in the VM
  unlink TOOL...      remove such links
  links               list such links
`

func main() {
	os.Exit(dispatch())
}

func dispatch() int {
	if name := filepath.Base(os.Args[0]); name != "hanare" {
		return runCmd(append([]string{name}, os.Args[1:]...))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "run":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		return runCmd(args)
	case "shell":
		return shell()
	case "up":
		err = up()
	case "down":
		err = down()
	case "status":
		err = status()
	case "logs":
		err = logs()
	case "switch":
		return switchConfig()
	case "reinstall":
		err = reinstall(len(args) > 0 && (args[0] == "-y" || args[0] == "--yes"))
	case "share", "shares":
		err = share(args)
	case "unshare":
		err = unshare(args)
	case "update":
		err = selfUpdate()
	case "link":
		err = link(args)
	case "unlink":
		err = unlink(args)
	case "links":
		err = listLinks()
	case "_vm":
		err = execVM()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "hanare: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 1
	}
	return 0
}

func runCmd(argv []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	dir, err := cfg.guestDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return cfg.remote("cd "+shellQuote(dir)+" && exec "+strings.Join(quoted, " "), false)
}

func shell() int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	dir, err := cfg.guestDir()
	if err != nil {
		dir = "/home/" + cfg.User
	}
	return cfg.remote("cd "+shellQuote(dir)+` && exec "$SHELL" -l`, true)
}

// switchConfig rebuilds the guest from the read-only config share and
// streams the rebuild log.
func switchConfig() int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	return cfg.remote(`journalctl -f -n0 -o cat -u hanare-switch.service & j=$!
systemctl start hanare-switch.service; rc=$?
sleep 1; kill $j; exit $rc`, false)
}

func link(tools []string) error {
	if len(tools) == 0 {
		return syncLinks()
	}
	for _, t := range tools {
		if err := addLink(t); err != nil {
			return err
		}
	}
	return nil
}

// syncLinks makes the hanare links in ~/.local/bin match hanare.json.
func syncLinks() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, t := range cfg.Links {
		want[t] = true
		if err := addLink(t); err != nil {
			fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		}
	}
	for _, t := range currentLinks() {
		if !want[t] {
			os.Remove(filepath.Join(binDir(), t))
			fmt.Println("removed", filepath.Join(binDir(), t))
		}
	}
	return nil
}

func addLink(t string) error {
	if t == "" || strings.ContainsRune(t, '/') || t == "hanare" {
		return fmt.Errorf("invalid tool name %q", t)
	}
	p := filepath.Join(binDir(), t)
	if dst, err := os.Readlink(p); err == nil && dst == "hanare" {
		return nil
	} else if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("%s already exists and is not a hanare link", p)
	}
	// ~/.local/bin comes late in PATH, so a host command would win.
	if host := hostCommand(t); host != "" {
		return fmt.Errorf("%s: skipped, shadowed by host %s (use \"hanare run %s\")", t, host, t)
	}
	if err := os.Symlink("hanare", p); err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

// hostCommand returns the first PATH entry other than ~/.local/bin that
// provides name.
func hostCommand(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == binDir() || dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func currentLinks() []string {
	entries, _ := os.ReadDir(binDir())
	var out []string
	for _, e := range entries {
		if dst, err := os.Readlink(filepath.Join(binDir(), e.Name())); err == nil && dst == "hanare" {
			out = append(out, e.Name())
		}
	}
	return out
}

func unlink(tools []string) error {
	for _, t := range tools {
		p := filepath.Join(binDir(), filepath.Base(t))
		if dst, err := os.Readlink(p); err != nil || dst != "hanare" {
			return fmt.Errorf("%s is not a hanare link", p)
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

func listLinks() error {
	for _, t := range currentLinks() {
		fmt.Println(t)
	}
	return nil
}

func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, os.ErrClosed) && line == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
