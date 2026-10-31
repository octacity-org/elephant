// Package agent registers Elephant's MCP server with coding agents and reads
// their configuration back. Every edit is limited to Elephant's own entry so
// unrelated agent settings survive untouched.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"elephant/internal/model"
)

// ServerName is Elephant's stable MCP server name in every agent configuration.
const ServerName = "elephant"

// Format names the configuration file syntax of a supported agent, and for
// JSON agents the layout of their server entry.
type Format string

const (
	TOML Format = "toml"
	JSON Format = "json"
	// ClaudeJSON is Claude Code's ~/.claude.json with an mcpServers object.
	ClaudeJSON Format = "claude-json"
)

// Env reads environment variables. A nil Env uses the process environment.
type Env func(string) string

func (e Env) get(key string) string {
	if e == nil {
		return os.Getenv(key)
	}
	return e(key)
}

// Target describes one supported agent integration. HookPath locates the
// file that holds session hooks; it is nil for agents without them.
type Target struct {
	Name     string
	Format   Format
	Path     func(Env) (string, error)
	HookPath func(Env) (string, error)
}

var targets = []Target{
	{Name: "codex", Format: TOML, Path: codexPath, HookPath: codexPath},
	{Name: "opencode", Format: JSON, Path: opencodePath},
	{Name: "claude-code", Format: ClaudeJSON, Path: claudePath, HookPath: claudeSettingsPath},
}

// Names lists supported agents in display order.
func Names() []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	return names
}

// Lookup resolves a supported agent by name.
func Lookup(name string) (Target, error) {
	for _, t := range targets {
		if t.Name == name {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("%w: unsupported agent %q; supported agents: %s", model.ErrInvalidInput, name, strings.Join(Names(), ", "))
}

func homeDir(env Env) (string, error) {
	if h := env.get("HOME"); h != "" {
		return h, nil
	}
	if h := env.get("USERPROFILE"); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("%w: HOME is not set, so the agent configuration location is unknown", model.ErrInvalidInput)
}

// codexPath resolves Codex's config.toml, honoring CODEX_HOME.
func codexPath(env Env) (string, error) {
	if root := env.get("CODEX_HOME"); root != "" {
		return filepath.Join(root, "config.toml"), nil
	}
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// opencodePath resolves OpenCode's global opencode.json, honoring
// OPENCODE_CONFIG and the XDG base directory specification.
func opencodePath(env Env) (string, error) {
	if path := env.get("OPENCODE_CONFIG"); path != "" {
		return path, nil
	}
	root := env.get("XDG_CONFIG_HOME")
	if root == "" {
		home, err := homeDir(env)
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "opencode", "opencode.json"), nil
}

// claudePath resolves ~/.claude.json, where Claude Code keeps user-scope MCP
// servers. Its documentation does not say that CLAUDE_CONFIG_DIR moves this
// file, so it is not consulted here; --config selects another file.
func claudePath(env Env) (string, error) {
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// claudeSettingsPath resolves Claude Code's user settings.json, which holds
// hooks, honoring CLAUDE_CONFIG_DIR.
func claudeSettingsPath(env Env) (string, error) {
	if root := env.get("CLAUDE_CONFIG_DIR"); root != "" {
		return filepath.Join(root, "settings.json"), nil
	}
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}
