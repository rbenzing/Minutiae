package evidence

import (
	"strings"
	"testing"
)

// insertRawRecords writes a parsers row, a record_batches row and n record rows
// of artifactID straight into the tables, with no audit entry and no writer: the
// forgery verify must catch. It returns the batch id. The immutability triggers
// do not block inserts.
func insertRawRecords(t *testing.T, c *Case, artifactID string, firstID int64, n int) int64 {
	t.Helper()
	db := c.store.db
	if _, err := db.Exec(`INSERT OR IGNORE INTO parsers (name, version, hash) VALUES ('raw', '1', NULL)`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES ('ing-raw', 1, ?, ?, ?, '2026-01-01T00:00:00Z')`,
		firstID, n, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	batchID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, summary, payload)
			VALUES (?, ?, 'event', 1, ?, (SELECT id FROM parsers WHERE name = 'raw'), 'raw', '{}')`, firstID+int64(i), batchID, artifactID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, firstID+int64(n)); err != nil {
		t.Fatal(err)
	}
	return batchID
}

func TestVerifyRecordRowsWithoutAuditAreFlagged(t *testing.T) {
	c, rec := caseWithArtifact(t)
	insertRawRecords(t, c, rec.ID, 1, 3)
	r := mustVerify(t, c)
	for _, want := range []string{
		"is not announced by any records.batch audit entry",
		"record 1 is outside every batch range",
		`parser "raw" "1" (id 1) is not named by any records.ingest.start audit entry`,
	} {
		if !containsSubstr(r.Problems, want) {
			t.Errorf("missing problem %q in %q", want, r.Problems)
		}
	}
	if r.OK() || r.RecordsChecked != 3 {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyFlagsMissingImmutabilityTrigger(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("a fresh v2 case must verify: %q", r.Problems)
	}
	if _, err := c.store.db.Exec(`DROP TRIGGER immut_record_runs_del`); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || len(r.Problems) != 1 || !strings.Contains(r.Problems[0], `trigger "immut_record_runs_del" is missing`) {
		t.Fatalf("report = %q", r.Problems)
	}
	dropImmutabilityTriggers(t, c)
	if r := mustVerify(t, c); len(r.Problems) != 16 {
		t.Fatalf("%d problems for 16 dropped triggers: %q", len(r.Problems), r.Problems)
	}
}

func TestVerifyFlagsAlteredAndExtraTrigger(t *testing.T) {
	c, _ := caseWithArtifact(t)
	// same name, a body that blocks nothing (whitespace is irrelevant, content is not)
	if _, err := c.store.db.Exec(`DROP TRIGGER immut_parsers_upd`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.db.Exec(`CREATE TRIGGER immut_parsers_upd BEFORE UPDATE ON parsers BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.db.Exec(`CREATE TRIGGER sneaky AFTER INSERT ON records_meta BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, `trigger "immut_parsers_upd" was altered`) || !containsSubstr(r.Problems, `trigger "sneaky" is not part of the schema`) || len(r.Problems) != 2 {
		t.Fatalf("report = %q", r.Problems)
	}
}

func TestTriggerComparisonIgnoresWhitespaceOnly(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if _, err := c.store.db.Exec(`DROP TRIGGER immut_parsers_upd`); err != nil {
		t.Fatal(err)
	}
	// the expected DDL laid out differently: the same trigger
	if _, err := c.store.db.Exec("CREATE TRIGGER immut_parsers_upd\n\tBEFORE UPDATE ON parsers\n\tBEGIN\n\t\tSELECT RAISE(ABORT, 'immutable table parsers');\n\tEND"); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("a reformatted trigger is not a problem: %q", r.Problems)
	}
}

// TestVerifyIntegrityCheckFlagged: a row that breaks a CHECK constraint (written with
// the checks switched off, as a tamper would) makes integrity_check fail.
func TestVerifyIntegrityCheckFlagged(t *testing.T) {
	c, rec := caseWithArtifact(t)
	insertRawRecords(t, c, rec.ID, 1, 1)
	dropImmutabilityTriggers(t, c) // an attacker removes the triggers first
	conn, err := c.store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `UPDATE records SET confidence = 500 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `PRAGMA ignore_check_constraints = OFF`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, "integrity_check failed") {
		t.Fatalf("report = %q", r.Problems)
	}
}

func TestVerifyRecordsUnreadableIsAProblemNotAnAbort(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if _, err := c.store.db.Exec(`DROP TABLE record_batches`); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c) // completes
	if r.OK() || !containsSubstr(r.Problems, "records unreadable") {
		t.Fatalf("report = %q", r.Problems)
	}
	assertLastAction(t, c, "verify.run")
}

// TestVerifyStreamsRowsInChunks: the record rows are read in keyset chunks of at
// most 5000 rows, never by one query.
func TestVerifyStreamsRowsInChunks(t *testing.T) {
	c, rec := caseWithArtifact(t)
	const n = 12000
	if _, err := c.store.db.Exec(`INSERT INTO parsers (name, version, hash) VALUES ('raw', '1', NULL)`); err != nil {
		t.Fatal(err)
	}
	tx, err := c.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES ('ing-raw', 1, 1, ?, ?, 'x')`, n, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := tx.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, summary, payload)
			VALUES (?, 1, 'event', 1, ?, 1, 'raw', '{}')`, i, rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A chunk is one query of the record rows (with the record_times of its id
	// range, in the same read transaction); the observer sees one event per chunk.
	var chunks []int
	rep, err := c.verify(func(table string, rows int) {
		if table == "records" {
			chunks = append(chunks, rows)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecordsChecked != n {
		t.Fatalf("RecordsChecked = %d, want %d", rep.RecordsChecked, n)
	}
	if want := []int{5000, 5000, 2000}; len(chunks) != len(want) || chunks[0] != want[0] || chunks[1] != want[1] || chunks[2] != want[2] {
		t.Fatalf("chunk sizes = %v, want %v", chunks, want)
	}
	// the rows are unaudited forgeries here; what matters is that none was missed
	if containsSubstr(rep.Problems, "verify read") {
		t.Errorf("the scans disagree with count(*): %q", rep.Problems)
	}
}
