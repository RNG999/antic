package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const home = "/home/alice"

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "antic.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `version = 1
default_guest = "dev"

# the dev guest
[guests.dev]
flake = "~/.config/antic/guest"
memory = "8G"

[guests.dev.commands]
link = ["tlc"]

[[guests.dev.share]]
path = "~/Work"

[guests.lab]
flake = "/srv/guest"
attr = "lab"
network = "none"
`

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, valid), home)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Guests["dev"]
	if g.Flake != "/home/alice/.config/antic/guest" || g.Attr != "antic" || g.CPUs != 2 ||
		g.MemoryBytes != 8<<30 || g.Network != NetworkFiltered || g.Shares[0].Path != "/home/alice/Work" {
		t.Fatalf("unexpected guest %+v", g)
	}
	if p, _ := c.Pick(""); p.Name != "dev" {
		t.Fatalf("Pick() = %s", p.Name)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"unknown key": {strings.Replace(valid, `path = "~/Work"`, "path = \"~/Work\"\nreadonly = true", 1), `did you mean "read_only"`},
		"no version":  {strings.Replace(valid, "version = 1\n", "", 1), "missing \"version = 1\""},
		"bad memory":  {strings.Replace(valid, `"8G"`, `"8GiB"`, 1), "memory"},
		"relative":    {strings.Replace(valid, `"/srv/guest"`, `"guest"`, 1), "must be absolute"},
		"network":     {strings.Replace(valid, `"none"`, `"off"`, 1), "network must be"},
		"overlap":     {valid + "\n[[guests.dev.share]]\npath = \"~/Work/sub\"\n", "overlap"},
		"dup link":    {valid + "\n[guests.lab.commands]\nlink = [\"tlc\"]\n", "already linked"},
		"default":     {strings.Replace(valid, `"dev"`+"\n", `"nope"`+"\n", 1), "default_guest"},
	} {
		if _, err := Load(write(t, tc.content), home); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.want)
		}
	}
}

func TestEdits(t *testing.T) {
	p := write(t, valid)
	c, err := Load(p, home)
	if err != nil {
		t.Fatal(err)
	}
	steps := []func(*Config) error{
		func(c *Config) error { return c.AppendGuest("new", "/home/alice/g", "x", home) },
		func(c *Config) error { return c.AddShare("new", Share{Path: "/home/alice/a b", ReadOnly: true}, home) },
		func(c *Config) error { return c.SetLinks("dev", []string{"z3", "tlc"}, home) },
		func(c *Config) error { return c.SetLinks("new", []string{"cvc5"}, home) },
		func(c *Config) error { return c.RemoveShare("dev", "/home/alice/Work", home) },
		func(c *Config) error { return c.RemoveGuest("lab", home) },
	}
	for i, step := range steps {
		if err := step(c); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if c, err = Load(p, home); err != nil {
			t.Fatalf("step %d: reload: %v", i, err)
		}
	}
	if c.Guests["lab"] != nil || len(c.Guests["dev"].Shares) != 0 ||
		strings.Join(c.Guests["dev"].Commands.Link, ",") != "z3,tlc" ||
		c.Guests["new"].Shares[0].Path != "/home/alice/a b" || !c.Guests["new"].Shares[0].ReadOnly ||
		c.Guests["new"].Commands.Link[0] != "cvc5" {
		b, _ := os.ReadFile(p)
		t.Fatalf("unexpected result:\n%s", b)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "# the dev guest") {
		t.Fatalf("comment lost:\n%s", b)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"4G": 4 << 30, "512M": 512 << 20, "1024": 1024, "2k": 2048} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "G", "-1G", "4GiB", "x"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded", in)
		}
	}
}
