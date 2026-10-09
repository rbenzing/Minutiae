package examine_test

import (
	"bytes"
	"context"
	"errors"
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

// A cancelled export keeps a partial unallocated.bin whose sidecar still lists every run: the reader accepts
// it, and the image bytes at the runs hold the bytes actually written as an exact prefix.
func TestDerivedRunsOfPartialUnallocatedExport(t *testing.T) {
	e := newRecEnv(t, delNode("/gone.txt", pat(5, 3*bs)), plain([]fstest.Node{{Path: "/keep.txt", Data: pat(9, 2*bs)}})[0])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.s.ExportUnallocated(ctx, examine.UnallocOptions{Partition: -1, Progress: func(d, _ int64) {
		if d > 0 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	var bin evidence.ManifestRecord
	for _, r := range mustManifest(t, e.c) {
		if r.Source.Kind == "unallocated" {
			bin = r
		}
	}
	if bin.ID == "" || !bin.Incomplete {
		t.Fatalf("no incomplete export: %+v", bin)
	}
	runs, err := e.c.DerivedRuns(bin, nil)
	if err != nil {
		t.Fatalf("DerivedRuns of a partial export: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(e.c.Dir, filepath.FromSlash(bin.Path)))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	var total int64
	for _, r := range runs {
		got = append(got, e.img[r.Offset:r.Offset+r.Length]...)
		total += r.Length
	}
	if len(data) == 0 || total < int64(len(data)) || !bytes.HasPrefix(got, data) {
		t.Errorf("runs cover %d bytes, export holds %d: image bytes are not an exact superset prefix", total, len(data))
	}
}
