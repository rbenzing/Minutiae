package evidence

import (
	"testing"
)

// withOutsideCap lowers the cap of distinct "outside" id blocks (D150) for one test.
func withOutsideCap(t *testing.T, n int) {
	t.Helper()
	old := ftsMaxOutsideBuckets
	t.Cleanup(func() { ftsMaxOutsideBuckets = old })
	ftsMaxOutsideBuckets = n
}

// TestFTSDigesterCapsOutsideBuckets (D150): the number of distinct outside-block buckets of one table is
// bounded; the postings past the cap fall in one overflow bucket, and none of them is lost.
func TestFTSDigesterCapsOutsideBuckets(t *testing.T) {
	withOutsideCap(t, 5)
	d, err := newFTSDigester([16]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		d.add(0, ftsBucketKey{batch: -1, block: int64(i)}, []byte("w"), int64(i)<<16, 0, 0)
	}
	if n := len(d.sums[0]); n != 5+1 {
		t.Fatalf("%d buckets after 50 distinct outside blocks, want the cap (5) and one overflow bucket", n)
	}
	ov := d.sums[0][ftsOverflowBucket]
	if ov == nil || ov.n != 45 {
		t.Fatalf("overflow bucket %+v, want 45 postings", ov)
	}
	if len(d.sums[1]) != 0 {
		t.Fatalf("the other table got buckets: %v", d.sums[1])
	}
	// a block that already has its bucket keeps it
	d.add(0, ftsBucketKey{batch: -1, block: 0}, []byte("w"), 1, 0, 1)
	if len(d.sums[0]) != 6 || d.sums[0][ftsBucketKey{batch: -1, block: 0}].n != 2 {
		t.Fatalf("a known block lost its bucket: %v", d.sums[0])
	}
	// audited batches are never capped
	for i := range 20 {
		d.add(0, ftsBucketKey{batch: i}, []byte("w"), int64(i), 0, 0)
	}
	if len(d.sums[0]) != 26 {
		t.Fatalf("%d buckets, want the 20 audited ones beside the 6", len(d.sums[0]))
	}
}

// TestFTSBucketSetCapsOutsideBuckets (D150): the buckets of delete markers and of failing segments are capped alike.
func TestFTSBucketSetCapsOutsideBuckets(t *testing.T) {
	withOutsideCap(t, 4)
	var s ftsBucketSet
	for i := range 100 {
		s.add(ftsBucketKey{batch: -1, block: int64(i)})
	}
	s.add(ftsBucketKey{batch: 3})
	if len(s.m) != 4+1+1 {
		t.Fatalf("%d entries, want 4 outside blocks, the overflow bucket and the audited one: %v", len(s.m), s.keys())
	}
	if !s.has(ftsOverflowBucket) || !s.has(ftsBucketKey{batch: 3}) || !s.has(ftsBucketKey{batch: -1, block: 0}) {
		t.Fatalf("missing entries: %v", s.keys())
	}
}

// TestVerifyFTSHostileRowidsKeepMemoryBounded (D150): an index whose postings sit in thousands of different id
// blocks (rowid deltas of 65536 and more) and whose delete markers do too holds a bounded number of buckets, and
// the overflow is always a PROBLEM.
func TestVerifyFTSHostileRowidsKeepMemoryBounded(t *testing.T) {
	const capBuckets = 8
	withOutsideCap(t, capBuckets)
	c := indexedTextCase(t, 20)
	tx, err := c.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for k := int64(1); k <= 60; k++ {
		id := k << 20 // a different block (id >> 16) for each
		for _, stmt := range []string{
			`INSERT INTO records_fts(rowid, summary, body) VALUES (?, 'hostile', 'text')`,
			`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', ?, 'hostile', 'text')`,
		} {
			if _, err := tx.Exec(stmt, id+1_000_000_000); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO records_fts(rowid, summary, body) VALUES (?, 'hostile', 'text')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var found, bad int
	c.verifyFTSInspect = func(r *ftsVerify) {
		found = len(r.found.sums[0])
		bad = len(r.bad[0].m)
	}
	rep := mustVerify(t, c)
	if rep.OK() {
		t.Fatal("a hostile index passed")
	}
	if found > capBuckets+2 || bad > capBuckets+2 { // the cap, the overflow bucket and the one block of the records
		t.Fatalf("found buckets %d, delete-marker buckets %d with a cap of %d: memory is not bounded", found, bad, capBuckets)
	}
	if !containsSubstr(rep.Problems, "too many unaudited id blocks") {
		t.Fatalf("no overflow problem in %q", ftsProblems(rep))
	}
}

// TestStartFTSDrawsADifferentNonZeroKeyEachRun (D152): the digest key is new for every run and never zero, so
// the digest cannot be predicted by whoever wrote the case.
func TestStartFTSDrawsADifferentNonZeroKeyEachRun(t *testing.T) {
	c := indexedTextCase(t, 3)
	sumOf := func(d *ftsDigester) ftsSum {
		d.add(0, ftsBucketKey{batch: 0}, []byte("term"), 7, 1, 3)
		return *d.sums[0][ftsBucketKey{batch: 0}]
	}
	zero, err := newFTSDigester([16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	var sums []ftsSum
	for range 3 {
		r := c.startFTS(t.Context(), &problemSet{rep: &VerifyReport{}, counts: map[string]int{}}, nil, nil)
		if r.ex == nil || !r.ex.enabled {
			t.Fatal("the expected side is not enabled")
		}
		sums = append(sums, sumOf(r.ex.dig))
	}
	z := sumOf(zero)
	for i, s := range sums {
		if s == z {
			t.Errorf("run %d used the all-zero key", i)
		}
		for j := range i {
			if sums[j] == s {
				t.Errorf("runs %d and %d drew the same key", j, i)
			}
		}
	}
}
