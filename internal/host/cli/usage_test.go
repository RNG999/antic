package cli

import (
	"bytes"
	"strings"
	"testing"
)

// UsageText returns the formatted usage summary for the CLI commands.
func UsageText() string {
	a := &app{}
	root := a.root()

	var buf bytes.Buffer
	buf.WriteString("antic run NAME [--offline] [-w DIR] [-t|-T] [--] CMD...   run CMD in the guest\n")
	buf.WriteString("antic shell NAME\n\n")

	buf.WriteString("antic guest ls [--json] [-q]\n")
	buf.WriteString("antic guest create NAME [-p PKG...] [--cpus N] [--memory SIZE] [--network MODE] [--port PORT...]\n")
	buf.WriteString("antic guest set NAME [--cpus N] [--memory SIZE] [--network MODE]\n")
	buf.WriteString("antic guest edit NAME\n")
	buf.WriteString("antic guest path [NAME]\n")
	buf.WriteString("antic guest check\n")
	buf.WriteString("antic guest rm NAME... [-y]\n")
	buf.WriteString("antic guest start|stop|restart NAME...\n")
	buf.WriteString("antic guest inspect NAME [--json]\n")
	buf.WriteString("antic guest logs NAME [-f] [--journal [-u UNIT] [-n N]]\n")
	buf.WriteString("antic guest ps NAME [--json]\n")
	buf.WriteString("antic guest kill NAME SESSION...\n")
	buf.WriteString("antic guest apply NAME\n\n")

	buf.WriteString("antic guest pkg ls NAME [--json] | add NAME PKG... | rm NAME PKG...\n")
	buf.WriteString("antic guest share ls NAME [--json] | add NAME [--ro] DIR... | rm NAME DIR...\n")
	buf.WriteString("antic guest port ls NAME [--json] | add NAME PORT... | rm NAME PORT...\n\n")

	buf.WriteString("antic skill install [-p] | rm [-p] | path [-p]\n")
	buf.WriteString("antic doctor | update [--source DIR] | version | completion SHELL\n")

	_ = root
	return buf.String()
}

// TestRegisteredCommandsMatchUsage ensures all commands registered on the root and guest command
// are present in the UsageText and documented.
func TestRegisteredCommandsMatchUsage(t *testing.T) {
	a := &app{}
	root := a.root()
	usage := UsageText()

	// Verify root subcommands are in usage
	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		name := cmd.Name()
		if !strings.Contains(usage, "antic "+name) && !strings.Contains(usage, name+" ") {
			t.Errorf("command %q is registered on antic root but not documented in UsageText", name)
		}
	}

	// Verify guest subcommands are in usage
	guestCmd, _, err := root.Find([]string{"guest"})
	if err != nil || guestCmd == nil {
		t.Fatalf("guest command not found: %v", err)
	}

	for _, sub := range guestCmd.Commands() {
		if sub.Hidden {
			continue
		}
		subName := sub.Name()
		if !strings.Contains(usage, subName) {
			t.Errorf("guest subcommand %q is registered on antic guest but not documented in UsageText", subName)
		}
	}
}

// TestCompletionFunctionsRegistered verifies that guest commands taking arguments have ValidArgsFunction registered.
func TestCompletionFunctionsRegistered(t *testing.T) {
	a := &app{}
	root := a.root()

	guestCmd, _, _ := root.Find([]string{"guest"})
	for _, sub := range guestCmd.Commands() {
		if sub.Name() == "help" || sub.Name() == "ls" || len(sub.Commands()) > 0 {
			// Subcommand groups (pkg, port, share) or no-arg commands don't take a guest argument directly.
			continue
		}
		if sub.ValidArgsFunction == nil {
			t.Errorf("guest subcommand %q has no ValidArgsFunction registered for shell completion", sub.Name())
		}
	}
}
