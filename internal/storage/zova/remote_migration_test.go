package zova

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"uuid"

	"elephant/internal/model"
	"elephant/internal/storage"
	native "github.com/ata-sesli/zova/bindings/go"
)

func TestSchemaOneMigrationPreservesLocalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.zova")
	db, err := native.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Construct the previous schema directly; using Store would silently create
	// the new schema and fail to exercise an actual upgrade.
	err = db.Exec(`CREATE TABLE elephant_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE projects(id TEXT PRIMARY KEY,identity TEXT NOT NULL UNIQUE,name TEXT NOT NULL,remote TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
INSERT INTO elephant_meta VALUES('schema_version','1');
INSERT INTO projects VALUES('prj_legacy','example.org/team/repo','repo','https://example.org/team/repo.git','p_0123456789abcdef0123456789abcdef','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');
INSERT INTO projects VALUES('prj_empty','example.org/team/empty','empty','','p_abcdef0123456789abcdef0123456789','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, tableName := range []string{"p_0123456789abcdef0123456789abcdef", "p_abcdef0123456789abcdef0123456789"} {
		err = db.Exec("CREATE TABLE " + tableName + ` (id TEXT PRIMARY KEY,kind TEXT NOT NULL CHECK(kind IN ('fact','decision','task')),title TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL,target_version TEXT,start_commit TEXT,end_commit TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX ` + tableName + `_kind_status ON ` + tableName + `(kind,status,updated_at DESC,id DESC);
CREATE INDEX ` + tableName + `_created ON ` + tableName + `(created_at);
CREATE INDEX ` + tableName + `_version ON ` + tableName + `(target_version);`)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	err = db.Exec(`INSERT INTO p_0123456789abcdef0123456789abcdef VALUES
('legacy-fact','fact','A saved fact','Existing context','active',NULL,NULL,NULL,'2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z'),
('legacy-decision','decision','A saved decision','Existing rationale','active','v1','abc123',NULL,'2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z'),
('legacy-task','task','A completed task','Existing work','done','v1','abc123','def456','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');`)
	if err == nil {
		err = db.CreateGraph("elephant")
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	namespace, projectID, file := "p_0123456789abcdef0123456789abcdef", "prj_legacy", "src/main.go"
	for _, item := range []struct{ id, kind string }{{"legacy-fact", "fact"}, {"legacy-decision", "decision"}, {"legacy-task", "task"}} {
		if err := db.PutGraphNode(native.GraphNodeInput{GraphName: "elephant", NodeID: "entry:" + item.id, Kind: item.kind, TargetType: native.GraphTargetRecord, TargetNamespace: &namespace, TargetRef: &item.id}); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.PutGraphNode(native.GraphNodeInput{GraphName: "elephant", NodeID: "file:prj_legacy:src/main.go", Kind: "file", TargetType: native.GraphTargetExternal, TargetNamespace: &projectID, TargetRef: &file}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	edges := []model.Relation{{From: "entry:legacy-fact", Type: "concerns", To: "file:prj_legacy:src/main.go"}, {From: "entry:legacy-task", Type: "depends_on", To: "entry:legacy-decision"}}
	for _, edge := range edges {
		if err := db.PutGraphEdge(native.GraphEdgeInput{GraphName: "elephant", FromNodeID: edge.From, EdgeType: edge.Type, ToNodeID: edge.To}); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	updated := created.Add(24 * time.Hour)
	version, start, end := "v1", "abc123", "def456"
	expected := []model.Entry{
		{ID: "legacy-fact", Kind: model.Fact, Title: "A saved fact", Body: "Existing context", Status: "active", ActorID: "unknown", CreatedAt: created, UpdatedAt: updated},
		{ID: "legacy-decision", Kind: model.Decision, Title: "A saved decision", Body: "Existing rationale", Status: "active", ActorID: "unknown", TargetVersion: &version, StartCommit: &start, CreatedAt: created, UpdatedAt: updated},
		{ID: "legacy-task", Kind: model.Task, Title: "A completed task", Body: "Existing work", Status: "done", ActorID: "unknown", TargetVersion: &version, StartCommit: &start, EndCommit: &end, CreatedAt: created, UpdatedAt: updated},
	}
	var identity string
	var tableIDs []string
	for attempt := range 2 {
		if !t.Run(fmt.Sprintf("open_%d", attempt), func(t *testing.T) {
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			err = s.Transact(context.Background(), func(tx storage.Tx) error {
				self, err := tx.Identity()
				if err != nil {
					return err
				}
				id, err := uuid.Parse(self)
				if err != nil || id[6]>>4 != 7 {
					return fmt.Errorf("invalid installation identity %q", self)
				}
				if attempt == 0 {
					identity = self
				} else if self != identity {
					return fmt.Errorf("identity changed after reopen")
				}
				p, err := tx.Project("example.org/team/repo")
				if err != nil {
					return err
				}
				if p.ID != projectID || p.TableName != namespace || p.Scope != "local" || p.SourceElephantID != self || !p.CreatedAt.Equal(created) || !p.UpdatedAt.Equal(updated) {
					return fmt.Errorf("project changed: %+v", p)
				}
				for _, want := range expected {
					got, err := tx.Get(p, want.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, want) {
						return fmt.Errorf("entry changed: got %+v, want %+v", got, want)
					}
				}
				for _, edge := range edges {
					got, err := tx.Relations(edge.From)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, []model.Relation{edge}) {
						return fmt.Errorf("graph changed: %+v", got)
					}
				}
				for i, project := range []struct{ identity, table string }{{"example.org/team/empty", "p_abcdef0123456789abcdef0123456789"}, {p.Identity, p.TableName}} {
					local, err := tx.Source(project.identity, self)
					if err != nil {
						return err
					}
					id, err := uuid.Parse(local.ID)
					if err != nil || id[6]>>4 != 7 || local.Scope != "local" || local.TableName != project.table || local.SourceElephantID != self {
						return fmt.Errorf("invalid local metadata: %+v", local)
					}
					if attempt == 0 {
						tableIDs = append(tableIDs, local.ID)
					} else if tableIDs[i] != local.ID {
						return fmt.Errorf("local metadata recreated")
					}
				}
				rows, err := tx.(*transaction).query("SELECT value FROM elephant_meta WHERE key='schema_version'")
				if err != nil {
					return err
				}
				if len(rows) != 1 || value(rows[0][0]) != "5" {
					return fmt.Errorf("migration did not persist schema 5")
				}
				rows, err = tx.(*transaction).query("SELECT id FROM project_tables")
				if err != nil {
					return err
				}
				if len(rows) != 2 {
					return fmt.Errorf("unexpected project table count: %d", len(rows))
				}
				for _, tableName := range []string{"p_0123456789abcdef0123456789abcdef", "p_abcdef0123456789abcdef0123456789"} {
					rows, err = tx.(*transaction).query("SELECT id FROM " + tableName + "_evidence")
					if err != nil {
						return fmt.Errorf("migration did not create the evidence table: %w", err)
					}
					if len(rows) != 0 {
						return fmt.Errorf("unexpected evidence rows after migration: %d", len(rows))
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}) {
			return
		}
	}
}
