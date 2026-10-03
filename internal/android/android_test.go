package android

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

const getprop = "[ro.product.model]: [Pixel 7]\n[ro.product.manufacturer]: [Google]\n" +
	"[ro.build.version.release]: [15]\n[ro.serialno]: [PX1]\n[ro.multi]: [a]: [b]]\n"

func fakeServer(t *testing.T, devs ...*adbtest.Device) *adbtest.Server {
	t.Helper()
	s, err := adbtest.Start(devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func findDevice(t *testing.T, s *adbtest.Server, serial string) *Device {
	t.Helper()
	devs, err := Enumerator{C: adb.New(s.Addr())}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if d.ID() == serial {
			return d.(*Device)
		}
	}
	t.Fatalf("device %s not listed", serial)
	return nil
}

func basicDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device", Props: "model:Pixel_7",
		Commands: map[string][]byte{"getprop": []byte(getprop)},
		Files:    map[string]adbtest.File{"/sdcard/a.txt": {Data: []byte("hello")}},
	}
}

func TestParseGetprop(t *testing.T) {
	p := ParseGetprop([]byte(getprop))
	if p["ro.product.model"] != "Pixel 7" || p["ro.multi"] != "a]: [b]" || len(p) != 5 {
		t.Fatalf("props = %v", p)
	}
}

func TestInfo(t *testing.T) {
	d := findDevice(t, fakeServer(t, basicDevice()), "PX1")
	info, err := d.Info(context.Background())
	if err != nil || info.Model != "Pixel 7" || info.Manufacturer != "Google" || info.OSVersion != "Android 15" || info.Extra["ro.serialno"] != "PX1" {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestUnauthorizedDevice(t *testing.T) {
	d := findDevice(t, fakeServer(t, &adbtest.Device{Serial: "U1", State: "unauthorized"}), "U1")
	if _, err := d.Info(context.Background()); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if _, err := d.Pull(context.Background(), "/x", &bytes.Buffer{}); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("pull err = %v", err)
	}
}

func TestListPullPush(t *testing.T) {
	s := fakeServer(t, basicDevice())
	d := findDevice(t, s, "PX1")
	es, err := d.List(context.Background(), "/sdcard")
	if err != nil || len(es) != 1 || es[0].Path != "/sdcard/a.txt" || es[0].Size != 5 || es[0].IsDir {
		t.Fatalf("list = %+v, %v", es, err)
	}
	var buf bytes.Buffer
	if n, err := d.Pull(context.Background(), "/sdcard/a.txt", &buf); err != nil || n != 5 || buf.String() != "hello" {
		t.Fatalf("pull n=%d err=%v %q", n, err, buf.String())
	}
	if err := d.Push(context.Background(), bytes.NewReader([]byte("tool")), "/data/local/tmp/t", 0o755); err != nil {
		t.Fatal(err)
	}
	if f, ok := s.Pushed("PX1", "/data/local/tmp/t"); !ok || string(f.Data) != "tool" || f.Mode != 0o100755 {
		t.Fatalf("pushed = %+v %v", f, ok)
	}
}

func newCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func logicalDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{
			"getprop":             []byte(getprop),
			"pm list packages -f": []byte("package:/data/app/base.apk=com.whatsapp\n"),
		},
		Files: map[string]adbtest.File{
			"/sdcard/DCIM/a.jpg":  {Data: []byte("jpeg")},
			"/sdcard/link":        {Data: []byte("/x"), Mode: 0o120777},
			"/sdcard/notes:1.txt": {Data: []byte("note")},
			"/sdcard/secret.db":   {Data: []byte("s")},
			"/sdcard/zz.txt":      {Data: []byte("last")},
		},
		Unreadable: map[string]bool{"/sdcard/secret.db": true},
	}
}

func TestAcquireLogicalContinuesAfterUnreadable(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, logicalDevice()), "PX1")
	var progressed int64
	err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{}, func(done, _ int64) { progressed = done })
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	byRemote := map[string]evidence.ManifestRecord{}
	for _, r := range m {
		byRemote[r.Source.RemotePath] = r
	}
	for _, p := range []string{"exec:getprop", "exec:pm list packages -f", "/sdcard/DCIM/a.jpg", "/sdcard/notes:1.txt", "/sdcard/zz.txt"} {
		if r, ok := byRemote[p]; !ok || r.Incomplete {
			t.Errorf("%s: record %+v ok=%v", p, r, ok)
		}
	}
	if r := byRemote["/sdcard/secret.db"]; !r.Incomplete {
		t.Errorf("unreadable file should be an incomplete record: %+v", r)
	}
	if _, ok := byRemote["/sdcard/link"]; ok {
		t.Error("symlink should be skipped, not pulled")
	}
	if !strings.HasSuffix(byRemote["/sdcard/notes:1.txt"].Path, "files/sdcard/notes_1.txt") {
		t.Errorf("path = %s", byRemote["/sdcard/notes:1.txt"].Path)
	}
	if progressed != 12 { // jpeg + note + last
		t.Errorf("progress = %d", progressed)
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if strings.Count(string(audit), `"action":"acquire.warning"`) != 2 || !strings.Contains(string(audit), `"action":"acquire.stats"`) {
		t.Errorf("audit missing warnings/stats:\n%s", audit)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}
