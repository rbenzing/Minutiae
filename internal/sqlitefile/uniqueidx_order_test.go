package sqlitefile_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// A UNIQUE index whose file holds a repeated NULL-free key contradicts its own
// declaration: every entry that repeats its predecessor's key is damage (LX-4 U61).
// Mutation: indexOrder builds KeyOrder without Unique.
func TestIndexEntriesFlagARepeatedKeyOfAUniqueIndex(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 60; i++ {
		tb.Insert(i, i, i/2) // 30 distinct keys, each twice, no NULL
	}
	b.CreateIndex("i", "t", "create unique index i on t(b)", 1)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	rows := scanIndexEntries(t, v, "i")
	if len(rows) != 60 {
		t.Fatalf("%d entries", len(rows))
	}
	want := nonAscending(t, rows, true)
	if len(want) == 0 {
		t.Fatal("the file holds no repeated key")
	}
	if fl := flaggedIdx(rows); !equalInts(fl, want) {
		t.Fatalf("flagged %v, want %v", fl, want)
	}
	if len(warningsOf(v, sqlitefile.WarnBTreeOrder)) == 0 {
		t.Error("no btree-order warning")
	}
}

// A repeated key holding a NULL is legitimate in a UNIQUE index (NULLs are distinct).
func TestIndexEntriesDoNotFlagRepeatedNullsOfAUniqueIndex(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 60; i++ {
		tb.Insert(i, i, nil)
	}
	b.CreateIndex("i", "t", "create unique index i on t(b)", 1)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	if fl := flaggedIdx(scanIndexEntries(t, v, "i")); len(fl) != 0 {
		t.Errorf("NULL keys flagged: %v", fl)
	}
}
