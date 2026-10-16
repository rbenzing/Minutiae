package ios_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/iostest"
)

// scriptedAFC serves files whose Stat size may differ from their content and,
// when block is set, whose List and Read hang until the client is closed.
type scriptedAFC struct {
	files  map[string][]byte
	sizes  map[string]int64
	names  []string          // List returns these when set
	other  map[string]string // path -> unsupported st_ifmt
	forced *ios.FileStat     // Stat returns this for every path when set
	stats  []string          // every path Stat was called with
	opened []string          // every path Open was called with
	block  bool
	closed chan struct{}
	once   sync.Once
}

func newScriptedAFC() *scriptedAFC {
	return &scriptedAFC{files: map[string][]byte{}, sizes: map[string]int64{}, closed: make(chan struct{})}
}

func (a *scriptedAFC) List(string) ([]string, error) {
	if a.block {
		<-a.closed
		return nil, errors.New("afc connection closed")
	}
	if a.names != nil {
		return a.names, nil
	}
	return []string{".", ".."}, nil
}

func (a *scriptedAFC) Stat(p string) (ios.FileStat, error) {
	a.stats = append(a.stats, p)
	if a.forced != nil {
		return *a.forced, nil
	}
	if o, ok := a.other[p]; ok {
		return ios.FileStat{Size: int64(len(a.files[p])), Type: o, Unsupported: true}, nil
	}
	d, ok := a.files[p]
	if !ok {
		return ios.FileStat{}, fs.ErrNotExist
	}
	if n, ok := a.sizes[p]; ok {
		return ios.FileStat{Size: n, IsRegular: true}, nil
	}
	return ios.FileStat{Size: int64(len(d)), IsRegular: true}, nil
}

func (a *scriptedAFC) Open(p string) (io.ReadCloser, error) {
	a.opened = append(a.opened, p)
	d, ok := a.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if a.block {
		return io.NopCloser(blockingReader{a.closed}), nil
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func (a *scriptedAFC) Close() error {
	a.once.Do(func() { close(a.closed) })
	return nil
}

type blockingReader struct{ closed chan struct{} }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("afc connection closed")
}

type afcBackend struct {
	*iostest.Backend
	afc ios.AFC
}

func (b afcBackend) OpenAFC(context.Context, string) (ios.AFC, error) { return b.afc, nil }

func afcDevice(t *testing.T, a ios.AFC) device.FileTransferer {
	t.Helper()
	return first(t, afcBackend{Backend: fakeIPhone(), afc: a}).(device.FileTransferer)
}

func TestPullShortReadIsFlaggedIncomplete(t *testing.T) {
	a := newScriptedAFC()
	a.files["/DCIM/x.jpg"] = []byte("abcd")
	a.sizes["/DCIM/x.jpg"] = 10 // the device said 10 bytes but delivered 4
	ft := afcDevice(t, a)
	c := newCase(t)
	rec, err := device.PullToCase(context.Background(), c, ft, "U1", "acq", "/DCIM/x.jpg")
	if err == nil || !strings.Contains(err.Error(), "4 of 10 bytes") {
		t.Fatalf("err = %v", err)
	}
	if !rec.Incomplete || rec.Size != 4 {
		t.Fatalf("record = %+v", rec)
	}
}

func cancelledSoon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// within runs f and fails the test if it has not returned after 3 s.
func within(t *testing.T, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- f() }()
	select {
	case err := <-done:
		if el := time.Since(start); el > time.Second {
			t.Errorf("returned after %v", el)
		}
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("still blocked 3s after a 100ms deadline")
		return nil
	}
}

func TestPullCancelClosesAFC(t *testing.T) {
	a := newScriptedAFC()
	a.files["/DCIM/x.jpg"] = []byte("abcd")
	a.block = true
	ft := afcDevice(t, a)
	ctx := cancelledSoon(t)
	err := within(t, func() error {
		_, err := ft.Pull(ctx, "/DCIM/x.jpg", io.Discard)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestListCancelClosesAFC(t *testing.T) {
	a := newScriptedAFC()
	a.block = true
	ft := afcDevice(t, a)
	ctx := cancelledSoon(t)
	err := within(t, func() error {
		_, err := ft.List(ctx, "/DCIM")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// endlessAFC claims a 10-byte file and streams zeros forever.
type endlessAFC struct{}

func (endlessAFC) List(string) ([]string, error) { return nil, nil }
func (endlessAFC) Stat(string) (ios.FileStat, error) {
	return ios.FileStat{Size: 10, IsRegular: true}, nil
}
func (endlessAFC) Open(string) (io.ReadCloser, error) { return io.NopCloser(zeroReader{}), nil }
func (endlessAFC) Close() error                       { return nil }

type endlessBackend struct{ *iostest.Backend }

func (endlessBackend) OpenAFC(context.Context, string) (ios.AFC, error) { return endlessAFC{}, nil }

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func TestPullStopsAnEndlessStreamAtTheDeclaredSize(t *testing.T) {
	be := iostest.NewBackend()
	be.Entries = []ios.Entry{{UDID: "U1", ConnectionType: "USB"}}
	devs, err := ios.Enumerator{B: endlessBackend{be}}.List(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("List = %v, %v", devs, err)
	}
	var w countWriter
	done := make(chan error, 1)
	go func() {
		_, err := devs[0].(device.FileTransferer).Pull(context.Background(), "/x", &w)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "copied 11 of 10 bytes") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pull never stopped an endless stream")
	}
	if w.n != 11 {
		t.Fatalf("wrote %d bytes, want size+1 = 11", w.n)
	}
}

// A size of MaxInt64 (accepted by Stat) must not wrap the copy limit negative
// and end the pull before the first byte.
type hugeAFC struct{ endlessAFC }

func (hugeAFC) Stat(string) (ios.FileStat, error) {
	return ios.FileStat{Size: math.MaxInt64, IsRegular: true}, nil
}

type hugeBackend struct{ *iostest.Backend }

func (hugeBackend) OpenAFC(context.Context, string) (ios.AFC, error) { return hugeAFC{}, nil }

func TestPullWithAMaxInt64SizeStillCopies(t *testing.T) {
	be := iostest.NewBackend()
	be.Entries = []ios.Entry{{UDID: "U1", ConnectionType: "USB"}}
	devs, err := ios.Enumerator{B: hugeBackend{be}}.List(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatal(devs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelAfter{limit: 1 << 20, cancel: cancel}
	_, _ = devs[0].(device.FileTransferer).Pull(ctx, "/x", w)
	if w.n < 1<<20 {
		t.Fatalf("only %d bytes copied before the stream was cut: the limit wrapped", w.n)
	}
}

// cancelAfter counts bytes and cancels its context after limit.
type cancelAfter struct {
	n, limit int64
	cancel   func()
}

func (c *cancelAfter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.n >= c.limit {
		c.cancel()
	}
	return len(p), nil
}

// W51: an entry of an unknown type is listed as irregular and is not pulled
// as a regular file: the artifact exists, is flagged incomplete with the
// cause, empty, and the device is never asked to open it.
func TestPullOfAnUnsupportedTypeIsFlaggedAndAudited(t *testing.T) {
	a := newScriptedAFC()
	a.files["/DCIM/s"] = []byte("sock")
	a.other = map[string]string{"/DCIM/s": "S_IFSOCK"}
	ft := afcDevice(t, a)
	c := newCase(t)
	rec, err := device.PullToCase(context.Background(), c, ft, "U1", "acq", "/DCIM/s")
	if !errors.Is(err, device.ErrUnsupported) || !strings.Contains(err.Error(), `"S_IFSOCK"`) {
		t.Fatalf("err = %v", err)
	}
	if !rec.Incomplete || rec.Size != 0 || !strings.Contains(rec.Error, "S_IFSOCK") {
		t.Fatalf("record = %+v", rec)
	}
	if len(a.opened) != 0 {
		t.Fatalf("Open called for %v", a.opened)
	}
}

func TestListReportsAnUnsupportedTypeAsIrregular(t *testing.T) {
	a := newScriptedAFC()
	a.names = []string{".", "..", "sock", "plain"}
	a.files["/D/sock"], a.files["/D/plain"] = []byte("s"), []byte("p")
	a.other = map[string]string{"/D/sock": "S_IFSOCK"}
	es, err := afcDevice(t, a).List(context.Background(), "/D")
	if err != nil || len(es) != 2 {
		t.Fatalf("List = %v, %v", es, err)
	}
	if es[0].Name != "sock" || es[0].Mode&fs.ModeIrregular == 0 || es[0].IsDir {
		t.Errorf("sock entry = %+v, want an irregular mode", es[0])
	}
	if es[1].Name != "plain" || es[1].Mode&fs.ModeIrregular != 0 {
		t.Errorf("plain entry = %+v", es[1])
	}
}

// W50: names and paths are device evidence. A listing returns them as sent and
// builds each entry's path from the directory as the caller gave it (no
// cleaning), and a pull records the raw device path while the host path is
// sanitised.
func TestListKeepsRawNamesAndPaths(t *testing.T) {
	a := newScriptedAFC()
	raw := "e\x01f\n g:h"
	a.names = []string{".", "..", raw}
	dir := "/a/../b//"
	a.files["/a/../b//"+raw] = []byte("x")
	es, err := afcDevice(t, a).List(context.Background(), dir)
	if err != nil || len(es) != 1 {
		t.Fatalf("List = %v, %v", es, err)
	}
	if es[0].Name != raw || es[0].Path != dir+raw {
		t.Errorf("entry = %q %q, want the raw name and %q", es[0].Name, es[0].Path, dir+raw)
	}
	if len(a.stats) != 1 || a.stats[0] != dir+raw {
		t.Errorf("Stat was asked for %q, want %q", a.stats, dir+raw)
	}
}

func TestPullRecordsTheRawDevicePathAndSanitisesTheHostPath(t *testing.T) {
	a := newScriptedAFC()
	p := "/DCIM/e\x01f\ng:h"
	a.files[p] = []byte("data")
	c := newCase(t)
	rec, err := device.PullToCase(context.Background(), c, afcDevice(t, a), "U1", "acq", p)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Source.RemotePath != p {
		t.Errorf("RemotePath = %q, want the raw %q", rec.Source.RemotePath, p)
	}
	for _, r := range rec.Path {
		if r < 0x20 || r == 0x7f || r == ':' {
			t.Errorf("host path %q holds %q", rec.Path, r)
		}
	}
}

// W60/W62: pull reads an entry only when the device positively reported a
// regular file; directories, links, unknown types and a Backend that sets no
// type at all are refused before Open, and the artifact is an empty incomplete
// record with the cause.
func TestPullRefusesWhatIsNotPositivelyARegularFile(t *testing.T) {
	cases := map[string]ios.FileStat{
		"directory": {IsDir: true, Type: "S_IFDIR"},
		"link":      {IsLink: true, Size: 4, Type: "S_IFLNK"},
		"unknown":   {Size: 4, Type: "S_IFSOCK", Unsupported: true},
		"default":   {Size: 4},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			a := newScriptedAFC()
			a.files["/x"] = []byte("data")
			a.forced = &st
			c := newCase(t)
			rec, err := device.PullToCase(context.Background(), c, afcDevice(t, a), "U1", "acq", "/x")
			if !errors.Is(err, device.ErrUnsupported) {
				t.Fatalf("err = %v, want device.ErrUnsupported", err)
			}
			if st.Type != "" && !strings.Contains(err.Error(), st.Type) {
				t.Errorf("err %q does not name the type %q", err, st.Type)
			}
			if !rec.Incomplete || rec.Size != 0 || rec.Error == "" {
				t.Errorf("record = %+v", rec)
			}
			if len(a.opened) != 0 {
				t.Errorf("Open called for %v", a.opened)
			}
		})
	}
}

func TestListRefusesAnEmptyDirectoryPath(t *testing.T) {
	a := newScriptedAFC()
	a.names = []string{".", "..", "n"}
	a.files["/n"] = []byte("x")
	es, err := afcDevice(t, a).List(context.Background(), "")
	if !errors.Is(err, ios.ErrEmptyPath) || len(es) != 0 {
		t.Fatalf("List = %v, %v; want ErrEmptyPath and no entry", es, err)
	}
	if len(a.stats) != 0 {
		t.Fatalf("Stat called for %v", a.stats)
	}
}

func TestPullOfADotDotPathIsRefusedWithoutAnArtifact(t *testing.T) {
	a := newScriptedAFC()
	a.files["/DCIM/../x"] = []byte("x")
	c := newCase(t)
	rec, err := device.PullToCase(context.Background(), c, afcDevice(t, a), "U1", "acq", "/DCIM/../x")
	if err == nil || rec.ID != "" {
		t.Fatalf("rec = %+v, err = %v; want a refusal and no artifact", rec, err)
	}
	if len(a.stats) != 0 || len(a.opened) != 0 {
		t.Fatalf("the device was asked: stat %v open %v", a.stats, a.opened)
	}
}
