package ios_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/iostest"
)

func fakeIPhone() *iostest.Backend {
	b := iostest.NewBackend()
	b.Entries = []ios.Entry{{UDID: "U1", ConnectionType: "USB"}}
	b.LockdownValues = map[string]any{
		"ProductType": "iPhone15,2", "ProductVersion": "17.5", "SerialNumber": "F2LXYZ",
		"DeviceName": "Jane's iPhone", "PasswordProtected": true, "Nested": map[string]any{"x": 1},
	}
	b.Files["/DCIM/100APPLE/IMG_0001.HEIC"] = []byte("heic")
	return b
}

func first(t *testing.T, b ios.Backend) device.Device {
	t.Helper()
	devs, err := ios.Enumerator{B: b}.List(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs = %v, %v", devs, err)
	}
	return devs[0]
}

func TestInfo(t *testing.T) {
	d := first(t, fakeIPhone())
	info, err := d.Info(context.Background())
	if err != nil || info.ID != "U1" || info.Model != "iPhone15,2" || info.OSVersion != "iOS 17.5" ||
		info.SerialNumber != "F2LXYZ" || info.Manufacturer != "Apple" || info.Extra["DeviceName"] != "Jane's iPhone" ||
		info.Extra["PasswordProtected"] != "true" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if _, ok := info.Extra["Nested"]; ok {
		t.Error("non-scalar values should not be flattened into Extra")
	}
}

func TestUnpairedIsUnauthorized(t *testing.T) {
	b := fakeIPhone()
	b.LockdownErr = errors.New("could not find pair record for U1")
	if _, err := first(t, b).Info(context.Background()); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestListPullPushUnsupported(t *testing.T) {
	ft := first(t, fakeIPhone()).(device.FileTransferer)
	es, err := ft.List(context.Background(), "/DCIM")
	if err != nil || len(es) != 1 || es[0].Name != "100APPLE" || !es[0].IsDir {
		t.Fatalf("list = %+v, %v", es, err)
	}
	var buf bytes.Buffer
	if n, err := ft.Pull(context.Background(), "/DCIM/100APPLE/IMG_0001.HEIC", &buf); err != nil || n != 4 || buf.String() != "heic" {
		t.Fatalf("pull %d %v %q", n, err, buf.String())
	}
	if err := ft.Push(context.Background(), &buf, "/x", 0o644); !errors.Is(err, device.ErrUnsupported) {
		t.Fatalf("push err = %v", err)
	}
}
