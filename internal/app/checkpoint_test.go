package app_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

var errCheckpointBoom = errors.New("injected checkpoint failure")

func checkpointRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/checkpoints.git"},
		{"-C", p, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
		{"-C", p, "rev-parse", "HEAD"},
	} {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	return p
}

func headOf(t *testing.T, cwd string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return strings.TrimSpace(string(out))
}

func TestCheckpointLifecycleRecallAndIsolation(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	path := filepath.Join(t.TempDir(), "checkpoint.zova")
	db, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := app.New(db)
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: ""}); err == nil {
		t.Fatal("empty summary accepted")
	}
	oversized := strings.Repeat("x", 2001)
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: oversized}); err == nil {
		t.Fatal("oversized summary accepted")
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "S", Completed: []string{""}}); err == nil {
		t.Fatal("empty list item accepted")
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "S", Next: []string{strings.Repeat("y", 2001)}}); err == nil {
		t.Fatal("oversized list item accepted")
	}
	many := make([]string, model.MaxCheckpointItems+1)
	for i := range many {
		many[i] = "item"
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "S", Commands: many}); err == nil {
		t.Fatal("overlong list accepted")
	}
	fact, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "F", Body: "B"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{
		Summary: "Implemented authentication", Next: []string{"Add integration tests"},
		Completed:    []string{"Wired handlers", "Updated docs"},
		RelatedFiles: []string{"internal/auth.go"},
		Relations:    []app.RelationInput{{Type: "relates", EntryID: fact.Entry.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint.ActorID == "" || first.Checkpoint.EndCommit == nil || len(first.Relations) != 2 {
		t.Fatalf("%+v", first)
	}
	if len(first.Checkpoint.Completed) != 2 || len(first.Checkpoint.Next) != 1 {
		t.Fatalf("ordered lists not preserved: %+v", first.Checkpoint)
	}
	// Explicit start commits are verified with Git.
	head := headOf(t, cwd)
	explicit, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Explicit", StartCommit: head})
	if err != nil || explicit.Checkpoint.StartCommit == nil || *explicit.Checkpoint.StartCommit != head {
		t.Fatalf("%+v %v", explicit, err)
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Bogus", StartCommit: "deadbeef"}); err == nil {
		t.Fatal("unknown start commit accepted")
	}
	// Atomicity: bad relation must roll back the checkpoint row and links.
	before, err := s.ListCheckpoints(ctx, cwd, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Bad", Relations: []app.RelationInput{{Type: "relates", EntryID: "missing"}}}); err == nil {
		t.Fatal("invalid checkpoint relation accepted")
	}
	after, err := s.ListCheckpoints(ctx, cwd, 10, 0)
	if err != nil || len(after) != len(before) {
		t.Fatal(after, err)
	}
	second, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Second", Completed: []string{"Did X"}, Commands: []string{"go test ./..."}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Checkpoint.StartCommit == nil || first.Checkpoint.EndCommit == nil || *second.Checkpoint.StartCommit != *first.Checkpoint.EndCommit {
		t.Fatalf("start commit should chain: %+v %+v", first.Checkpoint, second.Checkpoint)
	}
	// List bounds: newest first, offset pages, invalid bounds rejected.
	list, err := s.ListCheckpoints(ctx, cwd, 2, 0)
	if err != nil || len(list) != 2 || list[0].ID != second.Checkpoint.ID {
		t.Fatal(list, err)
	}
	page, err := s.ListCheckpoints(ctx, cwd, 2, 2)
	if err != nil || len(page) != 1 || page[0].ID != first.Checkpoint.ID {
		t.Fatal(page, err)
	}
	if _, err = s.ListCheckpoints(ctx, cwd, 201, 0); err == nil {
		t.Fatal("overlong limit accepted")
	}
	if _, err = s.ListCheckpoints(ctx, cwd, 10, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	// Recall carries only the latest checkpoint.
	packet, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if packet.Checkpoint == nil || packet.Checkpoint.Checkpoint.ID != second.Checkpoint.ID {
		t.Fatalf("%+v", packet.Checkpoint)
	}
	// Isolation: another project with the same origin URL shape sees nothing.
	// (checkpointRepo uses a distinct temp dir but the same origin; use a
	// truly different origin for isolation.)
	foreign := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", foreign},
		{"-C", foreign, "remote", "add", "origin", "https://github.com/test/other.git"},
		{"-C", foreign, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	none, err := s.ListCheckpoints(ctx, foreign, 10, 0)
	if err != nil || len(none) != 0 {
		t.Fatal(none, err)
	}
	otherPacket, err := s.Recall(ctx, foreign, "")
	if err != nil || otherPacket.Checkpoint != nil {
		t.Fatal(otherPacket, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2 := app.New(db2)
	restarted, err := s2.ListCheckpoints(ctx, cwd, 10, 0)
	if err != nil || len(restarted) != 3 {
		t.Fatal(restarted, err)
	}
	if restarted[0].ID != second.Checkpoint.ID {
		t.Fatalf("restart lost ordering: %v", restarted)
	}
}

func TestCheckpointRemoteRecallSeparation(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "separation.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	local, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Local boundary"})
	if err != nil {
		t.Fatal(err)
	}
	sender := uuid.NewV7().String()
	id := func() string { return uuid.NewV7().String() }
	now := time.Now().UTC()
	m := app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: sender,
		Project:   app.ProjectRef{Identity: "github.com/test/checkpoints", Name: "checkpoints"},
		Operation: "entry.send",
		Entry:     &model.Entry{ID: id(), Kind: model.Fact, Title: "Remote", Body: "fact", Status: "active", ActorID: "peer", CreatedAt: now, UpdatedAt: now}}
	if _, err = s.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	// Remote recall carries remote entries but never the local checkpoint.
	remote, err := s.RecallRemote(ctx, cwd, sender)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Checkpoint != nil {
		t.Fatalf("local checkpoint leaked into remote recall: %+v", remote.Checkpoint)
	}
	if len(remote.Facts) != 1 {
		t.Fatalf("%+v", remote)
	}
	localPacket, err := s.Recall(ctx, cwd, "")
	if err != nil || localPacket.Checkpoint == nil || localPacket.Checkpoint.Checkpoint.ID != local.Checkpoint.ID {
		t.Fatalf("%+v %v", localPacket, err)
	}
}

func TestCheckpointFailedLinkRollback(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "linkfail.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// A checkpoint whose graph link cannot be written must leave no row.
	// Force the failure by pre-creating a checkpoint node collision is not
	// possible through the API, so assert the observable contract instead:
	// every rejected mutation leaves the table unchanged.
	before, err := s.ListCheckpoints(ctx, cwd, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []app.CheckpointInput{
		{Summary: "Bad file", RelatedFiles: []string{"../escape"}},
		{Summary: "Bad edge", Relations: []app.RelationInput{{Type: "nope", EntryID: uuid.NewV7().String()}}},
		{Summary: ""},
	} {
		if _, err = s.AddCheckpoint(ctx, cwd, in); err == nil {
			t.Fatalf("invalid input accepted: %+v", in)
		}
	}
	after, err := s.ListCheckpoints(ctx, cwd, 200, 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("%v %v", before, after)
	}
	// Direct storage rollback: a failed transaction writes no checkpoint row.
	injected := errInjected()
	err = db.Transact(ctx, func(tx storage.Tx) error {
		c := model.Checkpoint{ID: uuid.NewV7().String(), ActorID: "t", Summary: "doomed",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := tx.PutCheckpoint(status.Project, c); err != nil {
			return err
		}
		return injected
	})
	if err != injected {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	after, err = s.ListCheckpoints(ctx, cwd, 200, 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("rolled-back checkpoint survived: %v", after)
	}
}

func errInjected() error { return errCheckpointBoom }
