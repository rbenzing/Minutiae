package ios_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
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
	return []string{".", ".."}, nil
}

func (a *scriptedAFC) Stat(p string) (ios.FileStat, error) {
	d, ok := a.files[p]
	if !ok {
		return ios.FileStat{}, fs.ErrNotExist
	}
	if n, ok := a.sizes[p]; ok {
		return ios.FileStat{Size: n}, nil
	}
	return ios.FileStat{Size: int64(len(d))}, nil
}

func (a *scriptedAFC) Open(p string) (io.ReadCloser, error) {
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
