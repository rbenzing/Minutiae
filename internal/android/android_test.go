package android

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/android/adb/adbtest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

const getprop = "[ro.product.model]: [Pixel 7]\n[ro.product.manufacturer]: [Google]\n" +
	"[ro.build.version.release]: [15]\n[ro.serialno]: [PX1]\n[ro.multi]: [a]: [b]]\n"

func fakeServer(t *testing.T, devs ...*adbtest.Device) *adbtest.Server {
	t.Helper()
	s, err := adbtest.Start(devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func findDevice(t *testing.T, s *adbtest.Server, serial string) *Device {
	t.Helper()
	devs, err := Enumerator{C: adb.New(s.Addr())}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if d.ID() == serial {
			return d.(*Device)
		}
	}
	t.Fatalf("device %s not listed", serial)
	return nil
}

func basicDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device", Props: "model:Pixel_7",
		Commands: map[string][]byte{"getprop": []byte(getprop)},
		Files:    map[string]adbtest.File{"/sdcard/a.txt": {Data: []byte("hello")}},
	}
}

func TestParseGetprop(t *testing.T) {
	p := ParseGetprop([]byte(getprop))
	if p["ro.product.model"] != "Pixel 7" || p["ro.multi"] != "a]: [b]" || len(p) != 5 {
		t.Fatalf("props = %v", p)
	}
}

func TestInfo(t *testing.T) {
	d := findDevice(t, fakeServer(t, basicDevice()), "PX1")
	info, err := d.Info(context.Background())
	if err != nil || info.Model != "Pixel 7" || info.Manufacturer != "Google" || info.OSVersion != "Android 15" || info.Extra["ro.serialno"] != "PX1" {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestUnauthorizedDevice(t *testing.T) {
	d := findDevice(t, fakeServer(t, &adbtest.Device{Serial: "U1", State: "unauthorized"}), "U1")
	if _, err := d.Info(context.Background()); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if _, err := d.Pull(context.Background(), "/x", &bytes.Buffer{}); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("pull err = %v", err)
	}
}

func TestListPullPush(t *testing.T) {
	s := fakeServer(t, basicDevice())
	d := findDevice(t, s, "PX1")
	es, err := d.List(context.Background(), "/sdcard")
	if err != nil || len(es) != 1 || es[0].Path != "/sdcard/a.txt" || es[0].Size != 5 || es[0].IsDir {
		t.Fatalf("list = %+v, %v", es, err)
	}
	var buf bytes.Buffer
	if n, err := d.Pull(context.Background(), "/sdcard/a.txt", &buf); err != nil || n != 5 || buf.String() != "hello" {
		t.Fatalf("pull n=%d err=%v %q", n, err, buf.String())
	}
	if err := d.Push(context.Background(), bytes.NewReader([]byte("tool")), "/data/local/tmp/t", 0o755); err != nil {
		t.Fatal(err)
	}
	if f, ok := s.Pushed("PX1", "/data/local/tmp/t"); !ok || string(f.Data) != "tool" || f.Mode != 0o100755 {
		t.Fatalf("pushed = %+v %v", f, ok)
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

func logicalDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{
			"getprop":             []byte(getprop),
			"pm list packages -f": []byte("package:/data/app/base.apk=com.whatsapp\n"),
		},
		Files: map[string]adbtest.File{
			"/sdcard/DCIM/a.jpg":  {Data: []byte("jpeg")},
			"/sdcard/link":        {Data: []byte("/x"), Mode: 0o120777},
			"/sdcard/notes:1.txt": {Data: []byte("note")},
			"/sdcard/secret.db":   {Data: []byte("s")},
			"/sdcard/zz.txt":      {Data: []byte("last")},
		},
		Unreadable: map[string]bool{"/sdcard/secret.db": true},
	}
}

func TestAcquireLogicalContinuesAfterUnreadable(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, logicalDevice()), "PX1")
	var progressed int64
	err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{}, func(done, _ int64) { progressed = done })
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	byRemote := map[string]evidence.ManifestRecord{}
	for _, r := range m {
		byRemote[r.Source.RemotePath] = r
	}
	for _, p := range []string{"exec:getprop", "exec:pm list packages -f", "/sdcard/DCIM/a.jpg", "/sdcard/notes:1.txt", "/sdcard/zz.txt"} {
		if r, ok := byRemote[p]; !ok || r.Incomplete {
			t.Errorf("%s: record %+v ok=%v", p, r, ok)
		}
	}
	if r := byRemote["/sdcard/secret.db"]; !r.Incomplete {
		t.Errorf("unreadable file should be an incomplete record: %+v", r)
	}
	if _, ok := byRemote["/sdcard/link"]; ok {
		t.Error("symlink should be skipped, not pulled")
	}
	if !strings.HasSuffix(byRemote["/sdcard/notes:1.txt"].Path, "files/sdcard/notes_1.txt") {
		t.Errorf("path = %s", byRemote["/sdcard/notes:1.txt"].Path)
	}
	if progressed != 12 { // jpeg + note + last
		t.Errorf("progress = %d", progressed)
	}
	audit, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if strings.Count(string(audit), `"action":"acquire.warning"`) != 2 || !strings.Contains(string(audit), `"action":"acquire.stats"`) {
		t.Errorf("audit missing warnings/stats:\n%s", audit)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

const procPartitions = "major minor  #blocks  name\n\n 179        0    7634944 mmcblk0\n 179        1          4 mmcblk0p1\n 179        2          8 mmcblk0p2\n"

const byName = "total 0\nlrwxrwxrwx 1 root root 20 2009-01-01 00:00 boot -> /dev/block/mmcblk0p1\nlrwxrwxrwx 1 root root 20 2009-01-01 00:00 userdata -> /dev/block/mmcblk0p2\n"

var bootImage = bytes.Repeat([]byte{0x41}, 4096)

// su and aospSu are the exact commands the fake device sees for the two su
// variants: su's own stderr is discarded outside the quoted command so it can
// never reach an image stream.
func su(cmd string) string     { return "su -c '" + cmd + "' 2>/dev/null" }
func aospSu(cmd string) string { return "su 0 sh -c '" + cmd + "' 2>/dev/null" }

func rootedDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "R1", State: "device",
		Commands: map[string][]byte{
			su("id"):                                           []byte("uid=0(root) gid=0(root)\n"),
			su("cat /proc/partitions"):                         []byte(procPartitions),
			su("ls -l /dev/block/by-name/"):                    []byte(byName),
			su("cat /sys/class/block/mmcblk0p1/size"):          []byte("8\n"),
			su("cat /sys/class/block/mmcblk0p2/size"):          []byte("16\n"),
			su("dd if=/dev/block/mmcblk0p1 bs=4M 2>/dev/null"): bootImage,
			su("dd if=/dev/block/mmcblk0p2 bs=4M 2>/dev/null"): []byte("short"),
		},
	}
}

func TestPartitions(t *testing.T) {
	d := findDevice(t, fakeServer(t, rootedDevice()), "R1")
	ps, err := d.Partitions(context.Background())
	if err != nil || len(ps) != 2 || ps[0].Name != "boot" || ps[0].Path != "/dev/block/mmcblk0p1" || ps[0].Size != 4096 || ps[1].Size != 8192 {
		t.Fatalf("partitions = %+v, %v", ps, err)
	}
}

func TestImageToCase(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, rootedDevice()), "R1")
	rec, err := device.ImageToCase(context.Background(), c, d, "R1", "acq", "boot", nil)
	if err != nil || rec.Size != 4096 || rec.Incomplete {
		t.Fatalf("rec = %+v, %v", rec, err)
	}
	got, _ := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)))
	if !bytes.Equal(got, bootImage) {
		t.Fatal("image bytes differ")
	}
}

func TestImageShortReadIsIncomplete(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, rootedDevice()), "R1")
	rec, err := device.ImageToCase(context.Background(), c, d, "R1", "acq", "userdata", nil)
	if err == nil || !strings.Contains(err.Error(), "short read") || !rec.Incomplete {
		t.Fatalf("rec = %+v, err = %v", rec, err)
	}
}

func TestImageRejectsBadPartitionName(t *testing.T) {
	d := findDevice(t, fakeServer(t, rootedDevice()), "R1")
	if _, err := d.Image(context.Background(), "boot; rm -rf /", &bytes.Buffer{}, nil); err == nil || !strings.Contains(err.Error(), "invalid partition") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotRooted(t *testing.T) {
	d := findDevice(t, fakeServer(t, basicDevice()), "PX1")
	if _, err := d.Partitions(context.Background()); !errors.Is(err, device.ErrNotRooted) {
		t.Fatalf("err = %v", err)
	}
}

func TestAOSPSuVariant(t *testing.T) {
	dev := &adbtest.Device{Serial: "A1", State: "device", Commands: map[string][]byte{
		aospSu("id"):                        []byte("uid=0(root)\n"),
		aospSu("cat /proc/partitions"):      []byte(procPartitions),
		aospSu("ls -l /dev/block/by-name/"): []byte("ls: /dev/block/by-name/: No such file or directory\n"),
	}}
	d := findDevice(t, fakeServer(t, dev), "A1")
	ps, err := d.Partitions(context.Background())
	if err != nil || len(ps) != 3 || ps[0].Path != "/dev/block/mmcblk0" {
		t.Fatalf("partitions = %+v, %v", ps, err)
	}
}

// hostileByName adds by-name links whose sizes need /sys, whose size cannot be
// determined at all, and whose targets try to leave /dev/block.
const hostileByName = byName +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 odd -> /dev/block/mmcblk0p3\n" +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 nosize -> /dev/block/mmcblk0p4\n" +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 unknown -> /dev/block/mmcblk0p5\n" +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 evil -> /dev/block/../../sdcard/evil\n" +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 dots -> /dev/block/./mmcblk0p1\n" +
	"lrwxrwxrwx 1 root root 20 2009-01-01 00:00 dbl -> /dev/block//mmcblk0p1\n"

var oddImage = bytes.Repeat([]byte{0x42}, 9*512) // 9 sectors; /proc/partitions rounds down to 4 KiB

func sizingDevice() *adbtest.Device {
	return &adbtest.Device{
		Serial: "R2", State: "device",
		Commands: map[string][]byte{
			su("id"):                                           []byte("uid=0(root) gid=0(root)\n"),
			su("cat /proc/partitions"):                         []byte(procPartitions + " 179        3          4 mmcblk0p3\n"),
			su("ls -l /dev/block/by-name/"):                    []byte(hostileByName),
			su("cat /sys/class/block/mmcblk0p3/size"):          []byte("9\n"),
			su("cat /sys/class/block/mmcblk0p4/size"):          []byte("2\n"),
			su("dd if=/dev/block/mmcblk0p3 bs=4M 2>/dev/null"): oddImage,
			su("dd if=/dev/block/mmcblk0p4 bs=4M 2>/dev/null"): bytes.Repeat([]byte{0x43}, 1024),
			su("dd if=/dev/block/mmcblk0p5 bs=4M 2>/dev/null"): []byte("unknown-size partition bytes"),
		},
	}
}

func TestPartitionsRejectsEscapingTargetsAndFillsSizeFromSys(t *testing.T) {
	d := findDevice(t, fakeServer(t, sizingDevice()), "R2")
	ps, err := d.Partitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]device.Partition{}
	for _, p := range ps {
		got[p.Name] = p
	}
	for _, bad := range []string{"evil", "dots", "dbl"} {
		if p, ok := got[bad]; ok {
			t.Errorf("non-canonical block target listed: %+v", p)
		}
	}
	if got["nosize"].Size != 1024 {
		t.Errorf("size of a partition missing from /proc/partitions should come from /sys: %+v", got["nosize"])
	}
	if len(ps) != 5 {
		t.Errorf("partitions = %+v", ps)
	}
}

func TestProcPartitionsFallbackRejectsDotDot(t *testing.T) {
	dev := &adbtest.Device{Serial: "A2", State: "device", Commands: map[string][]byte{
		aospSu("id"):                        []byte("uid=0(root)\n"),
		aospSu("cat /proc/partitions"):      []byte(procPartitions + " 179        9          4 ..\n 179       10          4 .\n"),
		aospSu("ls -l /dev/block/by-name/"): []byte("ls: /dev/block/by-name/: No such file or directory\n"),
	}}
	d := findDevice(t, fakeServer(t, dev), "A2")
	ps, err := d.Partitions(context.Background())
	if err != nil || len(ps) != 3 {
		t.Fatalf("partitions = %+v, %v", ps, err)
	}
	if _, err := d.Image(context.Background(), "..", &bytes.Buffer{}, nil); err == nil {
		t.Fatal(`imaging ".." should fail`)
	}
}

func TestImageExactSizeFromSys(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, sizingDevice()), "R2")
	for name, want := range map[string]int{"odd": len(oddImage), "nosize": 1024} {
		rec, err := device.ImageToCase(context.Background(), c, d, "R2", "acq", name, nil)
		if err != nil || rec.Incomplete || rec.Size != int64(want) {
			t.Errorf("%s: rec = %+v, err = %v", name, rec, err)
		}
	}
}

func TestImageUnknownSizeIsIncomplete(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, sizingDevice()), "R2")
	rec, err := device.ImageToCase(context.Background(), c, d, "R2", "acq", "unknown", nil)
	if err == nil || !strings.Contains(err.Error(), "size unknown; completeness unverifiable") || !rec.Incomplete {
		t.Fatalf("rec = %+v, err = %v", rec, err)
	}
	if rec.Size != int64(len("unknown-size partition bytes")) {
		t.Fatalf("partial bytes not kept: %+v", rec)
	}
}

func TestImageRejectsEscapingTarget(t *testing.T) {
	d := findDevice(t, fakeServer(t, sizingDevice()), "R2")
	for _, name := range []string{"evil", "dots", "dbl"} {
		if _, err := d.Image(context.Background(), name, &bytes.Buffer{}, nil); err == nil || !strings.Contains(err.Error(), "no partition named") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestResolvePartition(t *testing.T) {
	d := findDevice(t, fakeServer(t, sizingDevice()), "R2")
	p, err := d.ResolvePartition(context.Background(), "odd")
	if err != nil || p.Path != "/dev/block/mmcblk0p3" || p.Size != int64(len(oddImage)) {
		t.Fatalf("odd = %+v, %v", p, err)
	}
	if p, err := d.ResolvePartition(context.Background(), "unknown"); err != nil || p.Size != -1 {
		t.Fatalf("unknown = %+v, %v", p, err)
	}
}

func auditActions(t *testing.T, c *evidence.Case, action string) []evidence.AuditEntry {
	t.Helper()
	entries, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestAcquireLogicalSkipsInvalidDentNames(t *testing.T) {
	const dir, file = 0o040755, 0o100644
	dev := logicalDevice()
	dev.Unreadable = nil
	dev.ExtraDents = map[string][]adbtest.Dent{
		"/sdcard":      {{Name: "", Mode: dir}, {Name: "a/b", Mode: file, Size: 1}, {Name: "nul\x00x", Mode: file, Size: 1}},
		"/sdcard/DCIM": {{Name: "", Mode: dir}, {Name: "../escape", Mode: dir}},
	}
	c := newCase(t)
	d := findDevice(t, fakeServer(t, dev), "PX1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.AcquireLogical(ctx, c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	byRemote := map[string]evidence.ManifestRecord{}
	for _, r := range m {
		byRemote[r.Source.RemotePath] = r
	}
	for _, p := range []string{"/sdcard/DCIM/a.jpg", "/sdcard/notes:1.txt", "/sdcard/secret.db", "/sdcard/zz.txt"} {
		if r, ok := byRemote[p]; !ok || r.Incomplete {
			t.Errorf("%s: record %+v ok=%v", p, r, ok)
		}
	}
	if len(m) != 6 { // 2 info + 4 files
		t.Errorf("manifest has %d records, want 6", len(m))
	}
	invalid := 0
	for _, e := range auditActions(t, c, "acquire.warning") {
		if msg, _ := e.Details["error"].(string); strings.Contains(msg, "invalid entry name") {
			invalid++
		}
	}
	if invalid != 5 {
		t.Errorf("invalid-name warnings = %d, want 5", invalid)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

// acquireFiles runs a logical acquisition of a device holding files and
// checks that it succeeded, that every file became one complete artifact,
// that every manifest path is exactly (case included) a path on disk, and
// that the case verifies.
func acquireFiles(t *testing.T, files map[string]adbtest.File) map[string]evidence.ManifestRecord {
	t.Helper()
	c := newCase(t)
	d := findDevice(t, fakeServer(t, &adbtest.Device{
		Serial: "PX1", State: "device",
		Commands: map[string][]byte{"getprop": []byte(getprop), "pm list packages -f": []byte("package:x\n")},
		Files:    files,
	}), "PX1")
	if err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	byRemote := map[string]evidence.ManifestRecord{}
	inManifest := map[string]bool{}
	for _, r := range m {
		byRemote[r.Source.RemotePath] = r
		inManifest[r.Path] = true
	}
	for p, f := range files {
		r, ok := byRemote[p]
		if !ok || r.Incomplete || r.Size != int64(len(f.Data)) {
			t.Errorf("%s: record %+v ok=%v", p, r, ok)
		}
	}
	onDisk := map[string]bool{}
	_ = filepath.WalkDir(filepath.Join(c.Dir, "artifacts"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(c.Dir, p)
			onDisk[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	for p := range inManifest {
		if !onDisk[p] {
			t.Errorf("manifest path %s is not a path on disk; on disk: %v", p, onDisk)
		}
	}
	if len(onDisk) != len(inManifest) {
		t.Errorf("on disk %v, manifest %v", onDisk, inManifest)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	return byRemote
}

func TestAcquireLogicalCaseCollidingFiles(t *testing.T) {
	recs := acquireFiles(t, map[string]adbtest.File{
		"/sdcard/File.txt": {Data: []byte("UPPER")},
		"/sdcard/file.txt": {Data: []byte("lower")},
	})
	if a, b := recs["/sdcard/File.txt"].Path, recs["/sdcard/file.txt"].Path; strings.EqualFold(a, b) {
		t.Fatalf("case-colliding files share a local path: %s, %s", a, b)
	}
	if p := recs["/sdcard/file.txt"].Path; !strings.HasSuffix(p, "files/sdcard/file~2.txt") {
		t.Errorf("second file path = %s, want the ~2 suffix before the extension", p)
	}
}

func TestAcquireLogicalFileAndDirSanitizeAlike(t *testing.T) {
	recs := acquireFiles(t, map[string]adbtest.File{
		"/sdcard/a:b":    {Data: []byte("file")},
		"/sdcard/a_b/x":  {Data: []byte("child")},
		"/sdcard/zz.txt": {Data: []byte("later")},
	})
	if p := recs["/sdcard/a_b/x"].Path; !strings.HasSuffix(p, "files/sdcard/a_b~2/x") {
		t.Errorf("child of the renamed directory = %s", p)
	}
}

func TestAcquireLogicalCaseCollidingDirs(t *testing.T) {
	recs := acquireFiles(t, map[string]adbtest.File{
		"/sdcard/DCIM/a.jpg": {Data: []byte("A")},
		"/sdcard/dcim/b.jpg": {Data: []byte("B")},
	})
	if p := recs["/sdcard/dcim/b.jpg"].Path; !strings.HasSuffix(p, "files/sdcard/dcim~2/b.jpg") {
		t.Errorf("file in the case-colliding directory = %s", p)
	}
}

func TestAcquireLogicalOverlappingRootsCaptureOnce(t *testing.T) {
	c := newCase(t)
	d := findDevice(t, fakeServer(t, logicalDevice()), "PX1")
	if err := d.AcquireLogical(context.Background(), c, device.LogicalOptions{Roots: []string{"/sdcard", "/sdcard/DCIM"}}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest()
	n := 0
	for _, r := range m {
		if r.Source.RemotePath == "/sdcard/DCIM/a.jpg" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("/sdcard/DCIM/a.jpg captured %d times", n)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

func TestWithSuffixAndFoldCase(t *testing.T) {
	for in, want := range map[string]string{"a.txt": "a~2.txt", ".profile": ".profile~2", "noext": "noext~2", "x.tar.gz": "x.tar~2.gz"} {
		if got := withSuffix(in, 2, false); got != want {
			t.Errorf("withSuffix(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withSuffix("a.d", 3, true); got != "a.d~3" {
		t.Errorf("directory suffix = %q", got)
	}
	for _, pair := range [][2]string{{"DCIM/File.TXT", "dcim/file.txt"}, {"Σ", "ς"}, {"K", "K"}} {
		if foldCase(pair[0]) != foldCase(pair[1]) {
			t.Errorf("foldCase(%q) != foldCase(%q)", pair[0], pair[1])
		}
	}
	if foldCase("a.txt") == foldCase("b.txt") {
		t.Error("distinct names fold equal")
	}
}
