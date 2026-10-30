package zova

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"uuid"

	native "github.com/ata-sesli/zova/bindings/go"

	"elephant/internal/storage"
)

// craftSchemaThree builds a schema-3 database with one legacy
// identity-scoped checkpoints row, exercising the v4 upgrade path.
func craftSchemaThree(t *testing.T, path string) {
	t.Helper()
	db, err := native.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	elephantID := uuid.NewV7().String()
	table := "p_0123456789abcdef0123456789abcdef"
	if err = db.Exec(`CREATE TABLE elephant_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE projects(id TEXT PRIMARY KEY,identity TEXT NOT NULL UNIQUE,name TEXT NOT NULL,remote TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE project_tables(id TEXT PRIMARY KEY,project_identity TEXT NOT NULL,project_name TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,scope TEXT NOT NULL,source_elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_identity,source_elephant_id));
CREATE TABLE remotes(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,backend TEXT NOT NULL,backend_config TEXT NOT NULL,elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE received_messages(id TEXT PRIMARY KEY,digest TEXT NOT NULL);
INSERT INTO elephant_meta VALUES('schema_version','3');
INSERT INTO elephant_meta VALUES('elephant_id','` + elephantID + `');
INSERT INTO projects VALUES('prj_v3','example.org/team/v3','v3','https://example.org/team/v3.git','` + table + `','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');
INSERT INTO project_tables VALUES('` + uuid.NewV7().String() + `','example.org/team/v3','v3','` + table + `','local','` + elephantID + `','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("CREATE TABLE " + table + ` (id TEXT PRIMARY KEY,kind TEXT NOT NULL CHECK(kind IN ('fact','decision','task')),title TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL,target_version TEXT,start_commit TEXT,end_commit TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,actor_id TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`CREATE TABLE ` + table + `_evidence(id TEXT PRIMARY KEY,entry_id TEXT NOT NULL,path TEXT NOT NULL,line INTEGER,commit_hash TEXT,blob TEXT,created_at TEXT NOT NULL,verified_at TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`CREATE TABLE checkpoints(id TEXT PRIMARY KEY,project_identity TEXT NOT NULL,summary TEXT NOT NULL,completed TEXT NOT NULL,next TEXT NOT NULL,commands TEXT NOT NULL,failures TEXT NOT NULL,actor_id TEXT NOT NULL,start_commit TEXT,end_commit TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
INSERT INTO checkpoints VALUES('legacy-cp','example.org/team/v3','Old summary','Did work','Next steps','','','tester',NULL,NULL,'2026-09-03T12:00:00.000000000Z','2026-09-03T12:00:00.000000000Z');
INSERT INTO checkpoints VALUES('orphan-cp','example.org/team/gone','Orphan','x','','','','tester',NULL,NULL,'2026-09-03T12:00:00.000000000Z','2026-09-03T12:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateGraph("elephant"); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaThreeCheckpointMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.zova")
	craftSchemaThree(t, path)
	for attempt := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Transact(context.Background(), func(tx storage.Tx) error {
			rows, err := tx.(*transaction).query("SELECT value FROM elephant_meta WHERE key='schema_version'")
			if err != nil || len(rows) != 1 || value(rows[0][0]) != "5" {
				return errors.New("migration did not persist schema 5")
			}
			p, err := tx.Project("example.org/team/v3")
			if err != nil {
				return err
			}
			got, err := tx.ListCheckpoints(p, 10, 0)
			if err != nil {
				return err
			}
			if len(got) != 1 || got[0].ID != "legacy-cp" || got[0].Summary != "Old summary" {
				return errors.New("legacy checkpoint was not migrated")
			}
			if len(got[0].Completed) != 1 || got[0].Completed[0] != "Did work" || len(got[0].Next) != 1 || len(got[0].Commands) != 0 {
				return errors.New("legacy text was not wrapped as lists")
			}
			return nil
		})
		s.Close()
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
}

func TestCheckpointMigrationRollbackLeavesV3Readable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp-rollback.zova")
	craftSchemaThree(t, path)
	raw, err := native.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: raw}
	err = s.Transact(context.Background(), func(tx storage.Tx) error {
		if err := tx.(*transaction).migrateCheckpoints(); err != nil {
			return err
		}
		return errors.New("injected migration failure")
	})
	if err == nil || err.Error() != "injected migration failure" {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	raw.Close()
	if got := schemaVersion(t, path); got != "3" {
		t.Fatalf("failed migration changed the version to %q", got)
	}
	opened, err := Open(path)
	if err != nil {
		t.Fatalf("rolled-back database no longer opens: %v", err)
	}
	opened.Close()
	if got := schemaVersion(t, path); got != "5" {
		t.Fatalf("recovery migration reached %q", got)
	}
}
