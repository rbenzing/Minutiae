package recordstest_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func scaleFixture(t *testing.T, seed int64) (*recordstest.Scale, []string) {
	t.Helper()
	ids := []string{"a0", "a1", "a2", "a3", "a4", "tiny"}
	return recordstest.NewScale(recordstest.ScaleConfig{Seed: seed, Artifacts: ids}), ids
}

func TestScaleIsDeterministic(t *testing.T) {
	a, _ := scaleFixture(t, 5)
	b, _ := scaleFixture(t, 5)
	other, _ := scaleFixture(t, 6)
	for _, i := range []int64{0, 1, 99, 100_000, 7_654_321} {
		if !reflect.DeepEqual(a.At(i), b.At(i)) {
			t.Fatalf("record %d differs between two generators with one seed", i)
		}
	}
	if reflect.DeepEqual(a.At(1), other.At(1)) {
		t.Fatal("two seeds produced the same record")
	}
	blk := a.Block(10, 5)
	for k, r := range blk {
		if !reflect.DeepEqual(r, a.At(int64(10+k))) {
			t.Fatalf("Block[%d] differs from At(%d)", k, 10+k)
		}
	}
}

func TestScaleRowSize(t *testing.T) {
	s, _ := scaleFixture(t, 1)
	var total int64
	const n = 4000
	for _, r := range s.Block(0, n) {
		total += recordstest.RowBytes(r)
	}
	if avg := total / n; avg < 850 || avg > 1250 {
		t.Fatalf("average row data is %d bytes; the workload models ~1 KiB rows (850..1250)", avg)
	}
}

func TestScaleTinyArtifactIsExact(t *testing.T) {
	s, _ := scaleFixture(t, 2)
	got := 0
	for i := int64(0); i <= 250_000; i++ {
		// only the artifact choice matters here; At builds the whole record, so sample sparsely
		if i%997 != 0 && i%recordstest.TinyArtifactEvery != 0 {
			continue
		}
		r := s.At(i)
		if (r.ArtifactID == s.TinyArtifact()) != (i%recordstest.TinyArtifactEvery == 0) {
			t.Fatalf("record %d: tiny artifact choice is wrong (%s)", i, r.ArtifactID)
		}
		if r.ArtifactID == s.TinyArtifact() {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("tiny artifact received %d of records 0, 100000 and 200000, want 3", got)
	}
}

func TestScaleVocabularyIsSkewedAndSearchable(t *testing.T) {
	s, _ := scaleFixture(t, 3)
	seen := map[string]bool{}
	for r := 0; r < 20_000; r += 997 {
		w := s.Word(r)
		if len(w) < 4 || strings.ToLower(w) != w || strings.ContainsAny(w, " *\"-") {
			t.Fatalf("word %d = %q must be 4+ lowercase letters", r, w)
		}
		seen[w] = true
	}
	if len(seen) != len(seq(0, 20_000, 997)) {
		t.Fatal("words must be distinct")
	}
	has := func(word string, recs []records.Record) int {
		n := 0
		for _, r := range recs {
			if strings.Contains(" "+r.Summary+" "+r.Body+" ", " "+word+" ") {
				n++
			}
		}
		return n
	}
	recs := s.Block(0, 3000)
	hi, mid, lo := has(s.Word(0), recs), has(s.Word(200), recs), has(s.Word(10_000), recs)
	if hi <= mid || mid <= lo {
		t.Fatalf("word frequency must fall with rank: rank0 %d, rank200 %d, rank10000 %d", hi, mid, lo)
	}
}

func seq(from, to, step int) []int {
	var out []int
	for i := from; i < to; i += step {
		out = append(out, i)
	}
	return out
}

func TestScaleRecordsAreValidAndVerify(t *testing.T) {
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 6)
	s := recordstest.NewScale(recordstest.ScaleConfig{Seed: 4, Artifacts: ids})
	res := recordstest.Ingest(t, c, records.Parser{Name: "scale-parser", Version: "1"}, ids, s.Block(0, 3000))
	if res.Records != 3000 {
		t.Fatalf("ingested %d records, want 3000", res.Records)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %v", err, rep.Problems)
	}
}
