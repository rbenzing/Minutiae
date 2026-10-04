package evidence_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// TestRecordstestV1DDLMatchesEvidence ties the frozen v1 DDL copy in
// recordstest to evidence's v1 statements: the schema SQL SQLite stored for a
// recordstest v1 case is exactly the text of evidence.v1Statements.
func TestRecordstestV1DDLMatchesEvidence(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	db, err := sql.Open("sqlite", filepath.Join(dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(evidence.V1Statements) {
		t.Fatalf("recordstest v1 case has %d schema objects, evidence v1 has %d statements", len(got), len(evidence.V1Statements))
	}
	for i, want := range evidence.V1Statements {
		if got[i] != strings.TrimSpace(want) {
			t.Errorf("v1 statement %d differs:\nrecordstest: %q\nevidence:    %q", i, got[i], want)
		}
	}
}
