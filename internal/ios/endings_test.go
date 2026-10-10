package ios_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/ios/manifestdb"
	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
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
	if !cut.Incomplete || !strings.Contains(cut.Error, "gives no size consistent with the bytes received") || cut.Size != 3 {
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

func manifestWith(t *testing.T, rows map[string]uint64) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tab := b.CreateTable("Files", "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)")
	i := int64(0)
	for id, size := range rows {
		i++
		blob, err := plist.Marshal(map[string]any{
			"$archiver": "NSKeyedArchiver", "$version": uint64(100000),
			"$top":     map[string]any{"root": plist.UID(1)},
			"$objects": []any{"$null", map[string]any{"Size": size, "$class": plist.UID(2)}, map[string]any{"$classname": "MBFile"}},
		}, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		tab.Insert(i, id, "d", "p", int64(1), blob)
	}
	return b.Bytes()
}

func TestBackupConfirmsAFileEndedByADeviceErrorBlockWithManifestSize(t *testing.T) {
	good := "ab" + strings.Repeat("c", 38)
	bad := "cd" + strings.Repeat("e", 38)
	manifest := manifestWith(t, map[string]uint64{good: 3, bad: 99})
	b := fakeIPhone()
	b.Script = uploadExtraThenFinish(0,
		mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		mb2test.Upload{DeviceName: "/m", Name: "U1/Manifest.db", Data: manifest},
		mb2test.Upload{DeviceName: "/g", Name: "U1/ab/" + good, Data: []byte("abc"), EndRemote: true},
		mb2test.Upload{DeviceName: "/h", Name: "U1/cd/" + bad, Data: []byte("abc"), EndRemote: true},
	)
	c := newCase(t)
	err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "differs from the backup's Manifest.db") {
		t.Fatalf("a proven size mismatch must fail the acquisition: %v", err)
	}
	<-b.ScriptErr
	recs := recordByRemote(t, c)
	if r := recs["U1/ab/"+good]; r.Incomplete || r.Size != 3 {
		t.Errorf("confirmed file: %+v", r)
	}
	if r := recs["U1/cd/"+bad]; !r.Incomplete || !strings.Contains(r.Error, "another size") {
		t.Errorf("mismatching file: %+v", r)
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	for _, want := range []string{`"end_confirmed":1`, `"end_mismatch":1`, `"end_unconfirmed":0`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("%s not audited:\n%s", want, audit)
		}
	}
}

// A manifest that itself ended with a device error block is not an oracle.
func TestBackupManifestItselfCutIsNotTrusted(t *testing.T) {
	good := "ab" + strings.Repeat("c", 38)
	manifest := manifestWith(t, map[string]uint64{good: 3})
	b := fakeIPhone()
	b.Script = uploadExtraThenFinish(0,
		mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		mb2test.Upload{DeviceName: "/g", Name: "U1/ab/" + good, Data: []byte("abc"), EndRemote: true},
		// Re-sent with a device error block after data: the base script's Manifest.db is replaced.
		mb2test.Upload{DeviceName: "/m", Name: "U1/Manifest.db", Data: manifest, EndRemote: true},
	)
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	recs := recordByRemote(t, c)
	if r := recs["U1/ab/"+good]; !r.Incomplete {
		t.Errorf("a manifest that itself ended with a device error block must not confirm anything: %+v", r)
	}
}

// W13: a backup without a file that ended with an error block never reads Manifest.db.
func TestBackupDoesNotReadTheManifestWithoutAnErrorEndedFile(t *testing.T) {
	calls := 0
	t.Cleanup(ios.SetReadManifest(func(ctx context.Context, r io.ReaderAt, size int64, want func(string) bool) (*manifestdb.Declared, error) {
		calls++
		return manifestdb.Read(ctx, r, size, want)
	}))
	b := fakeIPhone()
	b.Script = uploadThenFinish(0, statusPlist(t, "finished"))
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	if calls != 0 {
		t.Fatalf("the manifest was read %d times for a backup with no error-ended file", calls)
	}
}

// W42: Manifest.db itself cut by the transport, with a truncated copy that
// still parses (here the whole page-aligned manifest, sent without an end
// marker), is not an oracle: every error-ended file stays unconfirmed and the
// cause names the cut.
func TestBackupManifestCutByTransportIsNotTrustedEvenWhenItParses(t *testing.T) {
	good := "ab" + strings.Repeat("c", 38)
	other := "cd" + strings.Repeat("e", 38)
	manifest := manifestWith(t, map[string]uint64{good: 3, other: 3})
	if len(manifest)%4096 != 0 {
		t.Fatalf("test manifest is not page-aligned: %d bytes", len(manifest))
	}
	b := fakeIPhone()
	b.Script = func(d *mb2test.Device) error {
		if err := d.Handshake(); err != nil {
			return err
		}
		if _, err := d.ExpectBackupRequest(); err != nil {
			return err
		}
		if _, err := d.UploadFiles(
			mb2test.Upload{DeviceName: "/a", Name: "U1/Info.plist", Data: []byte("info")},
			mb2test.Upload{DeviceName: "/g", Name: "U1/ab/" + good, Data: []byte("abc"), EndRemote: true},
			mb2test.Upload{DeviceName: "/h", Name: "U1/cd/" + other, Data: []byte("abc"), EndRemote: true},
		); err != nil {
			return err
		}
		return d.UploadPartial("/m", "U1/Manifest.db", manifest) // then the device disconnects
	}
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err == nil {
		t.Fatal("an interrupted backup reported success")
	}
	<-b.ScriptErr
	recs := recordByRemote(t, c)
	if r := recs["U1/Manifest.db"]; !r.Incomplete {
		t.Fatalf("the cut manifest must be flagged incomplete: %+v", r)
	}
	for _, id := range []string{"U1/ab/" + good, "U1/cd/" + other} {
		r := recs[id]
		if !r.Incomplete || !strings.Contains(r.Error, "cut off") {
			t.Errorf("%s: a manifest cut by the transport must not confirm anything: %+v", id, r)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	for _, want := range []string{`"end_confirmed":0`, `"end_unconfirmed":2`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("%s not audited:\n%s", want, audit)
		}
	}
}

type ctxKey struct{}

// W44: the acquisition's context reaches the manifest reader.
func TestBackupPassesItsContextToTheManifestReader(t *testing.T) {
	good := "ab" + strings.Repeat("c", 38)
	var seen any
	t.Cleanup(ios.SetReadManifest(func(ctx context.Context, r io.ReaderAt, size int64, want func(string) bool) (*manifestdb.Declared, error) {
		seen = ctx.Value(ctxKey{})
		return manifestdb.Read(ctx, r, size, want)
	}))
	b := fakeIPhone()
	b.Script = uploadExtraThenFinish(0,
		mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		mb2test.Upload{DeviceName: "/m", Name: "U1/Manifest.db", Data: manifestWith(t, map[string]uint64{good: 3})},
		mb2test.Upload{DeviceName: "/g", Name: "U1/ab/" + good, Data: []byte("abc"), EndRemote: true},
	)
	c := newCase(t)
	ctx := context.WithValue(context.Background(), ctxKey{}, "acquisition")
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(ctx, c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	if seen != "acquisition" {
		t.Fatalf("the manifest reader got a context unrelated to the acquisition's (%v)", seen)
	}
}

// W45: what the examiner reads never says more than the evidence supports.
func TestBackupConfirmationIsLabelledAsConsistencyWithTheManifest(t *testing.T) {
	good := "ab" + strings.Repeat("c", 38)
	b := fakeIPhone()
	b.Script = uploadExtraThenFinish(0,
		mb2test.Upload{DeviceName: "/c", Name: "U1/Status.plist", Data: statusPlist(t, "finished")},
		mb2test.Upload{DeviceName: "/m", Name: "U1/Manifest.db", Data: manifestWith(t, map[string]uint64{good: 3})},
		mb2test.Upload{DeviceName: "/g", Name: "U1/ab/" + good, Data: []byte("abc"), EndRemote: true},
	)
	c := newCase(t)
	if err := first(t, b).(device.LogicalAcquirer).AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	<-b.ScriptErr
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"end_confirmed_basis":"consistent with the backup's Manifest.db"`) {
		t.Fatalf("the audited stats do not say what confirmed means:\n%s", audit)
	}
}
