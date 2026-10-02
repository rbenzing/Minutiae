package evidence

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// migrations[i] upgrades schema version i to i+1. Append only; never edit.
var migrations = [][]string{
	{ // v1
		`CREATE TABLE artifacts (
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			size INTEGER,
			sha256 TEXT,
			md5 TEXT,
			device_id TEXT,
			source TEXT,
			incomplete INTEGER NOT NULL,
			created TEXT NOT NULL)`,
		`CREATE TABLE records (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL,
			artifact_id TEXT REFERENCES artifacts(id),
			source_path TEXT,
			offset INTEGER,
			length INTEGER,
			deleted INTEGER NOT NULL DEFAULT 0,
			timestamp TEXT,
			data TEXT NOT NULL)`,
		`CREATE INDEX records_type ON records(type)`,
		`CREATE INDEX records_ts ON records(timestamp)`,
	},
}

// Store is the case's artifacts.db.
type Store struct{ db *sql.DB }

// OpenStore opens or creates the database and applies pending migrations.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open artifacts.db: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifacts.db pragma: %w", err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("artifacts.db schema_version: %w", err)
	}
	v, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("artifacts.db schema version %d is newer than this build supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range migrations[i] {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("artifacts.db migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied schema version (0 if none).
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// InsertArtifact records a finished (or aborted) artifact.
func (s *Store) InsertArtifact(r ManifestRecord) error {
	src, err := json.Marshal(r.Source)
	if err != nil {
		return err
	}
	incomplete := 0
	if r.Incomplete {
		incomplete = 1
	}
	_, err = s.db.Exec(`INSERT INTO artifacts (id, path, size, sha256, md5, device_id, source, incomplete, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Path, r.Size, r.SHA256, r.MD5, r.Source.DeviceID, string(src), incomplete, r.Finished)
	if err != nil {
		return fmt.Errorf("artifacts.db insert %s: %w", r.ID, err)
	}
	return nil
}

// ArtifactHashes maps artifact id to sha256.
func (s *Store) ArtifactHashes() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, sha256 FROM artifacts`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		out[id] = sum
	}
	return out, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
