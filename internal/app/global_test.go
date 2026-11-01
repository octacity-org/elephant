package app_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"elephant/internal/app"
	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func TestGlobalMemorySharedAcrossProjects(t *testing.T) {
	ctx := context.Background()
	a, b, outside := exportRepo(t), repo(t), t.TempDir()
	db, err := zova.Open(filepath.Join(t.TempDir(), "global.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)

	// Before any global write, recall reports an empty global section and
	// read-only operations do not fail.
	empty, err := s.Recall(ctx, b, "")
	if err != nil || empty.Global == nil || len(empty.Global) != 0 {
		t.Fatalf("empty global: %#v %v", empty.Global, err)
	}
	if found, err := s.Search(ctx, b, app.SearchFilters{Global: true}); err != nil || len(found) != 0 {
		t.Fatalf("empty global search: %v %v", found, err)
	}

	pinned, err := s.Add(ctx, a, model.Fact, app.CreateInput{Title: "Builds use the external disk", Body: "The internal disk is nearly full", Tags: []string{"pinned", "build"}, Global: true})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Entry.StartCommit != nil {
		t.Fatalf("global entry recorded a commit: %v", *pinned.Entry.StartCommit)
	}
	origin := model.Relation{From: "entry:" + pinned.Entry.ID, Type: model.OriginEdge, To: model.ProjectNode("github.com/test/portable")}
	if !slices.Contains(pinned.Relations, origin) || !slices.Equal(pinned.Tags, []string{"build", "pinned"}) {
		t.Fatalf("record: %+v", pinned)
	}
	unpinned, err := s.Add(ctx, a, model.Decision, app.CreateInput{Title: "Prefer small PRs", Body: "Easier review", Global: true})
	if err != nil {
		t.Fatal(err)
	}

	// Global entries stay out of project sections and show, pinned only,
	// in every project's recall with their origin.
	for _, cwd := range []string{a, b} {
		r, err := s.Recall(ctx, cwd, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Facts)+len(r.Decisions) != 0 {
			t.Fatalf("global entries leaked into project recall: %+v %+v", r.Facts, r.Decisions)
		}
		if len(r.Global) != 1 || r.Global[0].Entry.ID != pinned.Entry.ID || r.Global[0].Origin != "github.com/test/portable" {
			t.Fatalf("recall global in %s: %+v", cwd, r.Global)
		}
	}

	// Search reaches global entries only with global set.
	if found, err := s.Search(ctx, b, app.SearchFilters{Query: "disk"}); err != nil || len(found) != 0 {
		t.Fatalf("project search found global: %v %v", found, err)
	}
	all, err := s.Search(ctx, b, app.SearchFilters{Global: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("global search: %v %v", all, err)
	}
	if found, err := s.Search(ctx, b, app.SearchFilters{Global: true, Tags: []string{"pinned"}}); err != nil || len(found) != 1 || found[0].Entry.ID != pinned.Entry.ID {
		t.Fatalf("global tag search: %v %v", found, err)
	}

	// Inspect and update find a global entry by ID from any project, and a
	// global entry never records commits.
	if got, err := s.Get(ctx, b, unpinned.Entry.ID); err != nil || got.Entry.Title != "Prefer small PRs" {
		t.Fatalf("inspect global: %+v %v", got, err)
	}
	retired, err := s.Update(ctx, b, model.Fact, pinned.Entry.ID, app.UpdateInput{Status: ptr("retired")})
	if err != nil || retired.Entry.Status != "retired" || retired.Entry.EndCommit != nil {
		t.Fatalf("retire global: %+v %v", retired.Entry, err)
	}
	if r, err := s.Recall(ctx, b, ""); err != nil || len(r.Global) != 0 {
		t.Fatalf("retired entry still pinned: %+v %v", r.Global, err)
	}
	replacement, err := s.Add(ctx, b, model.Decision, app.CreateInput{Title: "Prefer stacked PRs", Body: "Review in order", Supersedes: unpinned.Entry.ID, Global: true})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Get(ctx, a, unpinned.Entry.ID)
	if err != nil || old.Entry.Status != "superseded" || old.Entry.EndCommit != nil || replacement.Entry.StartCommit != nil {
		t.Fatalf("global supersede: %+v %+v %v", old.Entry, replacement.Entry, err)
	}

	// Project files, project entries, commits and remotes do not apply to
	// global memory.
	touchFiles(t, a, "build.sh")
	project, err := s.Add(ctx, a, model.Fact, app.CreateInput{Title: "Project", Body: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []app.CreateInput{
		{Title: "Bad", Body: "files", RelatedFiles: []string{"build.sh"}, Global: true},
		{Title: "Bad", Body: "relations", Relations: []app.RelationInput{{Type: "related_to", EntryID: project.Entry.ID}}, Global: true},
	} {
		if _, err := s.Add(ctx, a, model.Fact, in); !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("%s: %v", in.Body, err)
		}
	}
	if _, err := s.Update(ctx, a, model.Decision, replacement.Entry.ID, app.UpdateInput{RelatedFiles: &[]string{"build.sh"}}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("update global files: %v", err)
	}
	if _, err := s.Update(ctx, a, model.Decision, replacement.Entry.ID, app.UpdateInput{Relations: []app.RelationInput{{Type: "related_to", EntryID: project.Entry.ID}}}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("update global relations: %v", err)
	}
	for _, f := range []app.SearchFilters{{Global: true, File: "build.sh"}, {Global: true, Commit: "HEAD"}, {Global: true, Source: "peer"}} {
		if _, err := s.Search(ctx, a, f); !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("search %+v: %v", f, err)
		}
	}
	if _, err := s.BuildMessage(ctx, a, "entry.send", model.Fact, app.CreateInput{Title: "Global", Body: "send", Global: true}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("remote send of a global entry: %v", err)
	}

	// Outside Git only global memory is available, with no origin.
	if _, err := s.Add(ctx, outside, model.Fact, app.CreateInput{Title: "Project", Body: "outside"}); !errors.Is(err, gitrepo.ErrNotGitRepository) {
		t.Fatalf("project write outside Git: %v", err)
	}
	loose, err := s.Add(ctx, outside, model.Fact, app.CreateInput{Title: "Outside Git", Body: "still global", Global: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(loose.Relations, func(r model.Relation) bool { return r.Type == model.OriginEdge }) {
		t.Fatalf("origin recorded outside Git: %+v", loose.Relations)
	}
	if found, err := s.Search(ctx, outside, app.SearchFilters{Global: true, Query: "outside"}); err != nil || len(found) != 1 {
		t.Fatalf("global search outside Git: %v %v", found, err)
	}
	pins, truncated, err := s.Pinned(ctx)
	if err != nil || len(pins) != 0 || truncated {
		t.Fatalf("pinned: %+v %v %v", pins, truncated, err)
	}
}

func TestGlobalRecallIsBounded(t *testing.T) {
	ctx := context.Background()
	cwd := repo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "globalbound.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	for i := range app.RecallGlobalLimit + 1 {
		if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: fmt.Sprint("Pinned ", i), Body: "global", Tags: []string{"pinned"}, Global: true}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Recall(ctx, cwd, "")
	if err != nil || len(r.Global) != app.RecallGlobalLimit || !r.Truncated["global"] {
		t.Fatalf("bounded global: %d %v %v", len(r.Global), r.Truncated, err)
	}
	pins, truncated, err := s.Pinned(ctx)
	if err != nil || len(pins) != app.RecallGlobalLimit || !truncated {
		t.Fatalf("pinned: %d %v %v", len(pins), truncated, err)
	}
}
