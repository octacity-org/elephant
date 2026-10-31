package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"uuid"

	"elephant/internal/model"
)

// Input describes one setup request.
type Input struct {
	Agent      string
	Executable string
	Database   string
	ActorID    string
	ConfigPath string
	// Hooks also installs Elephant's session-start hook.
	Hooks bool
}

// Result reports what setup changed for one agent.
type Result struct {
	Agent      string   `json:"agent"`
	ConfigPath string   `json:"config_path,omitempty"`
	Command    []string `json:"mcp_command,omitempty"`
	ActorID    string   `json:"actor_id,omitempty"`
	Created    bool     `json:"created"`
	Changed    bool     `json:"changed"`
	Configured bool     `json:"configured"`
	Manual     string   `json:"manual,omitempty"`
	// HookPath and HookCommand are set when hooks were requested.
	HookPath    string   `json:"hook_path,omitempty"`
	HookCommand string   `json:"hook_command,omitempty"`
	HookChanged bool     `json:"hook_changed"`
	Notes       []string `json:"notes,omitempty"`
}

// State reports the Elephant entry currently present in an agent configuration.
type State struct {
	Agent      string   `json:"agent"`
	ConfigPath string   `json:"config_path,omitempty"`
	Exists     bool     `json:"exists"`
	Configured bool     `json:"configured"`
	Command    []string `json:"mcp_command,omitempty"`
	ActorID    string   `json:"actor_id,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

func launchCommand(executable, database string) []string {
	command := []string{executable}
	if database != "" {
		command = append(command, "--db", database)
	}
	return append(command, "serve")
}

// Command returns the MCP stdio launch command Elephant writes into agent
// configuration and reports from doctor. It resolves the database path as
// Configure does, so doctor can compare it with a registration.
func Command(executable, database string) []string {
	database = strings.TrimSpace(database)
	if database != "" {
		if resolved, err := resolvePath(database, "database path"); err == nil {
			database = resolved
		}
	}
	return launchCommand(strings.TrimSpace(executable), database)
}

func resolveExecutable(exe string) (string, error) {
	if strings.TrimSpace(exe) == "" {
		return "", fmt.Errorf("%w: Elephant executable path is required", model.ErrInvalidInput)
	}
	return resolvePath(exe, "Elephant executable")
}

// resolvePath absolutizes relative paths while preserving already-absolute
// paths verbatim. Unix-absolute paths are kept as-is even on Windows, where
// filepath would otherwise rewrite them against the current drive and corrupt
// the configured launch command.
func resolvePath(value, what string) (string, error) {
	value = strings.TrimSpace(value)
	if isAbsolutePath(value) {
		return value, nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %s: %v", model.ErrInvalidInput, what, err)
	}
	return abs, nil
}

// isAbsolutePath reports absolute paths in the host convention as well as the
// Unix convention, so configuration round-trips byte-identically on Windows.
func isAbsolutePath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return strings.HasPrefix(p, "/")
}

func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" || len(actor) > 300 || strings.ContainsRune(actor, 0) {
		return fmt.Errorf("%w: actor ID must be 1–300 bytes without NUL", model.ErrInvalidInput)
	}
	return nil
}

func readFile(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), true, nil
	}
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("%w: read %s: %v", model.ErrInvalidInput, path, err)
}

func writeFile(path, content string, existed bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("%w: create configuration directory: %v", model.ErrInvalidInput, err)
	}
	mode := os.FileMode(0600)
	if existed {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	// Write to a temporary file in the same directory, sync it, then rename
	// atomically so a crash or write failure never leaves a truncated
	// configuration behind. The deferred remove only fires when the rename
	// did not happen.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".elephant-config-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: stage configuration write: %v", model.ErrInvalidInput, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: write %s: %v", model.ErrInvalidInput, path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: sync %s: %v", model.ErrInvalidInput, path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", model.ErrInvalidInput, path, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("%w: protect %s: %v", model.ErrInvalidInput, path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("%w: replace %s: %v", model.ErrInvalidInput, path, err)
	}
	return nil
}

// Configure registers Elephant's MCP server with the selected agent. It is
// idempotent: repeating it with the same command and actor leaves the file
// byte-identical. Unsupported agents receive a manual example instead of an
// error so the caller can still guide the user.
func Configure(in Input, env Env) (Result, error) {
	name := strings.TrimSpace(in.Agent)
	out := Result{Agent: name, ActorID: strings.TrimSpace(in.ActorID)}
	if name == "" {
		return out, fmt.Errorf("%w: agent name is required", model.ErrInvalidInput)
	}
	if in.ActorID != "" && out.ActorID == "" {
		return out, fmt.Errorf("%w: actor ID must be 1–300 bytes without NUL", model.ErrInvalidInput)
	}
	executable, err := resolveExecutable(in.Executable)
	if err != nil {
		return out, err
	}
	database := strings.TrimSpace(in.Database)
	if database != "" {
		if database, err = resolvePath(database, "database path"); err != nil {
			return out, err
		}
	}
	target, err := Lookup(name)
	if in.Hooks && (err != nil || target.HookPath == nil) {
		return out, fmt.Errorf("%w: session hooks are supported for %s", model.ErrInvalidInput, strings.Join(HookNames(), " and "))
	}
	if err != nil {
		manualActor := out.ActorID
		if manualActor == "" {
			manualActor = "<actor-id>"
		}
		out.Manual = Manual(name, executable, database, manualActor)
		out.ActorID = ""
		return out, nil
	}
	path := strings.TrimSpace(in.ConfigPath)
	if path == "" {
		if path, err = target.Path(env); err != nil {
			return out, err
		}
	}
	if path, err = resolvePath(path, "configuration path"); err != nil {
		return out, err
	}
	content, existed, err := readFile(path)
	if err != nil {
		return out, err
	}
	state, err := readState(target, path, content, existed)
	if err != nil {
		return out, err
	}
	if out.ActorID == "" {
		out.ActorID = state.ActorID
	}
	if out.ActorID == "" {
		out.ActorID = name + "-" + uuid.NewV7().String()
	}
	if err = validateActor(out.ActorID); err != nil {
		return out, err
	}
	command := launchCommand(executable, database)
	updated, err := edit(target, content, command, out.ActorID)
	if err != nil {
		return out, err
	}
	out.ConfigPath, out.Command, out.Created, out.Configured = path, command, !existed, true
	out.Changed = updated != content
	if !in.Hooks {
		if out.Changed {
			return out, writeFile(path, updated, existed)
		}
		return out, nil
	}

	// Both edits are prepared before anything is written. Codex reads hooks
	// from the same config.toml, so a --config path applies to both and the
	// hook edit builds on the MCP edit.
	out.HookCommand = hookCommand(command)
	hookContent, hookUpdated, hookExisted := updated, "", existed
	if target.Format == TOML {
		out.HookPath = path
		out.Notes = append(out.Notes, "Codex runs a new or changed hook only after you review and trust it in /hooks (Codex 0.129.0 or later).")
	} else {
		if out.HookPath, err = target.HookPath(env); err != nil {
			return out, err
		}
		if out.HookPath, err = resolvePath(out.HookPath, "hook configuration path"); err != nil {
			return out, err
		}
		if hookContent, hookExisted, err = readFile(out.HookPath); err != nil {
			return out, err
		}
	}
	if hookUpdated, err = editHook(target, hookContent, out.HookCommand); err != nil {
		return out, err
	}
	out.HookChanged = hookUpdated != hookContent
	if target.Format == TOML {
		if hookUpdated != content {
			return out, writeFile(path, hookUpdated, existed)
		}
		return out, nil
	}
	if out.Changed {
		if err = writeFile(path, updated, existed); err != nil {
			return out, err
		}
	}
	if out.HookChanged {
		return out, writeFile(out.HookPath, hookUpdated, hookExisted)
	}
	return out, nil
}

// Inspect reports whether Elephant is registered with an agent. A missing
// configuration file is reported through State.Exists instead of an error.
func Inspect(name string, env Env) (State, error) {
	target, err := Lookup(name)
	if err != nil {
		return State{Agent: strings.TrimSpace(name)}, err
	}
	path, err := target.Path(env)
	if err != nil {
		return State{Agent: target.Name}, err
	}
	content, existed, err := readFile(path)
	if err != nil {
		return State{Agent: target.Name, ConfigPath: path}, err
	}
	return readState(target, path, content, existed)
}
