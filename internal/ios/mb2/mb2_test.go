package mb2_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

var manifestDB = bytes.Repeat([]byte("SQLite format 3\x00"), 13_000) // ~208 KB

func runBackup(t *testing.T, script func(*mb2test.Device) error) (string, mb2.Result, error) {
	t.Helper()
	host, dev := mb2test.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := script(dev)
		_ = dev.Close()
		errc <- err
	}()
	dir := t.TempDir()
	c := mb2.New(host)
	var res mb2.Result
	err := c.Handshake()
	if err == nil {
		res, err = c.Backup(context.Background(), mb2.BackupOptions{
			UDID: "U1", Dir: dir, FreeSpace: func() (uint64, error) { return 1 << 40, nil },
		})
	}
	_ = host.Close()
	if serr := <-errc; serr != nil {
		t.Fatalf("device script: %v", serr)
	}
	return dir, res, err
}

func check(cond bool, format string, a ...any) error {
	if !cond {
		return fmt.Errorf(format, a...)
	}
	return nil
}

func TestBackupFlow(t *testing.T) {
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		req, err := d.ExpectBackupRequest()
		if err != nil {
			return err
		}
		if err := check(req["TargetIdentifier"] == "U1", "target = %v", req["TargetIdentifier"]); err != nil {
			return err
		}
		if code, _, err := d.Request("DLMessageCreateDirectory", "U1"); err != nil || code != 0 {
			return fmt.Errorf("mkdir: %d %v", code, err)
		}
		if code, free, err := d.Request("DLMessageGetFreeDiskSpace"); err != nil || code != 0 || free != uint64(1<<40) {
			return fmt.Errorf("free: %d %v %v", code, free, err)
		}
		if got, code, err := d.DownloadFiles("U1/Status.plist"); err != nil || code != -13 || len(got) != 0 {
			return fmt.Errorf("download missing: %v %d %v", got, code, err)
		}
		code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/Manifest.db", Data: manifestDB},
			mb2test.Upload{DeviceName: "/b", Name: "U1/empty"},
			mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: []byte("status"), EndRemote: true},
			mb2test.Upload{DeviceName: "/d", Name: "U1/tmp/junk", Data: []byte("j")},
		)
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		_, listing, err := d.Request("DLContentsOfDirectory", "U1")
		if m, _ := listing.(map[string]any); err != nil || len(m) != 4 {
			return fmt.Errorf("listing: %v %v", listing, err)
		}
		if code, _, err := d.Request("DLMessageMoveItems", map[string]any{"U1/Status.plist": "U1/Status2.plist"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("move: %d %v", code, err)
		}
		if code, _, err := d.Request("DLMessageRemoveItems", []any{"U1/tmp"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("remove: %d %v", code, err)
		}
		if code, _, err := d.Request("DLMessageCopyItem", "U1/Status2.plist", "U1/Status3.plist"); err != nil || code != 0 {
			return fmt.Errorf("copy: %d %v", code, err)
		}
		got, code, err := d.DownloadFiles("U1/Status2.plist")
		if err != nil || code != 0 || string(got["U1/Status2.plist"]) != "status" {
			return fmt.Errorf("download: %q %d %v", got, code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return b
	}
	if !bytes.Equal(read("U1/Manifest.db"), manifestDB) || len(read("U1/empty")) != 0 ||
		string(read("U1/Status2.plist")) != "status" || string(read("U1/Status3.plist")) != "status" {
		t.Fatal("reconstructed files differ")
	}
	if _, err := os.Stat(filepath.Join(dir, "U1", "tmp")); !os.IsNotExist(err) {
		t.Fatalf("U1/tmp not removed: %v", err)
	}
	if res.BytesReceived != int64(len(manifestDB)+len("status")+1) {
		t.Fatalf("bytes received = %d", res.BytesReceived)
	}
}

func TestBackupRejectsEscapingPath(t *testing.T) {
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/x", Name: "../evil", Data: []byte("bad")},
			mb2test.Upload{DeviceName: "/y", Name: "U1/ok", Data: []byte("ok")},
		)
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); !os.IsNotExist(err) {
		t.Fatal("file written outside the backup directory")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "U1", "ok")); string(b) != "ok" {
		t.Fatal("file after the rejected one was not received")
	}
	if len(res.LocalErrors) != 1 {
		t.Fatalf("local errors = %v", res.LocalErrors)
	}
}

func TestBackupDeviceError(t *testing.T) {
	_, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		return d.Finish(105)
	})
	var de *mb2.DeviceError
	if !errors.As(err, &de) || de.Code != 105 {
		t.Fatalf("err = %v", err)
	}
}
