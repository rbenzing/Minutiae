package evidence

import (
	"fmt"
	"testing"
)

// ftsStructureWithExtraSegment returns the structure record of table with one more segment (id, pages first to
// last) in a new top level.
func ftsStructureWithExtraSegment(t *testing.T, c *Case, table string, id, first, last uint64) []byte {
	t.Helper()
	var orig []byte
	if err := c.store.db.QueryRow(`SELECT block FROM `+table+`_data WHERE id = ?`, ftsStructureRowID).Scan(&orig); err != nil {
		t.Fatal(err)
	}
	segs, err := decodeFTSStructureSegments(orig)
	if err != nil {
		t.Fatal(err)
	}
	b := orig[4:]
	rd := func() uint64 {
		v, n := SQLiteVarint(b)
		if n == 0 {
			t.Fatal("truncated structure record")
		}
		b = b[n:]
		return v
	}
	nLevel, _, counter := rd(), rd(), rd()
	out := append([]byte{}, orig[:4]...)
	out = appendSQLiteVarint(out, nLevel+1)
	out = appendSQLiteVarint(out, uint64(len(segs))+1)
	out = appendSQLiteVarint(out, counter)
	for l := range int(nLevel) {
		var level []ftsSegment
		for _, s := range segs {
			if s.Level == l {
				level = append(level, s)
			}
		}
		out = appendSQLiteVarint(out, 0)
		out = appendSQLiteVarint(out, uint64(len(level)))
		for _, s := range level {
			out = appendSQLiteVarint(appendSQLiteVarint(appendSQLiteVarint(out, uint64(s.ID)), uint64(s.First)), uint64(s.Last))
		}
	}
	out = appendSQLiteVarint(out, 0)
	out = appendSQLiteVarint(out, 1)
	out = appendSQLiteVarint(appendSQLiteVarint(appendSQLiteVarint(out, id), first), last)
	if _, err := decodeFTSStructureSegments(out); err != nil {
		t.Fatalf("the test built an invalid structure record: %v", err)
	}
	return out
}

// TestVerifyFTSOversizeSegmentIDIsAPerSegmentProblem (D158): a segment id no data row id can hold (its row ids
// would overflow) is one typed problem for that segment; the other segments of the table are still read.
func TestVerifyFTSOversizeSegmentIDIsAPerSegmentProblem(t *testing.T) {
	c := indexedTextCase(t, 300)
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 5, ?, ?)`, normalizeFor(5)...); err != nil {
		t.Fatal(err)
	}
	const huge = 1 << 30
	st := ftsStructureWithExtraSegment(t, c, FTSWordTable, huge, 1, 1)
	if _, err := c.store.db.Exec(`UPDATE records_fts_data SET block = ? WHERE id = ?`, st, ftsStructureRowID); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	want := fmt.Sprintf("records_fts: segment %d cannot have data rows", huge)
	if n := countContaining(rep.Problems, want); n != 1 {
		t.Fatalf("%d lines with %q in %q, want 1", n, want, ftsProblems(rep))
	}
	if !containsSubstr(rep.Problems, "holds a delete marker for record 5") {
		t.Errorf("the other segments were not read after the oversize one: %q", ftsProblems(rep))
	}
}

// TestVerifyFTSInteriorRowOfAReferencedSegmentIsReported (D156): a _data row of a referenced segment whose
// local id is above the leaf pages and carries no doclist-index flag is an interior (height) page; a genuine
// index of this case holds none, so it is reported (unknown is never absent).
func TestVerifyFTSInteriorRowOfAReferencedSegmentIsReported(t *testing.T) {
	c := indexedTextCase(t, 300)
	seg, _ := lastLeaf(t, c, FTSWordTable)
	id := seg<<ftsSegidShift + 1<<31 + 3 // height 1, page 3, no doclist-index bit
	if _, err := c.store.db.Exec(`INSERT INTO records_fts_data (id, block) VALUES (?, x'0000000400')`, id); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	want := fmt.Sprintf("records_fts: data row %d (segment %d) is an interior page", id, seg)
	if !containsSubstr(rep.Problems, want) {
		t.Fatalf("no %q in %q", want, ftsProblems(rep))
	}
}
