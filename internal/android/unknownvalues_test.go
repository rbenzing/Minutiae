package android

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// hugeFiles is a 5 GiB + 7 listing: a v1 LIST would carry only the low 32 bits.
func hugeFiles(features []string, mtime time.Time) *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device", Features: features,
		Commands: map[string][]byte{"getprop": []byte(getprop), "pm list packages -f": []byte("package:x\n")},
		Files: map[string]adbtest.File{
			"/sdcard/huge.bin": {Data: []byte("x"), ListedSize: 5<<30 + 7, MTime: mtime},
		},
	}
}

func acquireDevice(t *testing.T, dev *adbtest.Device) (*evidence.Case, map[string]evidence.ManifestRecord) {
	t.Helper()
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.AcquireLogical(ctx, c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]evidence.ManifestRecord{}
	for _, r := range m {
		by[r.Source.RemotePath] = r
	}
	return c, by
}

var knownTime = time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)

// W69: a v1 size may be truncated to 32 bits, so it is never recorded.
func TestRemoteSizeIsRecordedOnlyWhenListedOverV2(t *testing.T) {
	_, v1 := acquireDevice(t, hugeFiles([]string{}, knownTime))
	if got := v1["/sdcard/huge.bin"].Source.RemoteSize; got != 0 {
		t.Fatalf("v1 RemoteSize = %d, want unknown (0)", got)
	}
	_, v2 := acquireDevice(t, hugeFiles(nil, knownTime))
	if got := v2["/sdcard/huge.bin"].Source.RemoteSize; got != 5<<30+7 {
		t.Fatalf("v2 RemoteSize = %d, want %d", got, int64(5<<30+7))
	}
}

// W69: the sync version and the fallback reason are evidence.
func TestSyncVersionAndFallbackAreRecorded(t *testing.T) {
	c, _ := acquireDevice(t, hugeFiles([]string{}, knownTime))
	var n int
	for _, e := range auditActions(t, c, "acquire.warning") {
		if e.Details["reason"] == "sync-v1" {
			n++
			s, _ := e.Details["error"].(string)
			if e.Details["op"] != "sync" || !strings.Contains(s, "neither ls_v2 nor stat_v2") {
				t.Fatalf("warning = %v", e.Details)
			}
		}
	}
	stats := auditActions(t, c, "acquire.stats")[0].Details
	if n != 1 || stats["sync_v1"] != true || !strings.Contains(stats["sync_fallback"].(string), "neither ls_v2 nor stat_v2") {
		t.Fatalf("warnings = %d, stats = %v", n, stats)
	}

	c, _ = acquireDevice(t, hugeFiles(nil, knownTime))
	for _, e := range auditActions(t, c, "acquire.warning") {
		if e.Details["reason"] == "sync-v1" {
			t.Fatalf("a v2 session must not warn: %v", e.Details)
		}
	}
	stats = auditActions(t, c, "acquire.stats")[0].Details
	if stats["sync_v1"] != false {
		t.Fatalf("stats = %v", stats)
	}
	if _, ok := stats["sync_fallback"]; ok {
		t.Fatalf("stats = %v: sync_fallback only when sync_v1", stats)
	}
}

// W70: an unknown time (v2 reports one before 1970) is not recorded as a date.
func TestRemoteMTimeIsRecordedOnlyWhenKnown(t *testing.T) {
	_, unknown := acquireDevice(t, hugeFiles(nil, time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)))
	if got := unknown["/sdcard/huge.bin"].Source.RemoteMTime; got != "" {
		t.Fatalf("RemoteMTime = %q, want unknown (omitted)", got)
	}
	_, known := acquireDevice(t, hugeFiles(nil, knownTime))
	if got := known["/sdcard/huge.bin"].Source.RemoteMTime; got != "2023-11-14T22:13:20Z" {
		t.Fatalf("RemoteMTime = %q", got)
	}
}

// W70: Device.List (the android ls backend) leaves an unknown time as the zero time.
func TestListLeavesAnUnknownMTimeZero(t *testing.T) {
	d := findDevice(t, fakeServer(t, hugeFiles(nil, time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC))), "PX1")
	es, err := d.List(context.Background(), "/sdcard")
	if err != nil || len(es) != 1 || !es[0].ModTime.IsZero() {
		t.Fatalf("entries = %+v, err = %v", es, err)
	}
}

// W71: a v2 listing aborted by an oversized entry (a size above int64) is not
// a complete directory: the acquisition fails and writes no completed-stats record.
// (Passed on arrival: the walker already returns the protocol error; Task 12 adds
// the typed list warnings.)
func TestListAbortedByAnOversizedEntryIsNotACompleteDirectory(t *testing.T) {
	dev := hugeFiles(nil, knownTime)
	dev.ExtraDents = map[string][]adbtest.Dent{"/sdcard": {{Name: "evil", Mode: 0o100644, Size: 1 << 63}}}
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("err = %v, want a protocol error", err)
	}
	if n := len(auditActions(t, c, "acquire.stats")); n != 0 {
		t.Fatalf("acquire.stats written for a failed listing: %d", n)
	}
}
