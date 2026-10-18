// Package host implements the hanare commands that run on the host: managing
// the VM and running commands inside it.
//
// hanare is invoked as "hanare <command>", or through a symlink named after a
// tool (see "hanare link"), in which case that tool runs inside the VM in the
// current directory.
package host

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const usage = `usage: hanare [-g GUEST] <command> [args...]

Guests are defined in ~/.config/hanare/hanare.toml ([guests.NAME]). NAME
arguments, -g or $HANARE_GUEST pick one; otherwise "default" from that file.

  run [--offline] CMD [ARGS...]
                      run CMD in the guest, in the current directory (starts it if needed);
                      --offline (or "offline" in hanare.toml): no network for CMD
  shell               interactive login shell in the guest
  audit [-f] [-n N]   the guest's activity log (executions, connections, writes)

  guest ls            list all guests and their state
  guest ps [NAME]     sessions ("hanare run"s) running in a guest
  guest up [NAME...]  start guests and wait until they accept SSH
  guest down [NAME...]
                      shut guests down
  guest status [NAME] state, SSH and /nix verification of a guest
  guest logs [NAME]   console log
  guest apply [NAME]  apply changes in the guest's flake to the running guest (nixos-rebuild switch)
  guest reset [-y] [NAME]
                      replace the guest's disk with a fresh image built from its flake
  guest create NAME [--from GUEST]
                      add [guests.NAME] to hanare.toml (using GUEST's OS) and build its disk
  guest rm [-y] NAME  delete a guest's disk and state

  share ls            the guest's shared directories
  share add [--ro] [--save] DIR...
                      share DIR with the running guest at the same path (--save: record it in hanare.toml)
  share rm DIR...     remove a share added with "share add"

  link ls             ~/.local/bin links that run tools in a guest
  link sync           make the links match "expose" of all guests
  link add TOOL...    add links (run in the guest exposing TOOL, else the default guest)
  link rm TOOL...     remove links

  update              build hanare from ~/.local/src/hanare and install it to ~/.local/bin

  vm restrict-userns | unrestrict-userns | audit [-uid N]
                      run inside the guest VM by its systemd units
`

// Main runs the hanare command line on the host and returns the exit status.
func Main() int {
	if name := filepath.Base(os.Args[0]); name != "hanare" {
		return runLinked(name, os.Args[1:])
	}
	args := os.Args[1:]
	guest := ""
	if len(args) >= 2 && (args[0] == "-g" || args[0] == "--guest") {
		guest, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	sub := ""
	if (cmd == "guest" || cmd == "share" || cmd == "link") && len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "update":
		return report(selfUpdate())
	case "link":
		switch sub {
		case "ls":
			return report(listLinks())
		case "rm":
			return report(unlink(args))
		}
	}

	f, err := loadFile()
	if err != nil {
		return report(err)
	}
	yes := len(args) > 0 && (args[0] == "-y" || args[0] == "--yes")
	if yes {
		args = args[1:]
	}
	switch cmd + " " + sub {
	case "guest ls":
		return report(ps(f))
	case "guest up", "guest down":
		names := args
		if len(names) == 0 {
			names = []string{guest}
		}
		for _, n := range names {
			g, err := f.pick(n)
			if err == nil {
				if sub == "up" {
					err = g.up()
				} else {
					err = g.down()
				}
			}
			if err != nil {
				return report(err)
			}
		}
		return 0
	case "guest create":
		return report(create(f, args))
	case "link sync":
		return report(syncLinks(f))
	case "link add":
		return report(addLinks(args))
	case "guest ps", "guest status", "guest logs", "guest apply", "guest reset", "guest rm":
		if len(args) > 0 {
			guest = args[0]
		}
		if sub == "rm" && guest == "" {
			return report(errors.New("usage: hanare guest rm [-y] NAME"))
		}
	}
	g, err := f.pick(guest)
	if err != nil {
		return report(err)
	}
	switch cmd + " " + sub {
	case "run ":
		offline := false
		if len(args) > 0 && args[0] == "--offline" {
			offline, args = true, args[1:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		return g.run(args, offline)
	case "shell ":
		return g.shell()
	case "audit ":
		return g.auditLog(args)
	case "guest ps":
		return g.sessions()
	case "guest status":
		return report(g.status())
	case "guest logs":
		return report(g.logs())
	case "guest apply":
		return g.apply()
	case "guest reset":
		return report(g.reset(yes))
	case "guest rm":
		return report(g.remove(yes))
	case "share ls":
		return report(g.listShares())
	case "share add":
		return report(g.share(args))
	case "share rm":
		return report(g.unshare(args))
	}
	fmt.Fprintf(os.Stderr, "hanare: unknown command %q\n\n%s", strings.TrimSpace(cmd+" "+sub), usage)
	return 2
}

func report(err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 1
	}
	return 0
}

// runLinked runs a tool invoked through a ~/.local/bin link: in the guest
// whose "expose" lists it, else in the default guest.
func runLinked(tool string, args []string) int {
	f, err := loadFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	var g *Guest
	for _, n := range f.names() {
		if slices.Contains(f.Guests[n].Expose, tool) {
			g = f.Guests[n]
			break
		}
	}
	if g == nil {
		if g, err = f.pick(""); err != nil {
			fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
			return 125
		}
	}
	return g.run(append([]string{tool}, args...), false)
}

// create adds [guests.NAME] to hanare.toml, using the OS (flake and attr) of
// another guest, and builds its disk.
func create(f *File, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	from := fs.String("from", "", "guest whose OS (flake, attr) to use (default: the default guest)")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" || !guestName.MatchString(name) {
		return errors.New("usage: hanare create NAME [--from GUEST] (NAME: a-z, 0-9 and -)")
	}
	if f.Guests[name] != nil {
		return fmt.Errorf("guest %s already exists", name)
	}
	src, err := f.pick(*from)
	if err != nil {
		return err
	}
	block := fmt.Sprintf("\n[guests.%s]\nflake = %q\nattr = %q\n", name, tildePath(src.Flake), src.Attr)
	cf, err := os.OpenFile(configFile(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := cf.WriteString(block); err != nil {
		cf.Close()
		return err
	}
	if err := cf.Close(); err != nil {
		return err
	}
	fmt.Printf("hanare: added [guests.%s] to %s (no shares yet)\n", name, configFile())
	if f, err = loadFile(); err != nil {
		return err
	}
	return f.Guests[name].reset(true)
}

func (g *Guest) run(argv []string, offline bool) int {
	dir, err := g.guestDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 125
	}
	offline = offline || slices.Contains(g.Offline, filepath.Base(argv[0]))
	if offline {
		g.sshPort = 23
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return g.remote("cd "+shellQuote(dir)+" && exec "+strings.Join(quoted, " "), false)
}

func (g *Guest) shell() int {
	dir, err := g.guestDir()
	if err != nil {
		dir = "/home/" + guestUser()
	}
	return g.remote("cd "+shellQuote(dir)+` && exec "$SHELL" -l`, true)
}

// auditLog shows the guest's activity log (hanare-audit.service, one JSON
// object per line), optionally following it.
func (g *Guest) auditLog(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	n := fs.Int("n", 100, "number of lines")
	fs.Parse(args)
	cmd := fmt.Sprintf("journalctl -u hanare-audit.service -o cat --no-pager -n %d", *n)
	if *follow {
		cmd += " -f"
	}
	return g.remote(cmd, false)
}

// sessions lists the "hanare run" sessions in the guest: one sshd unit
// instance each (see guest/module.nix), with the command it runs.
func (g *Guest) sessions() int {
	return g.remote(`self=$(sed -n 's|.*/||p' /proc/self/cgroup)
{
  printf 'SESSION\tMODE\tSTARTED\tCOMMAND\n'
  # Instances live in per-template slices (system-hanare\x2dssh.slice, ...).
  for d in $(find /sys/fs/cgroup/system.slice -maxdepth 2 -type d -name 'hanare-ssh*@*.service'); do
    u=${d##*/}
    [ "$u" = "$self" ] && continue
    mode=online
    case $u in hanare-ssh-offline@*) mode=offline ;; esac
    started=$(systemctl show -p ActiveEnterTimestamp --value "$u")
    cmd=$(for p in $(cat "$d/cgroup.procs"); do
      tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null; echo
    done | grep -v -e '^sshd' -e '^$' | head -1)
    printf '%s\t%s\t%s\t%s\n' "$u" "$mode" "$started" "$cmd"
  done
} | column -t -s "$(printf '\t')"`, false)
}

// apply rebuilds the running guest from its flake (shared read-only at
// /run/hanare-config) and streams the rebuild log.
func (g *Guest) apply() int {
	if err := g.checkFlake(true); err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 1
	}
	return g.remote(`journalctl -f -n0 -o cat -u hanare-apply.service & j=$!
systemctl start hanare-apply.service; rc=$?
sleep 1; kill $j; exit $rc`, false)
}

func addLinks(tools []string) error {
	for _, t := range tools {
		if err := addLink(t); err != nil {
			return err
		}
	}
	return nil
}

// syncLinks makes the hanare links in ~/.local/bin match "expose" of all guests.
func syncLinks(f *File) error {
	want := map[string]string{}
	for _, n := range f.names() {
		for _, t := range f.Guests[n].Expose {
			if other, ok := want[t]; ok {
				fmt.Fprintf(os.Stderr, "hanare: %s is exposed by both %s and %s; links run it in %s\n", t, other, n, other)
				continue
			}
			want[t] = n
			if err := addLink(t); err != nil {
				fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
			}
		}
	}
	for _, t := range currentLinks() {
		if _, ok := want[t]; !ok {
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
