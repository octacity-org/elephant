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

// craftSchemaTwo builds a schema-2 database directly through the native API:
// version stamp, installation identity, one project with an actor_id column,
// ownership registry, and graph. Using the native API (never Store) keeps the
// fixture at version 2 so Open must perform the evidence migration.
func craftSchemaTwo(t *testing.T, path string) {
	t.Helper()
	db, err := native.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	elephantID := uuid.NewV7().String()
	table := "p_0123456789abcdef0123456789abcdef"
	err = db.Exec(`CREATE TABLE elephant_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE projects(id TEXT PRIMARY KEY,identity TEXT NOT NULL UNIQUE,name TEXT NOT NULL,remote TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE project_tables(id TEXT PRIMARY KEY,project_identity TEXT NOT NULL,project_name TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,scope TEXT NOT NULL,source_elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_identity,source_elephant_id));
CREATE TABLE remotes(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,backend TEXT NOT NULL,backend_config TEXT NOT NULL,elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE received_messages(id TEXT PRIMARY KEY,digest TEXT NOT NULL);
INSERT INTO elephant_meta VALUES('schema_version','2');
INSERT INTO elephant_meta VALUES('elephant_id','` + elephantID + `');
INSERT INTO projects VALUES('prj_v2','example.org/team/v2','v2','https://example.org/team/v2.git','` + table + `','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');
INSERT INTO project_tables VALUES('` + uuid.NewV7().String() + `','example.org/team/v2','v2','` + table + `','local','` + elephantID + `','2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("CREATE TABLE " + table + ` (id TEXT PRIMARY KEY,kind TEXT NOT NULL CHECK(kind IN ('fact','decision','task')),title TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL,target_version TEXT,start_commit TEXT,end_commit TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,actor_id TEXT NOT NULL);
CREATE INDEX ` + table + `_kind_status ON ` + table + `(kind,status,updated_at DESC,id DESC);`); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`INSERT INTO ` + table + ` VALUES('v2-fact','fact','Kept fact','Kept context','active',NULL,NULL,NULL,'2026-09-01T12:00:00.000000000Z','2026-09-02T12:00:00.000000000Z','tester');`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateGraph("elephant"); err != nil {
		t.Fatal(err)
	}
}

func schemaVersion(t *testing.T, path string) string {
	t.Helper()
	db, err := native.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db}
	var version string
	if err = s.Transact(context.Background(), func(tx storage.Tx) error {
		rows, err := tx.(*transaction).query("SELECT value FROM elephant_meta WHERE key='schema_version'")
		if err != nil || len(rows) != 1 {
			return err
		}
		version = value(rows[0][0])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestSchemaTwoEvidenceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.zova")
	craftSchemaTwo(t, path)
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
			rows, err = tx.(*transaction).query("SELECT id FROM p_0123456789abcdef0123456789abcdef_evidence")
			if err != nil {
				return errors.New("migration did not create the evidence table")
			}
			if len(rows) != 0 {
				return errors.New("unexpected evidence rows after migration")
			}
			p, err := tx.Project("example.org/team/v2")
			if err != nil {
				return err
			}
			entry, err := tx.Get(p, "v2-fact")
			if err != nil {
				return err
			}
			if entry.Title != "Kept fact" || entry.ActorID != "tester" {
				return errors.New("migration changed the entry")
			}
			return nil
		})
		s.Close()
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if got := schemaVersion(t, path); got != "5" {
		t.Fatalf("version=%q", got)
	}
}

func TestEvidenceMigrationRollbackLeavesV2Readable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.zova")
	craftSchemaTwo(t, path)
	// Fault-inject a failure midway through the evidence migration through
	// the same transactional boundary Open uses.
	raw, err := native.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: raw}
	err = s.Transact(context.Background(), func(tx storage.Tx) error {
		if err := tx.(*transaction).createEvidenceTable("p_0123456789abcdef0123456789abcdef"); err != nil {
			return err
		}
		return errors.New("injected migration failure")
	})
	if err == nil || err.Error() != "injected migration failure" {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	raw.Close()
	if got := schemaVersion(t, path); got != "2" {
		t.Fatalf("failed migration changed the version to %q", got)
	}
	// The rolled-back database still opens and migrates cleanly.
	opened, err := Open(path)
	if err != nil {
		t.Fatalf("rolled-back database no longer opens: %v", err)
	}
	opened.Close()
	if got := schemaVersion(t, path); got != "5" {
		t.Fatalf("recovery migration reached %q", got)
	}
}
