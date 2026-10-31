package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func endDetails(t *testing.T, c *evidence.Case, action string) map[string]any {
	t.Helper()
	es := carveAuditEntries(t, c, action)
	if len(es) != 1 {
		t.Fatalf("%d %s entries", len(es), action)
	}
	return es[0].Details
}

func TestCarveScanLimitCut(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	const limit = carveBS
	if sc.Bytes <= limit {
		t.Fatalf("test setup: scope of %d bytes", sc.Bytes)
	}
	e.s.SetScanBudget(ScanBudget{Bytes: limit})
	pl, err := e.s.PlanCarve(context.Background(), CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	if pl.LimitReached != "scan-limit" || pl.NotScannedBytes < sc.Bytes-limit || !slices.ContainsFunc(pl.Notes, func(n string) bool { return strings.HasPrefix(n, "scan-limit:") }) {
		t.Errorf("plan: limit %q not scanned %d notes %q", pl.LimitReached, pl.NotScannedBytes, pl.Notes)
	}
	cs := e.carve(t, CarveOptions{})
	if cs.LimitReached != "scan-limit" || cs.NotScannedBytes < sc.Bytes-limit || cs.Stats.Scanned > limit {
		t.Errorf("run: limit %q not scanned %d scanned %d", cs.LimitReached, cs.NotScannedBytes, cs.Stats.Scanned)
	}
	if n := countCode(t, e.c, "scan-limit"); n != 1 {
		t.Errorf("%d scan-limit warnings, want 1", n)
	}
	if d := endDetails(t, e.c, "analysis.end"); d["limit_reached"] != "scan-limit" {
		t.Errorf("limit_reached %v", d["limit_reached"])
	}
}

func TestCarveMaxFileSize(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	prep := func(size int64) (*carveJob, error) {
		return e.s.prepareCarve(CarveOptions{Partition: -1, MaxFileSize: size})
	}
	builtin := map[string]carve.Type{}
	for _, ty := range carve.Builtin().Types {
		builtin[ty.Name] = ty
	}
	j, err := prep(1 << 40) // never raises
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range j.set.Types {
		if ty.MaxLen != builtin[ty.Name].MaxLen {
			t.Errorf("%s MaxLen %d, built-in %d", ty.Name, ty.MaxLen, builtin[ty.Name].MaxLen)
		}
	}
	if j, err = prep(150); err != nil {
		t.Fatal(err)
	}
	for _, ty := range j.set.Types {
		if ty.MaxLen > 150 {
			t.Errorf("%s MaxLen %d, want at most 150", ty.Name, ty.MaxLen)
		}
	}
	cs := e.carve(t, CarveOptions{MaxFileSize: 150}) // the 203-byte JPEG no longer fits its window, the PNG does
	recs := carvedRecords(t, e.c)
	if cs.Carved != 1 || len(recs) != 1 || recs[0].Source.Derived.Recovery.Basis[0] != "sig=png" || recs[0].Source.Derived.Recovery.Params["max_file_size"] != "150" {
		t.Fatalf("carved %d: %+v", cs.Carved, recs)
	}
	if j, err = prep(60); err != nil { // jpeg needs 64 bytes at least
		t.Fatal(err)
	}
	if !slices.ContainsFunc(j.notes, func(n string) bool { return strings.HasPrefix(n, "type jpeg is not carved") }) ||
		slices.ContainsFunc(j.set.Types, func(ty carve.Type) bool { return ty.Name == "jpeg" }) {
		t.Errorf("notes %q", j.notes)
	}
	if _, err = prep(1); !errors.Is(err, ErrInvalidCarveOption) {
		t.Errorf("no type left: %v", err)
	}
}

func TestCarveTypesCappedWarning(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	defer SetCarveScan(func(c context.Context, r io.ReaderAt, runs []carve.Run, set *carve.Set, o carve.Options, emit func(carve.Hit) error) (carve.Stats, error) {
		st, err := carve.Scan(c, r, runs, set, o, emit)
		st.TypeCapped = []string{"jpeg"}
		return st, err
	})()
	e.carve(t, CarveOptions{})
	if n := countCode(t, e.c, "types-capped"); n != 1 {
		t.Errorf("%d types-capped warnings, want 1", n)
	}
	if d := endDetails(t, e.c, "analysis.end"); fmt.Sprint(d["types_capped"]) != "[jpeg]" {
		t.Errorf("types_capped %v", d["types_capped"])
	}
}

// badRangeImage fails every read that touches [lo, hi).
type badRangeImage struct {
	image.Image
	lo, hi int64
}

func (b *badRangeImage) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) <= b.lo || off >= b.hi {
		return b.Image.ReadAt(p, off)
	}
	if off >= b.lo {
		return 0, errors.New("injected bad sector")
	}
	n, _ := b.Image.ReadAt(p[:b.lo-off], off)
	return n, errors.New("injected bad sector")
}

func TestCarveUnreadableChunkIsAudited(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	last := sc.Runs[len(sc.Runs)-1]
	end := last.Offset + last.Length
	e.s.Image = &badRangeImage{Image: e.s.Image, lo: end - 512, hi: end}
	cs := e.carve(t, CarveOptions{})
	if cs.Carved != 2 || cs.Stats.UnreadableBytes != 512 {
		t.Fatalf("carved %d unreadable %d", cs.Carved, cs.Stats.UnreadableBytes)
	}
	if n := countCode(t, e.c, "unreadable"); n != 1 {
		t.Errorf("%d unreadable warnings, want 1", n)
	}
	if d := endDetails(t, e.c, "analysis.end"); fmt.Sprint(d["unreadable_bytes"]) != "512" || fmt.Sprint(d["uncopied_bytes"]) != "0" {
		t.Errorf("end %v", d)
	}
}

// carveE01Env imports a GPT/MTFS disk holding one deleted JPEG as a three-segment E01 whose error2 section
// declares the given sector ranges (zero-filled, as an acquirer does). The session image is wrapped so a copy can fail.
func carveE01Env(t *testing.T, jpg []byte, rangesAt []int64) (*carveEnv, int64, *failingImage) {
	t.Helper()
	nodes := []fstest.Node{carveDeleted("/del.jpg", jpg)}
	off := carveImageOff(t, "/del.jpg", nodes...)
	data := carveDisk(carveFS(nodes...))
	var ranges [][2]uint32
	for _, at := range rangesAt {
		ranges = append(ranges, [2]uint32{uint32((off + at) / 512), 1})
		clear(data[off+at : off+at+512])
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
	f := &failingImage{Image: s.Image}
	s.Image = f
	return &carveEnv{c: c, s: s, img: data, rec: recs[0], hook: hook}, off, f
}

func TestCarveWholeHitCarriesGapNote(t *testing.T) {
	jpg := carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 3000, ZeroSafe: true})
	e, off, f := carveE01Env(t, jpg, []int64{1024})
	cs := e.carve(t, CarveOptions{})
	recs := carvedRecords(t, e.c)
	if cs.Carved != 1 || cs.Partial != 0 || len(recs) != 1 {
		t.Fatalf("carved %d partial %d", cs.Carved, cs.Partial)
	}
	d, rv := recs[0].Source.Derived, recs[0].Source.Derived.Recovery
	want := gapNoteFor(f.AcquisitionGaps(), []evidence.Run{{Offset: off, Length: recs[0].Size}}, true, recs[0].Size)
	if want == nil || want.Bytes != 512 || d.AcquisitionGaps == nil || fmt.Sprintf("%+v", *d.AcquisitionGaps) != fmt.Sprintf("%+v", *want) {
		t.Errorf("note %+v, want %+v", d.AcquisitionGaps, want)
	}
	if !slices.Contains(rv.Assumptions, carveGapTag) {
		t.Errorf("assumptions %q", rv.Assumptions)
	}
}

// A copy that fails exactly at the first byte of a declared gap keeps nothing of it (no note, no assumption);
// one that fails exactly after the gap's last byte keeps all of it.
func TestCarveE01FailureOnGapSectorBoundary(t *testing.T) {
	jpg := carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 3000, ZeroSafe: true})
	for _, tc := range []struct {
		failAfter int64
		bytes     int64
	}{{1024, 0}, {1536, 512}, {1025, 1}} {
		t.Run(fmt.Sprint(tc.failAfter), func(t *testing.T) {
			e, off, f := carveE01Env(t, jpg, []int64{1024})
			defer armScan(f, tc.failAfter)()
			cs := e.carve(t, CarveOptions{})
			if cs.Partial != 1 {
				t.Fatalf("partial %d", cs.Partial)
			}
			d, rv := carvedRecords(t, e.c)[0].Source.Derived, carvedRecords(t, e.c)[0].Source.Derived.Recovery
			want := gapNoteFor(f.AcquisitionGaps(), []evidence.Run{{Offset: off, Length: tc.failAfter}}, true, tc.failAfter)
			if tc.bytes == 0 {
				if want != nil || d.AcquisitionGaps != nil || slices.Contains(rv.Assumptions, carveGapTag) {
					t.Errorf("note %+v assumptions %q: nothing of the gap was kept", d.AcquisitionGaps, rv.Assumptions)
				}
				return
			}
			if want == nil || want.Bytes != tc.bytes || d.AcquisitionGaps == nil || d.AcquisitionGaps.Bytes != tc.bytes || !slices.Contains(rv.Assumptions, carveGapTag) {
				t.Errorf("note %+v want %+v assumptions %q", d.AcquisitionGaps, want, rv.Assumptions)
			}
		})
	}
}

func TestCarveFreeSpaceCheckedBeforeEachWrite(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	var calls int
	e.s.SetFreeBytes(func(string) (int64, error) {
		calls++
		if calls == 1 {
			return 1 << 40, nil
		}
		return 0, nil
	})
	cs, err := e.s.Carve(context.Background(), CarveOptions{Partition: -1})
	if !errors.Is(err, ErrInsufficientSpace) || calls != 2 || cs.Carved != 1 || len(carvedRecords(t, e.c)) != 1 {
		t.Fatalf("err %v calls %d carved %d", err, calls, cs.Carved)
	}
	if len(carveAuditEntries(t, e.c, "analysis.error")) != 1 {
		t.Error("want analysis.error")
	}
}

func TestCarveTypesParamDigestForLongList(t *testing.T) {
	short := &carveJob{types: []string{"jpeg", "png"}}
	if k, v := short.typesParam(); k != "types" || v != "jpeg,png" {
		t.Errorf("short: %s=%s", k, v)
	}
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("type-%05d", i))
	}
	k, v := (&carveJob{types: names}).typesParam()
	if k != "types_sha256" || v != sha256Hex([]byte(strings.Join(names, ","))) || len(v) != 64 {
		t.Errorf("long: %s=%s", k, v)
	}
}

func TestCarvePlanEqualsRunUnderLimit(t *testing.T) {
	nodes := append(carveStdNodes(carveJPEG(), carvePNG()), carveDeleted("/del3.png", carvePNG()))
	e := newCarveEnv(t, nodes...)
	o := CarveOptions{Partition: -1, MaxFiles: 2, MinConfidence: DefaultCarveMinConfidence}
	pl, err := e.s.PlanCarve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := e.s.Carve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var writes int
	for _, h := range pl.Hits {
		if h.Skip == "" {
			writes++
		}
	}
	if pl.LimitReached != "max-files" || pl.LimitReached != cs.LimitReached || pl.NotScannedBytes != cs.NotScannedBytes ||
		fmt.Sprint(pl.SkippedBy) != fmt.Sprint(cs.SkippedBy) || writes != 2 || cs.Carved != 2 {
		t.Errorf("plan: limit %q not scanned %d skipped %v writes %d; run: limit %q not scanned %d skipped %v carved %d",
			pl.LimitReached, pl.NotScannedBytes, pl.SkippedBy, writes, cs.LimitReached, cs.NotScannedBytes, cs.SkippedBy, cs.Carved)
	}
}

func TestCarveUncopiedBytes(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	f := &failingImage{Image: e.s.Image}
	e.s.Image = f
	defer armScan(f, 40)()
	cs := e.carve(t, CarveOptions{})
	want := int64(len(carveJPEG())-40) + int64(len(carvePNG())-40)
	if cs.UncopiedBytes != want || cs.Stats.UnreadableBytes != 0 {
		t.Errorf("uncopied %d (want %d), scanner unreadable %d", cs.UncopiedBytes, want, cs.Stats.UnreadableBytes)
	}
	if d := endDetails(t, e.c, "analysis.end"); fmt.Sprint(d["uncopied_bytes"]) != fmt.Sprint(want) {
		t.Errorf("uncopied_bytes %v", d["uncopied_bytes"])
	}
}

func TestCarveNameFailureIsCountedAndReleased(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	e.s.SetNewArtifact(func(string, string, string, evidence.Source) (*evidence.ArtifactWriter, error) {
		return nil, fmt.Errorf("create: %w", syscall.ENAMETOOLONG)
	})
	// the first failure must give its file back, or the second hit would be refused by max-files
	cs, err := e.s.Carve(context.Background(), CarveOptions{Partition: -1, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Carved != 0 || cs.LimitReached != "" || cs.SkippedBy[carveSkipNoName] != 2 || cs.SkippedBy["max-files"] != 0 {
		t.Errorf("carved %d limit %q skipped %v", cs.Carved, cs.LimitReached, cs.SkippedBy)
	}
}

// TestCarveMaxFileSizeNoteIsOncePerFamily: a family of several rows (the eight
// tiff rows) that --max-file-size puts below its minimum length is named once,
// not once per row.
func TestCarveMaxFileSizeNoteIsOncePerFamily(t *testing.T) {
	e := newCarveEnv(t, carveStdNodes(carveJPEG(), carvePNG())...)
	j, err := e.s.prepareCarve(CarveOptions{Partition: -1, MaxFileSize: 31}) // tiff needs 32 bytes at least
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range j.notes {
		if strings.HasPrefix(n, "type ") && strings.Contains(n, " is not carved") {
			seen[n]++
		}
	}
	tiff := "type tiff is not carved: --max-file-size 31 is below its minimum length 32"
	if seen[tiff] != 1 {
		t.Errorf("notes %q, want %q exactly once", j.notes, tiff)
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("note %q appears %d times", n, c)
		}
	}
	if slices.ContainsFunc(j.set.Types, func(ty carve.Type) bool { return ty.Name == "tiff" }) {
		t.Error("a tiff row is still in the set")
	}
}
