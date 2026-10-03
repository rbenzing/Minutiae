package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/android"
	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func androidDeps(t *testing.T, devs ...*adbtest.Device) (Deps, *adbtest.Server) {
	t.Helper()
	s, err := adbtest.Start(devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return Deps{Registry: device.NewRegistry(android.Enumerator{C: adb.New(s.Addr())})}, s
}

func cliPixel() *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{
			"getprop":             []byte("[ro.product.model]: [Pixel 7]\n[ro.build.version.release]: [15]\n"),
			"pm list packages -f": []byte("package:/a.apk=com.a\n"),
		},
		Files: map[string]adbtest.File{"/sdcard/a.txt": {Data: []byte("hello")}},
	}
}

func TestAndroidInfoAndLs(t *testing.T) {
	d, _ := androidDeps(t, cliPixel())
	if code, out := run(t, d, "android", "info"); code != 0 || !strings.Contains(out, "Pixel 7") {
		t.Fatalf("info: %d %s", code, out)
	}
	if code, out := run(t, d, "android", "ls", "--serial", "PX1", "/sdcard"); code != 0 || !strings.Contains(out, "a.txt") {
		t.Fatalf("ls: %d %s", code, out)
	}
}

func TestAndroidInfoUnauthorizedExitCode(t *testing.T) {
	d, _ := androidDeps(t, &adbtest.Device{Serial: "U1", State: "unauthorized"})
	if code, _ := run(t, d, "android", "info"); code != ExitDevice {
		t.Fatalf("code = %d", code)
	}
}

func TestDevicesShowsUnauthorizedError(t *testing.T) {
	d, _ := androidDeps(t, &adbtest.Device{Serial: "U1", State: "unauthorized"})
	code, out := run(t, d, "devices")
	if code != 0 || !strings.Contains(out, "U1") || !strings.Contains(out, "error: ") || !strings.Contains(out, "not authorized") {
		t.Fatalf("devices: %d %q", code, out)
	}
}

func TestAndroidPullAndLogical(t *testing.T) {
	d, _ := androidDeps(t, cliPixel())
	c := newCLICase(t)
	if code, out := run(t, d, "android", "pull", "--case", c, "/sdcard/a.txt"); code != 0 || !strings.Contains(out, "files/sdcard/a.txt") {
		t.Fatalf("pull: %d %s", code, out)
	}
	if code, out := run(t, d, "android", "logical", "--case", c); code != 0 {
		t.Fatalf("logical: %d %s", code, out)
	}
	if code, out := run(t, d, "case", "verify", "--case", c); code != 0 {
		t.Fatalf("verify: %d %s", code, out)
	}
}

func TestAndroidPushRequiresFlag(t *testing.T) {
	d, s := androidDeps(t, cliPixel())
	c := newCLICase(t)
	local := filepath.Join(t.TempDir(), "tool")
	_ = os.WriteFile(local, []byte("bin"), 0o600)
	if code, _ := run(t, d, "android", "push", "--case", c, local, "/data/local/tmp/tool"); code != ExitDevice {
		t.Fatalf("push without flag: code %d", code)
	}
	if _, ok := s.Pushed("PX1", "/data/local/tmp/tool"); ok {
		t.Fatal("file pushed without --allow-device-write")
	}
	if code, out := run(t, d, "android", "push", "--case", c, "--allow-device-write", local, "/data/local/tmp/tool"); code != 0 {
		t.Fatalf("push: %d %s", code, out)
	}
	if _, ok := s.Pushed("PX1", "/data/local/tmp/tool"); !ok {
		t.Fatal("file not pushed")
	}
}

func TestAndroidMultipleDevicesNeedSerial(t *testing.T) {
	two := cliPixel()
	two.Serial = "PX2"
	d, _ := androidDeps(t, cliPixel(), two)
	if code, _ := run(t, d, "android", "info"); code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func cliRooted() *adbtest.Device {
	su := func(cmd string) string { return "su -c '" + cmd + "' 2>/dev/null" }
	return &adbtest.Device{
		Serial: "R1", State: "device",
		Commands: map[string][]byte{
			su("id"):                                           []byte("uid=0(root)\n"),
			su("cat /proc/partitions"):                         []byte("major minor  #blocks  name\n 179 1 4 mmcblk0p1\n"),
			su("ls -l /dev/block/by-name/"):                    []byte("lrwxrwxrwx 1 root root 20 2009-01-01 00:00 boot -> /dev/block/mmcblk0p1\n"),
			su("cat /sys/class/block/mmcblk0p1/size"):          []byte("9\n"),
			su("dd if=/dev/block/mmcblk0p1 bs=4M 2>/dev/null"): []byte(strings.Repeat("B", 9*512)),
		},
	}
}

func TestAndroidImageRecordsBlockPathAndExpectedSize(t *testing.T) {
	d, _ := androidDeps(t, cliRooted())
	c := newCLICase(t)
	if code, out := run(t, d, "android", "image", "--case", c, "--partition", "boot"); code != 0 {
		t.Fatalf("image: %d %s", code, out)
	}
	entries, err := evidence.ReadAuditEntries(filepath.Join(c, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var start map[string]any
	for _, e := range entries {
		if e.Action == "acquire.start" && e.Details["type"] == "physical" {
			start = e.Details
		}
	}
	if start["partition"] != "boot" || start["block_path"] != "/dev/block/mmcblk0p1" || fmt.Sprint(start["expected_size"]) != "4608" {
		t.Fatalf("acquire.start details = %v", start)
	}
	if code, out := run(t, d, "case", "verify", "--case", c); code != 0 {
		t.Fatalf("verify: %d %s", code, out)
	}
}

func TestAndroidLsEscapesControlCharacters(t *testing.T) {
	dev := cliPixel()
	dev.Files["/sdcard/evil\x1b[2J\nname"] = adbtest.File{Data: []byte("x")}
	d, _ := androidDeps(t, dev)
	code, out := run(t, d, "android", "ls", "/sdcard")
	if code != 0 || strings.ContainsAny(out, "\x1b") || !strings.Contains(out, `"evil\x1b[2J\nname"`) {
		t.Fatalf("ls: %d %q", code, out)
	}
	if !strings.Contains(out, " a.txt\n") {
		t.Fatalf("plain names should print unquoted: %q", out)
	}
}

func TestAndroidPullSkipsRecordsThatWereNeverCreated(t *testing.T) {
	d, _ := androidDeps(t, cliPixel())
	c := newCLICase(t)
	code, out := run(t, d, "android", "pull", "--case", c, "/sdcard/a.txt", "/sdcard/a.txt")
	if code == 0 || strings.Count(out, "files/sdcard/a.txt") != 2 { // one record line + the error
		t.Fatalf("pull: %d %q", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  ") {
			t.Fatalf("zero-value record printed: %q in %q", line, out)
		}
	}
}

type errDev struct{ stubDev }

func (e errDev) Info(context.Context) (device.Info, error) {
	return device.Info{}, errors.New("device unauthorized.\nPlease check the confirmation dialog")
}

type errEnum struct{}

func (errEnum) Kind() device.Kind { return device.KindAndroid }
func (errEnum) List(context.Context) ([]device.Device, error) {
	return []device.Device{errDev{stubDev{"E1"}}}, nil
}

func TestDevicesErrorColumnIsOneLine(t *testing.T) {
	code, out := run(t, Deps{Registry: device.NewRegistry(errEnum{})}, "devices")
	if code != 0 || !strings.Contains(out, "error: device unauthorized. Please check the confirmation dialog\n") {
		t.Fatalf("devices: %d %q", code, out)
	}
}
