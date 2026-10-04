package image

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251 + 1)
	}
	return b
}

func TestOpenRaw(t *testing.T) {
	dir := t.TempDir()
	data := pattern(1000)
	p := writeFile(t, dir, "disk.dd", data)
	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Size() != 1000 || img.Format() != "raw" || img.SectorSize() != 512 {
		t.Fatalf("size=%d format=%q sector=%d", img.Size(), img.Format(), img.SectorSize())
	}
	buf := make([]byte, 20)
	n, err := img.ReadAt(buf, 990)
	if n != 10 || err != io.EOF {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf[:10], data[990:]) {
		t.Fatal("content mismatch")
	}
	n, err = img.ReadAt(buf, 1000)
	if n != 0 || err != io.EOF {
		t.Fatalf("at end: n=%d err=%v", n, err)
	}
	n, err = img.ReadAt(buf, 5000)
	if n != 0 || err != io.EOF {
		t.Fatalf("past end: n=%d err=%v", n, err)
	}
	// a read that exactly fits is complete
	n, err = img.ReadAt(buf, 980)
	if n != 20 || (err != nil && err != io.EOF) {
		t.Fatalf("exact: n=%d err=%v", n, err)
	}
	if n, err = img.ReadAt(nil, 10); n != 0 || err != nil {
		t.Fatalf("empty read: n=%d err=%v", n, err)
	}
	md := img.Metadata()
	if len(md) != 2 || md[0] != (KV{"segments", "1"}) || md[1] != (KV{"segment.1", "disk.dd (1000 bytes)"}) {
		t.Fatalf("metadata: %+v", md)
	}
}

func TestOpenSplitRawCrossesSegments(t *testing.T) {
	dir := t.TempDir()
	all := pattern(1124)
	paths := []string{
		writeFile(t, dir, "d.001", all[:512]),
		writeFile(t, dir, "d.002", all[512:1024]),
		writeFile(t, dir, "d.003", all[1024:]),
	}
	img, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Size() != 1124 || img.Format() != "split-raw" {
		t.Fatalf("size=%d format=%q", img.Size(), img.Format())
	}
	buf := make([]byte, 600)
	n, err := img.ReadAt(buf, 300)
	if n != 600 || (err != nil && err != io.EOF) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, all[300:900]) {
		t.Fatal("content mismatch across segments")
	}
	// every window across boundaries and the end
	for off := 0; off < 1124; off += 37 {
		b := make([]byte, 200)
		n, err := img.ReadAt(b, int64(off))
		want := min(200, 1124-off)
		if n != want {
			t.Fatalf("off %d n=%d want %d", off, n, want)
		}
		if n < len(b) && err == nil {
			t.Fatalf("off %d short read without error", off)
		}
		if !bytes.Equal(b[:n], all[off:off+n]) {
			t.Fatalf("off %d mismatch", off)
		}
	}
	// read starting exactly at a boundary
	b := make([]byte, 4)
	if n, err := img.ReadAt(b, 512); n != 4 || err != nil || !bytes.Equal(b, all[512:516]) {
		t.Fatalf("boundary n=%d err=%v", n, err)
	}
	md := img.Metadata()
	if len(md) != 4 || md[0] != (KV{"segments", "3"}) || md[3].Key != "segment.3" {
		t.Fatalf("metadata: %+v", md)
	}
}

func TestOpenDetectsEx01Unsupported(t *testing.T) {
	p := writeFile(t, t.TempDir(), "a.Ex01", append([]byte("EVF2\x0d\x0a\x81\x00"), make([]byte, 100)...))
	_, err := Open([]string{p})
	if !errors.Is(err, ErrUnsupportedContainer) || !strings.Contains(err.Error(), "EWF2 (Ex01)") {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenDetectsL01Unsupported(t *testing.T) {
	p := writeFile(t, t.TempDir(), "a.L01", append([]byte("LVF\x09\x0d\x0a\xff\x00"), make([]byte, 100)...))
	_, err := Open([]string{p})
	if !errors.Is(err, ErrUnsupportedContainer) || !strings.Contains(err.Error(), "logical evidence file (L01)") {
		t.Fatalf("err=%v", err)
	}
}

type stubImage struct {
	Image
	files []*os.File
}

func (s *stubImage) Format() string { return "ewf" }
func (s *stubImage) Close() error {
	for _, f := range s.files {
		_ = f.Close()
	}
	return nil
}

func TestOpenRegisteredEWF(t *testing.T) {
	var got int
	RegisterEWF(func(files []*os.File) (Image, error) {
		got = len(files)
		return &stubImage{files: files}, nil
	})
	t.Cleanup(func() { RegisterEWF(nil) })
	dir := t.TempDir()
	sig := []byte("EVF\x09\x0d\x0a\xff\x00")
	p1 := writeFile(t, dir, "a.E01", append(sig, 1, 2, 3))
	p2 := writeFile(t, dir, "a.E02", append(sig, 4, 5, 6))
	img, err := Open([]string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Format() != "ewf" || got != 2 {
		t.Fatalf("format=%q segments=%d", img.Format(), got)
	}
}

func TestOpenEWFOpenerErrorClosesFiles(t *testing.T) {
	RegisterEWF(func(_ []*os.File) (Image, error) { return nil, errors.New("boom") })
	t.Cleanup(func() { RegisterEWF(nil) })
	p := writeFile(t, t.TempDir(), "a.E01", append([]byte("EVF\x09\x0d\x0a\xff\x00"), 1))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFiles([]*os.File{f}); err == nil {
		t.Fatal("want error")
	}
	if _, err := f.Stat(); err == nil {
		t.Fatal("file should have been closed on failure")
	}
}

func TestOpenShortFileIsRaw(t *testing.T) {
	p := writeFile(t, t.TempDir(), "tiny", []byte("EVF"))
	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Format() != "raw" || img.Size() != 3 {
		t.Fatalf("format=%q size=%d", img.Format(), img.Size())
	}
}

func TestOpenEmptySingleFile(t *testing.T) {
	p := writeFile(t, t.TempDir(), "empty", nil)
	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Size() != 0 {
		t.Fatalf("size=%d", img.Size())
	}
	if n, err := img.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOpenNegativeOffset(t *testing.T) {
	p := writeFile(t, t.TempDir(), "d", pattern(100))
	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if n, err := img.ReadAt(make([]byte, 4), -1); err == nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestReadAtOffsetOverflow(t *testing.T) {
	p := writeFile(t, t.TempDir(), "d", pattern(100))
	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	n, err := img.ReadAt(make([]byte, 64), math.MaxInt64-10)
	if n != 0 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOpenNoSegments(t *testing.T) {
	if _, err := Open(nil); !errors.Is(err, ErrNoSegments) {
		t.Fatalf("err=%v", err)
	}
	if _, err := OpenFiles(nil); !errors.Is(err, ErrNoSegments) {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenEmptySplitSegment(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		writeFile(t, dir, "d.001", pattern(512)),
		writeFile(t, dir, "d.002", nil),
		writeFile(t, dir, "d.003", pattern(10)),
	}
	_, err := Open(paths)
	if err == nil || !strings.Contains(err.Error(), "empty segment 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := Open([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("want error")
	}
}

func TestOpenMissingSecondFileClosesFirst(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "d.001", pattern(10))
	if _, err := Open([]string{p, filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("want error")
	}
	// the first file must not be left open (Windows refuses to remove it)
	if err := os.Remove(p); err != nil {
		t.Fatalf("segment still held open: %v", err)
	}
}

func TestOpenIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	data := pattern(2000)
	p := writeFile(t, dir, "d.dd", data)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)

	img, err := Open([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.ReadAt(make([]byte, 2000), 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	// the image exposes no write path
	if _, ok := img.(io.WriterAt); ok {
		t.Fatal("image must not implement io.WriterAt")
	}
	if _, ok := img.(io.Writer); ok {
		t.Fatal("image must not implement io.Writer")
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("file changed: %v -> %v", before, after)
	}
	got, _ := os.ReadFile(p)
	if sha256.Sum256(got) != sum {
		t.Fatal("content hash changed")
	}

	// OpenFiles with an O_RDONLY file: the OS rejects writes on the handle.
	f, err := os.OpenFile(p, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatal("write on read-only handle succeeded")
	}
	img2, err := OpenFiles([]*os.File{f})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img2.ReadAt(make([]byte, 10), 0); err != nil {
		t.Fatal(err)
	}
	if err := img2.Close(); err != nil {
		t.Fatal(err)
	}
	// OpenFiles took ownership: Close closed the file.
	if _, err := f.Stat(); err == nil {
		t.Fatal("Close did not close the underlying file")
	}
	got, _ = os.ReadFile(p)
	if sha256.Sum256(got) != sum {
		t.Fatal("content hash changed after OpenFiles")
	}
}

func openStubFiles(t *testing.T) []*os.File {
	t.Helper()
	p := writeFile(t, t.TempDir(), "x.E01", append(append([]byte{}, sigEWF...), pattern(64)...))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return []*os.File{f}
}

func TestOpenFilesRejectsOpenerReturningNilImage(t *testing.T) {
	RegisterEWF(func([]*os.File) (Image, error) { return nil, nil })
	t.Cleanup(func() { RegisterEWF(nil) })
	files := openStubFiles(t)
	img, err := OpenFiles(files)
	if err == nil || img != nil {
		t.Fatalf("OpenFiles = %v, %v; want an error and no image", img, err)
	}
	if _, rerr := files[0].ReadAt(make([]byte, 1), 0); rerr == nil || !errors.Is(rerr, os.ErrClosed) {
		t.Errorf("file not closed after a nil image: read err = %v", rerr)
	}
}

func TestOpenFilesRecoversOpenerPanic(t *testing.T) {
	RegisterEWF(func([]*os.File) (Image, error) { panic("boom") })
	t.Cleanup(func() { RegisterEWF(nil) })
	files := openStubFiles(t)
	var img Image
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("OpenFiles let the opener panic escape: %v", p)
			}
		}()
		img, err = OpenFiles(files)
	}()
	if err == nil || img != nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("OpenFiles = %v, %v; want an error mentioning the panic", img, err)
	}
	if pe := new(PanicError); !errors.As(err, &pe) || pe.Value != "boom" {
		t.Errorf("err = %v, want a *PanicError carrying the panic value", err)
	}
	if _, rerr := files[0].ReadAt(make([]byte, 1), 0); rerr == nil || !errors.Is(rerr, os.ErrClosed) {
		t.Errorf("file not closed after an opener panic: read err = %v", rerr)
	}
}
