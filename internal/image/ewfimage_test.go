package image

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// writeE01 writes the segments to dir as a.E01, a.E02 ... and returns the paths.
func writeE01(t *testing.T, dir string, segs [][]byte) []string {
	t.Helper()
	paths := make([]string, len(segs))
	for i, s := range segs {
		paths[i] = writeFile(t, dir, "a.E0"+string(rune('1'+i)), s)
	}
	return paths
}

func TestOpenEWFSegmentsFromFiles(t *testing.T) {
	const cs = 64 * 512
	media := pattern(5*cs + 4096)
	for _, bps := range []int{512, 4096} {
		files := ewftest.Build(ewftest.Options{BytesPerSector: bps, SectorsPerChunk: cs / bps, ChunksPerSegment: 2, Compress: ewftest.CompressMixed}, media)
		if len(files) != 3 {
			t.Fatalf("builder made %d segments", len(files))
		}
		img, err := Open(writeE01(t, t.TempDir(), files))
		if err != nil {
			t.Fatalf("bps %d: %v", bps, err)
		}
		if img.Format() != "ewf" || img.Size() != int64(len(media)) || img.SectorSize() != bps {
			t.Fatalf("bps %d: format %q size %d sector size %d", bps, img.Format(), img.Size(), img.SectorSize())
		}
		got := make([]byte, len(media))
		if n, err := img.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, media) {
			t.Fatalf("bps %d: ReadAt = %d, %v", bps, n, err)
		}
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegisterEWFNilRestoresBuiltIn(t *testing.T) {
	stub := false
	RegisterEWF(func(files []*os.File) (Image, error) { stub = true; return &stubImage{files: files}, nil })
	media := pattern(2 * 64 * 512)
	paths := writeE01(t, t.TempDir(), ewftest.Build(ewftest.Options{}, media))
	img, err := Open(paths)
	if err != nil || !stub {
		t.Fatalf("registered opener not used: %v, stub=%v", err, stub)
	}
	_ = img.Close()

	RegisterEWF(nil)
	stub = false
	img, err = Open(paths)
	if err != nil || stub {
		t.Fatalf("built-in opener not restored: %v, stub=%v", err, stub)
	}
	defer func() { _ = img.Close() }()
	if _, ok := img.(*ewfImage); !ok || img.Size() != int64(len(media)) {
		t.Fatalf("got %T size %d", img, img.Size())
	}
}

func TestOpenEWFGarbageIsCorrupt(t *testing.T) {
	p := writeFile(t, t.TempDir(), "a.E01", append(append([]byte{}, sigEWF...), make([]byte, 100)...))
	img, err := Open([]string{p})
	if img != nil || !errors.Is(err, ErrCorruptContainer) {
		t.Fatalf("img=%v err=%v; want ErrCorruptContainer", img, err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("file still held open after the failure: %v", err)
	}
}

func TestOpenEWFCorruptClosesFiles(t *testing.T) {
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 2}, pattern(5*64*512))
	files[0][13+50] ^= 1 // padding byte of the first descriptor: only its checksum notices
	dir := t.TempDir()
	paths := writeE01(t, dir, files)
	img, err := Open(paths)
	if img != nil || !errors.Is(err, ErrCorruptContainer) {
		t.Fatalf("img=%v err=%v; want ErrCorruptContainer", img, err)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Fatalf("segment still held open after the failure: %v", err)
		}
	}
	// Also through OpenFiles with an unreadable non-regular "segment".
	d := filepath.Join(dir, "sub")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	good := writeE01(t, t.TempDir(), ewftest.Build(ewftest.Options{}, pattern(2*64*512)))
	f1, err := os.Open(good[0])
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if img, err := openEWF([]*os.File{f1, f2}); err == nil || img != nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("openEWF with a directory: %v, %v", img, err)
	}
	_ = f1.Close()
	_ = f2.Close()
}

func TestEWFImageMetadataAndWarnings(t *testing.T) {
	media := pattern(3 * 64 * 512)
	segs := ewftest.Build(ewftest.Options{Case: "C-7", Examiner: "Ex", ErrorRanges: [][2]uint32{{10, 5}}}, media)

	// The expectation comes straight from the ewf reader on the same bytes.
	var es []ewf.Segment
	for i, s := range segs {
		es = append(es, ewf.Segment{Name: "a.E0" + string(rune('1'+i)), R: bytes.NewReader(s), Size: int64(len(s))})
	}
	r, err := ewf.Open(es)
	if err != nil {
		t.Fatal(err)
	}
	var want []KV
	for _, kv := range r.Metadata() {
		want = append(want, KV{Key: kv.Key, Value: kv.Value})
	}

	img, err := Open(writeE01(t, t.TempDir(), segs))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if got := img.Metadata(); len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("Metadata = %v\nwant       %v", got, want)
	}
	has := func(k, v string) bool { return slices.Contains(img.Metadata(), KV{Key: k, Value: v}) }
	if !has("case_number", "C-7") || !has("examiner", "Ex") {
		t.Fatalf("header fields missing: %v", img.Metadata())
	}
	w, ok := img.(Warner)
	if !ok {
		t.Fatal("the EWF image must be a Warner")
	}
	if ws := w.Warnings(); len(ws) != 1 || !strings.Contains(ws[0], "unreadable at acquisition") {
		t.Fatalf("Warnings = %q", ws)
	}
	if _, ok := img.(Verifier); !ok {
		t.Fatal("the EWF image must be a Verifier")
	}

	raw, err := Open([]string{writeFile(t, t.TempDir(), "d.dd", pattern(2048))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, ok := raw.(Warner); ok {
		t.Error("a raw image must not be a Warner")
	}
	if _, ok := raw.(Verifier); ok {
		t.Error("a raw image must not be a Verifier")
	}
}

func TestEWFImageCloseClosesFilesAndIsIdempotent(t *testing.T) {
	paths := writeE01(t, t.TempDir(), ewftest.Build(ewftest.Options{ChunksPerSegment: 1}, pattern(2*64*512)))
	img, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	e := img.(*ewfImage)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range e.files {
		if _, err := f.Stat(); err == nil {
			t.Fatalf("%s still open after Close", f.Name())
		}
	}
	if _, err := img.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("ReadAt after Close must fail")
	}
	_ = img.Close() // a second Close must not panic
}
