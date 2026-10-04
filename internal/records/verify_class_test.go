package records_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Storage-class tampering (final review C1): a BLOB in a TEXT or INTEGER column
// is read back by Go's Scan as the same string or number, so every digest, count
// and audit comparison still holds, while SQLite compares a BLOB with a different
// ordering (a BLOB never equals a TEXT value) and the reader's answers change.

// classCase is the base case plus a third ingest (parser sms-parser 2.0) that
// supersedes ingest 1, so record_superseded holds one pair.
func classCase(t *testing.T) (base baseCase, supersededIngest string) {
	t.Helper()
	base = buildBaseCase(t)
	c := openClone(t, base.dir)
	p2 := records.Parser{Name: testParser.Name, Version: "2.0", Hash: "abc999"}
	recordstest.Ingest(t, c, p2, []string{base.art.ID}, recordstest.Records(base.art.ID, 2, 9))
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("the class case is not clean: %q", rep.Problems)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return base, base.ing1
}

func TestVerifyDetectsStorageClassTampering(t *testing.T) {
	base, supIngest := classCase(t)
	art := base.art.ID
	// One cell per column of every record table (the columns SQLite cannot hold
	// as anything but an integer, the rowid aliases, are skipped). where picks one
	// row; row is how the problem must name it.
	type cell struct {
		table, column string
		where         string
		args          []any
		row           string
		want          string // the storage class the column must hold
	}
	rec := func(col, want string) cell {
		return cell{"records", col, "id = 1", nil, "id 1", want}
	}
	tm := func(col, want string) cell {
		return cell{"record_times", col, "record_id = 1 AND kind = 'created'", nil, `record 1 kind "created"`, want}
	}
	batch := cell{table: "record_batches", where: "ingest_id = ? AND batch_no = 1", args: []any{base.ing1}, row: fmt.Sprintf("ingest %q batch 1", base.ing1)}
	bt := func(col, want string) cell { c := batch; c.column, c.want = col, want; return c }
	run := func(col, want string) cell {
		return cell{"record_runs", col, "ingest_id = ?", []any{base.ing1}, fmt.Sprintf("ingest %q", base.ing1), want}
	}
	cells := []cell{
		rec("batch_id", "integer"), rec("type", "text"), rec("payload_v", "integer"), rec("artifact_id", "text"),
		rec("source_path", "text or NULL"), rec("locator", "text or NULL"), rec("src_offset", "integer or NULL"),
		rec("src_length", "integer or NULL"), rec("ts", "integer or NULL"), rec("ts_end", "integer or NULL"),
		rec("ts_basis", "text or NULL"), rec("tz_offset_min", "integer or NULL"), rec("deleted", "integer"),
		rec("recovered", "integer"), rec("recovery_method", "text or NULL"), rec("confidence", "integer or NULL"),
		rec("parser_id", "integer"), rec("summary", "text"), rec("body", "text or NULL"), rec("payload", "text"),

		tm("record_id", "integer"), tm("kind", "text"), tm("ts", "integer"), tm("ts_basis", "text"), tm("tz_offset_min", "integer or NULL"),

		bt("ingest_id", "text"), bt("batch_no", "integer"), bt("first_id", "integer"), bt("count", "integer"),
		bt("digest", "text"), bt("created", "text"),

		run("ingest_id", "text"), run("parser_id", "integer"), run("outcome", "text"), run("batches", "integer"),
		run("records", "integer"), run("first_id", "integer"), run("last_id", "integer"), run("rollup", "text"), run("ended", "text"),

		{"record_run_artifacts", "ingest_id", "ingest_id = ?", []any{base.ing1}, fmt.Sprintf("ingest %q artifact %q", base.ing1, art), "text"},
		{"record_run_artifacts", "artifact_id", "ingest_id = ?", []any{base.ing1}, fmt.Sprintf("ingest %q artifact %q", base.ing1, art), "text"},
		{"record_superseded", "ingest_id", "ingest_id = ?", []any{supIngest}, fmt.Sprintf("ingest %q artifact %q", supIngest, art), "text"},
		{"record_superseded", "artifact_id", "ingest_id = ?", []any{supIngest}, fmt.Sprintf("ingest %q artifact %q", supIngest, art), "text"},

		{"parsers", "name", "id = 1", nil, "id 1", "text"},
		{"parsers", "version", "id = 1", nil, "id 1", "text"},
		{"parsers", "hash", "id = 1", nil, "id 1", "text or NULL"},

		{"records_meta", "key", "key = 'next_id'", nil, `key "next_id"`, "text"},
		{"records_meta", "value", "key = 'next_id'", nil, `key "next_id"`, "text"},
	}
	for _, cl := range cells {
		t.Run(cl.table+"."+cl.column, func(t *testing.T) {
			dir := cloneCase(t, base.dir)
			recordstest.SetStorageClass(t, dir, cl.table, cl.column, cl.where, cl.args...)
			rep := mustVerify(t, openClone(t, dir))
			want := fmt.Sprintf(`table %s, row %s: column %q holds a blob value, want %s`, cl.table, cl.row, cl.column, cl.want)
			expectProblems(t, rep, []string{want}, append([]string{"_check failed"}, sideEffects[cl.table+"."+cl.column]...)...)
		})
	}
}

// TestStorageClassTamperingChangesReaderButVerifyFlagsIt is the reviewer's
// demonstration: with ingest_id of a run's batch stored as a BLOB, the reader's
// supersession join no longer matches and the default list resurfaces rows of a
// superseded run, while every digest still holds. Verify must say so.
func TestStorageClassTamperingChangesReaderButVerifyFlagsIt(t *testing.T) {
	base, _ := classCase(t)
	dir := cloneCase(t, base.dir)
	c := openClone(t, dir)
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.List(ctx, records.Filter{}, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.SetStorageClass(t, dir, "record_batches", "ingest_id", "ingest_id = ?", base.ing1)
	c = openClone(t, dir)
	r, err = records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	after, err := r.List(ctx, records.Filter{}, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Rows) == len(before.Rows) {
		t.Fatalf("the tampering was meant to change the default list (%d rows both times)", len(before.Rows))
	}
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{fmt.Sprintf(`table record_batches, row ingest %q batch 1: column "ingest_id" holds a blob value, want text`, base.ing1)}, "_check failed")
}

// TestVerifyTerminatesOnBlobRunArtifactRows: 6000 BLOB-class ingest_id rows (more
// than one verify chunk) used to make the coverage scan return the same chunk
// forever; verify must end and name the class problem.
func TestVerifyTerminatesOnBlobRunArtifactRows(t *testing.T) {
	base := buildBaseCase(t)
	dir := cloneCase(t, base.dir)
	recordstest.InjectBlobRunArtifacts(t, dir, 6000)
	c := openClone(t, dir)
	type result struct {
		rep evidence.VerifyReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := c.Verify()
		done <- result{rep, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		joined := strings.Join(res.rep.Problems, "\n")
		for _, want := range []string{
			`table record_run_artifacts, row ingest "ing-blob-00000001" artifact "x": column "ingest_id" holds a blob value, want text`,
			"scan of record_run_artifacts cannot make progress",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("no problem contains %q; problems: %.2000q", want, res.rep.Problems)
			}
		}
	case <-time.After(60 * time.Second):
		t.Fatal("verify did not return within 60s: the coverage scan loops forever on BLOB-class rows")
	}
}

// supersessionJoin: the recomputation of the supersession pairs joins on the
// changed column, so a BLOB there makes the pairs differ.
var supersessionJoin = []string{"record_superseded holds a pair no run implies", "record_superseded is missing the pair"}

// sideEffects are the other problems a class change causes by itself: a join on
// the column no longer matches (the parser, the supersession recomputation) or
// the lookup by key finds nothing.
var sideEffects = map[string][]string{
	"records.parser_id":                {"does not exist in parsers", "digest mismatch"},
	"record_runs.parser_id":            append([]string{"does not exist in parsers", "differs from the audit log"}, supersessionJoin...),
	"record_runs.ingest_id":            supersessionJoin,
	"record_run_artifacts.ingest_id":   supersessionJoin,
	"record_run_artifacts.artifact_id": supersessionJoin,
	"record_superseded.ingest_id":      supersessionJoin,
	"record_superseded.artifact_id":    supersessionJoin,
	"parsers.name":                     supersessionJoin,
	"records_meta.key":                 {"records_meta next_id", "records_meta holds the key"},
}
