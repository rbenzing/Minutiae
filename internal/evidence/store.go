package evidence

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// CurrentSchema is the artifacts.db schema version this build creates and reads.
const CurrentSchema = 2

// migration is one schema upgrade step, applied in a single transaction.
type migration struct {
	pre   func(tx *sql.Tx) error // first, in the same transaction; an error rolls everything back
	stmts []string
}

// v1Statements is schema v1. It is moved here verbatim and must stay byte-identical.
var v1Statements = []string{
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
}

// migrations[i] upgrades schema version i to i+1. Append only; never edit.
var migrations = []migration{
	{stmts: v1Statements},
	{pre: v2Precheck, stmts: v2Statements},
}

// Store is the case's artifacts.db.
type Store struct{ db *sql.DB }

// OpenStore opens or creates the database and migrates it to CurrentSchema. It
// is for a newly created case (and tests): opening an existing case never
// migrates (see OpenExistingStore).
func OpenStore(path string) (*Store, error) {
	s, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrateTo(CurrentSchema); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

// OpenExistingStore opens an existing database without ever changing its schema.
// A missing database, a missing schema_version table or one without a row is an
// integrity failure; a schema newer than this build is a plain error. An older
// schema opens as it is (Case.Upgrade is the only way forward).
func OpenExistingStore(path string) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: artifacts.db is missing", ErrIntegrity)
	}
	s, err := openDB(path)
	if err != nil {
		return nil, err
	}
	var tables int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&tables); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("artifacts.db schema: %w", err)
	}
	if tables == 0 {
		_ = s.db.Close()
		return nil, fmt.Errorf("%w: artifacts.db has no schema_version table", ErrIntegrity)
	}
	v, err := s.SchemaVersion()
	switch {
	case err != nil:
		err = fmt.Errorf("artifacts.db schema_version: %w", err)
	case v == 0:
		err = fmt.Errorf("%w: artifacts.db schema_version holds no version", ErrIntegrity)
	case v > CurrentSchema:
		err = fmt.Errorf("artifacts.db schema version %d is newer than this build supports (%d)", v, CurrentSchema)
	}
	if err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

// openDB opens the database file and sets the pragmas every connection needs:
// foreign keys on, rollback journal (never WAL, so no -wal/-shm file appears
// next to the evidence database) and full fsync.
func openDB(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open artifacts.db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := configure(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func configure(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	// Without recursive_triggers SQLite does not fire DELETE triggers for the
	// rows an INSERT OR REPLACE removes, which would let a REPLACE rewrite a row
	// of an immutable table.
	if _, err := db.Exec(`PRAGMA recursive_triggers = ON`); err != nil {
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
		return fmt.Errorf("artifacts.db journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("artifacts.db journal_mode is %q, want delete", mode)
	}
	if _, err := db.Exec(`PRAGMA synchronous = FULL`); err != nil {
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	return nil
}

// migrateTo applies the pending migrations up to target (at most
// len(migrations)). A database already at or past target is left alone; one
// newer than this build supports is an error.
func (s *Store) migrateTo(target int) error {
	if target > len(migrations) {
		return fmt.Errorf("artifacts.db cannot migrate to schema %d: this build knows %d", target, len(migrations))
	}
	if err := ensureVersionTable(s.db); err != nil {
		return err
	}
	v, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("artifacts.db schema version %d is newer than this build supports (%d)", v, len(migrations))
	}
	return applyMigrations(s.db, v, target)
}

func ensureVersionTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("artifacts.db schema_version: %w", err)
	}
	return nil
}

// applyMigrations runs migrations[from:to], each in its own transaction (the
// pre-check, the statements and the schema_version row commit together or not
// at all). Tests use it to build a database at an older version.
func applyMigrations(db *sql.DB, from, to int) error {
	if from < 0 || from > to || to > len(migrations) {
		return fmt.Errorf("artifacts.db cannot apply migrations %d..%d: this build knows %d", from, to, len(migrations))
	}
	if err := ensureVersionTable(db); err != nil {
		return err
	}
	for i := from; i < to; i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := runMigration(tx, i); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func runMigration(tx *sql.Tx, i int) error {
	m := migrations[i]
	if m.pre != nil {
		if err := m.pre(tx); err != nil {
			return fmt.Errorf("artifacts.db migration %d: %w", i+1, err)
		}
	}
	for _, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("artifacts.db migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
		return fmt.Errorf("artifacts.db migration %d: %w", i+1, err)
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
