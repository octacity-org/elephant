package mcp

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
	"elephant/internal/storage/zova"
)

// assertToolSet requires every named tool to be advertised exactly once. It
// replaces brittle exact-count assertions so stacked features compose without
// rewriting this test on every branch.
func assertToolSet(t *testing.T, tools *sdk.ListToolsResult, required []string) {
	t.Helper()
	seen := map[string]int{}
	for _, tool := range tools.Tools {
		seen[tool.Name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("tool %q advertised %d times", name, count)
		}
	}
	for _, name := range required {
		if seen[name] != 1 {
			t.Fatalf("required tool %q missing (have %v)", name, tools.Tools)
		}
	}
}

func TestMCPToolsValidationAndContinuity(t *testing.T) {
	// The budget covers process startup, database open, and several
	// sequential tool calls; Windows CI runners are significantly slower,
	// so allow a full minute instead of failing on general slowness.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()
	cmd := exec.Command("git", "init", "-q", cwd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "mcp.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := New(app.New(db), cwd)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertToolSet(t, tools, []string{
		"recall_project", "project_status", "inspect_entry",
		"add_fact", "update_fact", "list_facts", "retire_fact",
		"add_decision", "update_decision", "list_decisions", "supersede_decision",
		"add_task", "update_task", "list_tasks", "complete_task", "cancel_task",
		"list_remotes", "ensure_remote_project",
		"remote_send_fact", "remote_send_decision", "remote_send_task", "remote_recall",
		"remote_inbox", "remote_diff", "adopt_entry",
		"add_evidence", "list_evidence", "verify_evidence", "refresh_evidence", "remove_evidence",
		"add_checkpoint", "list_checkpoints",
		"export_project", "import_project",
		"search_entries", "related_entries", "list_tags", "file_history",
		"review_memory",
	})
	rr, re := session.CallTool(ctx, &sdk.CallToolParams{Name: "list_remotes", Arguments: map[string]any{}})
	if re != nil || rr.IsError {
		t.Fatal(rr, re)
	}
	for _, args := range []map[string]any{{"title": "missing body"}, {"title": "title", "body": " "}, {"title": "title", "body": "body", "related_files": []string{"../escape"}}} {
		r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "add_fact", Arguments: args})
		if err == nil && !r.IsError {
			t.Fatal("accepted invalid input", args)
		}
	}
	r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "add_task", Arguments: map[string]any{"title": "Task", "body": "Useful work"}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	data, _ := json.Marshal(r.StructuredContent)
	var record app.Record
	if err = json.Unmarshal(data, &record); err != nil || record.Entry.ID == "" {
		t.Fatalf("%s %v", data, err)
	}
	r, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "complete_task", Arguments: map[string]any{"id": record.Entry.ID}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	r, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "recall_project", Arguments: map[string]any{}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	data, _ = json.Marshal(r.StructuredContent)
	var packet app.RecallResult
	if err = json.Unmarshal(data, &packet); err != nil || len(packet.RecentCompleted) != 1 {
		t.Fatalf("%s %v", data, err)
	}
	r, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "add_fact", Arguments: map[string]any{"title": "Tagged", "body": "fact", "tags": []string{"Build"}}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	for _, call := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"list_tags", map[string]any{}, `{"entries":1,"tag":"build"}`},
		{"search_entries", map[string]any{"tags": []string{"build"}}, `"tags":["build"]`},
		{"add_fact", map[string]any{"title": "Global", "body": "fact", "tags": []string{"pinned"}, "global": true}, `"tags":["pinned"]`},
		{"recall_project", map[string]any{}, `"global":[{"entry":`},
		{"search_entries", map[string]any{"global": true}, `"title":"Global"`},
	} {
		r, err = session.CallTool(ctx, &sdk.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || r.IsError {
			t.Fatal(call.name, r, err)
		}
		if data, _ = json.Marshal(r.StructuredContent); !strings.Contains(string(data), call.want) {
			t.Fatalf("%s: %s", call.name, data)
		}
	}
}

// TestMCPSearchCommitRangeOrdering: commit range validation reaches the MCP
// boundary — reversed and divergent endpoints return invalid-input errors
// naming the problem, while forward, equal, and single-commit ranges succeed.
func TestMCPSearchCommitRangeOrdering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	commit := func(msg string) string {
		out, err := exec.Command("git", "-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com",
			"commit", "--allow-empty", "-qm", msg).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v", out, err)
		}
		out, err = exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	c1 := commit("c1")
	c2 := commit("c2")
	// Divergent commit: branch from c1 so neither c2 nor side contains the other.
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	branch := strings.TrimSpace(string(out))
	if out, err := exec.Command("git", "-C", cwd, "checkout", "-q", "-b", "side", c1).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	side := commit("side")
	if out, err := exec.Command("git", "-C", cwd, "checkout", "-q", branch).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "mcporder.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := New(app.New(db), cwd)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(args map[string]any) (*sdk.CallToolResult, error) {
		return session.CallTool(ctx, &sdk.CallToolParams{Name: "search_entries", Arguments: args})
	}
	// Forward, equal, and single-commit ranges succeed.
	for _, args := range []map[string]any{
		{"commit_start": c1, "commit_end": c2},
		{"commit_start": c2, "commit_end": c2},
		{"commit": c2},
	} {
		r, err := call(args)
		if err != nil || r.IsError {
			t.Fatalf("%v: %v %v", args, r, err)
		}
	}
	failure := func(r *sdk.CallToolResult, err error) string {
		if err != nil {
			return err.Error()
		}
		raw, _ := json.Marshal(r)
		return string(raw)
	}
	// Reversed endpoints are invalid input naming the problem.
	r, err := call(map[string]any{"commit_start": c2, "commit_end": c1})
	if err == nil && !r.IsError {
		t.Fatal("reversed range accepted")
	}
	if msg := failure(r, err); !strings.Contains(msg, "reversed") {
		t.Fatalf("unclear reversed error: %s", msg)
	}
	// Divergent endpoints are invalid input naming the problem.
	r, err = call(map[string]any{"commit_start": c2, "commit_end": side})
	if err == nil && !r.IsError {
		t.Fatal("divergent range accepted")
	}
	if msg := failure(r, err); !strings.Contains(msg, "divergent") {
		t.Fatalf("unclear divergent error: %s", msg)
	}
}
