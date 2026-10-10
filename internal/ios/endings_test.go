package ios_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

func recordByRemote(t *testing.T, c *evidence.Case) map[string]evidence.ManifestRecord {
	t.Helper()
	m, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]evidence.ManifestRecord{}
	for _, r := range m {
		out[r.Source.RemotePath] = r
	}
	return out
}

func TestBackupFlagsAFileEndedByADeviceErrorBlockAsIncomplete(t *testing.T) {
	b := fakeIPhone()
	b.Script = uploadExtraThenFinish(0,
		mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		mb2test.Upload{DeviceName: "/d", Name: "U1/cut.bin", Data: []byte("abc"), EndRemote: true, RemoteMsg: []byte("read failed")},
	)
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatalf("an unconfirmed end is flagged, it does not fail the acquisition: %v", err)
	}
	if err := <-b.ScriptErr; err != nil {
		t.Fatalf("script: %v", err)
	}
	recs := recordByRemote(t, c)
	cut := recs["U1/cut.bin"]
	if !cut.Incomplete || !strings.Contains(cut.Error, "no recorded size confirms the file is complete") || cut.Size != 3 {
		t.Fatalf("cut.bin record = %+v", cut)
	}
	for _, p := range []string{"U1/Info.plist", "U1/Manifest.db", "U1/Status.plist"} {
		if r, ok := recs[p]; !ok || r.Incomplete {
			t.Errorf("%s: %+v ok=%v", p, r, ok)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	for _, want := range []string{`"end_unconfirmed":1`, `"end_confirmed":0`, `"end_mismatch":0`, `"reason":"end-unconfirmed"`, `\"U1/cut.bin\": code 0x0b, 3 bytes`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("%s not audited:\n%s", want, audit)
		}
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

func TestBackupWithoutDeviceErrorBlocksAfterDataHasNoEndingWarning(t *testing.T) {
	b := fakeIPhone()
	b.Script = uploadThenFinish(0, statusPlist(t, "finished"))
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if strings.Contains(string(audit), `"reason":"end-unconfirmed"`) || !strings.Contains(string(audit), `"end_unconfirmed":0`) {
		t.Fatalf("unexpected ending warning:\n%s", audit)
	}
	for p, r := range recordByRemote(t, c) {
		if r.Incomplete {
			t.Errorf("%s flagged incomplete without cause", p)
		}
	}
}

// W10: the device moves a cut file after uploading it; the artifact under its
// final path is the one that must carry the flag.
func TestBackupFlagFollowsAMovedFile(t *testing.T) {
	b := fakeIPhone()
	b.Script = func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/tmp.bin", Data: []byte("abc"), EndRemote: true},
			mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		if code, _, err := d.Request("DLMessageMoveItems", map[string]any{"U1/tmp.bin": "U1/final.bin"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("move: %d %v", code, err)
		}
		return d.Finish(0)
	}
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	recs := recordByRemote(t, c)
	if r, ok := recs["U1/final.bin"]; !ok || !r.Incomplete {
		t.Fatalf("the moved file must keep its flag: %+v ok=%v", r, ok)
	}
	if _, ok := recs["U1/tmp.bin"]; ok {
		t.Error("the old path no longer exists")
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"cut_missing":0`) || !strings.Contains(string(audit), `"end_unconfirmed":1`) {
		t.Fatalf("audit:\n%s", audit)
	}
}

// W10: a removed file's mark is not lost; it stays in the audit record.
func TestBackupRemovedFilesMarkStaysInTheAuditRecord(t *testing.T) {
	b := fakeIPhone()
	b.Script = func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/gone.bin", Data: []byte("abc"), EndRemote: true},
			mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		if code, _, err := d.Request("DLMessageRemoveItems", []any{"U1/gone.bin"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("remove: %d %v", code, err)
		}
		return d.Finish(0)
	}
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	for _, want := range []string{`"end_removed_count":1`, `\"U1/gone.bin\": code 0x0b, 3 bytes`, `"end_unconfirmed":0`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("%s not audited:\n%s", want, audit)
		}
	}
}

// W11: data then a zero length with no terminal block is flagged like an error block.
func TestBackupZeroLengthAfterDataIsFlaggedIncomplete(t *testing.T) {
	b := fakeIPhone()
	b.Script = func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if err := d.Send("DLMessageUploadFiles", map[string]any{}, 0.0); err != nil {
			return err
		}
		for _, step := range []error{
			mb2.WriteName(d, "/a"), mb2.WriteName(d, "U1/cut.bin"),
			mb2.WriteBlock(d, mb2.CodeFileData, []byte("abc")), mb2.WriteU32(d, 0),
		} {
			if step != nil {
				return step
			}
		}
		if _, _, err := d.Status(); err != nil {
			return err
		}
		if code, err := d.UploadFiles(mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")}); err != nil || code != 0 {
			return fmt.Errorf("upload: %d %v", code, err)
		}
		return d.Finish(0)
	}
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	if r := recordByRemote(t, c)["U1/cut.bin"]; !r.Incomplete || r.Size != 3 {
		t.Fatalf("cut.bin = %+v", r)
	}
}
