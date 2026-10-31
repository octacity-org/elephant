package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"elephant/internal/model"
)

// Elephant's session-start hook runs `elephant recall --brief` when a session
// starts fresh, is cleared, or is compacted. Resumed and forked sessions are
// skipped because they already carry the earlier context. Setup manages only
// the hook command; a matcher or timeout the user changed is left alone.
const (
	hookMatcher = "startup|clear|compact"
	hookTimeout = 10
	hookSuffix  = " recall --brief"
)

// HookState reports Elephant's session-start hook in an agent configuration.
type HookState struct {
	Agent     string `json:"agent"`
	Path      string `json:"path,omitempty"`
	Installed bool   `json:"installed"`
	Command   string `json:"command,omitempty"`
}

// HookNames lists the agents that support Elephant's session-start hook.
func HookNames() []string {
	var names []string
	for _, t := range targets {
		if t.HookPath != nil {
			names = append(names, t.Name)
		}
	}
	return names
}

// HookCommand returns the shell command of Elephant's session-start hook,
// resolving the database path as Configure does so doctor can compare it.
func HookCommand(executable, database string) string {
	return hookCommand(Command(executable, database))
}

// hookCommand turns an MCP launch command into the hook command by replacing
// the trailing "serve" with "recall --brief".
func hookCommand(launch []string) string {
	words := make([]string, 0, len(launch)+1)
	for _, word := range launch[:len(launch)-1] {
		words = append(words, shellQuote(word))
	}
	return strings.Join(words, " ") + hookSuffix
}

// isElephantHook reports whether a hook command is Elephant's own.
func isElephantHook(command string) bool {
	return strings.HasSuffix(command, hookSuffix)
}

// shellQuote quotes a word for POSIX shells, leaving plain words unquoted.
func shellQuote(word string) string {
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// InspectHook reports whether Elephant's session-start hook is installed for
// an agent. A missing file is reported as not installed.
func InspectHook(name string, env Env) (HookState, error) {
	target, err := Lookup(name)
	if err != nil {
		return HookState{Agent: strings.TrimSpace(name)}, err
	}
	state := HookState{Agent: target.Name}
	if target.HookPath == nil {
		return state, fmt.Errorf("%w: %s has no session hooks", model.ErrInvalidInput, target.Name)
	}
	if state.Path, err = target.HookPath(env); err != nil {
		return state, err
	}
	content, _, err := readFile(state.Path)
	if err != nil {
		return state, err
	}
	state.Command, state.Installed, err = readHook(target, content)
	return state, err
}

func readHook(target Target, content string) (string, bool, error) {
	if target.Format == TOML {
		entry, _, err := codexHook(content)
		if err != nil || entry == nil {
			return "", false, err
		}
		command, _ := tomlScalar(entry.value)
		return command, true, nil
	}
	_, command, found, err := claudeHook(content)
	return command, found, err
}

func editHook(target Target, content, command string) (string, error) {
	if target.Format == TOML {
		return editCodexHook(content, command)
	}
	return editClaudeHook(content, command)
}

// Claude Code keeps hooks in settings.json under hooks.SessionStart, an array
// of matcher groups. Elephant owns one group, found by its handler's command.
var claudeSessionStartPath = []string{"hooks", "SessionStart"}

type claudeHookGroup struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeHookHandler `json:"hooks"`
}

type claudeHookHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func renderClaudeHookGroup(command string) (string, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	group := claudeHookGroup{Matcher: hookMatcher, Hooks: []claudeHookHandler{{Type: "command", Command: command, Timeout: hookTimeout}}}
	if err := encoder.Encode(group); err != nil {
		return "", fmt.Errorf("%w: encode hook: %v", model.ErrInvalidInput, err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

// claudeHook finds Elephant's group in hooks.SessionStart. It returns the
// group's span, or the array's own span with found=false when no group is
// Elephant's.
func claudeHook(content string) (span jsonSpan, command string, found bool, err error) {
	array, present, err := claudeSessionStart(content)
	if err != nil || !present {
		return jsonSpan{}, "", false, err
	}
	elements, err := jsonArrayElements(content, array)
	if err != nil {
		return jsonSpan{}, "", false, err
	}
	for _, element := range elements {
		value, err := decodeJSONSpan(content, element)
		if err != nil {
			return jsonSpan{}, "", false, err
		}
		group, _ := value.(map[string]any)
		handlers, _ := group["hooks"].([]any)
		for _, handler := range handlers {
			fields, _ := handler.(map[string]any)
			if command, _ := fields["command"].(string); isElephantHook(command) {
				return element, command, true, nil
			}
		}
	}
	return array, "", false, nil
}

// claudeSessionStart returns the span of the hooks.SessionStart array.
func claudeSessionStart(content string) (jsonSpan, bool, error) {
	if strings.TrimSpace(content) == "" {
		return jsonSpan{}, false, nil
	}
	hooks, found, err := walkJSONPath(content, claudeSessionStartPath[:1])
	if err != nil || !found {
		return jsonSpan{}, false, err
	}
	members, err := jsonObjectMembers(content, hooks)
	if err != nil {
		return jsonSpan{}, false, err
	}
	for _, m := range members {
		if m.key != claudeSessionStartPath[1] {
			continue
		}
		if content[m.valSpan.start] != '[' {
			return jsonSpan{}, false, fmt.Errorf("%w: existing \"hooks.SessionStart\" is not an array; configure it manually", model.ErrInvalidInput)
		}
		return m.valSpan, true, nil
	}
	return jsonSpan{}, false, nil
}

// editClaudeHook adds Elephant's group after the user's groups, or replaces
// it when its content differs. Other groups stay byte for byte.
func editClaudeHook(content, command string) (string, error) {
	group, err := renderClaudeHookGroup(command)
	if err != nil {
		return "", err
	}
	span, _, found, err := claudeHook(content)
	if err != nil {
		return "", err
	}
	if found {
		current, err := decodeJSONSpan(content, span)
		if err != nil {
			return "", err
		}
		var wanted any
		if err := json.Unmarshal([]byte(group), &wanted); err != nil {
			return "", fmt.Errorf("%w: encode hook: %v", model.ErrInvalidInput, err)
		}
		a, _ := json.Marshal(current)
		b, _ := json.Marshal(wanted)
		if bytes.Equal(a, b) {
			return content, nil
		}
		return content[:span.start] + group + content[span.end:], nil
	}
	if span == (jsonSpan{}) {
		return setJSONPath(content, claudeSessionStartPath, "["+group+"]")
	}
	elements, err := jsonArrayElements(content, span)
	if err != nil {
		return "", err
	}
	if len(elements) == 0 {
		return content[:span.start+1] + group + content[span.end-1:], nil
	}
	last := elements[len(elements)-1]
	return content[:last.end] + ", " + group + content[last.end:], nil
}

// jsonArrayElements lists the element spans of the array spanning arr.
func jsonArrayElements(s string, arr jsonSpan) ([]jsonSpan, error) {
	out := []jsonSpan{}
	i := arr.start + 1
	for {
		i = skipJSONTrivia(s, i)
		if i >= arr.end {
			return nil, fmt.Errorf("%w: unexpected end of configuration", model.ErrInvalidInput)
		}
		if s[i] == ']' {
			return out, nil
		}
		end, err := scanJSONValue(s, i)
		if err != nil {
			return nil, err
		}
		out = append(out, jsonSpan{i, end})
		i = skipJSONTrivia(s, end)
		if i < arr.end && s[i] == ',' {
			i++
			continue
		}
		if i < arr.end && s[i] == ']' {
			return out, nil
		}
		return nil, fmt.Errorf("%w: expected ',' or ']' in configuration", model.ErrInvalidInput)
	}
}

func decodeJSONSpan(content string, span jsonSpan) (any, error) {
	var value any
	raw := stripJSONTrailingCommas(stripJSONComments(content[span.start:span.end]))
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("%w: configuration is not valid JSON: %v", model.ErrInvalidInput, err)
	}
	return value, nil
}

// Codex reads hooks from config.toml as [[hooks.SessionStart]] matcher groups
// with [[hooks.SessionStart.hooks]] handlers. It records trust per group and
// handler index, so Elephant appends its group after the user's groups and
// never inserts one before them.
var codexHookHandlers = []string{"hooks", "SessionStart", "hooks"}

// codexHook finds the command entry of Elephant's handler. Hooks written as
// inline tables or dotted keys are an error, because setup cannot add an
// array-of-tables group next to them.
func codexHook(content string) (*tomlEntry, []string, error) {
	lines := strings.Split(content, "\n")
	for _, table := range scanTOML(lines) {
		if table.array {
			continue
		}
		for _, entry := range table.entries {
			full := append(slices.Clone(table.path), entry.key...)
			if slices.Equal(full, []string{"hooks"}) || len(full) >= 2 && slices.Equal(full[:2], []string{"hooks", "SessionStart"}) {
				return nil, nil, fmt.Errorf("%w: Codex configuration defines SessionStart hooks inline; write them as [[hooks.SessionStart]] tables and rerun setup", model.ErrInvalidInput)
			}
		}
	}
	for _, table := range scanTOML(lines) {
		if !table.array || !slices.Equal(table.path, codexHookHandlers) {
			continue
		}
		for _, entry := range table.entries {
			if len(entry.key) != 1 || entry.key[0] != "command" {
				continue
			}
			if command, ok := tomlScalar(entry.value); ok && isElephantHook(command) {
				return &entry, lines, nil
			}
		}
	}
	return nil, lines, nil
}

// editCodexHook rewrites the command of Elephant's handler when it differs,
// or appends Elephant's group at the end of the file.
func editCodexHook(content, command string) (string, error) {
	entry, lines, err := codexHook(content)
	if err != nil {
		return "", err
	}
	value := tomlString(command)
	if entry != nil {
		if tomlSameValue(entry.value, value) {
			return content, nil
		}
		return strings.Join(replaceTOMLEntry(lines, *entry, value), "\n"), nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines,
		"[[hooks.SessionStart]]",
		"matcher = "+tomlString(hookMatcher),
		"",
		"[[hooks.SessionStart.hooks]]",
		`type = "command"`,
		"command = "+value,
		"timeout = "+strconv.Itoa(hookTimeout),
		"")
	return strings.Join(lines, "\n"), nil
}
