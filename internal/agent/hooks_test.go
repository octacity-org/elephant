package agent_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/agent"
	"elephant/internal/model"
)

// Claude Code keeps MCP servers in ~/.claude.json next to its own state.
// Setup edits only Elephant's keys there: other servers, other top-level
// state, and extra keys or variables on Elephant's entry survive.
func TestConfigureClaudeCodeEditsOnlyElephantKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	original := `{
  "numStartups": 3,
  "mcpServers": {
    "other": {
      "type": "stdio",
      "command": "other-server"
    },
    "elephant": {
      "type": "stdio",
      "command": "/old/elephant",
      "args": [
        "serve"
      ],
      "env": {
        "OTHER": "kept"
      },
      "alwaysLoad": true
    }
  },
  "projects": {}
}
`
	write(t, path, original)
	e := env(map[string]string{"HOME": home})
	in := agent.Input{Agent: "claude-code", Executable: "/opt/elephant", Database: "/data/elephant.zova", ActorID: "claude"}
	first, err := agent.Configure(in, e)
	if err != nil || !first.Changed || first.ConfigPath != path {
		t.Fatalf("%+v %v", first, err)
	}
	want := `{
  "numStartups": 3,
  "mcpServers": {
    "other": {
      "type": "stdio",
      "command": "other-server"
    },
    "elephant": {
      "type": "stdio",
      "command": "/opt/elephant",
      "args": ["--db","/data/elephant.zova","serve"],
      "env": {
        "OTHER": "kept",
        "ELEPHANT_ACTOR_ID": "claude"
      },
      "alwaysLoad": true
    }
  },
  "projects": {}
}
`
	if got := read(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	second, err := agent.Configure(in, e)
	if err != nil || second.Changed {
		t.Fatalf("not idempotent: %+v %v", second, err)
	}
	state, err := agent.Inspect("claude-code", e)
	if err != nil || !state.Configured || state.ActorID != "claude" || strings.Join(state.Command, " ") != "/opt/elephant --db /data/elephant.zova serve" {
		t.Fatalf("%+v %v", state, err)
	}
	// Plain setup installs no hook.
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup without --hooks wrote settings: %v", err)
	}
	if hook, err := agent.InspectHook("claude-code", e); err != nil || hook.Installed {
		t.Fatalf("%+v %v", hook, err)
	}
}

// A registration Claude Code itself pretty-printed is already up to date when
// its values match, even though Elephant would render arrays differently.
func TestConfigureClaudeCodeMatchingEntryIsUnchanged(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	original := `{
  "mcpServers": {
    "elephant": {
      "type": "stdio",
      "command": "/opt/elephant",
      "args": [
        "serve"
      ],
      "env": {
        "ELEPHANT_ACTOR_ID": "claude"
      }
    }
  }
}
`
	write(t, path, original)
	result, err := agent.Configure(agent.Input{Agent: "claude-code", Executable: "/opt/elephant", ActorID: "claude"}, env(map[string]string{"HOME": home}))
	if err != nil || result.Changed || read(t, path) != original {
		t.Fatalf("%+v %v\n%s", result, err, read(t, path))
	}
}

func TestSetupHooksClaudeCode(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "claude-config")
	settings := filepath.Join(configDir, "settings.json")
	userGroup := `{
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "echo hello"
          }
        ]
      }`
	original := "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"SessionStart\": [\n      " + userGroup + "\n    ]\n  }\n}\n"
	write(t, settings, original)
	e := env(map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": configDir})
	in := agent.Input{Agent: "claude-code", Executable: "/opt/elephant", Database: "/data/elephant.zova", ActorID: "claude", Hooks: true}
	first, err := agent.Configure(in, e)
	if err != nil || !first.HookChanged || first.HookPath != settings || first.HookCommand != "/opt/elephant --db /data/elephant.zova recall --brief" {
		t.Fatalf("%+v %v", first, err)
	}
	ours := `{"matcher":"startup|clear|compact","hooks":[{"type":"command","command":"/opt/elephant --db /data/elephant.zova recall --brief","timeout":10}]}`
	want := "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"SessionStart\": [\n      " + userGroup + ", " + ours + "\n    ]\n  }\n}\n"
	if got := read(t, settings); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	second, err := agent.Configure(in, e)
	if err != nil || second.HookChanged || read(t, settings) != want {
		t.Fatalf("not idempotent: %+v %v", second, err)
	}

	// A moved executable updates Elephant's group in place.
	in.Executable = "/new/elephant"
	if _, err = agent.Configure(in, e); err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(want, "/opt/elephant --db", "/new/elephant --db", 1)
	if got := read(t, settings); got != moved {
		t.Fatalf("got:\n%s\nwant:\n%s", got, moved)
	}
	hook, err := agent.InspectHook("claude-code", e)
	if err != nil || !hook.Installed || hook.Path != settings || hook.Command != "/new/elephant --db /data/elephant.zova recall --brief" {
		t.Fatalf("%+v %v", hook, err)
	}
}

func TestSetupHooksClaudeCodeCreatesSettings(t *testing.T) {
	home := t.TempDir()
	e := env(map[string]string{"HOME": home})
	result, err := agent.Configure(agent.Input{Agent: "claude-code", Executable: "/opt/my tools/elephant", ActorID: "claude", Hooks: true}, e)
	if err != nil || !result.HookChanged {
		t.Fatalf("%+v %v", result, err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	want := `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "'/opt/my tools/elephant' recall --brief",
            "timeout": 10,
            "type": "command"
          }
        ],
        "matcher": "startup|clear|compact"
      }
    ]
  }
}
`
	if got := read(t, settings); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestSetupHooksCodex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	original := `model = "gpt-5"

[[hooks.SessionStart]]
matcher = "startup"

[[hooks.SessionStart.hooks]]
type = "command"
command = "echo hello"

[hooks.state."/x/config.toml:session_start:0:0"]
trusted_hash = "abc"
`
	write(t, path, original)
	e := env(map[string]string{"CODEX_HOME": root})
	in := agent.Input{Agent: "codex", Executable: "/opt/elephant", Database: "/data/elephant.zova", ActorID: "codex", Hooks: true}
	first, err := agent.Configure(in, e)
	if err != nil || !first.HookChanged || first.HookPath != path || len(first.Notes) != 1 || !strings.Contains(first.Notes[0], "/hooks") {
		t.Fatalf("%+v %v", first, err)
	}
	block := `
[[hooks.SessionStart]]
matcher = "startup|clear|compact"

[[hooks.SessionStart.hooks]]
type = "command"
command = "/opt/elephant --db /data/elephant.zova recall --brief"
timeout = 10
`
	content := read(t, path)
	if !strings.HasPrefix(content, original) || !strings.HasSuffix(content, block) || strings.Count(content, "recall --brief") != 1 {
		t.Fatalf("got:\n%s", content)
	}
	if second, err := agent.Configure(in, e); err != nil || second.HookChanged || second.Changed || read(t, path) != content {
		t.Fatalf("not idempotent: %+v %v", second, err)
	}
	in.Executable = "/new/elephant"
	if _, err = agent.Configure(in, e); err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(content, `command = "/opt/elephant --db`, `command = "/new/elephant --db`, 1)
	moved = strings.Replace(moved, `command = "/opt/elephant"`, `command = "/new/elephant"`, 1)
	if got := read(t, path); got != moved {
		t.Fatalf("got:\n%s\nwant:\n%s", got, moved)
	}
	hook, err := agent.InspectHook("codex", e)
	if err != nil || !hook.Installed || hook.Command != "/new/elephant --db /data/elephant.zova recall --brief" {
		t.Fatalf("%+v %v", hook, err)
	}
}

func TestSetupHooksRejectsInlineCodexHooksAndOpenCode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	original := "[hooks]\nSessionStart = [{ hooks = [{ type = \"command\", command = \"echo hi\" }] }]\n"
	write(t, path, original)
	_, err := agent.Configure(agent.Input{Agent: "codex", Executable: "/opt/elephant", ActorID: "codex", Hooks: true}, env(map[string]string{"CODEX_HOME": root}))
	if !errors.Is(err, model.ErrInvalidInput) || read(t, path) != original {
		t.Fatalf("inline hooks: %v\n%s", err, read(t, path))
	}

	config := filepath.Join(t.TempDir(), "opencode.json")
	_, err = agent.Configure(agent.Input{Agent: "opencode", Executable: "/opt/elephant", ActorID: "oc", ConfigPath: config, Hooks: true}, env(map[string]string{"HOME": t.TempDir()}))
	if !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("opencode hooks: %v", err)
	}
	if _, statErr := os.Stat(config); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("opencode configuration written: %v", statErr)
	}
}

// Elephant's group reformatted by another tool is still up to date when its
// values match, so setup leaves the file byte-identical.
func TestSetupHooksClaudeCodeReformattedGroupIsUnchanged(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	original := `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "/opt/elephant recall --brief",
            "timeout": 10,
            "type": "command"
          }
        ],
        "matcher": "startup|clear|compact"
      }
    ]
  }
}
`
	write(t, settings, original)
	result, err := agent.Configure(agent.Input{Agent: "claude-code", Executable: "/opt/elephant", ActorID: "claude", Hooks: true}, env(map[string]string{"HOME": home}))
	if err != nil || result.HookChanged || read(t, settings) != original {
		t.Fatalf("%+v %v\n%s", result, err, read(t, settings))
	}
}
