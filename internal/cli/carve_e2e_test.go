package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestImageCarveArtifactScope(t *testing.T) {
	e := newCarveCLIEnv(t)
	if code, out := e.image(t, "unalloc", e.ref, "-p", "1"); code != 0 {
		t.Fatalf("unalloc: %d\n%s", code, out)
	}
	var unalloc, extract evidence.ManifestRecord
	recs, err := manifest(e.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Source.Kind == "unallocated" {
			unalloc = r
		}
	}
	if unalloc.ID == "" {
		t.Fatal("no unallocated export")
	}
	code, out := e.image(t, "carve", "--artifact", unalloc.ID)
	if code != 0 || !strings.Contains(out, "carved/p1-mtfs/") {
		t.Fatalf("carve of the export: %d\n%s", code, out)
	}
	for _, r := range carveCLIRecords(t, e.c) {
		if d := r.Source.Derived; d.ParentID != e.ref || d.Recovery.Scope != "unallocated" {
			t.Errorf("export carve %s: parent %s scope %s", r.Path, d.ParentID, d.Recovery.Scope)
		}
	}
	// a carve of an extracted file that holds a PNG
	holder := append(append([]byte{}, recPat(3, 700)...), e.png...)
	e2 := newCarveCLIEnv(t, fstest.Node{Path: "/holder.bin", Data: holder})
	if code, out := e2.image(t, "extract", e2.ref, "/holder.bin"); code != 0 {
		t.Fatalf("extract: %d\n%s", code, out)
	}
	recs, err = manifest(e2.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Source.Kind == "extract" {
			extract = r
		}
	}
	code, out = e2.image(t, "carve", "--artifact", extract.ID, "--unaligned")
	if code != 0 || !strings.Contains(out, "carved/raw/") {
		t.Fatalf("carve of the extract: %d\n%s", code, out)
	}
	n := 0
	for _, r := range carveCLIRecords(t, e2.c) {
		if d := r.Source.Derived; d.Recovery.Scope == "artifact" && d.ParentID == extract.ID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d scope-artifact carves of the extract, want 1", n)
	}
	if code, out := run(t, e2.d, "case", "verify", "--case", e2.c); code != 0 {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	if code, _ := e.image(t, "carve", "--artifact", "nosuchid"); code == 0 {
		t.Error("a missing artifact id exits 0")
	}
}

// (PASSES ON ARRIVAL) 5C resolves any recovered kind, so a record on a carved artifact reaches the image offset
// with no change to records. Mutation step: make the 5C offset translation ignore the carve's inline run (treat
// it as a hole); the image-offset assertion fails.
func TestRecordsShowResolvesCarvedChain(t *testing.T) {
	e := newCarveCLIEnv(t)
	if code, out := e.carve(t, "--types", "jpeg"); code != 0 {
		t.Fatalf("carve: %d\n%s", code, out)
	}
	recs := carveCLIRecords(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d carve artifacts, want 1", len(recs))
	}
	art := recs[0]
	c, err := evidence.Open(e.c)
	if err != nil {
		t.Fatal(err)
	}
	recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{{
		Type: "note", ArtifactID: art.ID, Summary: "carved", Payload: map[string]any{}, Deleted: true,
		Recovery: "carve-validated", Confidence: rptr(70), Range: &records.Range{Offset: 2, Length: 4},
	}})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p := showClean(t, e.c, "1")
	if p.Status != "recovered" || p.Recovery == nil || p.Recovery.Recovery.Class != evidence.ClassCarved {
		t.Fatalf("status %q recovery %+v", p.Status, p.Recovery)
	}
	off := art.Source.Derived.Runs[0].Offset
	if len(p.Offset.Image) != 1 || p.Offset.Image[0].ImageOffset != off+2 {
		t.Fatalf("image offset %+v, want %d", p.Offset.Image, off+2)
	}
	requireOracle(t, p, artifactFile(t, e.c, art), e.media, 2, 4)
}
