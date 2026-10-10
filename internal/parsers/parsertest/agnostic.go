package parsertest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// AssertMapperSourceAgnostic opens the bundle's database and, for every mapping of
// p, maps up to limit rows of the table twice: as the live row, and rebuilt through
// sqlitedb.FromRecovered (the way the typed recovery pass of plan 4G presents a
// row). The mapper must return the same records (the locator is stamped by the
// recovery host and is the one allowed difference) or the same row error: a
// mapper may depend only on its row and its context (spec 12.1).
// The check compares the live mapping with the rebuilt one only: it does not say that the
// live mapping is right, so the caller also runs AssertMatchesOracle for that.
func AssertMapperSourceAgnostic(t testing.TB, h *Harness, p parse.RowMapper, b Bundle, limit int) {
	t.Helper()
	in := b.Input(h, p, false)
	files := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		files.WAL, files.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		files.Journal, files.JournalSize = a.R, a.Size
	}
	ctx := t.Context()
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		t.Errorf("open: %v", err)
		return
	}
	defer db.Release()
	mc := sqlitedb.NewContext(ctx, db, in, nil)
	defer mc.Close()
	checked := 0
	for _, mp := range p.Mappings() {
		tbl, err := db.Table(ctx, mp.Table, nil, nil)
		if errors.Is(err, sqlitedb.ErrNoSuchTable) {
			continue
		}
		if err != nil {
			t.Errorf("table %s: %v", mp.Table, err)
			continue
		}
		n := 0
		err = tbl.Scan(ctx, func(row sqlitedb.Row) error {
			if n >= limit {
				return sqlitedb.ErrStop
			}
			n++
			checked++
			live, lerr := mp.Map(mc, row)
			rec, err := sqlitedb.FromRecovered(tbl, recoveredFrom(row))
			if err != nil {
				return err
			}
			defer rec.Release()
			got, gerr := mp.Map(mc, rec)
			if (lerr == nil) != (gerr == nil) || (lerr != nil && lerr.Error() != gerr.Error()) {
				t.Errorf("%s rowid %v: live error %v, rebuilt error %v", mp.Table, rowid(row), lerr, gerr)
				return nil
			}
			if !reflect.DeepEqual(unlocated(live), unlocated(got)) {
				t.Errorf("%s rowid %v: the mapper gave other records for the same row rebuilt as a recovered row\n live %+v\n  got %+v", mp.Table, rowid(row), live, got)
			}
			return nil
		})
		if err != nil {
			t.Errorf("scan %s: %v", mp.Table, err)
		}
	}
	if err := mc.Err(); err != nil {
		t.Errorf("the mapping context failed: %v", err)
	}
	if checked == 0 {
		t.Error("no row was checked: the check is vacuous")
	}
}

func rowid(r sqlitedb.Row) any {
	if id, ok := r.Rowid(); ok {
		return id
	}
	return "lost"
}

func unlocated(rs []records.Record) []records.Record {
	out := make([]records.Record, len(rs))
	for i, r := range rs {
		r.Locator = ""
		out[i] = r
	}
	return out
}

func recoveredFrom(row sqlitedb.Row) sqlitefile.RecoveredRow {
	vals := make([]sqlitefile.Value, row.NumCols())
	for i := range vals {
		vals[i] = row.Value(i)
	}
	rr := sqlitefile.RecoveredRow{Table: row.Table(), Values: vals, Loc: row.Loc()}
	if id, ok := row.Rowid(); ok {
		rr.Rowid = &id
	}
	return rr
}
