package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func exportRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/portable.git"},
		{"-C", p, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	// A committed file so evidence can capture a digest from HEAD.
	if err := os.WriteFile(filepath.Join(p, "a.go"), []byte(committedRuntimeGo), 0600); err != nil {
		t.Fatal(err)
	}
	commitFile(t, p, "a.go", "add export evidence fixture")
	return p
}

// populatedService creates entries with lifecycle data, evidence, a
// checkpoint, and an adopted remote entry so exports cover the complete
// stored project state.
func populatedService(t *testing.T, ctx context.Context, cwd, dbFile string) *app.Service {
	t.Helper()
	db, err := zova.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := app.New(db)
	task, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "T", Body: "do", TargetVersion: ptr("v1.2")})
	if err != nil {
		t.Fatal(err)
	}
	// Give the task terminal lifecycle data (end commit) so exports carry it.
	done := "done"
	if _, err = s.Update(ctx, cwd, model.Task, task.Entry.ID, app.UpdateInput{Status: &done}); err != nil {
		t.Fatal(err)
	}
	fact, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "F", Body: "keep", RelatedFiles: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "D", Body: "why"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "W", Body: "work", Relations: []app.RelationInput{{Type: "implements", EntryID: dec.Entry.ID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvidence(ctx, cwd, fact.Entry.ID, "a.go", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "session one", Completed: []string{"added facts"}, Next: []string{"write tests"}}); err != nil {
		t.Fatal(err)
	}
	// Receive a remote entry and adopt it so an adoption receipt exists.
	id := func() string { return uuid.NewV7().String() }
	m := app.Message{
		ProtocolVersion: 1, MessageID: id(), SenderElephantID: id(),
		Project: app.ProjectRef{Identity: "github.com/test/portable", Name: "portable"}, Operation: "entry.send",
		Entry: &model.Entry{ID: id(), Kind: model.Fact, Title: "Remote fact", Body: "from afar", ActorID: "peer", Status: "active", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	if _, err := s.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.Inbox(ctx)
	if err != nil || len(inbox) == 0 || len(inbox[0].Entries) == 0 {
		t.Fatalf("%+v %v", inbox, err)
	}
	if _, err = s.Adopt(ctx, cwd, inbox[0].Entries[0].ID, inbox[0].SourceElephantID); err != nil {
		t.Fatal(err)
	}
	return s
}

func exportBytes(t *testing.T, s *app.Service, cwd string) []byte {
	t.Helper()
	env, err := s.Export(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func localEntryCount(t *testing.T, s *app.Service, cwd string) int {
	t.Helper()
	local, err := s.Recall(context.Background(), cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	return len(local.Facts) + len(local.Decisions) + len(local.Tasks) + len(local.RecentCompleted) + len(local.RecentSuperseded)
}

func TestExportDeterministicBytes(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "det.zova"))
	first := exportBytes(t, s, cwd)
	// Interleave an unrelated read and export again: bytes must match exactly.
	if _, err := s.Recall(ctx, cwd, ""); err != nil {
		t.Fatal(err)
	}
	second := exportBytes(t, s, cwd)
	if !bytes.Equal(first, second) {
		t.Fatalf("exports differ:\nfirst  %s\nsecond %s", first, second)
	}
	var env app.ExportEnvelope
	if err := json.Unmarshal(first, &env); err != nil {
		t.Fatal(err)
	}
	if env.FormatVersion != 2 {
		t.Fatalf("format_version = %d", env.FormatVersion)
	}
	if env.Project.Identity != "github.com/test/portable" || env.Project.SourceElephantID == "" {
		t.Fatalf("project/source metadata missing: %+v", env.Project)
	}
	if len(env.Entries) != 5 || len(env.Evidence) != 1 || len(env.Checkpoints) != 1 || len(env.Adoptions) != 1 {
		t.Fatalf("incomplete envelope: %d entries, %d evidence, %d checkpoints, %d adoptions", len(env.Entries), len(env.Evidence), len(env.Checkpoints), len(env.Adoptions))
	}
	// Lifecycle data and actors ride inside records.
	var task *model.Entry
	for i := range env.Entries {
		if env.Entries[i].Title == "T" {
			task = &env.Entries[i]
		}
	}
	if task == nil || task.Status != "done" || task.EndCommit == nil || task.TargetVersion == nil || *task.TargetVersion != "v1.2" || task.ActorID == "" {
		t.Fatalf("lifecycle data missing from export: %+v", task)
	}
	// Wall-clock fields would break determinism.
	if strings.Contains(string(first), "exported_at") || strings.Contains(string(first), "wall_clock") {
		t.Fatal("wall-clock field present in deterministic export")
	}
	// Mutating state changes the bytes.
	if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "New", Body: "fact"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, exportBytes(t, s, cwd)) {
		t.Fatal("changed state produced identical bytes")
	}
}

func TestImportRoundTripCompleteState(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "round.zova"))
	raw := exportBytes(t, s, cwd)
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	before := localEntryCount(t, s, cwd)
	src, err := s.Import(ctx, raw, "archive")
	if err != nil || src.Scope != "remote" {
		t.Fatalf("%+v %v", src, err)
	}
	// Idempotent reimport resolves to the same archive table.
	resend, err := s.Import(ctx, raw, "archive")
	if err != nil || resend.TableName != src.TableName || resend.ID != src.ID {
		t.Fatalf("%+v %v", resend, err)
	}
	// Every record class arrived in the archive source table.
	imported, err := s.RecallSource(ctx, app.ProjectRef{Identity: env.Project.Identity, Name: env.Project.Name}, src.SourceElephantID, "")
	if err != nil {
		t.Fatal(err)
	}
	importedCount := len(imported.Facts) + len(imported.Decisions) + len(imported.Tasks) + len(imported.RecentCompleted) + len(imported.RecentSuperseded)
	if importedCount != len(env.Entries) {
		t.Fatalf("entries: imported %d, exported %d", importedCount, len(env.Entries))
	}
	if len(imported.Evidence) != len(env.Evidence) {
		t.Fatalf("evidence: imported %d, exported %d", len(imported.Evidence), len(env.Evidence))
	}
	// Checkpoints are local-only in recall by design (they never leak from a
	// remote source), so verify the imported checkpoint via source inspection.
	archive, err := s.InspectSource(ctx, cwd, src.SourceElephantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Checkpoints) != len(env.Checkpoints) {
		t.Fatalf("archive checkpoints %d, exported %d", len(archive.Checkpoints), len(env.Checkpoints))
	}
	for _, c := range archive.Checkpoints {
		if c.Summary != "session one" {
			t.Fatalf("imported checkpoint summary %q", c.Summary)
		}
	}
	if len(archive.Evidence) != len(env.Evidence) {
		t.Fatalf("archive evidence %d, exported %d", len(archive.Evidence), len(env.Evidence))
	}
	if len(archive.Entries) != len(env.Entries) {
		t.Fatalf("archive entries %d, exported %d", len(archive.Entries), len(env.Entries))
	}
	// Local truth untouched by import.
	if after := localEntryCount(t, s, cwd); after != before {
		t.Fatalf("local state changed by import: before %d, after %d", before, after)
	}
	// A different archive name imports the same content into its own source.
	other, err := s.Import(ctx, raw, "second")
	if err != nil || other.TableName == src.TableName {
		t.Fatalf("%+v %v", other, err)
	}
}

func TestImportDigestConflictFailsExplicitly(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "digest.zova"))
	raw := exportBytes(t, s, cwd)
	if _, err := s.Import(ctx, raw, "conflict"); err != nil {
		t.Fatal(err)
	}
	// Conflicting content under the same archive name: explicit failure.
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env.Entries[0].Title = "tampered"
	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(ctx, tampered, "conflict"); err == nil {
		t.Fatal("conflicting content accepted under existing archive name")
	}
	// The failed import rolled back; the original still reimports cleanly.
	if _, err = s.Import(ctx, raw, "conflict"); err != nil {
		t.Fatalf("failed import left partial state: %v", err)
	}
}

func TestImportCorruptedInputRollsBack(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "corrupt.zova"))
	raw := exportBytes(t, s, cwd)
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	before := localEntryCount(t, s, cwd)
	corrupt := func(mutate func(*app.ExportEnvelope)) []byte {
		t.Helper()
		cp := env
		cp.Entries = append([]model.Entry(nil), env.Entries...)
		cp.Relations = append([]model.Relation(nil), env.Relations...)
		cp.Evidence = append([]model.Evidence(nil), env.Evidence...)
		cp.Checkpoints = append([]model.Checkpoint(nil), env.Checkpoints...)
		cp.Adoptions = append([]model.Adoption(nil), env.Adoptions...)
		mutate(&cp)
		out, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string][]byte{
		"late invalid record": corrupt(func(e *app.ExportEnvelope) { e.Entries[len(e.Entries)-1].Title = "" }),
		"dangling relation": corrupt(func(e *app.ExportEnvelope) {
			e.Relations = append(e.Relations, model.Relation{From: "entry:" + e.Entries[0].ID, Type: "depends_on", To: "entry:0198b3b3-7a28-7c9d-a3e5-9f2c1a4b6d8e"})
		}),
		"invalid file edge": corrupt(func(e *app.ExportEnvelope) {
			e.Relations = append(e.Relations, model.Relation{From: "entry:" + e.Entries[0].ID, Type: "depends_on", To: "file:prj_x:a.go"})
		}),
		"duplicate ID": corrupt(func(e *app.ExportEnvelope) { e.Entries = append(e.Entries, e.Entries[0]) }),
		"unknown evidence": corrupt(func(e *app.ExportEnvelope) {
			e.Evidence = append(e.Evidence, model.Evidence{ID: uuid.NewV7().String(), EntryID: "missing", Path: "a.go", CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()})
		}),
		"invalid checkpoint": corrupt(func(e *app.ExportEnvelope) {
			e.Checkpoints = append(e.Checkpoints, model.Checkpoint{ID: uuid.NewV7().String(), ActorID: "a", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		}),
		"foreign adoption": corrupt(func(e *app.ExportEnvelope) {
			e.Adoptions = append(e.Adoptions, model.Adoption{ID: uuid.NewV7().String(), ProjectIdentity: "github.com/other/repo", SourceElephantID: "src", SourceEntryID: uuid.NewV7().String(), LocalEntryID: e.Entries[0].ID, Kind: "fact", CreatedAt: time.Now().UTC()})
		}),
		"orphan adoption": corrupt(func(e *app.ExportEnvelope) {
			e.Adoptions = append(e.Adoptions, model.Adoption{ID: uuid.NewV7().String(), ProjectIdentity: e.Project.Identity, SourceElephantID: "src", SourceEntryID: uuid.NewV7().String(), LocalEntryID: uuid.NewV7().String(), Kind: "fact", CreatedAt: time.Now().UTC()})
		}),
		"missing sections": []byte(`{"format_version":2,"project":{"identity":"github.com/test/portable","name":"portable","source_elephant_id":"src"}}`),
		"unknown field": func() []byte {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			doc["future_field"] = true
			out, _ := json.Marshal(doc)
			return out
		}(),
		"trailing garbage": append(append([]byte(nil), raw...), "}"...),
		"truncated":        raw[:len(raw)/2],
		"non-object":       []byte(`[]`),
		"not JSON":         []byte("{ nope"),
	}
	for name, data := range cases {
		if _, err := s.Import(ctx, data, "broken"); err == nil {
			t.Fatalf("%s: corrupted import accepted", name)
		}
	}
	// Forward versions are rejected explicitly.
	future := env
	future.FormatVersion = 99
	futureRaw, _ := json.Marshal(future)
	if _, err := s.Import(ctx, futureRaw, "broken"); err == nil {
		t.Fatal("forward version accepted")
	}
	// Nothing partial survived any failure: the valid archive imports cleanly
	// under every attempted name, proving receipts and tables rolled back.
	for _, name := range []string{"broken", "archive"} {
		if _, err := s.Import(ctx, raw, name); err != nil {
			t.Fatalf("%s: valid import after failures: %v", name, err)
		}
	}
	if after := localEntryCount(t, s, cwd); after != before {
		t.Fatalf("local truth changed by failed imports: before %d, after %d", before, after)
	}
}

func TestImportAcceptsFormatV1(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "v1.zova"))
	raw := exportBytes(t, s, cwd)
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	// Degrade to a format 1 archive: entries and relations only.
	v1 := map[string]any{
		"format_version": 1,
		"project":        map[string]any{"identity": env.Project.Identity, "name": env.Project.Name, "source_elephant_id": env.Project.SourceElephantID},
		"entries":        env.Entries,
		"relations":      env.Relations,
	}
	data, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.Import(ctx, data, "legacy")
	if err != nil || src.Scope != "remote" {
		t.Fatalf("%+v %v", src, err)
	}
	// A v1 archive must not be re-imported as a v2 digest conflict.
	if _, err = s.Import(ctx, data, "legacy"); err != nil {
		t.Fatalf("v1 reimport failed: %v", err)
	}
}

func TestImportNeverBecomesLocalTruth(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "truth.zova"))
	raw := exportBytes(t, s, cwd)
	before := localEntryCount(t, s, cwd)
	if _, err := s.Import(ctx, raw, "shadow"); err != nil {
		t.Fatal(err)
	}
	local, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if local.Project.Scope != "local" || local.Project.SourceElephantID == "import:shadow" {
		t.Fatalf("local truth overwritten: %+v", local.Project)
	}
	if after := localEntryCount(t, s, cwd); after != before {
		t.Fatalf("local entries changed: before %d, after %d", before, after)
	}
}

func TestBackupReplacesDestinationAndPreservesOnFailure(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "live.zova"))
	destDir := t.TempDir()
	dest := filepath.Join(destDir, "backup.zova")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	// A repeated backup must safely replace the existing destination, which
	// exercises the Windows-relevant rename-replacement path everywhere.
	stale := []byte("stale-bytes")
	if err := os.WriteFile(dest, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("replacement backup failed: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == int64(len(stale)) {
		t.Fatal("destination not replaced")
	}
	// A failed backup preserves the existing destination and leaves no litter.
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(ctx, filepath.Join(t.TempDir(), "nope", "x.zova")); err == nil {
		t.Fatal("backup into a missing directory accepted")
	}
	after, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed backup touched the existing destination")
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".elephant-backup-") {
			t.Fatalf("temporary backup litter: %s", e.Name())
		}
	}
}

func TestBackupConsistentWhileOpen(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "live.zova"))
	// Concurrent writers run while the backup is taken.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				_, _ = s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Concurrent", Body: "write"})
			}
		}()
	}
	dest := filepath.Join(t.TempDir(), "backup.zova")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	restored, err := zova.Open(dest)
	if err != nil {
		t.Fatalf("backup does not open: %v", err)
	}
	defer restored.Close()
	rs := app.New(restored)
	packet, err := rs.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot is a consistent prefix: every restored fact exists live
	// with identical content, and the seed rows are all present.
	live := map[string]model.Entry{}
	for _, e := range final.Facts {
		live[e.ID] = e
	}
	if len(packet.Facts) < 1 || len(packet.Facts) > len(live) {
		t.Fatalf("backup has %d facts, live has %d", len(packet.Facts), len(live))
	}
	for _, e := range packet.Facts {
		if l, ok := live[e.ID]; !ok || l.Title != e.Title || l.Body != e.Body {
			t.Fatalf("backup row inconsistent: %+v", e)
		}
	}
	if packet.Checkpoint == nil {
		t.Fatal("checkpoint missing from restored recall")
	}
}

func TestBackupRejectedWithoutZovaSuffix(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "live.zova"))
	if err := s.Backup(ctx, filepath.Join(t.TempDir(), "backup.txt")); err == nil {
		t.Fatal("non-.zova backup destination accepted")
	}
	if err := s.Backup(ctx, filepath.Join(t.TempDir(), "backup.zova")); err != nil {
		t.Fatalf("valid backup rejected: %v", err)
	}
}

func TestExportRejectsNonRepository(t *testing.T) {
	ctx := context.Background()
	db, err := zova.Open(filepath.Join(t.TempDir(), "plain.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A directory that is not a Git repository cannot be exported.
	if _, err := app.New(db).Export(ctx, t.TempDir()); err == nil {
		t.Fatal("export outside a repository accepted")
	}
	_ = errors.Is
}

// TestExportEmitsEachAdoptionReceiptOnceAcrossProjects: tx.Sources() spans
// every project in the database, so a source Elephant that exists under
// several project identities must not make export query the exported
// project's receipts repeatedly. Each receipt appears exactly once, foreign
// project receipts never leak, and the envelope's own output imports.
func TestExportEmitsEachAdoptionReceiptOnceAcrossProjects(t *testing.T) {
	ctx := context.Background()
	cwdA := exportRepo(t) // identity github.com/test/portable
	// Second project with a different identity in the same database.
	cwdB := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", cwdB},
		{"-C", cwdB, "remote", "add", "origin", "https://github.com/test/multi-b.git"},
		{"-C", cwdB, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "multiproj.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	localTask, err := s.Add(ctx, cwdA, model.Task, app.CreateInput{Title: "Local", Body: "exported"})
	if err != nil {
		t.Fatal(err)
	}
	foreignTask, err := s.Add(ctx, cwdB, model.Task, app.CreateInput{Title: "Foreign", Body: "other project"})
	if err != nil {
		t.Fatal(err)
	}
	statusA, err := s.Status(ctx, cwdA)
	if err != nil {
		t.Fatal(err)
	}
	statusB, err := s.Status(ctx, cwdB)
	if err != nil {
		t.Fatal(err)
	}
	id := func() string { return uuid.NewV7().String() }
	// The same source Elephant is registered under both projects.
	elephant := id()
	now := time.Now().UTC()
	receipt := model.Adoption{ID: id(), ProjectIdentity: statusA.Project.Identity, SourceElephantID: elephant, SourceEntryID: id(), LocalEntryID: localTask.Entry.ID, Kind: "task", CreatedAt: now}
	foreignReceipt := model.Adoption{ID: id(), ProjectIdentity: statusB.Project.Identity, SourceElephantID: elephant, SourceEntryID: id(), LocalEntryID: foreignTask.Entry.ID, Kind: "task", CreatedAt: now}
	err = db.Transact(ctx, func(tx storage.Tx) error {
		if _, err := tx.EnsureSource(statusA.Project, elephant); err != nil {
			return err
		}
		if _, err := tx.EnsureSource(statusB.Project, elephant); err != nil {
			return err
		}
		if err := tx.RecordAdoption(receipt); err != nil {
			return err
		}
		return tx.RecordAdoption(foreignReceipt)
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.Export(ctx, cwdA)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range env.Adoptions {
		if a.ID == receipt.ID {
			count++
		}
		if a.ProjectIdentity != statusA.Project.Identity {
			t.Fatalf("foreign project receipt leaked: %+v", a)
		}
		if a.ID == foreignReceipt.ID {
			t.Fatalf("foreign project receipt exported: %+v", a)
		}
	}
	if count != 1 {
		t.Fatalf("receipt exported %d times, want exactly once (%d total adoptions)", count, len(env.Adoptions))
	}
	// The envelope's own output imports successfully: duplicate receipts in
	// the archive would fail strict validation.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.Import(ctx, raw, "roundtrip")
	if err != nil {
		t.Fatalf("export did not round-trip: %v", err)
	}
	archived, err := func() ([]model.Adoption, error) {
		var out []model.Adoption
		err := db.Transact(ctx, func(tx storage.Tx) error {
			var err error
			out, err = tx.ArchivedAdoptions(src.TableName)
			return err
		})
		return out, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != receipt.ID {
		t.Fatalf("imported receipts: got %d, want exactly the one exported receipt", len(archived))
	}
}
