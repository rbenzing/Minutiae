// Package recordstest builds cases and tampers with them for tests of the
// unified artifact database. Raw SQL against the record tables lives here (the
// single-writer rule allows internal/evidence and this package only).
package recordstest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// v1DDL is a frozen copy of the schema v1 artifacts.db DDL, exactly as the
// v1.0.0 build created it. v1 history is never edited: do not change these
// statements, and never derive them from internal/evidence (a later schema
// change must not alter what a v1 case looks like).
var v1DDL = []string{
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
	`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
	`INSERT INTO schema_version (version) VALUES (1)`,
}

// NewV1Case builds, under t.TempDir(), a case directory exactly as the v1.0.0
// build would have left it (case.json; an audit log whose case.create entry has
// no schema_version; an artifacts.db at schema v1; no case.lock) and returns
// its path. The case is closed: open it with evidence.Open.
func NewV1Case(t testing.TB) string {
	t.Helper()
	return newV1Case(t, false)
}

// NewV1CaseWithRecord is NewV1Case with one row in the v1 records table, which
// blocks the v1 -> v2 upgrade.
func NewV1CaseWithRecord(t testing.TB) string {
	t.Helper()
	return newV1Case(t, true)
}

func newV1Case(t testing.TB, withRecord bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "V1CASE")
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o750); err != nil {
		t.Fatal(err)
	}
	meta := evidence.Meta{
		ID: "V1CASE", Examiner: "Examiner", Created: "2026-01-01T00:00:00Z",
		ToolVersion: "v1.0.0", HostOS: runtime.GOOS, HostArch: runtime.GOARCH,
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "case.json"), append(b, 0x0a), 0o600); err != nil {
		t.Fatal(err)
	}

	audit, err := evidence.CreateAuditLog(filepath.Join(dir, "audit.jsonl"), "tester", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Append("case.create", "", map[string]any{
		"id": meta.ID, "examiner": meta.Examiner, "description": "",
	}); err != nil {
		_ = audit.Close()
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, stmt := range v1DDL {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if withRecord {
		if _, err := db.Exec(`INSERT INTO records (type, data) VALUES ('legacy', '{}')`); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
