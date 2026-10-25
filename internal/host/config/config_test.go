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

[spec]
cpus = 4
memory = "8G"

[network]
mode = "internet"

[[network.ports]]
host = 3000

[[network.ports]]
host = 8080
guest = 80

[[shares]]
source = "~/Work"
mode = "rw"

[[shares]]
source = "~/Docs/spec"
mode = "ro"
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
		"unknown key":             {validDev + "\nunknown_field = true\n", "unknown key"},
		"bad memory":              {strings.Replace(validDev, `"8G"`, `"8GiB"`, 1), "memory"},
		"bad port":                {strings.Replace(validDev, `host = 3000`, `host = 70000`, 1), "network.ports"},
		"dup port":                {strings.Replace(validDev, `host = 3000`, `host = 8080`, 1), "duplicate host port"},
		"relative":                {strings.Replace(validLab, `"/srv/guest#lab"`, `"guest#lab"`, 1), "must be absolute"},
		"network":                 {strings.Replace(validLab, `"none"`, `"off"`, 1), "network.mode must be"},
		"overlap":                 {validDev + "\n[[shares]]\nsource = \"~/Work/sub\"\nmode = \"rw\"\n", "overlap"},
		"reject top-level cpus":   {validDev + "\ncpus = 2\n", "unknown key"},
		"reject top-level memory": {validDev + "\nmemory = \"4G\"\n", "unknown key"},
		"reject top-level ports":  {validDev + "\n[[ports]]\nhost = 8080\n", "unknown key"},
		"reject legacy share path":     {validDev + "\n[[shares]]\npath = \"~/tmp\"\n", "unknown key"},
		"reject legacy share readonly": {validDev + "\n[[shares]]\nsource = \"~/tmp\"\nreadonly = true\n", "unknown key"},
		"reject scalar network":        {"description = \"test\"\nnetwork = \"internet\"\n[spec]\ncpus = 4\nmemory = \"8G\"\n", "type mismatch"},
		"allow none":              {validLab + "allow = [\"github.com\"]\n", "network.allow cannot be used"},
		"allow bare ip":           {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"1.1.1.1\"]\n", "IP addresses must use CIDR notation"},
		"allow lo":                {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"127.0.0.1/32\"]\n", "never reach"},
		"allow all":               {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"0.0.0.0/0\"]\n", "never reach"},
		"allow all6":              {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"::/0\"]\n", "never reach"},
		"allow meta":              {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"169.254.169.254/32\"]\n", "never reach"},
		"allow tld":               {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"com\"]\n", "at least two labels"},
		"allow bad":               {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"git hub.com\"]\n", "invalid domain"},
		"allow cidr":              {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"10.0.0.0/33\"]\n", "invalid CIDR"},
		"allow empty":             {"[spec]\ncpus = 2\nmemory = \"2G\"\n\n[network]\nmode = \"internet\"\nallow = [\"\"]\n", "empty"},
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
		func(c *Config) error { return c.AddShare("new", Share{Source: "/home/alice/a b", Mode: "ro"}, home) },
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

func TestPortProtocols(t *testing.T) {
	tomlData := `
[spec]
cpus = 2
memory = "4G"

[network]
mode = "internet"

[[network.ports]]
host = 53
guest = 53
proto = "tcp,udp"
description = "DNS server"

[[network.ports]]
host = 8080
guest = 80
proto = "tcp"

[[network.ports]]
host = 8080
guest = 80
proto = "udp"
`
	dir := t.TempDir()
	writeGuest(t, dir, "proto", tomlData)

	c, err := Load(dir, home)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	g, err := c.Get("proto")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if len(g.Ports) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(g.Ports))
	}
	if !g.Ports[0].TCP() || !g.Ports[0].UDP() || g.Ports[0].ProtoStr() != "tcp,udp" {
		t.Errorf("port 0 should be tcp,udp: %+v", g.Ports[0])
	}
	if !g.Ports[1].TCP() || g.Ports[1].UDP() || g.Ports[1].ProtoStr() != "tcp" {
		t.Errorf("port 1 should be tcp: %+v", g.Ports[1])
	}
	if g.Ports[2].TCP() || !g.Ports[2].UDP() || g.Ports[2].ProtoStr() != "udp" {
		t.Errorf("port 2 should be udp: %+v", g.Ports[2])
	}

	conflictToml := `
[spec]
cpus = 2
memory = "4G"

[network]
mode = "internet"

[[network.ports]]
host = 8080
guest = 80
proto = "tcp"

[[network.ports]]
host = 8080
guest = 8080
proto = "tcp,udp"
`
	dir2 := t.TempDir()
	writeGuest(t, dir2, "conflict", conflictToml)
	c2, err := Load(dir2, home)
	if err == nil {
		_, err = c2.Get("conflict")
	}
	if err == nil || !strings.Contains(err.Error(), "duplicate host port 8080") {
		t.Fatalf("expected duplicate host port error, got: %v", err)
	}
}

func TestParsePort(t *testing.T) {
	cases := []struct {
		raw       string
		wantHost  int
		wantGuest int
		wantProto string
	}{
		{"80", 80, 80, ""},
		{"8080:80", 8080, 80, ""},
		{"53/udp", 53, 53, "udp"},
		{"53:53/tcp,udp", 53, 53, "tcp,udp"},
		{"443:443/tcp", 443, 443, "tcp"},
	}
	for _, tc := range cases {
		p, err := ParsePort(tc.raw)
		if err != nil {
			t.Errorf("ParsePort(%q): unexpected err: %v", tc.raw, err)
			continue
		}
		if p.Host != tc.wantHost || p.Guest != tc.wantGuest || p.Proto != tc.wantProto {
			t.Errorf("ParsePort(%q) = %+v, want Host=%d, Guest=%d, Proto=%q", tc.raw, p, tc.wantHost, tc.wantGuest, tc.wantProto)
		}
	}
}
