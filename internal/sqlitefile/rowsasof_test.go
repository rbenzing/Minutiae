package sqlitefile_test

// asOfIdent pins (Task 14 review I-2): the label an image's own as-of schema
// gives is used only when the page is a leaf of the owner's kind and the cells
// fit the owner; a WITHOUT ROWID owner and the schema table itself own pages too.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// asOfScen: the live table t is on page 2 (db file and WAL tail). The WAL first
// holds the schema page of schemaImg and page pg of pageImg (the old era), then
// the live pages. It returns the rows of the old era's page frame (slot 2).
func asOfScen(t *testing.T, schemaImg, pageImg *sqlitetest.Image, pg uint32) []sqlitefile.RecoveredRow {
	t.Helper()
	return asOfScenLive(t, "create table t(a, b not null)", schemaImg, pageImg, pg)
}

func asOfScenLive(t *testing.T, liveSQL string, schemaImg, pageImg *sqlitetest.Image, pg uint32) []sqlitefile.RecoveredRow {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", liveSQL)
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	if pg > 2 { // pad so that page pg exists today
		big := b.CreateTable("big", "create table big(x)")
		for id := int64(1); big.Pages()[len(big.Pages())-1] < pg+1 || id < 3; id++ {
			big.Insert(id, longText(300, byte(id)))
		}
	}
	liveImg := b.Snapshot()
	n := liveImg.Pages()
	db := walMode(withCount(b.Bytes(), n))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, schemaImg.Page(1), n)
	w.Frame(pg, pageImg.Page(pg), n)
	w.Frame(1, liveImg.Page(1), n)
	w.Frame(pg, liveImg.Page(pg), n)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	var out []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.WAL != nil && r.WAL.Frame == 2 {
			out = append(out, r)
		}
	}
	return out
}

func eraTable(sql, name string, wr bool, rows [][]any) *sqlitetest.Image {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	var tb *sqlitetest.Table
	if wr {
		tb = b.CreateTableWithoutRowid(name, sql, 1)
	} else {
		tb = b.CreateTable(name, sql)
	}
	for i, r := range rows {
		tb.Insert(int64(i+1), r...)
	}
	return b.Snapshot()
}

func notLabelledByTheAsOfSchema(t *testing.T, rows []sqlitefile.RecoveredRow, name string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("no rows of the old era")
	}
	for _, r := range rows {
		if r.Table == name && r.TableBasis == sqlitefile.BasisSchema {
			t.Errorf("row labelled %q on the schema basis although the cells do not belong to it: %+v", name, r)
		}
	}
}

// The snapshot names z for page 2, but the cells do not fit z (y is NOT NULL and
// the cells hold NULL): the as-of label is not used.
func TestAsOfOwnerWhoseTableTheCellsDoNotFit(t *testing.T) {
	old := eraTable("create table z(x, y not null)", "z", false, [][]any{{int64(1), nil}, {int64(2), nil}, {int64(3), nil}})
	notLabelledByTheAsOfSchema(t, asOfScen(t, old, old, 2), "z")
}

// The snapshot names the rowid table z, but the page is a leaf of a WITHOUT
// ROWID table (an index leaf): the page type is not a table leaf.
func TestAsOfOwnerWhosePageIsOfAnotherKind(t *testing.T) {
	schema := eraTable("create table z(k primary key, y)", "z", false, [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}})
	wrPage := eraTable("create table z(k primary key, y) without rowid", "z", true, [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}})
	notLabelledByTheAsOfSchema(t, asOfScen(t, schema, wrPage, 2), "z")
}

// A WITHOUT ROWID as-of owner labels its index-leaf page.
func TestAsOfOwnerWithoutRowid(t *testing.T) {
	old := eraTable("create table z(k primary key, y) without rowid", "z", true, [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}})
	rows := asOfScen(t, old, old, 2)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		if r.Table != "z" || r.TableBasis != sqlitefile.BasisSchema || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteOwnerChanged) {
			t.Errorf("row: table %q basis %s relation %s notes %v", r.Table, r.TableBasis, r.Relation, r.Notes)
		}
	}
}

// The as-of owner of the page is the schema table itself (a leaf of
// sqlite_schema) and today nothing owns the page (the live schema is one
// page and no b-tree reaches it): the rows are labelled sqlite_schema, as of their era.
func TestAsOfOwnerIsTheSchemaTable(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	for i := 0; i < 40; i++ {
		b.CreateTable(fmt.Sprintf("tab%02d", i), fmt.Sprintf("create table tab%02d(a, b, c)", i))
	}
	old := b.Snapshot()
	n := old.Pages()
	data := walMode(withCount(b.Bytes(), n))
	d, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lay, err := d.Live().Layout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var pg uint32
	for p := n; p > 1; p-- {
		if lay.Owner[p] == 1 {
			pg = p
			break
		}
	}
	if pg == 0 {
		t.Fatal("no leaf of the schema table")
	}
	empty := bytes.Clone(old.Page(pg))
	clear(empty[3:5])                                 // no cells
	empty[5], empty[6] = byte(hps>>8), byte(hps&0xff) // content area starts at the page end
	sb := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	sb.CreateTable("only", "create table only(a, b, c)").Insert(1, int64(1), "x", "y")
	small := sb.Snapshot()
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, old.Page(1), n)
	w.Frame(pg, old.Page(pg), n)
	w.Frame(1, small.Page(1), n) // today a one-page schema: no b-tree reaches page pg
	w.Frame(pg, empty, n)
	_, h := openAll(t, data, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	got := 0
	for _, r := range rows {
		if r.WAL == nil || r.WAL.Frame != 2 {
			continue
		}
		got++
		if r.Table != "sqlite_schema" || r.TableBasis != sqlitefile.BasisSchema {
			t.Errorf("row: table %q basis %s relation %s notes %v", r.Table, r.TableBasis, r.Relation, r.Notes)
		}
	}
	if got == 0 {
		t.Fatalf("no rows of slot 2 in %v", methods(rows))
	}
}

// The as-of schema names z but the cells do not fit z, while they do fit
// today's owner t of the page: the owner at write time was z, so the cells are
// never taken for t's on the schema basis (review I-3).
func TestAsOfOwnerOtherThanTodaysIsNeverTodaysOnTheSchemaBasis(t *testing.T) {
	old := eraTable("create table z(x, y not null)", "z", false, [][]any{{int64(1), nil}, {int64(2), nil}, {int64(3), nil}})
	rows := asOfScenLive(t, "create table t(a, b)", old, old, 2)
	notLabelledByTheAsOfSchema(t, rows, "t")
	notLabelledByTheAsOfSchema(t, rows, "z")
}

// A transaction's end is looked for within the generation of its own frames:
// a stale frame of an older generation that follows an uncommitted frame (the
// file keeps them past the new end) is not its commit. The database file holds
// the era of table z; generation 2 writes an uncommitted page 2 of z over slot
// 1, and the stale slots 2 and 3 of generation 1 would, taken for its commit,
// hand the page to table t.
func TestAsOfTransactionEndStaysInItsGeneration(t *testing.T) {
	zb := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	z := zb.CreateTable("z", "create table z(x, y)")
	for id := int64(1); id <= 3; id++ {
		z.Insert(id, id*7, "era")
	}
	zimg := zb.Snapshot()
	n := zimg.Pages()
	db := walMode(withCount(zb.Bytes(), n))
	z.Update(2, int64(99), "era-uncommitted")
	zimg2 := zb.Snapshot()

	tb := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := tb.CreateTable("t", "create table t(a, b not null)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	tl := tb.Snapshot()
	w := zb.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, tl.Page(1), n) // generation 1: table t takes over the database
	w.Frame(2, tl.Page(2), n)
	w.Frame(2, tl.Page(2), n)
	w.Reset(0x2000, 0x2001)
	w.Frame(2, zimg2.Page(2), 0) // generation 2: uncommitted cells of z, over slot 1
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	got := 0
	for _, r := range rows {
		if r.WAL == nil || r.Origin != sqlitefile.OriginWALUncommitted {
			continue
		}
		got++
		if r.Table != "z" || r.TableBasis != sqlitefile.BasisSchema {
			t.Errorf("uncommitted row: table %q basis %s, want z on the schema basis", r.Table, r.TableBasis)
		}
	}
	if got == 0 {
		t.Fatalf("no uncommitted row in %v", methods(rows))
	}
}
