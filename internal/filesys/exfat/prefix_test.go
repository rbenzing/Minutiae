package exfat_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

const prefixCS = 4096 // the builder's default cluster size

// openPrefixFile opens path and returns the file; the open must succeed.
func openPrefixFile(t *testing.T, f *exfat.FS, path string) filesys.File {
	t.Helper()
	e, err := f.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", path, err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	return fl
}

// checkPrefix asserts that fl is a trusted prefix of exactly covered bytes
// that equal want[:covered], and that every read at or beyond it is a corrupt
// error that leaves the buffer untouched.
func checkPrefix(t *testing.T, f *exfat.FS, fl filesys.File, want []byte, covered int64) {
	t.Helper()
	if fl.Size() != int64(len(want)) {
		t.Errorf("Size = %d, want %d", fl.Size(), len(want))
	}
	runs := fl.Runs()
	got, err := filesys.CheckRunsPrefix(runs, fl.Size(), f.Info().Size)
	if err != nil || got != covered {
		t.Fatalf("CheckRunsPrefix = %d, %v; want %d, nil (runs %+v)", got, err, covered, runs)
	}
	if covered < fl.Size() {
		if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err == nil {
			t.Errorf("CheckRuns accepted a prefix of %d of %d bytes", covered, fl.Size())
		}
	}
	buf := make([]byte, covered)
	if n, err := fl.ReadAt(buf, 0); int64(n) != covered || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt(prefix) = %d, %v; want %d bytes", n, err, covered)
	}
	if !bytes.Equal(buf, want[:covered]) {
		t.Errorf("the prefix bytes differ from the written data")
	}
	if covered == fl.Size() {
		return
	}
	for _, off := range []int64{covered, covered + 1} {
		if off >= fl.Size() {
			continue
		}
		p := bytes.Repeat([]byte{0xAA}, 64)
		n, err := fl.ReadAt(p, off)
		if n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadAt at %d = %d, %v; want 0, a corrupt error", off, n, err)
		}
		if !bytes.Equal(p, bytes.Repeat([]byte{0xAA}, 64)) {
			t.Errorf("ReadAt at %d wrote into the buffer", off)
		}
	}
	// A read that starts inside the prefix and crosses its end returns the
	// prefix part and then the error, never zeros.
	if covered > 0 {
		p := bytes.Repeat([]byte{0xAA}, int(min(covered, 100))+200)
		off := covered - min(covered, 100)
		n, err := fl.ReadAt(p, off)
		if int64(n) != covered-off || !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("crossing read = %d, %v; want %d, a corrupt error", n, err, covered-off)
		}
	}
}

func TestExfatShortChainOpensWithPrefix(t *testing.T) {
	data := pattern(3*prefixCS, 3)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data, FatChain: true}})
	cl := lay.Find("/f.bin", false).Clusters
	le32(img, int(lay.FatEntryOffset(cl[1])), 0xFFFFFFFF)
	f := openImg(t, img)
	fl := openPrefixFile(t, f, "/f.bin")
	checkPrefix(t, f, fl, data, 2*prefixCS)
	if !hasWarn(f, "2 of 3 clusters") {
		t.Errorf("no warning naming 2 of 3 clusters: %v", f.Info().Warnings)
	}
}

func TestExfatChainIntoBadEntryOpensWithPrefix(t *testing.T) {
	data := pattern(3*prefixCS, 5)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data, FatChain: true}})
	cl := lay.Find("/f.bin", false).Clusters
	rows := map[string]uint32{
		"free": 0, "reserved": 1, "bad cluster": 0xFFFFFFF7,
		"beyond": lay.ClusterCount + 2, "huge": 0xFFFFFFF0, "loop": cl[0],
	}
	for name, v := range rows {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Clone(img)
			le32(bad, int(lay.FatEntryOffset(cl[0])), v)
			f := openImg(t, bad)
			fl := openPrefixFile(t, f, "/f.bin")
			checkPrefix(t, f, fl, data, prefixCS)
			if !hasWarn(f, "1 of 3 clusters") {
				t.Errorf("no warning naming 1 of 3 clusters: %v", f.Info().Warnings)
			}
		})
	}
}

func TestExfatNoFatChainPastTruncatedImageOpensWithPrefix(t *testing.T) {
	data := pattern(4*prefixCS, 7)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data}})
	cl := lay.Find("/f.bin", false).Clusters
	cut := img[:lay.ClusterOffset(cl[1])+prefixCS]
	f := openImg(t, cut)
	fl := openPrefixFile(t, f, "/f.bin")
	checkPrefix(t, f, fl, data, 2*prefixCS)
	if !hasWarn(f, "VolumeLength") {
		t.Errorf("the VolumeLength clamp warning is gone: %v", f.Info().Warnings)
	}
	if !hasWarn(f, "2 of 4 clusters") {
		t.Errorf("no warning naming 2 of 4 clusters: %v", f.Info().Warnings)
	}
}

func TestExfatTruncatedImageFirstClusterCutHasNoRuns(t *testing.T) {
	a, b := pattern(prefixCS, 1), pattern(3*prefixCS, 2)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/a.bin", Data: a}, {Path: "/b.bin", Data: b},
	})
	cut := img[:lay.ClusterOffset(lay.Find("/b.bin", false).FirstCluster)]
	f := openImg(t, cut)
	if fa := openPrefixFile(t, f, "/a.bin"); fa.Size() != prefixCS {
		t.Fatalf("the file before the cut changed: size %d", fa.Size())
	}
	fl := openPrefixFile(t, f, "/b.bin")
	if fl.Size() != int64(len(b)) || fl.Runs() != nil {
		t.Fatalf("Size = %d, Runs = %+v; want %d, nil", fl.Size(), fl.Runs(), len(b))
	}
	checkPrefix(t, f, fl, b, 0)
	if f.DeclaredClusterCount() <= f.ClusterCount() {
		t.Errorf("declared %d clusters, clamped %d: the cut did not clamp", f.DeclaredClusterCount(), f.ClusterCount())
	}
}

// Pins existing behaviour. Mutation: give an empty prefix for any first
// cluster and this fails.
func TestExfatInvalidFirstClusterStillCorrupt(t *testing.T) {
	data := pattern(2*prefixCS, 9)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/n.bin", Data: data}, {Path: "/c.bin", Data: data, FatChain: true},
	})
	for _, path := range []string{"/n.bin", "/c.bin"} {
		for _, first := range []uint32{0, 1, 0xFFFFFFFF, lay.ClusterCount + 2} {
			bad := bytes.Clone(img)
			exfattest.PatchStream(bad, lay.Find(path, false), exfattest.StreamPatch{First: &first})
			f := openImg(t, bad)
			e, err := f.Lookup(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("%s first cluster %d: Open = %v, want a corrupt error", path, first, err)
			}
		}
	}
}

// Pins existing behaviour. Mutation: compare DataLength with the clamped
// instead of the declared heap: the truncated rows below then fail to open.
func TestExfatDeclaredSizeInconsistenciesStillCorrupt(t *testing.T) {
	data := pattern(2*prefixCS, 11)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data}})
	loc := lay.Find("/f.bin", false)
	heap := uint64(lay.ClusterCount) * prefixCS
	u := func(v uint64) *uint64 { return &v }
	rows := []struct {
		name string
		p    exfattest.StreamPatch
	}{
		{"above the declared heap", exfattest.StreamPatch{DataLen: u(heap + 1)}},
		{"1<<63", exfattest.StreamPatch{DataLen: u(1 << 63)}},
		{"all ones", exfattest.StreamPatch{DataLen: u(^uint64(0))}},
		{"valid above data", exfattest.StreamPatch{Valid: u(2*prefixCS + 1)}},
		{"valid all ones", exfattest.StreamPatch{Valid: u(^uint64(0))}},
	}
	for _, r := range rows {
		bad := bytes.Clone(img)
		exfattest.PatchStream(bad, loc, r.p)
		// A truncated copy keeps the declared heap: still corrupt.
		for _, image := range [][]byte{bad, bad[:lay.ClusterOffset(loc.FirstCluster)+prefixCS]} {
			f := openImg(t, image)
			e, err := f.Lookup("/f.bin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("%s (image %d bytes): Open = %v, want a corrupt error", r.name, len(image), err)
			}
		}
	}
	// A DataLength the declared heap holds but the cut image does not is a
	// prefix, not corrupt.
	bad := bytes.Clone(img)
	exfattest.PatchStream(bad, loc, exfattest.StreamPatch{DataLen: u(heap), Valid: u(heap)})
	f := openImg(t, bad[:lay.ClusterOffset(loc.FirstCluster)+prefixCS])
	if _, err := f.Open(mustLookup(t, f, "/f.bin")); err != nil {
		t.Errorf("DataLength inside the declared heap on a cut image: Open = %v, want a prefix", err)
	}
}

func mustLookup(t *testing.T, f *exfat.FS, path string) filesys.Entry {
	t.Helper()
	e, err := f.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", path, err)
	}
	return e
}

func TestExfatPrefixHoleRule(t *testing.T) {
	data := pattern(3*prefixCS, 13)
	valid := int64(prefixCS + prefixCS/2)
	build := func() ([]byte, *exfattest.Layout) {
		return exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data, FatChain: true, ValidLength: &valid}})
	}
	// Chain cut after cluster 2: the prefix ends at ValidDataLength.
	img, lay := build()
	cl := lay.Find("/f.bin", false).Clusters
	le32(img, int(lay.FatEntryOffset(cl[1])), 0xFFFFFFFF)
	f := openImg(t, img)
	fl := openPrefixFile(t, f, "/f.bin")
	for _, r := range fl.Runs() {
		if r.Offset < 0 {
			t.Errorf("a short chain presented a hole run: %+v", fl.Runs())
		}
	}
	checkPrefix(t, f, fl, data, valid)
	// Whole chain: the hole run is there and reads zeros.
	img, _ = build()
	f = openImg(t, img)
	fl = openPrefixFile(t, f, "/f.bin")
	runs := fl.Runs()
	if len(runs) == 0 || runs[len(runs)-1].Offset != -1 || runs[len(runs)-1].Length != 3*prefixCS-valid {
		t.Fatalf("whole chain: runs %+v, want a trailing hole of %d", runs, 3*prefixCS-valid)
	}
	if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}
	buf := make([]byte, fl.Size())
	if n, err := fl.ReadAt(buf, 0); int64(n) != fl.Size() || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(buf[:valid], data[:valid]) || !bytes.Equal(buf[valid:], make([]byte, int64(len(buf))-valid)) {
		t.Errorf("the valid part or the zero hole is wrong")
	}
}

func TestExfatTruncatedFixtureEqualsUntruncatedPrefix(t *testing.T) {
	path := filepath.Join("testdata", "exfat.img.gz")
	want := loadOracle(t, path)
	img := gunzipFixture(t, path)
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s", got, want.Generator.ImageSHA256)
	}
	// The fixture is mostly free space, so "three quarters into the heap" is
	// taken over the clusters the live files use: the cut then falls inside the
	// used area and splits the files.
	var highest int64
	for _, w := range want.Files {
		for _, c := range w.Clusters {
			highest = max(highest, c[1])
		}
	}
	cutCluster := 2 + (highest-1)*3/4
	// Directories stay before the cut so every path resolves; only file data is cut.
	for _, w := range want.Files {
		if w.Type == "dir" {
			for _, c := range w.Clusters {
				cutCluster = max(cutCluster, c[1]+1)
			}
		}
	}
	cutOff := want.DataStart + (cutCluster-2)*int64(want.BlockSize)
	full := openImg(t, img)
	cut := openImg(t, img[:cutOff])
	var whole, partial int
	for _, w := range want.Files {
		if w.Type != "file" || w.Size == 0 {
			continue
		}
		var last int64
		for _, c := range w.Clusters {
			last = max(last, c[1])
		}
		fe, err := full.Lookup(w.Path)
		if err != nil {
			t.Fatalf("uncut Lookup(%q): %v", w.Path, err)
		}
		fullData := readAll(t, full, fe)
		fl := openPrefixFile(t, cut, w.Path)
		if last < cutCluster {
			whole++
			buf := make([]byte, fl.Size())
			if n, err := fl.ReadAt(buf, 0); n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
				t.Fatalf("%s: ReadAt = %d, %v", w.Path, n, err)
			}
			if s := sha256.Sum256(buf); hex.EncodeToString(s[:]) != w.SHA256 {
				t.Errorf("%s: wholly before the cut but differs from the oracle", w.Path)
			}
			continue
		}
		partial++
		covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), cut.Info().Size)
		if err != nil || covered >= fl.Size() {
			t.Fatalf("%s: covered %d of %d, %v", w.Path, covered, fl.Size(), err)
		}
		checkPrefix(t, cut, fl, fullData, covered)
	}
	if whole == 0 || partial == 0 {
		t.Fatalf("whole=%d partial=%d: the cut must produce both kinds of file", whole, partial)
	}
}

func TestExfatPrefixNeverAllocatesFromSize(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: pattern(2*prefixCS, 15)}})
	le32(img, 92, 1<<24) // ClusterCount: the declared heap is 1<<24 clusters = 64 GiB
	exfattest.FixBootChecksum(img)
	size, valid := uint64(1)<<36, uint64(prefixCS)
	exfattest.PatchStream(img, lay.Find("/f.bin", false), exfattest.StreamPatch{DataLen: &size, Valid: &valid})
	f := openImg(t, img)
	e := mustLookup(t, f, "/f.bin")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fl, err := f.Open(e)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if fl.Size() != 1<<36 {
		t.Errorf("Size = %d", fl.Size())
	}
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered == 0 || covered >= fl.Size() {
		t.Errorf("covered = %d, %v; want a short prefix", covered, err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("Open allocated %d bytes for a declared size of 1<<36", grew)
	}
	if !strings.Contains(strings.Join(f.Info().Warnings, "\n"), "clusters") {
		t.Errorf("no warning about the short chain: %v", f.Info().Warnings)
	}
}
