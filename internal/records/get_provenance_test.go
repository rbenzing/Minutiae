package records_test

import (
	"encoding/json"
	"errors"
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

// auditFailure damages audit.jsonl after the record is stored and returns what Get says.
func getAfterAuditDamage(t *testing.T, damage func(path string)) error {
	t.Helper()
	c, art := setup(t)
	res := recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, 1, 7))
	damage(filepath.Join(c.Dir, "audit.jsonl"))
	_, err := newReader(t, c).Get(ctx, res.FirstID)
	return err
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test case dir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// E30: an audit log that is corrupt, torn or gone is an integrity error (exit 4), never a plain one.
func TestGetAuditLogDamageIsIntegrity(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"corrupt line":   func(t *testing.T, p string) { appendTo(t, p, "this is not json\n") },
		"torn last line": func(t *testing.T, p string) { appendTo(t, p, `{"seq":99,"act`) },
		"missing file": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Skipf("cannot remove an open audit log here: %v", err)
			}
		},
		"empty file": func(t *testing.T, p string) {
			if err := os.Truncate(p, 0); err != nil {
				t.Skipf("cannot truncate an open audit log here: %v", err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			err := getAfterAuditDamage(t, func(p string) { damage(t, p) })
			if !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("want ErrIntegrity, got %v", err)
			}
		})
	}
}

// E30: a plain I/O error reading the audit log stays plain (exit 1, not 4).
func TestGetAuditLogIOErrorStaysPlain(t *testing.T) {
	err := getAfterAuditDamage(t, func(p string) {
		if rmErr := os.Remove(p); rmErr != nil {
			t.Skipf("cannot remove an open audit log here: %v", rmErr)
		}
		if mkErr := os.Mkdir(p, 0o700); mkErr != nil { // opens, but cannot be read as a file
			t.Skip(mkErr)
		}
	})
	if err == nil || errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("want a plain error, got %v", err)
	}
}

// E30: a derivation chain deeper than MaxDerivedDepth over a 100k-artifact manifest is bounded and too-deep.
func TestGetDeepChainOverHugeManifestIsTooDeep(t *testing.T) {
	const fillers, links = 100000, 20
	c, art := setup(t)
	res := recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, 1, 7))
	node := func(i int) string { return fmt.Sprintf("link%d", i) }
	recordstest.SetManifestSource(t, c.Dir, art.ID, func(s *evidence.Source) {
		s.Kind = "extract"
		s.Derived = &evidence.Derivation{ParentID: node(0), ParentSHA256: "x"}
	})
	f, err := os.OpenFile(filepath.Join(c.Dir, "manifest.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test case dir
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i := 0; i < links; i++ {
		r := evidence.ManifestRecord{ID: node(i), Path: "artifacts/" + node(i), SHA256: "x", Source: evidence.Source{Kind: "extract"}}
		if i < links-1 {
			r.Source.Derived = &evidence.Derivation{ParentID: node(i + 1), ParentSHA256: "x"}
		}
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < fillers; i++ {
		if err := enc.Encode(evidence.ManifestRecord{ID: fmt.Sprintf("f%d", i), Path: fmt.Sprintf("artifacts/f%d", i), SHA256: "y"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("took %v", d)
	}
	p := full.Provenance
	if len(p.Chain) > evidence.MaxDerivedDepth+1 || p.ReachedRoot {
		t.Fatalf("chain %d reached root %v", len(p.Chain), p.ReachedRoot)
	}
	found := false
	for _, pr := range p.Problems {
		found = found || pr.Kind == records.ProblemTooDeep
	}
	if !found {
		t.Fatalf("no too-deep problem: %+v", p.Problems)
	}
}
