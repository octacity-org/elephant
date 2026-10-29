package agent_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/agent"
	"elephant/internal/model"
)

func TestConfigureRejectsUnparsableAndUnsupportedInput(t *testing.T) {
	root := t.TempDir()
	jsonPath := filepath.Join(root, "opencode", "opencode.json")
	write(t, jsonPath, "{ not json")
	_, err := agent.Configure(agent.Input{Agent: "opencode", Executable: "/opt/elephant", ConfigPath: jsonPath}, env(nil))
	if !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("error=%v", err)
	}
	if read(t, jsonPath) != "{ not json" {
		t.Fatal("malformed configuration was rewritten")
	}
	objectPath := filepath.Join(root, "object", "opencode.json")
	write(t, objectPath, `{"mcp": "text"}`)
	if _, err = agent.Configure(agent.Input{Agent: "opencode", Executable: "/opt/elephant", ConfigPath: objectPath}, env(nil)); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("error=%v", err)
	}
	if read(t, objectPath) != `{"mcp": "text"}` {
		t.Fatal("unexpected mcp value was rewritten")
	}
	if _, err = agent.Configure(agent.Input{Agent: "codex", ConfigPath: filepath.Join(root, "config.toml")}, env(nil)); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("missing executable error=%v", err)
	}
	if _, err = agent.Configure(agent.Input{Agent: "codex", Executable: "/opt/elephant", ActorID: " ", ConfigPath: filepath.Join(root, "config.toml")}, env(nil)); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("blank actor error=%v", err)
	}
}

func TestUnsupportedAgentReturnsManualExample(t *testing.T) {
	result, err := agent.Configure(agent.Input{Agent: "zed", Executable: "/opt/elephant", Database: "/home/dev/elephant.zova"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Configured || result.ConfigPath != "" || result.ActorID != "" {
		t.Fatalf("%+v", result)
	}
	for _, want := range []string{"zed", "[mcp_servers.elephant]", "/opt/elephant --db /home/dev/elephant.zova serve", "<actor-id>"} {
		if !strings.Contains(result.Manual, want) {
			t.Fatalf("missing %q in:\n%s", want, result.Manual)
		}
	}
}

func TestInspectMissingConfiguration(t *testing.T) {
	state, err := agent.Inspect("codex", env(map[string]string{"CODEX_HOME": t.TempDir()}))
	if err != nil || state.Exists || state.Configured {
		t.Fatalf("%+v %v", state, err)
	}
	if _, err = agent.Inspect("unknown", env(nil)); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigureQuotesReservedCharacters(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	executable := `/opt/elephant "quoted"` + `\bin`
	in := agent.Input{Agent: "codex", Executable: executable, ActorID: "actor", ConfigPath: path}
	if _, err := agent.Configure(in, env(nil)); err != nil {
		t.Fatal(err)
	}
	content := read(t, path)
	if !strings.Contains(content, `\\bin`) || !strings.Contains(content, `\"quoted\"`) {
		t.Fatalf("reserved characters were not escaped:\n%s", content)
	}
	state, err := agent.Inspect("codex", env(map[string]string{"CODEX_HOME": root}))
	if err != nil || len(state.Command) == 0 {
		t.Fatalf("%+v %v", state, err)
	}
	if !strings.HasSuffix(state.Command[0], `\bin`) || !strings.Contains(state.Command[0], `"quoted"`) {
		t.Fatalf("command did not round-trip: %v", state.Command)
	}
}

func TestConfigureReadsCommentsAndTableSpacing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	write(t, path, "[ mcp_servers.elephant ] # keep this comment\ncommand = '/old/elephant'\nargs = ['serve']\n\n[other]\nvalue = \"kept\"\n")
	in := agent.Input{Agent: "codex", Executable: "/new/elephant", ActorID: "actor", ConfigPath: path}
	result, err := agent.Configure(in, env(nil))
	if err != nil || !result.Changed {
		t.Fatalf("%+v %v", result, err)
	}
	content := read(t, path)
	if strings.Contains(content, "/old/elephant") || !strings.Contains(content, "[ mcp_servers.elephant ] # keep this comment\n") {
		t.Fatalf("stale command was not replaced in place:\n%s", content)
	}
	if !strings.Contains(content, "[other]\nvalue = \"kept\"") || !strings.Contains(content, "command = \"/new/elephant\"") {
		t.Fatalf("unrelated content was lost:\n%s", content)
	}
}
