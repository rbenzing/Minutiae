package evidence

import (
	"context"
	"strconv"
	"strings"
)

// Storage-class verification. SQLite keeps a value in the class it was stored
// with: a BLOB in a TEXT or INTEGER column stays a BLOB (no affinity converts it),
// and Go's Scan reads it back as the same string or number. Every digest, count
// and audit comparison verify makes is on the Go value, so such a value passes
// them all while SQLite compares it differently (a BLOB never equals a TEXT
// value and sorts after every number), which changes the answers of every query
// that filters, orders or joins on the column. STRICT tables refuse the value
// when it is written; verify must not rely on that (an attacker edits the
// schema), so each scan selects typeof() of every column it reads, in the same
// statement, and compares it with the class the schema defines.

const (
	classInteger = "integer"
	classText    = "text"
)

// colClass is the storage class a column holds.
type colClass struct {
	name  string
	class string // classInteger or classText
	null  bool   // NULL is also valid
}

// tableClasses lists the columns of a table with their classes, in the order of
// the typeof signature.
type tableClasses struct {
	table string
	cols  []colClass
}

func intCol(name string) colClass     { return colClass{name, classInteger, false} }
func intNullCol(name string) colClass { return colClass{name, classInteger, true} }
func textCol(name string) colClass    { return colClass{name, classText, false} }
func textNullCol(name string) colClass {
	return colClass{name, classText, true}
}

// The classes of every record table: the DDL of schema v2 (schema_v2.go) says
// the same through the column types and NOT NULL.
var (
	recordsClasses = tableClasses{"records", []colClass{
		intCol("id"), intCol("batch_id"), intCol("parser_id"), textCol("type"), intCol("payload_v"), textCol("artifact_id"),
		textNullCol("source_path"), textNullCol("locator"), intNullCol("src_offset"), intNullCol("src_length"),
		intNullCol("ts"), intNullCol("ts_end"), textNullCol("ts_basis"), intNullCol("tz_offset_min"),
		intCol("deleted"), intCol("recovered"), textNullCol("recovery_method"), intNullCol("confidence"),
		textCol("summary"), textNullCol("body"), textCol("payload"),
	}}
	timesClasses = tableClasses{"record_times", []colClass{
		intCol("record_id"), textCol("kind"), intCol("ts"), textCol("ts_basis"), intNullCol("tz_offset_min"),
	}}
	batchesClasses = tableClasses{"record_batches", []colClass{
		intCol("batch_id"), textCol("ingest_id"), intCol("batch_no"), intCol("first_id"), intCol("count"),
		textCol("digest"), textCol("created"),
	}}
	runsClasses = tableClasses{"record_runs", []colClass{
		intCol("end_seq"), textCol("ingest_id"), intCol("parser_id"), textNullCol("analysis_id"), textCol("outcome"),
		intCol("batches"), intCol("records"), intCol("first_id"), intCol("last_id"), textCol("rollup"), textCol("ended"),
	}}
	coverageClasses   = tableClasses{"record_run_artifacts", []colClass{textCol("ingest_id"), textCol("artifact_id")}}
	supersededClasses = tableClasses{"record_superseded", []colClass{textCol("ingest_id"), textCol("artifact_id")}}
	parsersClasses    = tableClasses{"parsers", []colClass{intCol("id"), textCol("name"), textCol("version"), textNullCol("hash")}}
	metaClasses       = tableClasses{"records_meta", []colClass{textCol("key"), textCol("value")}}
)

// signature returns the SQL expression that concatenates typeof() of every
// column, comma separated, qualified with prefix ("r." or "").
func (tc tableClasses) signature(prefix string) string {
	parts := make([]string, len(tc.cols))
	for i, c := range tc.cols {
		parts[i] = "typeof(" + prefix + c.name + ")"
	}
	return strings.Join(parts, " || ',' || ")
}

// check compares a signature read from the database with the schema's classes
// and reports every column of the row that differs; rowName names the row (its key,
// as scanned: never a class the schema does not allow for it).
func (tc tableClasses) check(ps *problemSet, sig string, rowName func() string) {
	rest := sig
	for i, c := range tc.cols {
		var got string
		var more bool
		got, rest, more = strings.Cut(rest, ",")
		if got == c.class || (c.null && got == "null") {
			if !more && i < len(tc.cols)-1 {
				ps.add("storage-class", "storage class: table %s, row %s: the class signature %q is shorter than the %d columns", tc.table, rowName(), sig, len(tc.cols))
				return
			}
			continue
		}
		want := c.class
		if c.null {
			want += " or NULL"
		}
		ps.add("storage-class", "storage class: table %s, row %s: column %q holds a %s value, want %s", tc.table, rowName(), c.name, got, want)
	}
}

// mismatchWhere is the WHERE condition that selects the rows with a column of
// the wrong class.
func (tc tableClasses) mismatchWhere() string {
	parts := make([]string, len(tc.cols))
	for i, c := range tc.cols {
		allowed := "'" + c.class + "'"
		if c.null {
			allowed += ", 'null'"
		}
		parts[i] = "typeof(" + c.name + ") NOT IN (" + allowed + ")"
	}
	return strings.Join(parts, " OR ")
}

// verifyClassOnly checks the classes of a table no scan of verify reads for
// another purpose (record_superseded, records_meta), without paging: at most
// verifyMaxPerKind+1 offending rows are read. keys are the columns that name a
// row in a problem; name formats their values.
func (c *Case) verifyClassOnly(ctx context.Context, ps *problemSet, tc tableClasses, keys []string, name func(vals []string) string) {
	type bad struct {
		sig  string
		keys []string
	}
	var found []bad
	sel := make([]string, len(keys))
	for i, k := range keys {
		sel[i] = "COALESCE(CAST(" + k + " AS TEXT), '')"
	}
	q := `SELECT ` + tc.signature("") + `, ` + strings.Join(sel, ", ") + ` FROM ` + tc.table +
		` WHERE ` + tc.mismatchWhere() + ` LIMIT ` + strconv.Itoa(verifyMaxPerKind+1)
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		found = nil
		rows, err := h.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			b := bad{keys: make([]string, len(keys))}
			dest := make([]any, 0, len(keys)+1)
			dest = append(dest, &b.sig)
			for i := range b.keys {
				dest = append(dest, &b.keys[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			found = append(found, b)
		}
		return rows.Err()
	})
	if err != nil {
		unreadable(ps.rep, tc.table+" storage classes", err)
		return
	}
	for _, b := range found {
		tc.check(ps, b.sig, func() string { return name(b.keys) })
	}
}
