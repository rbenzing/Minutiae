package records_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func ip(n int) *int { return &n }

// recoveredCase is a case with a plain image artifact and a recovered artifact
// (deleted-file, confidence conf) derived from it.
func recoveredCase(t *testing.T, conf *int) (c *evidence.Case, img, rec evidence.ManifestRecord) {
	t.Helper()
	c = recordstest.NewCase(t)
	img = recordstest.AddArtifact(t, c, "disk.img", mib)
	rec = recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), conf)
	return c, img, rec
}

func startOn(t *testing.T, c *evidence.Case, ids ...string) *records.Writer {
	t.Helper()
	w := newWriter(t, c, testParser, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: ids}); err != nil {
		t.Fatal(err)
	}
	return w
}

func liveRec(art string) records.Record {
	return records.Record{Type: "message", ArtifactID: art, Summary: "live", Payload: map[string]any{"k": "v"}}
}

func recoveredRec(art string, conf *int) records.Record {
	return records.Record{
		Type: "message", ArtifactID: art, Summary: "recovered", Payload: map[string]any{"k": "v"},
		Deleted: true, Recovery: "fat-contiguous", Confidence: conf,
	}
}

func TestAddRejectsLiveRecordOnRecoveredArtifact(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	w := startOn(t, c, rec.ID)
	before := len(auditOf(t, c, ""))
	err := w.Add(ctx, liveRec(rec.ID))
	if !errors.Is(err, records.ErrRecoveredArtifactLiveRecord) || !errors.Is(err, records.ErrInvalidRecord) {
		t.Fatalf("Add = %v, want ErrRecoveredArtifactLiveRecord", err)
	}
	if got := len(auditOf(t, c, "")); got != before {
		t.Errorf("audit grew from %d to %d entries on a refused record", before, got)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, "records"); n != 0 {
		t.Errorf("%d records stored, want 0", n)
	}
}

func TestAddAcceptsRecoveredRecordWithinCap(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	w := startOn(t, c, rec.ID)
	for _, conf := range []int{55, 54, 0} {
		if err := w.Add(ctx, recoveredRec(rec.ID, ip(conf))); err != nil {
			t.Errorf("confidence %d: %v", conf, err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAddRejectsConfidenceAboveArtifact(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	w := startOn(t, c, rec.ID)
	for name, conf := range map[string]*int{"cap+1": ip(56), "100": ip(100), "nil": nil} {
		before := len(auditOf(t, c, ""))
		err := w.Add(ctx, recoveredRec(rec.ID, conf))
		if !errors.Is(err, records.ErrConfidenceAboveArtifact) || !errors.Is(err, records.ErrInvalidRecord) {
			t.Errorf("%s: Add = %v, want ErrConfidenceAboveArtifact", name, err)
		}
		if got := len(auditOf(t, c, "")); got != before {
			t.Errorf("%s: audit grew on a refused record", name)
		}
	}
}

func TestAddRejectsLiveRecordOnDescendantOfRecoveredArtifact(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "disk.img", mib)
	a := recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), ip(70))
	b := recordstest.AddDerived(t, c, a, "recovered/p1-mtfs/000002-b.bin", evidence.KindRecover, []byte("abcd"),
		&evidence.Recovery{
			Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(40), Basis: []string{"x"},
			Alloc: evidence.AllocSummary{Free: 4}, Algorithm: evidence.AlgorithmRecover,
		})
	d := recordstest.AddDerived(t, c, b, "extracted/c.txt", "extract", []byte("ab"), nil)
	w := startOn(t, c, d.ID)
	if err := w.Add(ctx, liveRec(d.ID)); !errors.Is(err, records.ErrRecoveredArtifactLiveRecord) {
		t.Errorf("live record on a descendant: %v, want ErrRecoveredArtifactLiveRecord", err)
	}
	// the chain minimum (40, not the nearest ancestor's 40 nor the root's 70) is the cap
	if err := w.Add(ctx, recoveredRec(d.ID, ip(41))); !errors.Is(err, records.ErrConfidenceAboveArtifact) {
		t.Errorf("confidence 41 over a chain minimum of 40: %v", err)
	}
	if err := w.Add(ctx, recoveredRec(d.ID, ip(40))); err != nil {
		t.Errorf("confidence 40: %v", err)
	}
	// a chain whose nearest recovered ancestor holds the HIGHER confidence is still capped by the lower root
	c2 := recordstest.NewCase(t)
	img2 := recordstest.AddArtifact(t, c2, "disk.img", mib)
	a2 := recordstest.AddRecovered(t, c2, img2, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), ip(40))
	b2 := recordstest.AddDerived(t, c2, a2, "recovered/p1-mtfs/000002-b.bin", evidence.KindRecover, []byte("abcd"),
		&evidence.Recovery{
			Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(70), Basis: []string{"x"},
			Alloc: evidence.AllocSummary{Free: 4}, Algorithm: evidence.AlgorithmRecover,
		})
	w2 := startOn(t, c2, b2.ID)
	if err := w2.Add(ctx, recoveredRec(b2.ID, ip(70))); !errors.Is(err, records.ErrConfidenceAboveArtifact) {
		t.Errorf("confidence 70 over a root cap of 40: %v", err)
	}
	if err := w2.Add(ctx, recoveredRec(b2.ID, ip(40))); err != nil {
		t.Errorf("confidence 40: %v", err)
	}
}

func TestAddLiveRecordOnPlainArtifactUnchanged(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "disk.img", mib)
	plain := recordstest.AddDerived(t, c, img, "extracted/p.txt", "extract", []byte("abcd"), nil)
	w := startOn(t, c, img.ID, plain.ID)
	for _, id := range []string{img.ID, plain.ID} {
		if err := w.Add(ctx, liveRec(id)); err != nil {
			t.Errorf("live record on %s: %v", id, err)
		}
		for _, conf := range []*int{nil, ip(0), ip(100)} {
			if err := w.Add(ctx, recoveredRec(id, conf)); err != nil {
				t.Errorf("recovered record (confidence %v) on %s: %v", conf, id, err)
			}
		}
	}
}

func TestRecoveredRecordIsIndexedAndSearchableAndMarked(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	r := recoveredRec(rec.ID, ip(50))
	r.Summary = "zebrafinch recovered note"
	w := startOn(t, c, rec.ID)
	if err := w.Add(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	rd := newReader(t, c)
	search := func(tri records.Tri) []records.Hit {
		t.Helper()
		res, err := rd.Search(ctx, records.Filter{Text: mustCompile(t, "zebrafinch", records.TextOptions{}), Recovered: tri}, records.Page{}, records.SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Hits
	}
	if hits := search(records.Only); len(hits) != 1 || !hits[0].Row.Recovered || hits[0].Row.Method != "fat-contiguous" {
		t.Errorf("Recovered: Only = %+v, want the one recovered row, marked", hits)
	}
	if hits := search(records.None); len(hits) != 0 {
		t.Errorf("Recovered: None found %d hits, want 0", len(hits))
	}
	if ids := recordstest.IndexedIDs(t, c, "records_fts"); len(ids) != 1 {
		t.Errorf("indexed ids %v, want the one record", ids)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("case verify: %q", rep.Problems)
	}
}
