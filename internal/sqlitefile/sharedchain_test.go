package sqlitefile_test

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestGetReadsASharedChainAsTheEngineDoesScanFlagsIt pins the Task 5 ruling:
// when the overflow chain of one row runs into the chain of another row, a
// point lookup follows it as the engine does (each lookup has its own visited
// set, so it cannot know), while a scan, which sees the whole tree, flags the
// sharing and does not deliver the other row's bytes.
func TestGetReadsASharedChainAsTheEngineDoesScanFlagsIt(t *testing.T) {
	const chunk = 512 - 4
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	tb.Insert(1, bytes.Repeat([]byte{0x11}, 5*chunk+100)) // row 1: a long chain
	tb.Insert(2, bytes.Repeat([]byte{0x22}, 3*chunk+100)) // row 2: a shorter one
	data := b.Bytes()
	c1, c2 := tb.Overflow(1), tb.Overflow(2)
	if len(c1) < 5 || len(c2) < 3 || len(c2) >= len(c1) {
		t.Fatalf("chains of %d and %d pages: not the fixture this test needs", len(c1), len(c2))
	}
	// Row 2's second page continues into row 1's fourth page: it still needs
	// as many pages as its own chain had, and finds them there.
	binary.BigEndian.PutUint32(pageAt(data, 512, c2[1]), c1[3])

	g, s, gv, sv := getAndScan(t, data, tb.Root(), 2, sqlitefile.Options{})
	gb := g.Values[0].Bytes
	if g.Values[0].Len != 3*chunk+100 || len(gb) != 3*chunk+100 {
		t.Fatalf("Get read %d of %d bytes", len(gb), g.Values[0].Len)
	}
	if n := bytes.Count(gb, []byte{0x11}); n == 0 {
		t.Error("Get did not follow the chain into the other row's pages, as the engine does")
	}
	if got := warnCodeSet(gv); len(got) != 0 {
		t.Errorf("Get warned %v: a lookup cannot see the sharing", got)
	}
	if !slices.Contains(warnCodeSet(sv), sqlitefile.WarnCellOverflowChain) {
		t.Errorf("the scan must flag the shared page: %v", warnCodeSet(sv))
	}
	if bytes.Contains(s.Values[0].Bytes, []byte{0x11}) {
		t.Error("the scan delivered another row's bytes as this row's")
	}
}
