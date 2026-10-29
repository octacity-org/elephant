package agent_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/agent"
	"elephant/internal/model"
)

// Setup updates only the keys Elephant owns in its Codex tables. User keys
// and comments inside those tables survive, a server whose quoted name merely
// starts with "elephant." is someone else's, and an inline registration is
// reported instead of duplicated.
func TestConfigureCodexEditsOnlyElephantKeys(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	original := `[mcp_servers.elephant]
# keep this comment
command = "/old/elephant"
args = [
  "--db",
  "/old.zova",
  "serve",
]
startup_timeout_sec = 30 # user timeout

[mcp_servers.elephant.env]
ELEPHANT_ACTOR_ID = "codex-old"
OTHER = "kept"

# comment before the next table
[mcp_servers."elephant.dev"]
command = "dev-server"
`
	write(t, path, original)
	in := agent.Input{Agent: "codex", Executable: "/opt/elephant", Database: "/data/elephant.zova", ActorID: "codex-new", ConfigPath: path}
	if _, err := agent.Configure(in, nil); err != nil {
		t.Fatal(err)
	}
	want := `[mcp_servers.elephant]
# keep this comment
command = "/opt/elephant"
args = ["--db", "/data/elephant.zova", "serve"]
startup_timeout_sec = 30 # user timeout

[mcp_servers.elephant.env]
ELEPHANT_ACTOR_ID = "codex-new"
OTHER = "kept"

# comment before the next table
[mcp_servers."elephant.dev"]
command = "dev-server"
`
	if got := read(t, path); got != want {
		t.Fatalf("setup rewrote more than Elephant's keys:\n%s", got)
	}
	state, err := agent.Inspect("codex", env(map[string]string{"CODEX_HOME": root}))
	if err != nil || state.ActorID != "codex-new" || strings.Join(state.Command, " ") != "/opt/elephant --db /data/elephant.zova serve" {
		t.Fatalf("%+v %v", state, err)
	}

	for _, inline := range []string{
		"[mcp_servers]\nelephant = { command = \"/opt/elephant\", args = [\"serve\"] }\n",
		"mcp_servers.elephant.command = \"/opt/elephant\"\n",
		"[mcp_servers.elephant]\ncommand = \"/opt/elephant\"\nenv = { ELEPHANT_ACTOR_ID = \"codex-x\" }\n",
	} {
		write(t, path, inline)
		if _, err := agent.Configure(in, nil); !errors.Is(err, model.ErrInvalidInput) || !strings.Contains(err.Error(), "inline") {
			t.Fatalf("inline registration %q: %v", inline, err)
		}
		if got := read(t, path); got != inline {
			t.Fatalf("setup changed an inline registration:\n%s", got)
		}
	}
}
