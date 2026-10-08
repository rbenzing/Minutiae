package sqlitedb_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func releaseDB(t *testing.T) (*sqlitedb.DB, *sqlitedb.Table, *testBudget) {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= 10; i++ {
			tt.Insert(i, i, fmt.Sprintf("v%d", i))
		}
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, tb, b
}

// TestRowReleaseReturnsTheCharge: Get and Clone rows are charged; Row.Release gives
// the charge back, a second call does nothing, copies share one release.
func TestRowReleaseReturnsTheCharge(t *testing.T) {
	d, tb, b := releaseDB(t)
	warm, _, _ := tb.Get(t.Context(), 1) // the library keeps the pages it read
	warm.Release()
	base := b.used
	r1, ok, err := tb.Get(t.Context(), 3)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	r2, _, _ := tb.Get(t.Context(), 4)
	if b.used <= base {
		t.Fatalf("Get charged nothing (used %d, base %d)", b.used, base)
	}
	afterBoth := b.used
	r1.Release()
	if b.used >= afterBoth {
		t.Errorf("Release returned nothing: %d -> %d", afterBoth, b.used)
	}
	afterOne := b.used
	r1.Release() // double release
	r1.Release()
	if b.used != afterOne {
		t.Errorf("a second Release changed used: %d -> %d", afterOne, b.used)
	}
	r2.Release()
	if b.used != base {
		t.Errorf("used %d after releasing both, want %d", b.used, base)
	}
	// A copy of a released row shares the release.
	r3, _, _ := tb.Get(t.Context(), 5)
	cp := r3
	r3.Release()
	cp.Release()
	if b.used != base {
		t.Errorf("a copy released twice: used %d, want %d", b.used, base)
	}
	d.Release()
	if b.used != 0 {
		t.Errorf("used %d after DB.Release", b.used)
	}
}

// TestRowReleaseOrders: Get, Clone and Release in any order balance, and a row
// released after DB.Release does not free more than was granted.
func TestRowReleaseOrders(t *testing.T) {
	d, tb, b := releaseDB(t)
	warm, _, _ := tb.Get(t.Context(), 1) // the library keeps the pages it read
	warm.Release()
	base := b.used
	g, _, _ := tb.Get(t.Context(), 1)
	c, err := g.Clone()
	if err != nil {
		t.Fatal(err)
	}
	// Release the original first, then the clone, then again.
	c.Release()
	g.Release()
	c.Release()
	g.Release()
	if b.used != base {
		t.Fatalf("used %d, want %d", b.used, base)
	}
	// A clone of a released row is charged anew and released on its own.
	c2, err := g.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if b.used <= base {
		t.Fatal("clone not charged")
	}
	c2.Release()
	if b.used != base {
		t.Fatalf("used %d, want %d", b.used, base)
	}
	// Release after DB.Release.
	h, _, _ := tb.Get(t.Context(), 2)
	d.Release()
	if b.used != 0 {
		t.Fatalf("used %d after DB.Release", b.used)
	}
	h.Release() // must not free what was already given back
	h.Release()
	if b.used != 0 {
		t.Errorf("Release after DB.Release left used = %d", b.used)
	}
	// The zero Row releases quietly.
	sqlitedb.Row{}.Release()
}

// TestReleaseAfterDBReleaseDoesNotStealOtherCharges: the shared budget holds a
// foreign charge; releasing a stale row must not touch it.
func TestReleaseAfterDBReleaseDoesNotStealOtherCharges(t *testing.T) {
	d, tb, b := releaseDB(t)
	h, _, _ := tb.Get(t.Context(), 2)
	d.Release()
	if err := b.Alloc(1000); err != nil { // someone else's charge
		t.Fatal(err)
	}
	h.Release()
	if b.used != 1000 {
		t.Errorf("a stale row release freed foreign bytes: used %d, want 1000", b.used)
	}
}

// TestCloneDeepCopiesLibraryOwnedSlices: overflow list, states, UTF-16 text
// and the notes of a recovered row are copied; scribbling over the original's
// copies leaves the clone unchanged.
func TestCloneDeepCopiesLibraryOwnedSlices(t *testing.T) {
	blob := bytes.Repeat([]byte{0xab, 0xcd}, 1500)
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b, c)")
		tt.Insert(1, "héllo", blob, int64(5))
	})
	tb := tableFrom(t, data, "t")
	err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		r = sqlitedb.MarkRecoveredWithNotes(r, "n1", "n2")
		c, err := r.Clone()
		if err != nil {
			return err
		}
		want := describe(c)
		sibling, err := r.Clone() // scribbling one clone must not touch another
		if err != nil {
			return err
		}
		wantNotes := fmt.Sprint(c.Recovered().Notes)
		sqlitedb.Scribble(sibling)
		s, x, o, n := sqlitedb.Scribble(r)
		if s == 0 || x == 0 || o == 0 || n == 0 {
			t.Fatalf("scribble reached states %d text %d overflow %d notes %d; the fixture must hold all four", s, x, o, n)
		}
		if got := describe(c); got != want {
			t.Errorf("the clone changed with its source:\n got  %s\n want %s", got, want)
		}
		if got := fmt.Sprint(c.Recovered().Notes); got != wantNotes {
			t.Errorf("notes changed: %s, want %s", got, wantNotes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetMissNextToAnEmptyLeafIsUncertain: a second damage construction for
// Get (T5 review minor 6): a miss at the edge of a leaf whose neighbour is
// empty is an ErrCorrupt error, never a clean absent. Rowids are 10 apart, so
// the missing rowid lies between two leaves.
func TestGetMissNextToAnEmptyLeafIsUncertain(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 300; i++ {
		tb.Insert(i*10, "padpadpadpadpadpadpadpad")
	}
	data := b.Bytes()
	leaves := tb.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	var maxMid int64
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := tb.CellBytes(i * 10); pg == leaves[1] {
			maxMid = i * 10
		}
	}
	page := data[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0 // cell count 0: the engine calls the page corrupt
	tt := tableFrom(t, data, "t")
	if _, ok, err := tt.Get(t.Context(), maxMid+5); ok || !errors.Is(err, sqlitedb.ErrCorrupt) {
		t.Errorf("Get next to an empty leaf: ok %v err %v, want ErrCorrupt", ok, err)
	}
}
