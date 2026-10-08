package evidence

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T, p string) *Store {
	t.Helper()
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreMigratesToV3(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	v, err := s.SchemaVersion()
	if err != nil || v != 3 {
		t.Fatalf("version = %d, %v", v, err)
	}
	for _, table := range []string{
		"artifacts", "parsers", "record_batches", "records", "record_times", "record_runs",
		"record_run_artifacts", "record_superseded", "records_meta", "schema_version",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
	for _, index := range []string{
		"records_type_ts", "records_ts", "records_artifact", "records_deleted", "records_parser",
		"record_times_kind", "record_run_artifacts_artifact",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing (n=%d err=%v)", index, n, err)
		}
	}
	var next string
	if err := s.db.QueryRow(`SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&next); err != nil || next != "1" {
		t.Fatalf("records_meta next_id = %q, %v", next, err)
	}

	// the full-text tables, their vocab tables and the 8 shadow tables (4 per FTS table, R7)
	for _, table := range []string{
		"records_fts", "records_fts_sub", "records_fts_v", "records_fts_sub_v",
		"records_fts_data", "records_fts_idx", "records_fts_docsize", "records_fts_config",
		"records_fts_sub_data", "records_fts_sub_idx", "records_fts_sub_docsize", "records_fts_sub_config",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
	var fts int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'records\_fts%' ESCAPE '\'`).Scan(&fts); err != nil || fts != 12 {
		t.Fatalf("%d full-text tables, %v; want 12 (2 FTS tables, 2 vocab tables, 8 shadow tables)", fts, err)
	}
	ddl := func(name string) string {
		var q string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, name).Scan(&q); err != nil {
			t.Fatal(err)
		}
		return q
	}
	if q := ddl("records_fts"); strings.Contains(q, "prefix") || !strings.Contains(q, "unicode61 remove_diacritics 2") {
		t.Errorf("word table DDL = %q: no prefix option and unicode61 remove_diacritics 2 expected", q)
	}
	if q := ddl("records_fts_sub"); strings.Contains(q, "prefix") || !strings.Contains(q, "trigram case_sensitive 0 remove_diacritics 1") {
		t.Errorf("sub table DDL = %q: trigram case_sensitive 0 remove_diacritics 1 expected", q)
	}
	var norm string
	if err := s.db.QueryRow(`SELECT value FROM records_meta WHERE key = 'fts_norm_version'`).Scan(&norm); err != nil || norm != FTSNormVersion() {
		t.Fatalf("fts_norm_version = %q, %v; want %q", norm, err, FTSNormVersion())
	}
}

func TestStoreMigrationIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s1, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s1.db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("schema_version rows = %d, %v", rows, err)
	}
	_ = s1.Close()
	s2 := openTestStore(t, p)
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("schema_version rows after reopen = %d, %v", rows, err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s := openTestStore(t, p)
	if _, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := OpenStore(p); err == nil {
		t.Fatal("expected error for newer schema")
	}
}

func TestStoreInsertAndReadHashes(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	r := ManifestRecord{
		ID: "id1", Path: "artifacts/d/a/x", Size: 3, SHA256: "s", MD5: "m",
		Source: Source{Kind: "file", DeviceID: "d", RemotePath: "/x"}, Finished: "2026-10-02T00:00:00Z",
	}
	if err := s.InsertArtifact(r); err != nil {
		t.Fatal(err)
	}
	h, err := s.ArtifactHashes()
	if err != nil || h["id1"] != "s" {
		t.Fatalf("hashes = %v, %v", h, err)
	}
}

func TestManifestAppendRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.jsonl")
	if rs, err := readManifest(p); err != nil || len(rs) != 0 {
		t.Fatalf("missing manifest should read empty: %v %v", rs, err)
	}
	for _, id := range []string{"a", "b"} {
		if err := appendManifest(p, ManifestRecord{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := readManifest(p)
	if err != nil || len(rs) != 2 || rs[1].ID != "b" {
		t.Fatalf("got %+v %v", rs, err)
	}
}

// pragmaState reads the four settings every connection to artifacts.db needs.
func pragmaState(t *testing.T, q func(string) (string, error)) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, name := range []string{"journal_mode", "synchronous", "foreign_keys", "recursive_triggers"} {
		v, err := q(`PRAGMA ` + name)
		if err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		got[name] = v
	}
	return got
}

func assertPragmas(t *testing.T, label string, got map[string]string) {
	t.Helper()
	want := map[string]string{"journal_mode": "delete", "synchronous": "2", "foreign_keys": "1", "recursive_triggers": "1"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: PRAGMA %s = %q, want %q", label, k, got[k], w)
		}
	}
}

// TestFreshConnectionGetsEveryPragma: database/sql may discard a connection
// (a driver reports driver.ErrBadConn) and open a new one. The new connection
// must carry foreign_keys, recursive_triggers, synchronous=FULL and the DELETE
// journal like the first one, or the immutability triggers could be bypassed by
// INSERT OR REPLACE on it.
func TestFreshConnectionGetsEveryPragma(t *testing.T) {
	for _, mk := range []struct {
		name string
		open func(t *testing.T, p string) *Store
	}{
		{"OpenStore", openTestStore},
		{"OpenExistingStore", func(t *testing.T, p string) *Store {
			s := openTestStore(t, p)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, err := OpenExistingStore(p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			return s2
		}},
	} {
		t.Run(mk.name, func(t *testing.T) {
			s := mk.open(t, filepath.Join(t.TempDir(), "a.db"))
			seedRecordTables(t, s)
			ctx := context.Background()

			// Discard the pooled connection: Raw reporting ErrBadConn makes
			// database/sql drop it when it is returned.
			for i := 0; i < 2; i++ {
				c, err := s.db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_ = c.Raw(func(any) error { return driver.ErrBadConn })
				_ = c.Close()

				fresh, err := s.db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				assertPragmas(t, "fresh connection", pragmaState(t, func(q string) (string, error) {
					var v any
					err := fresh.QueryRowContext(ctx, q).Scan(&v)
					return strings.ToLower(fmt.Sprint(v)), err
				}))
				// A REPLACE of an immutable row is refused on the fresh connection.
				_, err = fresh.ExecContext(ctx, `INSERT OR REPLACE INTO parsers (id, name, version, hash) VALUES (1, 'p', '1', 'forged')`)
				if err == nil || !strings.Contains(err.Error(), "immutable") {
					t.Errorf("REPLACE on a fresh connection: err = %v, want immutable", err)
				}
				// Foreign keys are enforced on it.
				_, err = fresh.ExecContext(ctx, `INSERT INTO record_run_artifacts (ingest_id, artifact_id) VALUES ('nope', 'nope')`)
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
					t.Errorf("FK violation on a fresh connection: err = %v, want a foreign key error", err)
				}
				_ = fresh.Close()
			}
		})
	}
}

// TestOpenDBPermissionErrorFromDriverIsNotIntegrity drives the real driver into
// SQLITE_CANTOPEN (the database path is a directory): that is an I/O problem,
// never evidence damage.
func TestOpenDBCannotOpenFromDriverIsNotIntegrity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "artifacts.db")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := OpenExistingStore(p)
	if err == nil {
		t.Fatal("OpenExistingStore succeeded on a directory")
	}
	if errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want a plain error (not ErrIntegrity)", err)
	}
}
