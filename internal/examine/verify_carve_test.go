package examine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// Tests of case verify over carved artifacts (R5, R9, R10, R6). Helpers carry the verifyCarve prefix.

// verifyCarveWith verifies the case of e with filesystem readers altered by setup.
func verifyCarveWith(t *testing.T, e *carveEnv, setup func(*carveHook)) evidence.VerifyReport {
	t.Helper()
	opts, _ := carveOpts(setup)
	rep, err := e.c.VerifyWith(context.Background(), VerifyChecks(opts)...)
	if err != nil {
		t.Fatalf("VerifyWith: %v", err)
	}
	return rep
}

// verifyCarveForge adds a carve artifact on the image of e whose runs and bytes are the image's own, as a
// writer with a bug would.
func verifyCarveForge(t *testing.T, e *carveEnv, scope, dir string, runs []evidence.Run, alloc evidence.AllocSummary, mutate func(*evidencetest.RecoveredSpec)) evidence.ManifestRecord {
	t.Helper()
	conf := 70
	spec := evidencetest.RecoveredSpec{
		Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-validated", Confidence: &conf, Scope: scope,
		Path: "carved/" + dir + "/ffffffffffffffff-forged.bin", Partition: 1, PartitionOffset: int64(carveFirstLBA) * 512, FSType: "mtfs",
		Runs: runs, Alloc: &alloc,
	}
	if mutate != nil {
		mutate(&spec)
	}
	return evidencetest.AddRecovered(t, e.c, e.rec, e.img, spec)
}

func verifyCarveCount(ss []string, sub string) (n int) {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestAllocationRecheckCoversCarvedUnallocated(t *testing.T) {
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	e := newCarveEnv(t, nodes...)
	cs := e.carve(t, CarveOptions{})
	if cs.Carved != 2 {
		t.Fatalf("carved %d", cs.Carved)
	}
	rep := carveVerify(t, e)
	evidencetest.RequireClean(t, rep)
	if rep.AllocationRechecked != 2 {
		t.Errorf("AllocationRechecked = %d, want the 2 carves", rep.AllocationRechecked)
	}
	live := carveImageOff(t, "/a.bin", nodes...)
	verifyCarveForge(t, e, CarveUnallocated, "p1-mtfs", []evidence.Run{{Offset: live, Length: 512}}, evidence.AllocSummary{Free: 512}, nil)
	rep = carveVerify(t, e)
	evidencetest.RequireProblem(t, rep, "allocation: run 0 (")
	evidencetest.RequireProblem(t, rep, "lies outside the space the filesystem reports unallocated")
	if rep.AllocationRechecked != 2 {
		t.Errorf("AllocationRechecked = %d, the forgery must not be counted", rep.AllocationRechecked)
	}
}

func TestAllocationRecheckFlagsCarvedOutsideFree(t *testing.T) {
	nodes := carveStdNodes(carveJPEG(), carvePNG())
	e := newCarveEnv(t, nodes...)
	e.carve(t, CarveOptions{})
	off := carveImageOff(t, "/del1.jpg", nodes...)
	rep := verifyCarveWith(t, e, func(h *carveHook) {
		h.dropFree = &filesys.Run{Offset: off - int64(carveFirstLBA)*512, Length: carveBS}
	})
	evidencetest.RequireProblem(t, rep, "lies outside the space the filesystem reports unallocated")
	if rep.CancelledOnly() || len(rep.Problems) == 0 {
		t.Error("a carve whose bytes are allocated now is an integrity problem (exit 4)")
	}
}

func TestAllocationRecheckCarvedRawRecomputesAlloc(t *testing.T) {
	nodes := []fstest.Node{
		{Path: "/live.jpg", Data: carvePad(carveJPEG())},
		carveDeleted("/del.png", carvePNG()),
	}
	e := newCarveEnv(t, nodes...)
	if cs := e.carve(t, CarveOptions{Scope: CarveRaw}); cs.Carved != 2 {
		t.Fatalf("carved %d", cs.Carved)
	}
	rep := carveVerify(t, e)
	evidencetest.RequireClean(t, rep)
	if rep.AllocationRechecked != 2 {
		t.Errorf("AllocationRechecked = %d", rep.AllocationRechecked)
	}
	live := carveImageOff(t, "/live.jpg", nodes...)
	run := []evidence.Run{{Offset: live, Length: 512}}
	verifyCarveForge(t, e, CarveRaw, "raw", run, evidence.AllocSummary{Free: 512}, nil)
	verifyCarveForge(t, e, CarveRaw, "raw", run, evidence.AllocSummary{Allocated: 512}, func(s *evidencetest.RecoveredSpec) {
		s.Partition, s.Path = 2, "carved/raw/eeeeeeeeeeeeeeee-forged.bin"
	})
	rep = carveVerify(t, e)
	evidencetest.RequireProblem(t, rep, "recorded alloc free 512 allocated 0 unknown 0, recomputed 0 512 0")
	evidencetest.RequireProblem(t, rep, `recorded region partition 2 at 20480 ("mtfs"), recomputed partition 1 at 20480 ("mtfs")`)
}

// verifyCarveGapEnv is an image with a PNG in the first table gap that holds it.
func verifyCarveGapEnv(t *testing.T) *carveEnv {
	t.Helper()
	base := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	png := carvePNG()
	var at int64 = -1
	for _, u := range base.s.Table.Unallocated {
		if u.Length >= int64(len(png)) && u.Offset%512 == 0 {
			at = u.Offset
			break
		}
	}
	if at < 0 {
		t.Fatalf("test setup: no table gap in %+v", base.s.Table.Unallocated)
	}
	img := slices.Clone(base.img)
	copy(img[at:], png)
	return newCarveImageEnv(t, img, nil)
}

func TestAllocationRecheckCarvedVolume(t *testing.T) {
	e := verifyCarveGapEnv(t)
	cs := e.carve(t, CarveOptions{Scope: CarveVolume})
	if cs.Carved != 1 {
		t.Fatalf("carved %d", cs.Carved)
	}
	rep := carveVerify(t, e)
	evidencetest.RequireClean(t, rep)
	if rep.AllocationRechecked != 1 {
		t.Errorf("AllocationRechecked = %d", rep.AllocationRechecked)
	}
	verifyCarveForge(t, e, CarveVolume, "volume", []evidence.Run{{Offset: int64(carveFirstLBA) * 512, Length: 512}}, evidence.AllocSummary{Free: 512}, nil)
	rep = carveVerify(t, e)
	evidencetest.RequireProblem(t, rep, "lies outside the volume's unallocated gaps")
}

func verifyCarveE01(t *testing.T, ranges []int64) (*carveEnv, int64, *failingImage) {
	t.Helper()
	return carveE01Env(t, carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 3000}), ranges)
}

func TestGapRecomputeCoversCarve(t *testing.T) {
	e, off, _ := verifyCarveE01(t, []int64{1024})
	e.carve(t, CarveOptions{})
	if carvedRecords(t, e.c)[0].Source.Derived.AcquisitionGaps == nil {
		t.Fatal("test setup: the carve has no gap note")
	}
	rep := carveVerify(t, e)
	evidencetest.RequireClean(t, rep)
	if rep.GapNotesRecomputed < 1 {
		t.Errorf("GapNotesRecomputed = %d", rep.GapNotesRecomputed)
	}
	for name, mutate := range map[string]func(*evidence.ManifestRecord){
		"rewritten": func(r *evidence.ManifestRecord) {
			r.Source.Derived.AcquisitionGaps = &evidence.AcquisitionGapNote{Bytes: 1024, Ranges: 2, First: []evidence.Run{{Offset: off, Length: 1024}}}
		},
		"stripped": func(r *evidence.ManifestRecord) { r.Source.Derived.AcquisitionGaps = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e2, _, _ := verifyCarveE01(t, []int64{1024})
			e2.carve(t, CarveOptions{})
			evidencetest.RewriteManifest(t, e2.c.Dir, carvedRecords(t, e2.c)[0].ID, mutate)
			evidencetest.RequireProblem(t, carveVerify(t, e2), "acquisition gaps: the artifact records")
		})
	}
}

func TestGapRecomputeCleanOnPartialCarve(t *testing.T) {
	const failAfter = 2000
	e, off, f := verifyCarveE01(t, []int64{1024, 2560})
	defer armScan(f, failAfter)()
	if cs := e.carve(t, CarveOptions{}); cs.Partial != 1 {
		t.Fatalf("partial %d", cs.Partial)
	}
	rec := carvedRecords(t, e.c)[0]
	if !rec.Incomplete || rec.Source.Derived.AcquisitionGaps == nil || rec.Source.Derived.AcquisitionGaps.Ranges != 1 {
		t.Fatalf("test setup: %+v", rec.Source.Derived.AcquisitionGaps)
	}
	rep := carveVerify(t, e)
	if verifyCarveCount(rep.Problems, "acquisition gaps") != 0 {
		t.Fatalf("R10 problems on a partial carve:\n%s", strings.Join(rep.Problems, "\n"))
	}
	if rep.GapNotesRecomputed != 1 {
		t.Errorf("GapNotesRecomputed = %d", rep.GapNotesRecomputed)
	}
	full := int64(len(carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 3000})))
	planned := gapNoteFor(f.AcquisitionGaps(), []evidence.Run{{Offset: off, Length: full}}, true, full)
	if planned == nil || planned.Ranges != 2 {
		t.Fatalf("test setup: planned note %+v", planned)
	}
	evidencetest.RewriteManifest(t, e.c.Dir, rec.ID, func(r *evidence.ManifestRecord) { r.Source.Derived.AcquisitionGaps = planned })
	evidencetest.RequireProblem(t, carveVerify(t, e), "acquisition gaps: the artifact records")
}

// TestReproduceCarvedArtifacts (PASSES ON ARRIVAL): R6 already reproduces carves of every scope.
func TestReproduceCarvedArtifacts(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	e.carve(t, CarveOptions{})
	e.carve(t, CarveOptions{Scope: CarveRaw})
	un := carvedRecords(t, e.c)
	opts, _ := carveOpts(nil)
	s, err := OpenArtifactScope(e.c, un[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if cs, err := s.Carve(context.Background(), CarveOptions{Partition: -1}); err != nil || cs.Carved != 1 {
		t.Fatalf("artifact carve %d, %v", cs.Carved, err)
	}
	// an artifact that opens as no container as it is: it starts with the EWF signature (3E-R9), so reading
	// it as a container fails while reading it as raw bytes finds the PNG at its second sector
	ewf := make([]byte, 512, 512+len(carvePNG()))
	copy(ewf, "EVF\x09\x0d\x0a\xff\x00")
	ewf = append(ewf, carvePNG()...)
	disguised := evidencetest.AddImage(t, e.c, ewf)
	s2, err := OpenArtifactScope(e.c, disguised.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if cs, err := s2.Carve(context.Background(), CarveOptions{Partition: -1}); err != nil || cs.Carved != 1 {
		t.Fatalf("carve of the disguised artifact %d, %v", cs.Carved, err)
	}
	all := carvedRecords(t, e.c)
	rep := carveVerify(t, e)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredReproduced != len(all) {
		t.Fatalf("reproduced %d of %d", rep.RecoveredReproduced, len(all))
	}
	scopes := map[string][]string{} // scope -> the paths to flip
	for _, r := range all {
		sc := r.Source.Derived.Recovery.Scope
		scopes[sc] = append(scopes[sc], r.Path)
	}
	idx := map[string]int{CarveUnallocated: 3, CarveRaw: 3, CarveArtifact: 5}
	for _, sc := range []string{CarveUnallocated, CarveRaw, CarveArtifact} {
		if len(scopes[sc]) == 0 {
			t.Fatalf("no carve of scope %s", sc)
		}
		for _, rel := range scopes[sc] {
			p := filepath.Join(e.c.Dir, filepath.FromSlash(rel))
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b[idx[sc]] ^= 0xff // another byte for scope artifact: its parent is a flipped artifact, flipping the same one would cancel out
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep = carveVerify(t, e)
	if n := verifyCarveCount(rep.Problems, "of the artifact differs from image offset"); n < len(all) {
		t.Errorf("%d reproduce problems, want one per flipped artifact (%d):\n%s", n, len(all), strings.Join(rep.Problems, "\n"))
	}
}

func TestVerifyNoticesNameCarved(t *testing.T) {
	e := newCarveEnv(t, carveDeleted("/del.png", carvePNG()))
	e.carve(t, CarveOptions{})
	rep, err := e.c.VerifyWith(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNotice(t, rep, "1 deleted-file or carved artifact(s): allocation not rechecked against the filesystem (this verify did not compose check R9)")
	evidencetest.RequireNotice(t, rep, "1 extract, unallocated or carve artifact(s): acquisition-gap notes shape-checked only, not recomputed (this verify did not compose check R10)")
}
