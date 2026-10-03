package exfat_test

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// A directory whose read budget is spent before its first entry is an error
// (never an empty listing); Walk reports it.
func TestExfatBudgetExhaustedBeforeFirstEntryIsAnError(t *testing.T) {
	files := []exfattest.File{
		{Path: "/d", Dir: true},
		{Path: "/d/x", Data: []byte("x")},
		{Path: "/y", Data: []byte("y")},
	}
	img := exfattest.Build(exfattest.Options{}, files)
	f := openImg(t, img)
	f.SetDirBudget(4096) // exactly the one chunk the root costs
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 2 {
		t.Fatalf("root: %d entries, %v; want its 2 entries", len(es), err)
	}
	d := byName(t, es, "d")
	got, err := f.ReadDir(d)
	if err == nil || len(got) != 0 || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("ReadDir(d) = %d entries, %v; want a CorruptError", len(got), err)
	}
	if want := "directory read budget exhausted"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say %q", err, want)
	}
	if !hasWarn(f, "budget") {
		t.Errorf("no budget warning: %v", f.Info().Warnings)
	}
	// Lookup of a name in the unreadable directory reports the budget, not ErrNotFound.
	if _, err := f.Lookup("/d/x"); err == nil || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/d/x) = %v, want the budget error", err)
	}
	var errPaths []string
	_ = filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, err error) error {
		if err != nil {
			errPaths = append(errPaths, p)
		}
		return nil
	})
	if !slices.Equal(errPaths, []string{"/"}) { // the budget is spent: not even the root can be read again
		t.Errorf("Walk reported errors at %v", errPaths)
	}
}

// An entry set at or beyond the first end-of-directory marker is not part of the
// directory: its dirent ID names nothing.
func TestExfatEntryBeyondEndMarkerIsNotFound(t *testing.T) {
	files := []exfattest.File{
		{Path: "/a.txt", Data: []byte("alpha")},
		{Path: "/d", Dir: true},
		{Path: "/d/in.txt", Data: []byte("inner")},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	a, d, in := lay.Find("/a.txt", false), lay.Find("/d", false), lay.Find("/d/in.txt", false)
	const setLen = 3 * 32 // File, Stream and one Name entry
	// Copy live-looking sets into the free entries after each directory's end.
	rootBase := int(lay.ClusterOffset(lay.RootCluster))
	const planted = 100
	copy(img[rootBase+planted*32:], img[a.Offset:a.Offset+setLen])
	dBase := int(lay.ClusterOffset(d.FirstCluster))
	copy(img[dBase+30*32:], img[in.Offset:in.Offset+setLen])

	f := openImg(t, img)
	root := strconv.Itoa(int(lay.RootCluster))
	dc := strconv.Itoa(int(d.FirstCluster))
	for _, id := range []string{"dirent:" + root + ":" + strconv.Itoa(a.Index), "dirent:" + dc + ":" + strconv.Itoa(in.Index)} {
		if _, err := f.Open(filesys.Entry{ID: id}); err != nil {
			t.Fatalf("control Open(%s): %v", id, err)
		}
	}
	for _, id := range []string{"dirent:" + root + ":" + strconv.Itoa(planted), "dirent:" + dc + ":30"} {
		if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%s) beyond the end marker = %v, want ErrNotFound", id, err)
		}
	}
	// A planted directory set is not a directory either.
	copy(img[rootBase+planted*32:], img[d.Offset:d.Offset+setLen])
	f = openImg(t, img)
	if _, err := f.ReadDir(filesys.Entry{ID: "dirent:" + root + ":" + strconv.Itoa(planted)}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir of a planted directory set = %v, want ErrNotFound", err)
	}
	if got := names(readDir(t, f, "/")); !slices.Equal(got, []string{"a.txt", "d"}) {
		t.Errorf("root lists %v", got)
	}
}

// "." and ".." are shown in the ~raw~ form (they cannot be shown as they are).
func TestExfatDotNamesUseRawForm(t *testing.T) {
	units := [][]uint16{{'.'}, {'.', '.'}, {'.', '.', '.'}}
	files := []exfattest.File{
		{Path: "/p1", RawName: units[0], Data: []byte("1")},
		{Path: "/p2", RawName: units[1], Data: []byte("2")},
		{Path: "/p3", RawName: units[2], Data: []byte("3")},
	}
	f := openImg(t, exfattest.Build(exfattest.Options{}, files))
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 3 {
		t.Fatalf("ReadDir = %d entries, %v", len(es), err)
	}
	for i, u := range units {
		var raw []byte
		for _, c := range u {
			raw = binary.LittleEndian.AppendUint16(raw, c)
		}
		e := es[i]
		if i < 2 {
			want := "~raw~" + base64.RawURLEncoding.EncodeToString(raw)
			if e.Name != want || !slices.Equal(e.RawName, raw) {
				t.Errorf("name %q: Name %q RawName % x, want %q / % x", string(rune(u[0])), e.Name, e.RawName, want, raw)
			}
			if got, err := f.Lookup("/" + want); err != nil || got.ID != e.ID {
				t.Errorf("Lookup(%q) = %+v, %v", want, got, err)
			}
		} else if e.Name != "..." || e.RawName != nil { // an ordinary name that only looks odd
			t.Errorf("name ...: Name %q RawName % x, want it as it is", e.Name, e.RawName)
		}
	}
}

// markFree clears the allocation-bitmap bits of clusters (the image then claims
// they are free).
func markFree(img []byte, lay *exfattest.Layout, clusters ...uint32) {
	bmp := int(lay.ClusterOffset(lay.BitmapCluster))
	for _, c := range clusters {
		img[bmp+int(c-2)/8] &^= 1 << ((c - 2) % 8)
	}
}

func runsOf(lay *exfattest.Layout, clusters []uint32) []filesys.Run {
	var out []filesys.Run
	for _, c := range clusters {
		out = append(out, filesys.Run{Offset: lay.ClusterOffset(c), Length: int64(lay.ClusterSize)})
	}
	return filesys.MergeRuns(out)
}

// Unallocated never reports the bitmap, the up-case table or the root directory
// as free, nor clusters the FAT marks bad, even if the bitmap says they are free.
func TestExfatUnallocatedExcludesSystemAndBadClusters(t *testing.T) {
	files := []exfattest.File{
		{Path: "/keep.bin", Data: pattern(9000, 1)},
		{Path: "/gone.bin", Data: pattern(20000, 2), Deleted: true},
		{Path: "/d", Dir: true},
		{Path: "/d/x", Data: []byte("x")},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	free := lay.FreeClusters()
	if len(free) < 6 {
		t.Fatalf("setup: only %d free clusters", len(free))
	}
	// Control: an honest image.
	if got, err := openImg(t, img).Unallocated(); err != nil || !slices.Equal(got, runsOf(lay, free)) {
		t.Fatalf("honest image: %v, %v", got, err)
	}
	system := []uint32{lay.BitmapCluster, lay.UpcaseCluster, lay.RootCluster}
	bad := free[2]
	markFree(img, lay, system...)
	binary.LittleEndian.PutUint32(img[lay.FatEntryOffset(bad):], 0xFFFFFFF7)
	// A bad mark on an allocated cluster changes nothing.
	binary.LittleEndian.PutUint32(img[lay.FatEntryOffset(lay.Find("/keep.bin", false).FirstCluster):], 0xFFFFFFF7)

	var want []uint32
	for _, c := range free {
		if c != bad {
			want = append(want, c)
		}
	}
	got, err := openImg(t, img).Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	if w := runsOf(lay, want); !slices.Equal(got, w) {
		t.Errorf("Unallocated = %v\nwant %v (the free clusters without the bad one; the system clusters %v stay out)", got, w, system)
	}
}

func TestExfatUnallocatedRunCap(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, nil)
	bmp := int(lay.ClusterOffset(lay.BitmapCluster))
	for i := range 32 {
		img[bmp+i] = 0x55 // every other cluster free
	}
	all, err := openImg(t, img).Unallocated()
	if err != nil || len(all) <= 5 {
		t.Fatalf("setup: %d runs without the cap, %v", len(all), err)
	}
	f := openImg(t, img)
	f.SetUnallocatedRunCap(5)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || !hasWarn(f, "more than 5 free runs") {
		t.Errorf("%d runs, warnings %v; want exactly 5 runs and the cap warning", len(got), f.Info().Warnings)
	}
	if !slices.Equal(got, all[:5]) {
		t.Errorf("capped runs %v are not the first five of the %d runs (%v ...)", got, len(all), all[:5])
	}
}

func TestExfatDirRecordCapDefault(t *testing.T) {
	if got, want := exfat.MaxDirRecords, 1<<18; got != want {
		t.Errorf("maxDirRecords = %d, want %d", got, want)
	}
	f := openImg(t, exfattest.Build(exfattest.Options{}, nil))
	if got := f.DirRecordCap(); got != 1<<18 {
		t.Errorf("a new FS caps directories at %d entry sets, want %d", got, 1<<18)
	}
}
