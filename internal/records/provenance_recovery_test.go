package records_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// getOne ingests recs on ids and returns the full first record.
func getOne(t *testing.T, c *evidence.Case, ids []string, recs ...records.Record) records.Full {
	t.Helper()
	res := recordstest.Ingest(t, c, testParser, ids, recs)
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

func problemKinds(p records.Provenance) map[records.ProblemKind]int {
	m := map[records.ProblemKind]int{}
	for _, pr := range p.Problems {
		m[pr.Kind]++
	}
	return m
}

func TestRecoveredRecordShowsRecoveredView(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(60))
	p := getOne(t, c, []string{rec.ID}, recoveredRec(rec.ID, ip(50))).Provenance
	v := p.Recovery
	if v == nil || v.ArtifactID != rec.ID || v.Hops != 0 || v.Recovery.Class != evidence.ClassDeletedFile ||
		v.Recovery.Alloc.Free != 8 || v.MinConfidence == nil || *v.MinConfidence != 60 || v.DeclaredRuns != nil {
		t.Fatalf("view %+v", v)
	}
	if !p.OK() {
		t.Fatalf("problems %+v", p.Problems)
	}
}

func TestLiveRecordOnRecoveredArtifactIsAProblem(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(60))
	forgedIngest(t, c, []string{rec.ID}, liveRec(rec.ID))
	full, err := newReader(t, c).Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	if p.Recovery == nil || p.Recovery.ArtifactID != rec.ID {
		t.Fatalf("view still set: %+v", p.Recovery)
	}
	if k := problemKinds(p); k[records.ProblemRecoveryLive] != 1 || len(p.Problems) != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestRecoveryLaunderingThroughExtractedPartitionImage(t *testing.T) {
	c := recordstest.NewCase(t)
	disk := recordstest.AddArtifact(t, c, "disk.img", mib)
	rv := &evidence.Recovery{
		Class: evidence.ClassCarved, Method: "carve-signature", Confidence: ip(40), Algorithm: "carve",
		Alloc: evidence.AllocSummary{Free: 16}, Scope: "raw", Content: "ok",
	}
	carved := recordstest.AddDerived(t, c, disk, "carved/raw/000001-x.img", evidence.KindCarve, make([]byte, 16), rv)
	file := recordstest.AddDerivedWith(t, c, "f.txt", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: carved.ID, ParentSHA256: carved.SHA256, Partition: 1, FSType: "ext4", FSPath: "/f.txt", FSID: "nid:5",
	}}, []byte("data"))
	p := getOne(t, c, []string{file.ID}, recoveredRec(file.ID, ip(30))).Provenance
	v := p.Recovery
	if v == nil || v.Hops != 1 || v.ArtifactID != carved.ID || v.MinConfidence == nil || *v.MinConfidence != 40 {
		t.Fatalf("view %+v", v)
	}
	if !p.OK() {
		t.Fatalf("problems %+v", p.Problems)
	}
}

func TestRecordConfidenceAboveChainIsAProblem(t *testing.T) {
	for name, conf := range map[string]*int{"above": ip(70), "none": nil} {
		t.Run(name, func(t *testing.T) {
			c, _, rec := recoveredCase(t, ip(60))
			forgedIngest(t, c, []string{rec.ID}, recoveredRec(rec.ID, conf))
			full, err := newReader(t, c).Get(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if k := problemKinds(full.Provenance); k[records.ProblemRecoveryConf] != 1 || len(full.Provenance.Problems) != 1 {
				t.Fatalf("%+v", full.Provenance.Problems)
			}
		})
	}
}

func TestRecoveredKindWithoutDescriptionIsStillRecovered(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(60))
	res := recordstest.Ingest(t, c, testParser, []string{rec.ID}, []records.Record{recoveredRec(rec.ID, ip(50))})
	recordstest.ClearRecoveryInManifest(t, c.Dir, rec.ID)
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	if p.Recovery == nil || p.Recovery.Recovery.Class != "unknown" || p.Recovery.ArtifactID != rec.ID {
		t.Fatalf("view %+v", p.Recovery)
	}
	if k := problemKinds(p); k[records.ProblemAuditDiffers] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestRecoveredRecordOnLiveChainIsANote(t *testing.T) {
	c, art := setup(t)
	p := getOne(t, c, []string{art.ID}, recoveredRec(art.ID, ip(50))).Provenance
	if p.Recovery != nil || !p.OK() {
		t.Fatalf("view %+v problems %+v", p.Recovery, p.Problems)
	}
	found := false
	for _, n := range p.Notes {
		found = found || strings.Contains(n, "recovered inside a live artifact")
	}
	if !found {
		t.Fatalf("notes %q", p.Notes)
	}
}

func TestDeclaredRunsChecks(t *testing.T) {
	const dir = "recovered/p1-mtfs/"
	rv := func(declared string) *evidence.Recovery {
		return &evidence.Recovery{
			Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(60),
			Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: 8}, Algorithm: evidence.AlgorithmRecover,
			DeclaredRunsArtifact: declared,
		}
	}
	runs := []evidence.Run{{Offset: 0, Length: 8}}
	tests := []struct {
		name  string
		build func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord
		state string
	}{
		{"ok", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			s := recordstest.AddRunsSidecar(t, c, img, dir+"000001-a.bin.declared.runs.jsonl", runs)
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
		}, "ok"},
		{"missing", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv("nope"))
		}, "missing"},
		{"not a runs sidecar", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			src := evidence.Source{Kind: "file", DeviceID: "dev1", Derived: &evidence.Derivation{ParentID: img.ID, ParentSHA256: img.SHA256}}
			s := recordstest.AddDerivedWith(t, c, dir+"000001-a.bin.declared.runs.jsonl", src, []byte("x"))
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
		}, "not-runs-sidecar"},
		{"other parent", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			other := recordstest.AddArtifact(t, c, "other.img", []byte("other"))
			s := recordstest.AddRunsSidecar(t, c, other, dir+"000001-a.bin.declared.runs.jsonl", runs)
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
		}, "other-parent"},
		{"other directory", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			s := recordstest.AddRunsSidecar(t, c, img, "elsewhere/000001-a.bin.declared.runs.jsonl", runs)
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
		}, "other-directory"},
		{"referenced by two", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			s := recordstest.AddRunsSidecar(t, c, img, dir+"000001-a.bin.declared.runs.jsonl", runs)
			recordstest.AddDerived(t, c, img, dir+"000002-b.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
			return recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
		}, "multiply-referenced"},
		{"equal to the captured runs", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) evidence.ManifestRecord {
			s := recordstest.AddRunsSidecar(t, c, img, dir+"000001-a.bin.runs.jsonl", runs)
			d := &evidence.Derivation{
				ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs",
				FSPath: "/a", FSID: "dentry:1:1:1", RunsArtifact: s.ID, Recovery: rv(s.ID),
			}
			src := evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: d}
			return recordstest.AddDerivedWith(t, c, dir+"000001-a.bin", src, []byte("abcdefgh"))
		}, "used-as-captured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			img := recordstest.AddArtifact(t, c, "disk.img", mib)
			art := tc.build(t, c, img)
			p := getOne(t, c, []string{art.ID}, recoveredRec(art.ID, ip(50))).Provenance
			if p.Recovery == nil || p.Recovery.DeclaredRuns == nil || p.Recovery.DeclaredRuns.State != tc.state {
				t.Fatalf("view %+v", p.Recovery)
			}
			k := problemKinds(p)
			if tc.state == "ok" {
				if len(p.Problems) != 0 {
					t.Fatalf("%+v", p.Problems)
				}
			} else if k[records.ProblemDeclaredRuns] != 1 || len(p.Problems) != 1 {
				t.Fatalf("%+v", p.Problems)
			}
		})
	}
}

// A sidecar that A declares and B uses as its captured runs is referenced twice, exactly as
// case verify counts references (E34).
func TestDeclaredRunsSharedWithACapturedReferenceIsMultiplyReferenced(t *testing.T) {
	const dir = "recovered/p1-mtfs/"
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "disk.img", mib)
	s := recordstest.AddRunsSidecar(t, c, img, dir+"000001-a.bin.declared.runs.jsonl", []evidence.Run{{Offset: 0, Length: 8}})
	rv := func(declared string) *evidence.Recovery {
		return &evidence.Recovery{
			Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(60),
			Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: 8}, Algorithm: evidence.AlgorithmRecover,
			DeclaredRunsArtifact: declared,
		}
	}
	b := evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/b", FSID: "dentry:1:1:2",
		RunsArtifact: s.ID, Recovery: rv(""),
	}}
	recordstest.AddDerivedWith(t, c, dir+"000002-b.bin", b, []byte("abcdefgh"))
	a := recordstest.AddDerived(t, c, img, dir+"000001-a.bin", evidence.KindRecover, []byte("abcdefgh"), rv(s.ID))
	p := getOne(t, c, []string{a.ID}, recoveredRec(a.ID, ip(50))).Provenance
	if p.Recovery == nil || p.Recovery.DeclaredRuns == nil || p.Recovery.DeclaredRuns.State != "multiply-referenced" {
		t.Fatalf("view %+v", p.Recovery)
	}
	if k := problemKinds(p); k[records.ProblemDeclaredRuns] != 1 || len(p.Problems) != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

// Get's confidence rule is verify R7's: a record without a confidence is a problem only when the
// chain has one, a confidence equal to the chain's lowest is fine, one above is a problem at the
// hop count of the recovered artifact (E35).
func TestRecordConfidenceRuleMirrorsVerify(t *testing.T) {
	tests := []struct {
		name     string
		chain    *int
		rec      *int
		wantProb bool
	}{
		{"no confidence anywhere", nil, nil, false},
		{"equal to the lowest", ip(60), ip(60), false},
		{"one above the lowest", ip(60), ip(61), true},
		{"missing while the chain has one", ip(60), nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _, rec := recoveredCase(t, tc.chain)
			forgedIngest(t, c, []string{rec.ID}, recoveredRec(rec.ID, tc.rec))
			full, err := newReader(t, c).Get(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			p := full.Provenance
			if got := len(p.Problems) > 0; got != tc.wantProb {
				t.Fatalf("problems %+v, want problem=%v", p.Problems, tc.wantProb)
			}
			if tc.wantProb && (p.Problems[0].Kind != records.ProblemRecoveryConf || p.Problems[0].Hop != 0) {
				t.Fatalf("%+v", p.Problems)
			}
			// verify agrees with Get on the same case
			got2 := verifyFlagsConfidence(t, c)
			if got2 != tc.wantProb {
				t.Fatalf("verify flags=%v, Get problem=%v", got2, tc.wantProb)
			}
		})
	}
}

// The minimum runs over every recovered artifact of the chain, not the nearest one (E35), and the
// problem's Hop is the number of hops to the artifact that carries the minimum.
func TestRecordConfidenceUsesTheMinimumAcrossTwoRecoveredArtifacts(t *testing.T) {
	c := recordstest.NewCase(t)
	disk := recordstest.AddArtifact(t, c, "disk.img", mib)
	carved := recordstest.AddRecovered(t, c, disk, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), ip(40))
	near := recordstest.AddRecovered(t, c, carved, "recovered/p1-mtfs/000002-b.bin", []byte("abcdefgh"), ip(70))
	forgedIngest(t, c, []string{near.ID}, recoveredRec(near.ID, ip(50)))
	full, err := newReader(t, c).Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	if p.Recovery == nil || p.Recovery.MinConfidence == nil || *p.Recovery.MinConfidence != 40 {
		t.Fatalf("view %+v", p.Recovery)
	}
	if len(p.Problems) != 1 || p.Problems[0].Kind != records.ProblemRecoveryConf || p.Problems[0].Hop != p.Recovery.Hops {
		t.Fatalf("%+v hops %d", p.Problems, p.Recovery.Hops)
	}
}

// A live record laundered through a carved partition image: the problem carries the hop count.
func TestLiveRecordProblemCarriesTheHop(t *testing.T) {
	c := recordstest.NewCase(t)
	disk := recordstest.AddArtifact(t, c, "disk.img", mib)
	carved := recordstest.AddDerived(t, c, disk, "carved/raw/000001-x.img", evidence.KindCarve, make([]byte, 16), &evidence.Recovery{
		Class: evidence.ClassCarved, Method: "carve-signature", Confidence: ip(40), Algorithm: "carve",
		Alloc: evidence.AllocSummary{Free: 16}, Scope: "raw", Content: "ok",
	})
	file := recordstest.AddDerivedWith(t, c, "f.txt", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: carved.ID, ParentSHA256: carved.SHA256, Partition: 1, FSType: "ext4", FSPath: "/f.txt", FSID: "nid:5",
	}}, []byte("data"))
	forgedIngest(t, c, []string{file.ID}, liveRec(file.ID))
	full, err := newReader(t, c).Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	if len(p.Problems) != 1 || p.Problems[0].Kind != records.ProblemRecoveryLive || p.Problems[0].Hop != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

// verifyFlagsConfidence reports whether case verify's R7 flags a record confidence.
func verifyFlagsConfidence(t *testing.T, c *evidence.Case) bool {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "has no confidence") || strings.Contains(p, "is above the") {
			return true
		}
	}
	return false
}
