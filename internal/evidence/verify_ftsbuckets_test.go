package evidence_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestVerifyFTSBucketsEqualOnCleanCase: on a case with audited batches the postings of the leaf pages equal
// those of the records in every bucket. A bucket is a (table, audited batch): the number compared is twice the
// number of audited batches.
func TestVerifyFTSBucketsEqualOnCleanCase(t *testing.T) {
	c := ftsCase(t, 6000) // batches of 5000 and 1000
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	batches := 0
	for _, e := range entries {
		if e.Action == evidence.ActionBatch {
			batches++
		}
	}
	if batches != 2 {
		t.Fatalf("%d audited batches, want 2", batches)
	}
	var got evidence.FTSBucketReport
	calls := 0
	c.SetFTSBucketObserver(func(r evidence.FTSBucketReport) { got, calls = r, calls+1 })
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %q", err, rep.Problems)
	}
	if calls != 1 || got.Compared != 2*batches || got.Failing != [2]int{0, 0} {
		t.Fatalf("observer calls %d, %+v: want one call, %d buckets compared and none failing", calls, got, 2*batches)
	}
}

// TestVerifyFTSCleanOnTokenizerEdgeCorpus (Review Focus 1): clean text at the tokenizer edges is OK, with no
// false positive: a token longer than 32,768 bytes, U+FFFE and U+FFFF, marks NormalizeText leaves uncomposed,
// Thai and CJK runs, letter and digit boundaries, characters outside the BMP.
func TestVerifyFTSCleanOnTokenizerEdgeCorpus(t *testing.T) {
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 1)
	u := func(cps ...rune) string { return string(cps) }
	texts := [][2]string{
		{"a long token", strings.Repeat("a", 40_000)},
		{"a long CJK run", strings.Repeat(u(0x4e16), 11_000)},
		{"x" + u(0xfffe) + "y" + u(0xffff) + "z", u(0xfffd, 0xfffe, 0xffff)},
		{"e" + u(0x301) + " a" + u(0x300) + " o" + u(0x323, 0x302) + " " + u(0x345) + " " + u(0x305), "caf" + u(0xe9) + " " + u(0xe0, 0xe8, 0xec) + " " + u(0xf1) + " " + u(0x1ea1, 0x301)},
		{u(0xe17, 0xe14, 0xe2a, 0xe2d, 0xe1a) + " " + u(0xe44, 0xe17, 0xe22) + " " + u(0xe01, 0xe32, 0xe23), u(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8) + " " + u(0xd55c, 0xad6d, 0xc5b4)},
		{"abc123def 12ab 34", "a1b2c3 99x ABC"},
		{u(0x1f600) + " smile " + u(0x1f1fa, 0x1f1f8) + " " + u(0x10400, 0x10428), u(0x1d7ce, 0x1d7cf) + " digits"},
		{"", ""},
		{"only a summary", ""},
		{"", "only a body"},
	}
	indexable := 0
	var recs []records.Record
	for i, tx := range texts {
		body := tx[1]
		if _, ok := evidence.NewFTSDoc(int64(i+1), tx[0], &body); ok {
			indexable++
		}
		recs = append(recs, records.Record{Type: "note", ArtifactID: ids[0], Summary: tx[0], Body: tx[1], Payload: map[string]any{"i": i}})
	}
	recordstest.Ingest(t, c, records.Parser{Name: "edge", Version: "1"}, ids, recs)
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %q", err, rep.Problems)
	}
	if rep.FTSDocsChecked != indexable || indexable < 7 {
		t.Fatalf("FTSDocsChecked = %d, want the %d indexable records", rep.FTSDocsChecked, indexable)
	}
}

// TestVerifyProgressIndexPassPagesEndAtTotal (14d, V3): the decode step of each table is a counted phase in the
// unit "pages" and its counter ends at the total the structure record announces.
func TestVerifyProgressIndexPassPagesEndAtTotal(t *testing.T) {
	c := ftsCase(t, 6000)
	got := snapshotsAtPhaseEnds(t, c)
	for _, table := range []string{"records_fts", "records_fts_sub"} {
		s, ok := got["fts.index/"+table+"/decode"]
		if !ok {
			t.Fatalf("no snapshot at the end of the decode step of %s: %v", table, got)
		}
		if !s.Counted || s.Unit != "pages" || s.Total <= 0 || s.Done != s.Total {
			t.Errorf("%s decode at its end: %+v, want a counted phase in pages with Done == Total > 0", table, s)
		}
	}
}
