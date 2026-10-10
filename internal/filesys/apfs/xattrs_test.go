package apfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

var _ filesys.XattrReader = (*apfs.FS)(nil)

func readXattr(t *testing.T, f *apfs.FS, e filesys.Entry, name string) []byte {
	t.Helper()
	fl, err := f.OpenXattr(e, name)
	if err != nil {
		t.Fatalf("OpenXattr(%q): %v", name, err)
	}
	b, err := io.ReadAll(io.NewSectionReader(fl, 0, fl.Size()))
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return b
}

func xattrFixture(t *testing.T) (*apfs.FS, filesys.Entry, []byte) {
	t.Helper()
	big := pattern(3*bs+17, 9)
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{
		Path: "/tagged", Data: []byte("x"),
		Xattrs: []apfstest.Xattr{
			{Name: "com.example.small", Value: []byte("value")},
			{Name: "com.example.empty", Value: nil},
			{Name: "com.example.big", Value: big, Stream: true, WriteStream: true},
			{Name: "b\xff", Value: []byte("raw")},
		},
	})))
	return f, mustLookup(t, f, "/Data/tagged"), big
}

func TestXattrsListNamesSizesAndStorage(t *testing.T) {
	f, e, big := xattrFixture(t)
	got, err := f.Xattrs(e)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]filesys.Xattr{
		"com.example.small": {Name: "com.example.small", Size: 5, Storage: filesys.XattrInline},
		"com.example.empty": {Name: "com.example.empty", Size: 0, Storage: filesys.XattrInline},
		"com.example.big":   {Name: "com.example.big", Size: int64(len(big)), Storage: filesys.XattrStream},
	}
	if len(got) != 4 {
		t.Fatalf("%d xattrs, want 4: %+v", len(got), got)
	}
	var raw int
	for _, x := range got {
		if w, ok := want[x.Name]; ok {
			if x != w {
				t.Errorf("%s = %+v, want %+v", x.Name, x, w)
			}
			continue
		}
		raw++
		if x.Name != filesys.RawNameAlias([]byte("b\xff")) {
			t.Errorf("a non-UTF-8 name must use the ~raw~ alias, got %q", x.Name)
		}
	}
	if raw != 1 {
		t.Errorf("%d aliased names, want 1", raw)
	}
}

func TestOpenXattrReturnsValues(t *testing.T) {
	f, e, big := xattrFixture(t)
	if got := readXattr(t, f, e, "com.example.small"); string(got) != "value" {
		t.Errorf("small = %q", got)
	}
	if got := readXattr(t, f, e, "com.example.empty"); len(got) != 0 {
		t.Errorf("empty = %q", got)
	}
	got := readXattr(t, f, e, "com.example.big")
	if !bytes.Equal(got, big) {
		t.Errorf("stream value differs (%d bytes, want %d)", len(got), len(big))
	}
	fl, _ := f.OpenXattr(e, "com.example.big")
	if len(fl.Runs()) == 0 {
		t.Error("a stream-stored value must report its image runs")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Errorf("runs: %v", err)
	}
	inl, _ := f.OpenXattr(e, "com.example.small")
	if len(inl.Runs()) != 0 {
		t.Errorf("an embedded value has no image runs, got %v", inl.Runs())
	}
	// partial and past-the-end reads
	buf := make([]byte, 100)
	if n, err := fl.ReadAt(buf, fl.Size()-40); n != 40 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:40], big[len(big)-40:]) {
		t.Errorf("ReadAt near the end = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf, fl.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	noWarnings(t, f)
}

func TestOpenXattrUnknownNameIsNotFound(t *testing.T) {
	f, e, _ := xattrFixture(t)
	if _, err := f.OpenXattr(e, "com.example.nothing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown name: %v, want ErrNotFound", err)
	}
	if _, err := f.Xattrs(filesys.Entry{ID: "bogus"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("bad ID: %v, want ErrNotFound", err)
	}
	// An entry with no xattr at all lists none (not an error, not "absent").
	g, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/plain", Data: []byte("p")})))
	if xs, err := g.Xattrs(mustLookup(t, g, "/Data/plain")); err != nil || len(xs) != 0 {
		t.Errorf("Xattrs of a plain file = %+v, %v", xs, err)
	}
}

func TestOpenXattrStreamHostile(t *testing.T) {
	const ino = 40
	rec := func(name string, val []byte) apfstest.FSRecord {
		return apfstest.FSRecord{ID: ino, Type: apfstest.TypeXattr, Key: apfstest.XattrKey(name), Val: val}
	}
	short := make([]byte, 4+8) // flags, xdata_len 8: an oid and no size
	short[0], short[2] = 1, 8
	both := apfstest.XattrStreamVal(0x5000, 10)
	both[0] = 3 // stream and inline
	neither := []byte{0, 0, 0, 0}
	tooLong := []byte{2, 0, 0xFF, 0x0F} // inline, xdata_len 4095 with no data
	plain := []apfstest.File{{Path: "/h", Ino: ino, Data: []byte("x")}}
	cases := map[string]struct {
		files []apfstest.File
		extra []apfstest.FSRecord
	}{
		"no extents (a hole)":    {files: []apfstest.File{{Path: "/h", Ino: ino, Data: []byte("x"), Xattrs: []apfstest.Xattr{{Name: "v", Value: []byte("12345"), Stream: true}}}}},
		"bigger than the image":  {files: plain, extra: []apfstest.FSRecord{rec("v", apfstest.XattrStreamVal(0x5000, 1<<50))}},
		"object id zero":         {files: plain, extra: []apfstest.FSRecord{rec("v", apfstest.XattrStreamVal(0, 10))}},
		"descriptor too short":   {files: plain, extra: []apfstest.FSRecord{rec("v", short)}},
		"stream and inline":      {files: plain, extra: []apfstest.FSRecord{rec("v", both)}},
		"neither storage flag":   {files: plain, extra: []apfstest.FSRecord{rec("v", neither)}},
		"length past the record": {files: plain, extra: []apfstest.FSRecord{rec("v", tooLong)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := dataVolume(tc.files...)
			v.Extra = tc.extra
			f, _ := openOpts(t, volOpts(v))
			e := mustLookup(t, f, "/Data/h")
			xs, err := f.Xattrs(e)
			if err != nil {
				t.Fatalf("Xattrs must list a damaged attribute, not fail the entry: %v", err)
			}
			var found bool
			for _, x := range xs {
				if x.Name == "v" {
					found = true
				}
			}
			if !found {
				t.Fatalf("the damaged attribute is not listed (unknown is never absent): %+v", xs)
			}
			fl, err := f.OpenXattr(e, "v")
			if !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
				t.Errorf("OpenXattr = %v, %v; want ErrCorrupt and no file", fl, err)
			}
		})
	}
}

// An error about a stream names the object that owns it (the inode), never only
// the stream's own object id, which means nothing to the examiner.
func TestOpenXattrStreamErrorNamesTheOwner(t *testing.T) {
	const ino, oid = 40, 0x5000
	xrec := apfstest.FSRecord{ID: ino, Type: apfstest.TypeXattr, Key: apfstest.XattrKey("v"), Val: apfstest.XattrStreamVal(oid, 10)}
	damaged := apfstest.FSRecord{ID: oid, Type: apfstest.TypeFileExtent, Key: make([]byte, 8), Val: []byte{1, 2, 3}}
	cases := map[string][]apfstest.FSRecord{
		"hole (no extent record)": {xrec},
		"damaged extent record":   {xrec, damaged},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			v := dataVolume(apfstest.File{Path: "/h", Ino: ino, Data: []byte("x")})
			v.Extra = extra
			f, _ := openOpts(t, volOpts(v))
			e := mustLookup(t, f, "/Data/h")
			_, err := f.OpenXattr(e, "v")
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Fatalf("OpenXattr = %v, want ErrCorrupt", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, fmt.Sprintf("inode %d", ino)) {
				t.Errorf("the error does not name the owner inode %d: %q", ino, msg)
			}
			if strings.Contains(msg, fmt.Sprintf("inode %d", oid)) {
				t.Errorf("the error names the stream id %d as if it were the owner: %q", oid, msg)
			}
		})
	}
}

// An attribute set that cannot be read is an error and no list; only an entry
// that has no attribute lists none.
func TestXattrsUnreadableSetIsAnErrorNotEmpty(t *testing.T) {
	v := dataVolume(
		apfstest.File{Path: "/none", Data: []byte("n")},
		apfstest.File{Path: "/tagged", Data: []byte("t"), Xattrs: []apfstest.Xattr{{Name: "a", Value: []byte("1")}}},
	)
	v.TreeMaxKeys = 3
	im := newImage(t, volOpts(v))
	vg := im.g.Volumes[0]
	if len(vg.FsNodes) < 2 {
		t.Fatalf("the fixture needs a multi-node fs tree, got %d nodes", len(vg.FsNodes))
	}
	if vg.Inodes["/tagged"] == 0 || vg.Inodes["/none"] == 0 {
		t.Fatalf("builder inode map lacks the fixture paths: %v", vg.Inodes)
	}
	idOf := func(p string) filesys.Entry { return filesys.Entry{ID: apfs.NodeID(0, 0, vg.Inodes[p])} }
	f := im.mustOpen()
	// Intact: the plain entry lists nothing and is not an error; the other lists one.
	if xs, err := f.Xattrs(idOf("/none")); err != nil || len(xs) != 0 {
		t.Fatalf("intact plain entry = %+v, %v; want an empty list and no error", xs, err)
	}
	if xs, err := f.Xattrs(idOf("/tagged")); err != nil || len(xs) != 1 {
		t.Fatalf("intact tagged entry = %+v, %v", xs, err)
	}
	// Damage every non-root fs-tree node (a byte changed, the checksum not
	// resealed) and read through a fresh FS so no node is cached.
	for _, n := range vg.FsNodes[1:] {
		im.blk(n)[40] ^= 0xFF
	}
	g, err := im.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, p := range []string{"/none", "/tagged"} {
		xs, err := g.Xattrs(idOf(p))
		if err == nil || xs != nil {
			t.Errorf("Xattrs(%s) on an unreadable tree = %+v, %v; want an error and no list (never an empty list)", p, xs, err)
		}
	}
}

func TestXattrRecordCap(t *testing.T) {
	var xs []apfstest.Xattr
	for _, n := range []string{"a", "b", "c", "d"} {
		xs = append(xs, apfstest.Xattr{Name: n, Value: []byte(n)})
	}
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/many", Data: []byte("m"), Xattrs: xs})))
	e := mustLookup(t, f, "/Data/many")
	f.SetXattrCap(3)
	got, err := f.Xattrs(e)
	if !errors.Is(err, filesys.ErrXattrCap) || got != nil {
		t.Errorf("Xattrs over the cap = %+v, %v; want ErrXattrCap and no partial list", got, err)
	}
	if _, err := f.OpenXattr(e, "a"); !errors.Is(err, filesys.ErrXattrCap) {
		t.Errorf("OpenXattr over the cap = %v; want ErrXattrCap", err)
	}
	f.SetXattrCap(4)
	if got, err := f.Xattrs(e); err != nil || len(got) != 4 {
		t.Errorf("Xattrs at the cap = %+v, %v", got, err)
	}
}

func TestXattrsEncryptedVolumeIsErrEncrypted(t *testing.T) {
	v := apfstest.Volume{Name: "Enc", Encrypted: true}
	f, _ := openOpts(t, volOpts(dataVolume(), v))
	e := filesys.Entry{ID: apfs.NodeID(1, 0, 16)}
	if _, err := f.Xattrs(e); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("Xattrs on an encrypted volume = %v, want ErrEncrypted", err)
	}
	if _, err := f.OpenXattr(e, "a"); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("OpenXattr on an encrypted volume = %v, want ErrEncrypted", err)
	}
}

func TestXattrsReadFromTheSnapshotView(t *testing.T) {
	v := dataVolume(apfstest.File{Path: "/f", Data: []byte("new"), Xattrs: []apfstest.Xattr{{Name: "k", Value: []byte("live")}}})
	v.Snapshots = []apfstest.Snapshot{{Name: "S", Files: []apfstest.File{
		{Path: "/f", Data: []byte("old"), Xattrs: []apfstest.Xattr{{Name: "k", Value: []byte("snap")}}},
	}}}
	f, _ := openOpts(t, volOpts(v))
	live := mustLookup(t, f, "/Data/f")
	snap := mustLookup(t, f, "/Data/.snapshots/S/f")
	if got := readXattr(t, f, live, "k"); string(got) != "live" {
		t.Errorf("live k = %q", got)
	}
	if got := readXattr(t, f, snap, "k"); string(got) != "snap" {
		t.Errorf("snapshot k = %q; a snapshot entry must read the snapshot's tree", got)
	}
}

// A record over the format's value limit is listed (size -1) and fails on open:
// it is never skipped, which would make a damaged attribute read as absent.
func TestXattrOversizeRecordIsListedAndCorrupt(t *testing.T) {
	huge := bytes.Repeat([]byte{'v'}, 3900) // the record value (3904 bytes) is over JOBJ_MAX_VALUE_SIZE (3808) yet still fits a 4096-byte node
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{
		Path: "/o", Data: []byte("o"),
		Xattrs: []apfstest.Xattr{{Name: "ok", Value: []byte("fine")}, {Name: "huge", Value: huge}},
	})))
	e := mustLookup(t, f, "/Data/o")
	xs, err := f.Xattrs(e)
	if err != nil || len(xs) != 2 {
		t.Fatalf("Xattrs = %+v, %v; want both attributes listed", xs, err)
	}
	for _, x := range xs {
		if x.Name == "huge" && x.Size != -1 {
			t.Errorf("an over-limit record is listed with size %d, want -1", x.Size)
		}
	}
	if fl, err := f.OpenXattr(e, "huge"); !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
		t.Errorf("OpenXattr(huge) = %v, %v; want ErrCorrupt", fl, err)
	}
	if got := readXattr(t, f, e, "ok"); string(got) != "fine" {
		t.Errorf("the intact attribute reads %q", got)
	}
}

// Opening one attribute clones only that attribute's value, however many there
// are. The reader keeps no node cache, so a scan allocates for every node it
// reads; listing reads the same nodes and clones no value, which makes it the
// baseline: opening may exceed it by the one match, not by all 200 values.
func TestOpenXattrClonesOnlyTheMatch(t *testing.T) {
	var xs []apfstest.Xattr
	for i := range 200 {
		xs = append(xs, apfstest.Xattr{Name: fmt.Sprintf("n%03d", i), Value: bytes.Repeat([]byte{byte(i)}, 3000)})
	}
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/many", Data: []byte("m"), Xattrs: xs})))
	e := mustLookup(t, f, "/Data/many")
	alloc := func(fn func() error) int64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := fn(); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc - before.TotalAlloc)
	}
	_ = alloc(func() error { _, err := f.Xattrs(e); return err }) // warm up
	list := alloc(func() error { _, err := f.Xattrs(e); return err })
	open := alloc(func() error { _, err := f.OpenXattr(e, "n199"); return err })
	// 200 values of 3000 bytes are 600 KB; the match is 3 KB.
	if extra := open - list; extra > 100<<10 {
		t.Errorf("OpenXattr allocated %d bytes more than listing for one 3000-byte value among 200 (listing %d)", extra, list)
	}
}
