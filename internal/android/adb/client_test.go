package adb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
)

func startServer(t *testing.T, devs ...*adbtest.Device) *adb.Client {
	t.Helper()
	s, err := adbtest.Start(devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return adb.New(s.Addr())
}

var pixel = &adbtest.Device{
	Serial: "PX1", State: "device", Props: "product:panther model:Pixel_7 device:panther transport_id:1",
	Commands: map[string][]byte{"getprop ro.product.model": []byte("Pixel 7\n")},
}

func TestVersion(t *testing.T) {
	c := startServer(t)
	if v, err := c.Version(context.Background()); err != nil || v != 41 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestDevices(t *testing.T) {
	c := startServer(t, pixel, &adbtest.Device{Serial: "emulator-5554", State: "unauthorized"})
	ds, err := c.Devices(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("devices = %+v, %v", ds, err)
	}
	if ds[0].Serial != "PX1" || ds[0].State != "device" || ds[0].Props["model"] != "Pixel_7" {
		t.Fatalf("entry 0 = %+v", ds[0])
	}
	if ds[1].State != "unauthorized" {
		t.Fatalf("entry 1 = %+v", ds[1])
	}
}

func TestServerUnavailable(t *testing.T) {
	_, err := adb.New("127.0.0.1:1").Version(context.Background())
	if !errors.Is(err, adb.ErrServerUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestOutput(t *testing.T) {
	c := startServer(t, pixel)
	out, err := c.Output(context.Background(), "PX1", "getprop ro.product.model")
	if err != nil || string(out) != "Pixel 7\n" {
		t.Fatalf("out = %q, %v", out, err)
	}
}

func TestTransportErrors(t *testing.T) {
	c := startServer(t, pixel, &adbtest.Device{Serial: "U1", State: "unauthorized"})
	var fe *adb.FailError
	if _, err := c.Output(context.Background(), "nope", "id"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "not found") {
		t.Fatalf("unknown device err = %v", err)
	}
	if _, err := c.Output(context.Background(), "U1", "id"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "unauthorized") {
		t.Fatalf("unauthorized err = %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	c := startServer(t, pixel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Output(ctx, "PX1", "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func noExecPixel() *adbtest.Device {
	return &adbtest.Device{
		Serial: "NE1", State: "device", NoExec: true,
		Commands: map[string][]byte{"getprop ro.product.model": []byte("Pixel 7\n")},
	}
}

func TestOutputFallsBackToShellWhenExecFails(t *testing.T) {
	c := startServer(t, noExecPixel())
	out, err := c.Output(context.Background(), "NE1", "getprop ro.product.model")
	if err != nil || string(out) != "Pixel 7\n" {
		t.Fatalf("out = %q, %v", out, err)
	}
	if _, err := c.Exec(context.Background(), "NE1", "getprop ro.product.model"); err == nil {
		t.Fatal("Exec must not fall back to shell:")
	}
}

func TestStreamReportsServiceUsed(t *testing.T) {
	c := startServer(t, pixel, noExecPixel())
	for serial, want := range map[string]string{"PX1": "exec:", "NE1": "shell:"} {
		rc, svc, err := c.Stream(context.Background(), serial, "getprop ro.product.model")
		if err != nil || svc != want {
			t.Fatalf("%s: service = %q, %v", serial, svc, err)
		}
		_ = rc.Close()
	}
	var fe *adb.FailError
	if _, _, err := c.Stream(context.Background(), "nope", "id"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "not found") {
		t.Fatalf("unknown device err = %v", err)
	}
}
