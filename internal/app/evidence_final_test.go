package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func TestEvidenceRemovalAndMissingStates(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "missing.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	entryID, second := attachFact(t, s, cwd, "runtime.go", 0)
	if _, err = s.RefreshEvidence(ctx, cwd, entryID, "missing"); !errors.Is(err, model.ErrEvidenceNotFound) {
		t.Fatalf("refresh missing: %v", err)
	}
	if _, err = s.RefreshEvidence(ctx, cwd, "missing", ""); !errors.Is(err, model.ErrEntryNotFound) {
		t.Fatalf("refresh missing entry: %v", err)
	}
	if _, err = s.RemoveEvidence(ctx, cwd, "missing"); !errors.Is(err, model.ErrEvidenceNotFound) {
		t.Fatalf("remove missing: %v", err)
	}
	if _, err = s.RemoveEvidence(ctx, cwd, second.ID); err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyEvidence(ctx, cwd, entryID)
	if err != nil || len(report.Evidence) != 0 {
		t.Fatalf("%+v %v", report, err)
	}
	if got, err := s.Get(ctx, cwd, entryID); err != nil || got.Entry.Status != "active" {
		t.Fatalf("verification must not retire the fact: %+v %v", got, err)
	}
	// A path never committed cannot be captured.
	extraFact, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Extra", Body: "Context"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvidence(ctx, cwd, extraFact.Entry.ID, "absent.go", 0); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("attaching to a file absent at HEAD: %v", err)
	}
	// A file deleted from the working tree still captures from HEAD, then
	// verifies missing.
	if err = os.Remove(filepath.Join(cwd, "runtime.go")); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AddEvidence(ctx, cwd, extraFact.Entry.ID, "runtime.go", 0)
	if err != nil {
		t.Fatalf("committed file deleted from the worktree must still attach: %v", err)
	}
	report, err = s.VerifyEvidence(ctx, cwd, extraFact.Entry.ID)
	if err != nil || report.Counts[model.EvidenceMissing] != 1 {
		t.Fatalf("deleted worktree file must be missing: %+v %v", report, err)
	}
	_ = deleted
}

func TestEvidenceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	path := filepath.Join(t.TempDir(), "restart.zova")
	db, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := app.New(db)
	entryID, first := attachFact(t, s, cwd, "runtime.go", 3)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s = app.New(db)
	report, err := s.VerifyEvidence(ctx, cwd, entryID)
	if err != nil || len(report.Evidence) != 1 || report.Evidence[0].ID != first.ID || report.Counts[model.EvidenceUnchanged] != 1 {
		t.Fatalf("evidence did not survive restart: %+v %v", report, err)
	}
}

func TestEvidenceVerifyReadOnlyAndRollback(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	path := filepath.Join(t.TempDir(), "readonly.zova")
	db, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	entryID, first := attachFact(t, s, cwd, "runtime.go", 3)
	if _, err = s.AddEvidence(ctx, cwd, entryID, "runtime.go", 0); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (model.Entry, []model.Evidence) {
		rec, err := s.Get(ctx, cwd, entryID)
		if err != nil {
			t.Fatal(err)
		}
		status, err := s.Status(ctx, cwd)
		if err != nil {
			t.Fatal(err)
		}
		var rows []model.Evidence
		if err = db.Transact(ctx, func(tx storage.Tx) error {
			var err error
			rows, err = tx.Evidence(status.Project, entryID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return rec.Entry, rows
	}
	beforeEntry, beforeRows := snapshot()
	if _, err = s.VerifyEvidence(ctx, cwd, entryID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Recall(ctx, cwd, ""); err != nil {
		t.Fatal(err)
	}
	afterEntry, afterRows := snapshot()
	if !reflect.DeepEqual(beforeEntry, afterEntry) {
		t.Fatalf("verify/recall mutated the fact:\n%+v\n%+v", beforeEntry, afterEntry)
	}
	if !reflect.DeepEqual(beforeRows, afterRows) {
		t.Fatalf("verify/recall mutated evidence rows:\n%+v\n%+v", beforeRows, afterRows)
	}
	// A failed evidence write rolls back without touching stored rows.
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	now := beforeEntry.UpdatedAt
	doomed := model.Evidence{ID: uuid.NewV7().String(), EntryID: entryID, Path: "runtime.go", CreatedAt: now, VerifiedAt: now}
	err = db.Transact(ctx, func(tx storage.Tx) error {
		if err := tx.PutEvidence(status.Project, doomed); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatalf("expected the injected rollback, got %v", err)
	}
	_, afterRows = snapshot()
	if !reflect.DeepEqual(beforeRows, afterRows) {
		t.Fatalf("rolled-back evidence survived: %+v", afterRows)
	}
	_ = first
}

func TestEvidenceUnavailableCommitAndShrunkenFile(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "edge.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	entryID, _ := attachFact(t, s, cwd, "runtime.go", 3)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// A recorded commit that cannot be read reports unavailable, never changed.
	ghost := model.Evidence{ID: uuid.NewV7().String(), EntryID: entryID, Path: "runtime.go", Line: 1,
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Blob:      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		CreatedAt: status.Project.CreatedAt, VerifiedAt: status.Project.CreatedAt}
	if err = db.Transact(ctx, func(tx storage.Tx) error { return tx.PutEvidence(status.Project, ghost) }); err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyEvidence(ctx, cwd, entryID)
	if err != nil || len(report.Evidence) != 2 {
		t.Fatalf("%+v %v", report, err)
	}
	if report.Counts[model.EvidenceUnavailable] != 1 || report.Counts[model.EvidenceUnchanged] != 1 {
		t.Fatalf("ghost commit must be unavailable: %+v", report)
	}
	if err = db.Transact(ctx, func(tx storage.Tx) error { return tx.DeleteEvidence(status.Project, ghost.ID) }); err != nil {
		t.Fatal(err)
	}
	// Shrinking the file below the pinned line reports missing, not changed.
	writeWorkingFile(t, cwd, "runtime.go", "package runtime\n")
	report, err = s.VerifyEvidence(ctx, cwd, entryID)
	if err != nil || report.Counts[model.EvidenceMissing] != 1 {
		t.Fatalf("shrunken file must be missing: %+v %v", report, err)
	}
	// Restoring the committed bytes verifies unchanged again.
	writeWorkingFile(t, cwd, "runtime.go", committedRuntimeGo)
	report, err = s.VerifyEvidence(ctx, cwd, entryID)
	if err != nil || report.Counts[model.EvidenceUnchanged] != 1 {
		t.Fatalf("%+v %v", report, err)
	}
}
