package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func TestRemoteReceiveIsolationAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "elephant.zova")
	db, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	self, err := s.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := func() string { return uuid.NewV7().String() }
	m := Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: id(), Project: ProjectRef{Identity: "github.com/test/repo", Name: "repo"}, Operation: "entry.send", Entry: &model.Entry{ID: id(), Kind: model.Fact, Title: "remote fact", Body: "context", Status: "active", ActorID: "actor-one", CreatedAt: now, UpdatedAt: now}, Files: []string{"src/main.go"}}
	first, err := s.Receive(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Receive(ctx, m)
	if err != nil || first.Table.ID != second.Table.ID {
		t.Fatalf("retry: %v", err)
	}
	m.MessageID = id()
	if _, err = s.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Entry.Body = "conflict"
	if _, err = s.Receive(ctx, m); err == nil {
		t.Fatal("expected conflict")
	}
	m.Entry.Body = "context"
	result, err := s.RecallSource(ctx, m.Project, m.SenderElephantID, "")
	if err != nil || len(result.Facts) != 1 || result.Facts[0].ActorID != "actor-one" || len(result.Relations) != 1 {
		t.Fatalf("recall: %+v %v", result, err)
	}
	m.MessageID = id()
	m.Entry.ID = id()
	m.Files = []string{"../../etc/passwd"}
	if _, err = s.Receive(ctx, m); err == nil {
		t.Fatal("invalid path accepted")
	}
	m.Files = nil
	m.SenderElephantID = id()
	m.MessageID = id()
	m.Entry.ActorID = "actor-two"
	if _, err = s.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	result, err = s.RecallSource(ctx, m.Project, m.SenderElephantID, "")
	if err != nil || len(result.Facts) != 1 {
		t.Fatal(result, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	again, err := New(db).Identity(ctx)
	if err != nil || self != again {
		t.Fatal("identity changed", err)
	}
}

func TestRemoteRegistryAndTransport(t *testing.T) {
	ctx := context.Background()
	db, err := zova.Open(filepath.Join(t.TempDir(), "registry.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	r, err := s.AddRemote(ctx, "fedora", "ash", `{"host":"fedora"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddRemote(ctx, "fedora", "ash", `{"host":"fedora"}`); err == nil {
		t.Fatal("duplicate name")
	}
	if _, err = s.AddRemote(ctx, "other", "unknown", `{}`); err == nil {
		t.Fatal("unknown backend")
	}
	rs, err := s.ListRemotes(ctx)
	if err != nil || len(rs) != 1 || rs[0].ID != r.ID {
		t.Fatal(rs, err)
	}
	if err = s.RemoveRemote(ctx, "fedora"); err != nil {
		t.Fatal(err)
	}
}

type loopback struct{ target *Service }

func (b loopback) Exchange(ctx context.Context, r model.Remote, payload []byte) ([]byte, error) {
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	response, err := b.target.Receive(ctx, m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}
func TestTwoElephantsKeepLocalTruth(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
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
	a, b, c := open(), open(), open()
	t.Setenv("ELEPHANT_ACTOR_ID", "codex-one")
	local, err := b.Add(ctx, cwd, model.Fact, CreateInput{Title: "B local", Body: "keep local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.AddRemote(ctx, "B", "ash", `{"host":"B"}`); err != nil {
		t.Fatal(err)
	}
	m, err := a.BuildMessage(ctx, cwd, "project.ensure", "", CreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	ensured, err := a.Send(ctx, loopback{b}, "B", m)
	if err != nil {
		t.Fatal(err)
	}
	m, err = a.BuildMessage(ctx, cwd, "entry.send", model.Task, CreateInput{Title: "A task", Body: "context", RelatedFiles: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := a.Send(ctx, loopback{b}, "B", m)
	if err != nil || sent.EntryID != m.Entry.ID {
		t.Fatal(sent, err)
	}
	if _, err = a.Send(ctx, loopback{b}, "B", m); err != nil {
		t.Fatal(err)
	}
	originalID := m.Entry.ID
	m.MessageID = uuid.NewV7().String()
	m.Entry.ID = uuid.NewV7().String()
	m.Entry.ActorID = "human-two"
	m.Relations = []RelationInput{{Type: "depends_on", EntryID: originalID}}
	if _, err = a.Send(ctx, loopback{b}, "B", m); err != nil {
		t.Fatal(err)
	}
	aid, _ := a.Identity(ctx)
	received, err := b.RecallRemote(ctx, cwd, aid)
	if err != nil || len(received.Tasks) != 2 || len(received.Relations) != 3 {
		t.Fatal(received, err)
	}
	if ensured.Table.ID != received.Project.ID {
		t.Fatal("table changed")
	}
	// Reusing an entry UUID in another source cannot overwrite its graph nodes.
	cid, _ := c.Identity(ctx)
	m.MessageID = uuid.NewV7().String()
	m.SenderElephantID = cid
	m.Entry.ID = originalID
	m.Files = []string{"c.go"}
	m.Relations = nil
	if _, err = b.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	other, err := b.RecallRemote(ctx, cwd, cid)
	if err != nil || len(other.Tasks) != 1 || len(other.Relations) != 1 {
		t.Fatal(other, err)
	}
	m.MessageID = uuid.NewV7().String()
	m.Entry.ID = uuid.NewV7().String()
	m.Relations = []RelationInput{{Type: "depends_on", EntryID: received.Tasks[0].ID}}
	// Pick the A-only second task, regardless of sort order.
	for _, e := range received.Tasks {
		if e.ID != originalID {
			m.Relations[0].EntryID = e.ID
		}
	}
	if _, err = b.Receive(ctx, m); err == nil {
		t.Fatal("cross-source relation accepted")
	}
	other, err = b.RecallRemote(ctx, cwd, cid)
	if err != nil || len(other.Tasks) != 1 {
		t.Fatal("failed receive left row", other, err)
	}
	own, err := b.Recall(ctx, cwd, "")
	if err != nil || len(own.Facts) != 1 || own.Facts[0].ID != local.Entry.ID || len(own.Tasks) != 0 || own.Project.Scope != "local" {
		t.Fatal(own, err)
	}
}
func TestMalformedRemoteMessages(t *testing.T) {
	id := func() string { return uuid.NewV7().String() }
	now := time.Now().UTC()
	base := Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: id(), Project: ProjectRef{"github.com/test/repo", "repo"}, Operation: "entry.send", Entry: &model.Entry{ID: id(), Kind: model.Fact, Title: "title", Body: "body", Status: "active", ActorID: "actor", CreatedAt: now, UpdatedAt: now}}
	for name, mutate := range map[string]func(*Message){"version": func(m *Message) { m.ProtocolVersion = 2 }, "sender": func(m *Message) { m.SenderElephantID = "bad" }, "entry": func(m *Message) { m.Entry.ID = "bad" }, "kind": func(m *Message) { m.Entry.Kind = "bad" }, "status": func(m *Message) { m.Entry.Status = "done" }, "actor": func(m *Message) { m.Entry.ActorID = "" }, "path": func(m *Message) { m.Files = []string{"/etc/passwd"} }, "traversal": func(m *Message) { m.Files = []string{"a/../b"} }, "local project": func(m *Message) { m.Project.Identity = "local:/tmp/repo" }, "relation": func(m *Message) { m.Relations = []RelationInput{{Type: "evil", EntryID: id()}} }} {
		t.Run(name, func(t *testing.T) {
			m := base
			e := *base.Entry
			m.Entry = &e
			mutate(&m)
			if validateMessage(m) == nil {
				t.Fatal("accepted invalid message")
			}
		})
	}
}

func TestRejectMalformedProjectIdentity(t *testing.T) {
	for _, identity := range []string{"", "../project", "C:\\repo", "repo", "github.com/../repo", "user:secret@github.com/repo"} {
		m := Message{ProtocolVersion: 1, MessageID: uuid.NewV7().String(), SenderElephantID: uuid.NewV7().String(), Project: ProjectRef{Identity: identity, Name: "repo"}, Operation: "project.ensure"}
		if validateMessage(m) == nil {
			t.Errorf("accepted %q", identity)
		}
	}
}

type failedBackend struct{}

func (failedBackend) Exchange(context.Context, model.Remote, []byte) ([]byte, error) {
	return nil, fmt.Errorf("ASH unavailable")
}
func TestSendReportsRemoteFailure(t *testing.T) {
	db, err := zova.Open(filepath.Join(t.TempDir(), "failure.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	ctx := context.Background()
	if _, err = s.AddRemote(ctx, "peer", "ash", `{"host":"peer"}`); err != nil {
		t.Fatal(err)
	}
	self, _ := s.Identity(ctx)
	m := Message{ProtocolVersion: 1, MessageID: uuid.NewV7().String(), SenderElephantID: self, Project: ProjectRef{Identity: "github.com/test/repo", Name: "repo"}, Operation: "project.ensure"}
	if _, err = s.Send(ctx, failedBackend{}, "peer", m); !errors.Is(err, model.ErrRemote) {
		t.Fatalf("missing remote error classification: %v", err)
	}
}
