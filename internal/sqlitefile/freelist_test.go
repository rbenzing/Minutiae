package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// putAt writes a big-endian uint32 at byte off of page pgno of data.
func putAt(data []byte, ps int, pgno uint32, off int, v uint32) {
	binary.BigEndian.PutUint32(data[int(pgno-1)*ps+off:], v)
}

// chain returns trunks of at most per leaves each over pages first..first+n-1:
// the first page of each group is the trunk.
func chain(first uint32, n, per int) (trunks [][]uint32, all []uint32) {
	for i := 0; i < n; {
		g := []uint32{first + uint32(i)}
		i++
		for len(g) <= per && i < n {
			g = append(g, first+uint32(i))
			i++
		}
		trunks = append(trunks, g)
		all = append(all, g...)
	}
	return
}

func freeView(t *testing.T, data []byte, o sqlitefile.Options) (*sqlitefile.Freelist, *sqlitefile.View) {
	t.Helper()
	_, v := openLive(t, data, o)
	t.Cleanup(v.Release)
	fl, err := v.Freelist(context.Background())
	if err != nil {
		t.Fatalf("Freelist: %v", err)
	}
	return fl, v
}

func TestFreelistWalk(t *testing.T) {
	const ps = 512
	for _, c := range []struct {
		name       string
		pages      int
		wantTrunks int
	}{
		{"two trunks", 200 + 2, 2},
		{"600 leaves", 600 + 5, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
			trunks, all := chain(2, c.pages, 126)
			if len(trunks) != c.wantTrunks {
				t.Fatalf("fixture: %d trunks", len(trunks))
			}
			b.SetFreelist(trunks)
			fl, v := freeView(t, b.Bytes(), sqlitefile.Options{})
			var wantTrunks, wantLeaves []uint32
			for _, g := range trunks {
				wantTrunks = append(wantTrunks, g[0])
				wantLeaves = append(wantLeaves, g[1:]...)
			}
			if !slices.Equal(fl.Trunks, wantTrunks) || !slices.Equal(fl.Leaves, wantLeaves) {
				t.Errorf("trunks %v leaves %d, want %v and %d (chain order)", fl.Trunks, len(fl.Leaves), wantTrunks, len(wantLeaves))
			}
			if fl.Walked != uint32(len(all)) || fl.Walked != fl.HeaderCount {
				t.Errorf("walked %d, header count %d, want %d", fl.Walked, fl.HeaderCount, len(all))
			}
			if len(fl.Anomalies) != 0 || len(v.Warnings()) != 0 {
				t.Errorf("anomalies %v warnings %v", fl.Anomalies, v.Warnings())
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		fl, _ := freeView(t, sqlitetest.New(sqlitetest.Options{PageSize: ps}).Bytes(), sqlitefile.Options{})
		if len(fl.Trunks) != 0 || len(fl.Leaves) != 0 || fl.Walked != 0 || fl.HeaderCount != 0 || len(fl.Anomalies) != 0 {
			t.Errorf("%+v", fl)
		}
	})
}

func TestFreelistCycle(t *testing.T) {
	const ps = 512
	t.Run("a trunk points at itself", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		b.SetFreelist([][]uint32{{2, 3, 4}, {5, 6}})
		data := b.Bytes()
		putAt(data, ps, 5, 0, 5)
		fl, v := freeView(t, data, sqlitefile.Options{})
		if !slices.Equal(fl.Trunks, []uint32{2, 5}) || !slices.Equal(fl.Leaves, []uint32{3, 4, 6}) {
			t.Errorf("trunks %v leaves %v", fl.Trunks, fl.Leaves)
		}
		if !viewWarns(v, sqlitefile.WarnFreelistCycle, 0) || len(fl.Anomalies) == 0 {
			t.Errorf("a self loop must warn: %v %v", v.Warnings(), fl.Anomalies)
		}
	})
	t.Run("A to B to A", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		b.SetFreelist([][]uint32{{2, 3}, {4, 5}})
		data := b.Bytes()
		putAt(data, ps, 4, 0, 2)
		fl, v := freeView(t, data, sqlitefile.Options{})
		if !slices.Equal(fl.Trunks, []uint32{2, 4}) || fl.Walked != 4 {
			t.Errorf("trunks %v walked %d: each trunk is walked once", fl.Trunks, fl.Walked)
		}
		if !viewWarns(v, sqlitefile.WarnFreelistCycle, 0) {
			t.Errorf("a cycle must warn: %v", v.Warnings())
		}
	})
}

func TestFreelistLeafCountOverflow(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetFreelist([][]uint32{{2, 3, 4}})
	data := b.Bytes()
	putAt(data, ps, 2, 4, 100000)
	fl, v := freeView(t, data, sqlitefile.Options{})
	if !viewWarns(v, sqlitefile.WarnFreelistLeafCount, 2) {
		t.Errorf("no freelist-leaf-count warning for the trunk: %v", v.Warnings())
	}
	if !slices.Equal(fl.Leaves, []uint32{3, 4}) {
		t.Errorf("leaves %v: the list is cut at the page capacity, the slots past the real leaves are empty", fl.Leaves)
	}
	if n := len(v.Warnings()); n > 130 {
		t.Errorf("%d warnings for one trunk: not bounded by the 126-slot maximum", n)
	}
}

func TestFreelistCountMismatchWarns(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetFreelist([][]uint32{{2, 3, 4, 5}})
	data := b.Bytes()
	binary.BigEndian.PutUint32(data[36:], 9)
	fl, v := freeView(t, data, sqlitefile.Options{})
	if fl.Walked != 4 || fl.HeaderCount != 9 {
		t.Errorf("walked %d header %d: the walked list wins", fl.Walked, fl.HeaderCount)
	}
	if !viewWarns(v, sqlitefile.WarnFreelistCount, 0) {
		t.Errorf("no freelist-count warning: %v", v.Warnings())
	}
	// A header that names no trunk but a count.
	binary.BigEndian.PutUint32(data[32:], 0)
	fl, v = freeView(t, data, sqlitefile.Options{})
	if fl.Walked != 0 || !viewWarns(v, sqlitefile.WarnFreelistCount, 0) {
		t.Errorf("count without a trunk: walked %d %v", fl.Walked, v.Warnings())
	}
}

func TestFreelistPageOutOfRange(t *testing.T) {
	const ps = 512
	// leaf slots: 0, page 1, beyond the end, a pointer-map page, a repeated leaf, a good one.
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps, AutoVacuum: 1})
	b.SetFreelist([][]uint32{{3, 4, 5, 6, 7, 8, 9, 10}})
	data := b.Bytes()
	for slot, v := range []uint32{0, 1, 999999, 2, 4, 4, 9} {
		putAt(data, ps, 3, 8+4*slot, v)
	}
	putAt(data, ps, 3, 4, 7)
	fl, v := freeView(t, data, sqlitefile.Options{})
	if !slices.Equal(fl.Leaves, []uint32{4, 9}) {
		t.Errorf("leaves %v, want only the valid 4 and 9 (0, 1, out of range, the pointer-map page 2 and the repeated 4 are skipped)", fl.Leaves)
	}
	if !viewWarns(v, sqlitefile.WarnPageRange, 0) || !viewWarns(v, sqlitefile.WarnFreelistCycle, 0) {
		t.Errorf("skipped leaves must each be warned about: %v", v.Warnings())
	}
	if fl.Walked != 3 || len(fl.Anomalies) < 5 {
		t.Errorf("walked %d anomalies %d", fl.Walked, len(fl.Anomalies))
	}
	// A trunk that is outside the file ends the walk.
	binary.BigEndian.PutUint32(data[32:], 999999)
	fl, v = freeView(t, data, sqlitefile.Options{})
	if fl.Walked != 0 || !viewWarns(v, sqlitefile.WarnPageRange, 0) {
		t.Errorf("a trunk outside the file: walked %d %v", fl.Walked, v.Warnings())
	}
}
