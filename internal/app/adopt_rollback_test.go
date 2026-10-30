package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
)

func adoptRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/shared.git"},
		{"-C", p, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	return p
}

// touchFiles creates repository files so adopt-side file links can resolve.
func touchFiles(t *testing.T, cwd string, names ...string) {
	t.Helper()
	for _, f := range names {
		if err := os.WriteFile(filepath.Join(cwd, f), []byte("package shared"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// sendAdoptable stores one remote fact (with optional files and relations)
// from the given sender in the fake's remote source table and returns its ID.
func sendAdoptable(t *testing.T, ctx context.Context, s *app.Service, sender, title, body string, files []string, relations []app.RelationInput) string {
	t.Helper()
	return sendAdoptableKind(t, ctx, s, model.Fact, sender, title, body, files, relations)
}

// sendAdoptableKind stores one remote entry of the given kind.
func sendAdoptableKind(t *testing.T, ctx context.Context, s *app.Service, kind model.Kind, sender, title, body string, files []string, relations []app.RelationInput) string {
	t.Helper()
	now := time.Now().UTC()
	m := app.Message{ProtocolVersion: 1, MessageID: uuid.NewV7().String(), SenderElephantID: sender,
		Project:   app.ProjectRef{Identity: "github.com/test/shared", Name: "shared"},
		Operation: "entry.send",
		Entry:     &model.Entry{ID: uuid.NewV7().String(), Kind: kind, Title: title, Body: body, Status: model.DefaultStatus(kind), ActorID: "peer", CreatedAt: now, UpdatedAt: now},
		Files:     files, Relations: relations}
	resp, err := s.Receive(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	return resp.EntryID
}

func TestAdoptLinkFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	cwd := adoptRepo(t)
	touchFiles(t, cwd, "a.go")
	store := newFakeStore()
	s := app.New(store)
	sender := uuid.NewV7().String()
	entryID := sendAdoptable(t, ctx, s, sender, "rolled back", "never lands", []string{"a.go"}, nil)

	store.failLinkPrefix = "file:"
	if _, err := s.Adopt(ctx, cwd, entryID, sender); err == nil {
		t.Fatal("adopt accepted a failing storage link")
	}
	store.failLinkPrefix = ""

	// The failed transaction must have left nothing behind: no adoption
	// receipt, so a repeat adopt creates a fresh local entry instead of
	// failing forever or reporting success without links.
	out, err := s.Adopt(ctx, cwd, entryID, sender)
	if err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if out.AlreadyAdopted {
		t.Fatalf("adoption receipt survived the rolled-back transaction: %+v", out)
	}
	if len(out.Relations) != 1 || !strings.HasPrefix(out.Relations[0].To, "file:") || !strings.HasSuffix(out.Relations[0].To, ":a.go") {
		t.Fatalf("file link not copied on retry: %+v", out)
	}

	again, err := s.Adopt(ctx, cwd, entryID, sender)
	if err != nil || !again.AlreadyAdopted || again.Entry.ID != out.Entry.ID {
		t.Fatalf("repeat adopt after recovery: %+v %v", again, err)
	}
}

// injectLink stores one raw file link in the sender's remote source table,
// bypassing receive-time protocol validation. This simulates a link that is
// legal on the wire but unsafe or stale in the local repository.
func injectLink(t *testing.T, ctx context.Context, store *fakeStore, sender, entryID, file string) {
	t.Helper()
	err := store.Transact(ctx, func(tx storage.Tx) error {
		src, err := tx.Source("github.com/test/shared", sender)
		if err != nil {
			return err
		}
		return tx.Link(src, model.Relation{From: model.EntryNode(src, entryID), Type: model.FileEdge(model.Fact), To: "file:" + src.ID + ":" + file})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdoptSkipsUnsafeAndMissingFileTargets(t *testing.T) {
	ctx := context.Background()
	cwd := adoptRepo(t)

	// Outside the repository, reachable only through a symlink escape.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := newFakeStore()
	s := app.New(store)
	sender := uuid.NewV7().String()
	touchFiles(t, cwd, "a.go")
	entryID := sendAdoptable(t, ctx, s, sender, "mixed files", "some skip, one lands",
		[]string{"a.go", "escape/secret.txt", "gone.go"}, nil)
	// Protocol validation rejects traversal at receive time, so the traversal
	// path is injected directly into the source table.
	injectLink(t, ctx, store, sender, entryID, "../outside.txt")

	out, err := s.Adopt(ctx, cwd, entryID, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Relations) != 1 || !strings.HasPrefix(out.Relations[0].To, "file:") || !strings.HasSuffix(out.Relations[0].To, ":a.go") {
		t.Fatalf("only the safe existing file must be linked: %+v", out)
	}
	if len(out.Skipped) != 3 {
		t.Fatalf("expected three skipped links, got %+v", out.Skipped)
	}
	for _, want := range []string{"../outside.txt", "escape/secret.txt", "gone.go"} {
		if !strings.Contains(strings.Join(out.Skipped, "\n"), want) {
			t.Fatalf("skip reason for %q missing in %+v", want, out.Skipped)
		}
	}
}

func TestAdoptIdempotentAndMultiSender(t *testing.T) {
	ctx := context.Background()
	cwd := adoptRepo(t)
	touchFiles(t, cwd, "a.go", "b.go")
	store := newFakeStore()
	s := app.New(store)
	senderA, senderB := uuid.NewV7().String(), uuid.NewV7().String()

	entryA := sendAdoptable(t, ctx, s, senderA, "A fact", "from A", []string{"a.go"}, nil)
	entryB := sendAdoptable(t, ctx, s, senderB, "B fact", "from B", []string{"b.go"}, nil)

	first, err := s.Adopt(ctx, cwd, entryA, senderA)
	if err != nil || first.AlreadyAdopted || first.SourceEntryID != entryA || first.SourceElephantID != senderA {
		t.Fatalf("%+v %v", first, err)
	}
	if first.Entry.ID == entryA {
		t.Fatal("adopted entry must receive a new local ID")
	}
	again, err := s.Adopt(ctx, cwd, entryA, senderA)
	if err != nil || !again.AlreadyAdopted || again.Entry.ID != first.Entry.ID {
		t.Fatalf("%+v %v", again, err)
	}

	second, err := s.Adopt(ctx, cwd, entryB, senderB)
	if err != nil || second.AlreadyAdopted {
		t.Fatalf("%+v %v", second, err)
	}
	if second.Entry.ID == first.Entry.ID {
		t.Fatal("different senders must adopt into distinct entries")
	}
	if len(second.Relations) != 1 || !strings.HasPrefix(second.Relations[0].To, "file:") || !strings.HasSuffix(second.Relations[0].To, ":b.go") {
		t.Fatalf("%+v", second)
	}
}

func TestAdoptRelationTargetsSkippedAndMapped(t *testing.T) {
	ctx := context.Background()
	cwd := adoptRepo(t)
	store := newFakeStore()
	s := app.New(store)
	sender := uuid.NewV7().String()

	// A decision and a fact supporting it, sent by the same peer.
	decID := sendAdoptableKind(t, ctx, s, model.Decision, sender, "D", "why", nil, nil)
	factID := sendAdoptable(t, ctx, s, sender, "F", "supports D", nil, []app.RelationInput{{Type: "supports", EntryID: decID}})

	// Nothing adopted yet: the relation target cannot be resolved locally, so
	// it is skipped without failing the adoption.
	out, err := s.Adopt(ctx, cwd, factID, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Relations) != 0 || len(out.Skipped) != 1 {
		t.Fatalf("relation must be skipped while target is unknown: %+v", out)
	}

	// After the decision is adopted, a later fact resolves its relation
	// through the adoption mapping.
	dec, err := s.Adopt(ctx, cwd, decID, sender)
	if err != nil {
		t.Fatal(err)
	}
	fact2ID := sendAdoptable(t, ctx, s, sender, "F2", "supports D too", nil, []app.RelationInput{{Type: "supports", EntryID: decID}})
	out2, err := s.Adopt(ctx, cwd, fact2ID, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2.Relations) != 1 || out2.Relations[0].Type != "supports" || out2.Relations[0].To != "entry:"+dec.Entry.ID {
		t.Fatalf("relation must map to the adopted decision: %+v", out2)
	}
}

func TestDiffSeparatesRemoteOnlyAndConflicting(t *testing.T) {
	ctx := context.Background()
	cwd := adoptRepo(t)
	store := newFakeStore()
	s := app.New(store)
	sender := uuid.NewV7().String()

	// One entry only the sender has, plus a duplicate of local knowledge:
	// same kind, title, and body under a different remote ID.
	newID := sendAdoptable(t, ctx, s, sender, "only remote", "fresh", nil, nil)
	sendAdoptable(t, ctx, s, sender, "conflict", "same locally", nil, nil)
	if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "conflict", Body: "same locally"}); err != nil {
		t.Fatal(err)
	}

	diff, err := s.Diff(ctx, cwd, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.RemoteOnly) != 1 || diff.RemoteOnly[0].ID != newID {
		t.Fatalf("remote_only must hold the genuinely new row: %+v", diff)
	}
	if len(diff.Conflicting) != 1 || diff.Conflicting[0].Title != "conflict" {
		t.Fatalf("conflicting must hold the duplicate row: %+v", diff)
	}
}
