package android

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func reasonCount(t *testing.T, dev *adbtest.Device, reason string) (int, map[string]any) {
	t.Helper()
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.AcquireLogical(ctx, c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range auditActions(t, c, "acquire.warning") {
		if e.Details["reason"] == reason {
			n++
		}
	}
	return n, auditActions(t, c, "acquire.stats")[0].Details
}

func hugeDevice(features []string) *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device", Features: features,
		Commands: map[string][]byte{"getprop": []byte(getprop), "pm list packages -f": []byte("package:x\n")},
		Files: map[string]adbtest.File{
			"/sdcard/huge.bin":  {Data: []byte("x"), ListedSize: 5<<30 + 7},
			"/sdcard/small.txt": {Data: []byte("hello")},
		},
	}
}

func TestLogicalV2RecordsASizeOverFourGiB(t *testing.T) {
	n, stats := reasonCount(t, hugeDevice(nil), "sync-v1")
	if n != 0 || stats["sync_v1"] != false {
		t.Fatalf("a v2 session must not warn: n=%d stats=%v", n, stats)
	}
	recs := acquireFilesFrom(t, hugeDevice(nil))
	if got := recs["/sdcard/huge.bin"].Source.RemoteSize; got != 5<<30+7 {
		t.Fatalf("RemoteSize = %d, want %d", got, int64(5<<30+7))
	}
	if got := recs["/sdcard/small.txt"].Source.RemoteSize; got != 5 {
		t.Fatalf("small RemoteSize = %d", got)
	}
}

// W18: under v2 the declared size is compared with the received bytes. The
// listing claims 5 GiB + 7 for a file that delivers one byte: the artifact is
// kept, flagged incomplete, and a size-mismatch warning is audited. A file
// whose size matches is complete.
func TestLogicalFlagsAFileWhoseBytesDifferFromTheV2Size(t *testing.T) {
	n, _ := reasonCount(t, hugeDevice(nil), "size-mismatch")
	if n != 1 {
		t.Fatalf("size-mismatch warnings = %d, want 1 (huge.bin only)", n)
	}
	recs := acquireFilesFrom(t, hugeDevice(nil))
	huge := recs["/sdcard/huge.bin"]
	if !huge.Incomplete || huge.Size != 1 || !strings.Contains(huge.Error, "received 1 bytes, listed") {
		t.Fatalf("huge.bin = %+v", huge)
	}
	if small := recs["/sdcard/small.txt"]; small.Incomplete || small.Size != 5 {
		t.Fatalf("small.txt = %+v", small)
	}
}

func TestLogicalV1DoesNotCompareSizes(t *testing.T) {
	if n, _ := reasonCount(t, hugeDevice([]string{"shell_v2"}), "size-mismatch"); n != 0 {
		t.Fatalf("a v1 size (possibly truncated) must not be compared: %d warnings", n)
	}
}

// W15: only a v2 listing makes the size trustworthy; the version is the
// listing session's, not the session current when the file is pulled.
func TestRemoteSourceTrustsOnlyAV2Listing(t *testing.T) {
	v1 := adb.SyncEntry{Name: "a", Mode: 0o100644, Size: 7, MTime: time.Unix(1700000000, 0).UTC()}
	v2 := v1
	v2.V2 = true
	if got := remoteSource("PX1", "/sdcard/a", v1).RemoteSize; got != 0 {
		t.Errorf("v1 listing: RemoteSize = %d, want it omitted", got)
	}
	if got := remoteSource("PX1", "/sdcard/a", v2).RemoteSize; got != 7 {
		t.Errorf("v2 listing: RemoteSize = %d, want 7", got)
	}
	unknown := v2
	unknown.MTime, unknown.MTimeUnknown = time.Time{}, true
	if got := remoteSource("PX1", "/sdcard/a", unknown).RemoteMTime; got != "" {
		t.Errorf("unknown mtime recorded as %q", got)
	}
	if got := remoteSource("PX1", "/sdcard/a", v2).RemoteMTime; got != "2023-11-14T22:13:20Z" {
		t.Errorf("mtime = %q", got)
	}
}

func TestLogicalV1FallbackStatesItsLimitationAndOmitsSizes(t *testing.T) {
	legacy := hugeDevice([]string{"shell_v2"})
	n, stats := reasonCount(t, legacy, "sync-v1")
	if n != 1 || stats["sync_v1"] != true || !strings.Contains(stats["sync_fallback"].(string), "ls_v2") {
		t.Fatalf("v1 warning count = %d, stats = %v", n, stats)
	}
	recs := acquireFilesFrom(t, hugeDevice([]string{"shell_v2"}))
	for _, p := range []string{"/sdcard/huge.bin", "/sdcard/small.txt"} {
		if got := recs[p].Source.RemoteSize; got != 0 {
			t.Errorf("%s: v1 RemoteSize = %d, want it omitted (unknown): a v1 size may be truncated", p, got)
		}
	}
	// The data is still captured in full.
	if recs["/sdcard/small.txt"].Size != 5 || recs["/sdcard/small.txt"].Incomplete {
		t.Fatalf("small = %+v", recs["/sdcard/small.txt"])
	}
}

func TestLogicalWarnsOnUnreadableDirectoriesInBothVersions(t *testing.T) {
	for name, features := range map[string][]string{"v2": nil, "v1": {"shell_v2"}} {
		dev := hugeDevice(features)
		dev.Files["/sdcard/locked/x"] = adbtest.File{Data: []byte("x")}
		dev.ListErrno = map[string]uint32{"/sdcard/locked": 13}
		want := "list-errno"
		if name == "v1" {
			want = "list-unknown"
		}
		n, stats := reasonCount(t, dev, want)
		if n != 1 || fmt.Sprint(stats["skipped"]) == "0" {
			t.Errorf("%s: %q warnings = %d, stats = %v", name, want, n, stats)
		}
	}
}

func TestLogicalDoesNotWarnOnAGenuinelyEmptyDirectory(t *testing.T) {
	for name, features := range map[string][]string{"v2": nil, "v1": {"shell_v2"}} {
		dev := hugeDevice(features)
		dev.EmptyDirs = map[string]bool{"/sdcard/empty": true}
		for _, r := range []string{"list-errno", "list-unknown"} {
			if n, _ := reasonCount(t, dev, r); n != 0 {
				t.Errorf("%s: an empty directory warned %q", name, r)
			}
		}
	}
}

func TestLogicalWarnsOnEntriesTheDeviceCouldNotStat(t *testing.T) {
	dev := hugeDevice(nil)
	dev.EntryErrno = map[string]uint32{"/sdcard/small.txt": 5}
	n, _ := reasonCount(t, dev, "entry-errno")
	if n != 1 {
		t.Fatalf("entry-errno warnings = %d", n)
	}
	recs := acquireFilesLenient(t, dev)
	if _, ok := recs["/sdcard/small.txt"]; ok {
		t.Error("an entry the device could not stat was pulled as if listed")
	}
	if _, ok := recs["/sdcard/huge.bin"]; !ok {
		t.Error("the other entries must still be pulled")
	}
}

func TestDeviceListReturnsAnErrorForAnUnreadableDirectory(t *testing.T) {
	dev := hugeDevice(nil)
	dev.Files["/sdcard/locked/x"] = adbtest.File{Data: []byte("x")}
	dev.ListErrno = map[string]uint32{"/sdcard/locked": 13}
	d := findDevice(t, fakeServer(t, dev), "PX1")
	es, err := d.List(context.Background(), "/sdcard/locked")
	if err == nil || len(es) != 0 {
		t.Fatalf("List = %v, %v: an unreadable directory must not list as empty", es, err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
	if es, err := d.List(context.Background(), "/sdcard"); err != nil || len(es) != 3 {
		t.Fatalf("readable List = %v, %v", es, err)
	}
}

// W17: the readable entries are returned together with a typed partial error.
func TestDeviceListKeepsReadableEntriesOnAPartialListing(t *testing.T) {
	dev := hugeDevice(nil)
	dev.EntryErrno = map[string]uint32{"/sdcard/small.txt": 5}
	d := findDevice(t, fakeServer(t, dev), "PX1")
	es, err := d.List(context.Background(), "/sdcard")
	if !errors.Is(err, device.ErrPartialListing) || len(es) != 1 || es[0].Name != "huge.bin" {
		t.Fatalf("List = %+v, %v", es, err)
	}
}

func TestDeviceListHonoursTheWalkLimit(t *testing.T) {
	d := findDevice(t, fakeServer(t, hugeDevice(nil)), "PX1")
	d.SetWalkLimits(WalkLimits{MaxEntriesPerDir: 1})
	es, err := d.List(context.Background(), "/sdcard")
	if !errors.Is(err, device.ErrPartialListing) || !errors.Is(err, adb.ErrListLimit) || len(es) != 1 {
		t.Fatalf("List = %+v, %v", es, err)
	}
}

// W16: a time the device did not give is unknown, not 0001-01-01.
func TestDeviceListShowsAnUnknownTimeAsNull(t *testing.T) {
	dev := hugeDevice(nil) // the files have no MTime: v2 reports a negative time
	d := findDevice(t, fakeServer(t, dev), "PX1")
	es, err := d.List(context.Background(), "/sdcard")
	if err != nil || len(es) != 2 {
		t.Fatalf("List = %+v, %v", es, err)
	}
	for _, e := range es {
		if !e.ModTime.IsZero() {
			t.Errorf("%s: ModTime = %v, want the zero time (unknown)", e.Name, e.ModTime)
		}
		b, err := json.Marshal(e)
		if err != nil || !strings.Contains(string(b), `"mod_time":null`) {
			t.Errorf("%s: json = %s, %v", e.Name, b, err)
		}
	}
}

// acquireFilesFrom runs a logical acquisition of dev, requires success and
// returns the manifest records keyed by Source.RemotePath.
func acquireFilesFrom(t *testing.T, dev *adbtest.Device) map[string]evidence.ManifestRecord {
	t.Helper()
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.AcquireLogical(ctx, c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	return pulled(t, c)
}

func acquireFilesLenient(t *testing.T, dev *adbtest.Device) map[string]evidence.ManifestRecord {
	t.Helper()
	return acquireFilesFrom(t, dev)
}

// W75: an unreadable directory is an error in v1 as well as v2 (a v1 LIST
// of it carries no entry frame at all), never an empty listing; a really empty
// directory still lists as empty.
func TestDeviceListUnreadableIsAnErrorInBothVersions(t *testing.T) {
	for name, features := range map[string][]string{"v2": nil, "v1": {"shell_v2"}} {
		dev := hugeDevice(features)
		dev.Files["/sdcard/locked/x"] = adbtest.File{Data: []byte("x")}
		dev.ListErrno = map[string]uint32{"/sdcard/locked": 13}
		dev.EmptyDirs = map[string]bool{"/sdcard/empty": true}
		d := findDevice(t, fakeServer(t, dev), "PX1")
		es, err := d.List(context.Background(), "/sdcard/locked")
		if err == nil || len(es) != 0 {
			t.Errorf("%s: List(locked) = %v, %v: must be an error, not an empty listing", name, es, err)
		}
		if es, err := d.List(context.Background(), "/sdcard/empty"); err != nil || len(es) != 0 {
			t.Errorf("%s: List(empty) = %v, %v: a really empty directory is not an error", name, es, err)
		}
	}
}

// W73: a walk that fails after pulling files still writes acquire.stats, with
// incomplete:true and the error, before acquire.error; the files already
// pulled stay in the case.
func TestLogicalWalkErrorAfterPullingWritesIncompleteStats(t *testing.T) {
	dev := hugeDevice(nil)
	dev.Files["/sdcard/ok/a.txt"] = adbtest.File{Data: []byte("a")}
	dev.ExtraDents = map[string][]adbtest.Dent{"/sdcard": {{Name: "evil", Mode: 0o100644, Size: 1 << 63}}}
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{Roots: []string{"/sdcard/ok", "/sdcard"}}, nil)
	if err == nil {
		t.Fatal("want the protocol error")
	}
	st := auditActions(t, c, "acquire.stats")
	if len(st) != 1 || st[0].Details["incomplete"] != true || st[0].Details["files"] == nil ||
		!strings.Contains(fmt.Sprint(st[0].Details["error"]), "protocol") {
		t.Fatalf("stats = %+v", st)
	}
	entries, rerr := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	iStats, iErr := -1, -1
	for i, e := range entries {
		switch e.Action {
		case "acquire.stats":
			iStats = i
		case "acquire.error":
			iErr = i
		}
	}
	if iStats < 0 || iErr < 0 || iStats > iErr {
		t.Fatalf("acquire.stats (%d) must come before acquire.error (%d)", iStats, iErr)
	}
	if _, ok := pulled(t, c)["/sdcard/ok/a.txt"]; !ok {
		t.Fatal("the file pulled before the failure must be kept")
	}
}

// W77: the run's acquire.stats is incomplete when any artifact of the run is
// (here a v2 size mismatch), not only after a limit hit, and counts them.
func TestLogicalStatsAreIncompleteWhenAnArtifactIsFlagged(t *testing.T) {
	_, stats := reasonCount(t, hugeDevice(nil), "size-mismatch")
	if stats["incomplete"] != true || fmt.Sprint(stats["size_mismatch"]) != "1" {
		t.Fatalf("stats = %v, want incomplete:true and size_mismatch:1", stats)
	}
	_, clean := reasonCount(t, &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{"getprop": []byte(getprop), "pm list packages -f": []byte("package:x\n")},
		Files:    map[string]adbtest.File{"/sdcard/small.txt": {Data: []byte("hello")}},
	}, "size-mismatch")
	if clean["incomplete"] != false || fmt.Sprint(clean["size_mismatch"]) != "0" {
		t.Fatalf("clean run stats = %v, want incomplete:false and size_mismatch:0", clean)
	}
}
