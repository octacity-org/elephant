package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

// TestImportedAdoptionReceiptsStayArchived proves that an imported archive
// keeps its adoption receipts as archived data: they stay inspectable and
// countable on the archive, never enter the live adoption registry, and cannot
// poison or alter a later local adoption of the same remote entry.
func TestImportedAdoptionReceiptsStayArchived(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	origin := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "origin.zova"))
	raw := exportBytes(t, origin, cwd)
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Adoptions) != 1 {
		t.Fatalf("fixture exported %d adoption receipts, want 1", len(env.Adoptions))
	}
	receipt := env.Adoptions[0]

	db, err := zova.Open(filepath.Join(t.TempDir(), "receiver.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	archive, err := s.Import(ctx, raw, "archive")
	if err != nil {
		t.Fatal(err)
	}

	// Archived receipts remain inspectable and countable on the archive.
	inspection, err := s.InspectSource(ctx, cwd, archive.SourceElephantID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.AdoptionCount != len(env.Adoptions) {
		t.Fatalf("adoption_count = %d, want %d", inspection.AdoptionCount, len(env.Adoptions))
	}
	var stored *model.Adoption
	for i := range inspection.Adoptions {
		if inspection.Adoptions[i].ID == receipt.ID {
			stored = &inspection.Adoptions[i]
		}
	}
	if stored == nil {
		t.Fatalf("receipt %s not inspectable on the archive: %+v", receipt.ID, inspection.Adoptions)
	}
	if stored.SourceEntryID != receipt.SourceEntryID || stored.LocalEntryID != receipt.LocalEntryID || stored.SourceElephantID != receipt.SourceElephantID {
		t.Fatalf("archived receipt altered: got %+v, want %+v", *stored, receipt)
	}

	// The very peer named by the archived receipt sends that same entry again.
	now := time.Now().UTC()
	id := func() string { return uuid.NewV7().String() }
	remote := model.Entry{ID: receipt.SourceEntryID, Kind: model.Fact, Title: "Remote fact", Body: "from afar", ActorID: "peer", Status: "active", CreatedAt: now, UpdatedAt: now}
	if _, err = s.Receive(ctx, app.Message{
		ProtocolVersion: 1, MessageID: id(), SenderElephantID: receipt.SourceElephantID,
		Project: app.ProjectRef{Identity: env.Project.Identity, Name: env.Project.Name}, Operation: "entry.send", Entry: &remote,
	}); err != nil {
		t.Fatal(err)
	}

	// The archive's receipt must not register as a local adoption of the peer.
	inbox, err := s.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range inbox {
		if group.SourceElephantID != receipt.SourceElephantID {
			continue
		}
		if localID, adopted := group.Adopted[receipt.SourceEntryID]; adopted {
			t.Fatalf("import poisoned adoption state: entry %s reported adopted into %s", receipt.SourceEntryID, localID)
		}
	}

	before := localEntryCount(t, s, cwd)
	res, err := s.Adopt(ctx, cwd, receipt.SourceEntryID, receipt.SourceElephantID)
	if err != nil {
		t.Fatalf("import poisoned subsequent adoption: %v", err)
	}
	if res.AlreadyAdopted {
		t.Fatalf("imported receipt satisfied a local adoption: %+v", res)
	}
	if res.Entry.ID == receipt.LocalEntryID {
		t.Fatalf("adoption reused the archive's local entry %s", res.Entry.ID)
	}
	if after := localEntryCount(t, s, cwd); after != before+1 {
		t.Fatalf("local adoption created %d entries, want exactly 1 (before %d, after %d)", after-before, before, after)
	}

	// Repeating the adoption is idempotent against the new local receipt.
	again, err := s.Adopt(ctx, cwd, receipt.SourceEntryID, receipt.SourceElephantID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyAdopted || again.Entry.ID != res.Entry.ID {
		t.Fatalf("repeat adoption not idempotent: %+v", again)
	}
}

// TestImportRejectsDuplicateAdoptionReceipts proves strict validation rejects
// duplicate receipts before anything is committed, and that such failures leave
// neither partial state nor a poisoned adoption registry.
func TestImportRejectsDuplicateAdoptionReceipts(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	s := populatedService(t, ctx, cwd, filepath.Join(t.TempDir(), "dup.zova"))
	raw := exportBytes(t, s, cwd)
	var env app.ExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Adoptions) == 0 {
		t.Fatal("fixture exported no adoption receipts")
	}
	before := localEntryCount(t, s, cwd)

	duplicate := func(mutate func(*model.Adoption)) []byte {
		t.Helper()
		cp := env
		cp.Adoptions = append([]model.Adoption(nil), env.Adoptions...)
		dup := env.Adoptions[0]
		mutate(&dup)
		cp.Adoptions = append(cp.Adoptions, dup)
		out, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string][]byte{
		"identical receipt": duplicate(func(*model.Adoption) {}),
		"same source entry": duplicate(func(a *model.Adoption) {
			a.ID = uuid.NewV7().String()
		}),
		"duplicate receipt ID": duplicate(func(a *model.Adoption) {
			a.SourceEntryID = uuid.NewV7().String()
		}),
	}
	for name, data := range cases {
		if _, err := s.Import(ctx, data, "dups"); err == nil {
			t.Fatalf("%s: duplicate adoption receipts accepted", name)
		}
	}

	// Rejected imports left nothing behind: the clean archive still imports.
	if _, err := s.Import(ctx, raw, "dups"); err != nil {
		t.Fatalf("clean import after rejected duplicates: %v", err)
	}
	if after := localEntryCount(t, s, cwd); after != before {
		t.Fatalf("rejected imports changed local state: before %d, after %d", before, after)
	}
}
