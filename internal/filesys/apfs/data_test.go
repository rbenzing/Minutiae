package apfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

const bs = 4096

// pattern returns n deterministic non-zero bytes.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+int(seed)*7) | 1
	}
	return b
}

// openPath opens the file at path (a path under the volume "Data").
func openPath(t *testing.T, f *apfs.FS, p string) filesys.File {
	t.Helper()
	fl, err := f.Open(mustLookup(t, f, p))
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	return fl
}

// readAllAt reads the whole file in 64 KiB chunks.
func readAllAt(t *testing.T, fl filesys.File) []byte {
	t.Helper()
	if fl.Size() > 64<<20 {
		t.Fatalf("file of %d bytes is too large to read whole", fl.Size())
	}
	out := make([]byte, 0, fl.Size())
	buf := make([]byte, 65536)
	for off := int64(0); off < fl.Size(); {
		n, err := fl.ReadAt(buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if err != nil && (!errors.Is(err, io.EOF) || off != fl.Size()) {
			t.Fatalf("ReadAt at %d: %v", off, err)
		}
		if n == 0 {
			t.Fatalf("ReadAt at %d made no progress", off)
		}
	}
	return out
}

// dataStart is the first byte of the file's data blocks in the image.
func dataStart(im *image, path string) int64 {
	return int64(im.g.Volumes[0].DataStart[path]) * bs
}

func noWarnings(t *testing.T, f *apfs.FS) {
	t.Helper()
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

func TestSingleExtentFile(t *testing.T) {
	data := pattern(2*bs+1000, 1)
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/one", Data: data})))
	fl := openPath(t, f, "/Data/one")
	if fl.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", fl.Size(), len(data))
	}
	want := []filesys.Run{{Offset: dataStart(im, "/one"), Length: int64(len(data))}}
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v (trimmed to the size, not the block end)", got, want)
	}
	if got := readAllAt(t, fl); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	// A read that runs over the end returns the bytes and io.EOF; at the end, EOF.
	buf := make([]byte, 100)
	n, err := fl.ReadAt(buf, int64(len(data))-40)
	if n != 40 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:40], data[len(data)-40:]) {
		t.Errorf("ReadAt over the end = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf, int64(len(data))); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := fl.ReadAt(buf, -1); err == nil {
		t.Error("negative offset accepted")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}
	noWarnings(t, f)
}

func TestMultiExtentFileLogicalOrder(t *testing.T) {
	data := pattern(6*bs, 2)
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/frag", Data: data, Fragments: 3})))
	fl := openPath(t, f, "/Data/frag")
	runs := fl.Runs()
	if len(runs) != 3 {
		t.Fatalf("Runs = %v, want 3 runs", runs)
	}
	start := dataStart(im, "/frag")
	// Logical order is file order; physical placement is the reverse.
	want := []filesys.Run{{Offset: start + 4*bs, Length: 2 * bs}, {Offset: start + 2*bs, Length: 2 * bs}, {Offset: start, Length: 2 * bs}}
	if !slices.Equal(runs, want) {
		t.Errorf("Runs = %v, want %v", runs, want)
	}
	if got := readAllAt(t, fl); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	// A read across an extent boundary.
	buf := make([]byte, 3000)
	if n, err := fl.ReadAt(buf, 2*bs-1500); n != 3000 || err != nil || !bytes.Equal(buf, data[2*bs-1500:2*bs+1500]) {
		t.Errorf("ReadAt across the boundary = %d, %v", n, err)
	}
	noWarnings(t, f)
}

func TestAdjacentExtentsAreMerged(t *testing.T) {
	data := pattern(6*bs, 3)
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/split", Data: data, SplitContig: 3})))
	fl := openPath(t, f, "/Data/split")
	want := []filesys.Run{{Offset: dataStart(im, "/split"), Length: 6 * bs}}
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want one merged run %v", got, want)
	}
	if !bytes.Equal(readAllAt(t, fl), data) {
		t.Error("content differs")
	}
}

func TestSparseHolesExplicitAndGaps(t *testing.T) {
	// Eight blocks: data, explicit hole, data x2, gap x2, data x2; then a hole tail.
	data := pattern(8*bs, 4)
	clear(data[1*bs : 2*bs])
	clear(data[4*bs : 6*bs])
	files := []apfstest.File{
		{Path: "/sparse", Data: data, Holes: [][2]int64{{1 * bs, bs}}, Gaps: [][2]int64{{4 * bs, 2 * bs}}, SparseTail: 2 * bs},
		// An explicit hole next to a gap is one hole run.
		{Path: "/holegap", Data: slices.Concat(pattern(bs, 5), make([]byte, 2*bs), pattern(bs, 6)), Holes: [][2]int64{{bs, bs}}, Gaps: [][2]int64{{2 * bs, bs}}},
	}
	f, im := openOpts(t, volOpts(dataVolume(files...)))
	s := dataStart(im, "/sparse")
	fl := openPath(t, f, "/Data/sparse")
	want := []filesys.Run{
		{Offset: s, Length: bs},
		{Offset: -1, Length: bs},
		{Offset: s + bs, Length: 2 * bs},
		{Offset: -1, Length: 2 * bs},
		{Offset: s + 3*bs, Length: 2 * bs},
		{Offset: -1, Length: 2 * bs},
	}
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Errorf("Runs = %v\nwant   %v", got, want)
	}
	exp := slices.Concat(data, make([]byte, 2*bs))
	if got := readAllAt(t, fl); !bytes.Equal(got, exp) {
		t.Error("content differs (holes must read as zeros)")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}

	h := dataStart(im, "/holegap")
	hl := openPath(t, f, "/Data/holegap")
	wantH := []filesys.Run{{Offset: h, Length: bs}, {Offset: -1, Length: 2 * bs}, {Offset: h + bs, Length: bs}}
	if got := hl.Runs(); !slices.Equal(got, wantH) {
		t.Errorf("hole+gap Runs = %v, want %v", got, wantH)
	}
	noWarnings(t, f)
}

func TestExtentBeyondSizeIsIgnored(t *testing.T) {
	// Size 100: the first extent covers two blocks (the run is trimmed to 100), the
	// second lies entirely past the end and is not even validated.
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{
		Path: "/f", Data: pattern(100, 7),
		Extents: []apfstest.Extent{
			{Logical: 0, Length: 2 * bs, Phys: 0, Rel: true},
			{Logical: 8 * bs, Length: bs, Phys: 1 << 50},
		},
	})))
	fl := openPath(t, f, "/Data/f")
	want := []filesys.Run{{Offset: dataStart(im, "/f"), Length: 100}}
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v", got, want)
	}
	if !bytes.Equal(readAllAt(t, fl), pattern(100, 7)) {
		t.Error("content differs")
	}
	noWarnings(t, f)
}

func TestEmptyFile(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/empty"},
		apfstest.File{Path: "/empty2", Data: []byte{}},
	)))
	for _, p := range []string{"/Data/empty", "/Data/empty2"} {
		fl := openPath(t, f, p)
		if fl.Size() != 0 || fl.Runs() != nil {
			t.Errorf("%s: Size %d, Runs %v; want an empty file with nil runs", p, fl.Size(), fl.Runs())
		}
		if n, err := fl.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt = %d, %v", p, n, err)
		}
	}
	noWarnings(t, f)
}

func TestAllHoleFile(t *testing.T) {
	size := int64(5*bs + 17)
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/none", Size: size},
		apfstest.File{Path: "/explicit", Size: size, Extents: []apfstest.Extent{{Logical: 0, Length: 6 * bs, Phys: 0}}},
	)))
	for _, p := range []string{"/Data/none", "/Data/explicit"} {
		fl := openPath(t, f, p)
		if want := []filesys.Run{{Offset: -1, Length: size}}; !slices.Equal(fl.Runs(), want) {
			t.Errorf("%s: Runs = %v, want %v", p, fl.Runs(), want)
		}
		if got := readAllAt(t, fl); len(got) != int(size) || !bytes.Equal(got, make([]byte, size)) {
			t.Errorf("%s: an all-hole file must read as zeros", p)
		}
	}
	noWarnings(t, f)
}

func TestClonedFileReadsViaPrivateID(t *testing.T) {
	data := pattern(3*bs+5, 8)
	f, im := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/orig", Data: data},
		apfstest.File{Path: "/clone1", Clone: "/orig"},
		apfstest.File{Path: "/clone2", Clone: "/orig"},
	)))
	ino := im.g.Volumes[0].Inodes
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	for _, n := range []string{"clone1", "clone2"} {
		e := byName(t, es, n)
		if v, ok := attr(e, "private_id"); !ok || v != fmt.Sprint(ino["/orig"]) {
			t.Errorf("%s private_id = %q, %v; want %d", n, v, ok, ino["/orig"])
		}
		if e.Size != int64(len(data)) {
			t.Errorf("%s Size = %d", n, e.Size)
		}
	}
	orig := openPath(t, f, "/Data/orig")
	for _, n := range []string{"clone1", "clone2"} {
		fl := openPath(t, f, "/Data/"+n)
		if !bytes.Equal(readAllAt(t, fl), data) {
			t.Errorf("%s reads other bytes than its source", n)
		}
		if !slices.Equal(fl.Runs(), orig.Runs()) {
			t.Errorf("%s Runs = %v, want %v", n, fl.Runs(), orig.Runs())
		}
	}
	noWarnings(t, f)
}

func TestSymlinkOpenReturnsTarget(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/ok", Symlink: "docs/a.txt"},
		apfstest.File{Path: "/stream", Symlink: "x", SymlinkStream: true},
		apfstest.File{Path: "/notarget", Mode: 0o120777},
	)))
	fl := openPath(t, f, "/Data/ok")
	if fl.Size() != int64(len("docs/a.txt")) || fl.Runs() != nil {
		t.Errorf("Size %d, Runs %v", fl.Size(), fl.Runs())
	}
	if got := readAllAt(t, fl); string(got) != "docs/a.txt" {
		t.Errorf("target = %q", got)
	}
	if _, err := f.Open(mustLookup(t, f, "/Data/stream")); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("stream symlink: %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(mustLookup(t, f, "/Data/notarget")); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("symlink without a target: %v, want ErrCorrupt", err)
	}
}

func TestCompressedFileUnsupported(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/c", Data: pattern(50, 1), CompressedFlag: true, UncompressedSize: 9999, Xattrs: []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(3)}}})))
	e := mustLookup(t, f, "/Data/c")
	if v, ok := attr(e, "compressed"); !ok || v != "zlib-attr" || e.Size != 9999 {
		t.Errorf("entry %+v: the listing must still show the compressed file", e)
	}
	_, err := f.Open(e)
	if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "decmpfs-compressed file") {
		t.Errorf("Open = %v, want ErrUnsupported (decmpfs-compressed file)", err)
	}
}

func TestDirectoryOpenIsUnsupported(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(richFiles()...)))
	for _, e := range []filesys.Entry{mustLookup(t, f, "/Data"), mustLookup(t, f, "/Data/docs"), f.Root()} {
		_, err := f.Open(e)
		if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "directory") {
			t.Errorf("Open(%q) = %v, want ErrUnsupported (directory)", e.ID, err)
		}
	}
}

func TestOpenSpecialFileIsUnsupported(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/fifo", Mode: 0o010644})))
	if _, err := f.Open(mustLookup(t, f, "/Data/fifo")); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(fifo) = %v, want ErrUnsupported", err)
	}
}

func TestEncryptedVolumeFileOpenIsErrEncrypted(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(), apfstest.Volume{Name: "Enc", Encrypted: true}))
	for _, id := range []string{apfs.NodeID(1, 0, 2), apfs.NodeID(1, 0, 16)} {
		if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrEncrypted) {
			t.Errorf("Open(%s) = %v, want ErrEncrypted", id, err)
		}
	}
}

func TestPerFileCryptoIDAnomaly(t *testing.T) {
	const unassigned = ^uint64(0)
	data := pattern(2*bs, 9)
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/plain", Data: data},
		apfstest.File{Path: "/sw", Data: data, CryptoID: 4, ExtentCrypto: 4},
		apfstest.File{Path: "/keyed", Data: data, CryptoID: 5, ExtentCrypto: 5},
		apfstest.File{Path: "/unassigned", Data: data, CryptoID: unassigned, ExtentCrypto: unassigned},
		apfstest.File{Path: "/extentonly", Data: data, ExtentCrypto: 7},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	for n, want := range map[string]bool{"plain": false, "sw": false, "keyed": true, "unassigned": true} {
		if got := byName(t, es, n).Encrypted; got != want {
			t.Errorf("%s: Encrypted = %v, want %v", n, got, want)
		}
	}
	for _, n := range []string{"plain", "sw"} {
		fl := openPath(t, f, "/Data/"+n)
		if !bytes.Equal(readAllAt(t, fl), data) {
			t.Errorf("%s: content differs", n)
		}
	}
	noWarnings(t, f)
	for _, n := range []string{"keyed", "unassigned", "extentonly"} {
		before := len(f.Info().Warnings)
		fl := openPath(t, f, "/Data/"+n)
		// The bytes are returned as stored (ciphertext), never decrypted.
		if !bytes.Equal(readAllAt(t, fl), data) {
			t.Errorf("%s: Open must return the stored bytes", n)
		}
		if len(f.Info().Warnings) != before+1 || !hasWarn(f, "per-file encryption") {
			t.Errorf("%s: warnings %q", n, f.Info().Warnings)
		}
	}
}

// brokenFile builds a file of six blocks whose first two extents are good and
// whose later records come from tail; the readable prefix is the first two
// blocks.
func brokenFile(tail ...apfstest.Extent) apfstest.File {
	ex := []apfstest.Extent{{Logical: 0, Length: 2 * bs, Phys: 0, Rel: true}}
	return apfstest.File{Path: "/b", Data: pattern(6*bs, 10), Extents: append(ex, tail...)}
}

// wantPrefix checks the trusted-prefix contract of a file whose first covered
// bytes are the first n bytes of data.
func wantPrefix(t *testing.T, f *apfs.FS, fl filesys.File, data []byte, n int64) {
	t.Helper()
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered != n {
		t.Fatalf("CheckRunsPrefix = %d, %v; runs %v; want a prefix of %d bytes", covered, err, fl.Runs(), n)
	}
	buf := make([]byte, n)
	if got, err := fl.ReadAt(buf, 0); got != int(n) || err != nil || !bytes.Equal(buf, data[:n]) {
		t.Errorf("reading the prefix = %d, %v", got, err)
	}
	// Reads at or past the prefix end are errors, never zeros.
	big := make([]byte, 100)
	for _, off := range []int64{n, n + 1, fl.Size() - 1} {
		if got, err := fl.ReadAt(big, off); got != 0 || !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadAt(%d) beyond the prefix = %d, %v; want 0, ErrCorrupt", off, got, err)
		}
	}
	// A read that starts inside the prefix returns its bytes, then the error.
	if n < 10 {
		return
	}
	got, err := fl.ReadAt(big, n-10)
	if got != 10 || !errors.Is(err, filesys.ErrCorrupt) || !bytes.Equal(big[:10], data[n-10:n]) {
		t.Errorf("ReadAt across the prefix end = %d, %v", got, err)
	}
}

func TestBrokenExtentsKeepTrustedPrefix(t *testing.T) {
	good2 := apfstest.Extent{Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2}
	cases := []struct {
		name   string
		tail   []apfstest.Extent
		prefix int64 // bytes readable
		warn   string
	}{
		{"overlap", []apfstest.Extent{{Logical: bs, Length: 2 * bs, Rel: true, Phys: 2}}, 2 * bs, "overlap"},
		{"backwards", []apfstest.Extent{good2, {Logical: 0, Length: bs, Rel: true, Phys: 4}}, 4 * bs, "out of key order"},
		{"unaligned", []apfstest.Extent{{Logical: 2*bs + 1, Length: bs, Rel: true, Phys: 2}}, 2 * bs, "aligned"},
		{"zero length", []apfstest.Extent{{Logical: 2 * bs, Length: 0, Rel: true, Phys: 2}}, 2 * bs, "length"},
		{"length not a block multiple", []apfstest.Extent{{Logical: 2 * bs, Length: bs + 1, Rel: true, Phys: 2}}, 2 * bs, "length"},
		{"physical range outside", []apfstest.Extent{{Logical: 2 * bs, Length: bs, Phys: 1 << 20}}, 2 * bs, "container"},
		{"physical overflow", []apfstest.Extent{{Logical: 2 * bs, Length: bs, Phys: 1 << 63}}, 2 * bs, "container"},
		{"gap before a broken extent", []apfstest.Extent{{Logical: 3 * bs, Length: 0, Rel: true, Phys: 3}}, 2 * bs, "length"},
		{"broken after a hole", []apfstest.Extent{{Logical: 2 * bs, Length: bs}, {Logical: 3 * bs, Length: bs, Phys: 1 << 20}}, 3 * bs, "container"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := brokenFile(c.tail...)
			f, _ := openOpts(t, volOpts(dataVolume(file)))
			fl := openPath(t, f, "/Data/b")
			if fl.Size() != 6*bs {
				t.Fatalf("Size = %d", fl.Size())
			}
			// The data under a hole reads as zeros, so compare against that.
			exp := slices.Clone(file.Data)
			if c.name == "broken after a hole" {
				clear(exp[2*bs : 3*bs])
			}
			wantPrefix(t, f, fl, exp, c.prefix)
			if !hasWarn(f, c.warn) {
				t.Errorf("warnings %q lack %q", f.Info().Warnings, c.warn)
			}
		})
	}
}

func TestMalformedExtentRecordsKeepTrustedPrefix(t *testing.T) {
	data := pattern(6*bs, 11)
	for _, c := range []struct {
		name     string
		key, val []byte
	}{
		{"short value", apfstestKey(2 * bs), make([]byte, 23)},
		{"short key", make([]byte, 0), make([]byte, 24)},
		{"long key", append(apfstestKey(2*bs), 0), make([]byte, 24)},
		{"long value", apfstestKey(2 * bs), make([]byte, 3000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{{Logical: 0, Length: 2 * bs, Rel: true}}})
			v.Extra = []apfstest.FSRecord{{ID: 40, Type: apfstest.TypeFileExtent, Key: c.key, Val: c.val}}
			f, _ := openOpts(t, volOpts(v))
			wantPrefix(t, f, openPath(t, f, "/Data/b"), data, 2*bs)
			if len(f.Info().Warnings) == 0 {
				t.Error("no warning")
			}
		})
	}
}

// apfstestKey is the part of an extent key after the header: the logical
// address.
func apfstestKey(logical uint64) []byte {
	k := make([]byte, 8)
	for i := range k {
		k[i] = byte(logical >> (8 * i))
	}
	return k
}

// A tree that is out of key order stops a scan early: the file must not turn
// into "data then a hole tail" because its later extents were not reached.
func TestOutOfOrderExtentRecordsAreNotAHoleTail(t *testing.T) {
	data := pattern(4*bs, 12)
	v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true}, {Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
	}})
	v.Reorder = func(recs []apfstest.FSRecord) []apfstest.FSRecord {
		// Put a record of a smaller object id between the two extents: the scan
		// meets a record that sorts before the range after one inside it.
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].ID == 40 && recs[i].Type == apfstest.TypeFileExtent {
				out := slices.Clone(recs)
				return slices.Insert(out, i, recs[0])
			}
		}
		t.Fatal("no extent record")
		return recs
	}
	v.Files = append(v.Files, apfstest.File{Path: "/later", Data: pattern(10, 1)})
	f, _ := openOpts(t, volOpts(v))
	wantPrefix2(t, f, openPath(t, f, "/Data/b"))
}

func wantPrefix2(t *testing.T, f *apfs.FS, fl filesys.File) {
	t.Helper()
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered >= fl.Size() {
		t.Fatalf("CheckRunsPrefix = %d, %v (size %d); the file must be a strict prefix", covered, err, fl.Size())
	}
	if _, err := fl.ReadAt(make([]byte, 10), fl.Size()-10); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read of the unreached tail: %v, want ErrCorrupt", err)
	}
	if !hasWarn(f, "out of key order") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

func TestInvalidPrivateIDKeepsEmptyPrefix(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/p", Data: pattern(bs, 1), PrivateID: 1 << 61})))
	fl := openPath(t, f, "/Data/p")
	wantPrefix(t, f, fl, pattern(bs, 1), 0)
	if !hasWarn(f, "private id") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

func TestRunCapIsCorrupt(t *testing.T) {
	if apfs.MaxFileRuns != 1<<20 {
		t.Fatalf("MaxFileRuns = %d, want %d", apfs.MaxFileRuns, 1<<20)
	}
	data := pattern(40*bs, 13)
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/many", Data: data, Fragments: 40})))
	f.SetMaxFileRuns(40)
	fl := openPath(t, f, "/Data/many")
	if len(fl.Runs()) != 40 {
		t.Fatalf("%d runs, want exactly the cap of 40 to pass", len(fl.Runs()))
	}
	noWarnings(t, f)

	f.SetMaxFileRuns(10)
	fl = openPath(t, f, "/Data/many")
	if len(fl.Runs()) != 10 {
		t.Fatalf("%d runs, want the 10 allowed", len(fl.Runs()))
	}
	wantPrefix(t, f, fl, data, 10*bs)
	if !hasWarn(f, "more than 10 runs") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// failingReader fails every read that touches [from, to) once armed.
type failingReader struct {
	r        io.ReaderAt
	from, to int64
	armed    bool
	err      error
}

func (r *failingReader) ReadAt(p []byte, off int64) (int, error) {
	if r.armed && off < r.to && off+int64(len(p)) > r.from {
		return 0, r.err
	}
	return r.r.ReadAt(p, off)
}

func TestDataIOErrorIsNotCorruption(t *testing.T) {
	data := pattern(3*bs, 14)
	im := newImage(t, volOpts(dataVolume(apfstest.File{Path: "/d", Data: data})))
	start := dataStart(im, "/d")
	boom := errors.New("device went away")
	fr := &failingReader{r: bytes.NewReader(im.b), from: start + bs, to: start + 2*bs, err: boom}
	f, err := apfs.Open(fr, int64(len(im.b)))
	if err != nil {
		t.Fatal(err)
	}
	fr.armed = true
	fl := openPath(t, f, "/Data/d")
	buf := make([]byte, bs)
	if n, err := fl.ReadAt(buf, 0); n != bs || err != nil {
		t.Fatalf("read of the healthy first block: %d, %v", n, err)
	}
	n, err := fl.ReadAt(buf, bs)
	if !errors.Is(err, boom) || errors.Is(err, filesys.ErrCorrupt) || n != 0 {
		t.Errorf("ReadAt = %d, %v; want the I/O error itself, not a corruption verdict", n, err)
	}
}

func TestTruncatedImageKeepsReadablePrefix(t *testing.T) {
	data := pattern(4*bs, 15)
	im := newImage(t, volOpts(dataVolume(apfstest.File{Path: "/t", Data: data, Fragments: 2})))
	start := dataStart(im, "/t")
	// Fragments place the second half first: cutting after one block of the data
	// area keeps the second logical half's first block (the first physical one)
	// only if the cut is after it; cut inside the first physical extent.
	cut := start + 2*bs // physical blocks 0-1 (the logical second half) survive
	f, err := apfs.Open(bytes.NewReader(im.b[:cut]), cut)
	if err != nil {
		t.Fatal(err)
	}
	fl := openPath(t, f, "/Data/t")
	// The first logical extent lies in the cut-off part: nothing is trusted.
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered != 0 {
		t.Fatalf("covered %d, %v; runs %v", covered, err, fl.Runs())
	}
	if _, err := fl.ReadAt(make([]byte, 10), 0); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("ReadAt = %v, want ErrCorrupt", err)
	}
}

func TestRunsReproduceContent(t *testing.T) {
	files := []apfstest.File{
		{Path: "/a", Data: pattern(5*bs+3, 20), Fragments: 3},
		{Path: "/b", Data: slices.Concat(pattern(bs, 21), make([]byte, bs), pattern(2*bs+9, 22)), Holes: [][2]int64{{bs, bs}}, SparseTail: 3*bs + 1},
		{Path: "/c", Data: pattern(bs, 23), SplitContig: 1},
		{Path: "/d", Size: 7 * bs},
	}
	f, im := openOpts(t, volOpts(dataVolume(files...)))
	for _, file := range files {
		fl := openPath(t, f, "/Data"+file.Path)
		if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
			t.Fatalf("%s: %v", file.Path, err)
		}
		var fromRuns []byte
		for _, r := range fl.Runs() {
			if r.Offset < 0 {
				fromRuns = append(fromRuns, make([]byte, r.Length)...)
			} else {
				fromRuns = append(fromRuns, im.b[r.Offset:r.Offset+r.Length]...)
			}
		}
		if got := readAllAt(t, fl); !bytes.Equal(got, fromRuns) {
			t.Errorf("%s: ReadAt and the bytes at the runs differ", file.Path)
		}
	}
}

func TestSizeLargerThanExtentsIsAHoleTail(t *testing.T) {
	data := pattern(2*bs, 24)
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/s", Data: data, SparseTail: 3*bs + 5})))
	fl := openPath(t, f, "/Data/s")
	want := []filesys.Run{{Offset: dataStart(im, "/s"), Length: 2 * bs}, {Offset: -1, Length: 3*bs + 5}}
	if !slices.Equal(fl.Runs(), want) {
		t.Errorf("Runs = %v, want %v", fl.Runs(), want)
	}
	buf := make([]byte, 20)
	if n, err := fl.ReadAt(buf, 4*bs); n != 20 || err != nil || !bytes.Equal(buf, make([]byte, 20)) {
		t.Errorf("tail read = %d, %v, %v", n, err, buf)
	}
	noWarnings(t, f)
}

func TestHugeSizeDoesNotAllocate(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/huge", Size: 1 << 62},
		apfstest.File{Path: "/hugetail", Data: pattern(bs, 25), SparseTail: 1<<62 - bs},
	)))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, p := range []string{"/Data/huge", "/Data/hugetail"} {
		fl := openPath(t, f, p)
		if fl.Size() != 1<<62 {
			t.Errorf("%s: Size = %d", p, fl.Size())
		}
		if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		buf := make([]byte, 64)
		if n, err := fl.ReadAt(buf, 1<<61); n != 64 || err != nil {
			t.Errorf("%s: ReadAt = %d, %v", p, n, err)
		}
		if n, err := fl.ReadAt(buf, 1<<62-10); n != 10 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt at the end = %d, %v", p, n, err)
		}
	}
	runtime.ReadMemStats(&after)
	if d := after.TotalAlloc - before.TotalAlloc; d > 16<<20 {
		t.Errorf("a file of 2^62 bytes cost %d bytes of allocation", d)
	}
}

func TestOpenIsConcurrentSafe(t *testing.T) {
	data := pattern(8*bs, 26)
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/c", Data: data, Fragments: 4},
		apfstest.File{Path: "/link", Symlink: "target"},
	)))
	e := mustLookup(t, f, "/Data/c")
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				fl, err := f.Open(e)
				if err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
				buf := make([]byte, 5000)
				if n, err := fl.ReadAt(buf, int64(g)*1000); n != 5000 || err != nil || !bytes.Equal(buf, data[g*1000:g*1000+5000]) {
					t.Errorf("goroutine %d: ReadAt = %d, %v", g, n, err)
					return
				}
				if _, err := f.Open(filesys.Entry{ID: apfs.NodeID(0, 0, 2)}); !errors.Is(err, filesys.ErrUnsupported) {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	noWarnings(t, f)
}

func TestOpenForgedEntriesAndIDs(t *testing.T) {
	data := pattern(bs+7, 27)
	f, im := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/f", Data: data}, apfstest.File{Path: "/d", Dir: true})))
	ino := im.g.Volumes[0].Inodes
	id := apfs.NodeID(0, 0, ino["/f"])
	// Whatever the caller says about the entry, the disk decides.
	forged := filesys.Entry{
		ID: id, Name: "other", Size: 1 << 40, Type: filesys.TypeDir, Deleted: true, Encrypted: true,
		LinkTarget: "x", Attrs: []filesys.KV{{Key: "compressed", Value: "true"}, {Key: "private_id", Value: "99"}},
	}
	fl, err := f.Open(forged)
	if err != nil {
		t.Fatalf("Open(forged) = %v", err)
	}
	if fl.Size() != int64(len(data)) || !bytes.Equal(readAllAt(t, fl), data) {
		t.Error("a forged field changed what was read")
	}
	// A directory forged as a file is still a directory.
	if _, err := f.Open(filesys.Entry{ID: apfs.NodeID(0, 0, ino["/d"]), Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("directory forged as a file: %v", err)
	}
	// Forged IDs are rejected before any tree is scanned.
	scans := f.Scans()
	for _, bad := range []string{
		"", "n:0:0:0", "n:0:0:016", "n:0:0:+16", "n:0:0:0x10", "n:00:0:16", "n:0:0:16 ", "n:0:0:1152921504606846976",
		"n:100:0:16", "n:0:0", "node:0:0:16", "N:0:0:16", "n:0:1:16", "n:0:0:16:1",
	} {
		if _, err := f.Open(filesys.Entry{ID: bad}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	// An inode that does not exist.
	if _, err := f.Open(filesys.Entry{ID: apfs.NodeID(0, 0, 9999)}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open(missing inode) = %v, want ErrNotFound", err)
	}
	if got := f.Scans() - scans; got != 1 {
		t.Errorf("%d tree scans for the forged IDs, want only the one inode lookup", got)
	}
}

func TestExtentScanCountIsBounded(t *testing.T) {
	// Opening a file reads its inode and its extents: two scans, whatever the
	// number of extents.
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/f", Data: pattern(32*bs, 28), SplitContig: 32})))
	e := mustLookup(t, f, "/Data/f")
	before := f.Scans()
	if _, err := f.Open(e); err != nil {
		t.Fatal(err)
	}
	if got := f.Scans() - before; got != 2 {
		t.Errorf("Open made %d tree scans, want 2", got)
	}
}

// moveBeforeExtent returns a Reorder that moves the record chosen by from to just
// before the extent record number before (0-based) of object 40.
func moveBeforeExtent(t *testing.T, from func(recs []apfstest.FSRecord) int, before int) func([]apfstest.FSRecord) []apfstest.FSRecord {
	return func(recs []apfstest.FSRecord) []apfstest.FSRecord {
		src := from(recs)
		moved := recs[src]
		out := slices.Delete(slices.Clone(recs), src, src+1)
		n := 0
		for i, r := range out {
			if r.ID == 40 && r.Type == apfstest.TypeFileExtent {
				if n == before {
					return slices.Insert(out, i, moved)
				}
				n++
			}
		}
		t.Fatal("extent record not found")
		return recs
	}
}

func lastRecord(recs []apfstest.FSRecord) int { return len(recs) - 1 }

// A record of a LARGER object id between two extents of one file: the tree is
// out of order, so the second extent is not reachable by the seek. The file
// keeps the first extent as its trusted prefix; the missing tail is never a
// hole of zeros.
func TestLargerIDRecordBetweenExtentsEndsPrefix(t *testing.T) {
	data := pattern(4*bs, 12)
	v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true}, {Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
	}})
	v.Files = append(v.Files, apfstest.File{Path: "/later", Ino: 50, Data: pattern(10, 1)})
	v.Reorder = moveBeforeExtent(t, lastRecord, 1)
	f, _ := openOpts(t, volOpts(v))
	fl := openPath(t, f, "/Data/b")
	wantPrefix(t, f, fl, data, 2*bs)
	if !hasWarn(f, "out of key order") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// The larger-id record sits before the first extent: no extent is reached at
// all. That must be an empty trusted prefix, not an all-hole file.
func TestLargerIDRecordBeforeFirstExtentIsEmptyPrefix(t *testing.T) {
	data := pattern(4*bs, 12)
	v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true}, {Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
	}})
	v.Files = append(v.Files, apfstest.File{Path: "/later", Ino: 50, Data: pattern(10, 1)})
	v.Reorder = moveBeforeExtent(t, lastRecord, 0)
	f, _ := openOpts(t, volOpts(v))
	fl := openPath(t, f, "/Data/b")
	wantPrefix(t, f, fl, data, 0)
	if !hasWarn(f, "out of key order") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// A well-ordered tree with ordinary holes and gaps is unaffected.
func TestWellOrderedSparseFileStaysHoles(t *testing.T) {
	data := pattern(4*bs, 14)
	v := dataVolume(apfstest.File{Path: "/s", Ino: 40, Data: data, Gaps: [][2]int64{{bs, bs}}, Holes: [][2]int64{{2 * bs, bs}}})
	v.Files = append(v.Files, apfstest.File{Path: "/later", Ino: 50, Data: pattern(10, 1)})
	f, _ := openOpts(t, volOpts(v))
	fl := openPath(t, f, "/Data/s")
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Fatalf("CheckRuns: %v", err)
	}
	noWarnings(t, f)
}

// A duplicated record (not strictly greater than the previous key) is out of
// order too: here a second copy of extent 0 follows the first.
func TestDuplicateKeyEndsPrefix(t *testing.T) {
	data := pattern(4*bs, 12)
	v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true}, {Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
	}})
	v.Reorder = func(recs []apfstest.FSRecord) []apfstest.FSRecord {
		for i, r := range recs {
			if r.ID == 40 && r.Type == apfstest.TypeFileExtent {
				return slices.Insert(slices.Clone(recs), i, r) // two equal keys in a row
			}
		}
		t.Fatal("no extent record")
		return recs
	}
	f, _ := openOpts(t, volOpts(v))
	fl := openPath(t, f, "/Data/b")
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered >= fl.Size() {
		t.Fatalf("CheckRunsPrefix = %d, %v; want a strict prefix", covered, err)
	}
	if _, err := fl.ReadAt(make([]byte, 10), fl.Size()-10); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read of the unreached tail: %v, want ErrCorrupt", err)
	}
}

// A file whose extents (not its dstream) carry a key is reported encrypted by
// the opened file, so examine flags the artifact. The listing cannot know
// without an extent scan per file, so its entry stays unflagged.
func TestOpenedFileReportsPerFileEncryption(t *testing.T) {
	data := pattern(2*bs, 9)
	f, _ := openOpts(t, volOpts(dataVolume(
		apfstest.File{Path: "/plain", Data: data},
		apfstest.File{Path: "/sw", Data: data, CryptoID: 4, ExtentCrypto: 4},
		apfstest.File{Path: "/keyed", Data: data, CryptoID: 5, ExtentCrypto: 5},
		apfstest.File{Path: "/extentonly", Data: data, ExtentCrypto: 7},
		apfstest.File{Path: "/dstreamonly", Data: data, CryptoID: 8},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	if byName(t, es, "extentonly").Encrypted {
		t.Errorf("the listing flagged extentonly: it cannot know without an extent scan")
	}
	for n, want := range map[string]bool{"plain": false, "sw": false, "keyed": true, "extentonly": true, "dstreamonly": true} {
		if got := filesys.FileEncrypted(openPath(t, f, "/Data/"+n)); got != want {
			t.Errorf("%s: FileEncrypted = %v, want %v", n, got, want)
		}
	}
}

// Review scenario: a damaged/reordered record whose logical address lies past
// the end of the file sits before a real extent. The scan must not stop at it:
// the later extent is out of order, so the file keeps the prefix before it
// and the rest reads as an error, never as zeros.
func TestBeyondSizeRecordBeforeRealExtentIsNotAHoleTail(t *testing.T) {
	data := pattern(4*bs, 15)
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/b", Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true},
		{Logical: 1 << 40, Length: bs, Phys: 1 << 50}, // beyond the size
		{Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
	}})))
	fl := openPath(t, f, "/Data/b")
	wantPrefix(t, f, fl, data, 2*bs)
	if !hasWarn(f, "out of key order") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// Several legitimate extents past the end (preallocation) in order are
// ignored for the content, without a warning.
func TestOrderedBeyondSizeExtentsAreIgnored(t *testing.T) {
	data := pattern(100, 16)
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/p", Data: data, Extents: []apfstest.Extent{
		{Logical: 0, Length: bs, Rel: true},
		{Logical: 4 * bs, Length: bs, Phys: 1 << 50},
		{Logical: 8 * bs, Length: bs, Phys: 1 << 50},
	}})))
	fl := openPath(t, f, "/Data/p")
	if !bytes.Equal(readAllAt(t, fl), data) {
		t.Error("content differs")
	}
	noWarnings(t, f)
}

// Exhausting the node-read budget while the extents are read is the budget
// error, not a corrupt file that opens with a prefix.
func TestNodeBudgetDuringExtentScanFailsOpen(t *testing.T) {
	data := pattern(8*bs, 17)
	im := newImage(t, volOpts(dataVolume(apfstest.File{Path: "/m", Data: data, Fragments: 4})))
	var failed, full int
	for n := int64(0); n < 60; n++ {
		f := im.mustOpen()
		e := mustLookup(t, f, "/Data/m")
		f.SetNodeReads(n)
		fl, err := f.Open(e)
		if err != nil {
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Fatalf("budget %d: Open = %v", n, err)
			}
			failed++
			continue
		}
		if covered, cerr := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size); cerr != nil || covered != fl.Size() {
			t.Fatalf("budget %d: Open succeeded with a damaged file (covered %d, %v, warnings %q)", n, covered, cerr, f.Info().Warnings)
		}
		full++
	}
	if failed == 0 || full == 0 {
		t.Errorf("%d failed, %d full opens; the sweep must reach both", failed, full)
	}
}

// decmpfs builds the value of a com.apple.decmpfs xattr: "fpmc", the
// compression type and the uncompressed size, then (here) no payload.
func decmpfs(typ uint32) []byte {
	v := make([]byte, 16)
	copy(v, "fpmc")
	binary.LittleEndian.PutUint32(v[4:], typ)
	binary.LittleEndian.PutUint64(v[8:], 1234)
	return v
}

// The compressed attribute names the decmpfs type (a name for the known
// types, the decimal number otherwise); "unknown" when the header cannot be
// read. Such a file stays unsupported.
func TestCompressedAttrNamesTheDecmpfsType(t *testing.T) {
	badMagic := decmpfs(7)
	copy(badMagic, "xxxx")
	cases := []struct {
		name string
		xa   []apfstest.Xattr
		want string
	}{
		{"zlib attr", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(3)}}, "zlib-attr"},
		{"zlib rsrc", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(4)}}, "zlib-rsrc"},
		{"lzvn attr", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(7)}}, "lzvn-attr"},
		{"lzvn rsrc", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(8)}}, "lzvn-rsrc"},
		{"uncompressed attr", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(9)}}, "uncompressed-attr"},
		{"uncompressed rsrc", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(10)}}, "uncompressed-rsrc"},
		{"lzfse attr", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(11)}}, "lzfse-attr"},
		{"lzfse rsrc", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(12)}}, "lzfse-rsrc"},
		{"unknown number", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(99)}}, "99"},
		{"no xattr", nil, "unknown"},
		{"bad magic", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: badMagic}}, "unknown"},
		{"short header", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(7)[:6]}}, "unknown"},
		{"stream form", []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(7), Stream: true}}, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/c", Data: pattern(50, 1), CompressedFlag: true, Xattrs: c.xa})))
			e := mustLookup(t, f, "/Data/c")
			if v, ok := attr(e, "compressed"); !ok || v != c.want {
				t.Errorf("compressed = %q, %v; want %q", v, ok, c.want)
			}
			if _, err := f.Open(e); !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("Open = %v, want ErrUnsupported", err)
			}
		})
	}
}

// A record of the SAME object id but a later type ends the first leaf, and the
// file's remaining extent sits at the start of the second leaf: the extent
// follows a larger key, so the tree is out of order. The ordering check must go
// on across the leaf boundary while the object id is still the file's, not stop
// at the end of the leaf (the missing tail would otherwise read as a hole).
func TestOrderViolationInLaterLeafEndsPrefix(t *testing.T) {
	data := pattern(6*bs, 31)
	extents := []apfstest.Extent{
		{Logical: 0, Length: 2 * bs, Rel: true},
		{Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
		{Logical: 4 * bs, Length: 2 * bs, Rel: true, Phys: 4},
	}
	var lastOfLeaf int // records before the hostile one (it ends the first leaf)
	hostile := func(recs []apfstest.FSRecord) []apfstest.FSRecord {
		n := 0
		for i, r := range recs {
			if r.ID == 40 && r.Type == apfstest.TypeFileExtent {
				if n++; n == 3 { // before the third extent
					lastOfLeaf = i
					extra := apfstest.FSRecord{ID: 40, Type: apfstest.TypeDrec, Key: apfstest.DrecKey([]byte("x"), false, 0), Val: apfstest.DrecVal(40, 8)}
					return slices.Insert(slices.Clone(recs), i, extra)
				}
			}
		}
		t.Fatal("no third extent record")
		return recs
	}
	mk := func(maxKeys int) apfstest.Options {
		v := dataVolume(apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: extents})
		v.Reorder = hostile
		v.TreeMaxKeys = maxKeys
		return volOpts(v)
	}
	apfstest.Build(mk(0)) // finds the position of the hostile record
	f, _ := openOpts(t, mk(lastOfLeaf+1))
	fl := openPath(t, f, "/Data/b")
	wantPrefix(t, f, fl, data, 4*bs)
	if !hasWarn(f, "out of key order") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// Records of OTHER objects that sort wrongly before the file's range must not
// turn an intact file into an empty incomplete one: the scan only warns, and the
// file's own extents are read and checked as usual.
func TestUnrelatedOrderViolationBeforeRangeIsOnlyAWarning(t *testing.T) {
	data := pattern(4*bs, 33)
	v := dataVolume(
		apfstest.File{Path: "/p", Ino: 20},
		apfstest.File{Path: "/q", Ino: 21},
		apfstest.File{Path: "/b", Ino: 40, Data: data, Extents: []apfstest.Extent{
			{Logical: 0, Length: 2 * bs, Rel: true}, {Logical: 2 * bs, Length: 2 * bs, Rel: true, Phys: 2},
		}})
	v.Reorder = func(recs []apfstest.FSRecord) []apfstest.FSRecord {
		a, b := -1, -1
		for i, r := range recs {
			if r.Type == apfstest.TypeInode && r.ID == 20 {
				a = i
			}
			if r.Type == apfstest.TypeInode && r.ID == 21 {
				b = i
			}
		}
		if a < 0 || b != a+1 {
			t.Fatalf("inode records of 20 and 21 at %d, %d", a, b)
		}
		out := slices.Clone(recs)
		out[a], out[b] = out[b], out[a]
		return out
	}
	f, _ := openOpts(t, volOpts(v))
	fl := openPath(t, f, "/Data/b")
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Fatalf("an unrelated swap damaged the file: %v (runs %v)", err, fl.Runs())
	}
	if got := readAllAt(t, fl); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	if !hasWarn(f, "out of key order") {
		t.Errorf("no warning for the unordered records: %q", f.Info().Warnings)
	}
}
