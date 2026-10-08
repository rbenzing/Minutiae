package examine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// A carved or recovered artifact whose parent is an unallocated export must verify clean: the export's
// sidecar ({offset, length, image_offset}) is the export's own run map, not a list of evidence.Run.
func TestVerifyCleanWhenRecoveredArtifactHasUnallocatedExportParent(t *testing.T) {
	e := newRecEnv(t, delNode("/gone.txt", pat(5, 3*bs)), plain([]fstest.Node{{Path: "/keep.txt", Data: pat(9, 2*bs)}})[0])
	if _, err := e.s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1}); err != nil {
		t.Fatalf("ExportUnallocated: %v", err)
	}
	recs := mustManifest(t, e.c)
	var bin evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == "unallocated" {
			bin = r
		}
	}
	if bin.ID == "" || bin.Size < 2*bs {
		t.Fatalf("no usable unallocated export: %+v", bin)
	}
	data, err := os.ReadFile(filepath.Join(e.c.Dir, filepath.FromSlash(bin.Path)))
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.AddRecovered(t, e.c, bin, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-signature", Scope: "artifact",
		Path: "carved/raw/c1.bin", Runs: []evidence.Run{{Offset: 0, Length: 1024}},
	})
	evidencetest.AddRecovered(t, e.c, bin, data, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000001-a.bin", Runs: []evidence.Run{{Offset: 0, Length: 1024}},
	})
	rep, err := e.c.VerifyWith(context.Background(), examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("verify problems: %q", rep.Problems)
	}
}

// DerivedRuns of an unallocated export reads its sidecar as the export's own run map and returns the image
// runs. Read as plain evidence.Run lines the same bytes failed ("unknown field \"image_offset\"").
func TestDerivedRunsOfUnallocatedExport(t *testing.T) {
	e := newRecEnv(t, delNode("/gone.txt", pat(5, 3*bs)), plain([]fstest.Node{{Path: "/keep.txt", Data: pat(9, 2*bs)}})[0])
	if _, err := e.s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1}); err != nil {
		t.Fatal(err)
	}
	var bin evidence.ManifestRecord
	for _, r := range mustManifest(t, e.c) {
		if r.Source.Kind == "unallocated" {
			bin = r
		}
	}
	runs, err := e.c.DerivedRuns(bin, nil)
	if err != nil {
		t.Fatalf("DerivedRuns: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(e.c.Dir, filepath.FromSlash(bin.Path)))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, r := range runs {
		got = append(got, e.img[r.Offset:r.Offset+r.Length]...)
	}
	if len(runs) == 0 || !bytes.Equal(got, data) {
		t.Errorf("%d runs reproduce %d bytes, want the export's %d bytes", len(runs), len(got), len(data))
	}
}
