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
