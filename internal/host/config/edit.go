package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	mem := g.Memory
	if mem == "" {
		mem = "4G"
	}
	lines = append(lines, "", "[spec]",
		fmt.Sprintf("cpus = %d", cpus),
		"memory = "+tomlString(mem))

	lines = append(lines, "", "[network]")
	netMode := g.Network.Mode
	if netMode == "" {
		netMode = NetworkInternet
	}
	lines = append(lines, "mode = "+tomlString(netMode))
	if len(g.Network.Allow) > 0 {
		lines = append(lines, "allow = "+tomlArray(g.Network.Allow))
	}
	if len(g.Network.Deny) > 0 {
		lines = append(lines, "deny = "+tomlArray(g.Network.Deny))
	}
	for _, p := range g.Ports {
		lines = append(lines, "", "[[network.ports]]",
			fmt.Sprintf("host = %d", p.Host),
			fmt.Sprintf("guest = %d", p.Guest))
		if p.Description != "" {
			lines = append(lines, "description = "+tomlString(p.Description))
		}
	}
	for _, s := range g.Shares {
		sp := s.Source
		if sp == "" {
			sp = s.Path
		}
		mode := "rw"
		if s.ReadOnly || s.Mode == "ro" {
			mode = "ro"
		}
		lines = append(lines, "", "[[shares]]",
			"source = "+tomlString(tildePath(sp, home)),
			"mode = "+tomlString(mode))
	}
	if len(g.Env) > 0 {
		lines = append(lines, "", "[env]")
		var keys []string
		for k := range g.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("%s = %s", k, tomlString(g.Env[k])))
		}
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
	sp := s.Source
	if sp == "" {
		sp = s.Path
	}
	if g := c.Guests[guest]; g != nil {
		for _, have := range g.Shares {
			hp := have.Source
			if hp == "" {
				hp = have.Path
			}
			if hp == sp {
				return nil
			}
		}
	}
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		mode := "rw"
		if s.ReadOnly || s.Mode == "ro" {
			mode = "ro"
		}
		lines = append(lines, "", "[[shares]]",
			"source = "+tomlString(tildePath(sp, home)),
			"mode = "+tomlString(mode))
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
			sp := s.Source
			if sp == "" {
				sp = s.Path
			}
			p, _ := absPath(sp, home, "", nil)
			return p == path
		})
		if removed == 0 {
			return nil, fmt.Errorf("%s is not a configured share of %s", path, guest)
		}
		return out, nil
	})
}

// AddPort appends a [[network.ports]] entry unless host port is already configured.
func (c *Config) AddPort(guest string, p Port, home string) error {
	if g := c.Guests[guest]; g != nil {
		for _, have := range g.Ports {
			if have.Host == p.Host {
				return fmt.Errorf("host port %d is already forwarded on %s", p.Host, guest)
			}
		}
	}
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		lines = append(lines, "", "[[network.ports]]",
			fmt.Sprintf("host = %d", p.Host),
			fmt.Sprintf("guest = %d", p.Guest))
		if p.Description != "" {
			lines = append(lines, "description = "+tomlString(p.Description))
		}
		return lines, nil
	})
}

// RemovePort removes the [[network.ports]] or [[ports]] entry for hostPort.
func (c *Config) RemovePort(guest string, hostPort int, home string) error {
	return c.editGuest(guest, home, func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "ports" || h == "network.ports"
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
			if hasTable(lines, "spec") {
				lines = setTableKey(lines, "spec", "cpus", strconv.Itoa(*u.CPUs))
			} else {
				lines = setRootKey(lines, "cpus", strconv.Itoa(*u.CPUs))
			}
		}
		if u.Memory != nil {
			if hasTable(lines, "spec") {
				lines = setTableKey(lines, "spec", "memory", tomlString(*u.Memory))
			} else {
				lines = setRootKey(lines, "memory", tomlString(*u.Memory))
			}
		}
		if u.Network != nil {
			lines = setTableKey(lines, "network", "mode", tomlString(*u.Network))
		}
		return lines, nil
	})
}

func hasTable(lines []string, tableName string) bool {
	header := "[" + tableName + "]"
	for _, l := range lines {
		if strings.TrimSpace(l) == header {
			return true
		}
	}
	return false
}

// setTableKey sets key in [tableName] table. If [tableName] does not exist,
// it creates it before any array of tables ([[...]]).
func setTableKey(lines []string, tableName, key, valueExpr string) []string {
	tableHeader := "[" + tableName + "]"
	start := -1
	end := len(lines)
	for i, l := range lines {
		trim := strings.TrimSpace(l)
		if trim == tableHeader {
			start = i
			continue
		}
		if start != -1 {
			if strings.HasPrefix(trim, "[") {
				end = i
				break
			}
		}
	}

	if start == -1 {
		insertAt := len(lines)
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "[[") {
				insertAt = i
				break
			}
		}
		var newLines []string
		newLines = append(newLines, lines[:insertAt]...)
		if insertAt > 0 && lines[insertAt-1] != "" {
			newLines = append(newLines, "")
		}
		newLines = append(newLines, tableHeader, key+" = "+valueExpr)
		if insertAt < len(lines) && lines[insertAt] != "" {
			newLines = append(newLines, "")
		}
		newLines = append(newLines, lines[insertAt:]...)
		return newLines
	}

	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i := start + 1; i < end; i++ {
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

	insertAt := end
	for insertAt > start+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	var out []string
	out = append(out, lines[:insertAt]...)
	out = append(out, key+" = "+valueExpr)
	out = append(out, lines[insertAt:]...)
	return out
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
