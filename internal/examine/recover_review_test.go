package examine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
)

// Tests added after the Task 9 review (C56): behaviour the first tests left unpinned.

// artifactCreate returns the details of the artifact.create audit entry of a manifest record.
func artifactCreate(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) map[string]any {
	t.Helper()
	for _, e := range auditByAction(t, c, "artifact.create") {
		if e.Details["id"] == rec.ID {
			return e.Details
		}
	}
	t.Fatalf("no artifact.create entry for %s", rec.ID)
	return nil
}

// I1: the text that names a cut is part of the hash-chained record (manifest and audit).
func TestRecoverCutErrorTextAndRecoveryFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T) *recEnv
		wantError string
		wantAs    []string
		excluded  int64
	}{
		{
			"allocated since deletion", func(t *testing.T) *recEnv {
				nodes := []fstest.Node{delNode("/a-del.bin", pat(1, 4*bs)), {Path: "/b-live.bin", Data: pat(2, 2*bs)}}
				lay := layout(t, nodes)
				nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 6 * bs, Runs: []filesys.Run{lay["/a-del.bin"][0], lay["/b-live.bin"][0]}}}
				return newRecEnv(t, nodes...)
			}, "1 of 2 runs not captured: allocated since deletion",
			[]string{"prefix-only", "cut-state=allocated"},
			2 * bs,
		},
		{
			"allocation state unknown", func(t *testing.T) *recEnv {
				nodes := []fstest.Node{delNode("/a-del.bin", pat(1, 2*bs)), {Path: "/b-live.bin", Data: pat(2, 2*bs)}}
				lay := layout(t, nodes)
				nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 3 * bs, Runs: []filesys.Run{blocks(lay["/a-del.bin"], 0, 2), blocks(lay["/b-live.bin"], 0, 1)}}}
				e, _ := newHookEnv(t, func(h *fsHook) { h.warnOnUnal = []string{"bitmap group 3 skipped"} }, nodes...)
				return e
			}, "1 of 2 runs not captured: allocation state unknown",
			[]string{"prefix-only", "cut-state=unknown"},
			bs,
		},
		{"declared size larger than the map", func(t *testing.T) *recEnv {
			nodes := []fstest.Node{delNode("/a-del.bin", pat(1, 2*bs))}
			lay := layout(t, nodes)
			nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 3 * bs, Runs: lay["/a-del.bin"]}}
			return newRecEnv(t, nodes...)
		}, fmt.Sprintf("the map covers %d of %d declared bytes", 2*bs, 3*bs), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.setup(t)
			e.recover(t, examine.RecoverOptions{All: true})
			recs := recovered(t, e.c)
			if len(recs) != 1 {
				t.Fatalf("%d artifacts", len(recs))
			}
			rec := recs[0]
			if !rec.Incomplete || rec.Error != tc.wantError {
				t.Errorf("incomplete=%v error=%q, want %q", rec.Incomplete, rec.Error, tc.wantError)
			}
			if got := artifactCreate(t, e.c, rec)["error"]; got != tc.wantError {
				t.Errorf("audit error = %v, want %q", got, tc.wantError)
			}
			rv := rec.Source.Derived.Recovery
			for _, a := range tc.wantAs {
				if !slices.Contains(rv.Assumptions, a) {
					t.Errorf("assumptions %q lack %q", rv.Assumptions, a)
				}
			}
			if !anyContains(rv.Assumptions, "size-declared=") {
				t.Errorf("assumptions %q lack size-declared", rv.Assumptions)
			}
			if rv.Alloc.ExcludedBytes != tc.excluded || (tc.excluded > 0 && (len(rv.Excluded) != 1 || rv.Excluded[0].Length != tc.excluded)) {
				t.Errorf("alloc %+v excluded %v, want %d excluded bytes", rv.Alloc, rv.Excluded, tc.excluded)
			}
			verifyOK(t, e.c)
		})
	}
}

// failingImage reads like inner until armed; then a read that reaches failOff returns the bytes before
// it and an error (eof: a clean end of data instead).
type failingImage struct {
	image.Image
	armed   bool
	failOff int64
	eof     bool
}

func (f *failingImage) ReadAt(p []byte, off int64) (int, error) {
	if !f.armed || off+int64(len(p)) <= f.failOff {
		return f.Image.ReadAt(p, off)
	}
	n := 0
	if off < f.failOff {
		var err error
		if n, err = f.Image.ReadAt(p[:f.failOff-off], off); err != nil {
			return n, err
		}
	}
	if f.eof {
		return n, io.EOF
	}
	return n, fmt.Errorf("injected: %w", image.ErrChunkCorrupt)
}

// I2: a read that fails or ends mid-copy keeps the bytes before it and narrows the provenance to them.
func TestRecoverReadFailureNarrowsProvenance(t *testing.T) {
	for _, mode := range []struct {
		name string
		eof  bool
		text string
	}{{"read error", false, "injected"}, {"image ends", true, "image ended after"}} {
		t.Run(mode.name, func(t *testing.T) {
			data := pat(1, 64*bs)
			e := newRecEnv(t, delNode("/big.bin", data))
			fi := &failingImage{Image: e.s.Image, eof: mode.eof}
			start := partStart + e.layout["/big.bin"][0].Offset
			const keep = 16*bs + 100
			fi.failOff = start + keep
			e.s.Image = fi
			sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, Progress: func(_, _ int64) { fi.armed = true }})
			if err != nil {
				t.Fatal(err)
			}
			recs := recovered(t, e.c)
			if len(recs) != 1 || sum.Partial != 1 || sum.Recovered != 1 {
				t.Fatalf("%d artifacts, summary %+v", len(recs), sum)
			}
			rec := recs[0]
			if !rec.Incomplete || rec.Size != keep || !strings.Contains(rec.Error, mode.text) {
				t.Errorf("incomplete=%v size=%d error=%q, want %d bytes and %q", rec.Incomplete, rec.Size, rec.Error, keep, mode.text)
			}
			rv := rec.Source.Derived.Recovery
			if rv.Alloc.Free != keep || !slices.Contains(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", keep)) {
				t.Errorf("alloc %+v assumptions %q, want free=%d and read-failed-after", rv.Alloc, rv.Assumptions, keep)
			}
			art := readArtifact(t, e.c, rec)
			if !bytes.Equal(art, data[:keep]) || !bytes.Equal(readImageRuns(e.img, rec.Source.Derived.Runs), art) {
				t.Error("kept bytes or their runs are wrong")
			}
			if !anyContains(warnReasons(t, e.c), "read failed after") {
				t.Errorf("warnings = %q", warnReasons(t, e.c))
			}
			verifyOK(t, e.c)
		})
	}
}

// I3: the free-space check is given exactly the admitted bytes.
func TestRecoverFreeSpaceCheckUsesAdmittedBytes(t *testing.T) {
	const admitted = 2*bs + 3*bs
	for _, tc := range []struct {
		free int64
		ok   bool
	}{{examine.FreeSpaceReserve + admitted - 1, false}, {examine.FreeSpaceReserve + admitted, true}} {
		e := newRecEnv(t, delNode("/a.bin", pat(1, 2*bs)), delNode("/b.bin", pat(2, 3*bs)))
		e.s.SetFreeBytes(func(string) (int64, error) { return tc.free, nil })
		_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, examine.ErrInsufficientSpace)) {
			t.Errorf("free %d: err = %v, want ok=%v", tc.free, err, tc.ok)
		}
	}
}

// I4: unallocated runs the planner had to ignore are an audited warning (not a skip) and a plan FSWarning.
func TestRecoverUnallocatedNoteIsAuditedNotCounted(t *testing.T) {
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.unalloc = func(in []filesys.Run) []filesys.Run { return append(in, filesys.Run{Offset: 1 << 40, Length: 4096}) }
	}, delNode("/a.bin", pat(1, 2*bs)))
	plan, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !anyContains(plan.FSWarnings, "ignored 1 unallocated run(s)") {
		t.Errorf("plan FSWarnings = %q", plan.FSWarnings)
	}
	sum := e.recover(t, examine.RecoverOptions{All: true})
	var found bool
	for _, en := range auditByAction(t, e.c, "analysis.warning") {
		if r, _ := en.Details["reason"].(string); strings.Contains(r, "ignored 1 unallocated run(s)") {
			found = true
		}
	}
	if !found || sum.Skipped != 0 || sum.Recovered != 1 {
		t.Errorf("note audited=%v skipped=%d recovered=%d, want audited, not counted", found, sum.Skipped, sum.Recovered)
	}
}

// I5 and I6: every counter of analysis.end equals the summary, and Files and Bytes count complete artifacts only.
func TestRecoverAnalysisEndCountersEqualSummary(t *testing.T) {
	nodes := []fstest.Node{
		delNode("/a.bin", pat(1, 2*bs)), delNode("/b.bin", pat(2, 2*bs)),
		delNode("/z.bin", make([]byte, bs)), delNode("/cut.bin", pat(3, 4*bs)),
		{Path: "/live.bin", Data: pat(4, 2*bs)},
	}
	lay := layout(t, nodes)
	nodes[1].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: lay["/a.bin"]}} // overlaps a
	nodes[3].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 6 * bs, Runs: []filesys.Run{lay["/cut.bin"][0], lay["/live.bin"][0]}}}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.Overlap != 2 || sum.Uniform != 1 || sum.Partial != 1 || sum.Recovered != 3 {
		t.Fatalf("summary %+v, want overlap 2, uniform 1, partial 1, recovered 3", sum)
	}
	if sum.Files != 2 || sum.Bytes != 4*bs {
		t.Errorf("files %d bytes %d, want the 2 complete artifacts and 4 blocks (the cut one is not counted)", sum.Files, sum.Bytes)
	}
	ends := auditByAction(t, e.c, "analysis.end")
	if len(ends) != 1 {
		t.Fatalf("%d analysis.end entries", len(ends))
	}
	d := ends[0].Details
	for k, want := range map[string]any{
		"files": sum.Files, "bytes": sum.Bytes, "considered": sum.Considered, "candidates": sum.Candidates,
		"recovered": sum.Recovered, "partial": sum.Partial, "uniform": sum.Uniform, "overlap": sum.Overlap,
		"limit_reached": sum.LimitReached, "not_processed": sum.NotProcessed,
	} {
		if fmt.Sprint(d[k]) != fmt.Sprint(want) {
			t.Errorf("analysis.end %s = %v, want %v", k, d[k], want)
		}
	}
}

// m1: a name holding a separator is one flat file name; it never creates a directory.
func TestRecoverNameWithSeparatorStaysFlat(t *testing.T) {
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs))}
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.rewriteKids = func(kids []filesys.Entry) []filesys.Entry {
			for i := range kids {
				switch kids[i].Name {
				case "a.bin":
					kids[i].Name = "x/y.bin"
				case "b.bin":
					kids[i].Name = `p\q.bin`
				}
			}
			return kids
		}
	}, nodes...)
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d artifacts", len(recs))
	}
	want := path.Dir(recs[0].Path)
	for _, r := range recs {
		if path.Dir(r.Path) != want || !strings.HasSuffix(want, "recovered/p1-mtfs") {
			t.Errorf("%q: directory %q, want every artifact directly in %q", r.Path, path.Dir(r.Path), want)
		}
	}
	verifyOK(t, e.c)
}

// m2: a retry after the main name was taken never leaves an orphan runs sidecar.
func TestRecoverNameRetryLeavesNoOrphanSidecar(t *testing.T) {
	const n = evidence.MaxInlineRuns + 4
	buildBS = 512
	t.Cleanup(func() { buildBS = bs })
	node := fstest.Node{Path: "/many.bin", Data: pat(1, n*512), Deleted: true, Freed: true, Fragments: n, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
	e := newRecEnv(t, node)
	failed := 0
	e.s.SetNewArtifact(func(dev, acq, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
		if src.Kind == evidence.KindRecover && failed == 0 {
			failed++
			return nil, fmt.Errorf("%w: %s", evidence.ErrArtifactExists, rel)
		}
		return e.c.NewArtifact(dev, acq, rel, src)
	})
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if failed != 1 || sum.Recovered != 1 {
		t.Fatalf("failed %d, recovered %d", failed, sum.Recovered)
	}
	sidecars := 0
	recs, err := e.c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Source.Kind == "runs" {
			sidecars++
		}
	}
	if sidecars != 1 {
		t.Errorf("%d runs sidecars in the case, want exactly the one the artifact names", sidecars)
	}
	verifyOK(t, e.c)
}

// m5: walk errors are listed one by one up to a cap, then counted in one note.
func TestRecoverWalkErrorsAreCapped(t *testing.T) {
	defer examine.SetWalkSkipCap(3)()
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs))}
	for i := range 6 {
		nodes = append(nodes, fstest.Node{Path: fmt.Sprintf("/d%d", i), Dir: true})
	}
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			if strings.HasPrefix(dir.Name, "d") {
				return fmt.Errorf("injected: %w", filesys.ErrCorrupt)
			}
			return nil
		}
	}, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["corrupt"] != 6 || sum.Recovered != 1 {
		t.Fatalf("summary %+v, want 6 corrupt skips counted and the file recovered", sum)
	}
	corrupt := 0
	for _, r := range warnReasons(t, e.c) {
		if strings.HasPrefix(r, "corrupt") {
			corrupt++
		}
	}
	if corrupt != 4 || !anyContains(warnReasons(t, e.c), "3 further") {
		t.Errorf("%d corrupt warnings %q, want 3 listed and one note for 3 further", corrupt, warnReasons(t, e.c))
	}
}

// m4: boundaries the spec states.
func TestRecoverNameAttemptsAreBounded(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs)))
	calls := 0
	e.s.SetNewArtifact(func(_, _, rel string, _ evidence.Source) (*evidence.ArtifactWriter, error) {
		calls++
		return nil, fmt.Errorf("%w: %s", evidence.ErrArtifactExists, rel)
	})
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if calls != 16 || sum.Recovered != 0 || !anyContains(warnReasons(t, e.c), "no free local name after 16 attempts") {
		t.Errorf("%d attempts, summary %+v, warnings %q", calls, sum, warnReasons(t, e.c))
	}
}

func TestRecoverSizeOneMapWithoutRunsIsNoRuns(t *testing.T) {
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.recoverable = func(_ filesys.Recoverer, _ filesys.Entry) ([]filesys.Candidate, error) {
			return []filesys.Candidate{{Method: mtfsRecM, Size: 1}}, nil
		}
	}, delNode("/a.bin", pat(1, bs)))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["no-runs"] != 1 || sum.Recovered != 0 {
		t.Errorf("summary %+v, want a size-1 map without runs refused as no-runs", sum)
	}
}

func TestRecoverMinConfidenceBoundaryIsInclusive(t *testing.T) {
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs))} // fat-contiguous: confidence 55
	for _, tc := range []struct {
		min  int
		want int
	}{{55, 1}, {56, 0}} {
		e := newRecEnv(t, nodes...)
		sum := e.recover(t, examine.RecoverOptions{All: true, MinConfidence: tc.min})
		if sum.Recovered != tc.want {
			t.Errorf("min %d: recovered %d, want %d", tc.min, sum.Recovered, tc.want)
		}
	}
}

func TestRecoverInlineRunsBoundary(t *testing.T) {
	buildBS = 512
	t.Cleanup(func() { buildBS = bs })
	for _, tc := range []struct {
		n       int
		sidecar bool
	}{{evidence.MaxInlineRuns, false}, {evidence.MaxInlineRuns + 1, true}} {
		node := fstest.Node{Path: "/many.bin", Data: pat(1, tc.n*512), Deleted: true, Freed: true, Fragments: tc.n, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
		e := newRecEnv(t, node)
		sum := e.recover(t, examine.RecoverOptions{All: true})
		if got := len(sum.Artifacts) == 2; got != tc.sidecar {
			t.Errorf("%d runs: sidecar written = %v, want %v", tc.n, got, tc.sidecar)
		}
	}
}
