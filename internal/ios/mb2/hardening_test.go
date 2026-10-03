package mb2_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

// nestedPlist builds a binary plist whose object i is an array of fan
// references to object i-1 (object 0 is the integer 0). The file is tiny but
// expands to fan^n nodes when references are resolved.
func nestedPlist(n, fan int) []byte {
	b := []byte("bplist00")
	offsets := []byte{byte(len(b))}
	b = append(b, 0x10, 0x00)
	for i := 1; i <= n; i++ {
		offsets = append(offsets, byte(len(b)))
		b = append(b, 0xA0|byte(fan))
		for range fan {
			b = append(b, byte(i-1))
		}
	}
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(n+1))
	binary.BigEndian.PutUint64(trailer[16:], uint64(n))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

func frame(b []byte) *bytes.Buffer {
	return bytes.NewBuffer(binary.BigEndian.AppendUint32(nil, uint32(len(b))))
}

func recv(b []byte) ([]any, error) {
	buf := frame(b)
	buf.Write(b)
	return mb2.NewCodec(buf).Recv()
}

func TestRecvRejectsPlistBombs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n, fan  int
		wantErr bool
	}{
		{"exp21", 21, 2, true},
		{"exp40", 40, 2, true},
		{"deep70", 70, 1, true},
		{"ok-exp10", 10, 2, false},
		{"ok-deep30", 30, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			msg, err := recv(nestedPlist(tc.n, tc.fan))
			if time.Since(start) > time.Second {
				t.Fatalf("decode took %v", time.Since(start))
			}
			if tc.wantErr && err == nil {
				t.Fatalf("decoded a plist bomb (%d top-level elements)", len(msg))
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecvRejectsNonBinaryPlist(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?><plist version="1.0"><array><string>x</string></array></plist>`)
	if _, err := recv(xml); err == nil {
		t.Fatal("XML plist accepted")
	}
}

func TestBackupTruncatesHugeRemoteError(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/x", Name: "U1/x", EndRemote: true, RemoteMsg: bytes.Repeat([]byte("E"), 10<<20)},
			mb2test.Upload{DeviceName: "/y", Name: "U1/y", Data: []byte("y")},
		)
		if err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RemoteErrors) != 1 || len(res.RemoteErrors[0]) > 4096+16 {
		t.Fatalf("remote errors not bounded: %d entries, first %d bytes", len(res.RemoteErrors), len(res.RemoteErrors[0]))
	}
	if res.BytesReceived != 1 {
		t.Fatalf("stream out of sync, bytes = %d", res.BytesReceived)
	}
}

func TestBackupDisconnectBeforeCompletionIsError(t *testing.T) {
	_, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		return d.Send("DLMessageDisconnect", mb2.EmptyParameter)
	})
	if err == nil || !strings.Contains(err.Error(), "disconnected before completion") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupPurgeDiskSpaceUnsupported(t *testing.T) {
	_, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, _, err := d.Request("DLMessagePurgeDiskSpace", 100); err != nil || code != -1 {
			return fmt.Errorf("purge: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackupDownloadSkipsEmptyNames(t *testing.T) {
	_, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/a", Data: []byte("data")}); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		got, code, err := d.DownloadFiles("", "U1/a")
		if err != nil || code != 0 || string(got["U1/a"]) != "data" || len(got) != 1 {
			return fmt.Errorf("download: %q %d %v", got, code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackupZeroBlockEndsUpload(t *testing.T) {
	dir, _, err := runBackup(t, func(d *mb2test.Device) error {
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
		if err := mb2.WriteName(d, "U1/a"); err != nil {
			return err
		}
		if err := mb2.WriteU32(d, 0); err != nil { // zero length where a block is expected
			return err
		}
		if code, _, err := d.Status(); err != nil || code != 0 {
			return fmt.Errorf("status: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "U1", "a")); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRefusesStagingRootAndSelfCopy(t *testing.T) {
	dir, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/dir/f", Data: []byte("f")}); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		for _, p := range []string{".", "U1/..", "./"} {
			if code, _, err := d.Request("DLMessageRemoveItems", []any{p}, 0.0); err != nil || code == 0 {
				return fmt.Errorf("remove %q: code %d %v", p, code, err)
			}
		}
		for _, to := range []string{"U1/dir", "U1/dir/sub"} {
			if code, _, err := d.Request("DLMessageCopyItem", "U1/dir", to); err != nil || code == 0 {
				return fmt.Errorf("copy to %q: code %d %v", to, code, err)
			}
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "U1", "dir", "f")); err != nil {
		t.Fatalf("staging content lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "U1", "dir", "sub")); !os.IsNotExist(err) {
		t.Fatalf("self-copy created U1/dir/sub: %v", err)
	}
}

func TestHandshakeSendsOwnVersionAndChecksReply(t *testing.T) {
	_, _, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Send("DLMessageVersionExchange", 400, 0); err != nil {
			return err
		}
		msg, err := d.Recv()
		if err != nil {
			return err
		}
		if v, _ := mb2.ToInt(msg[len(msg)-1]); mb2.Name(msg) != "DLMessageVersionExchange" || v != 300 {
			return fmt.Errorf("version reply = %v", msg)
		}
		if err := d.Send("DLMessageDeviceReady"); err != nil {
			return err
		}
		if _, err := d.Recv(); err != nil {
			return err
		}
		return d.Send("DLMessageProcessMessage", map[string]any{"ErrorCode": 0, "MessageName": "Bogus"})
	})
	if err == nil || !strings.Contains(err.Error(), "Hello") {
		t.Fatalf("err = %v", err)
	}
}

// rawPlist assembles a binary plist from obj (placed at offset 8) and a
// one-byte offset table; object 0 is the top object.
func rawPlist(obj, offsets []byte) []byte {
	b := append([]byte("bplist00"), obj...)
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 3
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(offsets)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

// bigCount returns a marker for type nibble typ followed by an 8-byte length.
func bigCount(typ byte, n uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{typ<<4 | 0x0f, 0x13}, n)
}

func TestRecvRejectsOverflowingCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  []byte
	}{
		{"dict-2^63", bigCount(0xD, 1<<63)},
		{"dict-2^63+1", bigCount(0xD, 1<<63+1)},
		{"utf16-2^63+1", bigCount(0x6, 1<<63+1)},
		{"array-2^63", bigCount(0xA, 1<<63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg, err := recv(rawPlist(tc.obj, []byte{8})); err == nil {
				t.Fatalf("accepted: %v", msg)
			}
		})
	}
}

func TestRecvRejectsHugeObjectTable(t *testing.T) {
	offsets := bytes.Repeat([]byte{8}, 1<<20+1) // one more object than the expansion cap
	offsets[0] = 10                             // top object: [1] (refSize is 3)
	if msg, err := recv(rawPlist([]byte{0x10, 0x00, 0xA1, 0, 0, 1}, offsets)); err == nil {
		t.Fatalf("accepted: %v", msg)
	}
}

func TestBackupFileInterruptedMidTransferIsIncomplete(t *testing.T) {
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/a", Name: "U1/whole", Data: []byte("whole")}); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.UploadPartial("/b", "U1/sub/../partial", []byte("part"))
	})
	if err == nil {
		t.Fatal("interrupted transfer reported success")
	}
	if len(res.Incomplete) != 1 || res.Incomplete[0] != "U1/partial" {
		t.Fatalf("incomplete = %q", res.Incomplete)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "U1", "partial")); string(b) != "part" {
		t.Fatalf("partial bytes = %q", b)
	}
}
