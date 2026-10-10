package exfat_test

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

func recImage(t *testing.T, files []exfattest.File) (*exfat.FS, []byte, *exfattest.Layout) {
	t.Helper()
	img, l := exfattest.BuildLayout(exfattest.Options{}, files)
	return openImg(t, img), img, l
}

func recID(loc exfattest.EntryLoc) filesys.Entry {
	return filesys.Entry{ID: "dirent:" + strconv.FormatUint(uint64(loc.DirCluster), 10) + ":" + strconv.Itoa(loc.Index)}
}

func one(t *testing.T, f *exfat.FS, loc exfattest.EntryLoc) filesys.Candidate {
	t.Helper()
	cs, err := f.Recoverable(recID(loc))
	if err != nil || len(cs) != 1 {
		t.Fatalf("Recoverable = %v, %v; want one candidate", cs, err)
	}
	return cs[0]
}

func ptr[T any](v T) *T { return &v }

func TestExfatRecoverableNoFatChain(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 200) // 3200 bytes: one cluster
	f, img, l := recImage(t, []exfattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := l.Find("/GONE.BIN", true)
	c := one(t, f, loc)
	if c.Method != exfat.MethodNoFatChain || c.Size != int64(len(data)) {
		t.Fatalf("candidate %+v", c)
	}
	want := []filesys.Run{{Offset: l.ClusterOffset(loc.FirstCluster), Length: int64(len(data))}}
	if !slices.Equal(c.Runs, want) {
		t.Fatalf("runs %v, want %v", c.Runs, want)
	}
	if !bytes.Equal(img[c.Runs[0].Offset:c.Runs[0].Offset+c.Runs[0].Length], data) {
		t.Error("the bytes at the run are not the data")
	}
	if !slices.Contains(c.Assumptions, "contiguity-from-no-fat-chain") {
		t.Errorf("assumptions %v", c.Assumptions)
	}
	dirent := "dirent:" + strconv.FormatUint(uint64(loc.DirCluster), 10) + ":" + strconv.Itoa(loc.Index)
	for _, b := range []string{dirent, "first_cluster=" + strconv.FormatUint(uint64(loc.FirstCluster), 10), "data_length=3200", "valid_data_length=3200", "no_fat_chain"} {
		if !slices.Contains(c.Basis, b) {
			t.Errorf("basis %v lacks %q", c.Basis, b)
		}
	}
	if _, err := filesys.CheckCandidate(c, int64(len(img))); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
}

func TestExfatRecoverableMultiClusterNoFatChain(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 9000) // three clusters of 4096
	f, _, l := recImage(t, []exfattest.File{{Path: "/BIG.BIN", Data: data, Deleted: true}})
	c := one(t, f, l.Find("/BIG.BIN", true))
	if len(c.Runs) != 1 || c.Runs[0].Length != 9000 || c.Method != exfat.MethodNoFatChain {
		t.Fatalf("candidate %+v", c)
	}
}

func TestExfatRecoverableValidDataLength(t *testing.T) {
	data := bytes.Repeat([]byte{9}, 1500)
	f, _, l := recImage(t, []exfattest.File{{Path: "/V.BIN", Data: data, Deleted: true, ValidLength: ptr(int64(700))}})
	c := one(t, f, l.Find("/V.BIN", true))
	if c.Size != 700 || len(c.Runs) != 1 || c.Runs[0].Length != 700 {
		t.Fatalf("candidate %+v", c)
	}
	for _, a := range []string{"data-length=1500", "tail-beyond-valid-data-length-not-stored"} {
		if !slices.Contains(c.Assumptions, a) {
			t.Errorf("assumptions %v lack %q", c.Assumptions, a)
		}
	}
	for _, r := range c.Runs {
		if r.Offset == -1 {
			t.Error("hole run")
		}
	}
	if !slices.Contains(c.Basis, "valid_data_length=700") || !slices.Contains(c.Basis, "data_length=1500") {
		t.Errorf("basis %v", c.Basis)
	}
}

func TestExfatRecoverableZeroValidLengthIsEmpty(t *testing.T) {
	f, _, l := recImage(t, []exfattest.File{{Path: "/Z.BIN", Data: bytes.Repeat([]byte{1}, 300), Deleted: true, ValidLength: ptr(int64(0))}})
	c := one(t, f, l.Find("/Z.BIN", true))
	if c.Size != 0 || len(c.Runs) != 0 || !slices.Contains(c.Assumptions, "data-length=300") {
		t.Fatalf("candidate %+v", c)
	}
	// A truly empty file records no data-length assumption.
	f, _, l = recImage(t, []exfattest.File{{Path: "/E.BIN", Deleted: true}})
	c = one(t, f, l.Find("/E.BIN", true))
	if c.Size != 0 || len(c.Runs) != 0 || len(c.Assumptions) != 0 {
		t.Fatalf("empty file candidate %+v", c)
	}
}

// TestExfatRecoverableChainedFileIsContiguousUntilTask8: Task 8 replaces this
// expectation (a chained file then gets the FAT-chain method); until then a
// file that is not NoFatChain falls back to the contiguous assumption.
func TestExfatRecoverableChainedFileIsContiguousUntilTask8(t *testing.T) {
	data := bytes.Repeat([]byte{5}, 9000)
	f, _, l := recImage(t, []exfattest.File{{Path: "/F.BIN", Data: data, Deleted: true, FatChain: true, Fragmented: true}})
	c := one(t, f, l.Find("/F.BIN", true))
	if c.Method != exfat.MethodContiguous || !slices.Contains(c.Assumptions, "contiguous-allocation") {
		t.Fatalf("candidate %+v", c)
	}
	if slices.Contains(c.Assumptions, "contiguity-from-no-fat-chain") || !slices.Contains(c.Basis, "fat_chain") {
		t.Errorf("assumptions %v basis %v", c.Assumptions, c.Basis)
	}
}

func TestExfatRecoverableLiveIsNotDeleted(t *testing.T) {
	f, _, l := recImage(t, []exfattest.File{{Path: "/LIVE.TXT", Data: []byte("x")}, {Path: "/D", Dir: true}})
	cs, err := f.Recoverable(recID(l.Find("/LIVE.TXT", false)))
	if !errors.Is(err, filesys.ErrNotDeleted) || cs != nil {
		t.Errorf("live file: %v, %v", cs, err)
	}
	d := l.Find("/D", false)
	cs, err = f.Recoverable(filesys.Entry{ID: "dir:" + strconv.FormatUint(uint64(d.FirstCluster), 10)})
	if !errors.Is(err, filesys.ErrNotDeleted) || cs != nil {
		t.Errorf("live directory id: %v, %v", cs, err)
	}
	cs, err = f.Recoverable(filesys.Entry{ID: "dir:123"})
	if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Errorf("unknown directory id: %v, %v", cs, err)
	}
}

func TestExfatRecoverableForgedIDsAreNotFound(t *testing.T) {
	f, _, _ := recImage(t, []exfattest.File{{Path: "/A.TXT", Data: []byte("x")}})
	for _, id := range []string{"", "dirent:", "dirent:01:2", "dirent:+1:2", "dirent:5:-1", "dirent:5", "dirent:5:1:2", "dirent:4294967296:0", "nid:7", " dirent:5:1", "dirent:5:99999999", "dirent:99:0", "dir:", "dir:01"} {
		cs, err := f.Recoverable(filesys.Entry{ID: id})
		if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
			t.Errorf("Recoverable(%q) = %v, %v; want ErrNotFound", id, cs, err)
		}
	}
}

func TestExfatRecoverableDeletedDirectoryUnsupported(t *testing.T) {
	f, _, l := recImage(t, []exfattest.File{{Path: "/GONE", Dir: true, Deleted: true}})
	cs, err := f.Recoverable(recID(l.Find("/GONE", true)))
	if !errors.Is(err, filesys.ErrUnsupported) || cs != nil {
		t.Errorf("deleted directory: %v, %v", cs, err)
	}
}

func TestExfatRecoverableBadChecksumIsCorrupt(t *testing.T) {
	f, _, l := recImage(t, []exfattest.File{{Path: "/BAD.BIN", Data: []byte("data"), Deleted: true, BadChecksum: true}})
	cs, err := f.Recoverable(recID(l.Find("/BAD.BIN", true)))
	var ce *filesys.CorruptError
	if !errors.Is(err, filesys.ErrCorrupt) || !errors.As(err, &ce) || ce.Structure != "recovery map" || cs != nil {
		t.Errorf("bad checksum: %v, %v", cs, err)
	}
}

// A deleted set whose SecondaryCount is above 18 is not listed at all, so its
// id names nothing.
func TestExfatRecoverableDamagedSetIsCorrupt(t *testing.T) {
	_, img, l := recImage(t, []exfattest.File{{Path: "/BAD.BIN", Data: []byte("data"), Deleted: true}})
	loc := l.Find("/BAD.BIN", true)
	img[loc.Offset+1] = 19
	cs, err := openImg(t, img).Recoverable(recID(loc))
	if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Errorf("SecondaryCount 19: %v, %v; want ErrNotFound", cs, err)
	}
}

func TestExfatRecoverableLongNameBasisFlags(t *testing.T) {
	name := strings.Repeat("n", 60) + ".bin"
	f, _, l := recImage(t, []exfattest.File{{Path: "/" + name, Data: []byte("data"), Deleted: true}})
	c := one(t, f, l.Find("/"+name, true))
	for _, b := range c.Basis {
		if strings.HasPrefix(b, "name=") {
			t.Errorf("a clean long name has the flag %q", b)
		}
	}
	// A raw name that is not valid UTF-16 does not break the basis.
	f, _, l = recImage(t, []exfattest.File{{Path: "/x", Data: []byte("d"), Deleted: true, RawName: []uint16{'a', 0xD800, 'b'}}})
	c = one(t, f, l.Find("/x", true))
	if _, err := filesys.CheckCandidate(c, 1<<30); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
}

func TestExfatRecoverableNameFlags(t *testing.T) {
	// A name shorter than NameLength makes the set inconsistent: no candidate, so
	// the name=truncated flag can never reach a map.
	name := "/" + strings.Repeat("m", 40)
	_, img, l := recImage(t, []exfattest.File{{Path: name, Data: []byte("d"), Deleted: true}})
	loc := l.Find(name, true)
	img[loc.Offset+32+3] = 255
	exfattest.FixSetChecksum(img, loc.Offset)
	cs, err := openImg(t, img).Recoverable(recID(loc))
	if !errors.Is(err, filesys.ErrCorrupt) || cs != nil {
		t.Errorf("truncated name: %v, %v; want a corrupt recovery map", cs, err)
	}
}

func TestExfatRecoverableHostileExtents(t *testing.T) {
	cases := []struct {
		name  string
		patch exfattest.StreamPatch
	}{
		{"valid above data length", exfattest.StreamPatch{Valid: ptr(uint64(5000))}},
		{"data length above the heap", exfattest.StreamPatch{DataLen: ptr(uint64(1 << 40)), Valid: ptr(uint64(1 << 40))}},
		{"data length above the heap, small valid", exfattest.StreamPatch{DataLen: ptr(uint64(1 << 40)), Valid: ptr(uint64(1))}},
		{"data length above int64", exfattest.StreamPatch{DataLen: ptr(uint64(math.MaxUint64)), Valid: ptr(uint64(1))}},
		{"valid above int64", exfattest.StreamPatch{DataLen: ptr(uint64(math.MaxUint64)), Valid: ptr(uint64(math.MaxUint64))}},
		{"first cluster 0", exfattest.StreamPatch{First: ptr(uint32(0))}},
		{"first cluster 1", exfattest.StreamPatch{First: ptr(uint32(1))}},
		{"first cluster past the heap", exfattest.StreamPatch{First: ptr(uint32(0xFFFFFFF0))}},
		{"extent runs past the heap", exfattest.StreamPatch{First: ptr(uint32(255)), DataLen: ptr(uint64(20000)), Valid: ptr(uint64(20000))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, img, l := recImage(t, []exfattest.File{{Path: "/H.BIN", Data: bytes.Repeat([]byte{1}, 3000), Deleted: true}})
			loc := l.Find("/H.BIN", true)
			exfattest.PatchStream(img, loc, c.patch)
			cs, err := openImg(t, img).Recoverable(recID(loc))
			var ce *filesys.CorruptError
			if !errors.As(err, &ce) || ce.Structure != "recovery map" || cs != nil {
				t.Errorf("= %v, %v; want a recovery map CorruptError", cs, err)
			}
		})
	}
}

// A zero ValidDataLength gives an empty map whatever the first cluster says.
func TestExfatRecoverableZeroValidIgnoresExtent(t *testing.T) {
	_, img, l := recImage(t, []exfattest.File{{Path: "/H.BIN", Data: bytes.Repeat([]byte{1}, 3000), Deleted: true}})
	loc := l.Find("/H.BIN", true)
	exfattest.PatchStream(img, loc, exfattest.StreamPatch{Valid: ptr(uint64(0)), First: ptr(uint32(0xFFFFFFF0))})
	c := one(t, openImg(t, img), loc)
	if c.Size != 0 || len(c.Runs) != 0 {
		t.Errorf("candidate %+v", c)
	}
}

func TestExfatRecoverableEmptyNameFlag(t *testing.T) {
	f, _, l := recImage(t, []exfattest.File{{Path: "/x", Data: []byte("d"), Deleted: true, RawName: []uint16{}}})
	cs, err := f.Recoverable(recID(l.Find("/x", true)))
	if err != nil {
		t.Fatalf("Recoverable: %v", err)
	}
	if !slices.Contains(cs[0].Basis, "name=empty") {
		t.Errorf("basis %v lacks name=empty", cs[0].Basis)
	}
}

// failRange fails every read that overlaps [lo, hi) once armed.
type failRange struct {
	r      *bytes.Reader
	lo, hi int64
	armed  bool
}

var errInjected = errors.New("injected read failure")

func (f *failRange) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off < f.hi && off+int64(len(p)) > f.lo {
		return 0, errInjected
	}
	return f.r.ReadAt(p, off)
}

// TestExfatResolveDirUnreadableSubdirectoryIsNotAbsent (F67): a listing that
// failed during the search makes a not-found target undecidable, never absent.
func TestExfatResolveDirUnreadableSubdirectoryIsNotAbsent(t *testing.T) {
	files := []exfattest.File{{Path: "/D1", Dir: true}, {Path: "/D1/D2", Dir: true}, {Path: "/D1/D2/f.txt", Data: []byte("f")}}
	img, l := exfattest.BuildLayout(exfattest.Options{}, files)
	d1 := l.Find("/D1", false)
	d2 := l.Find("/D1/D2", false)
	fr := &failRange{r: bytes.NewReader(img)}
	fr.lo, fr.hi = l.ClusterOffset(d1.FirstCluster), l.ClusterOffset(d1.FirstCluster)+int64(l.ClusterSize)
	f, err := exfat.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	fr.armed = true
	cs, err := f.Recoverable(filesys.Entry{ID: "dirent:" + strconv.FormatUint(uint64(d2.FirstCluster), 10) + ":0"})
	if cs != nil || err == nil || errors.Is(err, filesys.ErrNotFound) || !errors.Is(err, errInjected) {
		t.Fatalf("Recoverable = %v, %v; want nil and an error wrapping the read failure, not ErrNotFound", cs, err)
	}
}

// TestExfatResolveDirScanCapIsUndecidableNotAbsent (F64): the scan cap is a
// typed filesys.ErrLimit, never "not found".
func TestExfatResolveDirScanCapIsUndecidableNotAbsent(t *testing.T) {
	files := []exfattest.File{{Path: "/D1", Dir: true}, {Path: "/D1/D2", Dir: true}, {Path: "/D1/D2/D3", Dir: true}, {Path: "/b.txt", Data: []byte("b")}}
	img, _ := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	f.SetResolveDirs(2)
	cs, err := f.Recoverable(filesys.Entry{ID: "dirent:77:3"})
	if cs != nil || !errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("Recoverable = %v, %v; want nil and filesys.ErrLimit (not ErrNotFound)", cs, err)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error %q does not name the cap", err)
	}
}

// cutDirImage builds /D1/f.txt and returns a reader whose read of D1's
// directory cluster fails once armed.
func cutDirImage(t *testing.T) (*exfat.FS, *failRange, filesys.Entry) {
	t.Helper()
	files := []exfattest.File{{Path: "/D1", Dir: true}, {Path: "/D1/f.txt", Data: []byte("f")}}
	img, l := exfattest.BuildLayout(exfattest.Options{}, files)
	d1 := l.Find("/D1", false)
	fr := &failRange{r: bytes.NewReader(img)}
	fr.lo, fr.hi = l.ClusterOffset(d1.FirstCluster), l.ClusterOffset(d1.FirstCluster)+int64(l.ClusterSize)
	f, err := exfat.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.Lookup("/D1")
	if err != nil {
		t.Fatal(err)
	}
	fr.armed = true
	return f, fr, e
}

// TestExfatReadDirUnreadableFirstChunkIsAnError (F73): an unreadable listing is
// never an empty directory.
func TestExfatReadDirUnreadableFirstChunkIsAnError(t *testing.T) {
	f, _, d1 := cutDirImage(t)
	es, err := f.ReadDir(d1)
	if err == nil || !errors.Is(err, errInjected) || es != nil {
		t.Fatalf("ReadDir = %v, %v; want nil and an error wrapping the read failure", es, err)
	}
}

// TestExfatLookupMissingNameInCutListingIsAnError (F73): a name not found in a
// listing that could not be read is undecidable, never ErrNotFound.
func TestExfatLookupMissingNameInCutListingIsAnError(t *testing.T) {
	f, _, _ := cutDirImage(t)
	e, err := f.Lookup("/D1/missing.txt")
	if err == nil || errors.Is(err, filesys.ErrNotFound) || !errors.Is(err, errInjected) {
		t.Fatalf("Lookup = %v, %v; want an error wrapping the read failure, not ErrNotFound", e, err)
	}
}

// TestExfatLookupCapCutIsLimitNotAbsent (F73): the per-directory entry cap cuts
// the listing; a missing name is filesys.ErrLimit, not ErrNotFound.
func TestExfatLookupCapCutIsLimitNotAbsent(t *testing.T) {
	files := []exfattest.File{{Path: "/a", Data: []byte("a")}, {Path: "/b", Data: []byte("b")}, {Path: "/c", Data: []byte("c")}}
	img, _ := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	f.SetDirRecordCap(1)
	e, err := f.Lookup("/zzz")
	if !errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("Lookup = %v, %v; want filesys.ErrLimit", e, err)
	}
	if es, err := f.ReadDir(f.Root()); !errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("ReadDir = %v, %v; want filesys.ErrLimit", es, err)
	}
}

// TestExfatRootMetaCutIsAWarning (F73): a root directory read that fails while
// the system entries are loaded is a counted warning visible in Info.
func TestExfatRootMetaCutIsAWarning(t *testing.T) {
	img, l := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/a", Data: []byte("a")}})
	f, err := exfat.Open(&failAll{r: bytes.NewReader(img), root: l.ClusterOffset(l.RootCluster), size: int64(l.ClusterSize)}, int64(len(img)))
	if err != nil {
		t.Skipf("Open fails outright: %v", err)
	}
	if !hasWarn(f, "unreadable") {
		t.Errorf("no warning: %v", f.Info().Warnings)
	}
}

type failAll struct {
	r    *bytes.Reader
	root int64
	size int64
}

func (f *failAll) ReadAt(p []byte, off int64) (int, error) {
	if off < f.root+f.size && off+int64(len(p)) > f.root {
		return 0, errInjected
	}
	return f.r.ReadAt(p, off)
}
