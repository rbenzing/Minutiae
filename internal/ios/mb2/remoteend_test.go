package mb2_test

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

// The block format and codes are from memory of libimobiledevice's
// mobilebackup2 (CODE_SUCCESS 0x00, CODE_ERROR_LOCAL 0x06, CODE_ERROR_REMOTE
// 0x0b, CODE_FILE_DATA 0x0c; big-endian length that includes the code byte).
// This probe pins them so every later test in this plan builds on stated bytes.
func TestBlockWireFormat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    byte
		payload []byte
		want    []byte
	}{
		{"remote error", mb2.CodeErrorRemote, []byte("end"), []byte{0, 0, 0, 4, 0x0b, 'e', 'n', 'd'}},
		{"file data", mb2.CodeFileData, []byte("x"), []byte{0, 0, 0, 2, 0x0c, 'x'}},
		{"success", mb2.CodeSuccess, nil, []byte{0, 0, 0, 1, 0x00}},
		{"local error", mb2.CodeErrorLocal, []byte("no"), []byte{0, 0, 0, 3, 0x06, 'n', 'o'}},
	} {
		var b bytes.Buffer
		if err := mb2.WriteBlock(&b, tc.code, tc.payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b.Bytes(), tc.want) {
			t.Errorf("%s: block = % x, want % x", tc.name, b.Bytes(), tc.want)
		}
	}
}

func TestBackupRecordsRemoteEndAfterData(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/cut.bin", Data: []byte("abc"), EndRemote: true, RemoteMsg: []byte("read failed")},
			mb2test.Upload{DeviceName: "/b", Name: "U1/ok.bin", Data: []byte("defg")},
			mb2test.Upload{DeviceName: "/c", Name: "U1/gone", EndRemote: true},
		)
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]mb2.RemoteEnd{
		"U1/cut.bin": {Code: mb2.CodeErrorRemote, Bytes: 3, Message: "read failed"},
	}
	if !reflect.DeepEqual(res.RemoteEnded, want) {
		t.Fatalf("RemoteEnded = %+v, want %+v", res.RemoteEnded, want)
	}
	if res.RemoteErrorCount != 1 { // U1/gone: the marker without data is still a counted remote error
		t.Fatalf("RemoteErrorCount = %d", res.RemoteErrorCount)
	}
}

func TestBackupResentFileClearsItsRemoteEnd(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("abc"), EndRemote: true}); err != nil || code != 0 {
			return fmt.Errorf("first upload: %d %v", code, err)
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("abcdef")}); err != nil || code != 0 {
			return fmt.Errorf("second upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RemoteEnded) != 0 {
		t.Fatalf("a file re-sent with a success marker kept its remote end: %+v", res.RemoteEnded)
	}
}

func TestBackupRemoteEndMessageIsBounded(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("x"), EndRemote: true, RemoteMsg: bytes.Repeat([]byte("E"), 1<<20)})
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if e := res.RemoteEnded["U1/f"]; len(e.Message) != 4096 || e.Bytes != 1 {
		t.Fatalf("end = bytes %d, message %d bytes", e.Bytes, len(e.Message))
	}
}

// W11 (K3): file data followed by a zero length, with no success or error
// block, is an interrupted transfer, not a normal end.
func TestBackupZeroLengthAfterDataIsAnInterruptedTransfer(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if err := d.Send("DLMessageUploadFiles", map[string]any{}, 0.0); err != nil {
			return err
		}
		if err := mb2.WriteName(d, "/a"); err != nil {
			return err
		}
		if err := mb2.WriteName(d, "U1/cut.bin"); err != nil {
			return err
		}
		if err := mb2.WriteBlock(d, mb2.CodeFileData, []byte("abc")); err != nil {
			return err
		}
		if err := mb2.WriteU32(d, 0); err != nil { // a zero length where a block was expected
			return err
		}
		if _, _, err := d.Status(); err != nil {
			return err
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := res.RemoteEnded["U1/cut.bin"]
	if !ok || e.Code != mb2.CodeNoTerminator || e.Bytes != 3 {
		t.Fatalf("RemoteEnded = %+v", res.RemoteEnded)
	}
}

// W10 (K2): the device moves, copies and removes files while it backs up. A
// mark follows the content: a cut file can never lose its flag by a rename.
func TestBackupRemoteEndFollowsMoveCopyAndRemove(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/a", Data: []byte("aaa"), EndRemote: true},
			mb2test.Upload{DeviceName: "/b", Name: "U1/b", Data: []byte("bbb"), EndRemote: true},
			mb2test.Upload{DeviceName: "/c", Name: "U1/c", Data: []byte("ccc"), EndRemote: true},
			mb2test.Upload{DeviceName: "/d", Name: "U1/dir/d", Data: []byte("ddd"), EndRemote: true},
			mb2test.Upload{DeviceName: "/e", Name: "U1/e", Data: []byte("eee")},
		)
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		for _, step := range []struct {
			name string
			msg  []any
		}{
			{"move", []any{"DLMessageMoveItems", map[string]any{"U1/a": "U1/a2", "U1/dir": "U1/dir2"}, 0.0}},
			{"copy", []any{"DLMessageCopyItem", "U1/b", "U1/b2"}},
			{"remove", []any{"DLMessageRemoveItems", []any{"U1/c"}, 0.0}},
			// A complete file moved over a marked one replaces its content: the mark goes with it.
			{"overwrite", []any{"DLMessageMoveItems", map[string]any{"U1/e": "U1/b2"}, 0.0}},
		} {
			if code, _, err := d.Request(step.msg...); err != nil || code != 0 {
				return fmt.Errorf("%s: %d %v", step.name, code, err)
			}
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for k := range res.RemoteEnded {
		got[k] = true
	}
	want := map[string]bool{"U1/a2": true, "U1/b": true, "U1/dir2/d": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RemoteEnded keys = %v, want %v", got, want)
	}
	if res.RemovedEndCount != 2 || len(res.RemovedEnds) != 2 {
		t.Fatalf("removed marks = %d %v, want U1/c and the overwritten U1/b2", res.RemovedEndCount, res.RemovedEnds)
	}
}

// W12 (K4): the detail (message) is capped; the flag set is not.
func TestBackupRemoteEndedKeepsEveryFileAndCapsTheDetail(t *testing.T) {
	const n = mb2.MaxRecordedErrors + 5
	files := make([]mb2test.Upload, n)
	for i := range files {
		files[i] = mb2test.Upload{DeviceName: "/x", Name: fmt.Sprintf("U1/f%04d", i), Data: []byte("x"), EndRemote: true, RemoteMsg: []byte("m")}
	}
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(files...); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	withoutMessage := 0
	for _, e := range res.RemoteEnded {
		if e.Message == "" {
			withoutMessage++
		}
	}
	if len(res.RemoteEnded) != n || res.RemoteEndedDetailOmitted != 5 || withoutMessage != 5 {
		t.Fatalf("recorded %d of %d, detail omitted %d, without message %d", len(res.RemoteEnded), n, res.RemoteEndedDetailOmitted, withoutMessage)
	}
}
