package sqlitefile_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestLayoutNeverCallsPagesOrphansWhenSchemaIsIncomplete: with the first schema
// cell unreadable, the pages of the object that row described are on no list
// because the row was lost, not because they are leftovers. Layout does not call
// them orphans: they are unattributed, and a Problem names the schema damage.
func TestLayoutNeverCallsPagesOrphansWhenSchemaIsIncomplete(t *testing.T) {
	b, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 3)
	data := b.Bytes()
	if data[100] != 0x0d {
		t.Fatalf("page 1 is not a table leaf (%#x)", data[100])
	}
	data[108], data[109] = 0xff, 0xff // the first cell pointer points outside the page
	l, v := layoutOf(t, data, sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Skipped == 0 {
		t.Error("Schema().Skipped is 0 although a schema cell was unreadable")
	}
	if !l.SchemaIncomplete {
		t.Error("Layout.SchemaIncomplete is false")
	}
	if l.OrphansTotal != 0 || len(l.Orphans) != 0 {
		t.Errorf("orphans %v (total %d) reported from an incomplete schema", l.Orphans, l.OrphansTotal)
	}
	unattributed := 0
	for pg := uint32(1); pg <= l.Addressable; pg++ {
		switch l.Class[pg] {
		case sqlitefile.ClassOrphan:
			t.Errorf("page %d is classed orphan", pg)
		case sqlitefile.ClassUnattributed:
			unattributed++
		}
	}
	if unattributed == 0 {
		t.Error("no page is unattributed: the table whose schema row was lost has pages")
	}
	if !strings.Contains(strings.Join(l.Problems, "\n"), "schema") {
		t.Errorf("no problem names the schema damage: %v", l.Problems)
	}
	if sqlitefile.ClassUnattributed.String() != "unattributed (schema incomplete)" {
		t.Errorf("class text %q", sqlitefile.ClassUnattributed.String())
	}
}

// TestIntactSchemaSkipsNothing: the control of the test above.
func TestIntactSchemaSkipsNothing(t *testing.T) {
	data, _ := orphanDB(t, 5)
	l, v := layoutOf(t, data, sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Skipped != 0 || l.SchemaIncomplete || l.OrphansTotal < 5 {
		t.Errorf("skipped %d incomplete %v orphans %d", s.Skipped, l.SchemaIncomplete, l.OrphansTotal)
	}
}

// TestFreelistAndLayoutChargesAreExact: the budget is charged for what the
// result holds, per element actually appended and once: a leaf that was skipped
// is not charged, and Layout does not charge the freelist's lists again.
func TestFreelistAndLayoutChargesAreExact(t *testing.T) {
	t.Run("skipped leaves are not charged", func(t *testing.T) {
		const ps = 512
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		b.SetFreelist([][]uint32{{3, 4, 5, 6, 7, 8, 9, 10}})
		data := b.Bytes()
		for slot, v := range []uint32{0, 1, 999999, 4, 4, 9} { // three good ones at most
			putAt(data, ps, 3, 8+4*slot, v)
		}
		putAt(data, ps, 3, 4, 6)
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		fl, err := v.Freelist(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := sqlitefile.ListCharges(v)
		if want := 4 * int64(len(fl.Trunks)+len(fl.Leaves)); got != want {
			t.Errorf("freelist charge %d, want %d (%d trunks, %d leaves)", got, want, len(fl.Trunks), len(fl.Leaves))
		}
	})
	t.Run("layout does not charge the lists again", func(t *testing.T) {
		b, _ := freeScenario(sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 12)
		b.WritePtrmap()
		l, v := layoutOf(t, b.Bytes(), sqlitefile.Options{})
		fl, lay := sqlitefile.ListCharges(v)
		f, _ := v.Freelist(context.Background())
		if fl != 4*int64(len(f.Trunks)+len(f.Leaves)) {
			t.Errorf("freelist charge %d", fl)
		}
		want := 5*(int64(l.Addressable)+1) + 4*int64(len(l.PtrmapPages)) + 4*int64(len(l.Orphans))
		if lay != want {
			t.Errorf("layout charge %d, want %d: the arrays, the pointer-map list and the orphans only", lay, want)
		}
	})
	t.Run("a page claimed elsewhere makes a charged copy", func(t *testing.T) {
		b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 400)
		leaf := tb.Leaves()[1]
		extra := uint32(len(b.Bytes())/512 + 1)
		b.SetFreelist([][]uint32{{extra, leaf}})
		l, v := layoutOf(t, b.Bytes(), sqlitefile.Options{})
		_, lay := sqlitefile.ListCharges(v)
		want := 5*(int64(l.Addressable)+1) + 4*int64(len(l.PtrmapPages)) + 4*int64(len(l.Orphans)) + 4*int64(len(l.Leaves))
		if lay != want {
			t.Errorf("layout charge %d, want %d (the surviving leaves are a copy)", lay, want)
		}
	})
}

// TestFreelistLockBytePageIsNeverUsed: the lock-byte page as a trunk (named by
// the header) and as a leaf (named by a trunk) ends the walk or is skipped with
// a warning, and is never read.
func TestFreelistLockBytePageIsNeverUsed(t *testing.T) {
	const ps, pages = 4096, 300000
	lock := sqlitefile.LockBytePage(ps)
	build := func(patch func(data []byte)) (*sparseDB, *sqlitefile.View) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		b.SetFreelist([][]uint32{{2, 3, 4}})
		b.SetHeaderPages(pages, true)
		data := b.Bytes()
		patch(data)
		s := &sparseDB{head: data, size: int64(pages) * ps, lockFrom: int64(lock-1) * ps, lockTo: int64(lock) * ps}
		db, err := sqlitefile.Open(s, s.size, sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		v := db.Live()
		t.Cleanup(v.Release)
		return s, v
	}
	t.Run("as a leaf", func(t *testing.T) {
		s, v := build(func(data []byte) { putAt(data, ps, 2, 8, lock) })
		fl, err := v.Freelist(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range fl.Leaves {
			if p == lock {
				t.Errorf("the lock-byte page is on the leaf list %v", fl.Leaves)
			}
		}
		if len(fl.Leaves) != 1 || !viewWarns(v, sqlitefile.WarnPageRange, 2) {
			t.Errorf("leaves %v warnings %v", fl.Leaves, v.Warnings())
		}
		if s.lockRead.Load() {
			t.Error("the lock-byte page was read")
		}
		l, err := v.Layout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if l.Class[lock] != sqlitefile.ClassLockByte {
			t.Errorf("class %d", l.Class[lock])
		}
	})
	t.Run("as a trunk", func(t *testing.T) {
		s, v := build(func(data []byte) { putAt(data, ps, 1, 32, lock) })
		fl, err := v.Freelist(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if fl.Walked != 0 || !viewWarns(v, sqlitefile.WarnPageRange, lock) {
			t.Errorf("walked %d warnings %v", fl.Walked, v.Warnings())
		}
		if s.lockRead.Load() {
			t.Error("the lock-byte page was read")
		}
	})
}

// TestIndexCorpusAgreesWithTheEngine: every index statement the author wrote for
// the parser is run through the engine too: what the parser accepts the engine
// accepts, and what the parser refuses the engine refuses (an empty statement
// and a statement that is not an index are exempt).
func TestIndexCorpusAgreesWithTheEngine(t *testing.T) {
	for _, c := range indexCases {
		if c.sql == "" || strings.HasPrefix(c.sql, "CREATE TABLE") {
			continue
		}
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range []string{"CREATE TABLE t(a, b, c, name, x)", "CREATE TABLE [t y](`a b`)"} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		_, err = db.Exec(c.sql)
		_ = db.Close()
		if accepted := err == nil; accepted != !c.notOK {
			t.Errorf("%s: the parser accepts %v, the engine accepts %v (%v): %s", c.name, !c.notOK, accepted, err, c.sql)
		}
	}
}
