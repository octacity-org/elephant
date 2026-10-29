package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"elephant/internal/model"
)

// readState reads the Elephant entry from configuration content. A blank or
// absent file is a valid empty state; malformed content is an error so callers
// never overwrite configuration they cannot parse.
func readState(target Target, path, content string, existed bool) (State, error) {
	state := State{Agent: target.Name, ConfigPath: path, Exists: existed}
	if strings.TrimSpace(content) == "" {
		return state, nil
	}
	if target.Format == JSON {
		return readOpenCodeState(target, path, content, existed)
	}
	server, env, err := codexRegistration(content)
	if err != nil || server == nil {
		return state, err
	}
	state.Configured = true
	state.Detail = "mcp_servers." + ServerName
	if raw, ok := tomlLookup(server, "command"); ok {
		if command, ok := tomlScalar(raw); ok && command != "" {
			state.Command = append(state.Command, command)
		}
	}
	if raw, ok := tomlLookup(server, "args"); ok {
		state.Command = append(state.Command, tomlArray(raw)...)
	}
	if raw, ok := tomlLookup(env, "ELEPHANT_ACTOR_ID"); ok {
		state.ActorID, _ = tomlScalar(raw)
	}
	return state, nil
}

func edit(target Target, content string, command []string, actor string) (string, error) {
	if target.Format == JSON {
		return editOpenCode(content, command, actor)
	}
	return setCodexServer(content, command, actor), nil
}

// openCodePaths locates the current and legacy Elephant server entries.
// Current OpenCode keeps servers under mcp.servers; older Elephant setups
// wrote mcp.elephant directly. The legacy entry is read for migration only.
var openCodeCurrentPath = []string{"mcp", "servers", ServerName}
var openCodeLegacyPath = []string{"mcp", ServerName}

// openCodeServer is the parsed semantic content of one server entry.
type openCodeServer struct {
	command  []string
	actor    string
	disabled bool
}

// parseOpenCodeServer extracts the launch command, actor, and disablement
// from a decoded server entry, accepting both current and legacy shapes.
func parseOpenCodeServer(value any) (openCodeServer, bool) {
	entry, ok := value.(map[string]any)
	if !ok {
		return openCodeServer{}, false
	}
	var server openCodeServer
	if list, ok := entry["command"].([]any); ok {
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return openCodeServer{}, false
			}
			server.command = append(server.command, text)
		}
	}
	if environment, ok := entry["environment"].(map[string]any); ok {
		if actor, ok := environment["ELEPHANT_ACTOR_ID"].(string); ok {
			server.actor = actor
		}
	}
	// Current semantics use opt-in "disabled"; the legacy shape used
	// opt-out "enabled". A legacy enabled:false migrates to disabled:true.
	if disabled, ok := entry["disabled"].(bool); ok && disabled {
		server.disabled = true
	} else if enabled, ok := entry["enabled"].(bool); ok && !enabled {
		server.disabled = true
	}
	return server, true
}

// renderOpenCodeServer encodes the current-format server entry with stable
// key order so repeated setup produces byte-identical output.
func renderOpenCodeServer(command []string, actor string, disabled bool) (string, error) {
	entry := struct {
		Type        string            `json:"type"`
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
		Disabled    *bool             `json:"disabled,omitempty"`
	}{Type: "local", Command: append([]string{}, command...), Environment: map[string]string{"ELEPHANT_ACTOR_ID": actor}}
	if disabled {
		entry.Disabled = &disabled
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("%w: encode configuration entry: %v", model.ErrInvalidInput, err)
	}
	return string(encoded), nil
}

// readOpenCodeState reports the Elephant entry in an OpenCode document,
// preferring the current path and falling back to the legacy path.
func readOpenCodeState(target Target, path, content string, existed bool) (State, error) {
	state := State{Agent: target.Name, ConfigPath: path, Exists: existed}
	if strings.TrimSpace(content) == "" {
		return state, nil
	}
	current, found, err := readJSONPath(content, openCodeCurrentPath)
	if err != nil {
		return state, fmt.Errorf("%w: %s is not valid JSON: %v", model.ErrInvalidInput, path, err)
	}
	if found {
		server, ok := parseOpenCodeServer(current)
		if !ok {
			return state, fmt.Errorf("%w: existing %q is not a server entry; configure it manually", model.ErrInvalidInput, "mcp.servers."+ServerName)
		}
		state.Configured = true
		state.Detail = "mcp.servers." + ServerName
		state.Command = server.command
		state.ActorID = server.actor
		return state, nil
	}
	legacy, found, err := readJSONPath(content, openCodeLegacyPath)
	if err != nil {
		return state, fmt.Errorf("%w: %s is not valid JSON: %v", model.ErrInvalidInput, path, err)
	}
	if !found {
		return state, nil
	}
	server, ok := parseOpenCodeServer(legacy)
	if !ok {
		return state, nil
	}
	state.Configured = true
	state.Detail = "mcp." + ServerName + " (legacy)"
	state.Command = server.command
	state.ActorID = server.actor
	return state, nil
}

// editOpenCode writes the current-format server entry with a
// comment-preserving splice. A legacy entry seeds disablement and is then
// removed so exactly one Elephant server remains.
func editOpenCode(content string, command []string, actor string) (string, error) {
	disabled := false
	if current, found, err := readJSONPath(content, openCodeCurrentPath); err != nil {
		return "", err
	} else if found {
		server, ok := parseOpenCodeServer(current)
		if !ok {
			return "", fmt.Errorf("%w: existing %q is not a server entry; configure it manually", model.ErrInvalidInput, "mcp.servers."+ServerName)
		}
		disabled = server.disabled
	} else if legacy, found, err := readJSONPath(content, openCodeLegacyPath); err != nil {
		return "", err
	} else if found {
		if server, ok := parseOpenCodeServer(legacy); ok {
			disabled = server.disabled
		}
	}
	rendered, err := renderOpenCodeServer(command, actor, disabled)
	if err != nil {
		return "", err
	}
	if current, found, err := readJSONPath(content, openCodeCurrentPath); err != nil {
		return "", err
	} else if found {
		server, ok := parseOpenCodeServer(current)
		if !ok {
			return "", fmt.Errorf("%w: existing %q is not a server entry; configure it manually", model.ErrInvalidInput, "mcp.servers."+ServerName)
		}
		if equalStringSlices(server.command, command) && server.actor == actor && server.disabled == disabled {
			return content, nil
		}
	}
	updated, err := setJSONPath(content, openCodeCurrentPath, rendered)
	if err != nil {
		return "", err
	}
	// Migrate the legacy entry away only when it parses as a server entry;
	// anything else is left for the user.
	if legacy, found, err := readJSONPath(content, openCodeLegacyPath); err != nil {
		return "", err
	} else if found {
		if _, ok := parseOpenCodeServer(legacy); ok {
			mcpObj, mcpFound, err := walkJSONPath(updated, []string{"mcp"})
			if err != nil {
				return "", err
			}
			if mcpFound {
				updated, _, err = removeJSONMember(updated, mcpObj, ServerName)
				if err != nil {
					return "", err
				}
			}
		}
	}
	return updated, nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Manual returns a copy-ready configuration example for an agent Elephant does
// not configure automatically.
func Manual(name, executable, database, actor string) string {
	command := launchCommand(executable, database)
	quoted := make([]string, 0, len(command))
	for _, value := range command {
		quoted = append(quoted, tomlString(value))
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Elephant does not configure %s automatically. Start an MCP server named %q with:\n\n  %s\n\n", name, ServerName, strings.Join(command, " "))
	out.WriteString("Add that command to the agent's MCP configuration, for example:\n\n")
	fmt.Fprintf(&out, "[mcp_servers.%s]\ncommand = %s\nargs = [%s]\n\n", ServerName, tomlString(executable), strings.Join(quoted[1:], ", "))
	fmt.Fprintf(&out, "[mcp_servers.%s.env]\nELEPHANT_ACTOR_ID = %s\n", ServerName, tomlString(actor))
	return out.String()
}
