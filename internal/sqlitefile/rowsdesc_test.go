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

// descVariant is one more way a DESC-key table's pages come to have history: the setup runs in one
// transaction (then a checkpoint, if asked), the change in another.
type descVariant struct {
	name       string
	pageSize   int
	checkpoint bool
	setup      []string
	change     []string
	// samePage: the page is still the table's today (every row must keep the schema basis);
	// false: another table owns it now (every row must stay owner-changed and unknown).
	samePage bool
}

func descVariants() []descVariant {
	ins := func(n int, f string) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf(f, i, 1000-i))
		}
		return out
	}
	const ddl = "create table t(x real, y integer, z, primary key(y desc, x desc)) without rowid"
	rows := ins(38, "insert into t values(%d, %d, 'zzzz')")
	return []descVariant{
		{"freed-page reuse: delete all rows, re-insert", 512, true, append([]string{ddl}, rows...),
			append([]string{"delete from t"}, ins(38, "insert into t values(%d, %d, 'wwww')")...), true},
		{"no checkpoint", 512, false, append([]string{ddl}, rows...), []string{"update t set z = 'wwww' where y % 2 = 0"}, true},
		{"4096-byte pages, a deeper tree", 4096, true, append([]string{ddl}, ins(3000, "insert into t values(%d, %d, 'zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz')")...),
			[]string{"update t set z = 'wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww' where y % 3 = 0"}, true},
		{"ALTER TABLE ADD COLUMN after the rows were written", 512, true, append([]string{ddl}, rows...),
			[]string{"alter table t add column q default 7", "update t set z = 'wwww' where y % 2 = 0"}, true},
		{"a DESC NOCASE text key", 512, true,
			append([]string{"create table t(k text collate nocase, v, primary key(k desc)) without rowid"}, ins(38, "insert into t values('Key%03d', %d)")...),
			[]string{"update t set v = v + 5000 where v % 2 = 0"}, true},
		{"drop and recreate: another table owns the page now", 512, true, append([]string{ddl}, rows...),
			[]string{"drop table t", "create table other(a, b)", "insert into other values(1, 'x'), (2, 'y'), (3, 'z')"}, false},
	}
}

func TestWithoutRowidDescKeyMoreVariants(t *testing.T) {
	for _, v := range descVariants() {
		t.Run(v.name, func(t *testing.T) {
			r := newWALRun(t, v.pageSize, 0, 0)
			r.tx(v.setup[:1]...)
			r.tx(v.setup[1:]...)
			if v.checkpoint {
				var busy, logFrames, ckpt int
				if err := r.db.QueryRow("pragma wal_checkpoint(truncate)").Scan(&busy, &logFrames, &ckpt); err != nil || busy != 0 {
					t.Fatalf("checkpoint: %v busy %d", err, busy)
				}
			}
			r.tx(v.change...)
			db, wal := r.snap()
			_, h := openAll(t, db, wal, nil)
			rows, _ := collectRows(t, h)
			if len(rows) == 0 {
				t.Fatal("no history rows")
			}
			judged, changed := 0, 0
			for _, row := range rows {
				if row.Loc.Page == 1 { // the schema table's own history
					continue
				}
				owner := hasNote(row, sqlitefile.NoteOwnerChanged)
				judged++
				if owner {
					changed++
				}
				switch {
				case v.samePage:
					if row.Table != "t" || row.TableBasis != sqlitefile.BasisSchema || owner || row.Relation == sqlitefile.RelUnknown {
						t.Fatalf("row %v: table %q basis %s relation %s notes %v, want t, schema, a decided relation, no owner-changed", row.Values, row.Table, row.TableBasis, row.Relation, row.Notes)
					}
				case row.Relation != sqlitefile.RelUnknown:
					// the contrast: no page of the dropped table's era is compared with today's rows
					t.Fatalf("row %v: table %q basis %s relation %s notes %v, want unknown", row.Values, row.Table, row.TableBasis, row.Relation, row.Notes)
				}
			}
			if judged == 0 || (!v.samePage && changed == 0) {
				t.Fatalf("%d rows judged, %d owner-changed: the variant proves nothing", judged, changed)
			}
		})
	}
}
