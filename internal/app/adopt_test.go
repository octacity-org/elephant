package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func sharedRepo(t *testing.T) string {
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

func TestInboxDiffAdopt(t *testing.T) {
	ctx := context.Background()
	cwd := sharedRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "adopt.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	// File links now copy only when the target exists in the local repository,
	// so create the referenced files before receiving.
	for _, f := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(cwd, f), []byte("package shared"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := func() string { return uuid.NewV7().String() }
	now := time.Now().UTC()
	senderA, senderB := id(), id()
	mk := func(sender, msgID, entryID, title, body string, files []string, relations []app.RelationInput) app.Message {
		return app.Message{ProtocolVersion: 1, MessageID: msgID, SenderElephantID: sender,
			Project:   app.ProjectRef{Identity: "github.com/test/shared", Name: "shared"},
			Operation: "entry.send",
			Entry:     &model.Entry{ID: entryID, Kind: model.Fact, Title: title, Body: body, Status: "active", ActorID: "peer", CreatedAt: now, UpdatedAt: now},
			Files:     files, Relations: relations}
	}
	entryA, entryB := id(), id()
	if _, err = s.Receive(ctx, mk(senderA, id(), entryA, "A fact", "from A", []string{"a.go"}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Receive(ctx, mk(senderB, id(), entryB, "B fact", "from B", []string{"b.go"}, nil)); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.Inbox(ctx)
	if err != nil || len(inbox) != 2 {
		t.Fatalf("%+v %v", inbox, err)
	}
	for _, g := range inbox {
		if len(g.Entries) != 1 || g.ProjectIdentity != "github.com/test/shared" {
			t.Fatalf("%+v", g)
		}
	}
	diff, err := s.Diff(ctx, cwd, senderA)
	if err != nil || len(diff.Remote) != 1 || len(diff.RemoteOnly) != 1 || len(diff.Local) != 0 {
		t.Fatalf("%+v %v", diff, err)
	}
	if diff.Source.SourceElephantID != senderA || diff.Project.Scope != "local" {
		t.Fatalf("%+v", diff)
	}
	first, err := s.Adopt(ctx, cwd, entryA, senderA)
	if err != nil || first.AlreadyAdopted || first.SourceEntryID != entryA || first.SourceElephantID != senderA {
		t.Fatalf("%+v %v", first, err)
	}
	if first.Entry.ID == entryA || len(first.Relations) != 1 {
		t.Fatalf("%+v", first)
	}
	again, err := s.Adopt(ctx, cwd, entryA, senderA)
	if err != nil || !again.AlreadyAdopted || again.Entry.ID != first.Entry.ID {
		t.Fatalf("%+v %v", again, err)
	}
	// Adopting an entry whose relation target cannot be resolved locally skips safely.
	entryA2 := id()
	rel := []app.RelationInput{{Type: "supports", EntryID: entryA}}
	// supports requires fact->decision; use a decision target instead: send decision then fact supporting it.
	decID := id()
	if _, err = s.Receive(ctx, app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: senderA,
		Project: app.ProjectRef{Identity: "github.com/test/shared", Name: "shared"}, Operation: "entry.send",
		Entry: &model.Entry{ID: decID, Kind: model.Decision, Title: "D", Body: "why", Status: "active", ActorID: "peer", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	_ = rel
	factSupporting := id()
	if _, err = s.Receive(ctx, mk(senderA, id(), factSupporting, "F", "supports D", nil, []app.RelationInput{{Type: "supports", EntryID: decID}})); err != nil {
		t.Fatal(err)
	}
	adopted, err := s.Adopt(ctx, cwd, factSupporting, senderA)
	if err != nil || len(adopted.Relations) != 0 {
		// Relation target was not adopted locally yet, so it must be skipped, not fail.
		t.Fatalf("%+v %v", adopted, err)
	}
	_ = entryA2
	// Second sender still unadopted.
	diffB, err := s.Diff(ctx, cwd, senderB)
	if err != nil || len(diffB.RemoteOnly) != 1 {
		t.Fatalf("%+v %v", diffB, err)
	}
	// Unknown source fails cleanly.
	if _, err = s.Adopt(ctx, cwd, entryB, uuid.NewV7().String()); err == nil {
		t.Fatal("adopt from unknown source accepted")
	}
	// Local recall never includes remote rows.
	local, err := s.Recall(ctx, cwd, "")
	if err != nil || len(local.Facts) != 2 {
		t.Fatalf("%+v %v", local, err)
	}
}
