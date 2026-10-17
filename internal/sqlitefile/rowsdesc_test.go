package sqlitefile_test

// History rows of a WITHOUT ROWID table with DESC key columns (LX-4 Task 10, L108 item 3): the
// engine writes the database, the expectations come from the engine's own content of the live table.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// descScenario: t(x real, y integer, z) WITHOUT ROWID with PRIMARY KEY(y DESC, x DESC), x holding
// integral values (stored as integers), 38 rows. After a checkpoint, the rows with an even y are
// rewritten (same length), so the database file holds their old versions under the WAL's new pages.
// It returns the files and the number of rewritten rows.
func descScenario(t *testing.T, ddl string) (db, wal []byte, changed int) {
	t.Helper()
	r := newWALRun(t, 512, 0, 0)
	r.tx(ddl)
	var ins []string
	for i := range 38 {
		ins = append(ins, fmt.Sprintf("insert into t values(%d, %d, 'zzzz-%03d')", i*3, 100-i, i))
	}
	r.tx(ins...)
	var busy, logFrames, ckpt int
	if err := r.db.QueryRow("pragma wal_checkpoint(truncate)").Scan(&busy, &logFrames, &ckpt); err != nil || busy != 0 {
		t.Fatalf("checkpoint: %v busy %d", err, busy)
	}
	r.tx("update t set z = 'w' || substr(z, 2) where y % 2 = 0")
	if err := r.db.QueryRow("select count(*) from t where z like 'w%'").Scan(&changed); err != nil {
		t.Fatal(err)
	}
	db, wal = r.snap()
	return db, wal, changed
}

const descDDL = "create table t(x real, y integer, z, primary key(y desc, x desc)) without rowid"

// TestWithoutRowidDescKeyRowsKeepTheSchemaBasis: the history rows of the page that is t's today are
// t's, on the schema basis, with no owner-changed note, and the comparison with the live rows
// decides their relation. The expectation is the engine's: exactly the rows it rewrote.
func TestWithoutRowidDescKeyRowsKeepTheSchemaBasis(t *testing.T) {
	db, wal, changed := descScenario(t, descDDL)
	if changed == 0 {
		t.Fatal("the scenario rewrote nothing")
	}
	_, h := openAll(t, db, wal, nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if r.Table != "t" {
			continue
		}
		n++
		if r.TableBasis != sqlitefile.BasisSchema || hasNote(r, sqlitefile.NoteOwnerChanged) || r.Relation != sqlitefile.RelSupersededVersion {
			t.Errorf("row %v: basis %s relation %s notes %v, want schema, superseded-version, no owner-changed", r.Values, r.TableBasis, r.Relation, r.Notes)
		}
		if s, ok := r.Values[2].Text(); !ok || !strings.HasPrefix(s, "zzzz-") {
			t.Errorf("row %v: not an old version", r.Values)
		}
	}
	if n != changed {
		t.Errorf("%d history rows of t, want the %d rows the engine rewrote (all rows: %v)", n, changed, methods(rows))
	}
}

// TestWithoutRowidDescKeyVariantsKeepTheSchemaBasis: the same holds across key shapes (DESC on
// one or both key columns, the key columns declared after another column, a REAL key column) and
// kinds of change (a rewrite, a deleted row, a key that changes), in the write-ahead log.
func TestWithoutRowidDescKeyVariantsKeepTheSchemaBasis(t *testing.T) {
	ddls := []struct{ name, ddl, ins string }{
		{"both desc, no other column", "create table t(x real, y integer, primary key(y desc, x desc)) without rowid", "insert into t values(%d, %d)"},
		{"x desc first", "create table t(x real, y integer, z, primary key(x desc, y desc)) without rowid", "insert into t values(%d, %d, 'zz')"},
		{"key declared after another column", "create table t(z, x real, y integer, primary key(y desc, x desc)) without rowid", "insert into t values('zz', %d, %d)"},
		{"one desc", "create table t(x real, y integer, z, primary key(y desc, x)) without rowid", "insert into t values(%d, %d, 'zz')"},
	}
	updates := []struct{ name, stmt string }{
		{"key change", "update t set y = y + 1000 where y % 2 = 0"},
		{"delete", "delete from t where y % 2 = 0"},
		{"real key value", "update t set x = x + 0.5 where y % 2 = 0"},
	}
	for _, d := range ddls {
		for _, u := range updates {
			t.Run(d.name+"/"+u.name, func(t *testing.T) {
				r := newWALRun(t, 512, 0, 0)
				r.tx(d.ddl)
				var ins []string
				for i := range 38 {
					ins = append(ins, fmt.Sprintf(d.ins, i, 100-i))
				}
				r.tx(ins...)
				var busy, logFrames, ckpt int
				if err := r.db.QueryRow("pragma wal_checkpoint(truncate)").Scan(&busy, &logFrames, &ckpt); err != nil || busy != 0 {
					t.Fatalf("checkpoint: %v busy %d", err, busy)
				}
				r.tx(u.stmt)
				db, wal := r.snap()
				_, h := openAll(t, db, wal, nil)
				rows, _ := collectRows(t, h)
				if len(rows) == 0 {
					t.Fatal("no history rows")
				}
				for _, row := range rows {
					if row.Table != "t" || row.TableBasis != sqlitefile.BasisSchema || hasNote(row, sqlitefile.NoteOwnerChanged) || row.Relation == sqlitefile.RelUnknown {
						t.Errorf("row %v: table %q basis %s relation %s notes %v, want t, schema, a decided relation, no owner-changed", row.Values, row.Table, row.TableBasis, row.Relation, row.Notes)
					}
				}
			})
		}
	}
}
