package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Edits keep the user's formatting and comments: they change only the
// affected tables, and the result must load cleanly before it replaces the
// file (atomically, next to the real file if hanare.toml is a symlink).

// AppendGuest adds a [guests.NAME] table.
func (c *Config) AppendGuest(name, flake, attr, home string) error {
	return c.edit(home, func(lines []string) ([]string, error) {
		return append(lines, "", "[guests."+name+"]",
			"flake = "+tomlString(tildePath(flake, home)),
			"attr = "+tomlString(attr)), nil
	})
}

// RemoveGuest removes every table of guest name.
func (c *Config) RemoveGuest(name, home string) error {
	return c.edit(home, func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "guests."+name || strings.HasPrefix(h, "guests."+name+".")
		}, nil)
		if removed == 0 {
			return nil, fmt.Errorf("no [guests.%s] table in %s", name, c.path)
		}
		return out, nil
	})
}

// AddShare records a share for guest, unless it is already configured.
func (c *Config) AddShare(guest string, s Share, home string) error {
	for _, have := range c.Guests[guest].Shares {
		if have.Path == s.Path {
			return nil
		}
	}
	return c.edit(home, func(lines []string) ([]string, error) {
		lines = append(lines, "", "[[guests."+guest+".share]]", "path = "+tomlString(tildePath(s.Path, home)))
		if s.ReadOnly {
			lines = append(lines, "read_only = true")
		}
		return lines, nil
	})
}

// RemoveShare removes the configured share of guest at path.
func (c *Config) RemoveShare(guest, path, home string) error {
	return c.edit(home, func(lines []string) ([]string, error) {
		out, removed := removeBlocks(lines, func(h string) bool {
			return h == "guests."+guest+".share"
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

var linkKey = regexp.MustCompile(`^\s*link\s*=`)

// SetLinks replaces commands.link of guest.
func (c *Config) SetLinks(guest string, links []string, home string) error {
	value := "link = " + tomlArray(links)
	return c.edit(home, func(lines []string) ([]string, error) {
		start, end := findBlock(lines, "guests."+guest+".commands")
		if start < 0 {
			return append(lines, "", "[guests."+guest+".commands]", value), nil
		}
		for i := start + 1; i < end; i++ {
			if !linkKey.MatchString(lines[i]) {
				continue
			}
			j := i
			for j < end && !strings.Contains(lines[j], "]") {
				j++
			}
			return append(append(append([]string{}, lines[:i]...), value), lines[j+1:]...), nil
		}
		return append(append(append([]string{}, lines[:start+1]...), value), lines[start+1:]...), nil
	})
}

func (c *Config) edit(home string, change func([]string) ([]string, error)) error {
	real, err := filepath.EvalSymlinks(c.path)
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
	out := []byte(strings.Join(lines, "\n") + "\n")
	tmp, err := os.CreateTemp(filepath.Dir(real), ".hanare.toml.*")
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
	if _, err := Load(tmp.Name(), home); err != nil {
		return fmt.Errorf("refusing to write %s, the result would not load: %w", c.path, err)
	}
	if fi, err := os.Stat(real); err == nil {
		os.Chmod(tmp.Name(), fi.Mode().Perm())
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

// findBlock returns the header line of table name and the end of its body.
func findBlock(lines []string, name string) (int, int) {
	for i, l := range lines {
		if h, ok := header(l); ok && h == name {
			end := i + 1
			for end < len(lines) {
				if _, ok := header(lines[end]); ok {
					break
				}
				end++
			}
			return i, end
		}
	}
	return -1, -1
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
