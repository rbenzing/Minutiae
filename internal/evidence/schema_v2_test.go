package evidence

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawDB opens path with a plain connection (no pragmas, no migrations).
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// buildV1DB creates a schema v1 database at path.
func buildV1DB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := rawDB(t, path)
	if err := applyMigrations(db, 0, 1); err != nil {
		t.Fatal(err)
	}
	return db
}

func fileSHA(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func rawVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestMigrateV1ToV2KeepsArtifacts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	db := buildV1DB(t, p)
	if v := rawVersion(t, db); v != 1 {
		t.Fatalf("v1 build version = %d", v)
	}
	if _, err := db.Exec(`INSERT INTO artifacts (id, path, size, sha256, md5, device_id, source, incomplete, created)
		VALUES ('a1', 'artifacts/d/a/x', 3, 'sha', 'md5', 'd', '{}', 0, '2026-10-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s := openTestStore(t, p)
	if v, err := s.SchemaVersion(); err != nil || v != 2 {
		t.Fatalf("version = %d, %v", v, err)
	}
	h, err := s.ArtifactHashes()
	if err != nil || len(h) != 1 || h["a1"] != "sha" {
		t.Fatalf("artifacts after migration = %v, %v", h, err)
	}
	rows, err := s.db.Query(`PRAGMA table_info(records)`)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, want := range []string{
		"id", "batch_id", "type", "payload_v", "artifact_id", "source_path", "locator", "src_offset", "src_length",
		"ts", "ts_end", "ts_basis", "tz_offset_min", "deleted", "recovered", "recovery_method", "confidence",
		"parser_id", "summary", "body", "payload",
	} {
		if !cols[want] {
			t.Errorf("records column %q missing (have %v)", want, cols)
		}
	}
	for _, gone := range []string{"offset", "length", "timestamp", "data"} {
		if cols[gone] {
			t.Errorf("v1 records column %q survived the migration", gone)
		}
	}
}

func TestMigrateRefusesNonEmptyV1Records(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	db := buildV1DB(t, p)
	if _, err := db.Exec(`INSERT INTO records (type, data) VALUES ('x', '{}')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before := fileSHA(t, p)

	s, err := OpenStore(p)
	if err == nil {
		_ = s.Close()
		t.Fatal("OpenStore migrated a v1 database that holds records")
	}
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("err = %v, want ErrMigrationBlocked", err)
	}
	if !strings.Contains(err.Error(), "records table holds 1 row") {
		t.Fatalf("message does not name the row count: %v", err)
	}
	if after := fileSHA(t, p); after != before {
		t.Fatal("a blocked migration changed the database file")
	}
	db2 := rawDB(t, p)
	if v := rawVersion(t, db2); v != 1 {
		t.Fatalf("version after blocked migration = %d, want 1", v)
	}
	if tableExists(t, db2, "parsers") {
		t.Fatal("v2 tables exist after a blocked migration")
	}
}

func TestOpenExistingStoreDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()

	v1 := filepath.Join(dir, "v1.db")
	_ = buildV1DB(t, v1).Close()
	s, err := OpenExistingStore(v1)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.SchemaVersion(); err != nil || v != 1 {
		t.Fatalf("v1 db reports version %d, %v", v, err)
	}
	_ = s.Close()
	db := rawDB(t, v1)
	if tableExists(t, db, "parsers") {
		t.Fatal("OpenExistingStore created v2 tables in a v1 database")
	}
	if v := rawVersion(t, db); v != 1 {
		t.Fatalf("OpenExistingStore left version %d, want 1", v)
	}
	_ = db.Close()

	v99 := filepath.Join(dir, "v99.db")
	s2, err := OpenStore(v99)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.Exec(`INSERT INTO schema_version (version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	if s3, err := OpenExistingStore(v99); err == nil {
		_ = s3.Close()
		t.Fatal("expected an error for a newer schema")
	} else if errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema err = %v, want a plain 'newer' error", err)
	}

	empty := filepath.Join(dir, "empty.db")
	edb := rawDB(t, empty)
	if _, err := edb.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = edb.Close()
	if s4, err := OpenExistingStore(empty); err == nil {
		_ = s4.Close()
		t.Fatal("expected an error for an empty schema_version")
	} else if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("empty schema_version err = %v, want ErrIntegrity", err)
	}

	noTable := filepath.Join(dir, "notable.db")
	ndb := rawDB(t, noTable)
	if _, err := ndb.Exec(`CREATE TABLE other (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	_ = ndb.Close()
	if s5, err := OpenExistingStore(noTable); err == nil {
		_ = s5.Close()
		t.Fatal("expected an error for a missing schema_version table")
	} else if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("missing schema_version err = %v, want ErrIntegrity", err)
	}

	missing := filepath.Join(dir, "missing.db")
	if s6, err := OpenExistingStore(missing); err == nil {
		_ = s6.Close()
		t.Fatal("expected an error for a missing database")
	} else if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("missing db err = %v, want ErrIntegrity", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExistingStore created the missing database (stat err = %v)", err)
	}
}

// seedRecordTables inserts one valid row into every table of the v2 schema.
func seedRecordTables(t *testing.T, s *Store) {
	t.Helper()
	if err := s.InsertArtifact(ManifestRecord{
		ID: "a1", Path: "artifacts/d/a/x", Size: 3, SHA256: "s", MD5: "m",
		Source: Source{Kind: "file", DeviceID: "d", RemotePath: "/x"}, Finished: "2026-10-02T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO parsers (id, name, version, hash) VALUES (1, 'p', '1', NULL)`,
		`INSERT INTO record_batches (batch_id, ingest_id, batch_no, first_id, count, digest, created)
			VALUES (1, 'ing', 1, 1, 1, 'd', '2026-10-02T00:00:00Z')`,
		`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, payload)
			VALUES (1, 1, 'event', 1, 'a1', 1, '{}')`,
		`INSERT INTO record_times (record_id, kind, ts, ts_basis) VALUES (1, 'read', 5, 'utc')`,
		`INSERT INTO record_runs (end_seq, ingest_id, parser_id, outcome, batches, records, first_id, last_id, rollup, ended)
			VALUES (7, 'ing', 1, 'complete', 1, 1, 1, 1, 'r', '2026-10-02T00:00:00Z')`,
		`INSERT INTO record_run_artifacts (ingest_id, artifact_id) VALUES ('ing', 'a1')`,
		`INSERT INTO record_superseded (ingest_id, artifact_id) VALUES ('old', 'a1')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestRecordTablesImmutableTriggers(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	seedRecordTables(t, s)

	// A column that exists in each table, for a no-op UPDATE (the trigger fires per row).
	col := map[string]string{
		"artifacts": "path", "parsers": "name", "record_batches": "digest", "records": "summary",
		"record_times": "ts", "record_runs": "rollup", "record_run_artifacts": "artifact_id",
		"record_superseded": "artifact_id",
	}
	if len(immutableTables) != len(col) {
		t.Fatalf("immutableTables = %v, test knows %d tables", immutableTables, len(col))
	}
	for _, table := range immutableTables {
		c, ok := col[table]
		if !ok {
			t.Fatalf("no test column for %s", table)
		}
		var before int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&before); err != nil || before != 1 {
			t.Fatalf("%s: seeded rows = %d, %v", table, before, err)
		}
		_, err := s.db.Exec(fmt.Sprintf(`UPDATE %s SET %s = %s`, table, c, c))
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("UPDATE %s: err = %v, want immutable", table, err)
		}
		_, err = s.db.Exec(fmt.Sprintf(`DELETE FROM %s`, table))
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("DELETE FROM %s: err = %v, want immutable", table, err)
		}
		var after int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&after); err != nil || after != 1 {
			t.Errorf("%s: rows after refused changes = %d, %v", table, after, err)
		}
	}

	if _, err := s.db.Exec(`UPDATE records_meta SET value = '5' WHERE key = 'next_id'`); err != nil {
		t.Fatalf("records_meta must stay mutable: %v", err)
	}
}

func TestExpectedTriggersMatchSchema(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	want := expectedTriggers()
	if len(want) != 2*len(immutableTables) || len(want) != 16 {
		t.Fatalf("expectedTriggers has %d entries, want 16", len(want))
	}
	for _, table := range immutableTables {
		for _, op := range []string{"upd", "del"} {
			name := immutabilityTriggerName(table, op)
			if want[name] != table {
				t.Errorf("expectedTriggers[%s] = %q, want %q", name, want[name], table)
			}
		}
	}
	rows, err := s.db.Query(`SELECT name, tbl_name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var name, tbl string
		if err := rows.Scan(&name, &tbl); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		got[name] = tbl
	}
	_ = rows.Close()
	if len(got) != len(want) {
		t.Fatalf("db has %d triggers, expected %d: %v", len(got), len(want), got)
	}
	for name, table := range want {
		if got[name] != table {
			t.Errorf("trigger %s on %q, want %q", name, got[name], table)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	seedRecordTables(t, s)

	cols := []string{
		"id", "batch_id", "type", "payload_v", "artifact_id", "ts", "ts_end", "ts_basis", "tz_offset_min",
		"deleted", "recovered", "recovery_method", "confidence", "src_offset", "src_length", "parser_id", "payload",
	}
	insert := func(id int, over map[string]any) error {
		vals := map[string]any{
			"id": id, "batch_id": 1, "type": "event", "payload_v": 1, "artifact_id": "a1",
			"deleted": 0, "recovered": 0, "parser_id": 1, "payload": "{}",
		}
		for k, v := range over {
			vals[k] = v
		}
		args := make([]any, len(cols))
		for i, c := range cols {
			args[i] = vals[c] // a missing key is a NULL
		}
		_, err := s.db.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, ts, ts_end, ts_basis, tz_offset_min,
			deleted, recovered, recovery_method, confidence, src_offset, src_length, parser_id, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		return err
	}

	good := []struct {
		name string
		over map[string]any
	}{
		{"plain", nil},
		{"utc time", map[string]any{"ts": 5, "ts_basis": "utc"}},
		{"local offset", map[string]any{"ts": 5, "ts_end": 9, "ts_basis": "local-offset", "tz_offset_min": -330}},
		{"local unknown", map[string]any{"ts": 5, "ts_basis": "local-unknown"}},
		{"deleted", map[string]any{"deleted": 1}},
		{"recovered", map[string]any{"deleted": 1, "recovered": 1, "recovery_method": "carve", "confidence": 100}},
		{"range", map[string]any{"src_offset": 0, "src_length": 0}},
	}
	for i, tc := range good {
		if err := insert(10+i, tc.over); err != nil {
			t.Errorf("valid row %q rejected: %v", tc.name, err)
		}
	}

	bad := []struct {
		name string
		over map[string]any
	}{
		{"recovered without deleted", map[string]any{"recovered": 1, "recovery_method": "carve"}},
		{"recovered without method", map[string]any{"deleted": 1, "recovered": 1}},
		{"method without recovered", map[string]any{"recovery_method": "carve"}},
		{"recovered flag 2", map[string]any{"recovered": 2, "deleted": 1, "recovery_method": "carve"}},
		{"deleted flag 2", map[string]any{"deleted": 2}},
		{"offset without length", map[string]any{"src_offset": 5}},
		{"length without offset", map[string]any{"src_length": 5}},
		{"negative offset", map[string]any{"src_offset": -1, "src_length": 1}},
		{"negative length", map[string]any{"src_offset": 1, "src_length": -1}},
		{"confidence 101", map[string]any{"confidence": 101}},
		{"confidence -1", map[string]any{"confidence": -1}},
		{"bad ts_basis", map[string]any{"ts": 5, "ts_basis": "bogus"}},
		{"tz with NULL basis", map[string]any{"tz_offset_min": 60}},
		{"tz with utc basis", map[string]any{"ts": 5, "ts_basis": "utc", "tz_offset_min": 60}},
		{"local-offset without tz", map[string]any{"ts": 5, "ts_basis": "local-offset"}},
		{"ts without basis", map[string]any{"ts": 5}},
		{"basis without ts", map[string]any{"ts_basis": "utc"}},
		{"ts_end without ts", map[string]any{"ts_end": 5}},
		{"unknown artifact", map[string]any{"artifact_id": "nope"}},
		{"unknown batch", map[string]any{"batch_id": 99}},
		{"unknown parser", map[string]any{"parser_id": 99}},
	}
	for i, tc := range bad {
		if err := insert(100+i, tc.over); err == nil {
			t.Errorf("bad row %q was accepted", tc.name)
		}
	}

	badTimes := []struct {
		name string
		args []any
	}{
		{"unknown record", []any{99, "k", 5, "utc", nil}},
		{"bad basis", []any{1, "k", 5, "bogus", nil}},
		{"tz with utc", []any{1, "k", 5, "utc", 60}},
		{"local-offset without tz", []any{1, "k", 5, "local-offset", nil}},
	}
	for _, tc := range badTimes {
		if _, err := s.db.Exec(`INSERT INTO record_times (record_id, kind, ts, ts_basis, tz_offset_min)
			VALUES (?, ?, ?, ?, ?)`, tc.args...); err == nil {
			t.Errorf("bad record_times row %q was accepted", tc.name)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO record_runs (end_seq, ingest_id, parser_id, outcome, batches, records, first_id, last_id, rollup, ended)
		VALUES (8, 'x', 1, 'bogus', 0, 0, 0, 0, 'r', 't')`); err == nil {
		t.Error("record_runs accepted an unknown outcome")
	}
}

func journalMode(t *testing.T, db *sql.DB) string {
	t.Helper()
	var m string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestJournalModeIsDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "artifacts.db")
	s := openTestStore(t, p)
	if m := journalMode(t, s.db); m != "delete" {
		t.Fatalf("journal_mode = %q, want delete", m)
	}
	var sync int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
		t.Fatalf("synchronous = %d (want 2 = FULL), %v", sync, err)
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v", fk, err)
	}
	if err := s.InsertArtifact(ManifestRecord{ID: "a1", Path: "artifacts/x", Source: Source{Kind: "file"}, Finished: "t"}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(p + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("artifacts.db%s exists (stat err = %v)", suffix, err)
		}
	}
	_ = s.Close()

	// A database found in WAL mode is converted at open.
	raw := rawDB(t, p)
	if m := journalMode2(t, raw, "WAL"); m != "wal" {
		t.Skipf("could not put the database in WAL mode (got %q)", m)
	}
	_ = raw.Close()
	s2, err := OpenExistingStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if m := journalMode(t, s2.db); m != "delete" {
		t.Fatalf("journal_mode after OpenExistingStore = %q, want delete", m)
	}
}

func journalMode2(t *testing.T, db *sql.DB, mode string) string {
	t.Helper()
	var m string
	if err := db.QueryRow(`PRAGMA journal_mode=` + mode).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFTS5Available(t *testing.T) {
	db := rawDB(t, filepath.Join(t.TempDir(), "fts.db"))
	if _, err := db.Exec(`CREATE VIRTUAL TABLE words USING fts5(body, content='', tokenize = "unicode61 remove_diacritics 2")`); err != nil {
		t.Fatalf("unicode61 fts5: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO words (rowid, body) VALUES (1, 'Café au lait')`); err != nil {
		t.Fatal(err)
	}
	var rowid int
	if err := db.QueryRow(`SELECT rowid FROM words WHERE words MATCH 'cafe'`).Scan(&rowid); err != nil || rowid != 1 {
		t.Fatalf("unicode61 MATCH = %d, %v", rowid, err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE grams USING fts5(body, content='', tokenize = "trigram case_sensitive 0")`); err != nil {
		t.Fatalf("trigram fts5: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO grams (rowid, body) VALUES (2, 'Hello World')`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT rowid FROM grams WHERE grams MATCH 'ORLD'`).Scan(&rowid); err != nil || rowid != 2 {
		t.Fatalf("trigram MATCH = %d, %v", rowid, err)
	}
}
