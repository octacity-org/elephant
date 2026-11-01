package app_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func TestTagsAddSearchUpdateAndList(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "tags.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	touchFiles(t, cwd, "build.sh")
	tagged, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Builds use the external disk", Body: "Internal disk is nearly full", RelatedFiles: []string{"build.sh"}, Tags: []string{"Build", "disk", "build"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tagged.Tags, []string{"build", "disk"}) {
		t.Fatalf("tags = %q", tagged.Tags)
	}
	plain, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Builds are reproducible", Body: "Pinned toolchain"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Tags == nil || len(plain.Tags) != 0 {
		t.Fatalf("untagged entry tags = %#v", plain.Tags)
	}
	got, err := s.Get(ctx, cwd, tagged.Entry.ID)
	if err != nil || !slices.Equal(got.Tags, []string{"build", "disk"}) {
		t.Fatalf("inspect: %q %v", got.Tags, err)
	}

	// Invalid tags write nothing.
	before := localEntryCount(t, s, cwd)
	tooMany := make([]string, model.MaxTags+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint("t", i)
	}
	for _, bad := range [][]string{{"two words"}, tooMany} {
		if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Bad", Body: "tags", Tags: bad}); !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if after := localEntryCount(t, s, cwd); after != before {
		t.Fatalf("rejected adds wrote entries: %d -> %d", before, after)
	}
	// Remote messages carry no tags, so sending tags is an error, not a drop.
	if _, err := s.BuildMessage(ctx, cwd, "entry.send", model.Fact, app.CreateInput{Title: "Sent", Body: "fact", Tags: []string{"build"}}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("remote send with tags: %v", err)
	}

	search := func(tags ...string) []app.SearchResult {
		t.Helper()
		res, err := s.Search(ctx, cwd, app.SearchFilters{Query: "builds", Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := search(); len(res) != 2 {
		t.Fatalf("untagged search: %+v", res)
	}
	for _, tags := range [][]string{{"build"}, {"BUILD", "disk"}} {
		res := search(tags...)
		if len(res) != 1 || res[0].Entry.ID != tagged.Entry.ID || !slices.Equal(res[0].Tags, []string{"build", "disk"}) || !slices.Contains(res[0].Match, "tag") {
			t.Fatalf("%q: %+v", tags, res)
		}
	}
	if res := search("build", "other"); len(res) != 0 {
		t.Fatalf("every tag must match: %+v", res)
	}
	if _, err := s.Search(ctx, cwd, app.SearchFilters{Tags: []string{"bad tag"}}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("invalid search tag: %v", err)
	}

	counts, err := s.Tags(ctx, cwd)
	if err != nil || len(counts) != 2 || counts[0] != (app.TagCount{Tag: "build", Entries: 1}) || counts[1] != (app.TagCount{Tag: "disk", Entries: 1}) {
		t.Fatalf("tags: %+v %v", counts, err)
	}

	// Update replaces the tag set and leaves file links alone.
	replacement := []string{"disk", "macos"}
	updated, err := s.Update(ctx, cwd, model.Fact, tagged.Entry.ID, app.UpdateInput{Tags: &replacement})
	if err != nil || !slices.Equal(updated.Tags, []string{"disk", "macos"}) {
		t.Fatalf("update: %q %v", updated.Tags, err)
	}
	files := 0
	for _, r := range updated.Relations {
		if strings.HasPrefix(r.To, "file:") {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("file links changed: %+v", updated.Relations)
	}
	title := "Builds use /Volumes/Build"
	if updated, err = s.Update(ctx, cwd, model.Fact, tagged.Entry.ID, app.UpdateInput{Title: &title}); err != nil || !slices.Equal(updated.Tags, []string{"disk", "macos"}) {
		t.Fatalf("update without tags changed them: %q %v", updated.Tags, err)
	}
	none := []string{}
	if updated, err = s.Update(ctx, cwd, model.Fact, tagged.Entry.ID, app.UpdateInput{Tags: &none}); err != nil || len(updated.Tags) != 0 {
		t.Fatalf("clear: %q %v", updated.Tags, err)
	}
	if counts, err = s.Tags(ctx, cwd); err != nil || len(counts) != 0 {
		t.Fatalf("tags after clear: %+v %v", counts, err)
	}
}

// Tags are shared graph nodes, so counts and filters must stay inside the
// project that owns the entry.
func TestTagsStayInTheirProject(t *testing.T) {
	ctx := context.Background()
	cwdA, cwdB := exportRepo(t), otherExportRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "scoped.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	if _, err = s.Add(ctx, cwdA, model.Fact, app.CreateInput{Title: "A fact", Body: "in A", Tags: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Add(ctx, cwdB, model.Fact, app.CreateInput{Title: "B fact", Body: "in B", Tags: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	counts, err := s.Tags(ctx, cwdA)
	if err != nil || len(counts) != 1 || counts[0].Entries != 1 {
		t.Fatalf("%+v %v", counts, err)
	}
	res, err := s.Search(ctx, cwdA, app.SearchFilters{Tags: []string{"shared"}})
	if err != nil || len(res) != 1 || res[0].Entry.Title != "A fact" {
		t.Fatalf("%+v %v", res, err)
	}
}

// Export carries tag edges, import validates them and keeps them on the
// archive, and review does not mistake them for dangling relations.
func TestTagsExportImportAdoptReview(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	originDB, err := zova.Open(filepath.Join(t.TempDir(), "origin.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer originDB.Close()
	origin := app.New(originDB)
	fact, err := origin.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Tagged fact", Body: "travels", Tags: []string{"zova", "host:a"}})
	if err != nil {
		t.Fatal(err)
	}
	findings, err := origin.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Code == app.CodeDanglingRelation {
			t.Fatalf("tag edge reported as dangling: %+v", f)
		}
	}
	raw := exportBytes(t, origin, cwd)
	if !strings.Contains(string(raw), `"to":"tag:host:a"`) {
		t.Fatalf("export lacks tag edges: %s", raw)
	}
	bad := strings.Replace(string(raw), `"to":"tag:zova"`, `"to":"tag:Bad Tag"`, 1)
	if _, err = origin.Import(ctx, []byte(bad), "bad"); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("invalid tag imported: %v", err)
	}
	wrongEdge := strings.Replace(string(raw), `"type":"tagged","to":"tag:zova"`, `"type":"concerns","to":"tag:zova"`, 1)
	if wrongEdge == string(raw) {
		t.Fatalf("fixture edge not found: %s", raw)
	}
	if _, err = origin.Import(ctx, []byte(wrongEdge), "bad"); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("non-tag edge to a tag imported: %v", err)
	}

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
	recalled, err := s.RecallSource(ctx, app.ProjectRef{Identity: archive.Identity}, archive.SourceElephantID, "")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range recalled.Relations {
		if name, ok := model.TagFromNode(r.To); ok && r.Type == model.TagEdge && strings.HasSuffix(r.From, fact.Entry.ID) {
			tags = append(tags, name)
		}
	}
	slices.Sort(tags)
	if !slices.Equal(tags, []string{"host:a", "zova"}) {
		t.Fatalf("archive tags = %q in %+v", tags, recalled.Relations)
	}
}
