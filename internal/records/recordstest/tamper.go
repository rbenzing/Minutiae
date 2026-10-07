package recordstest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Tamper helpers: named functions that change a case the way an attacker with
// file access would, so verify's detection can be tested. They take the case
// directory and work through their own connection to artifacts.db, opened with
// foreign_keys=OFF. Every helper drops the immutability triggers, tampers and
// then RE-CREATES the triggers from their expected definitions (a careful
// attacker restores them), so the tampering under test is the only problem left;
// DropImmutabilityTriggers is the one helper that leaves them dropped.

// SetNextID overwrites records_meta.next_id (the one mutable record table), as
// a stale or tampered counter would be: the writer must not trust it alone.
func SetNextID(t testing.TB, c *evidence.Case, v int64) {
	t.Helper()
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, strconv.FormatInt(v, 10))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// openTamperDB opens a second connection to the case's artifacts.db.
func openTamperDB(t testing.TB, caseDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(caseDir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func dropTriggers(db *sql.DB) error {
	for _, tr := range evidence.ImmutabilityTriggers() {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS "` + tr.Name + `"`); err != nil {
			return err
		}
	}
	return nil
}

func createTriggers(db *sql.DB) error {
	for _, tr := range evidence.ImmutabilityTriggers() {
		if _, err := db.Exec(tr.SQL); err != nil {
			return err
		}
	}
	return nil
}

// tamper runs fn with the triggers dropped and restores them afterwards.
func tamper(t testing.TB, caseDir string, fn func(db *sql.DB) error) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if err := dropTriggers(db); err != nil {
		t.Fatalf("drop triggers: %v", err)
	}
	ferr := fn(db)
	if err := createTriggers(db); err != nil {
		t.Fatalf("restore triggers: %v", err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
}

func execAll(db *sql.DB, stmts ...[]any) error {
	for _, s := range stmts {
		if _, err := db.Exec(s[0].(string), s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// DropImmutabilityTriggers drops all 16 immutability triggers and leaves them
// dropped.
func DropImmutabilityTriggers(t testing.TB, caseDir string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if err := dropTriggers(db); err != nil {
		t.Fatal(err)
	}
}

// DropTrigger drops one immutability trigger and leaves it dropped.
func DropTrigger(t testing.TB, caseDir, name string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
		t.Fatal(err)
	}
}

// ReplaceTrigger leaves the named trigger in place under the same name with a
// weaker body (one that no longer blocks anything).
func ReplaceTrigger(t testing.TB, caseDir, name, table, event string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER "` + name + `" BEFORE ` + event + ` ON ` + table + ` BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
}

// recordColumns are the columns SetRecordColumn may change.
var recordColumns = []string{
	"type", "payload_v", "artifact_id", "source_path", "locator", "src_offset", "src_length", "ts", "ts_end",
	"ts_basis", "tz_offset_min", "deleted", "recovered", "recovery_method", "confidence", "summary", "body",
	"payload", "parser_id", "batch_id",
}

// SetRecordColumn sets one column of a record row (value nil sets NULL). The
// column must be one of the stored columns; the table's CHECK constraints still apply.
func SetRecordColumn(t testing.TB, caseDir string, id int64, column string, value any) {
	t.Helper()
	SetRecordColumns(t, caseDir, id, map[string]any{column: value})
}

// SetRecordColumns sets several columns of a record row at once (a nil value
// sets NULL), for changes the CHECK constraints only allow together.
func SetRecordColumns(t testing.TB, caseDir string, id int64, values map[string]any) {
	t.Helper()
	cols := make([]string, 0, len(values))
	for c := range values {
		if !slices.Contains(recordColumns, c) {
			t.Fatalf("recordstest: %q is not a records column the tamper helpers may change", c)
		}
		cols = append(cols, c)
	}
	slices.Sort(cols)
	var set string
	args := make([]any, 0, len(cols)+1)
	for i, c := range cols {
		if i > 0 {
			set += ", "
		}
		set += c + " = ?"
		args = append(args, values[c])
	}
	args = append(args, id)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE records SET `+set+` WHERE id = ?`, args...) //nolint:gosec // column names are whitelisted above
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("record %d: %d rows changed", id, n)
		}
		return nil
	})
}

// SetRecordSummary changes the summary of a record in place.
func SetRecordSummary(t testing.TB, caseDir string, id int64, summary string) {
	t.Helper()
	SetRecordColumn(t, caseDir, id, "summary", summary)
}

// RepointRecord makes a record point at another artifact.
func RepointRecord(t testing.TB, caseDir string, id int64, artifactID string) {
	t.Helper()
	SetRecordColumn(t, caseDir, id, "artifact_id", artifactID)
}

// SetRecordParser makes a record point at another parsers row.
func SetRecordParser(t testing.TB, caseDir string, id, parserID int64) {
	t.Helper()
	SetRecordColumn(t, caseDir, id, "parser_id", parserID)
}

// SetRecordTime changes the ts of an existing record_times row.
func SetRecordTime(t testing.TB, caseDir string, id int64, kind string, ts int64) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE record_times SET ts = ? WHERE record_id = ? AND kind = ?`, ts, id, kind)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("record %d time %q: %d rows changed", id, kind, n)
		}
		return nil
	})
}

// InjectRecordTime adds a record_times row.
func InjectRecordTime(t testing.TB, caseDir string, id int64, kind string, ts int64) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO record_times (record_id, kind, ts, ts_basis, tz_offset_min) VALUES (?, ?, ?, 'utc', NULL)`, id, kind, ts)
		return err
	})
}

// DeleteRecordTime removes one record_times row.
func DeleteRecordTime(t testing.TB, caseDir string, id int64, kind string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`DELETE FROM record_times WHERE record_id = ? AND kind = ?`, id, kind)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("record %d time %q: %d rows deleted", id, kind, n)
		}
		return nil
	})
}

// DeleteRecord removes a record row and its record_times rows.
func DeleteRecord(t testing.TB, caseDir string, id int64) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		return execAll(db,
			[]any{`DELETE FROM record_times WHERE record_id = ?`, id},
			[]any{`DELETE FROM records WHERE id = ?`, id})
	})
}

// DeleteRecordKeepTimes removes a record row but leaves its record_times rows
// behind (orphans).
func DeleteRecordKeepTimes(t testing.TB, caseDir string, id int64) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`DELETE FROM records WHERE id = ?`, id)
		return err
	})
}

// DeleteBatch removes a record_batches row and every record (and time) of it.
func DeleteBatch(t testing.TB, caseDir, ingestID string, batchNo int) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		var batchID int64
		if err := db.QueryRow(`SELECT batch_id FROM record_batches WHERE ingest_id = ? AND batch_no = ?`, ingestID, batchNo).Scan(&batchID); err != nil {
			return err
		}
		return execAll(db,
			[]any{`DELETE FROM record_times WHERE record_id IN (SELECT id FROM records WHERE batch_id = ?)`, batchID},
			[]any{`DELETE FROM records WHERE batch_id = ?`, batchID},
			[]any{`DELETE FROM record_batches WHERE batch_id = ?`, batchID})
	})
}

// DeleteBatchRowOnly removes a record_batches row and leaves its records.
func DeleteBatchRowOnly(t testing.TB, caseDir, ingestID string, batchNo int) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`DELETE FROM record_batches WHERE ingest_id = ? AND batch_no = ?`, ingestID, batchNo)
		return err
	})
}

// InjectRecord inserts a minimal record row (type event, empty payload, the
// first parsers row) with the given id, batch id and artifact.
func InjectRecord(t testing.TB, caseDir string, id, batchID int64, artifactID, summary string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, summary, payload)
			VALUES (?, ?, 'event', 1, ?, (SELECT min(id) FROM parsers), ?, '{}')`, id, batchID, artifactID, summary)
		return err
	})
}

// InjectBatchRow inserts a record_batches row and returns its batch id.
func InjectBatchRow(t testing.TB, caseDir, ingestID string, batchNo int, firstID int64, count int, digest string) int64 {
	t.Helper()
	var batchID int64
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES (?, ?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
			ingestID, batchNo, firstID, count, digest)
		if err != nil {
			return err
		}
		batchID, err = res.LastInsertId()
		return err
	})
	return batchID
}

// CopyBatchRow inserts a copy of a real record_batches row under a new batch_no
// (its first_id, count and digest are those of the original) and returns the new
// batch id.
func CopyBatchRow(t testing.TB, caseDir, ingestID string, batchNo, newBatchNo int) int64 {
	t.Helper()
	var batchID int64
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created)
			SELECT ingest_id, ?, first_id, count, digest, created FROM record_batches WHERE ingest_id = ? AND batch_no = ?`, newBatchNo, ingestID, batchNo)
		if err != nil {
			return err
		}
		batchID, err = res.LastInsertId()
		return err
	})
	return batchID
}

// SetBatchDigest changes the digest stored in a record_batches row.
func SetBatchDigest(t testing.TB, caseDir, ingestID string, batchNo int, digest string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE record_batches SET digest = ? WHERE ingest_id = ? AND batch_no = ?`, digest, ingestID, batchNo)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("batch %d of %s: %d rows changed", batchNo, ingestID, n)
		}
		return nil
	})
}

// BatchID returns the batch id of a record_batches row.
func BatchID(t testing.TB, c *evidence.Case, ingestID string, batchNo int) int64 {
	t.Helper()
	var id int64
	err := c.ReadTx(context.Background(), func(h evidence.ReadHandle) error {
		return h.QueryRow(`SELECT batch_id FROM record_batches WHERE ingest_id = ? AND batch_no = ?`, ingestID, batchNo).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const manifestName = "manifest.jsonl"

func readManifestLines(t testing.TB, caseDir string) []evidence.ManifestRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(caseDir, manifestName)) //nolint:gosec // test helper on a case directory the test created
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r evidence.ManifestRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeManifestLines(t testing.TB, caseDir string, recs []evidence.ManifestRecord) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(caseDir, manifestName), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// RemoveArtifactEverywhere erases an artifact from the manifest and from
// artifacts.db (the file under artifacts/ is left where it is).
func RemoveArtifactEverywhere(t testing.TB, caseDir, artifactID string) {
	t.Helper()
	recs := readManifestLines(t, caseDir)
	kept := recs[:0]
	for _, r := range recs {
		if r.ID != artifactID {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(recs) {
		t.Fatalf("recordstest: artifact %q is not in the manifest", artifactID)
	}
	writeManifestLines(t, caseDir, kept)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`DELETE FROM artifacts WHERE id = ?`, artifactID)
		return err
	})
}

// RewriteArtifactConsistently replaces the bytes of an artifact and brings the
// manifest record and the artifacts.db row (size, SHA-256, MD5) in line with
// them, as a forger who fixes every copy of the hash would.
func RewriteArtifactConsistently(t testing.TB, caseDir, artifactID string, newBytes []byte) {
	t.Helper()
	recs := readManifestLines(t, caseDir)
	i := slices.IndexFunc(recs, func(r evidence.ManifestRecord) bool { return r.ID == artifactID })
	if i < 0 {
		t.Fatalf("recordstest: artifact %q is not in the manifest", artifactID)
	}
	full := filepath.Join(caseDir, filepath.FromSlash(recs[i].Path))
	if err := os.WriteFile(full, newBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := evidence.HashFile(full)
	if err != nil {
		t.Fatal(err)
	}
	recs[i].Size, recs[i].SHA256, recs[i].MD5 = d.Size, d.SHA256, d.MD5
	writeManifestLines(t, caseDir, recs)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE artifacts SET size = ?, sha256 = ?, md5 = ? WHERE id = ?`, d.Size, d.SHA256, d.MD5, artifactID)
		return err
	})
}

// DropTable drops a table of artifacts.db (its triggers go with it and are not
// restored: the table is gone).
func DropTable(t testing.TB, caseDir, table string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DROP TABLE "` + table + `"`); err != nil {
		t.Fatal(err)
	}
}

// runColumns are the record_runs columns SetRunColumn may change.
var runColumns = []string{
	"end_seq", "parser_id", "analysis_id", "outcome", "batches", "records", "first_id", "last_id", "rollup", "ended",
}

// SetRunColumn sets one column of a record_runs row (value nil sets NULL).
func SetRunColumn(t testing.TB, caseDir, ingestID, column string, value any) {
	t.Helper()
	if !slices.Contains(runColumns, column) {
		t.Fatalf("recordstest: %q is not a record_runs column the tamper helpers may change", column)
	}
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE record_runs SET `+column+` = ? WHERE ingest_id = ?`, value, ingestID) //nolint:gosec // column name is whitelisted above
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("run of %s: %d rows changed", ingestID, n)
		}
		return nil
	})
}

// DeleteRun erases a run: its record_runs row and its record_run_artifacts rows
// (the records and the supersession rows are left as they are).
func DeleteRun(t testing.TB, caseDir, ingestID string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		return execAll(db,
			[]any{`DELETE FROM record_run_artifacts WHERE ingest_id = ?`, ingestID},
			[]any{`DELETE FROM record_runs WHERE ingest_id = ?`, ingestID})
	})
}

// InsertRun inserts a record_runs row by hand (no audit entry): the parser is
// looked up by name and version.
func InsertRun(t testing.TB, caseDir string, endSeq int64, ingestID, parserName, parserVersion, outcome string, batches, records int, firstID, lastID int64, rollup string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO record_runs (end_seq, ingest_id, parser_id, analysis_id, outcome, batches, records, first_id, last_id, rollup, ended)
			VALUES (?, ?, (SELECT id FROM parsers WHERE name = ? AND version = ?), NULL, ?, ?, ?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
			endSeq, ingestID, parserName, parserVersion, outcome, batches, records, firstID, lastID, rollup)
		return err
	})
}

// InjectRunArtifact adds a record_run_artifacts row.
func InjectRunArtifact(t testing.TB, caseDir, ingestID, artifactID string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO record_run_artifacts (ingest_id, artifact_id) VALUES (?, ?)`, ingestID, artifactID)
		return err
	})
}

// DeleteRunArtifact removes one record_run_artifacts row.
func DeleteRunArtifact(t testing.TB, caseDir, ingestID, artifactID string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`DELETE FROM record_run_artifacts WHERE ingest_id = ? AND artifact_id = ?`, ingestID, artifactID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("run coverage (%s, %s): %d rows deleted", ingestID, artifactID, n)
		}
		return nil
	})
}

// DeleteSuperseded removes a record_superseded pair, which resurrects the records
// of that ingest for that artifact.
func DeleteSuperseded(t testing.TB, caseDir, ingestID, artifactID string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`DELETE FROM record_superseded WHERE ingest_id = ? AND artifact_id = ?`, ingestID, artifactID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("superseded (%s, %s): %d rows deleted", ingestID, artifactID, n)
		}
		return nil
	})
}

// InjectSuperseded adds a record_superseded pair, which hides the records of that
// ingest for that artifact.
func InjectSuperseded(t testing.TB, caseDir, ingestID, artifactID string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO record_superseded (ingest_id, artifact_id) VALUES (?, ?)`, ingestID, artifactID)
		return err
	})
}

// InjectParser adds a parsers row no audit entry names.
func InjectParser(t testing.TB, caseDir, name, version, hash string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO parsers (name, version, hash) VALUES (?, ?, ?)`, name, version, hash)
		return err
	})
}

// ReplaceInAuditLine replaces old by new inside the audit line with sequence
// number seq (line seq of audit.jsonl), keeping the rest of the line as it is: a
// hand edit of an audit entry, which breaks the hash chain. It fails the test
// when the line does not hold old.
func ReplaceInAuditLine(t testing.TB, caseDir string, seq int, old, replacement string) {
	t.Helper()
	p := filepath.Join(caseDir, "audit.jsonl")
	b, err := os.ReadFile(p) //nolint:gosec // test helper on a case directory the test created
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(b, []byte("\n"))
	if seq < 1 || seq > len(lines) {
		t.Fatalf("recordstest: audit line %d does not exist", seq)
	}
	line := lines[seq-1]
	if !bytes.Contains(line, []byte(old)) {
		t.Fatalf("recordstest: audit line %d does not contain %q: %s", seq, old, line)
	}
	lines[seq-1] = bytes.Replace(line, []byte(old), []byte(replacement), 1)
	if err := os.WriteFile(p, bytes.Join(lines, []byte("\n")), 0o600); err != nil { //nolint:gosec // test helper on a case directory the test created
		t.Fatal(err)
	}
}

// batchColumns, parserColumns, timeColumns and recordColumns+id are the columns
// the Set*Column helpers may change.
var (
	batchColumns  = []string{"batch_id", "ingest_id", "batch_no", "first_id", "count", "digest", "created"}
	parserColumns = []string{"id", "name", "version", "hash"}
	timeColumns   = []string{"record_id", "kind", "ts", "ts_basis", "tz_offset_min"}
)

func setOne(t testing.TB, caseDir, table string, allowed []string, column string, value any, where string, args ...any) {
	t.Helper()
	if !slices.Contains(allowed, column) {
		t.Fatalf("recordstest: %q is not a %s column the tamper helpers may change", column, table)
	}
	tamper(t, caseDir, func(db *sql.DB) error {
		q := `UPDATE ` + table + ` SET ` + column + ` = ? WHERE ` + where //nolint:gosec // table and column names are whitelisted by the callers
		res, err := db.Exec(q, append([]any{value}, args...)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s %s: %d rows changed", table, column, n)
		}
		return nil
	})
}

// SetBatchColumn sets one column of a record_batches row (value nil sets NULL).
func SetBatchColumn(t testing.TB, caseDir, ingestID string, batchNo int, column string, value any) {
	t.Helper()
	setOne(t, caseDir, "record_batches", batchColumns, column, value, `ingest_id = ? AND batch_no = ?`, ingestID, batchNo)
}

// SetParserColumn sets one column of a parsers row (value nil sets NULL).
func SetParserColumn(t testing.TB, caseDir string, parserID int64, column string, value any) {
	t.Helper()
	setOne(t, caseDir, "parsers", parserColumns, column, value, `id = ?`, parserID)
}

// SetRecordTimeColumn sets one column of a record_times row (value nil sets NULL).
func SetRecordTimeColumn(t testing.TB, caseDir string, recordID int64, kind, column string, value any) {
	t.Helper()
	setOne(t, caseDir, "record_times", timeColumns, column, value, `record_id = ? AND kind = ?`, recordID, kind)
}

// SetRecordID changes the id of a record row (its record_times rows keep the old id).
func SetRecordID(t testing.TB, caseDir string, id, newID int64) {
	t.Helper()
	setOne(t, caseDir, "records", []string{"id"}, "id", newID, `id = ?`, id)
}

// withoutStrict runs fn with the table's STRICT keyword removed from its stored
// definition, then puts the original definition back. An attacker who wants a
// BLOB in an INTEGER column has to do exactly this (STRICT refuses the value);
// restoring the text leaves the class tampering as the only change, so verify's
// storage-class checks are tested on their own. The schema is edited through
// writable_schema, which only takes effect on a new connection: each step opens
// its own.
func withoutStrict(t testing.TB, caseDir, table string, fn func(db *sql.DB) error) {
	t.Helper()
	orig := schemaSQL(t, caseDir, "table", table)
	loose := strings.NewReplacer(" STRICT, WITHOUT ROWID", " WITHOUT ROWID", " STRICT", "").Replace(orig)
	setSchemaSQL(t, caseDir, "table", table, loose)
	tamper(t, caseDir, func(db *sql.DB) error {
		if _, err := db.Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		return fn(db)
	})
	setSchemaSQL(t, caseDir, "table", table, orig)
}

// SetStorageClass changes the storage class of one column of the rows matching
// where (all rows when where is empty) to BLOB, keeping the bytes of the value
// (CAST(col AS BLOB)); NULLs stay NULL. SQLite keeps a BLOB in a TEXT or INTEGER
// column as a BLOB, and Go's Scan reads it back as the same string or number, so
// only a storage-class check sees it. The table's STRICT keyword and CHECK
// constraints are bypassed while the change is made and restored afterwards. It
// fails the test unless at least one row changed, and returns how many did.
func SetStorageClass(t testing.TB, caseDir, table, column, where string, args ...any) int64 {
	t.Helper()
	if where == "" {
		where = "1"
	}
	var n int64
	withoutStrict(t, caseDir, table, func(db *sql.DB) error {
		q := `UPDATE ` + table + ` SET ` + column + ` = CAST(` + column + ` AS BLOB) WHERE typeof(` + column + `) <> 'null' AND (` + where + `)` //nolint:gosec // test helper; names come from the test
		res, err := db.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if n == 0 {
		t.Fatalf("recordstest: no %s.%s value matched %q", table, column, where)
	}
	return n
}

// InjectBlobRunArtifacts adds n record_run_artifacts rows whose ingest_id is a
// BLOB (distinct, sorted after every TEXT value): a hostile table that makes a
// keyset scan binding the last key as TEXT return the same rows forever.
func InjectBlobRunArtifacts(t testing.TB, caseDir string, n int) {
	t.Helper()
	withoutStrict(t, caseDir, "record_run_artifacts", func(db *sql.DB) error {
		_, err := db.Exec(`WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
			INSERT INTO record_run_artifacts (ingest_id, artifact_id) SELECT CAST('ing-blob-' || printf('%08d', i) AS BLOB), 'x' FROM s`, n)
		return err
	})
}

// schemaSQL returns the stored definition of a schema object.
func schemaSQL(t testing.TB, caseDir, typ, name string) string {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	var def string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, typ, name).Scan(&def); err != nil {
		t.Fatalf("recordstest: %s %q: %v", typ, name, err)
	}
	return def
}

// setSchemaSQL rewrites the stored definition of a schema object through
// writable_schema, leaving its b-tree as it is. SQLite only reads the new text on
// a new connection, so this opens (and closes) its own.
func setSchemaSQL(t testing.TB, caseDir, typ, name, def string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`PRAGMA writable_schema = ON`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE sqlite_master SET sql = ? WHERE type = ? AND name = ?`, def, typ, name)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("recordstest: %s %q: %d schema rows changed", typ, name, n)
	}
}

// RedefineSchemaObject replaces the stored definition of a table, index or
// trigger (writable_schema), as an attacker redefining `records_deleted` over
// another predicate would: the rows and the index b-tree stay, the queries that
// use the object now mean something else.
func RedefineSchemaObject(t testing.TB, caseDir, typ, name, newSQL string) {
	t.Helper()
	setSchemaSQL(t, caseDir, typ, name, newSQL)
}

// SchemaSQL returns the stored definition of a schema object (for building a
// redefinition from the real one).
func SchemaSQL(t testing.TB, caseDir, typ, name string) string {
	t.Helper()
	return schemaSQL(t, caseDir, typ, name)
}

// CreateSchemaObject executes DDL that adds an object (a view, an extra table,
// index or trigger) to the case's artifacts.db.
func CreateSchemaObject(t testing.TB, caseDir, ddl string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
}

// DropSchemaObject drops an index or view (`DROP INDEX` / `DROP VIEW`).
func DropSchemaObject(t testing.TB, caseDir, typ, name string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DROP ` + strings.ToUpper(typ) + ` "` + name + `"`); err != nil { //nolint:gosec // test helper; typ and name come from the test
		t.Fatal(err)
	}
}

// DesyncIndex makes an index disagree with its table while its stored definition
// stays exactly as it was: the index is rebuilt (REINDEX) under another
// definition and the original text is put back. The schema text then passes a
// comparison, `PRAGMA quick_check` says "ok" and only `PRAGMA integrity_check`,
// which checks every index against its table, sees the desynchronised b-tree.
func DesyncIndex(t testing.TB, caseDir, index, otherDef string) {
	t.Helper()
	orig := schemaSQL(t, caseDir, "index", index)
	setSchemaSQL(t, caseDir, "index", index, otherDef)
	db := openTamperDB(t, caseDir)
	if _, err := db.Exec(`REINDEX "` + index + `"`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	setSchemaSQL(t, caseDir, "index", index, orig)
}

// InjectMetaKey adds a records_meta row under another key (the one table that
// holds only the next_id counter).
func InjectMetaKey(t testing.TB, caseDir, key, value string) {
	t.Helper()
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO records_meta (key, value) VALUES (?, ?)`, key, value)
		return err
	})
}

// SetManifestPath changes the path of an artifact's manifest record (the file and
// artifacts.db are left as they are), so `case verify` reports it with the text
// given.
func SetManifestPath(t testing.TB, caseDir, artifactID, path string) {
	t.Helper()
	recs := readManifestLines(t, caseDir)
	i := slices.IndexFunc(recs, func(r evidence.ManifestRecord) bool { return r.ID == artifactID })
	if i < 0 {
		t.Fatalf("recordstest: artifact %q is not in the manifest", artifactID)
	}
	recs[i].Path = path
	writeManifestLines(t, caseDir, recs)
}

// DuplicateManifestRecord appends a second manifest line holding the same record (same id and
// path) as artifactID, which verify and OpenArtifact refuse as a duplicate id.
func DuplicateManifestRecord(t testing.TB, caseDir, artifactID string) {
	t.Helper()
	recs := readManifestLines(t, caseDir)
	for _, r := range recs {
		if r.ID == artifactID {
			writeManifestLines(t, caseDir, append(recs, r))
			return
		}
	}
	t.Fatalf("no manifest record %q", artifactID)
}
