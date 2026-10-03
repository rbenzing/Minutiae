package ios_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/iostest"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
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

func newCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func statusPlist(t *testing.T, state string) []byte {
	t.Helper()
	b, err := plist.Marshal(map[string]any{"SnapshotState": state}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// uploadThenFinish uploads two files plus, when snapshot is non-nil,
// U1/Status.plist, then finishes with the given ProcessMessage code.
func uploadThenFinish(code int, snapshot []byte) func(*mb2test.Device) error {
	var extra []mb2test.Upload
	if snapshot != nil {
		extra = append(extra, mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: snapshot})
	}
	return uploadExtraThenFinish(code, extra...)
}

// uploadExtraThenFinish uploads two files plus extra, then finishes with the
// given ProcessMessage code.
func uploadExtraThenFinish(code int, extra ...mb2test.Upload) func(*mb2test.Device) error {
	return func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		files := []mb2test.Upload{
			{DeviceName: "/a", Name: "U1/Info.plist", Data: []byte("info")},
			{DeviceName: "/b", Name: "U1/Manifest.db", Data: []byte("sqlite")},
		}
		files = append(files, extra...)
		if _, err := d.UploadFiles(files...); err != nil {
			return err
		}
		return d.Finish(code)
	}
}

func TestBackupPromotesStagedFiles(t *testing.T) {
	b := fakeIPhone()
	b.Script = uploadThenFinish(0, statusPlist(t, "finished"))
	c := newCase(t)
	la := first(t, b).(device.LogicalAcquirer)
	if err := la.AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-b.ScriptErr; err != nil {
		t.Fatalf("script: %v", err)
	}
	m, _ := c.Manifest()
	var paths []string
	for _, r := range m {
		paths = append(paths, r.Path)
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{"device/lockdown.json", "backup/U1/Info.plist", "backup/U1/Manifest.db", "backup/U1/Status.plist"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in\n%s", want, joined)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(c.Dir, "staging")); len(entries) != 0 {
		t.Errorf("staging not cleaned: %v", entries)
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"snapshot_state":"finished"`) {
		t.Errorf("snapshot_state not audited:\n%s", audit)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

func TestBackupDeviceErrorStillPromotes(t *testing.T) {
	b := fakeIPhone()
	b.Script = uploadThenFinish(105, statusPlist(t, "finished"))
	c := newCase(t)
	err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "105") {
		t.Fatalf("err = %v", err)
	}
	<-b.ScriptErr
	m, _ := c.Manifest()
	if len(m) != 4 {
		t.Fatalf("expected lockdown + 3 promoted files, got %d", len(m))
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"action":"acquire.error"`) {
		t.Fatal("acquire.error not audited")
	}
}

// dictBombPlist is a binary plist whose only object is a dict claiming 2^63
// entries; an unvalidated decoder panics or allocates without bound on it.
func dictBombPlist() []byte {
	b := binary.BigEndian.AppendUint64([]byte("bplist00\xdf\x13"), 1<<63)
	tableOff := len(b)
	b = append(b, 8)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 3
	binary.BigEndian.PutUint64(trailer[8:], 1)
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

func xmlStatus(state string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>` +
		`<key>SnapshotState</key><string>` + state + `</string></dict></plist>`)
}

func TestBackupUnfinishedSnapshotIsError(t *testing.T) {
	status := func(data []byte) []mb2test.Upload {
		return []mb2test.Upload{{DeviceName: "/c", Name: "U1/Status.plist", Data: data}}
	}
	for _, tc := range []struct {
		name      string
		extra     []mb2test.Upload
		wantState string
	}{
		{"uploading", status(statusPlist(t, "uploading")), "uploading"},
		{"xml-uploading", status(xmlStatus("uploading")), "uploading"},
		{"missing", nil, "missing"},
		{"garbage", status([]byte("not a plist")), "unreadable"},
		{"dict-bomb", status(dictBombPlist()), "unreadable"},
		{"oversized", status(append(xmlStatus("finished"), bytes.Repeat([]byte(" "), 1<<20)...)), "unreadable"},
		{"directory", []mb2test.Upload{{DeviceName: "/c", Name: "U1/Status.plist/x", Data: []byte("x")}}, "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fakeIPhone()
			b.Script = uploadExtraThenFinish(0, tc.extra...)
			c := newCase(t)
			err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil)
			if err == nil || !strings.Contains(err.Error(), "backup snapshot not finished") {
				t.Fatalf("err = %v", err)
			}
			if serr := <-b.ScriptErr; serr != nil {
				t.Fatalf("script: %v", serr)
			}
			m, _ := c.Manifest()
			if want := 3 + len(tc.extra); len(m) != want {
				t.Fatalf("expected %d promoted artifacts, got %d", want, len(m))
			}
			if entries, _ := os.ReadDir(filepath.Join(c.Dir, "staging")); len(entries) != 0 {
				t.Errorf("staging not cleaned: %v", entries)
			}
			audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
			if !strings.Contains(string(audit), `"action":"acquire.error"`) {
				t.Fatalf("acquire.error not audited:\n%s", audit)
			}
			if want := `"snapshot_state":"` + tc.wantState + `"`; !strings.Contains(string(audit), want) {
				t.Fatalf("want %s audited:\n%s", want, audit)
			}
		})
	}
}
