package cli

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
)

func lsDevice(features []string) *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device", Features: features,
		Files: map[string]adbtest.File{
			"/sdcard/a.txt":     {Data: []byte("hello")}, // no MTime: the time is unknown
			"/sdcard/locked/x":  {Data: []byte("x")},
			"/sdcard/other/y":   {Data: []byte("y")},
			"/sdcard/other/bad": {Data: []byte("z")},
		},
		ListErrno:  map[string]uint32{"/sdcard/locked": 13},
		EntryErrno: map[string]uint32{"/sdcard/other/bad": 5},
	}
}

// W72: an unknown time is null in JSON, never a zero date; "unknown" in text.
func TestAndroidLsUnknownTimeIsNullInJSONAndUnknownInText(t *testing.T) {
	d, _ := androidDeps(t, lsDevice(nil))
	code, out := run(t, d, "android", "ls", "--json", "--serial", "PX1", "/sdcard")
	nullTime := strings.Contains(out, `"mod_time":null`) || strings.Contains(out, `"mod_time": null`)
	if code != 0 || !nullTime || strings.Contains(out, "0001-01-01") {
		t.Fatalf("json: %d %s", code, out)
	}
	code, out = run(t, d, "android", "ls", "--serial", "PX1", "/sdcard")
	if code != 0 || !strings.Contains(out, "unknown") || strings.Contains(out, "0001-01-01") {
		t.Fatalf("text: %d %s", code, out)
	}
}

// W75: an unreadable directory is an error in text and JSON, v1 and v2, never
// an empty listing; a partial listing prints what was readable and still fails.
func TestAndroidLsUnreadableDirectoryIsAnErrorNotEmpty(t *testing.T) {
	for name, features := range map[string][]string{"v2": nil, "v1": {"shell_v2"}} {
		d, _ := androidDeps(t, lsDevice(features))
		for _, asJSON := range []bool{false, true} {
			args := []string{"android", "ls", "--serial", "PX1", "/sdcard/locked"}
			if asJSON {
				args = []string{"android", "ls", "--json", "--serial", "PX1", "/sdcard/locked"}
			}
			code, out := run(t, d, args...)
			if code == 0 || strings.Contains(out, "[]") || strings.Contains(out, "null") {
				t.Errorf("%s json=%v: exit %d, output %q: an unreadable directory must fail, not list as empty", name, asJSON, code, out)
			}
		}
	}
	d, _ := androidDeps(t, lsDevice(nil))
	var stdout, stderr bytes.Buffer
	d.Out, d.Err, d.In = &stdout, &stderr, strings.NewReader("")
	code := Run([]string{"android", "ls", "--serial", "PX1", "/sdcard/other"}, d)
	// The readable entry is on stdout as a whole line; the failure is on stderr.
	var entryLine bool
	for _, l := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(l, "-rw-r--r--") && strings.HasSuffix(l, " y") {
			entryLine = true
		}
	}
	if code == 0 || !entryLine || stderr.Len() == 0 {
		t.Fatalf("partial listing (v2): exit %d, stdout %q, stderr %q: want the readable entry line on stdout and a failure", code, stdout.String(), stderr.String())
	}
}

type zeroTimeIOS struct{}

func (zeroTimeIOS) ID() string        { return "I1" }
func (zeroTimeIOS) Kind() device.Kind { return device.KindIOS }
func (zeroTimeIOS) Info(context.Context) (device.Info, error) {
	return device.Info{ID: "I1", Kind: device.KindIOS}, nil
}

func (zeroTimeIOS) List(context.Context, string) ([]device.FileEntry, error) {
	return []device.FileEntry{{Name: "a.jpg", Path: "/DCIM/a.jpg", Size: 3, Mode: 0o644}}, nil
}
func (zeroTimeIOS) Pull(context.Context, string, io.Writer) (int64, error)     { return 0, nil }
func (zeroTimeIOS) Push(context.Context, io.Reader, string, fs.FileMode) error { return nil }

type zeroTimeEnum struct{}

func (zeroTimeEnum) Kind() device.Kind { return device.KindIOS }
func (zeroTimeEnum) List(context.Context) ([]device.Device, error) {
	return []device.Device{zeroTimeIOS{}}, nil
}

// W74: the ls command is shared by android and ios; a zero ModTime (an iOS
// listing may carry none) prints "unknown" for both.
func TestIOSLsPrintsUnknownForAZeroModTime(t *testing.T) {
	d := Deps{Registry: device.NewRegistry(zeroTimeEnum{})}
	code, out := run(t, d, "ios", "ls", "/DCIM")
	if code != 0 || !strings.Contains(out, "unknown") || strings.Contains(out, "0001-01-01") {
		t.Fatalf("ios ls: %d %s", code, out)
	}
}
