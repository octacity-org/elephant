package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"uuid"

	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func TestRemoteSendCarriesTags(t *testing.T) {
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
	a, b := open(), open()
	if _, err := a.AddRemote(ctx, "B", "ash", `{"host":"B"}`); err != nil {
		t.Fatal(err)
	}
	aid, err := a.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Tags are normalized on the sender and carried with the entry.
	m, err := a.BuildMessage(ctx, cwd, "entry.send", model.Fact, CreateInput{Title: "Builds use the external disk", Body: "context", Tags: []string{"Build", "host:Mac", "build"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"build", "host:mac"}; !slices.Equal(m.Tags, want) {
		t.Fatalf("message tags = %q", m.Tags)
	}
	if _, err = a.Send(ctx, loopback{b}, "B", m); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Send(ctx, loopback{b}, "B", m); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// Resending the entry with other tags is a conflict, not an update.
	retagged := m
	retagged.MessageID, retagged.Tags = uuid.NewV7().String(), []string{"build"}
	if _, err = a.Send(ctx, loopback{b}, "B", retagged); err == nil {
		t.Fatal("entry resent with other tags was accepted")
	}
	found, err := b.Search(ctx, cwd, SearchFilters{Source: aid, Tags: []string{"build"}})
	if err != nil || len(found) != 1 || !slices.Equal(found[0].Tags, []string{"build", "host:mac"}) {
		t.Fatalf("received tags: %+v %v", found, err)
	}

	// Adopting copies the tags onto the local entry.
	adopted, err := b.Adopt(ctx, cwd, m.Entry.ID, aid)
	if err != nil || len(adopted.Skipped) != 0 {
		t.Fatalf("adopt: %+v %v", adopted, err)
	}
	local, err := b.Get(ctx, cwd, adopted.Entry.ID)
	if err != nil || !slices.Equal(local.Tags, []string{"build", "host:mac"}) {
		t.Fatalf("adopted tags: %q %v", local.Tags, err)
	}

	// A message without tags has no tags field, so older peers accept it.
	plain, err := a.BuildMessage(ctx, cwd, "entry.send", model.Fact, CreateInput{Title: "Plain", Body: "context"})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), `"tags"`) {
		t.Fatalf("tagless message carries a tags field: %s", raw)
	}

	// The receiver accepts only canonical tags on entry messages, within the
	// relation bound.
	files := make([]string, 95)
	for i := range files {
		files[i] = fmt.Sprint("f", i, ".go")
	}
	many := make([]string, 6)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	for name, mutate := range map[string]func(*Message){
		"uppercase":  func(m *Message) { m.Tags = []string{"Build"} },
		"unsorted":   func(m *Message) { m.Tags = []string{"host:mac", "build"} },
		"duplicate":  func(m *Message) { m.Tags = []string{"build", "build"} },
		"invalid":    func(m *Message) { m.Tags = []string{"two words"} },
		"over bound": func(m *Message) { m.Files, m.Tags = files, many },
		"ensure": func(m *Message) {
			m.Operation, m.Entry, m.Tags = "project.ensure", nil, []string{"build"}
		},
	} {
		bad := m
		entry := *m.Entry
		bad.Entry = &entry
		mutate(&bad)
		if err := validateMessage(bad); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
