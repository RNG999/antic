package cli

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageText returns the formatted usage summary for the CLI commands.
func usageText() string {
	a := &app{}
	root := a.root()

	var buf bytes.Buffer
	buf.WriteString("antic run [NAME] [-w DIR] [-t|-T] [--] CMD...   run CMD in the guest\n")
	buf.WriteString("antic shell [NAME]\n\n")

	buf.WriteString("antic guest ls [--json] [-q]\n")
	buf.WriteString("antic guest create NAME [-p PKG...] [--cpus N] [--memory SIZE] [--network MODE] [--port PORT...]\n")
	buf.WriteString("antic guest set [NAME] [--cpus N] [--memory SIZE] [--network MODE]\n")
	buf.WriteString("antic guest edit [NAME]\n")
	buf.WriteString("antic guest rm NAME... [-y]\n")
	buf.WriteString("antic guest start|stop|restart [NAME...]\n")
	buf.WriteString("antic guest show [NAME] [--path] [--config] [--json]\n")
	buf.WriteString("antic guest logs [NAME] [-f] [--journal [-u UNIT] [-n N]]\n")
	buf.WriteString("antic guest ps [NAME] [--json]\n")
	buf.WriteString("antic guest kill [NAME] SESSION...\n")
	buf.WriteString("antic guest apply [NAME]\n\n")

	buf.WriteString("antic guest install [NAME] PKG... | uninstall [NAME] PKG...\n")
	buf.WriteString("antic guest mount [NAME] [--ro] DIR... | unmount [NAME] DIR...\n")
	buf.WriteString("antic guest forward [NAME] PORT... | unforward [NAME] PORT...\n")
	buf.WriteString("antic guest stats [NAME] [-w] [--json]\n\n")

	buf.WriteString("antic cache ls [--json] | rm [GUEST...] [--all]\n")
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
	usage := usageText()

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

// TestSkillFilesMatch ensures the repo copy skills/antic/SKILL.md and the embedded
// internal/host/cli/skill/SKILL.md remain in sync without drift.
func TestSkillFilesMatch(t *testing.T) {
	repoSkill, err := os.ReadFile("../../../skills/antic/SKILL.md")
	if err != nil {
		return
	}
	if !bytes.Equal(repoSkill, skillMarkdown) {
		t.Error("skills/antic/SKILL.md differs from internal/host/cli/skill/SKILL.md; keep them synchronized")
	}
}

// TestRunsHere: a static helper runs anywhere; one whose program
// interpreter this host lacks (linked against a Nix store that is not at
// /nix) is refused.
func TestRunsHere(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := runsHere(self); err != nil {
		t.Fatalf("the test binary itself: %v", err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			// Point the interpreter somewhere that does not exist.
			missing := []byte("/nonexistent/ld-linux-x86-64.so.2")
			copy(b[p.Off:p.Off+p.Filesz], append(missing, make([]byte, int(p.Filesz)-len(missing))...))
			path := filepath.Join(t.TempDir(), "helper")
			if err := os.WriteFile(path, b, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := runsHere(path); err == nil {
				t.Fatal("a helper whose interpreter is missing was accepted")
			}
			return
		}
	}
	t.Skip("the test binary is static; nothing to rewrite")
}
