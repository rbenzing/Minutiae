package evidence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"modernc.org/sqlite" // pure-Go SQLite driver; also registered as "sqlite" for test helpers
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
// A missing database, one that is not a SQLite database or is corrupt, a missing
// schema_version table or one without a row is an integrity failure (it is never
// recreated); a schema newer than this build and ordinary I/O errors are plain
// errors. An older schema opens as it is (Case.Upgrade is the only way forward).
func OpenExistingStore(path string) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: artifacts.db is missing", ErrIntegrity)
	}
	s, err := openDB(path)
	if err != nil {
		return nil, classifyDBError(err)
	}
	var tables int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&tables); err != nil {
		_ = s.db.Close()
		return nil, classifyDBError(fmt.Errorf("artifacts.db schema: %w", err))
	}
	if tables == 0 {
		_ = s.db.Close()
		return nil, fmt.Errorf("%w: artifacts.db has no schema_version table", ErrIntegrity)
	}
	v, err := s.SchemaVersion()
	switch {
	case err != nil:
		err = classifyDBError(fmt.Errorf("artifacts.db schema_version: %w", err))
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

// SQLite primary result codes that mean the file itself is damaged.
const (
	sqliteCorrupt = 11 // SQLITE_CORRUPT: database disk image is malformed
	sqliteNotADB  = 26 // SQLITE_NOTADB: file is not a database
)

// classifyDBError wraps ErrIntegrity around an error that says artifacts.db is
// not a SQLite database or is corrupt (a structural failure of the evidence
// file, so it fails closed like a missing database). Any other error (permission,
// descriptor exhaustion, ...) is returned unchanged: it is not evidence damage.
func classifyDBError(err error) error {
	if err == nil || errors.Is(err, ErrIntegrity) {
		return err
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case sqliteCorrupt, sqliteNotADB:
			return fmt.Errorf("%w: artifacts.db is corrupt or not a SQLite database: %w", ErrIntegrity, err)
		}
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "file is not a database") || strings.Contains(msg, "database disk image is malformed") {
		return fmt.Errorf("%w: artifacts.db is corrupt or not a SQLite database: %w", ErrIntegrity, err)
	}
	return err
}

// openDB opens the database file. Every connection the pool ever opens (not
// only the first: database/sql discards a bad connection and dials a new one)
// is configured by dbConnector: foreign keys on, recursive triggers on, rollback
// journal (never WAL, so no -wal/-shm file appears next to the evidence
// database) and full fsync. The pragmas run inside Connect, so no caller can
// obtain a connection that lacks them, and a connection that cannot be
// configured is refused rather than used.
func openDB(path string) (*Store, error) {
	db := sql.OpenDB(dbConnector{path: path})
	db.SetMaxOpenConns(1)
	// Prove the first connection can be made and configured now, so a bad file
	// fails here with the real error.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// dbConnector opens connections to artifacts.db through the SQLite driver and
// configures each one. It uses the driver directly (not a DSN) so the path is
// never parsed as a URL: Windows drive letters, spaces and '?' stay as they are.
type dbConnector struct{ path string }

func (c dbConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func (c dbConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, fmt.Errorf("open artifacts.db: %w", err)
	}
	if err := configureConn(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// connPragmas are applied to every connection, in this order. recursive_triggers
// matters for integrity: without it SQLite does not fire DELETE triggers for the
// rows an INSERT OR REPLACE removes, which would let a REPLACE rewrite a row of
// an immutable table.
var connPragmas = []string{
	`PRAGMA foreign_keys = ON`,
	`PRAGMA recursive_triggers = ON`,
	`PRAGMA synchronous = FULL`,
}

func configureConn(ctx context.Context, conn driver.Conn) error {
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		return errors.New("artifacts.db: the SQLite driver cannot execute statements")
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		return errors.New("artifacts.db: the SQLite driver cannot run queries")
	}
	for _, p := range connPragmas {
		if _, err := execer.ExecContext(ctx, p, nil); err != nil {
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
	}
	// journal_mode answers with the mode in force, which must be DELETE.
	rows, err := queryer.QueryContext(ctx, `PRAGMA journal_mode = DELETE`, nil)
	if err != nil {
		return fmt.Errorf("artifacts.db journal_mode: %w", err)
	}
	defer func() { _ = rows.Close() }()
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		return fmt.Errorf("artifacts.db journal_mode: %w", err)
	}
	mode := fmt.Sprint(dest[0])
	if b, ok := dest[0].([]byte); ok {
		mode = string(b)
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("artifacts.db journal_mode is %q, want delete", mode)
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
