package app

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func TestRemoteGlobalSendAndAdopt(t *testing.T) {
	ctx := context.Background()
	cwd, outside := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q", cwd}, {"-C", cwd, "remote", "add", "origin", "https://github.com/test/shared.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	open := func() *Service {
		db, err := zova.Open(filepath.Join(t.TempDir(), "elephant.zova"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return New(db)
	}
	a, b := open(), open()
	if _, err := a.AddRemote(ctx, "B", "ash", `{"host":"B"}`); err != nil {
		t.Fatal(err)
	}
	aid, err := a.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send := func(k model.Kind, in CreateInput) Message {
		t.Helper()
		in.Global = true
		m, err := a.BuildMessage(ctx, outside, "entry.send", k, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.Send(ctx, loopback{b}, "B", m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// A global entry is sent from outside Git, under the reserved global
	// identity and without commits.
	fact := send(model.Fact, CreateInput{Title: "Builds use the external disk", Body: "context", Tags: []string{"pinned", "host:mac"}})
	if fact.Project != (ProjectRef{model.GlobalIdentity, "global"}) || fact.Entry.StartCommit != nil {
		t.Fatalf("message: %+v %v", fact.Project, fact.Entry.StartCommit)
	}

	// The receiver keeps it in a source table for the sender's global memory,
	// out of its projects and its own global memory until adopted.
	inbox, err := b.Inbox(ctx)
	if err != nil || len(inbox) != 1 || inbox[0].ProjectIdentity != model.GlobalIdentity || len(inbox[0].Entries) != 1 {
		t.Fatalf("inbox: %+v %v", inbox, err)
	}
	if _, err := b.RecallRemote(ctx, cwd, aid); !errors.Is(err, model.ErrProjectNotFound) {
		t.Fatalf("project recall of the sender: %v", err)
	}
	received, err := b.RecallRemoteGlobal(ctx, outside, aid)
	if err != nil || len(received.Facts) != 1 || received.Facts[0].ID != fact.Entry.ID {
		t.Fatalf("global recall of the sender: %+v %v", received.Facts, err)
	}
	t.Setenv("ELEPHANT_HOST", "mac")
	if r, err := b.Recall(ctx, cwd, ""); err != nil || len(r.Facts)+len(r.Global) != 0 {
		t.Fatalf("received entry is local before adopt: %+v %+v %v", r.Facts, r.Global, err)
	}
	diff, err := b.DiffGlobal(ctx, outside, aid)
	if err != nil || len(diff.RemoteOnly) != 1 || diff.Source.Identity != model.GlobalIdentity {
		t.Fatalf("global diff: %+v %v", diff, err)
	}

	// Adopting makes it a local global entry with its tags, idempotently, and
	// only through the global scope.
	if _, err := b.Adopt(ctx, cwd, fact.Entry.ID, aid); err == nil {
		t.Fatal("project adopt took a global entry")
	}
	adopted, err := b.AdoptGlobal(ctx, outside, fact.Entry.ID, aid)
	if err != nil || len(adopted.Skipped) != 0 || adopted.SourceElephantID != aid || adopted.Entry.StartCommit != nil {
		t.Fatalf("adopt: %+v %v", adopted, err)
	}
	if again, err := b.AdoptGlobal(ctx, outside, fact.Entry.ID, aid); err != nil || !again.AlreadyAdopted || again.Entry.ID != adopted.Entry.ID {
		t.Fatalf("adopt again: %+v %v", again, err)
	}
	if global, err := b.isGlobal(ctx, adopted.Entry.ID); err != nil || !global {
		t.Fatalf("adopted entry is not global: %v %v", global, err)
	}
	r, err := b.Recall(ctx, cwd, "")
	if err != nil || len(r.Facts) != 0 || len(r.Global) != 1 || r.Global[0].Entry.ID != adopted.Entry.ID {
		t.Fatalf("recall after adopt: %+v %+v %v", r.Facts, r.Global, err)
	}
	if got, err := b.Get(ctx, cwd, adopted.Entry.ID); err != nil || !slices.Equal(got.Tags, []string{"host:mac", "pinned"}) {
		t.Fatalf("adopted tags: %q %v", got.Tags, err)
	}
	if diff, err := b.DiffGlobal(ctx, outside, aid); err != nil || diff.Adopted[fact.Entry.ID] != adopted.Entry.ID || len(diff.RemoteOnly) != 0 {
		t.Fatalf("global diff after adopt: %+v %v", diff, err)
	}

	// A sent replacement supersedes the adopted decision it replaces.
	old := send(model.Decision, CreateInput{Title: "Prefer small PRs", Body: "review"})
	replacement := send(model.Decision, CreateInput{Title: "Prefer stacked PRs", Body: "review in order", Supersedes: old.Entry.ID})
	oldLocal, err := b.AdoptGlobal(ctx, outside, old.Entry.ID, aid)
	if err != nil {
		t.Fatal(err)
	}
	if adopted, err := b.AdoptGlobal(ctx, outside, replacement.Entry.ID, aid); err != nil || len(adopted.Skipped) != 0 {
		t.Fatalf("adopt replacement: %+v %v", adopted, err)
	}
	if got, err := b.Get(ctx, cwd, oldLocal.Entry.ID); err != nil || got.Entry.Status != "superseded" || got.Entry.EndCommit != nil {
		t.Fatalf("superseded decision: %+v %v", got.Entry, err)
	}

	// A global source table holding file links or other relations, as an
	// imported archive can, adopts without them, even when the file exists
	// in the process directory.
	other := uuid.NewV7().String()
	now := time.Now().UTC()
	target := model.Entry{ID: uuid.NewV7().String(), Kind: model.Task, Title: "Target", Body: "task", Status: "open", ActorID: "peer", CreatedAt: now, UpdatedAt: now}
	linked := model.Entry{ID: uuid.NewV7().String(), Kind: model.Task, Title: "Linked", Body: "task", Status: "open", ActorID: "peer", CreatedAt: now, UpdatedAt: now}
	err = b.store.Transact(ctx, func(tx storage.Tx) error {
		src, err := tx.EnsureSource(model.Project{Identity: model.GlobalIdentity, Name: "global"}, other)
		if err != nil {
			return err
		}
		for _, e := range []model.Entry{target, linked} {
			if err = tx.Put(src, e); err != nil {
				return err
			}
		}
		if err = tx.Link(src, model.Relation{From: model.EntryNode(src, linked.ID), Type: model.FileEdge(model.Task), To: "file:" + src.ID + ":remote_global_test.go"}); err != nil {
			return err
		}
		return tx.Link(src, model.Relation{From: model.EntryNode(src, linked.ID), Type: "depends_on", To: model.EntryNode(src, target.ID)})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AdoptGlobal(ctx, outside, target.ID, other); err != nil {
		t.Fatal(err)
	}
	stripped, err := b.AdoptGlobal(ctx, outside, linked.ID, other)
	if err != nil || len(stripped.Skipped) != 2 || slices.ContainsFunc(stripped.Relations, func(r model.Relation) bool { return r.Type != model.TagEdge }) {
		t.Fatalf("adopt with project links: %+v %v", stripped, err)
	}

	// Global messages carry nothing tied to a repository.
	if _, err := a.BuildMessage(ctx, outside, "entry.send", model.Fact, CreateInput{Title: "T", Body: "B", Evidence: []EvidenceInput{{EntryID: other}}, Global: true}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("global send with evidence: %v", err)
	}
	commit := "abc123"
	for name, mutate := range map[string]func(*Message){
		"files":      func(m *Message) { m.Files = []string{"go.mod"} },
		"commit":     func(m *Message) { m.Entry.StartCommit = &commit },
		"end commit": func(m *Message) { m.Entry.EndCommit = &commit },
		"relation":   func(m *Message) { m.Relations = []RelationInput{{Type: "depends_on", EntryID: other}} },
		"evidence": func(m *Message) {
			m.Evidence = []model.Evidence{{ID: uuid.NewV7().String(), EntryID: m.Entry.ID, Path: "go.mod", CreatedAt: now, VerifiedAt: now}}
		},
	} {
		bad := fact
		entry := *fact.Entry
		bad.Entry = &entry
		mutate(&bad)
		if err := validateMessage(bad); err == nil || !strings.Contains(err.Error(), "global") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Peers from before global memory validate the identity as a repository
	// and so reject global messages instead of storing them in a project.
	if _, err := gitrepo.NormalizeRemote("https://" + model.GlobalIdentity); err == nil {
		t.Fatal("global identity parses as a repository")
	}
}
