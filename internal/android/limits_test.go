package android

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func filesDevice(paths ...string) *adbtest.Device {
	files := map[string]adbtest.File{}
	for _, p := range paths {
		files[p] = adbtest.File{Data: []byte("x")}
	}
	return &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{"getprop": []byte(getprop), "pm list packages -f": []byte("package:x\n")},
		Files:    files,
	}
}

func acquireLimited(t *testing.T, dev *adbtest.Device, lim WalkLimits) (*evidence.Case, error) {
	t.Helper()
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	d.SetWalkLimits(lim)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c, d.AcquireLogical(ctx, c, device.LogicalOptions{}, nil)
}

func pulled(t *testing.T, c *evidence.Case) map[string]evidence.ManifestRecord {
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

func limitWarnings(t *testing.T, c *evidence.Case, reason string) int {
	t.Helper()
	n := 0
	for _, e := range auditActions(t, c, "acquire.warning") {
		if e.Details["op"] == "limit" && e.Details["reason"] == reason {
			n++
		}
	}
	return n
}

func requireIncompleteStats(t *testing.T, c *evidence.Case, reason string, hits int) {
	t.Helper()
	st := auditActions(t, c, "acquire.stats")
	if len(st) != 1 || st[0].Details["incomplete"] != true {
		t.Fatalf("stats = %+v", st)
	}
	lh, _ := st[0].Details["limit_hits"].(map[string]any)
	if fmt.Sprint(lh[reason]) != fmt.Sprint(hits) {
		t.Fatalf("limit_hits = %v, want %s: %v", lh, reason, hits)
	}
}

func TestLogicalCapsEntriesPerDirectory(t *testing.T) {
	var paths []string
	for i := 1; i <= 6; i++ {
		paths = append(paths, fmt.Sprintf("/sdcard/d/f%d.txt", i))
	}
	c, err := acquireLimited(t, filesDevice(paths...), WalkLimits{MaxEntriesPerDir: 5})
	if !errors.Is(err, ErrAcquisitionLimited) {
		t.Fatalf("err = %v, want ErrAcquisitionLimited", err)
	}
	recs := pulled(t, c)
	for i := 1; i <= 5; i++ {
		if r, ok := recs[fmt.Sprintf("/sdcard/d/f%d.txt", i)]; !ok || r.Incomplete {
			t.Errorf("f%d.txt: %+v ok=%v (the entries read before the cap are still pulled)", i, r, ok)
		}
	}
	if _, ok := recs["/sdcard/d/f6.txt"]; ok {
		t.Error("the entry past the cap was pulled")
	}
	if n := limitWarnings(t, c, "entries-per-dir"); n != 1 {
		t.Fatalf("entries-per-dir warnings = %d", n)
	}
	requireIncompleteStats(t, c, "entries-per-dir", 1)
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

func TestLogicalCapsDepth(t *testing.T) {
	c, err := acquireLimited(t, filesDevice("/sdcard/top.txt", "/sdcard/a/b/c/d/e/f.txt"), WalkLimits{MaxDepth: 3})
	if !errors.Is(err, ErrAcquisitionLimited) {
		t.Fatalf("err = %v", err)
	}
	recs := pulled(t, c)
	if _, ok := recs["/sdcard/top.txt"]; !ok {
		t.Error("a file above the cap was not pulled")
	}
	if _, ok := recs["/sdcard/a/b/c/d/e/f.txt"]; ok {
		t.Error("a file below the depth cap was pulled")
	}
	if n := limitWarnings(t, c, "depth"); n != 1 {
		t.Fatalf("depth warnings = %d", n)
	}
	requireIncompleteStats(t, c, "depth", 1)
}

func TestLogicalCapsTotalEntries(t *testing.T) {
	c, err := acquireLimited(t, filesDevice(
		"/sdcard/x/1.txt", "/sdcard/x/2.txt", "/sdcard/x/3.txt",
		"/sdcard/y/1.txt", "/sdcard/y/2.txt", "/sdcard/y/3.txt",
	), WalkLimits{MaxTotalEntries: 5})
	if !errors.Is(err, ErrAcquisitionLimited) {
		t.Fatalf("err = %v", err)
	}
	recs := pulled(t, c)
	for _, p := range []string{"/sdcard/x/1.txt", "/sdcard/x/2.txt", "/sdcard/x/3.txt"} {
		if _, ok := recs[p]; !ok {
			t.Errorf("%s not pulled before the total cap", p)
		}
	}
	for _, p := range []string{"/sdcard/y/1.txt", "/sdcard/y/2.txt", "/sdcard/y/3.txt"} {
		if _, ok := recs[p]; ok {
			t.Errorf("%s pulled after the total cap", p)
		}
	}
	if n := limitWarnings(t, c, "entries-total"); n != 1 {
		t.Fatalf("entries-total warnings = %d", n)
	}
	requireIncompleteStats(t, c, "entries-total", 1)
}

// W14: the walker remembers every pulled path, so path text is budgeted too.
func TestLogicalCapsPathBytes(t *testing.T) {
	c, err := acquireLimited(t, filesDevice(
		"/sdcard/x/1.txt", "/sdcard/x/2.txt", "/sdcard/x/3.txt",
		"/sdcard/y/1.txt", "/sdcard/y/2.txt", "/sdcard/y/3.txt",
	), WalkLimits{MaxPathBytes: 50})
	if !errors.Is(err, ErrAcquisitionLimited) {
		t.Fatalf("err = %v", err)
	}
	recs := pulled(t, c)
	for _, p := range []string{"/sdcard/x/1.txt", "/sdcard/x/2.txt"} {
		if _, ok := recs[p]; !ok {
			t.Errorf("%s not pulled before the path budget ran out", p)
		}
	}
	for _, p := range []string{"/sdcard/x/3.txt", "/sdcard/y/1.txt", "/sdcard/y/2.txt", "/sdcard/y/3.txt"} {
		if _, ok := recs[p]; ok {
			t.Errorf("%s pulled after the path budget ran out", p)
		}
	}
	if n := limitWarnings(t, c, "path-bytes"); n != 1 {
		t.Fatalf("path-bytes warnings = %d", n)
	}
	requireIncompleteStats(t, c, "path-bytes", 1)
}

func TestLogicalWithinTheDefaultLimitsHasNoLimitWarning(t *testing.T) {
	c, err := acquireLimited(t, filesDevice("/sdcard/a/b.txt"), WalkLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(auditActions(t, c, "acquire.warning")); n != 0 {
		t.Fatalf("warnings = %d", n)
	}
	st := auditActions(t, c, "acquire.stats")
	if len(st) != 1 || st[0].Details["incomplete"] != false {
		t.Fatalf("stats = %+v", st)
	}
}
