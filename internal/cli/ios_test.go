package cli

import (
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/iostest"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

func iosDeps(t *testing.T) (Deps, *iostest.Backend) {
	t.Helper()
	status, err := plist.Marshal(map[string]any{"SnapshotState": "finished"}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	b := iostest.NewBackend()
	b.Entries = []ios.Entry{{UDID: "U1"}}
	b.LockdownValues = map[string]any{"ProductType": "iPhone15,2", "ProductVersion": "17.5"}
	b.Files["/DCIM/IMG_1.HEIC"] = []byte("heic")
	b.Script = func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if _, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/Manifest.db", Data: []byte("db")},
			mb2test.Upload{DeviceName: "/b", Name: "U1/Status.plist", Data: status},
		); err != nil {
			return err
		}
		return d.Finish(0)
	}
	return Deps{Registry: device.NewRegistry(ios.Enumerator{B: b})}, b
}

func TestIOSInfoPullBackup(t *testing.T) {
	d, b := iosDeps(t)
	if code, out := run(t, d, "ios", "info"); code != 0 || !strings.Contains(out, "iPhone15,2") {
		t.Fatalf("info: %d %s", code, out)
	}
	c := newCLICase(t)
	if code, out := run(t, d, "ios", "pull", "--case", c, "/DCIM/IMG_1.HEIC"); code != 0 || !strings.Contains(out, "IMG_1.HEIC") {
		t.Fatalf("pull: %d %s", code, out)
	}
	if code, out := run(t, d, "ios", "backup", "--udid", "U1", "--case", c); code != 0 {
		t.Fatalf("backup: %d %s", code, out)
	}
	if err := <-b.ScriptErr; err != nil {
		t.Fatalf("device script: %v", err)
	}
	if code, out := run(t, d, "case", "verify", "--case", c); code != 0 {
		t.Fatalf("verify: %d %s", code, out)
	}
}

// ios has no push subcommand; newGroupCmd makes the unknown subcommand a usage error.
func TestIOSPushNotOffered(t *testing.T) {
	d, _ := iosDeps(t)
	if code, _ := run(t, d, "ios", "push", "a", "b"); code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}
