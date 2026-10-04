package detect_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func mtfsDriver() detect.Driver {
	return detect.Driver{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open}
}

func image() []byte {
	return fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/a", Data: []byte("x")}}})
}

func TestOpenWithNoMatch(t *testing.T) {
	img := bytes.Repeat([]byte{0x55}, 4096)
	for name, drivers := range map[string][]detect.Driver{
		"nil":         nil,
		"empty":       {},
		"no match":    {mtfsDriver()},
		"nil funcs":   {{Name: "broken"}},
		"nil probe":   {{Name: "broken", Open: fstest.Open}},
		"nil open":    {{Name: "broken", Probe: func(io.ReaderAt, int64) bool { return true }}},
		"not matched": {{Name: "never", Probe: func(io.ReaderAt, int64) bool { return false }, Open: fstest.Open}},
	} {
		t.Run(name, func(t *testing.T) {
			fsys, err := detect.OpenWith(drivers, bytes.NewReader(img), int64(len(img)))
			if fsys != nil || !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "no recognized filesystem") {
				t.Errorf("OpenWith = %v, %v; want ErrUnsupported (no recognized filesystem)", fsys, err)
			}
		})
	}
	if _, err := detect.Open(bytes.NewReader(img), int64(len(img))); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open err = %v, want ErrUnsupported", err)
	}
	if name, ok := detect.Probe(bytes.NewReader(img), int64(len(img))); ok {
		t.Errorf("Probe = %q, true; want no match", name)
	}
}

func TestOpenWithFirstMatchWins(t *testing.T) {
	img := image()
	var opened []string
	mk := func(name string, match bool) detect.Driver {
		return detect.Driver{
			Name:  name,
			Probe: func(io.ReaderAt, int64) bool { return match },
			Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
				opened = append(opened, name)
				return fstest.Open(r, size)
			},
		}
	}
	drivers := []detect.Driver{mk("first", false), mk("second", true), mk("third", true)}
	fsys, err := detect.OpenWith(drivers, bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if fsys.Info().Type != "mtfs" {
		t.Errorf("Info = %+v", fsys.Info())
	}
	if strings.Join(opened, ",") != "second" {
		t.Errorf("opened = %v, want only second", opened)
	}

	// A matching driver whose Open fails is final: later drivers are not tried.
	boom := errors.New("boom")
	failing := detect.Driver{
		Name:  "failing",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return nil, boom },
	}
	if _, err := detect.OpenWith([]detect.Driver{failing, mtfsDriver()}, bytes.NewReader(img), int64(len(img))); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}

	if name, ok := detect.ProbeWith(drivers, bytes.NewReader(img), int64(len(img))); !ok || name != "second" {
		t.Errorf("ProbeWith = %q, %v", name, ok)
	}
}

func TestDetectRecoversPanic(t *testing.T) {
	img := image()
	panicOpen := detect.Driver{
		Name:  "evil",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) {
			i := len(img)
			_ = img[i] // index out of range
			return nil, nil
		},
	}
	fsys, err := detect.OpenWith([]detect.Driver{panicOpen}, bytes.NewReader(img), int64(len(img)))
	var ce *filesys.CorruptError
	if fsys != nil || !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("OpenWith = %v, %v; want *CorruptError", fsys, err)
	}
	if ce.Structure != "evil" || !strings.HasPrefix(ce.Reason, "parser panic: ") || !strings.Contains(ce.Reason, "index out of range") {
		t.Errorf("CorruptError = %+v", ce)
	}

	// A panic in Probe is reported by OpenWith as a CorruptError (not swallowed
	// as "no match", and not a crash); ProbeWith treats it as a non-match.
	panicProbe := detect.Driver{
		Name:  "evilprobe",
		Probe: func(io.ReaderAt, int64) bool { panic("probe boom") },
		Open:  fstest.Open,
	}
	fsys, err = detect.OpenWith([]detect.Driver{panicProbe, mtfsDriver()}, bytes.NewReader(img), int64(len(img)))
	if fsys != nil || !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("OpenWith(panicking Probe) = %v, %v; want *CorruptError", fsys, err)
	}
	if ce.Structure != "evilprobe" || ce.Reason != "probe panic: probe boom" {
		t.Errorf("CorruptError = %+v", ce)
	}
	if name, ok := detect.ProbeWith([]detect.Driver{panicProbe, mtfsDriver()}, bytes.NewReader(img), int64(len(img))); !ok || name != "mtfs" {
		t.Errorf("ProbeWith = %q, %v; want the panicking driver skipped", name, ok)
	}

	// (nil, nil) from a driver is reported, not returned as a nil filesystem.
	nilFS := detect.Driver{
		Name: "nilfs", Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) { return nil, nil },
	}
	if fsys, err := detect.OpenWith([]detect.Driver{nilFS}, bytes.NewReader(img), int64(len(img))); fsys != nil || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("OpenWith(nil,nil driver) = %v, %v", fsys, err)
	}
}

func TestProbeWithSkipsDriversWithoutOpen(t *testing.T) {
	img := image()
	noOpen := detect.Driver{Name: "noopen", Probe: func(io.ReaderAt, int64) bool { return true }}
	r := bytes.NewReader(img)
	if name, ok := detect.ProbeWith([]detect.Driver{noOpen}, r, int64(len(img))); ok {
		t.Errorf("ProbeWith matched a driver without Open: %q", name)
	}
	if name, ok := detect.ProbeWith([]detect.Driver{noOpen, mtfsDriver()}, r, int64(len(img))); !ok || name != "mtfs" {
		t.Errorf("ProbeWith = %q, %v; want mtfs", name, ok)
	}
	// Consistent with OpenWith, which also skips it.
	if _, err := detect.OpenWith([]detect.Driver{noOpen}, r, int64(len(img))); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("OpenWith err = %v", err)
	}
}

// typedNilFS is a FileSystem wrapper whose nil pointer, returned alongside an
// error, makes the interface value non-nil.
type typedNilFS struct{ filesys.FileSystem }

func TestOpenWithErrorDropsTypedNilFileSystem(t *testing.T) {
	img := image()
	want := errors.New("driver failed")
	d := detect.Driver{
		Name: "typednil", Probe: func(io.ReaderAt, int64) bool { return true },
		Open: func(io.ReaderAt, int64) (filesys.FileSystem, error) { return (*typedNilFS)(nil), want },
	}
	fsys, err := detect.OpenWith([]detect.Driver{d}, bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if fsys != nil {
		t.Errorf("OpenWith returned a non-nil FileSystem %T alongside an error", fsys)
	}
}

// registryOrder is the spec §6 probe order; Drivers lists the implemented ones
// in this relative order.
var registryOrder = []string{"apfs", "f2fs", "ext4", "exfat", "hfsplus", "fat"}

func TestDriversFollowSpecOrder(t *testing.T) {
	last := -1
	for _, d := range detect.Drivers {
		pos := slices.Index(registryOrder, d.Name)
		if pos < 0 || pos <= last {
			t.Errorf("driver %q is unknown or out of the spec order %v", d.Name, registryOrder)
		}
		last = pos
		if d.Probe == nil || d.Open == nil {
			t.Errorf("driver %q has no Probe or Open", d.Name)
		}
	}
	if !slices.ContainsFunc(detect.Drivers, func(d detect.Driver) bool { return d.Name == "ext4" }) {
		t.Error("ext4 is not registered")
	}
}

func TestDetectOpensExt4(t *testing.T) {
	for _, o := range []ext4test.Options{
		{Extents: true, MetadataCsum: true, Label: "evidence"},
		{Journal: true},
		{},
	} {
		img := ext4test.Build(o, []ext4test.File{{Path: "/hello.txt", Data: []byte("hello")}})
		r := bytes.NewReader(img)
		if name, ok := detect.Probe(r, int64(len(img))); !ok || name != "ext4" {
			t.Fatalf("Probe = %q, %v; want ext4", name, ok)
		}
		fsys, err := detect.Open(r, int64(len(img)))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		info := fsys.Info()
		want := "ext4"
		switch {
		case !o.Extents && o.Journal:
			want = "ext3"
		case !o.Extents:
			want = "ext2"
		}
		if info.Type != want || info.Label != o.Label {
			t.Errorf("Info = %+v, want type %s label %q", info, want, o.Label)
		}
		e, err := fsys.Lookup("/hello.txt")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, f.Size())
		if _, err := f.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) || string(got) != "hello" {
			t.Errorf("content = %q, %v", got, err)
		}
		if runs, err := fsys.Unallocated(); err != nil || len(runs) == 0 {
			t.Errorf("Unallocated = %v, %v", runs, err)
		}
	}
}

func TestDetectExt4OpenFailureIsUntypedNil(t *testing.T) {
	// A superblock that probes as ext (magic, block size) but has an impossible
	// geometry fails to open: the registered driver must return a nil interface.
	img := make([]byte, 8192)
	binary.LittleEndian.PutUint16(img[1024+0x38:], 0xEF53)
	var ext *detect.Driver
	for i := range detect.Drivers {
		if detect.Drivers[i].Name == "ext4" {
			ext = &detect.Drivers[i]
		}
	}
	if ext == nil || !ext.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatalf("setup: ext4 driver %v does not probe the image", ext)
	}
	fsys, err := ext.Open(bytes.NewReader(img), int64(len(img)))
	if err == nil || fsys != nil {
		t.Fatalf("Open = %v, %v; want a nil FileSystem and an error", fsys, err)
	}
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) {
		t.Errorf("err = %v, want a CorruptError", err)
	}
	if f, err := detect.Open(bytes.NewReader(img), int64(len(img))); f != nil || err == nil {
		t.Errorf("detect.Open = %v, %v", f, err)
	}
}

// An F2FS image (builder-made and real) is detected as f2fs, an ext4 image
// still as ext4, and the opened filesystem works end to end.
func TestDetectOpensF2FS(t *testing.T) {
	img := f2fstest.Build(f2fstest.Options{Segments: 2, Label: "userdata"},
		[]f2fstest.File{{Path: "/hello.txt", Data: []byte("hello"), Inline: true}, {Path: "/big.bin", Data: bytes.Repeat([]byte{7}, 9000)}})
	r := bytes.NewReader(img)
	if name, ok := detect.Probe(r, int64(len(img))); !ok || name != "f2fs" {
		t.Fatalf("Probe = %q, %v; want f2fs", name, ok)
	}
	fsys, err := detect.Open(r, int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if info := fsys.Info(); info.Type != "f2fs" || info.Label != "userdata" {
		t.Errorf("Info = %+v, want type f2fs label userdata", info)
	}
	e, err := fsys.Lookup("/hello.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) || string(got) != "hello" {
		t.Errorf("content = %q, %v", got, err)
	}
	runs, err := fsys.Unallocated()
	if err != nil || len(runs) == 0 {
		t.Fatalf("Unallocated = %v, %v", runs, err)
	}
	// The big file's blocks are not free.
	big, err := fsys.Lookup("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	bf, err := fsys.Open(big)
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range bf.Runs() {
		for _, fr := range runs {
			if br.Offset < fr.Offset+fr.Length && fr.Offset < br.Offset+br.Length {
				t.Errorf("free run %+v overlaps the file's run %+v", fr, br)
			}
		}
	}

	// An ext4 image is still ext4.
	ext := ext4test.Build(ext4test.Options{Extents: true}, nil)
	if name, ok := detect.Probe(bytes.NewReader(ext), int64(len(ext))); !ok || name != "ext4" {
		t.Errorf("ext4 image: Probe = %q, %v; want ext4", name, ok)
	}
	fsys, err = detect.Open(bytes.NewReader(ext), int64(len(ext)))
	if err != nil || fsys.Info().Type != "ext4" {
		t.Errorf("ext4 image: Open = %v, %v", fsys, err)
	}
	if !slices.ContainsFunc(detect.Drivers, func(d detect.Driver) bool { return d.Name == "f2fs" }) {
		t.Error("f2fs is not registered")
	}
}

// An image that probes as F2FS but cannot be opened yields an untyped nil
// FileSystem with the error from the registered driver.
func TestDetectF2FSOpenFailureIsUntypedNil(t *testing.T) {
	img := make([]byte, 8192)
	binary.LittleEndian.PutUint32(img[1024:], 0xF2F52010) // magic
	binary.LittleEndian.PutUint16(img[1024+4:], 1)        // major_ver
	binary.LittleEndian.PutUint32(img[1024+16:], 12)      // log_blocksize
	var d *detect.Driver
	for i := range detect.Drivers {
		if detect.Drivers[i].Name == "f2fs" {
			d = &detect.Drivers[i]
		}
	}
	if d == nil || !d.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatalf("setup: f2fs driver %v does not probe the image", d)
	}
	fsys, err := d.Open(bytes.NewReader(img), int64(len(img)))
	if err == nil || fsys != nil {
		t.Fatalf("Open = %v, %v; want a nil FileSystem and an error", fsys, err)
	}
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) {
		t.Errorf("error %v is not a *filesys.CorruptError", err)
	}
	if _, err := detect.Open(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Error("detect.Open succeeded on a corrupt f2fs image")
	}
}
