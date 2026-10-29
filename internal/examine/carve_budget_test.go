package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// failingImage fails reads at and after failAt while armed (the copy of a hit, not the scan).
type failingImage struct {
	image.Image
	armed  bool
	failAt int64
}

func (f *failingImage) ReadAt(p []byte, off int64) (int, error) {
	if !f.armed || off+int64(len(p)) <= f.failAt {
		return f.Image.ReadAt(p, off)
	}
	if off >= f.failAt {
		return 0, errors.New("injected read failure")
	}
	n, _ := f.Image.ReadAt(p[:f.failAt-off], off)
	return n, errors.New("injected read failure")
}

// AcquisitionGaps passes the gaps of the wrapped image on, so the writer sees the declared gaps.
func (f *failingImage) AcquisitionGaps() image.AcquisitionGaps {
	if gr, ok := f.Image.(image.GapReporter); ok {
		return gr.AcquisitionGaps()
	}
	return image.AcquisitionGaps{}
}

// armScan arms the failing image around each emitted hit, so only the copy of a hit fails, after failAfter bytes.
func armScan(f *failingImage, failAfter int64) (restore func()) {
	return SetCarveScan(func(ctx context.Context, r io.ReaderAt, runs []carve.Run, set *carve.Set, o carve.Options, emit func(carve.Hit) error) (carve.Stats, error) {
		return carve.Scan(ctx, r, runs, set, o, func(h carve.Hit) error {
			f.armed, f.failAt = true, h.Offset+failAfter
			defer func() { f.armed = false }()
			return emit(h)
		})
	})
}

func TestCarveReadFailureKeepsPartial(t *testing.T) {
	t.Run("raw", func(t *testing.T) {
		nodes := carveStdNodes(carveJPEG(), carvePNG())
		e := newCarveEnv(t, nodes...)
		f := &failingImage{Image: e.s.Image}
		e.s.Image = f
		defer armScan(f, 40)()
		off := carveImageOff(t, "/del1.jpg", nodes...)
		cs := e.carve(t, CarveOptions{})
		if cs.Carved != 2 || cs.Partial != 2 || cs.Files != 0 {
			t.Fatalf("summary carved %d partial %d files %d", cs.Carved, cs.Partial, cs.Files)
		}
		r := carvedRecords(t, e.c)[0]
		d, rv := r.Source.Derived, r.Source.Derived.Recovery
		if !r.Incomplete || r.Error == "" || r.Size != 40 {
			t.Errorf("incomplete %v error %q size %d", r.Incomplete, r.Error, r.Size)
		}
		if len(d.Runs) != 1 || d.Runs[0] != (evidence.Run{Offset: off, Length: 40}) {
			t.Errorf("runs %+v, want the copied prefix", d.Runs)
		}
		if *rv.Confidence != 20 || !slices.Contains(rv.Assumptions, "read-failed-after=40") || rv.Alloc != (evidence.AllocSummary{Free: 40}) {
			t.Errorf("recovery %+v", rv)
		}
		if got := carveArtifactBytes(t, e.c, r); len(got) != 40 {
			t.Errorf("%d bytes kept", len(got))
		}
		f.armed = false
		rep := carveVerify(t, e)
		for _, p := range rep.Problems {
			t.Errorf("verify problem: %s", p)
		}
	})

	t.Run("e01", func(t *testing.T) {
		jpg := carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 3000})
		nodes := []fstest.Node{carveDeleted("/del.jpg", jpg)}
		off := carveImageOff(t, "/del.jpg", nodes...)
		const failAfter = 2000
		// two declared gaps inside the object: sector 2 lies inside the kept prefix, sector 5 beyond it
		ranges := [][2]uint32{{uint32(off/512) + 2, 1}, {uint32(off/512) + 5, 1}}
		data := carveDisk(carveFS(nodes...))
		for _, r := range ranges {
			clear(data[int64(r[0])*512 : int64(r[0]+r[1])*512])
		}
		segs := ewftest.Build(ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: (len(data)/4096 + 2) / 3, Compress: ewftest.CompressNone, ErrorRanges: ranges}, data)
		dir := t.TempDir()
		paths := make([]string, len(segs))
		for i, s := range segs {
			paths[i] = filepath.Join(dir, fmt.Sprintf("disk.E0%d", i+1))
			if err := os.WriteFile(paths[i], s, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c := carveNewCase(t)
		recs, err := Import(context.Background(), c, "img", paths, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts, hook := carveOpts(nil)
		s, err := Open(c, recs[0].ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		e := &carveEnv{c: c, s: s, img: data, rec: recs[0], hook: hook}
		f := &failingImage{Image: s.Image}
		s.Image = f
		defer armScan(f, failAfter)()
		cs := e.carve(t, CarveOptions{})
		if cs.Carved != 1 || cs.Partial != 1 {
			t.Fatalf("carved %d partial %d, skipped %v", cs.Carved, cs.Partial, cs.SkippedBy)
		}
		r := carvedRecords(t, c)[0]
		d, rv := r.Source.Derived, r.Source.Derived.Recovery
		prefix := []evidence.Run{{Offset: off, Length: failAfter}}
		want := gapNoteFor(f.AcquisitionGaps(), prefix, true, failAfter)
		if want == nil || want.Ranges != 1 || want.Bytes != 512 {
			t.Fatalf("test setup: prefix note %+v", want)
		}
		if d.AcquisitionGaps == nil || fmt.Sprintf("%+v", *d.AcquisitionGaps) != fmt.Sprintf("%+v", *want) {
			t.Errorf("note %+v, want the note over the kept prefix %+v", d.AcquisitionGaps, want)
		}
		if !slices.Contains(rv.Assumptions, carveGapTag) || !slices.Contains(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", failAfter)) {
			t.Errorf("assumptions %q", rv.Assumptions)
		}
	})
}

// carveStatsEnv has a one-block first scope run and three PNG objects in the last one, so the bytes scanned are
// known when a later hit stops the run.
func carveStatsEnv(t *testing.T) *carveEnv {
	t.Helper()
	nodes := []fstest.Node{
		{Path: "/f.bin", Data: carveFill(1, 4*carveBS), Fragments: 2}, // one free block between its two runs
	}
	base := newCarveEnv(t, nodes...)
	sc, err := base.s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Runs) < 2 || sc.Runs[len(sc.Runs)-1].Length < 3*carveBS {
		t.Fatalf("test setup: scope runs %v", sc.Runs)
	}
	last := sc.Runs[len(sc.Runs)-1]
	img := slices.Clone(base.img)
	for i := range 3 {
		copy(img[last.Offset+int64(i)*carveBS:], carvePNG())
	}
	return newCarveImageEnv(t, img, nil)
}

func requireEndKeys(t *testing.T, c *evidence.Case, action string) map[string]any {
	t.Helper()
	es := carveAuditEntries(t, c, action)
	if len(es) != 1 {
		t.Fatalf("%d %s entries", len(es), action)
	}
	for _, k := range carveEndKeys {
		if _, ok := es[0].Details[k]; !ok {
			t.Errorf("%s lacks %q", action, k)
		}
	}
	if v, _ := strconv.ParseFloat(fmt.Sprint(es[0].Details["scanned_bytes"]), 64); v <= 0 {
		t.Errorf("%s scanned_bytes = %v", action, es[0].Details["scanned_bytes"])
	}
	return es[0].Details
}

func TestCarveEndStatsOnCancelAndLimits(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		e := carveStatsEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer SetCarveScan(func(c context.Context, r io.ReaderAt, runs []carve.Run, set *carve.Set, o carve.Options, emit func(carve.Hit) error) (carve.Stats, error) {
			return carve.Scan(c, r, runs, set, o, func(h carve.Hit) error {
				err := emit(h)
				cancel()
				return err
			})
		})()
		cs, err := e.s.Carve(ctx, CarveOptions{Partition: -1})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if cs.Carved != 1 || len(carvedRecords(t, e.c)) != 1 {
			t.Errorf("carved %d, the first artifact must be kept", cs.Carved)
		}
		requireEndKeys(t, e.c, "analysis.error")
	})
	t.Run("max-files", func(t *testing.T) {
		e := carveStatsEnv(t)
		cs := e.carve(t, CarveOptions{MaxFiles: 1})
		d := requireEndKeys(t, e.c, "analysis.end")
		if d["limit_reached"] != "max-files" || cs.LimitReached != "max-files" {
			t.Errorf("limit_reached %v", d["limit_reached"])
		}
	})
	t.Run("validate-budget", func(t *testing.T) {
		e := carveStatsEnv(t)
		defer SetCarveValidateBudget(1)()
		cs := e.carve(t, CarveOptions{})
		_ = cs
		d := requireEndKeys(t, e.c, "analysis.end")
		if d["limit_reached"] != "validate-budget" {
			t.Errorf("limit_reached %v", d["limit_reached"])
		}
	})
}

func TestCarveValidateBudgetIsAudited(t *testing.T) {
	greedy := carvetest.JPEG(carvetest.JPEGOpts{OmitEOI: true, EntropyLen: 3000})
	var nodes []fstest.Node
	for i := range 6 {
		nodes = append(nodes,
			fstest.Node{Path: fmt.Sprintf("/l%d.bin", i), Data: carveFill(byte(i), carveBS)},
			carveDeleted(fmt.Sprintf("/g%d.jpg", i), greedy))
	}
	e := newCarveEnv(t, nodes...)
	defer SetCarveValidateBudget(4096)()
	cs := e.carve(t, CarveOptions{})
	if cs.LimitReached != "validate-budget" || !cs.Stats.BudgetExhausted || cs.Stats.Rejected["validate-budget"] == 0 {
		t.Fatalf("limit %q exhausted %v rejected %v", cs.LimitReached, cs.Stats.BudgetExhausted, cs.Stats.Rejected)
	}
	if n := countCode(t, e.c, "validate-budget"); n != 1 {
		t.Errorf("%d validate-budget warnings, want 1", n)
	}
	if cs.Stats.ValidateBytes > 4096+64<<10 {
		t.Errorf("validators read %d bytes", cs.Stats.ValidateBytes)
	}
}

func TestCarvePanicFailsRun(t *testing.T) {
	e := carveStatsEnv(t)
	defer SetCarveScan(func(c context.Context, r io.ReaderAt, runs []carve.Run, set *carve.Set, o carve.Options, emit func(carve.Hit) error) (carve.Stats, error) {
		stop := errors.New("stop")
		st, err := carve.Scan(c, r, runs, set, o, func(h carve.Hit) error {
			if err := emit(h); err != nil {
				return err
			}
			return stop
		})
		if !errors.Is(err, stop) {
			return st, err
		}
		return st, &carve.PanicError{Value: "boom"}
	})()
	cs, err := e.s.Carve(context.Background(), CarveOptions{Partition: -1})
	var pe *carve.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *carve.PanicError", err)
	}
	if cs.Carved != 1 || len(carvedRecords(t, e.c)) != 1 {
		t.Errorf("carved %d: the earlier artifact must be kept", cs.Carved)
	}
	d := requireEndKeys(t, e.c, "analysis.error")
	if s, _ := d["error"].(string); !strings.Contains(s, "boom") {
		t.Errorf("error %q", s)
	}
}
