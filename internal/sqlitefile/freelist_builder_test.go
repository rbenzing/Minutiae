package sqlitefile_test

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// freeScenario fills a builder with tables, an index, overflow rows and a
// dropped table, then frees extra pages.
func freeScenario(o sqlitetest.Options, extraFree int) (*sqlitetest.Builder, []uint32) {
	b := sqlitetest.New(o)
	t1 := b.CreateTable("t1", "create table t1(a, b)")
	for i := int64(1); i <= 60; i++ {
		t1.Insert(i, "row", bytes.Repeat([]byte{byte(i)}, 40))
	}
	t1.Insert(100, "big", bytes.Repeat([]byte("overflow"), 400))
	b.CreateIndex("i1", "t1", "create index i1 on t1(a)", 0)
	t2 := b.CreateTable("t2", "create table t2(a)")
	t2.Insert(1, bytes.Repeat([]byte("x"), 3000))
	b.DropTable("t2")
	var extra []uint32
	if extraFree > 0 {
		base := uint32(len(b.Bytes())/b.PageSize()) + 1
		per := uint32(b.PageSize()-o.Reserved)/5 + 1
		for pg := base; len(extra) < extraFree; pg++ {
			if o.AutoVacuum != 0 && (pg-2)%per == 0 { // a pointer-map page is not free
				continue
			}
			extra = append(extra, pg)
		}
		b.Free(extra...)
	}
	return b, extra
}

// TestBuilderFreelistAndPtrmapMatchEngine: the engine opens the builder's
// freelist and auto-vacuum databases, integrity_check is ok and freelist_count
// equals the builder's.
func TestBuilderFreelistAndPtrmapMatchEngine(t *testing.T) {
	for _, c := range []struct {
		name string
		o    sqlitetest.Options
		free int
		ptr  bool
	}{
		{"freelist 4096", sqlitetest.Options{}, 5, false},
		{"freelist 512 with several trunks", sqlitetest.Options{PageSize: 512}, 300, false},
		{"auto-vacuum full", sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 0, true},
		{"auto-vacuum with free pages", sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 20, true},
		{"incremental auto-vacuum", sqlitetest.Options{PageSize: 1024, AutoVacuum: 2}, 7, true},
		{"auto-vacuum spanning ptrmap pages", sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 250, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _ := freeScenario(c.o, c.free)
			if c.ptr {
				b.WritePtrmap()
			}
			db := openEngine(t, writeTemp(t, b.Bytes()))
			if got := pragmaString(t, db, "integrity_check"); got != "ok" {
				t.Fatalf("integrity_check = %q", got)
			}
			// The freelist_count comparison below is a consistency check of the
			// builder against its own header; the engine evidence is integrity_check.
			hdr := b.PageBytes(1)
			want := strconv.Itoa(int(hdr[36])<<24 | int(hdr[37])<<16 | int(hdr[38])<<8 | int(hdr[39]))
			if got := pragmaString(t, db, "freelist_count"); got != want {
				t.Errorf("freelist_count = %s, the header says %s", got, want)
			}
			if c.free > 0 && want == "0" {
				t.Error("nothing was freed")
			}
		})
	}
}
