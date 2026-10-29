package agent_test

import (
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/agent"
)

// JSONC allows trailing commas anywhere on the path to Elephant's entry, so
// an entry written that way is still recognised and its actor kept.
func TestOpenCodeEntryWithTrailingCommasIsConfigured(t *testing.T) {
	for name, body := range map[string]string{
		"current": `{
  "mcp": {
    "servers": {
      "elephant": {
        "type": "local",
        "command": ["/opt/elephant", "serve",],
        "environment": {"ELEPHANT_ACTOR_ID": "kept-actor",},
      },
    },
  },
}
`,
		"legacy": `{
  "mcp": {
    "elephant": {
      "type": "local",
      "command": ["/opt/elephant", "serve",],
      "environment": {"ELEPHANT_ACTOR_ID": "kept-actor",},
    },
  },
}
`,
	} {
		root := t.TempDir()
		path := filepath.Join(root, "opencode", "opencode.json")
		write(t, path, body)
		state, err := agent.Inspect("opencode", env(map[string]string{"XDG_CONFIG_HOME": root}))
		if err != nil || !state.Configured || state.ActorID != "kept-actor" || strings.Join(state.Command, " ") != "/opt/elephant serve" {
			t.Fatalf("%s: %+v %v", name, state, err)
		}
		result, err := agent.Configure(agent.Input{Agent: "opencode", Executable: "/opt/elephant", ConfigPath: path}, env(nil))
		if err != nil || result.ActorID != "kept-actor" {
			t.Fatalf("%s: setup did not keep the actor: %+v %v", name, result, err)
		}
	}
}
