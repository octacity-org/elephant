package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

// fileURIFromPath converts a local directory to a correctly encoded file://
// root URI on every platform: forward slashes throughout, a leading slash
// before Windows drive letters (file:///D:/path/to/repo), and URL-escaped
// special characters. Raw string concatenation would emit backslashes on
// Windows, which are invalid in URIs and silently fall back to the server
// directory.
func fileURIFromPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	slash := filepath.ToSlash(abs)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	return (&url.URL{Scheme: "file", Path: slash}).String()
}

func TestFileURIToPath(t *testing.T) {
	for _, tc := range []struct {
		uri  string
		want string
	}{
		{"file:///repo/share", "/repo/share"},
		{"file:///tmp/with%20space/x.go", "/tmp/with space/x.go"},
		{"file:///C:/repo/share", "C:/repo/share"},
		{"file:///c:/repo/share", "c:/repo/share"},
		{"file://localhost/C:/repo", "C:/repo"},
		{"file://build-host/share/dir", "//build-host/share/dir"},
		{"file:C:/repo", "C:/repo"},
		{"file:///C|/repo", "C:/repo"},
		{"file:///", "/"},
	} {
		got, err := FileURIToPath(tc.uri)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q,%v want %q", tc.uri, got, err, tc.want)
		}
	}
	for _, uri := range []string{"", "https://example.com/x", "untitled:Untitled-1", "file://", "file://?q=1", "file:///%zz", "relative/path"} {
		if got, err := FileURIToPath(uri); err == nil {
			t.Errorf("%q: accepted as %q", uri, got)
		}
	}
}

func TestFileURIFromPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	uri := fileURIFromPath(t, dir)
	if !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("URI %q must carry an absolute path", uri)
	}
	if strings.ContainsAny(uri, "\\") {
		t.Fatalf("URI %q must not contain backslashes", uri)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FileURIToPath(uri)
	if err != nil {
		t.Fatalf("helper output %q rejected: %v", uri, err)
	}
	if got != filepath.ToSlash(abs) {
		t.Fatalf("round trip: got %q want %q", got, filepath.ToSlash(abs))
	}
	// A directory with spaces must survive URL encoding.
	spaced := filepath.Join(dir, "with space")
	if err := os.Mkdir(spaced, 0700); err != nil {
		t.Fatal(err)
	}
	got, err = FileURIToPath(fileURIFromPath(t, spaced))
	if err != nil {
		t.Fatal(err)
	}
	abs, err = filepath.Abs(spaced)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.ToSlash(abs) {
		t.Fatalf("encoded round trip: got %q want %q", got, filepath.ToSlash(abs))
	}
}

func TestRequestScopeSelection(t *testing.T) {
	// Without a roots-capable client the server directory always wins,
	// including when an (untrusted) response map is somehow present.
	if got, need := RequestScope(nil, "/explicit", nil, "/default"); got != "/explicit" || need {
		t.Fatalf("explicit lost: %q %v", got, need)
	}
	if got, need := RequestScope(nil, "", nil, "/default"); got != "/default" || need {
		t.Fatalf("nil session: %q need=%v", got, need)
	}
	// A wrong-kind input response resolves to the default rather than failing.
	bad := sdk.InputResponseMap{rootsInputRequestID: &sdk.ElicitResult{}}
	if got, need := RequestScope(nil, "", bad, "/default"); got != "/default" || need {
		t.Fatalf("malformed response: %q need=%v", got, need)
	}
	// First usable file root wins; invalid and non-file roots are skipped.
	res := &sdk.ListRootsResult{Roots: []*sdk.Root{{URI: "::bad"}, {URI: "https://example.com/x"}, {URI: "file:///second"}, {URI: "file:///third"}}}
	path, ok := FirstFileRoot(res.Roots)
	if !ok || path != "/second" {
		t.Fatalf("multiple roots: %q %v", path, ok)
	}
	if _, ok := FirstFileRoot([]*sdk.Root{nil}); ok {
		t.Fatal("nil root accepted")
	}
	if _, ok := FirstFileRoot(nil); ok {
		t.Fatal("no roots accepted")
	}
}

func TestResourceAndPromptResolveAdvertisedRoots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Two distinct repositories plus the server directory. A resource or
	// prompt read without an explicit cwd must resolve to the repository the
	// client currently advertises, and follow root changes.
	repoA, repoB, serverDir := t.TempDir(), t.TempDir(), t.TempDir()
	mkRepo := func(dir, name string) {
		t.Helper()
		for _, args := range [][]string{
			{"init", "-q", dir},
			{"-C", dir, "remote", "add", "origin", "https://github.com/test/" + name + ".git"},
			{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"},
		} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("%s %v", out, err)
			}
		}
	}
	mkRepo(repoA, "repoa")
	mkRepo(repoB, "repob")
	uriA := fileURIFromPath(t, repoA)
	uriB := fileURIFromPath(t, repoB)

	db, err := zova.Open(filepath.Join(t.TempDir(), "roots.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := app.New(db)
	server := New(svc, serverDir)

	st, ct := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "roots-test", Version: "1"}, &sdk.ClientOptions{
		Capabilities: &sdk.ClientCapabilities{RootsV2: &sdk.RootCapabilities{ListChanged: true}},
	})
	client.AddRoots(&sdk.Root{URI: uriA, Name: "a"}, &sdk.Root{URI: "https://example.com/remote", Name: "remote"})
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// The actual resource handler runs across the protocol: the client
	// advertises its roots, fulfills the input request, and the retry reads
	// the repository the client currently advertises.
	recallProject := func(want string) {
		t.Helper()
		rr, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: resourceRecall})
		if err != nil {
			t.Fatalf("read resource: %v", err)
		}
		var packet app.RecallResult
		if err = json.Unmarshal([]byte(rr.Contents[0].Text), &packet); err != nil {
			t.Fatal(err)
		}
		if packet.Project.Identity != want {
			t.Fatalf("resource resolved %q, want %q", packet.Project.Identity, want)
		}
	}
	// The resume_project prompt resolves the same way without an explicit cwd.
	promptProject := func(want string) {
		t.Helper()
		pr, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "resume_project"})
		if err != nil {
			t.Fatalf("get prompt: %v", err)
		}
		if len(pr.Messages) == 0 {
			t.Fatal("empty prompt")
		}
		text := pr.Messages[0].Content.(*sdk.TextContent).Text
		if !strings.Contains(text, want) {
			t.Fatalf("prompt resolved wrong repository, want %q: %q", want, text)
		}
	}
	// Tools called without cwd resolve the same repository as resources.
	toolProject := func(args map[string]any, want string) {
		t.Helper()
		r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "recall_project", Arguments: args})
		if err != nil || r.IsError {
			t.Fatalf("call tool: %v %+v", err, r)
		}
		data, _ := json.Marshal(r.StructuredContent)
		var packet app.RecallResult
		if err = json.Unmarshal(data, &packet); err != nil {
			t.Fatal(err)
		}
		if packet.Project.Identity != want {
			t.Fatalf("tool resolved %q, want %q", packet.Project.Identity, want)
		}
	}
	recallProject("github.com/test/repoa")
	promptProject("github.com/test/repoa")
	toolProject(map[string]any{}, "github.com/test/repoa")

	// Changing the advertised roots selects a different repository on the
	// next read: a roots/listChanged notification followed by the fulfilled
	// roots request in the retry.
	client.AddRoots(&sdk.Root{URI: uriB, Name: "b"})
	client.RemoveRoots(uriA)
	recallProject("github.com/test/repob")
	promptProject("github.com/test/repob")
	toolProject(map[string]any{}, "github.com/test/repob")
	toolProject(map[string]any{"cwd": repoA}, "github.com/test/repoa")

	// An explicit cwd still wins over advertised roots.
	pr, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "resume_project", Arguments: map[string]string{"cwd": repoA}})
	if err != nil {
		t.Fatal(err)
	}
	if text := pr.Messages[0].Content.(*sdk.TextContent).Text; !strings.Contains(text, "github.com/test/repoa") {
		t.Fatalf("explicit cwd lost: %q", text)
	}
}

func TestResourceWithoutRootsFallsBackToServerDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	serverDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", serverDir},
		{"-C", serverDir, "remote", "add", "origin", "https://github.com/test/serverdir.git"},
		{"-C", serverDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "fallback.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := New(app.New(db), serverDir)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	// A client without the roots capability: no roots request may be issued,
	// and reads resolve to the server directory.
	client := sdk.NewClient(&sdk.Implementation{Name: "bare", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	rr, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: resourceRecall})
	if err != nil {
		t.Fatal(err)
	}
	var packet app.RecallResult
	if err = json.Unmarshal([]byte(rr.Contents[0].Text), &packet); err != nil {
		t.Fatal(err)
	}
	if packet.Project.Identity != "github.com/test/serverdir" {
		t.Fatalf("fallback project: %+v", packet.Project)
	}
}

func TestAdvertisedCapabilitiesMatchHandlers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "caps.zova"))
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
	client := sdk.NewClient(&sdk.Implementation{Name: "caps", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range res.Resources {
		seen[r.URI] = true
	}
	for _, uri := range []string{resourceRecall, resourceTasks, resourceDecisions} {
		if !seen[uri] {
			t.Fatalf("resource %q not advertised: %v", uri, seen)
		}
	}
	if len(res.Resources) != 3 {
		t.Fatalf("unexpected resources: %v", seen)
	}
	pr, err := session.ListPrompts(ctx, nil)
	if err != nil || len(pr.Prompts) != 1 {
		t.Fatalf("%+v %v", pr, err)
	}
	prompt := pr.Prompts[0]
	if prompt.Name != "resume_project" {
		t.Fatalf("prompt=%v", prompt.Name)
	}
	args := map[string]bool{}
	for _, a := range prompt.Arguments {
		args[a.Name] = true
	}
	if !args["cwd"] || !args["target_version"] {
		t.Fatalf("prompt args=%v", args)
	}
}

func TestTasksResourceIsGloballyNewestFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "newest.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := app.New(db)
	mk := func(title string) string {
		rec, err := svc.Add(ctx, cwd, model.Task, app.CreateInput{Title: title, Body: "work"})
		if err != nil {
			t.Fatal(err)
		}
		return rec.Entry.ID
	}
	oldOpen := mk("old open")
	mid := mk("middle")
	// Reopen recency: move the oldest entry to active so statuses interleave.
	active := "active"
	if _, err = svc.Update(ctx, cwd, model.Task, oldOpen, app.UpdateInput{Status: &active}); err != nil {
		t.Fatal(err)
	}
	newest := mk("newest open")
	server := New(svc, cwd)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "order", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	rr, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: resourceTasks})
	if err != nil {
		t.Fatal(err)
	}
	var entries []model.Entry
	if err = json.Unmarshal([]byte(rr.Contents[0].Text), &entries); err != nil {
		t.Fatal(err)
	}
	want := []string{newest, oldOpen, mid}
	if len(entries) != len(want) {
		t.Fatalf("entries=%v", entries)
	}
	for i, id := range want {
		if entries[i].ID != id {
			t.Fatalf("position %d: got %s want %s (%v)", i, entries[i].ID, id, entries)
		}
	}
}
