package agent

import (
	"fmt"
	"slices"
	"strings"

	"elephant/internal/model"
)

// Codex keeps Elephant's registration in these two tables. Setup edits only
// the keys it owns inside them and leaves every other line untouched.
var codexServerTable = []string{"mcp_servers", ServerName}
var codexEnvTable = []string{"mcp_servers", ServerName, "env"}

// tomlEntry is one key/value pair. key is relative to its table, value is the
// comment-free value text, and lines [start, end) hold the whole pair.
type tomlEntry struct {
	key        []string
	value      string
	start, end int
}

// tomlTable is a table header and the pairs under it. The root table has no
// header and starts at line -1.
type tomlTable struct {
	path    []string
	array   bool
	start   int
	entries []tomlEntry
}

// scanTOML splits lines into tables and pairs. It understands dotted and
// quoted keys and values spanning several lines; it does not validate TOML.
func scanTOML(lines []string) []tomlTable {
	tables := []tomlTable{{start: -1}}
	for i := 0; i < len(lines); i++ {
		text := strings.TrimSpace(stripTOMLComment(lines[i]))
		if text == "" {
			continue
		}
		if text[0] == '[' {
			if table, ok := tomlHeader(text, i); ok {
				tables = append(tables, table)
			}
			continue
		}
		equals := tomlEquals(text)
		if equals < 0 {
			continue
		}
		key, ok := tomlKey(text[:equals])
		if !ok {
			continue
		}
		entry := tomlEntry{key: key, value: strings.TrimSpace(text[equals+1:]), start: i, end: i + 1}
		if delim := tomlOpenMultiline(entry.value); delim != "" {
			for entry.end < len(lines) {
				closed := strings.Contains(lines[entry.end], delim)
				entry.end++
				if closed {
					break
				}
			}
		} else {
			for depth := tomlDepth(entry.value); depth > 0 && entry.end < len(lines); entry.end++ {
				next := strings.TrimSpace(stripTOMLComment(lines[entry.end]))
				entry.value += "\n" + next
				depth += tomlDepth(next)
			}
		}
		i = entry.end - 1
		last := &tables[len(tables)-1]
		last.entries = append(last.entries, entry)
	}
	return tables
}

// tomlHeader parses a [table] or [[array]] header.
func tomlHeader(text string, line int) (tomlTable, bool) {
	array := strings.HasPrefix(text, "[[")
	open, close := "[", "]"
	if array {
		open, close = "[[", "]]"
	}
	if !strings.HasSuffix(text, close) || len(text) < len(open)+len(close)+1 {
		return tomlTable{}, false
	}
	path, ok := tomlKey(text[len(open) : len(text)-len(close)])
	return tomlTable{path: path, array: array, start: line}, ok
}

// tomlKey decodes a dotted key into its segments. Quoted segments keep their
// dots, so "elephant.dev" is one segment.
func tomlKey(text string) ([]string, bool) {
	var path []string
	rest := strings.TrimSpace(text)
	for {
		if rest == "" {
			return nil, false
		}
		var segment string
		switch rest[0] {
		case '"', '\'':
			end := tomlStringEnd(rest)
			if end < 0 {
				return nil, false
			}
			value, ok := tomlScalar(rest[:end])
			if !ok {
				return nil, false
			}
			segment, rest = value, rest[end:]
		default:
			end := strings.IndexFunc(rest, func(r rune) bool {
				return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
			})
			if end < 0 {
				end = len(rest)
			}
			if end == 0 {
				return nil, false
			}
			segment, rest = rest[:end], rest[end:]
		}
		path = append(path, segment)
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return path, true
		}
		if rest[0] != '.' {
			return nil, false
		}
		rest = strings.TrimSpace(rest[1:])
	}
}

// tomlStringEnd returns the index just past the string that starts text.
func tomlStringEnd(text string) int {
	quote := text[0]
	for i := 1; i < len(text); i++ {
		switch {
		case quote == '"' && text[i] == '\\':
			i++
		case text[i] == quote:
			return i + 1
		}
	}
	return -1
}

// tomlEquals returns the index of the first = outside a quoted key.
func tomlEquals(text string) int {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '"', '\'':
			end := tomlStringEnd(text[i:])
			if end < 0 {
				return -1
			}
			i += end - 1
		case '=':
			return i
		}
	}
	return -1
}

// tomlDepth returns how many brackets and braces text leaves open.
func tomlDepth(text string) int {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '"', '\'':
			end := tomlStringEnd(text[i:])
			if end < 0 {
				return depth
			}
			i += end - 1
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth
}

// tomlOpenMultiline returns the delimiter of a multi-line string value that
// does not close on its first line.
func tomlOpenMultiline(value string) string {
	for _, delim := range []string{`"""`, `'''`} {
		if strings.HasPrefix(value, delim) && !strings.Contains(value[len(delim):], delim) {
			return delim
		}
	}
	return ""
}

// codexRegistration reads Elephant's tables. An entry that registers
// Elephant inline (an inline table, a dotted key, or env = {...}) is an
// error, because setup cannot edit it without writing a duplicate.
func codexRegistration(content string) (server, env *tomlTable, err error) {
	tables := scanTOML(strings.Split(content, "\n"))
	for i := range tables {
		table := &tables[i]
		if table.array {
			continue
		}
		for _, entry := range table.entries {
			full := append(slices.Clone(table.path), entry.key...)
			ours := len(full) >= len(codexServerTable) && slices.Equal(full[:len(codexServerTable)], codexServerTable)
			above := len(table.path) < len(codexServerTable)
			envInline := slices.Equal(table.path, codexServerTable) && entry.key[0] == "env"
			if ours && (above || envInline) {
				return nil, nil, fmt.Errorf("%w: Codex configuration registers %s inline; move it into a [mcp_servers.%s] table and rerun setup", model.ErrInvalidInput, ServerName, ServerName)
			}
		}
		switch {
		case server == nil && slices.Equal(table.path, codexServerTable):
			server = table
		case env == nil && slices.Equal(table.path, codexEnvTable):
			env = table
		}
	}
	return server, env, nil
}

// tomlLookup returns the raw value of a single-segment key.
func tomlLookup(table *tomlTable, key string) (string, bool) {
	if table == nil {
		return "", false
	}
	for _, entry := range table.entries {
		if len(entry.key) == 1 && entry.key[0] == key {
			return entry.value, true
		}
	}
	return "", false
}

// setCodexServer writes Elephant's command and actor into its tables.
func setCodexServer(content string, command []string, actor string) string {
	args := make([]string, 0, len(command))
	for _, value := range command[1:] {
		args = append(args, tomlString(value))
	}
	lines := strings.Split(content, "\n")
	lines = setTOMLKey(lines, codexServerTable, "command", tomlString(command[0]))
	lines = setTOMLKey(lines, codexServerTable, "args", "["+strings.Join(args, ", ")+"]")
	lines = setTOMLKey(lines, codexEnvTable, "ELEPHANT_ACTOR_ID", tomlString(actor))
	return strings.Join(lines, "\n")
}

// setTOMLKey sets key in the table at path. A value that already decodes to
// the same strings is left as written; a changed value keeps its line's
// indentation and comment; a missing key goes after the table's last pair;
// a missing table is appended to the file.
func setTOMLKey(lines []string, path []string, key, value string) []string {
	line := key + " = " + value
	var table *tomlTable
	tables := scanTOML(lines)
	for i := range tables {
		if !tables[i].array && slices.Equal(tables[i].path, path) {
			table = &tables[i]
			break
		}
	}
	if table == nil {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		header := make([]string, len(path))
		for i, segment := range path {
			if quoted, ok := tomlKey(segment); !ok || len(quoted) != 1 || quoted[0] != segment {
				segment = tomlString(segment)
			}
			header[i] = segment
		}
		return append(lines, "["+strings.Join(header, ".")+"]", line, "")
	}
	at := table.start + 1
	for _, entry := range table.entries {
		at = entry.end
		if len(entry.key) != 1 || entry.key[0] != key {
			continue
		}
		if tomlSameValue(entry.value, value) {
			return lines
		}
		first, last := lines[entry.start], lines[entry.end-1]
		replaced := first[:strings.IndexByte(first, '=')+1] + " " + value
		if comment := last[len(stripTOMLComment(last)):]; comment != "" {
			replaced += " " + comment
		}
		return slices.Replace(lines, entry.start, entry.end, replaced)
	}
	return slices.Insert(lines, at, line)
}

// tomlSameValue reports whether two values decode to the same strings.
func tomlSameValue(current, next string) bool {
	if a, ok := tomlScalar(current); ok {
		b, ok := tomlScalar(next)
		return ok && a == b
	}
	// tomlArray skips non-string items, so also require that none were skipped.
	a, b := tomlArray(current), tomlArray(next)
	return a != nil && slices.Equal(a, b) && len(splitTOMLArray(current[1:len(current)-1])) == len(a)
}

// tomlArray decodes a bounded array of string values.
func tomlArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		return nil
	}
	var out []string
	for _, part := range splitTOMLArray(raw[1 : len(raw)-1]) {
		if value, ok := tomlScalar(strings.TrimSpace(part)); ok {
			out = append(out, value)
		}
	}
	return out
}
func splitTOMLArray(inner string) []string {
	var parts []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	for _, r := range inner {
		switch {
		case escaped:
			escaped = false
			current.WriteRune(r)
		case quote == '"' && r == '\\':
			escaped = true
			current.WriteRune(r)
		case quote != 0:
			current.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			current.WriteRune(r)
		case r == ',':
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		parts = append(parts, current.String())
	}
	return parts
}
