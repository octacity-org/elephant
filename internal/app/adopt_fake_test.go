package app_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"elephant/internal/model"
	"elephant/internal/storage"
)

// fakeState is the full mutable state of a fakeStore. Transactions clone it
// copy-on-write and swap it in only on success, so a failed transaction leaves
// no partial writes — mirroring the real store's rollback.
type fakeState struct {
	projects map[string]model.Project          // local: identity; remote: identity+"\x00"+sender
	entries  map[string]map[string]model.Entry // table -> id -> entry
	links    map[string][]model.Relation       // from node -> outgoing relations
	receipts map[string]model.Adoption         // identity+"\x00"+source+"\x00"+entryID -> receipt
	archived map[string][]model.Adoption       // archive table -> receipts
	messages map[string]bool
}

func newFakeState() *fakeState {
	return &fakeState{projects: map[string]model.Project{}, entries: map[string]map[string]model.Entry{}, links: map[string][]model.Relation{}, receipts: map[string]model.Adoption{}, archived: map[string][]model.Adoption{}, messages: map[string]bool{}}
}

func (s *fakeState) clone() *fakeState {
	c := newFakeState()
	for k, v := range s.projects {
		c.projects[k] = v
	}
	for k, m := range s.entries {
		c.entries[k] = map[string]model.Entry{}
		for id, e := range m {
			c.entries[k][id] = e
		}
	}
	for k, rs := range s.links {
		c.links[k] = append([]model.Relation(nil), rs...)
	}
	for k, v := range s.receipts {
		c.receipts[k] = v
	}
	for k, rs := range s.archived {
		c.archived[k] = append([]model.Adoption(nil), rs...)
	}
	for k, v := range s.messages {
		c.messages[k] = v
	}
	return c
}

// fakeStore is a minimal in-memory storage.Store for adopt tests. When
// failLinkPrefix is nonempty, every Link whose target starts with that prefix
// fails, simulating a storage error mid-adoption.
type fakeStore struct {
	mu             sync.Mutex
	state          *fakeState
	failLinkPrefix string
}

func newFakeStore() *fakeStore { return &fakeStore{state: newFakeState()} }

func (f *fakeStore) Transact(ctx context.Context, fn func(storage.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	snap := f.state.clone()
	tx := &fakeTx{st: snap, failLinkPrefix: f.failLinkPrefix}
	if err := fn(tx); err != nil {
		return err
	}
	f.state = snap
	return nil
}

type fakeTx struct {
	st             *fakeState
	failLinkPrefix string
}

func (t *fakeTx) Identity() (string, error) { return "elf_fake_self", nil }

func (t *fakeTx) EnsureSource(p model.Project, sender string) (model.Project, error) {
	key := p.Identity + "\x00" + sender
	if src, ok := t.st.projects[key]; ok {
		return src, nil
	}
	id := fmt.Sprintf("%d", len(t.st.projects)+1)
	src := model.Project{ID: "prj_" + id, Identity: p.Identity, Name: p.Name, TableName: "p_src_" + id, Scope: "remote", SourceElephantID: sender}
	t.st.projects[key] = src
	t.st.entries[src.TableName] = map[string]model.Entry{}
	return src, nil
}

func (t *fakeTx) Source(identity, sender string) (model.Project, error) {
	if src, ok := t.st.projects[identity+"\x00"+sender]; ok {
		return src, nil
	}
	return model.Project{}, model.ErrEntryNotFound
}

func (t *fakeTx) Message(id, digest, kind string) (bool, error) {
	if t.st.messages[id] {
		return true, nil
	}
	t.st.messages[id] = true
	return false, nil
}

func (t *fakeTx) Remotes() ([]model.Remote, error) { return nil, nil }
func (t *fakeTx) SaveRemote(r model.Remote) error  { return nil }
func (t *fakeTx) RemoveRemote(name string) error   { return nil }

func (t *fakeTx) Project(identity string) (model.Project, error) {
	if p, ok := t.st.projects[identity]; ok {
		return p, nil
	}
	return model.Project{}, model.ErrProjectNotFound
}

func (t *fakeTx) CreateProject(p model.Project) error {
	t.st.projects[p.Identity] = p
	t.st.entries[p.TableName] = map[string]model.Entry{}
	return nil
}

func (t *fakeTx) Get(p model.Project, id string) (model.Entry, error) {
	m, ok := t.st.entries[p.TableName]
	if !ok {
		return model.Entry{}, model.ErrEntryNotFound
	}
	e, ok := m[id]
	if !ok {
		return model.Entry{}, model.ErrEntryNotFound
	}
	return e, nil
}

func (t *fakeTx) Put(p model.Project, e model.Entry) error {
	if t.st.entries[p.TableName] == nil {
		t.st.entries[p.TableName] = map[string]model.Entry{}
	}
	t.st.entries[p.TableName][e.ID] = e
	return nil
}

func (t *fakeTx) List(p model.Project, f model.Filter) ([]model.Entry, error) {
	m := t.st.entries[p.TableName]
	out := make([]model.Entry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (t *fakeTx) Link(p model.Project, r model.Relation) error {
	if t.failLinkPrefix != "" && strings.HasPrefix(r.To, t.failLinkPrefix) {
		return fmt.Errorf("%w: injected link failure for %q", model.ErrStorage, r.To)
	}
	t.st.links[r.From] = append(t.st.links[r.From], r)
	return nil
}

func (t *fakeTx) Unlink(r model.Relation) error {
	rs := t.st.links[r.From]
	for i, x := range rs {
		if x.Type == r.Type && x.To == r.To {
			t.st.links[r.From] = append(rs[:i:i], rs[i+1:]...)
			return nil
		}
	}
	return model.ErrRelationNotFound
}

func (t *fakeTx) Relations(nodeID string) ([]model.Relation, error) {
	return append([]model.Relation(nil), t.st.links[nodeID]...), nil
}

func (t *fakeTx) Incoming(nodeID string) ([]model.Relation, error) {
	out := []model.Relation{}
	for from, links := range t.st.links {
		for _, r := range links {
			if r.To == nodeID {
				out = append(out, model.Relation{From: from, Type: r.Type, To: r.To})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].From < out[j].From
	})
	return out, nil
}

// Search mirrors the real store's deterministic text retrieval over the fake
// tables: equality filters plus literal title/body substring, newest first
// with ID DESC tie-break, bounded by limit.
func (t *fakeTx) Search(p model.Project, q model.SearchQuery) ([]model.Entry, error) {
	if q.Kind != "" && q.Kind != model.Fact && q.Kind != model.Decision && q.Kind != model.Task {
		return nil, model.ErrInvalidKind
	}
	if q.Status != "" && !model.ValidStatus(q.Kind, q.Status) {
		return nil, model.ErrInvalidStatus
	}
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if q.Offset < 0 {
		return nil, model.ErrInvalidInput
	}
	terms, err := model.SearchTerms(strings.ToLower(q.Query))
	if err != nil {
		return nil, err
	}
	m := t.st.entries[p.TableName]
	out := make([]model.Entry, 0, len(m))
	for _, e := range m {
		if q.Kind != "" && e.Kind != q.Kind {
			continue
		}
		if q.Status != "" && e.Status != q.Status {
			continue
		}
		if q.Actor != "" && e.ActorID != q.Actor {
			continue
		}
		if q.TargetVersion != "" && (e.TargetVersion == nil || *e.TargetVersion != q.TargetVersion) {
			continue
		}
		if q.UpdatedAfter != nil && e.UpdatedAt.Before(*q.UpdatedAfter) {
			continue
		}
		if q.UpdatedBefore != nil && e.UpdatedAt.After(*q.UpdatedBefore) {
			continue
		}
		if slices.ContainsFunc(terms, func(term string) bool {
			return !strings.Contains(strings.ToLower(e.Title), term) && !strings.Contains(strings.ToLower(e.Body), term)
		}) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if q.Offset >= len(out) {
		return []model.Entry{}, nil
	}
	out = out[q.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (t *fakeTx) PutEvidence(model.Project, model.Evidence) error { return nil }
func (t *fakeTx) Evidence(model.Project, string) ([]model.Evidence, error) {
	return nil, nil
}
func (t *fakeTx) EvidenceByID(model.Project, string) (model.Evidence, error) {
	return model.Evidence{}, model.ErrEntryNotFound
}
func (t *fakeTx) DeleteEvidence(model.Project, string) error { return nil }
func (t *fakeTx) PutCheckpoint(model.Project, model.Checkpoint) error {
	return nil
}
func (t *fakeTx) GetCheckpoint(model.Project, string) (model.Checkpoint, error) {
	return model.Checkpoint{}, model.ErrEntryNotFound
}
func (t *fakeTx) ListCheckpoints(model.Project, int, int) ([]model.Checkpoint, error) {
	return nil, nil
}

func (t *fakeTx) Sources() ([]model.Project, error) {
	out := []model.Project{}
	for _, p := range t.st.projects {
		if p.Scope == "remote" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (t *fakeTx) RecordAdoption(a model.Adoption) error {
	t.st.receipts[a.ProjectIdentity+"\x00"+a.SourceElephantID+"\x00"+a.SourceEntryID] = a
	return nil
}

func (t *fakeTx) Adoption(identity, source, entryID string) (model.Adoption, error) {
	if a, ok := t.st.receipts[identity+"\x00"+source+"\x00"+entryID]; ok {
		return a, nil
	}
	return model.Adoption{}, model.ErrEntryNotFound
}

func (t *fakeTx) AdoptionsBySource(identity, source string) ([]model.Adoption, error) {
	out := []model.Adoption{}
	for _, a := range t.st.receipts {
		if a.ProjectIdentity == identity && a.SourceElephantID == source {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceEntryID < out[j].SourceEntryID })
	return out, nil
}

func (t *fakeTx) RecordArchivedAdoption(archiveTable string, a model.Adoption) error {
	if archiveTable == "" {
		return fmt.Errorf("%w: archived adoption requires an archive table", model.ErrInvalidInput)
	}
	for _, existing := range t.st.archived[archiveTable] {
		if existing.ID == a.ID || (existing.SourceElephantID == a.SourceElephantID && existing.SourceEntryID == a.SourceEntryID) {
			return fmt.Errorf("%w: duplicate archived adoption receipt", model.ErrStorage)
		}
	}
	t.st.archived[archiveTable] = append(t.st.archived[archiveTable], a)
	return nil
}

func (t *fakeTx) ArchivedAdoptions(archiveTable string) ([]model.Adoption, error) {
	return append([]model.Adoption(nil), t.st.archived[archiveTable]...), nil
}

func (f *fakeStore) Backup(string) error { return nil }
