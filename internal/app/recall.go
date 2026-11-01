package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

type RecallResult struct {
	Project          model.Project     `json:"project"`
	Git              gitrepo.Metadata  `json:"git"`
	Checkpoint       *CheckpointRecord `json:"latest_checkpoint,omitempty"`
	Tasks            []model.Entry     `json:"tasks"`
	Decisions        []model.Entry     `json:"decisions"`
	Facts            []model.Entry     `json:"facts"`
	Evidence         []EvidenceView    `json:"evidence"`
	RecentCompleted  []model.Entry     `json:"recent_completed_tasks"`
	RecentSuperseded []model.Entry     `json:"recent_superseded_decisions"`
	Relations        []model.Relation  `json:"relations"`
	ChangedFiles     []ChangedFile     `json:"changed_files"`
	Staleness        []Staleness       `json:"staleness"`
	Global           []GlobalEntry     `json:"global"`
	Truncated        map[string]bool   `json:"truncated"`
}

// GlobalEntry is a pinned global entry with the identity of the project it
// was written from, empty when it was written outside Git.
type GlobalEntry struct {
	Entry  model.Entry `json:"entry"`
	Origin string      `json:"origin,omitempty"`
}

// Staleness compares the latest checkpoint or an unfinished entry with the
// current HEAD. Commit is the end commit when one is recorded, otherwise the
// start commit. History is "current" when Commit is HEAD, "behind" when it is
// an older ancestor of HEAD, "unknown" when it is not reachable from HEAD
// (another branch, rewritten history, or no HEAD yet), and "unrecorded" when
// no commit was recorded. CommitsBehind is set only for current and behind.
// ChangedFiles lists linked files whose content differs between Commit and
// HEAD.
type Staleness struct {
	Node          string   `json:"node"`
	AgeDays       int      `json:"age_days"`
	Commit        string   `json:"commit,omitempty"`
	History       string   `json:"history"`
	CommitsBehind *int     `json:"commits_behind"`
	ChangedFiles  []string `json:"changed_files"`
}

// ChangedFile pairs a file with uncommitted changes in the working tree with
// the entries linked to it through related files, in any lifecycle state.
type ChangedFile struct {
	Path    string        `json:"path"`
	Entries []model.Entry `json:"entries"`
}

// RecallGlobalLimit bounds the pinned global entries in one recall, and
// recallGlobalScan bounds the global entries read to find them.
const (
	RecallGlobalLimit = 20
	recallGlobalScan  = 1000
)

// recallEvidenceLimit bounds verification work in one recall.
const recallEvidenceLimit = 50

// Changed-file bounds: paths scanned, files reported, and entries per file.
const (
	recallChangedScanLimit  = 200
	recallChangedFileLimit  = 20
	recallChangedEntryLimit = 10
)

// RecallStaleCommitLimit bounds the distinct recorded commits compared with
// HEAD in one recall; recallStaleFileLimit bounds changed files per item.
const (
	RecallStaleCommitLimit = 20
	recallStaleFileLimit   = 20
)

func (s *Service) Recall(ctx context.Context, cwd, version string) (out RecallResult, err error) {
	return s.recallWith(ctx, version, true, func(fn func(storage.Tx, model.Project, gitrepo.Metadata) error) error { return s.within(ctx, cwd, fn) })
}

// RecallExisting recalls the repository's project only when it is already
// known, reporting found=false otherwise. Unlike Recall it never creates a
// project record, so it can run in any repository, as session hooks do.
func (s *Service) RecallExisting(ctx context.Context, cwd string) (out RecallResult, found bool, err error) {
	out, err = s.recallWith(ctx, "", true, func(fn func(storage.Tx, model.Project, gitrepo.Metadata) error) error {
		g, err := gitrepo.Inspect(ctx, cwd)
		if err != nil {
			return err
		}
		return s.store.Transact(ctx, func(tx storage.Tx) error {
			p, err := tx.Project(g.Identity)
			if errors.Is(err, model.ErrProjectNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			return fn(tx, p, g)
		})
	})
	return
}

// recallWith fills the pinned global entries only when global is set: a
// remote source's recall carries none of this Elephant's global memory.
func (s *Service) recallWith(ctx context.Context, version string, global bool, within func(func(storage.Tx, model.Project, gitrepo.Metadata) error) error) (out RecallResult, err error) {
	out = RecallResult{Tasks: []model.Entry{}, Decisions: []model.Entry{}, Facts: []model.Entry{}, Evidence: []EvidenceView{}, RecentCompleted: []model.Entry{}, RecentSuperseded: []model.Entry{}, Relations: []model.Relation{}, ChangedFiles: []ChangedFile{}, Staleness: []Staleness{}, Global: []GlobalEntry{}, Truncated: map[string]bool{}}
	err = within(func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		out.Project = p
		out.Git = g
		for _, group := range []struct {
			key    string
			kind   model.Kind
			status string
			limit  int
			dest   *[]model.Entry
		}{
			{"active_tasks", model.Task, "active", 50, &out.Tasks}, {"blocked_tasks", model.Task, "blocked", 50, &out.Tasks}, {"open_tasks", model.Task, "open", 50, &out.Tasks},
			{"decisions", model.Decision, "active", 50, &out.Decisions}, {"facts", model.Fact, "active", 50, &out.Facts},
			{"recent_completed_tasks", model.Task, "done", 10, &out.RecentCompleted}, {"recent_superseded_decisions", model.Decision, "superseded", 5, &out.RecentSuperseded},
		} {
			entries, err := tx.List(p, model.Filter{Kind: group.kind, Status: group.status, TargetVersion: version, Limit: group.limit + 1})
			if err != nil {
				return err
			}
			if len(entries) > group.limit {
				out.Truncated[group.key] = true
				entries = entries[:group.limit]
			}
			*group.dest = append(*group.dest, entries...)
			for _, e := range entries {
				links, err := tx.Relations(model.EntryNode(p, e.ID))
				if err != nil {
					return err
				}
				out.Relations = append(out.Relations, links...)
			}
		}
		for _, fact := range out.Facts {
			stored, err := tx.Evidence(p, fact.ID)
			if err != nil {
				return err
			}
			for _, evidence := range stored {
				if len(out.Evidence) >= recallEvidenceLimit {
					out.Truncated["evidence"] = true
					break
				}
				view, err := verifyEvidence(ctx, g.Root, evidence)
				if err != nil {
					return err
				}
				out.Evidence = append(out.Evidence, view)
			}
			if out.Truncated["evidence"] {
				break
			}
		}
		// Checkpoints are local-only: remote source recall must not leak
		// local session boundaries. Rows are additionally scoped by project
		// table in storage.
		if p.Scope == "local" {
			latest, err := tx.ListCheckpoints(p, 1, 0)
			if err != nil {
				return err
			}
			if len(latest) == 1 {
				links, err := tx.Relations(model.CheckpointNode(p.TableName, latest[0].ID))
				if err != nil {
					return err
				}
				out.Checkpoint = &CheckpointRecord{Checkpoint: latest[0], Relations: links}
			}
			if out.ChangedFiles, err = changedFileEntries(tx, p, g.ChangedFiles, out.Truncated); err != nil {
				return err
			}
			if out.Staleness, err = recallStaleness(ctx, p, g, out); err != nil {
				return err
			}
		}
		if global {
			var truncated bool
			if out.Global, truncated, err = pinnedGlobal(tx); err != nil {
				return err
			}
			if truncated {
				out.Truncated["global"] = true
			}
		}
		return nil
	})
	return
}

// Pinned reads the pinned global entries without creating anything, for
// callers outside Git or in a project Elephant does not know.
func (s *Service) Pinned(ctx context.Context) (out []GlobalEntry, truncated bool, err error) {
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		var err error
		out, truncated, err = pinnedGlobal(tx)
		return err
	})
	return
}

// pinnedGlobal lists unfinished global entries tagged pinned, newest first.
// Tags live in the graph, so it reads at most recallGlobalScan entries.
func pinnedGlobal(tx storage.Tx) (out []GlobalEntry, truncated bool, err error) {
	out = []GlobalEntry{}
	p, found, err := globalProject(tx, false)
	if err != nil || !found {
		return out, false, err
	}
	const page = 200
	for offset := 0; ; offset += page {
		entries, err := tx.List(p, model.Filter{Limit: page, Offset: offset})
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if model.Terminal(e.Kind, e.Status) {
				continue
			}
			links, err := tx.Relations(model.EntryNode(p, e.ID))
			if err != nil {
				return nil, false, err
			}
			if !slices.Contains(tagsOf(links), model.PinnedTag) {
				continue
			}
			if len(out) == RecallGlobalLimit {
				return out, true, nil
			}
			entry := GlobalEntry{Entry: e}
			for _, r := range links {
				if identity, ok := model.ProjectFromNode(r.To); ok && r.Type == model.OriginEdge {
					entry.Origin = identity
				}
			}
			out = append(out, entry)
		}
		if len(entries) < page {
			return out, false, nil
		}
		if offset+page >= recallGlobalScan {
			return out, true, nil
		}
	}
}

// recallStaleness compares the latest checkpoint and the unfinished entries,
// in recall order, with HEAD. Each distinct recorded commit costs at most two
// Git calls, so past RecallStaleCommitLimit commits the remaining items are
// left out and the result is marked truncated.
func recallStaleness(ctx context.Context, p model.Project, g gitrepo.Metadata, r RecallResult) ([]Staleness, error) {
	type item struct {
		node       string
		updated    time.Time
		start, end *string
	}
	var items []item
	files := map[string][]string{}
	addFiles := func(relations []model.Relation) {
		for _, rel := range relations {
			if path, ok := strings.CutPrefix(rel.To, "file:"+p.ID+":"); ok {
				files[rel.From] = append(files[rel.From], path)
			}
		}
	}
	if r.Checkpoint != nil {
		c := r.Checkpoint.Checkpoint
		items = append(items, item{model.CheckpointNode(p.TableName, c.ID), c.UpdatedAt, c.StartCommit, c.EndCommit})
		addFiles(r.Checkpoint.Relations)
	}
	for _, group := range [][]model.Entry{r.Tasks, r.Decisions, r.Facts} {
		for _, e := range group {
			items = append(items, item{model.EntryNode(p, e.ID), e.UpdatedAt, e.StartCommit, e.EndCommit})
		}
	}
	addFiles(r.Relations)

	type comparison struct {
		behind    int
		reachable bool
		changed   map[string]bool
	}
	compared := map[string]*comparison{}
	now := time.Now().UTC()
	out := []Staleness{}
	for _, it := range items {
		st := Staleness{Node: it.node, AgeDays: max(0, int(now.Sub(it.updated)/(24*time.Hour))), History: "unrecorded", ChangedFiles: []string{}}
		commit := it.end
		if commit == nil || *commit == "" {
			commit = it.start
		}
		if commit != nil && *commit != "" {
			st.Commit, st.History = *commit, "unknown"
		}
		if st.Commit != "" && g.Head != "" {
			c, ok := compared[st.Commit]
			if !ok {
				if len(compared) == RecallStaleCommitLimit {
					r.Truncated["staleness"] = true
					continue
				}
				behind, reachable, err := gitrepo.Behind(ctx, g.Root, st.Commit, g.Head)
				if err != nil {
					return nil, err
				}
				c = &comparison{behind: behind, reachable: reachable}
				compared[st.Commit] = c
			}
			if c.reachable {
				st.History = "current"
				if c.behind > 0 {
					st.History = "behind"
				}
				st.CommitsBehind = &c.behind
				if c.behind > 0 && len(files[it.node]) > 0 {
					if c.changed == nil {
						paths, err := gitrepo.ChangedBetween(ctx, g.Root, st.Commit, g.Head)
						if err != nil {
							return nil, err
						}
						c.changed = map[string]bool{}
						for _, path := range paths {
							c.changed[path] = true
						}
					}
					for _, path := range files[it.node] {
						if c.changed[path] {
							st.ChangedFiles = append(st.ChangedFiles, path)
						}
					}
					slices.Sort(st.ChangedFiles)
					if len(st.ChangedFiles) > recallStaleFileLimit {
						r.Truncated["staleness"] = true
						st.ChangedFiles = st.ChangedFiles[:recallStaleFileLimit]
					}
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// changedFileEntries reports, in path order, the working-tree changes that
// have linked memory. Files without linked entries are omitted.
func changedFileEntries(tx storage.Tx, p model.Project, paths []string, truncated map[string]bool) ([]ChangedFile, error) {
	out := []ChangedFile{}
	if len(paths) > recallChangedScanLimit {
		truncated["changed_files"] = true
		paths = paths[:recallChangedScanLimit]
	}
	for _, path := range paths {
		clean, err := model.CleanFile(path)
		if err != nil {
			continue
		}
		entries, err := fileEntries(tx, p, clean)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		if len(out) == recallChangedFileLimit {
			truncated["changed_files"] = true
			break
		}
		if len(entries) > recallChangedEntryLimit {
			truncated["changed_files"] = true
			entries = entries[:recallChangedEntryLimit]
		}
		out = append(out, ChangedFile{Path: clean, Entries: entries})
	}
	return out, nil
}
