package config

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Line-level editing of a guest's TOML file for the Draft edits: they keep
// the user's comments and formatting, and the Draft loads the result before
// anything is written.

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
