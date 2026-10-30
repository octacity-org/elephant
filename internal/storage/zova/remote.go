package zova

import (
	"fmt"
	"strings"
	"time"
	"uuid"

	"elephant/internal/model"
)

func newID() string { return uuid.NewV7().String() }
func (t *transaction) migrate() error {
	if err := t.db.Exec(`CREATE TABLE project_tables(id TEXT PRIMARY KEY,project_identity TEXT NOT NULL,project_name TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,scope TEXT NOT NULL,source_elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_identity,source_elephant_id));
 CREATE TABLE remotes(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,backend TEXT NOT NULL,backend_config TEXT NOT NULL,elephant_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 CREATE TABLE received_messages(id TEXT PRIMARY KEY,digest TEXT NOT NULL);
 INSERT INTO elephant_meta(key,value) VALUES('elephant_id','` + newID() + `');`); err != nil {
		return err
	}
	rows, err := t.query("SELECT identity FROM projects")
	if err != nil {
		return err
	}
	for _, r := range rows {
		p, err := t.Project(value(r[0]))
		if err != nil {
			return err
		}
		name, err := table(p)
		if err != nil {
			return err
		}
		if err = t.db.Exec("ALTER TABLE " + name + " ADD COLUMN actor_id TEXT NOT NULL DEFAULT 'unknown'"); err != nil {
			return err
		}
		if err = t.registerLocal(p); err != nil {
			return err
		}
	}
	_, err = t.query("UPDATE elephant_meta SET value='2' WHERE key='schema_version'")
	if err != nil {
		return err
	}
	if err = t.migrateEvidence(); err != nil {
		return err
	}
	return t.migrateCheckpoints()
}

// migrateEvidence upgrades schema 2 by adding the per-project evidence table.
// It is idempotent and transactional with the version stamp.
func (t *transaction) migrateEvidence() error {
	rows, err := t.query("SELECT identity FROM projects")
	if err != nil {
		return err
	}
	for _, r := range rows {
		p, err := t.Project(value(r[0]))
		if err != nil {
			return err
		}
		name, err := table(p)
		if err != nil {
			return err
		}
		if err = t.createEvidenceTable(name); err != nil {
			return err
		}
	}
	_, err = t.query("UPDATE elephant_meta SET value='3' WHERE key='schema_version'")
	return err
}
func (t *transaction) Identity() (string, error) {
	rows, err := t.query("SELECT value FROM elephant_meta WHERE key='elephant_id'")
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", model.ErrSchema
	}
	return value(rows[0][0]), nil
}
func (t *transaction) registerLocal(p model.Project) error {
	self, err := t.Identity()
	if err != nil {
		return err
	}
	_, err = t.query("INSERT INTO project_tables VALUES(?,?,?,?,?,?,?,?)", newID(), p.Identity, p.Name, p.TableName, "local", self, stamp(p.CreatedAt), stamp(p.UpdatedAt))
	return err
}
func (t *transaction) Source(identity, source string) (model.Project, error) {
	rows, err := t.query("SELECT id,project_name,table_name,scope,created_at,updated_at FROM project_tables WHERE project_identity=? AND source_elephant_id=?", identity, source)
	if err != nil {
		return model.Project{}, err
	}
	if len(rows) == 0 {
		return model.Project{}, model.ErrProjectNotFound
	}
	r := rows[0]
	c, err := parseTime(r[4])
	if err != nil {
		return model.Project{}, err
	}
	u, err := parseTime(r[5])
	return model.Project{ID: value(r[0]), Identity: identity, Name: value(r[1]), TableName: value(r[2]), Scope: value(r[3]), SourceElephantID: source, CreatedAt: c, UpdatedAt: u}, err
}
func (t *transaction) EnsureSource(p model.Project, source string) (model.Project, error) {
	existing, err := t.Source(p.Identity, source)
	if err == nil {
		return existing, nil
	}
	if err != model.ErrProjectNotFound {
		return p, err
	}
	self, err := t.Identity()
	if err != nil {
		return p, err
	}
	if self == source {
		return p, fmt.Errorf("%w: cannot receive own state", model.ErrInvalidInput)
	}
	p.ID = newID()
	p.TableName = "p_" + strings.ReplaceAll(p.ID, "-", "")
	p.Scope = "remote"
	p.SourceElephantID = source
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	_, err = t.query("INSERT INTO project_tables VALUES(?,?,?,?,?,?,?,?)", p.ID, p.Identity, p.Name, p.TableName, p.Scope, source, stamp(p.CreatedAt), stamp(p.UpdatedAt))
	if err != nil {
		return p, err
	}
	err = t.createEntryTable(p)
	return p, err
}

// Message records a digest in the caller's transaction, so failed mutations roll it back.
func (t *transaction) Message(id, digest, namespace string) (bool, error) {
	key := namespace + ":" + id
	rows, err := t.query("SELECT digest FROM received_messages WHERE id=?", key)
	if err != nil {
		return false, err
	}
	if len(rows) > 0 {
		if value(rows[0][0]) != digest {
			return false, fmt.Errorf("%w: conflicting reuse of ID", model.ErrInvalidInput)
		}
		return true, nil
	}
	_, err = t.query("INSERT INTO received_messages VALUES(?,?)", key, digest)
	return false, err
}
func (t *transaction) Remotes() ([]model.Remote, error) {
	rows, err := t.query("SELECT id,name,backend,backend_config,elephant_id,created_at,updated_at FROM remotes ORDER BY name")
	if err != nil {
		return nil, err
	}
	out := []model.Remote{}
	for _, r := range rows {
		c, err := parseTime(r[5])
		if err != nil {
			return nil, err
		}
		u, err := parseTime(r[6])
		if err != nil {
			return nil, err
		}
		out = append(out, model.Remote{ID: value(r[0]), Name: value(r[1]), Backend: value(r[2]), BackendConfig: value(r[3]), ElephantID: value(r[4]), CreatedAt: c, UpdatedAt: u})
	}
	return out, nil
}
func (t *transaction) SaveRemote(r model.Remote) error {
	_, err := t.query("INSERT INTO remotes VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET elephant_id=excluded.elephant_id,updated_at=excluded.updated_at", r.ID, r.Name, r.Backend, r.BackendConfig, r.ElephantID, stamp(r.CreatedAt), stamp(r.UpdatedAt))
	return err
}
func (t *transaction) RemoveRemote(name string) error {
	_, err := t.query("DELETE FROM remotes WHERE name=?", name)
	return err
}
