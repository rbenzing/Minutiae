package recordstest_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
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

// inBand reports whether got is within 4.5 standard deviations (binomial) of n*p, plus one.
func inBand(got, n int, p float64) bool {
	mean := float64(n) * p
	sd := math.Sqrt(mean * (1 - p))
	return math.Abs(float64(got)-mean) <= 4.5*sd+1
}

func TestScaleMixMatchesTheModel(t *testing.T) {
	const n = 4000
	s, ids := scaleFixture(t, 1)
	blk := s.Block(0, n)
	types := map[string]int{}
	byArt := map[string]int{}
	untimed, deleted, recovered, nonTiny := 0, 0, 0, 0
	for _, r := range blk {
		types[r.Type]++
		if r.ArtifactID != s.TinyArtifact() {
			nonTiny++
			byArt[r.ArtifactID]++
		}
		if r.Time == nil {
			untimed++
		}
		if r.Deleted {
			deleted++
			if r.Recovery != "" {
				recovered++
			}
		}
	}
	for typ, w := range map[string]int{"message": 50, "web_visit": 15, "call": 8, "location": 8, "file": 6, "event": 4, "contact": 3, "calendar_event": 3, "note": 3} {
		if !inBand(types[typ], n, float64(w)/100) {
			t.Errorf("type %s: %d of %d, want about %d%%", typ, types[typ], n, w)
		}
	}
	if !inBand(byArt[ids[0]], nonTiny, 0.40) || !inBand(byArt[ids[1]], nonTiny, 0.30) {
		t.Errorf("artifact split of %d records: a0 %d (want 40%%), a1 %d (want 30%%)", nonTiny, byArt[ids[0]], byArt[ids[1]])
	}
	if !inBand(untimed, n, 0.05) {
		t.Errorf("untimed %d of %d, want about 5%%", untimed, n)
	}
	if !inBand(deleted, n, 0.05) {
		t.Errorf("deleted %d of %d, want about 5%%", deleted, n)
	}
	if !inBand(recovered, deleted, 0.2) {
		t.Errorf("recovered %d of %d deleted, want about 1/5", recovered, deleted)
	}
}

// TestScaleGoldenDigest pins the generator across platforms and Go versions: the SHA-256 of the
// stored-text size of records 0..1999 at seed 7.
func TestScaleGoldenDigest(t *testing.T) {
	const want = "35888ebb19c062392dd33c2687c106b6805bd654895c654c8c20888f2a7a61bc"
	s, _ := scaleFixture(t, 7)
	h := sha256.New()
	for i := int64(0); i < 2000; i++ {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(recordstest.RowBytes(s.At(i))))
		h.Write(b[:])
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("golden digest %s, want %s", got, want)
	}
}
