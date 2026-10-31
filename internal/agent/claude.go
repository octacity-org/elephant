package agent

import (
	"encoding/json"
	"fmt"
	"slices"

	"elephant/internal/model"
)

// claudeServerPath is Elephant's user-scope server entry in ~/.claude.json.
// Claude Code owns the rest of that file, so setup sets only the keys below
// and leaves extra keys and variables on the entry alone.
var claudeServerPath = []string{"mcpServers", ServerName}

// readClaudeState reports Elephant's entry in ~/.claude.json.
func readClaudeState(target Target, path, content string, existed bool) (State, error) {
	state := State{Agent: target.Name, ConfigPath: path, Exists: existed}
	value, found, err := readJSONPath(content, claudeServerPath)
	if err != nil {
		return state, fmt.Errorf("%w: %s is not valid JSON: %v", model.ErrInvalidInput, path, err)
	}
	if !found {
		return state, nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return state, fmt.Errorf("%w: existing %q is not a server entry; configure it manually", model.ErrInvalidInput, "mcpServers."+ServerName)
	}
	state.Configured = true
	state.Detail = "mcpServers." + ServerName
	if command, ok := entry["command"].(string); ok && command != "" {
		state.Command = append(state.Command, command)
	}
	if args, ok := entry["args"].([]any); ok {
		for _, arg := range args {
			if text, ok := arg.(string); ok {
				state.Command = append(state.Command, text)
			}
		}
	}
	if environment, ok := entry["env"].(map[string]any); ok {
		state.ActorID, _ = environment["ELEPHANT_ACTOR_ID"].(string)
	}
	return state, nil
}

// editClaude sets the stdio type, command, args, and actor on Elephant's
// entry. A key whose value already matches is not rewritten, so a file Claude
// Code formatted itself stays byte-identical when nothing changed.
func editClaude(content string, command []string, actor string) (string, error) {
	for _, field := range []struct {
		path  []string
		value any
	}{
		{slices.Concat(claudeServerPath, []string{"type"}), "stdio"},
		{slices.Concat(claudeServerPath, []string{"command"}), command[0]},
		{slices.Concat(claudeServerPath, []string{"args"}), command[1:]},
		{slices.Concat(claudeServerPath, []string{"env", "ELEPHANT_ACTOR_ID"}), actor},
	} {
		rendered, err := json.Marshal(field.value)
		if err != nil {
			return "", fmt.Errorf("%w: encode configuration entry: %v", model.ErrInvalidInput, err)
		}
		current, found, err := readJSONPath(content, field.path)
		if err != nil {
			return "", err
		}
		if found {
			if existing, err := json.Marshal(current); err == nil && string(existing) == string(rendered) {
				continue
			}
		}
		if content, err = setJSONPath(content, field.path, string(rendered)); err != nil {
			return "", err
		}
	}
	return content, nil
}
