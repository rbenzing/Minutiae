package records_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// E38: a hop whose manifest record no longer matches its hash-chained audit entry is never
// translated, even though every link, hash and run checks out.
func TestOffsetUnboundAuditHopIsUnavailable(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 100, Length: 8}}}), []byte("abcdefgh"))
	recordstest.SetManifestSource(t, c.Dir, f.ID, func(s *evidence.Source) { s.Derived.FSPath = "/forged" })
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "not bound to its audit entry")
	if !strings.Contains(p.Offset.Reason, f.ID) || !strings.Contains(p.Offset.Reason, string(evidence.AuditDiffers)) {
		t.Fatalf("reason %q", p.Offset.Reason)
	}
	if len(p.Offset.Hops) != 1 || len(p.Offset.Hops[0].Extents) != 1 {
		t.Fatalf("hop extents are kept: %+v", p.Offset.Hops)
	}
}

func verifyReport(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func hasProblemText(rep evidence.VerifyReport, part string) bool {
	for _, p := range rep.Problems {
		if strings.Contains(p, part) {
			return true
		}
	}
	return false
}

// E39: the record range is checked against the artifact for EVERY record, before and independent
// of translation, with verify's P9 arithmetic. Get and case verify agree on each case.
func TestGetRangeCheckAgreesWithVerify(t *testing.T) {
	type tc struct {
		name        string
		build       func(t *testing.T, c *evidence.Case) evidence.ManifestRecord
		off, n      int64
		wantOutside bool
	}
	derivedWithRuns := func(t *testing.T, c *evidence.Case) evidence.ManifestRecord {
		img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
		return recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 7}}}), patterned(7))
	}
	plain := func(t *testing.T, c *evidence.Case) evidence.ManifestRecord {
		return recordstest.AddArtifact(t, c, "a.bin", patterned(1<<20))
	}
	noRuns := func(t *testing.T, c *evidence.Case) evidence.ManifestRecord {
		img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
		return recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{}), patterned(7))
	}
	cases := []tc{
		{"non-derived beyond", plain, 4, 5000000, true},
		{"non-derived inside", plain, 4, 100, false},
		{"derived without runs beyond", noRuns, 4, 50, true},
		{"derived with runs beyond", derivedWithRuns, 4, 50, true},
		{"derived with runs inside", derivedWithRuns, 2, 5, false},
		{"range ends at the size", derivedWithRuns, 3, 4, false},
		{"range starts past the size", derivedWithRuns, 8, 0, true},
		{"length near max int64", derivedWithRuns, 3, 1<<63 - 1, true},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			art := k.build(t, c)
			p := tamperedRangeProv(t, c, art, k.off, k.n)
			rep := verifyReport(t, c)
			if outside := hasProblemText(rep, "range beyond artifact"); outside != k.wantOutside {
				t.Fatalf("verify range problem = %v, want %v: %q", outside, k.wantOutside, rep.Problems)
			}
			kinds := problemKinds(p)
			if got := kinds[records.ProblemRangeOutside]; (got == 1) != k.wantOutside || got > 1 {
				t.Fatalf("Get problems %+v, want range-outside = %v", p.Problems, k.wantOutside)
			}
			if k.wantOutside {
				requireUnavailable(t, p, "outside the artifact")
			}
		})
	}
}

// recoveredSource is a deleted-file recover derivation with the given runs.
func recoveredSource(img evidence.ManifestRecord, runs []evidence.Run) evidence.Source {
	rv := &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(60),
		Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: 8}, Algorithm: evidence.AlgorithmRecover,
	}
	return evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a", FSID: "dentry:1:1:1",
		Runs: runs, Recovery: rv,
	}}
}

func incompleteRecovered(t *testing.T, c *evidence.Case, src evidence.Source, data []byte) evidence.ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq-derived", "recovered/p1-mtfs/000001-a.bin", src, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		return errors.New("test: copy cut off")
	})
	if err == nil || !rec.Incomplete {
		t.Fatalf("record %+v err %v", rec, err)
	}
	return rec
}

// E40: for a recovered kind the runs must add up to the size and hold no hole, whether or not the
// artifact is incomplete (R4); Get flags what verify flags, and accepts what verify accepts.
func TestGetRecoveredRunsAgreeWithVerify(t *testing.T) {
	data8 := []byte("abcdefgh")
	cases := []struct {
		name       string
		runs       []evidence.Run
		data       []byte
		incomplete bool
		verifyText string // "" = verify must report no runs problem and Get must translate
	}{
		{"incomplete, runs longer than bytes held", []evidence.Run{{Offset: 2000, Length: 1000}}, patterned(100), true, "runs cover 1000 bytes but the artifact holds 100"},
		{"incomplete, runs shorter than bytes held", []evidence.Run{{Offset: 2000, Length: 50}}, patterned(100), true, "runs cover 50 bytes but the artifact holds 100"},
		{"complete, runs longer", []evidence.Run{{Offset: 0, Length: 8}}, []byte("abcd"), false, "runs cover 8 bytes but the artifact holds 4"},
		{"hole only", []evidence.Run{{Offset: -1, Length: 8}}, data8, false, "run 0 is a hole"},
		{"hole among runs", []evidence.Run{{Offset: 100, Length: 4}, {Offset: -1, Length: 4}}, data8, false, "run 1 is a hole"},
		{"complete, exact", []evidence.Run{{Offset: 100, Length: 8}}, data8, false, ""},
		{"incomplete, exact", []evidence.Run{{Offset: 100, Length: 8}}, data8, true, ""},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
			src := recoveredSource(img, k.runs)
			var f evidence.ManifestRecord
			if k.incomplete {
				f = incompleteRecovered(t, c, src, k.data)
			} else {
				f = recordstest.AddDerivedWith(t, c, "recovered/p1-mtfs/000001-a.bin", src, k.data)
			}
			r := recoveredRec(f.ID, ip(50))
			r.Range = &records.Range{Offset: 0, Length: 2}
			p := getOne(t, c, []string{f.ID}, r).Provenance
			rep := verifyReport(t, c)
			if k.verifyText == "" {
				if hasProblemText(rep, "runs cover") || hasProblemText(rep, "is a hole") {
					t.Fatalf("verify flags runs: %q", rep.Problems)
				}
				requireTranslated(t, p)
				if kinds := problemKinds(p); kinds[records.ProblemRunsInconsistent] != 0 {
					t.Fatalf("%+v", p.Problems)
				}
				return
			}
			if !hasProblemText(rep, k.verifyText) {
				t.Fatalf("verify does not say %q: %q", k.verifyText, rep.Problems)
			}
			requireUnavailable(t, p, k.verifyText)
			if kinds := problemKinds(p); kinds[records.ProblemRunsInconsistent] != 1 {
				t.Fatalf("%+v", p.Problems)
			}
		})
	}
}

// E9's tolerance stays for an incomplete artifact that is not of a recovered kind, and verify
// has no R4 rule for it.
func TestGetIncompleteNonRecoveredToleranceAgreesWithVerify(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := incompleteDerived(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 2000, Length: 1000}}}), patterned(100))
	p := offsetProv(t, c, f, &records.Range{Offset: 10, Length: 20})
	requireTranslated(t, p)
	if rep := verifyReport(t, c); hasProblemText(rep, "runs cover") {
		t.Fatalf("%q", rep.Problems)
	}
}

// M3: runs that point past an intermediate artifact are an inconsistency of the runs of the hop
// before it, not a record range outside anything (verify P9 does not flag it either).
func TestOffsetMidChainPieceBeyondParentIsRunsInconsistent(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	mid := recordstest.AddDerivedWith(t, c, "mid.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 100, Length: 64}}}), patterned(64))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(mid, evidence.Derivation{Runs: []evidence.Run{{Offset: 100, Length: 16}}}), patterned(16))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "outside the artifact")
	if k := problemKinds(p); k[records.ProblemRunsInconsistent] != 1 || k[records.ProblemRangeOutside] != 0 {
		t.Fatalf("%+v", p.Problems)
	}
}

// M5: the "runs describe more than the bytes held" note belongs to a translated offset only.
func TestOffsetExceedNoteOnlyWhenTranslated(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := incompleteDerived(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 2000, Length: 1000}}}), patterned(100))
	recordstest.SetManifestSource(t, c.Dir, f.ID, func(s *evidence.Source) { s.Derived.ParentSHA256 = strings.Repeat("0", 64) })
	p := offsetProv(t, c, f, &records.Range{Offset: 10, Length: 20})
	requireUnavailable(t, p, "parent hash differs")
	if hasNote(p, "more than the bytes held") {
		t.Fatalf("notes %q", p.Notes)
	}
}

// M4: when the last hop yields more than MaxExtentsPerHop extents from several pieces, the image
// extents are cut at the cap and the hop says so.
func TestOffsetLastHopExtentsAreCappedAcrossPieces(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	runs := make([]evidence.Run, 300)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(2 * i), Length: 1}
	}
	mid := recordstest.AddDerivedWith(t, c, "mid.bin", extractSrc(img, evidence.Derivation{Runs: runs}), patterned(300))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(mid, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 150}, {Offset: 150, Length: 150}}}), patterned(300))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 300})
	requireTranslated(t, p)
	if len(p.Offset.Image) != records.MaxExtentsPerHop || len(p.Offset.Hops) != 2 ||
		!p.Offset.Hops[1].Truncated || p.Offset.Hops[1].Total != 300 || len(p.Offset.Hops[1].Extents) != records.MaxExtentsPerHop {
		t.Fatalf("image %d hops %+v", len(p.Offset.Image), p.Offset.Hops)
	}
}
