package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"elephant/internal/model"
)

// This file implements a comment-preserving JSON member splice used for
// OpenCode configuration. OpenCode files may be JSONC: comments and unrelated
// settings must survive byte for byte, so Elephant never decodes and
// re-encodes the whole document. Instead it locates the member spans for the
// mcp.servers.elephant path and replaces or inserts only those bytes.

// jsonSpan is a half-open byte range into a JSONC document.
type jsonSpan struct{ start, end int }

// jsonMember is one object member: the decoded key plus raw key/value spans.
type jsonMember struct {
	key     string
	keySpan jsonSpan
	valSpan jsonSpan
}

// stripJSONComments removes // and /* */ comments outside strings so a span
// can be decoded with encoding/json.
func stripJSONComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '"' {
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '"' {
					j++
					break
				}
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// stripJSONTrailingCommas removes commas that JSONC allows before a closing
// bracket or brace, so a comment-free span can be decoded with encoding/json.
func stripJSONTrailingCommas(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			end := scanJSONString(s, i)
			if end < 0 {
				end = len(s)
			}
			out.WriteString(s[i:end])
			i = end - 1
			continue
		case ',':
			next := skipJSONTrivia(s, i+1)
			if next < len(s) && (s[next] == '}' || s[next] == ']') {
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// skipJSONTrivia advances past whitespace and comments.
func skipJSONTrivia(s string, i int) int {
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		return i
	}
	return i
}

// scanJSONString returns the end offset (exclusive) of the string starting at
// s[start] == '"', or -1 when unterminated.
func scanJSONString(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		case '\n':
			return -1
		}
	}
	return -1
}

// scanJSONValue returns the end offset (exclusive) of the JSON value starting
// at start, tolerating comments inside composite values.
func scanJSONValue(s string, start int) (int, error) {
	i := skipJSONTrivia(s, start)
	if i >= len(s) {
		return 0, fmt.Errorf("%w: unexpected end of configuration", model.ErrInvalidInput)
	}
	switch s[i] {
	case '"':
		end := scanJSONString(s, i)
		if end < 0 {
			return 0, fmt.Errorf("%w: unterminated string in configuration", model.ErrInvalidInput)
		}
		return end, nil
	case '{', '[':
		open, close := s[i], byte('}')
		if s[i] == '[' {
			close = ']'
		}
		depth := 0
		for ; i < len(s); i++ {
			c := s[i]
			if c == '"' {
				end := scanJSONString(s, i)
				if end < 0 {
					return 0, fmt.Errorf("%w: unterminated string in configuration", model.ErrInvalidInput)
				}
				i = end - 1
				continue
			}
			if c == '/' && i+1 < len(s) && (s[i+1] == '/' || s[i+1] == '*') {
				i = skipJSONTrivia(s, i) - 1
				continue
			}
			if c == open {
				depth++
			} else if c == close {
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, fmt.Errorf("%w: unbalanced brackets in configuration", model.ErrInvalidInput)
	default:
		j := i
		for j < len(s) && !strings.ContainsRune(",}] \t\n\r", rune(s[j])) {
			if s[j] == '/' && j+1 < len(s) && (s[j+1] == '/' || s[j+1] == '*') {
				break
			}
			j++
		}
		if j == i {
			return 0, fmt.Errorf("%w: invalid value in configuration", model.ErrInvalidInput)
		}
		return j, nil
	}
}

// jsonObjectMembers lists the members of the object spanning obj, in order.
func jsonObjectMembers(s string, obj jsonSpan) ([]jsonMember, error) {
	out := []jsonMember{}
	i := skipJSONTrivia(s, obj.start+1)
	if i < obj.end && s[i] == '}' {
		return out, nil
	}
	for {
		i = skipJSONTrivia(s, i)
		if i >= len(s) {
			return nil, fmt.Errorf("%w: unexpected end of configuration", model.ErrInvalidInput)
		}
		if s[i] == '}' {
			return out, nil
		}
		if s[i] != '"' {
			return nil, fmt.Errorf("%w: expected a member name in configuration", model.ErrInvalidInput)
		}
		keyEnd := scanJSONString(s, i)
		if keyEnd < 0 {
			return nil, fmt.Errorf("%w: unterminated member name in configuration", model.ErrInvalidInput)
		}
		var key string
		if err := json.Unmarshal([]byte(s[i:keyEnd]), &key); err != nil {
			return nil, fmt.Errorf("%w: invalid member name in configuration", model.ErrInvalidInput)
		}
		j := skipJSONTrivia(s, keyEnd)
		if j >= len(s) || s[j] != ':' {
			return nil, fmt.Errorf("%w: expected ':' in configuration", model.ErrInvalidInput)
		}
		valEnd, err := scanJSONValue(s, j+1)
		if err != nil {
			return nil, err
		}
		valStart := skipJSONTrivia(s, j+1)
		out = append(out, jsonMember{key, jsonSpan{i, keyEnd}, jsonSpan{valStart, valEnd}})
		i = skipJSONTrivia(s, valEnd)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == '}' {
			return out, nil
		}
		return nil, fmt.Errorf("%w: expected ',' or '}' in configuration", model.ErrInvalidInput)
	}
}

// jsonDocObject returns the span of the top-level object, rejecting blank or
// non-object documents.
func jsonDocObject(s string) (jsonSpan, error) {
	i := skipJSONTrivia(s, 0)
	if i >= len(s) {
		return jsonSpan{}, fmt.Errorf("%w: configuration is empty", model.ErrInvalidInput)
	}
	if s[i] != '{' {
		return jsonSpan{}, fmt.Errorf("%w: configuration root must be an object", model.ErrInvalidInput)
	}
	end, err := scanJSONValue(s, i)
	if err != nil {
		return jsonSpan{}, err
	}
	if rest := skipJSONTrivia(s, end); rest != len(s) {
		return jsonSpan{}, fmt.Errorf("%w: unexpected trailing content in configuration", model.ErrInvalidInput)
	}
	return jsonSpan{i, end}, nil
}

// detectIndentUnit infers the file's indentation unit, defaulting to two
// spaces when nothing is detectable.
func detectIndentUnit(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || trimmed == line {
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		if strings.Trim(indent, " ") == "" && indent != "" {
			return indent
		}
		return indent
	}
	return "  "
}

// baseIndent returns the indentation of the line containing offset.
func baseIndent(s string, offset int) string {
	lineStart := strings.LastIndex(s[:offset], "\n") + 1
	i := lineStart
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[lineStart:i]
}

// setJSONObjectMember replaces the rendered value of key inside obj, or
// inserts it before the closing brace when absent. Everything else is
// preserved byte for byte.
func setJSONObjectMember(s string, obj jsonSpan, key, rendered string) (string, error) {
	members, err := jsonObjectMembers(s, obj)
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.key == key {
			return s[:m.valSpan.start] + rendered + s[m.valSpan.end:], nil
		}
	}
	unit := detectIndentUnit(s)
	if len(members) == 0 {
		inner := baseIndent(s, obj.end) + unit
		return s[:obj.start+1] + "\n" + inner + jsonMemberName(key) + ": " + rendered + "\n" + baseIndent(s, obj.end) + "}" + s[obj.end:], nil
	}
	last := members[len(members)-1]
	inner := baseIndent(s, last.keySpan.start)
	return s[:last.valSpan.end] + ",\n" + inner + jsonMemberName(key) + ": " + rendered + s[last.valSpan.end:], nil
}

// jsonMemberName renders a JSON string literal for a member name.
func jsonMemberName(key string) string {
	encoded, err := json.Marshal(key)
	if err != nil {
		return `"` + key + `"`
	}
	return string(encoded)
}

// walkJSONPath resolves the object span at segments, reporting whether every
// segment exists. A present non-object intermediate is an explicit error.
func walkJSONPath(content string, segments []string) (jsonSpan, bool, error) {
	obj, err := jsonDocObject(content)
	if err != nil {
		return jsonSpan{}, false, err
	}
	for _, key := range segments {
		members, err := jsonObjectMembers(content, obj)
		if err != nil {
			return jsonSpan{}, false, err
		}
		matched := false
		for _, m := range members {
			if m.key != key {
				continue
			}
			matched = true
			trimmed := strings.TrimSpace(stripJSONComments(content[m.valSpan.start:m.valSpan.end]))
			if !strings.HasPrefix(trimmed, "{") {
				return jsonSpan{}, false, fmt.Errorf("%w: existing %q is not an object; configure it manually", model.ErrInvalidInput, key)
			}
			obj = jsonSpan{m.valSpan.start, m.valSpan.end}
			break
		}
		if !matched {
			return jsonSpan{}, false, nil
		}
	}
	return obj, true, nil
}

// removeJSONMember deletes key from obj, returning the updated document and
// whether the member existed. Comma cleanup prefers the following comma so
// preceding comments survive; removing the last member backtracks over
// whitespace to its preceding comma.
func removeJSONMember(content string, obj jsonSpan, key string) (string, bool, error) {
	members, err := jsonObjectMembers(content, obj)
	if err != nil {
		return "", false, err
	}
	index := -1
	for i, m := range members {
		if m.key == key {
			index = i
			break
		}
	}
	if index < 0 {
		return content, false, nil
	}
	target := members[index]
	// Swallow the member's own leading indentation (spaces/tabs on its line
	// only) so no orphaned whitespace survives the removal.
	lead := target.keySpan.start
	for lead > 0 && (content[lead-1] == ' ' || content[lead-1] == '\t') {
		lead--
	}
	// Prefer the following comma: it cannot belong to another member.
	j := skipJSONTrivia(content, target.valSpan.end)
	if j < len(content) && content[j] == ',' {
		end := j + 1
		if end < len(content) && content[end] == '\n' {
			end++
		} else if end+1 < len(content) && content[end] == '\r' && content[end+1] == '\n' {
			end += 2
		}
		return content[:lead] + content[end:], true, nil
	}
	if index == 0 {
		// Only member: leave an empty object, keeping the braces.
		return content[:obj.start+1] + content[obj.end-1:], true, nil
	}
	// Last member: backtrack over whitespace to the preceding comma, then
	// swallow one following newline to avoid a trailing blank line.
	k := target.keySpan.start
	for k > 0 && (content[k-1] == ' ' || content[k-1] == '\t' || content[k-1] == '\n' || content[k-1] == '\r') {
		k--
	}
	if k <= 0 || content[k-1] != ',' {
		return "", false, fmt.Errorf("%w: cannot remove configuration entry", model.ErrInvalidInput)
	}
	rest := content[target.valSpan.end:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	}
	return content[:k-1] + rest, true, nil
}

// setJSONPath sets the leaf member at path inside nested objects, creating
// missing intermediate objects. A present non-object blocks the path with an
// explicit error so Elephant never rewrites configuration it cannot parse.
func setJSONPath(content string, path []string, renderedLeaf string) (string, error) {
	if strings.TrimSpace(content) == "" {
		doc := map[string]any{}
		current := doc
		for _, key := range path[:len(path)-1] {
			next := map[string]any{}
			current[key] = next
			current = next
		}
		var leaf any
		if err := json.Unmarshal([]byte(renderedLeaf), &leaf); err != nil {
			return "", fmt.Errorf("%w: encode configuration entry: %v", model.ErrInvalidInput, err)
		}
		current[path[len(path)-1]] = leaf
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", fmt.Errorf("%w: encode configuration: %v", model.ErrInvalidInput, err)
		}
		return string(encoded) + "\n", nil
	}
	if _, err := jsonDocObject(content); err != nil {
		return "", err
	}
	for depth := 0; depth < len(path)-1; depth++ {
		_, found, err := walkJSONPath(content, path[:depth+1])
		if err != nil {
			return "", err
		}
		if found {
			continue
		}
		parent, _, err := walkJSONPath(content, path[:depth])
		if err != nil {
			return "", err
		}
		content, err = setJSONObjectMember(content, parent, path[depth], "{}")
		if err != nil {
			return "", err
		}
	}
	parent, found, err := walkJSONPath(content, path[:len(path)-1])
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: cannot create configuration path", model.ErrInvalidInput)
	}
	return setJSONObjectMember(content, parent, path[len(path)-1], renderedLeaf)
}

// readJSONPath decodes the value at path, reporting whether every segment
// exists. Non-object intermediates stop the search without an error.
func readJSONPath(content string, path []string) (any, bool, error) {
	if strings.TrimSpace(content) == "" {
		return nil, false, nil
	}
	obj, err := jsonDocObject(content)
	if err != nil {
		return nil, false, err
	}
	span := jsonSpan{obj.start, obj.end}
	raw := content
	for i, key := range path {
		members, err := jsonObjectMembers(raw, span)
		if err != nil {
			return nil, false, err
		}
		found := false
		for _, m := range members {
			if m.key != key {
				continue
			}
			found = true
			rawVal := stripJSONTrailingCommas(stripJSONComments(raw[m.valSpan.start:m.valSpan.end]))
			if i == len(path)-1 {
				var value any
				decoder := json.NewDecoder(strings.NewReader(rawVal))
				decoder.UseNumber()
				if err := decoder.Decode(&value); err != nil {
					return nil, false, fmt.Errorf("%w: existing %q is not valid JSON: %v", model.ErrInvalidInput, key, err)
				}
				return value, true, nil
			}
			var nested any
			if err := json.Unmarshal([]byte(rawVal), &nested); err != nil {
				return nil, false, nil
			}
			if _, ok := nested.(map[string]any); !ok {
				return nil, false, nil
			}
			span = jsonSpan{m.valSpan.start, m.valSpan.end}
			break
		}
		if !found {
			return nil, false, nil
		}
	}
	return nil, false, nil
}
