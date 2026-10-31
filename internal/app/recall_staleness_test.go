package app_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func gitIn(t *testing.T, cwd string, args ...string) {
	t.Helper()
	full := append([]string{"-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
}

// writeCommit writes content to a repository-relative file and commits it.
func writeCommit(t *testing.T, cwd, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	commitFile(t, cwd, file, "change "+file)
}

func stalenessOf(t *testing.T, r app.RecallResult, node string) app.Staleness {
	t.Helper()
	for _, s := range r.Staleness {
		if s.Node == node {
			return s
		}
	}
	t.Fatalf("no staleness for %s in %+v", node, r.Staleness)
	return app.Staleness{}
}

func TestRecallReportsStaleness(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	writeCommit(t, cwd, "a.go", "a1")
	writeCommit(t, cwd, "b.go", "b1")
	db, err := zova.Open(filepath.Join(t.TempDir(), "stale.zova"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := app.New(db)
	linked, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "A", Body: "about a", RelatedFiles: []string{"a.go", "b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "T", Body: "no files"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "work", RelatedFiles: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	recorded := headOf(t, cwd)

	// Nothing has moved yet: everything is current.
	r, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	cpNode := model.CheckpointNode(r.Project.TableName, checkpoint.Checkpoint.ID)
	for _, node := range []string{cpNode, "entry:" + linked.Entry.ID, "entry:" + plain.Entry.ID} {
		got := stalenessOf(t, r, node)
		if got.History != "current" || got.CommitsBehind == nil || *got.CommitsBehind != 0 || len(got.ChangedFiles) != 0 || got.Commit != recorded || got.AgeDays != 0 {
			t.Fatalf("%s at HEAD: %+v", node, got)
		}
	}

	// Two commits later, one touching a.go.
	writeCommit(t, cwd, "a.go", "a2")
	writeCommit(t, cwd, "c.go", "c1")
	if r, err = s.Recall(ctx, cwd, ""); err != nil {
		t.Fatal(err)
	}
	for node, files := range map[string][]string{cpNode: {"a.go"}, "entry:" + linked.Entry.ID: {"a.go"}, "entry:" + plain.Entry.ID: {}} {
		got := stalenessOf(t, r, node)
		if got.History != "behind" || got.CommitsBehind == nil || *got.CommitsBehind != 2 || !slices.Equal(got.ChangedFiles, files) {
			t.Fatalf("%s behind: %+v, want 2 commits and %v", node, got, files)
		}
	}

	// Rewinding HEAD past the recorded commit makes it unreachable.
	gitIn(t, cwd, "reset", "-q", "--hard", "HEAD~3")
	if r, err = s.Recall(ctx, cwd, ""); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{cpNode, "entry:" + linked.Entry.ID} {
		got := stalenessOf(t, r, node)
		if got.History != "unknown" || got.CommitsBehind != nil || len(got.ChangedFiles) != 0 || got.Commit != recorded {
			t.Fatalf("%s unreachable: %+v", node, got)
		}
	}
}

func TestRecallStalenessAgeAndMissingCommit(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "age.zova"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := app.New(db)
	old, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Old", Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// A commit that history no longer contains, and an entry with no commit.
	missing := "0123456789abcdef0123456789abcdef01234567"
	gone := model.Entry{ID: "01a20000-0000-7000-8000-000000000002", ActorID: "t", Kind: model.Decision, Title: "Gone", Body: "rewritten history", Status: "active", StartCommit: &missing, CreatedAt: old.Entry.CreatedAt, UpdatedAt: old.Entry.UpdatedAt}
	none := gone
	none.ID, none.Title, none.StartCommit = "01a20000-0000-7000-8000-000000000001", "No commit", nil
	if err = db.Transact(ctx, func(tx storage.Tx) error {
		aged := old.Entry
		aged.UpdatedAt = time.Now().UTC().Add(-75 * time.Hour)
		for _, e := range []model.Entry{aged, gone, none} {
			if err := tx.Put(status.Project, e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := stalenessOf(t, r, "entry:"+old.Entry.ID); got.AgeDays != 3 || got.History != "current" {
		t.Fatalf("aged entry: %+v", got)
	}
	if got := stalenessOf(t, r, "entry:"+gone.ID); got.History != "unknown" || got.CommitsBehind != nil || got.Commit != missing {
		t.Fatalf("missing commit: %+v", got)
	}
	if got := stalenessOf(t, r, "entry:"+none.ID); got.History != "unrecorded" || got.CommitsBehind != nil || got.Commit != "" {
		t.Fatalf("no commit: %+v", got)
	}
}

func TestRecallStalenessBoundsCommitChecks(t *testing.T) {
	ctx := context.Background()
	cwd := checkpointRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "bound.zova"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := app.New(db)
	for i := range app.RecallStaleCommitLimit + 1 {
		if _, err = s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: fmt.Sprint("F", i), Body: "b"}); err != nil {
			t.Fatal(err)
		}
		writeCommit(t, cwd, "f.txt", fmt.Sprint(i))
	}
	r, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated["staleness"] || len(r.Staleness) != app.RecallStaleCommitLimit {
		t.Fatalf("staleness not bounded: %d entries, truncated=%v", len(r.Staleness), r.Truncated)
	}
}
