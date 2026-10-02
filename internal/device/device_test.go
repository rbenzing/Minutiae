package device_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

type fakeDevice struct {
	id     string
	files  map[string]string
	pushed map[string]string
	onPush func()
	block  chan struct{} // if non-nil, Pull writes "part" then waits for ctx
}

func (f *fakeDevice) ID() string        { return f.id }
func (f *fakeDevice) Kind() device.Kind { return device.KindAndroid }
func (f *fakeDevice) Info(context.Context) (device.Info, error) {
	return device.Info{ID: f.id, Kind: device.KindAndroid}, nil
}

func (f *fakeDevice) List(context.Context, string) ([]device.FileEntry, error) { return nil, nil }

func (f *fakeDevice) Pull(ctx context.Context, p string, w io.Writer) (int64, error) {
	if f.block != nil {
		n, _ := io.WriteString(w, "part")
		<-ctx.Done()
		return int64(n), ctx.Err()
	}
	s, ok := f.files[p]
	if !ok {
		return 0, device.ErrNotFound
	}
	n, err := io.WriteString(w, s)
	return int64(n), err
}

func (f *fakeDevice) Push(_ context.Context, r io.Reader, p string, _ fs.FileMode) error {
	if f.onPush != nil {
		f.onPush()
	}
	b, err := io.ReadAll(r)
	f.pushed[p] = string(b)
	return err
}

func (f *fakeDevice) Partitions(context.Context) ([]device.Partition, error) {
	return []device.Partition{{Name: "boot", Path: "/dev/block/boot", Size: 6}}, nil
}

func (f *fakeDevice) Image(_ context.Context, _ string, w io.Writer, progress device.ProgressFunc) (int64, error) {
	n, err := device.WriteCounter(w, 6, progress).Write([]byte("BOOTIM"))
	return int64(n), err
}

type fakeEnum struct {
	devs []device.Device
	err  error
}

func (e fakeEnum) Kind() device.Kind                             { return device.KindAndroid }
func (e fakeEnum) List(context.Context) ([]device.Device, error) { return e.devs, e.err }

func newCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func lastActions(t *testing.T, c *evidence.Case, n int) []string {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es[len(es)-n:] {
		out = append(out, e.Action)
	}
	return out
}

func TestRegistryListAndFind(t *testing.T) {
	d := &fakeDevice{id: "A1"}
	r := device.NewRegistry(fakeEnum{devs: []device.Device{d}}, fakeEnum{err: errors.New("adb down")})
	devs, errs := r.List(context.Background(), "")
	if len(devs) != 1 || len(errs) != 1 {
		t.Fatalf("devs=%v errs=%v", devs, errs)
	}
	if got, err := r.Find(context.Background(), device.KindAndroid, "A1"); err != nil || got.ID() != "A1" {
		t.Fatalf("find: %v %v", got, err)
	}
	if _, err := r.Find(context.Background(), device.KindAndroid, "nope"); !errors.Is(err, device.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if devs, _ := r.List(context.Background(), device.KindIOS); len(devs) != 0 {
		t.Fatalf("kind filter failed: %v", devs)
	}
}

func TestPullToCase(t *testing.T) {
	c := newCase(t)
	d := &fakeDevice{id: "A1", files: map[string]string{"/sdcard/x:y.txt": "hello"}}
	var rec evidence.ManifestRecord
	err := device.RunAcquisition(c, "A1", "pull", nil, func(acq string) error {
		var err error
		rec, err = device.PullToCase(context.Background(), c, d, "A1", acq, "/sdcard/x:y.txt")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rec.Path, "/files/sdcard/x_y.txt") || rec.Source.RemotePath != "/sdcard/x:y.txt" || rec.Size != 5 {
		t.Fatalf("rec = %+v", rec)
	}
	got := lastActions(t, c, 3)
	if got[0] != "acquire.start" || got[1] != "artifact.create" || got[2] != "acquire.end" {
		t.Fatalf("actions = %v", got)
	}
}

func TestRunAcquisitionRecordsError(t *testing.T) {
	c := newCase(t)
	boom := errors.New("boom")
	if err := device.RunAcquisition(c, "A1", "x", nil, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := lastActions(t, c, 1); got[0] != "acquire.error" {
		t.Fatalf("actions = %v", got)
	}
}

func TestPullToCaseCancelledKeepsPartial(t *testing.T) {
	c := newCase(t)
	d := &fakeDevice{id: "A1", block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec, err := device.PullToCase(ctx, c, d, "A1", "acq", "/big")
	if !errors.Is(err, context.Canceled) || !rec.Incomplete || rec.Size != 4 {
		t.Fatalf("rec=%+v err=%v", rec, err)
	}
}

func TestImageToCaseReportsProgress(t *testing.T) {
	c := newCase(t)
	var last int64
	rec, err := device.ImageToCase(context.Background(), c, &fakeDevice{id: "A1"}, "A1", "acq", "boot",
		func(done, _ int64) { last = done })
	if err != nil || !strings.HasSuffix(rec.Path, "/partitions/boot.img") || last != 6 || rec.Source.Partition != "boot" {
		t.Fatalf("rec=%+v err=%v last=%d", rec, err, last)
	}
}

func TestPushAuditedRequiresPermission(t *testing.T) {
	c := newCase(t)
	d := &fakeDevice{id: "A1", pushed: map[string]string{}}
	local := filepath.Join(t.TempDir(), "tool")
	_ = os.WriteFile(local, []byte("bin"), 0o600)
	err := device.PushAudited(context.Background(), c, d, "A1", local, "/data/local/tmp/tool", false)
	if !errors.Is(err, device.ErrDeviceWriteNotAllowed) || len(d.pushed) != 0 {
		t.Fatalf("err=%v pushed=%v", err, d.pushed)
	}
}

func TestPushAuditedAuditsBeforeWrite(t *testing.T) {
	c := newCase(t)
	auditPath := filepath.Join(c.Dir, "audit.jsonl")
	var auditedFirst bool
	d := &fakeDevice{id: "A1", pushed: map[string]string{}, onPush: func() {
		b, _ := os.ReadFile(auditPath)
		auditedFirst = bytes.Contains(b, []byte(`"action":"device.modify"`))
	}}
	local := filepath.Join(t.TempDir(), "tool")
	_ = os.WriteFile(local, []byte("bin"), 0o600)
	if err := device.PushAudited(context.Background(), c, d, "A1", local, "/data/local/tmp/tool", true); err != nil {
		t.Fatal(err)
	}
	if !auditedFirst || d.pushed["/data/local/tmp/tool"] != "bin" {
		t.Fatalf("auditedFirst=%v pushed=%v", auditedFirst, d.pushed)
	}
}
