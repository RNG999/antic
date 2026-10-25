package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const home = "/home/alice"

func writeGuest(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const validDev = `# the dev guest
description = "tools for my agent"
packages = ["git", "ripgrep"]
cpus = 4
memory = "8G"

[[shares]]
path = "~/Work"

[[shares]]
path = "~/Docs/spec"
readonly = true

[[ports]]
host = 3000

[[ports]]
host = 8080
guest = 80
`

const validLab = `flake = "/srv/guest#lab"

[network]
mode = "none"
`

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	writeGuest(t, dir, "dev", validDev)
	writeGuest(t, dir, "lab", validLab)

	c, err := Load(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Guests["dev"]
	if len(g.Packages) != 2 || g.Packages[0] != "git" || g.CPUs != 4 ||
		g.MemoryBytes != 8<<30 || g.Network.Mode != NetworkInternet || len(g.Ports) != 2 ||
		g.Ports[0].Host != 3000 || g.Ports[0].Guest != 3000 ||
		g.Ports[1].Host != 8080 || g.Ports[1].Guest != 80 ||
		g.Shares[0].Path != "/home/alice/Work" || g.Shares[0].ReadOnly ||
		g.Shares[1].Path != "/home/alice/Docs/spec" || !g.Shares[1].ReadOnly {
		t.Fatalf("unexpected guest %+v", g)
	}
	lab := c.Guests["lab"]
	if lab.FlakePath != "/srv/guest" || lab.FlakeAttr != "lab" || lab.Network.Mode != NetworkNone {
		t.Fatalf("unexpected lab flake %+v", lab)
	}
	if _, err := c.Get(""); err == nil {
		t.Fatalf("Get(\"\") succeeded, want syntax error")
	}
	if p, err := c.Get("dev"); err != nil || p.Name != "dev" {
		t.Fatalf("Get(\"dev\") = %v, %v", p, err)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"unknown key": {strings.Replace(validDev, `path = "~/Work"`, "path = \"~/Work\"\nread_only = true", 1), `did you mean "readonly"`},
		"bad memory":  {strings.Replace(validDev, `"8G"`, `"8GiB"`, 1), "memory"},
		"bad port":    {strings.Replace(validDev, `host = 3000`, `host = 70000`, 1), "ports"},
		"dup port":    {strings.Replace(validDev, `host = 3000`, `host = 8080`, 1), "duplicate host port"},
		"relative":    {strings.Replace(validLab, `"/srv/guest#lab"`, `"guest#lab"`, 1), "must be absolute"},
		"network":     {strings.Replace(validLab, `"none"`, `"off"`, 1), "network.mode must be"},
		"overlap":     {validDev + "\n[[shares]]\npath = \"~/Work/sub\"\n", "overlap"},
		"allow none":  {validLab + "allow = [\"github.com\"]\n", "network.allow cannot be used"},
		"allow bare ip": {"[network]\nallow = [\"1.1.1.1\"]\n", "IP addresses must use CIDR notation"},
		"allow lo":    {"[network]\nallow = [\"127.0.0.1/32\"]\n", "never reach"},
		"allow all":   {"[network]\nallow = [\"0.0.0.0/0\"]\n", "never reach"},
		"allow all6":  {"[network]\nallow = [\"::/0\"]\n", "never reach"},
		"allow meta":  {"[network]\nallow = [\"169.254.169.254/32\"]\n", "never reach"},
		"allow tld":   {"[network]\nallow = [\"com\"]\n", "at least two labels"},
		"allow bad":   {"[network]\nallow = [\"git hub.com\"]\n", "invalid domain"},
		"allow cidr":  {"[network]\nallow = [\"10.0.0.0/33\"]\n", "invalid CIDR"},
		"allow empty": {"[network]\nallow = [\"\"]\n", "empty"},
	} {
		dir := t.TempDir()
		writeGuest(t, dir, "dev", tc.content)
		c, err := Load(dir, home)
		if err == nil {
			_, err = c.Get("dev")
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.want)
		}
	}
}

func TestEdits(t *testing.T) {
	dir := t.TempDir()
	devPath := writeGuest(t, dir, "dev", validDev)
	writeGuest(t, dir, "lab", validLab)

	c, err := Load(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	cpus := 6
	mem := "16G"
	net := "lan"
	steps := []func(*Config) error{
		func(c *Config) error {
			return c.CreateGuest("new", Guest{Packages: []string{"git"}}, home)
		},
		func(c *Config) error { return c.AddShare("new", Share{Path: "/home/alice/a b", ReadOnly: true}, home) },
		func(c *Config) error { return c.SetPackages("dev", []string{"git", "jq"}, home) },
		func(c *Config) error { return c.AddPort("new", Port{Host: 5173, Guest: 5173}, home) },
		func(c *Config) error { return c.RemovePort("dev", 3000, home) },
		func(c *Config) error {
			return c.SetResources("new", ResourceUpdates{CPUs: &cpus, Memory: &mem, Network: &net}, home)
		},
		func(c *Config) error { return c.RemoveShare("dev", "/home/alice/Work", home) },
		func(c *Config) error { return c.RemoveGuest("lab") },
	}
	for i, step := range steps {
		if err := step(c); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if c, err = Load(dir, home); err != nil {
			t.Fatalf("step %d: reload: %v", i, err)
		}
	}
	if c.Guests["lab"] != nil || len(c.Guests["dev"].Shares) != 1 || len(c.Guests["dev"].Ports) != 1 ||
		strings.Join(c.Guests["dev"].Packages, ",") != "git,jq" ||
		c.Guests["new"].Shares[0].Path != "/home/alice/a b" || !c.Guests["new"].Shares[0].ReadOnly ||
		c.Guests["new"].CPUs != 6 || c.Guests["new"].Memory != "16G" || c.Guests["new"].Network.Mode != NetworkLAN ||
		len(c.Guests["new"].Ports) != 1 {
		b, _ := os.ReadFile(devPath)
		t.Fatalf("unexpected result:\n%s", b)
	}
	if b, _ := os.ReadFile(devPath); !strings.Contains(string(b), "# the dev guest") {
		t.Fatalf("comment lost:\n%s", b)
	}
}

func TestLoadNewFormat(t *testing.T) {
	const newDev = `description = "new format guest"
packages = ["git"]

[spec]
cpus = 4
memory = "8G"

[network]
mode = "internet"

[[network.ports]]
host = 3000
guest = 3000

[[network.ports]]
host = 8080
guest = 80

[[shares]]
source = "~/Work"
mode = "rw"

[[shares]]
source = "~/Docs/spec"
mode = "ro"

[env]
EDITOR = "nvim"
DEBUG = "1"
`
	dir := t.TempDir()
	writeGuest(t, dir, "newdev", newDev)

	c, err := Load(dir, home)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	g := c.Guests["newdev"]
	if g == nil {
		t.Fatalf("guest not found")
	}
	if g.CPUs != 4 || g.Spec.CPUs != 4 || g.Memory != "8G" || g.Spec.Memory != "8G" {
		t.Errorf("unexpected spec: cpus=%d, memory=%s", g.CPUs, g.Memory)
	}
	if len(g.Ports) != 2 || g.Ports[0].Host != 3000 || g.Ports[1].Guest != 80 {
		t.Errorf("unexpected ports: %+v", g.Ports)
	}
	if len(g.Shares) != 2 || g.Shares[0].Path != "/home/alice/Work" || g.Shares[0].Mode != "rw" || g.Shares[0].ReadOnly {
		t.Errorf("unexpected share 0: %+v", g.Shares[0])
	}
	if g.Shares[1].Path != "/home/alice/Docs/spec" || g.Shares[1].Mode != "ro" || !g.Shares[1].ReadOnly {
		t.Errorf("unexpected share 1: %+v", g.Shares[1])
	}
	if g.Env["EDITOR"] != "nvim" || g.Env["DEBUG"] != "1" {
		t.Errorf("unexpected env: %+v", g.Env)
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
