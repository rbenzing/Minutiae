package sqlitedb_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// flaggedPositions scans table name and returns the positions of the rows carrying
// FlagKeyRange and the number of rows.
func flaggedPositions(t *testing.T, d *sqlitedb.DB, name string) (flagged []int, total int) {
	t.Helper()
	tb, err := d.Table(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		if r.Flags()&sqlitedb.FlagKeyRange != 0 {
			flagged = append(flagged, total)
		}
		total++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return flagged, total
}

// B27 for index-shaped trees (LX-4 Task 8): a WITHOUT ROWID entry that does not follow the
// previous one carries FlagKeyRange, and Stats.RowsFlagged counts it.
func TestWithoutRowidScanFlagsKeyRangeOnOutOfOrderKeys(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := b.CreateTableWithoutRowid("w", "create table w(k integer primary key, v) without rowid", 1)
	for i := range 60 {
		k := int64(10 + i)
		tb.Insert(k, k, strings.Repeat("x", 40))
	}
	leaf := tb.Leaves()[1]
	data := b.Bytes()
	p := data[(int(leaf)-1)*ps : int(leaf)*ps]
	p[8+4], p[8+5], p[8+6], p[8+7] = p[8+6], p[8+7], p[8+4], p[8+5] // swap the pointers of cells 2 and 3

	d := openBytes(t, data, nil, nil, bigBudget())
	flagged, total := flaggedPositions(t, d, "w")
	if total != 60 {
		t.Fatalf("%d rows delivered, want all 60", total)
	}
	if len(flagged) != 1 {
		t.Fatalf("flagged positions %v, want exactly the entry that descends", flagged)
	}
	if s := d.Stats(); s.RowsFlagged != 1 {
		t.Errorf("RowsFlagged = %d, want 1", s.RowsFlagged)
	}
}

// A clean WITHOUT ROWID table written by the engine, with a DESC composite key under NOCASE,
// has no flagged row.
func TestWithoutRowidEngineTableHasNoKeyRangeFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clean.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"pragma page_size=512",
		"create table w(a text collate nocase, b integer, v, primary key(a, b desc)) without rowid",
		"with recursive c(i) as (select 0 union all select i+1 from c where i<1500) insert into w select char(97 + i%26) || (i%7), i, 'pad-' || i from c",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := openBytes(t, data, nil, nil, bigBudget())
	flagged, total := flaggedPositions(t, d, "w")
	if total != 1501 || !slices.Equal(flagged, nil) {
		t.Errorf("%d rows, flagged %v: an engine-written table must have none", total, flagged)
	}
	if s := d.Stats(); s.RowsFlagged != 0 {
		t.Errorf("RowsFlagged = %d", s.RowsFlagged)
	}
}
