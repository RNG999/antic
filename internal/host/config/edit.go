package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Edits operate on a single guest's file (~/.config/antic/guests/<name>.toml),
// preserving user comments and formatting, and validating the result with
// LoadGuest before atomically replacing the file.

// CreateGuest writes a new ~/.config/antic/guests/<name>.toml file.
func (c *Config) CreateGuest(name string, g Guest, home string) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	path := c.GuestPath(name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("guest config %s already exists", path)
	}
	var lines []string
	if g.Description != "" {
		lines = append(lines, "description = "+tomlString(g.Description))
	}
	if g.Flake != "" {
		lines = append(lines, "flake = "+tomlString(tildePath(g.Flake, home)))
	} else {
		lines = append(lines, "packages = "+tomlArray(g.Packages))
	}
	cpus := g.CPUs
	if cpus == 0 {
		cpus = 2
	}
	lines = append(lines, fmt.Sprintf("cpus = %d", cpus))
	mem := g.Memory
	if mem == "" {
		mem = "4G"
	}
	lines = append(lines, "memory = "+tomlString(mem))
	netMode := g.Network
	if netMode == "" {
		netMode = NetworkInternet
	}
	lines = append(lines, "network = "+tomlString(netMode))
	for _, s := range g.Shares {
		lines = append(lines, "", "[[shares]]", "path = "+tomlString(tildePath(s.Path, home)))
		if s.ReadOnly {
			lines = append(lines, "readonly = true")
		}
	}
	for _, p := range g.Ports {
		lines = append(lines, "", "[[ports]]",
			fmt.Sprintf("host = %d", p.Host),
			fmt.Sprintf("guest = %d", p.Guest))
	}
	return writeGuestAtomic(path, name, home, lines)
}

// RemoveGuest deletes ~/.config/antic/guests/<name>.toml.
func (c *Config) RemoveGuest(name string) error {
	path := c.GuestPath(name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(c.Guests, name)
	return nil
}

// SetPackages updates packages in ~/.config/antic/guests/<name>.toml.
func (c *Config) SetPackages(guest string, pkgs []string, home string) error {
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		return setRootKey(lines, "packages", tomlArray(pkgs)), nil
	})
}

// AddShare appends a [[shares]] entry unless already configured.
func (c *Config) AddShare(guest string, s Share, home string) error {
	if g := c.Guests[guest]; g != nil {
		for _, have := range g.Shares {
			if have.Path == s.Path {
				return nil
			}
		}
	}
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		lines = append(lines, "", "[[shares]]", "path = "+tomlString(tildePath(s.Path, home)))
		if s.ReadOnly {
			lines = append(lines, "readonly = true")
		}
		return lines, nil
	})
}

// RemoveShare removes the [[shares]] entry at path.
func (c *Config) RemoveShare(guest, path, home string) error {
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "shares"
		}, func(body []string) bool {
			var s Share
			if _, err := toml.Decode(strings.Join(body, "\n"), &s); err != nil {
				return false
			}
			p, _ := absPath(s.Path, home, "", nil)
			return p == path
		})
		if removed == 0 {
			return nil, fmt.Errorf("%s is not a configured share of %s", path, guest)
		}
		return out, nil
	})
}

// AddPort appends a [[ports]] entry unless host port is already configured.
func (c *Config) AddPort(guest string, p Port, home string) error {
	if g := c.Guests[guest]; g != nil {
		for _, have := range g.Ports {
			if have.Host == p.Host {
				return fmt.Errorf("host port %d is already forwarded on %s", p.Host, guest)
			}
		}
	}
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		lines = append(lines, "", "[[ports]]",
			fmt.Sprintf("host = %d", p.Host),
			fmt.Sprintf("guest = %d", p.Guest))
		return lines, nil
	})
}

// RemovePort removes the [[ports]] entry for hostPort.
func (c *Config) RemovePort(guest string, hostPort int, home string) error {
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "ports"
		}, func(body []string) bool {
			var p Port
			if _, err := toml.Decode(strings.Join(body, "\n"), &p); err != nil {
				return false
			}
			return p.Host == hostPort
		})
		if removed == 0 {
			return nil, fmt.Errorf("port %d is not configured on %s", hostPort, guest)
		}
		return out, nil
	})
}

// ResourceUpdates specifies optional updates to a guest's cpus, memory, and network.
type ResourceUpdates struct {
	CPUs    *int
	Memory  *string
	Network *string
}

// SetResources updates cpus, memory, and network for guest.
func (c *Config) SetResources(guest string, u ResourceUpdates, home string) error {
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		if u.CPUs != nil {
			lines = setRootKey(lines, "cpus", strconv.Itoa(*u.CPUs))
		}
		if u.Memory != nil {
			lines = setRootKey(lines, "memory", tomlString(*u.Memory))
		}
		if u.Network != nil {
			lines = setRootKey(lines, "network", tomlString(*u.Network))
		}
		return lines, nil
	})
}

// setRootKey sets or deletes key in the root (before the first [table] or [[array]]).
func setRootKey(lines []string, key, valueExpr string) []string {
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	end := len(lines)
	for i, l := range lines {
		if _, ok := header(l); ok {
			end = i
			break
		}
	}
	for i := 0; i < end; i++ {
		if !keyRE.MatchString(lines[i]) {
			continue
		}
		j := i
		depth := bracketDepth(lines[i])
		for depth > 0 && j+1 < end {
			j++
			depth += bracketDepth(lines[j])
		}
		var out []string
		out = append(out, lines[:i]...)
		if valueExpr != "" {
			out = append(out, key+" = "+valueExpr)
		}
		out = append(out, lines[j+1:]...)
		return out
	}
	if valueExpr == "" {
		return lines
	}
	insertAt := end
	for insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	var out []string
	out = append(out, lines[:insertAt]...)
	out = append(out, key+" = "+valueExpr)
	out = append(out, lines[insertAt:]...)
	return out
}

// bracketDepth returns net '[' minus ']' outside strings and comments on a line.
func bracketDepth(line string) int {
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if ch == '\\' && quote == '"' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			quote = ch
		case '#':
			return depth
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

func (c *Config) editGuest(guest, home string, change func([]string) ([]string, error)) error {
	path := c.GuestPath(guest)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return err
	}
	lines, err := change(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
	if err != nil {
		return err
	}
	return writeGuestAtomic(real, guest, home, lines)
}

func writeGuestAtomic(real, name, home string, lines []string) error {
	out := []byte(strings.Join(lines, "\n") + "\n")
	tmp, err := os.CreateTemp(filepath.Dir(real), "."+name+".toml.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := LoadGuest(tmp.Name(), name, home); err != nil {
		return fmt.Errorf("refusing to write %s, the result would not load: %w", real, err)
	}
	if fi, err := os.Stat(real); err == nil {
		os.Chmod(tmp.Name(), fi.Mode().Perm())
	} else {
		os.Chmod(tmp.Name(), 0o644)
	}
	return os.Rename(tmp.Name(), real)
}

// header returns the table name of a [table] or [[array]] header line.
func header(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return "", false
	}
	return strings.TrimSpace(strings.Trim(t, "[]")), true
}

// removeBlocks drops tables whose header matches (and whose body matches,
// if body is set), with the comment lines directly above them.
func removeBlocks(lines []string, match func(string) bool, body func([]string) bool) ([]string, int) {
	var out []string
	removed := 0
	for i := 0; i < len(lines); {
		h, ok := header(lines[i])
		end := i + 1
		for end < len(lines) {
			if _, ok := header(lines[end]); ok {
				break
			}
			end++
		}
		if ok && match(h) && (body == nil || body(lines[i+1:end])) {
			for len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") {
				out = out[:len(out)-1]
			}
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			removed++
			i = end
			continue
		}
		out = append(out, lines[i:end]...)
		i = end
	}
	return out, removed
}

func tildePath(p, home string) string {
	if within(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func tomlString(s string) string {
	var buf bytes.Buffer
	toml.NewEncoder(&buf).Encode(map[string]string{"v": s})
	return strings.TrimSpace(strings.TrimPrefix(buf.String(), "v = "))
}

func tomlArray(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = tomlString(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
