package android

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
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
