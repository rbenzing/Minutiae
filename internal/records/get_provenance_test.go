package records_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// getFirst ingests n records on art and returns the full first one.
func getFirst(t *testing.T, c *evidence.Case, art evidence.ManifestRecord) records.Full {
	t.Helper()
	res := recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, 1, 7))
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

func extractedFile(t *testing.T, c *evidence.Case) (img, file evidence.ManifestRecord) {
	t.Helper()
	img = recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	file = recordstest.AddDerivedWith(t, c, "f.txt", evidence.Source{Kind: "extract", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 2, PartitionOffset: 1024, FSType: "ext4",
		FSPath: "/a/b.txt", FSID: "nid:5", Snapshot: &evidence.SnapshotRef{Name: "snap1", Xid: 261},
	}}, mib)
	return img, file
}

func TestGetProvenanceNonDerivedArtifact(t *testing.T) {
	c, art := setup(t)
	p := getFirst(t, c, art).Provenance
	if !p.OK() || !p.ReachedRoot || len(p.Chain) != 1 || p.Chain[0].Link != records.LinkRoot || p.Chain[0].Artifact.ID != art.ID {
		t.Fatalf("%+v", p)
	}
	if p.Chain[0].Audit.State != evidence.AuditBound {
		t.Fatalf("audit %+v", p.Chain[0].Audit)
	}
	if len(p.Notes) != 1 || p.Notes[0] != "artifact is not derived: no image offset" {
		t.Fatalf("notes %q", p.Notes)
	}
}

func TestGetProvenanceExtractedFile(t *testing.T) {
	c := recordstest.NewCase(t)
	img, file := extractedFile(t, c)
	p := getFirst(t, c, file).Provenance
	if !p.OK() || !p.ReachedRoot || len(p.Chain) != 2 {
		t.Fatalf("%+v", p)
	}
	if p.Chain[0].Artifact.ID != file.ID || p.Chain[1].Artifact.ID != img.ID ||
		p.Chain[0].Link != records.LinkOK || p.Chain[1].Link != records.LinkRoot {
		t.Fatalf("chain %+v", p.Chain)
	}
	d := p.Chain[0].Artifact.Source.Derived
	if d == nil || d.Partition != 2 || d.FSPath != "/a/b.txt" || d.FSID != "nid:5" || d.FSType != "ext4" ||
		d.Snapshot == nil || d.Snapshot.Name != "snap1" || d.Snapshot.Xid != 261 {
		t.Fatalf("derivation %+v", d)
	}
	for i, h := range p.Chain {
		if h.Audit.State != evidence.AuditBound || h.Audit.Seq == 0 {
			t.Fatalf("hop %d audit %+v", i, h.Audit)
		}
	}
}

func TestGetProvenanceManifestSourceRewrittenIsAProblem(t *testing.T) {
	c := recordstest.NewCase(t)
	_, file := extractedFile(t, c)
	res := recordstest.Ingest(t, c, testParser, []string{file.ID}, recordstest.Records(file.ID, 1, 7))
	recordstest.SetManifestSource(t, c.Dir, file.ID, func(s *evidence.Source) { s.Derived.FSPath = "/forged" })
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	if p.OK() || len(p.Problems) != 1 || p.Problems[0].Kind != records.ProblemAuditDiffers || p.Problems[0].Hop != 0 ||
		!strings.Contains(p.Problems[0].Detail, "source") {
		t.Fatalf("%+v", p.Problems)
	}
	if p.Chain[0].Artifact.Source.Derived.FSPath != "/forged" || p.Chain[0].Audit.State != evidence.AuditDiffers {
		t.Fatalf("the manifest value is shown as it is: %+v", p.Chain[0])
	}
}

func TestGetProvenanceParentSwapped(t *testing.T) {
	c := recordstest.NewCase(t)
	_, file := extractedFile(t, c)
	other := recordstest.AddArtifact(t, c, "other.bin", []byte("different"))
	res := recordstest.Ingest(t, c, testParser, []string{file.ID}, recordstest.Records(file.ID, 1, 7))
	recordstest.SetManifestSource(t, c.Dir, file.ID, func(s *evidence.Source) { s.Derived.ParentID = other.ID })
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	p := full.Provenance
	kinds := map[records.ProblemKind]int{}
	for _, pr := range p.Problems {
		kinds[pr.Kind]++
	}
	if kinds[records.ProblemParentHash] != 1 || kinds[records.ProblemAuditDiffers] != 1 || len(p.Problems) != 2 {
		t.Fatalf("%+v", p.Problems)
	}
	if len(p.Chain) != 2 || p.Chain[1].Artifact.ID != other.ID || p.Chain[1].Link != records.LinkHashDiffers {
		t.Fatalf("chain %+v", p.Chain)
	}
	if d := p.Chain[0].Artifact.Source.Derived; d.FSPath != "/a/b.txt" || d.FSID != "nid:5" || d.Partition != 2 {
		t.Fatalf("other fields still returned: %+v", d)
	}
}

func TestGetHostileManifestIsBounded(t *testing.T) {
	const n = 100000
	c, art := setup(t)
	res := recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, 1, 7))
	segs := make([]evidence.SegmentRef, n)
	for i := range segs {
		segs[i] = evidence.SegmentRef{ID: fmt.Sprintf("seg%d", i), SHA256: "x"}
	}
	// the artifact is rewritten to derive from a 2-cycle with a 100k-segment parent list
	recordstest.SetManifestSource(t, c.Dir, art.ID, func(s *evidence.Source) {
		s.Kind = "extract"
		s.Derived = &evidence.Derivation{ParentID: "cyc1", ParentSHA256: "x", ParentSegments: segs}
	})
	f, err := os.OpenFile(filepath.Join(c.Dir, "manifest.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test case dir
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	add := func(r evidence.ManifestRecord) {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	cyc := func(id, parent string) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: id, Path: "artifacts/" + id, SHA256: "x", Source: evidence.Source{
			Kind:    "extract",
			Derived: &evidence.Derivation{ParentID: parent, ParentSHA256: "x"},
		}}
	}
	add(cyc("cyc1", "cyc2"))
	add(cyc("cyc2", "cyc1"))
	for i := 0; i < n; i++ {
		add(evidence.ManifestRecord{ID: fmt.Sprintf("filler%d", i), Path: fmt.Sprintf("artifacts/filler%d", i), SHA256: "y"})
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("took %v", d)
	}
	p := full.Provenance
	per := map[records.ProblemKind]int{}
	for _, pr := range p.Problems {
		per[pr.Kind]++
	}
	for k, v := range per {
		if v > records.MaxProblemsPerKind {
			t.Fatalf("%s: %d problems listed", k, v)
		}
	}
	if per[records.ProblemSegment] != records.MaxProblemsPerKind || per[records.ProblemCycle] != 1 || p.ProblemsSuppressed != n-records.MaxProblemsPerKind {
		t.Fatalf("kinds %v suppressed %d", per, p.ProblemsSuppressed)
	}
	if p.OK() || p.Chain[0].SegmentsTotal != n || len(p.Chain[0].Segments) != records.MaxShownSegments {
		t.Fatalf("total %d shown %d", p.Chain[0].SegmentsTotal, len(p.Chain[0].Segments))
	}
}
