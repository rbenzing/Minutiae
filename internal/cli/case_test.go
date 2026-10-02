package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func run(t *testing.T, d Deps, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	d.Out = &out
	if d.In == nil {
		d.In = strings.NewReader("")
	}
	if d.Err == nil {
		d.Err = &out
	}
	if d.Registry == nil {
		d.Registry = device.NewRegistry()
	}
	code := Run(args, d)
	return code, out.String()
}

func newCLICase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if code, out := run(t, Deps{}, "case", "new", "--dir", dir, "--id", "C1", "--examiner", "E"); code != 0 {
		t.Fatalf("case new: %d %s", code, out)
	}
	return filepath.Join(dir, "C1")
}

func TestCaseNewInfoVerify(t *testing.T) {
	c := newCLICase(t)
	code, out := run(t, Deps{}, "case", "info", "--case", c, "--json")
	if code != 0 {
		t.Fatalf("info: %d %s", code, out)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil || info["id"] != "C1" {
		t.Fatalf("info json %q: %v", out, err)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", c); code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("verify: %d %s", code, out)
	}
}

func TestCaseNewTwiceFails(t *testing.T) {
	dir := t.TempDir()
	args := []string{"case", "new", "--dir", dir, "--id", "C1", "--examiner", "E"}
	if code, _ := run(t, Deps{}, args...); code != 0 {
		t.Fatal("first create failed")
	}
	if code, out := run(t, Deps{}, args...); code != ExitError || !strings.Contains(out, "already exists") {
		t.Fatalf("second create: %d %s", code, out)
	}
}

func TestCaseUnknownSubcommandIsUsage(t *testing.T) {
	if code, _ := run(t, Deps{}, "case", "bogus"); code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}

func TestCaseNewMissingFlagIsUsage(t *testing.T) {
	if code, _ := run(t, Deps{}, "case", "new", "--dir", t.TempDir()); code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}

func TestCaseVerifyExitCodeOnTamper(t *testing.T) {
	c := newCLICase(t)
	ec, err := evidence.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ec.Capture("d", "a", "x.bin", evidence.Source{Kind: "file", DeviceID: "d"}, func(w io.Writer) error {
		_, err := w.Write([]byte("abc"))
		return err
	})
	_ = ec.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(c, filepath.FromSlash(rec.Path)), []byte("abd"), 0o600)
	if code, out := run(t, Deps{}, "case", "verify", "--case", c); code != ExitIntegrity || !strings.Contains(out, "hash mismatch") {
		t.Fatalf("verify: %d %s", code, out)
	}
}

type stubDev struct{ id string }

func (s stubDev) ID() string        { return s.id }
func (s stubDev) Kind() device.Kind { return device.KindAndroid }
func (s stubDev) Info(context.Context) (device.Info, error) {
	return device.Info{ID: s.id, Kind: device.KindAndroid, Model: "Pixel"}, nil
}

type stubEnum struct{}

func (stubEnum) Kind() device.Kind { return device.KindAndroid }
func (stubEnum) List(context.Context) ([]device.Device, error) {
	return []device.Device{stubDev{"SER1"}}, nil
}

func TestDevicesCommand(t *testing.T) {
	code, out := run(t, Deps{Registry: device.NewRegistry(stubEnum{})}, "devices")
	if code != 0 || !strings.Contains(out, "SER1") || !strings.Contains(out, "Pixel") {
		t.Fatalf("devices: %d %s", code, out)
	}
}

func TestExitCodeDeviceAndIntegrity(t *testing.T) {
	if ExitCode(device.ErrNotRooted) != ExitDevice {
		t.Error("ErrNotRooted should map to ExitDevice")
	}
	if ExitCode(evidence.ErrIntegrity) != ExitIntegrity {
		t.Error("ErrIntegrity should map to ExitIntegrity")
	}
}
