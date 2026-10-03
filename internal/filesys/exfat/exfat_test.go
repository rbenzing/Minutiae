package exfat_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// Compile-time proof that FS is a filesys.FileSystem.
var _ filesys.FileSystem = (*exfat.FS)(nil)

func openImg(t testing.TB, img []byte) *exfat.FS {
	t.Helper()
	f, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func names(es []filesys.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func byName(t testing.TB, es []filesys.Entry, name string) filesys.Entry {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %q", name, names(es))
	return filesys.Entry{}
}

func attr(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

func hasAttr(e filesys.Entry, key, value string) bool {
	v, ok := attr(e, key)
	return ok && v == value
}

func readDir(t testing.TB, f *exfat.FS, path string) []filesys.Entry {
	t.Helper()
	d, err := f.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", path, err)
	}
	es, err := f.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	return es
}

func readAll(t testing.TB, f *exfat.FS, e filesys.Entry) []byte {
	t.Helper()
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", e.Name, err)
	}
	b := make([]byte, fl.Size())
	if n, err := fl.ReadAt(b, 0); n != len(b) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt(%q) = %d, %v", e.Name, n, err)
	}
	return b
}

func hasWarn(f *exfat.FS, sub string) bool {
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+i/251) ^ seed
	}
	return b
}

func le16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func le32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func le64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

// walkAll visits every entry below the root, deleted ones included.
func walkAll(t testing.TB, f *exfat.FS) map[string]filesys.Entry {
	t.Helper()
	out := map[string]filesys.Entry{}
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		out[p] = e
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

func TestExfatProbeAndBootChecksum(t *testing.T) {
	img := exfattest.Build(exfattest.Options{Label: "TESTVOL", Serial: 0xCAFEBABE}, nil)
	r := bytes.NewReader(img)
	if !exfat.Probe(r, int64(len(img))) {
		t.Fatal("Probe(exFAT image) = false")
	}
	if exfat.Probe(r, 511) || exfat.Probe(r, 0) || exfat.Probe(r, -1) {
		t.Error("Probe must be false for a size below one sector")
	}
	if exfat.Probe(bytes.NewReader(make([]byte, 4096)), 4096) {
		t.Error("Probe(zeros) = true")
	}
	fat := bytes.Clone(img)
	copy(fat[3:], "MSDOS5.0")
	if exfat.Probe(bytes.NewReader(fat), int64(len(fat))) {
		t.Error("Probe(FAT OEM) = true")
	}
	for _, sh := range []struct{ bps, spc byte }{{8, 3}, {13, 0}, {9, 17}, {12, 14}, {9, 255}} {
		bad := bytes.Clone(img)
		bad[108], bad[109] = sh.bps, sh.spc
		if exfat.Probe(bytes.NewReader(bad), int64(len(bad))) {
			t.Errorf("Probe with shifts %d/%d = true", sh.bps, sh.spc)
		}
		if _, err := exfat.Open(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("Open with shifts %d/%d = %v, want a corrupt error", sh.bps, sh.spc, err)
		}
	}
	for _, sh := range []struct{ bps, spc byte }{{9, 0}, {9, 16}, {12, 13}, {10, 4}} {
		ok := bytes.Clone(img)
		ok[108], ok[109] = sh.bps, sh.spc
		if !exfat.Probe(bytes.NewReader(ok), int64(len(ok))) {
			t.Errorf("Probe with shifts %d/%d = false", sh.bps, sh.spc)
		}
	}

	f := openImg(t, img)
	info := f.Info()
	if info.Type != "exfat" || info.Label != "TESTVOL" || info.UUID != "CAFE-BABE" || info.BlockSize != 4096 || info.Size != int64(len(img)) {
		t.Errorf("Info = %+v", info)
	}
	if len(info.Warnings) != 0 || info.Encrypted {
		t.Errorf("Info = %+v, want no warnings", info)
	}

	// The checksum covers sectors 0-10 except VolumeFlags and PercentInUse.
	vf := bytes.Clone(img)
	vf[106] ^= 2 // VolumeDirty
	vf[112] = 77
	if f := openImg(t, vf); len(f.Info().Warnings) != 0 {
		t.Errorf("VolumeFlags/PercentInUse changes must not affect the checksum: %v", f.Info().Warnings)
	}
	chk := exfattest.BootChecksum(img[:11*512])
	if got := exfat.BootChecksum(img[:11*512]); got != chk {
		t.Errorf("bootChecksum = %#x, builder says %#x", got, chk)
	}

	bad := exfattest.Build(exfattest.Options{Label: "TESTVOL", BadBootChecksum: true}, nil)
	fb := openImg(t, bad)
	if !hasWarn(fb, "boot region checksum") {
		t.Errorf("bad boot checksum not reported: %v", fb.Info().Warnings)
	}
	patched := bytes.Clone(img)
	le32(patched, 100, 0x11112222) // the serial number
	fp := openImg(t, patched)
	if !hasWarn(fp, "boot region checksum") {
		t.Errorf("boot sector change without a new checksum not reported: %v", fp.Info().Warnings)
	}
	if fp.Info().UUID != "1111-2222" {
		t.Errorf("UUID = %q", fp.Info().UUID)
	}
	exfattest.FixBootChecksum(patched)
	if f := openImg(t, patched); len(f.Info().Warnings) != 0 {
		t.Errorf("fixed checksum still warns: %v", f.Info().Warnings)
	}
	// One wrong word in the checksum sector is enough.
	word := bytes.Clone(img)
	le32(word, 11*512+64, 0)
	if !hasWarn(openImg(t, word), "boot region checksum") {
		t.Error("a wrong word in the checksum sector must warn")
	}

	// Other bit patterns.
	if exfat.Probe(bytes.NewReader(img[:300]), 300) {
		t.Error("Probe on a 300-byte image")
	}
	if _, err := exfat.Open(bytes.NewReader(img[:300]), 300); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open(300 bytes) = %v", err)
	}
	if _, err := exfat.Open(bytes.NewReader(img), 100); err == nil {
		t.Error("Open with size 100 must fail")
	}
}

func TestExfatChecksumHelpers(t *testing.T) {
	data := pattern(1000, 3)
	var want uint32
	for _, c := range data {
		want = (want&1)<<31 | want>>1
		want += uint32(c)
	}
	if got := exfat.TableChecksum(data); got != want {
		t.Errorf("TableChecksum = %#x, want %#x", got, want)
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/a-name-of-some-length.txt", Data: []byte("x")}})
	loc := lay.Find("/a-name-of-some-length.txt", false)
	n := (int(img[loc.Offset+1]) + 1) * 32
	set := slices.Clone(img[loc.Offset : int(loc.Offset)+n])
	if got, stored := exfat.SetChecksum(set), binary.LittleEndian.Uint16(set[2:]); got != stored {
		t.Errorf("SetChecksum = %#x, builder stored %#x", got, stored)
	}
}

func TestExfatEntrySetsAndNames(t *testing.T) {
	cjk := strings.Repeat("漢字", 127) + "x" // 255 units
	long31 := "0123456789abcdef0123456789abcde"
	files := []exfattest.File{
		{Path: "/short.txt", Data: []byte("hello")},
		{Path: "/exactly15chars!", Data: []byte("15")},
		{Path: "/0123456789abcdef", Data: []byte("16")},
		{Path: "/" + long31, Data: []byte("31")},
		{Path: "/" + cjk, Data: []byte("cjk")},
		{Path: "/ünï-€.txt", Data: []byte("unicode")},
		{Path: "/emoji-😀.txt", Data: []byte("astral")},
		{Path: "/Dir", Dir: true},
		{Path: "/Dir/Nested", Dir: true},
		{Path: "/Dir/Nested/deep.txt", Data: []byte("deep content")},
		{Path: "/Dir/Readme.TXT", Data: []byte("readme")},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{Label: "TESTVOL"}, files)
	f := openImg(t, img)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	root := f.Root()
	if root.ID != "dir:"+strconv.Itoa(int(lay.RootCluster)) || root.Type != filesys.TypeDir || root.Name != "" {
		t.Errorf("Root = %+v", root)
	}
	es, err := f.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Dir", "short.txt", "exactly15chars!", "0123456789abcdef", long31, cjk, "ünï-€.txt", "emoji-😀.txt"}
	want = slices.Sorted(slices.Values(want))
	if got := names(es); !slices.Equal(got, want) {
		t.Fatalf("root names = %q\nwant %q", got, want)
	}
	// Entry order is the on-disk order, and the first set follows the bitmap,
	// up-case and label entries.
	if es[0].Name != "short.txt" || es[0].ID != fmt.Sprintf("dirent:%d:3", lay.RootCluster) {
		t.Errorf("first entry = %q %q", es[0].Name, es[0].ID)
	}
	for _, e := range es {
		if e.Deleted {
			t.Errorf("%q is Deleted", e.Name)
		}
		if e.Name == "Dir" {
			continue
		}
		loc := lay.Find("/"+e.Name, false)
		if e.ID != fmt.Sprintf("dirent:%d:%d", lay.RootCluster, loc.Index) {
			t.Errorf("%q ID = %q, want dirent:%d:%d", e.Name, e.ID, lay.RootCluster, loc.Index)
		}
		if e.Type != filesys.TypeFile {
			t.Errorf("%q type %v", e.Name, e.Type)
		}
		if v, _ := attr(e, "first_cluster"); v != strconv.Itoa(int(loc.FirstCluster)) {
			t.Errorf("%q first_cluster = %q, want %d", e.Name, v, loc.FirstCluster)
		}
		if v, _ := attr(e, "size"); v != strconv.FormatInt(e.Size, 10) {
			t.Errorf("%q size attr = %q, Size %d", e.Name, v, e.Size)
		}
	}
	// A name of 16 units takes two Name entries, 31 takes three, 255 takes 17.
	for name, wantSec := range map[string]byte{"short.txt": 2, "exactly15chars!": 2, "0123456789abcdef": 3, long31: 4, cjk: 18} {
		if got := img[lay.Find("/"+name, false).Offset+1]; got != wantSec {
			t.Errorf("SecondaryCount of %q = %d, want %d", name, got, wantSec)
		}
	}

	dir := byName(t, es, "Dir")
	dloc := lay.Find("/Dir", false)
	if dir.Type != filesys.TypeDir || dir.ID != fmt.Sprintf("dir:%d", dloc.FirstCluster) || !hasAttr(dir, "dirent", fmt.Sprintf("%d:%d", lay.RootCluster, dloc.Index)) {
		t.Errorf("Dir entry = %+v", dir)
	}
	if dir.Size != int64(lay.ClusterSize) {
		t.Errorf("Dir size %d", dir.Size)
	}
	sub := readDir(t, f, "/Dir")
	if got := names(sub); !slices.Equal(got, []string{"Nested", "Readme.TXT"}) {
		t.Errorf("/Dir = %q", got)
	}
	if e := byName(t, sub, "Readme.TXT"); e.ID != fmt.Sprintf("dirent:%d:%d", dloc.FirstCluster, lay.Find("/Dir/Readme.TXT", false).Index) {
		t.Errorf("nested ID = %q", e.ID)
	}

	// Content and Lookup, exact and case-insensitive through the up-case table.
	for path, content := range map[string]string{
		"/Dir/Nested/deep.txt": "deep content", "/DIR/nested/DEEP.TXT": "deep content",
		"/dir/readme.txt": "readme", "/" + cjk: "cjk", "/ÜNÏ-€.TXT": "unicode", "/emoji-😀.txt": "astral", "//Dir//Readme.TXT": "readme",
	} {
		e, err := f.Lookup(path)
		if err != nil {
			t.Errorf("Lookup(%q): %v", path, err)
			continue
		}
		if got := readAll(t, f, e); string(got) != content {
			t.Errorf("Lookup(%q) content %q, want %q", path, got, content)
		}
	}
	for _, p := range []string{"/nope", "/Dir/nope", "/short.txt/x", "/Dir/Nested/deep.txt/more"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if e, err := f.Lookup("/"); err != nil || e.ID != root.ID {
		t.Errorf("Lookup(/) = %+v, %v", e, err)
	}

	// Walk sees the whole tree.
	all := walkAll(t, f)
	if _, ok := all["/Dir/Nested/deep.txt"]; !ok || len(all) != len(files) {
		t.Errorf("walk found %d entries, want %d", len(all), len(files))
	}
}

func TestExfatLookupCaseFolding(t *testing.T) {
	files := []exfattest.File{
		{Path: "/a", Data: []byte("lower")},
		{Path: "/A", Data: []byte("upper")},
		{Path: "/Mixed.Name", Data: []byte("m")},
		{Path: "/zebra", Data: []byte("z")},
	}
	img := exfattest.Build(exfattest.Options{}, files)
	f := openImg(t, img)
	// An exact match wins over a case-insensitive one.
	for path, want := range map[string]string{"/a": "lower", "/A": "upper"} {
		e, err := f.Lookup(path)
		if err != nil || string(readAll(t, f, e)) != want {
			t.Errorf("Lookup(%q) = %v, %v", path, e.Name, err)
		}
	}
	if _, err := f.Lookup("/MIXED.NAME"); err != nil {
		t.Errorf("table fold: %v", err)
	}

	// The up-case table is authoritative: this one does not fold 'z'.
	noZ := exfattest.Build(exfattest.Options{Upcase: func(u uint16) uint16 {
		if u >= 'a' && u <= 'y' {
			return u - 0x20
		}
		return u
	}}, files)
	g := openImg(t, noZ)
	if !g.UpcaseLoaded() {
		t.Fatal("up-case table not loaded")
	}
	if _, err := g.Lookup("/ZEBRA"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/ZEBRA) with a table that keeps z = %v, want ErrNotFound", err)
	}
	if _, err := g.Lookup("/zebra"); err != nil {
		t.Errorf("exact: %v", err)
	}
	if _, err := g.Lookup("/MIXED.NAME"); err != nil {
		t.Errorf("fold with custom table: %v", err)
	}

	// Without a table strings.EqualFold is the fallback (valid UTF-8 only).
	nt := exfattest.Build(exfattest.Options{NoUpcase: true}, files)
	h := openImg(t, nt)
	if h.UpcaseLoaded() {
		t.Error("table loaded from an image without one")
	}
	if len(h.Info().Warnings) != 0 {
		t.Errorf("missing table warned: %v", h.Info().Warnings)
	}
	if _, err := h.Lookup("/ZEBRA"); err != nil {
		t.Errorf("EqualFold fallback: %v", err)
	}
	if got := h.Upper('q'); got != 'q' {
		t.Errorf("Upper without a table = %c", got)
	}
	if got := f.Upper('q'); got != 'Q' {
		t.Errorf("Upper with the default table = %c", got)
	}
	if got := f.Upper(0xFF); got != 0x178 {
		t.Errorf("Upper(U+00FF) = %#x", got)
	}
	if got := f.Upper(0x4E00); got != 0x4E00 { // inside the compressed identity run
		t.Errorf("Upper(U+4E00) = %#x", got)
	}
}

func TestExfatRawNames(t *testing.T) {
	lone := []uint16{'a', 0xD800, 'b'}   // unpaired high surrogate
	loneLow := []uint16{0xDC00, 'c'}     // unpaired low surrogate
	withSlash := []uint16{'x', '/', 'y'} // not a usable path component
	withNul := []uint16{'n', 0, 'z'}     // NUL
	files := []exfattest.File{
		{Path: "/p1", RawName: lone, Data: []byte("1")},
		{Path: "/p2", RawName: loneLow, Data: []byte("2")},
		{Path: "/p3", RawName: withSlash, Data: []byte("3")},
		{Path: "/p4", RawName: withNul, Data: []byte("4")},
	}
	img := exfattest.Build(exfattest.Options{}, files)
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 4 {
		t.Fatalf("ReadDir = %d entries, %v", len(es), err)
	}
	for i, units := range [][]uint16{lone, loneLow, withSlash, withNul} {
		var raw []byte
		for _, u := range units {
			raw = binary.LittleEndian.AppendUint16(raw, u)
		}
		e := es[i]
		wantName := "~raw~" + base64.RawURLEncoding.EncodeToString(raw)
		if e.Name != wantName || !bytes.Equal(e.RawName, raw) {
			t.Errorf("entry %d: Name %q RawName % x, want %q / % x", i, e.Name, e.RawName, wantName, raw)
		}
		// The display form finds it again, and so does nothing else.
		got, err := f.Lookup("/" + wantName)
		if err != nil || got.ID != e.ID {
			t.Errorf("Lookup(%q) = %+v, %v", wantName, got, err)
		}
	}
	// A valid name has no RawName.
	good := exfattest.Build(exfattest.Options{}, []exfattest.File{{Path: "/ok", Data: []byte("1")}})
	ge, _ := openImg(t, good).ReadDir(openImg(t, good).Root())
	if len(ge) != 1 || ge[0].RawName != nil || ge[0].Name != "ok" {
		t.Errorf("valid name entry = %+v", ge)
	}
	// A real name that looks like a display form wins over the form it spells.
	literal := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte{'a', 0, 0x00, 0xD8, 'b', 0})
	two := exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/" + literal, Data: []byte("literal")},
		{Path: "/q", RawName: lone, Data: []byte("raw")},
	})
	g := openImg(t, two)
	e, err := g.Lookup("/" + literal)
	if err != nil || string(readAll(t, g, e)) != "literal" {
		t.Errorf("literal name lookup = %v, %v", e.Name, err)
	}
}

func TestExfatSetChecksumBadFlagged(t *testing.T) {
	files := []exfattest.File{
		{Path: "/good.txt", Data: []byte("good")},
		{Path: "/badsum.txt", Data: []byte("bad one"), BadChecksum: true},
		{Path: "/flip-in-name-entry.txt", Data: []byte("flipped")},
		{Path: "/flip-in-stream.txt", Data: []byte("stream")},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	img[lay.Find("/flip-in-name-entry.txt", false).Offset+64+2]++ // first name unit, checksum not updated
	img[lay.Find("/flip-in-stream.txt", false).Offset+32+4] ^= 1  // NameHash, checksum not updated
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 4 {
		t.Fatalf("ReadDir = %d, %v", len(es), err)
	}
	for _, e := range es {
		// The flipped name unit turns the first 'f' into 'g'.
		_, bad := attr(e, "checksum")
		if wantBad := e.Name != "good.txt"; bad != wantBad {
			t.Errorf("%q: checksum attr present=%v, want %v", e.Name, bad, wantBad)
		}
	}
	if byName(t, es, "glip-in-name-entry.txt").Size != 7 {
		t.Error("flipped-name entry missing")
	}
	good := byName(t, es, "good.txt")
	if v, ok := attr(good, "checksum"); ok {
		t.Errorf("good set has checksum attr %q", v)
	}
	bad := byName(t, es, "badsum.txt")
	if !hasAttr(bad, "checksum", "bad") {
		t.Errorf("badsum.txt attrs = %v", bad.Attrs)
	}
	// A listed set with a bad checksum still opens: it is flagged, not hidden.
	if got := readAll(t, f, bad); string(got) != "bad one" {
		t.Errorf("content = %q", got)
	}
	n := 0
	for _, e := range es {
		if hasAttr(e, "checksum", "bad") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d sets flagged checksum=bad, want 3 (%v)", n, names(es))
	}
	// Fixing the checksum clears the flag.
	exfattest.FixSetChecksum(img, lay.Find("/flip-in-stream.txt", false).Offset)
	es2, _ := openImg(t, img).ReadDir(openImg(t, img).Root())
	if e := byName(t, es2, "flip-in-stream.txt"); hasAttr(e, "checksum", "bad") {
		t.Error("fixed set still flagged")
	}
}

func TestExfatDeletedSetFlagged(t *testing.T) {
	files := []exfattest.File{
		{Path: "/live.txt", Data: []byte("live")},
		{Path: "/gone.txt", Data: []byte("deleted content"), Deleted: true},
		{Path: "/a-deleted-file-with-a-long-name.dat", Data: pattern(5000, 1), Deleted: true},
		{Path: "/stale-sum.txt", Data: []byte("stale"), Deleted: true, DeletedStaleChecksum: true},
		{Path: "/bad-sum.txt", Data: []byte("bad"), Deleted: true, BadChecksum: true},
		{Path: "/deldir", Dir: true, Deleted: true},
		{Path: "/deldir/child.txt", Data: []byte("child")},
		{Path: "/live.txt", Data: []byte("replacement"), Deleted: true}, // same name as a live entry
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 8-1 { // the child lives in the deleted directory
		t.Fatalf("%d entries: %q", len(es), names(es))
	}
	seen := map[string]bool{}
	for _, e := range es {
		if seen[e.ID] {
			t.Errorf("duplicate ID %q", e.ID)
		}
		seen[e.ID] = true
	}
	var del, live int
	for _, e := range es {
		if e.Deleted {
			del++
		} else {
			live++
		}
	}
	if del != 6 || live != 1 {
		t.Errorf("%d deleted, %d live", del, live)
	}

	var gone filesys.Entry
	for _, e := range es {
		if e.Name == "gone.txt" {
			gone = e
		}
	}
	loc := lay.Find("/gone.txt", true)
	if !gone.Deleted || gone.ID != fmt.Sprintf("dirent:%d:%d", lay.RootCluster, loc.Index) || gone.Type != filesys.TypeFile || gone.Size != 15 {
		t.Fatalf("gone.txt = %+v", gone)
	}
	if !hasAttr(gone, "first_cluster", strconv.Itoa(int(loc.FirstCluster))) || !hasAttr(gone, "size", "15") {
		t.Errorf("gone.txt attrs = %v", gone.Attrs)
	}
	if !hasAttr(gone, "dirent", fmt.Sprintf("%d:%d", lay.RootCluster, loc.Index)) {
		t.Errorf("gone.txt attrs = %v", gone.Attrs)
	}
	if _, ok := attr(gone, "checksum"); ok {
		t.Errorf("a deleted set with a matching checksum is flagged: %v", gone.Attrs)
	}
	if _, err := f.Open(gone); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(deleted) = %v, want ErrDeleted", err)
	}
	// The deleted multi-entry name is reassembled.
	long := byName(t, es, "a-deleted-file-with-a-long-name.dat")
	if !long.Deleted || long.Size != 5000 {
		t.Errorf("long deleted = %+v", long)
	}
	// The checksum of the live set (stale after the types were cleared) verifies too.
	st := byName(t, es, "stale-sum.txt")
	if _, ok := attr(st, "checksum"); ok || !st.Deleted {
		t.Errorf("stale-sum.txt = %+v", st)
	}
	bs := byName(t, es, "bad-sum.txt")
	if !hasAttr(bs, "checksum", "bad") || !bs.Deleted {
		t.Errorf("bad-sum.txt = %+v", bs)
	}
	dd := byName(t, es, "deldir")
	if !dd.Deleted || dd.Type != filesys.TypeDir || !strings.HasPrefix(dd.ID, "dirent:") {
		t.Errorf("deldir = %+v", dd)
	}
	if _, err := f.ReadDir(dd); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("ReadDir(deleted dir) = %v, want ErrDeleted", err)
	}
	if _, err := f.Open(dd); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(deleted dir) = %v, want ErrDeleted", err)
	}

	// Lookup sees live entries only, and the live twin of a deleted name.
	for _, p := range []string{"/gone.txt", "/deldir", "/deldir/child.txt", "/bad-sum.txt"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	e, err := f.Lookup("/live.txt")
	if err != nil || e.Deleted || string(readAll(t, f, e)) != "live" {
		t.Errorf("Lookup(/live.txt) = %+v, %v", e, err)
	}
	// Deleted entries are passed to Walk but never recursed into.
	visited := 0
	_ = filesys.Walk(f, f.Root(), "/", func(_ string, _ filesys.Entry, _ error) error {
		visited++
		return nil
	})
	if visited != 7 {
		t.Errorf("walk visited %d entries, want 7 (the deleted directory is not entered)", visited)
	}
}

func TestExfatNoFatChainAndFatChainFiles(t *testing.T) {
	cs := 4096
	dA := pattern(3*cs+100, 1)
	dB := pattern(2*cs+7, 2)
	dC := pattern(4*cs-5, 3)
	dD := pattern(cs, 4)
	files := []exfattest.File{
		{Path: "/nofat.bin", Data: dA},
		{Path: "/chain.bin", Data: dB, FatChain: true},
		{Path: "/frag.bin", Data: dC, Fragmented: true},
		{Path: "/exact.bin", Data: dD},
		{Path: "/empty.txt"},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	fsSize := f.Info().Size
	check := func(name string, data []byte, wantRuns []filesys.Run, noFat bool) {
		t.Helper()
		e := byName(t, es, name)
		if e.Size != int64(len(data)) {
			t.Errorf("%s: Size %d", name, e.Size)
		}
		if got := hasAttr(e, "no_fat_chain", "true"); got != noFat {
			t.Errorf("%s: no_fat_chain = %v, want %v", name, got, noFat)
		}
		fl, err := f.Open(e)
		if err != nil {
			t.Fatalf("%s: Open: %v", name, err)
		}
		if fl.Size() != int64(len(data)) {
			t.Errorf("%s: file Size %d", name, fl.Size())
		}
		runs := fl.Runs()
		if !slices.Equal(runs, wantRuns) {
			t.Errorf("%s: Runs = %v, want %v", name, runs, wantRuns)
		}
		if err := filesys.CheckRuns(runs, fl.Size(), fsSize); err != nil {
			t.Errorf("%s: CheckRuns: %v", name, err)
		}
		if got := readAll(t, f, e); !bytes.Equal(got, data) {
			t.Errorf("%s: content differs", name)
		}
		// Reads at every kind of offset.
		for _, off := range []int64{0, 1, int64(cs) - 1, int64(cs), int64(cs) + 1, int64(len(data)) - 3} {
			if off < 0 || off >= int64(len(data)) {
				continue
			}
			buf := make([]byte, 5000)
			n, err := fl.ReadAt(buf, off)
			wantN := min(int64(len(buf)), int64(len(data))-off)
			if int64(n) != wantN || !bytes.Equal(buf[:n], data[off:off+wantN]) {
				t.Errorf("%s: ReadAt(off %d) = %d bytes", name, off, n)
			}
			if wantN < int64(len(buf)) && !errors.Is(err, io.EOF) {
				t.Errorf("%s: short ReadAt err = %v, want EOF", name, err)
			}
		}
		if n, err := fl.ReadAt(make([]byte, 4), int64(len(data))); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt at EOF = %d, %v", name, n, err)
		}
		if _, err := fl.ReadAt(make([]byte, 1), -1); err == nil {
			t.Errorf("%s: negative offset accepted", name)
		}
	}
	off := lay.ClusterOffset
	cl := func(name string) []uint32 { return lay.Find("/"+name, false).Clusters }
	check("nofat.bin", dA, []filesys.Run{{Offset: off(cl("nofat.bin")[0]), Length: int64(len(dA))}}, true)
	check("chain.bin", dB, []filesys.Run{{Offset: off(cl("chain.bin")[0]), Length: int64(len(dB))}}, false)
	check("exact.bin", dD, []filesys.Run{{Offset: off(cl("exact.bin")[0]), Length: int64(cs)}}, true)
	var fragRuns []filesys.Run
	for i, c := range cl("frag.bin") {
		n := int64(min(cs, len(dC)-i*cs))
		fragRuns = append(fragRuns, filesys.Run{Offset: off(c), Length: n})
	}
	if len(fragRuns) != 4 || fragRuns[1].Offset == fragRuns[0].Offset+int64(cs) {
		t.Fatalf("builder did not fragment: %v", fragRuns)
	}
	check("frag.bin", dC, fragRuns, false)

	e := byName(t, es, "empty.txt")
	fl, err := f.Open(e)
	if err != nil || fl.Size() != 0 || fl.Runs() == nil || len(fl.Runs()) != 0 {
		t.Errorf("empty file: %+v %v", fl, err)
	}
	if n, err := fl.ReadAt(make([]byte, 3), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("empty ReadAt = %d, %v", n, err)
	}
	// Runs returns a copy.
	fl2, _ := f.Open(byName(t, es, "nofat.bin"))
	r := fl2.Runs()
	r[0].Length = 1
	if fl2.Runs()[0].Length != int64(len(dA)) {
		t.Error("Runs shares its backing array")
	}
}

func TestExfatDirectoriesSpanClusters(t *testing.T) {
	var files []exfattest.File
	for _, d := range []struct {
		name  string
		chain bool
	}{{"nofat", false}, {"fatchain", true}} {
		files = append(files, exfattest.File{Path: "/" + d.name, Dir: true, FatChain: d.chain})
		for i := 0; i < 200; i++ {
			files = append(files, exfattest.File{Path: fmt.Sprintf("/%s/file-%03d.txt", d.name, i), Data: []byte(strconv.Itoa(i))})
		}
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{ClusterCount: 1024}, files)
	f := openImg(t, img)
	root, _ := f.ReadDir(f.Root())
	nofat, fatchain := byName(t, root, "nofat"), byName(t, root, "fatchain")
	if len(lay.Find("/nofat", false).Clusters) < 4 {
		t.Fatalf("directory too small to span clusters: %d", len(lay.Find("/nofat", false).Clusters))
	}
	if !hasAttr(nofat, "no_fat_chain", "true") || hasAttr(fatchain, "no_fat_chain", "true") {
		t.Errorf("attrs: %v / %v", nofat.Attrs, fatchain.Attrs)
	}
	for _, d := range []filesys.Entry{nofat, fatchain} {
		es, err := f.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 200 {
			t.Errorf("%s: %d entries, want 200", d.Name, len(es))
		}
		for _, e := range es {
			i, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e.Name, "file-"), ".txt"))
			if got := readAll(t, f, e); string(got) != strconv.Itoa(i) {
				t.Errorf("%s/%s content %q", d.Name, e.Name, got)
			}
		}
		if _, err := f.Lookup("/" + d.Name + "/file-199.txt"); err != nil {
			t.Errorf("Lookup in %s: %v", d.Name, err)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestExfatValidDataLengthZeros(t *testing.T) {
	cs := int64(4096)
	data := bytes.Repeat([]byte{0xAA}, 3*4096+500)
	vdl := func(n int64) *int64 { return &n }
	files := []exfattest.File{
		{Path: "/partial", Data: data, ValidLength: vdl(5000)},
		{Path: "/none", Data: data, ValidLength: vdl(0)},
		{Path: "/full", Data: data},
		{Path: "/aligned", Data: data, ValidLength: vdl(cs)},
		{Path: "/chained", Data: data, ValidLength: vdl(100), Fragmented: true},
		{Path: "/lastbyte", Data: data, ValidLength: vdl(int64(len(data)) - 1)},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	es, _ := f.ReadDir(f.Root())
	for _, tc := range []struct {
		name  string
		valid int64
	}{{"partial", 5000}, {"none", 0}, {"full", int64(len(data))}, {"aligned", cs}, {"chained", 100}, {"lastbyte", int64(len(data)) - 1}} {
		e := byName(t, es, tc.name)
		fl, err := f.Open(e)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if fl.Size() != int64(len(data)) {
			t.Errorf("%s: Size = %d", tc.name, fl.Size())
		}
		got := readAll(t, f, e)
		want := slices.Concat(data[:tc.valid], make([]byte, int64(len(data))-tc.valid))
		if !bytes.Equal(got, want) {
			t.Errorf("%s: bytes at/after ValidDataLength must be zero", tc.name)
		}
		runs := fl.Runs()
		if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		var onDisk, holes int64
		for i, r := range runs {
			if r.Offset < 0 {
				if i != len(runs)-1 || r.Offset != -1 {
					t.Errorf("%s: hole not last/-1: %v", tc.name, runs)
				}
				holes += r.Length
			} else {
				onDisk += r.Length
			}
		}
		if onDisk != tc.valid || holes != int64(len(data))-tc.valid {
			t.Errorf("%s: runs %v cover %d valid + %d hole, want %d + %d", tc.name, runs, onDisk, holes, tc.valid, int64(len(data))-tc.valid)
		}
		if tc.valid < int64(len(data)) {
			if !hasAttr(e, "valid_data_length", strconv.FormatInt(tc.valid, 10)) {
				t.Errorf("%s: attrs %v", tc.name, e.Attrs)
			}
			// A read that straddles the boundary.
			off := max(tc.valid-32, 0)
			buf := make([]byte, min(64, int64(len(data))-off))
			if _, err := fl.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !bytes.Equal(buf, want[off:off+int64(len(buf))]) {
				t.Errorf("%s: straddling read wrong", tc.name)
			}
		} else if _, ok := attr(e, "valid_data_length"); ok {
			t.Errorf("%s: valid_data_length attr on a fully valid file", tc.name)
		}
	}
	_ = lay
}

func TestExfatUtcOffsetTimes(t *testing.T) {
	wall := func(h, m, s, ns int) time.Time { return time.Date(2023, 11, 14, h, m, s, ns, time.UTC) }
	files := []exfattest.File{
		{
			Path:    "/tz",
			Data:    []byte("x"),
			Times:   [3]time.Time{wall(10, 20, 30, 0), wall(22, 13, 31, 0), wall(8, 0, 0, 0)},
			Offsets: [3]exfattest.Offset{{Valid: true, Quarters: 4}, {Valid: true, Quarters: -20}, {}},
			Inc10ms: [2]uint8{0, 2},
		},
		{
			Path:    "/utc",
			Data:    []byte("y"),
			Times:   [3]time.Time{wall(0, 0, 0, 0), wall(23, 59, 58, 0), wall(12, 0, 2, 0)},
			Offsets: [3]exfattest.Offset{{Valid: true}, {Valid: true, Quarters: 63}, {Valid: true, Quarters: -64}},
		},
		{Path: "/nozone", Data: []byte("z"), Times: [3]time.Time{wall(1, 2, 4, 0), wall(1, 2, 5, 0), wall(1, 2, 6, 0)}},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	es, _ := f.ReadDir(f.Root())

	e := byName(t, es, "tz")
	if !e.Times.Created.ZoneKnown || !e.Times.Created.T.Equal(wall(9, 20, 30, 0)) {
		t.Errorf("tz created = %+v", e.Times.Created)
	}
	if !e.Times.Modified.ZoneKnown || !e.Times.Modified.T.Equal(time.Date(2023, 11, 15, 3, 13, 31, 20_000_000, time.UTC)) {
		t.Errorf("tz modified = %v (zone known %v)", e.Times.Modified.T.UTC(), e.Times.Modified.ZoneKnown)
	}
	if _, off := e.Times.Modified.T.Zone(); off != -5*3600 {
		t.Errorf("modified zone offset = %d", off)
	}
	if e.Times.Accessed.ZoneKnown || !e.Times.Accessed.T.Equal(wall(8, 0, 0, 0)) {
		t.Errorf("tz accessed (no valid offset) = %+v", e.Times.Accessed)
	}
	if !e.Times.Changed.T.IsZero() || !e.Times.Deleted.T.IsZero() {
		t.Errorf("Changed/Deleted should be absent: %+v", e.Times)
	}

	u := byName(t, es, "utc")
	if !u.Times.Created.ZoneKnown || !u.Times.Created.T.Equal(wall(0, 0, 0, 0)) {
		t.Errorf("utc created = %+v", u.Times.Created)
	}
	if !u.Times.Modified.ZoneKnown || !u.Times.Modified.T.Equal(wall(23, 59, 58, 0).Add(-63*15*time.Minute)) {
		t.Errorf("utc modified = %v", u.Times.Modified.T.UTC())
	}
	if !u.Times.Accessed.ZoneKnown || !u.Times.Accessed.T.Equal(wall(12, 0, 2, 0).Add(64*15*time.Minute)) {
		t.Errorf("utc accessed = %v", u.Times.Accessed.T.UTC())
	}

	n := byName(t, es, "nozone")
	for i, ts := range []filesys.Timestamp{n.Times.Created, n.Times.Modified, n.Times.Accessed} {
		if ts.ZoneKnown || !ts.T.Equal(wall(1, 2, 4+i, 0)) {
			t.Errorf("nozone[%d] = %+v", i, ts)
		}
	}

	// Impossible fields make the timestamp absent (and are noted), they do
	// not shift into the next month.
	loc := lay.Find("/nozone", false)
	bad := bytes.Clone(img)
	mod := binary.LittleEndian.Uint32(bad[loc.Offset+12:])
	le32(bad, int(loc.Offset)+12, mod&^(0xF<<21)|13<<21) // month 13
	acc := binary.LittleEndian.Uint32(bad[loc.Offset+16:])
	le32(bad, int(loc.Offset)+16, acc&^(0x1F<<16)|31<<16&^(0xF<<21)|2<<21) // 31 February
	le32(bad, int(loc.Offset)+8, 0)                                        // an all-zero timestamp
	exfattest.FixSetChecksum(bad, loc.Offset)
	g := openImg(t, bad)
	es2, _ := g.ReadDir(g.Root())
	nz := byName(t, es2, "nozone")
	if !nz.Times.Modified.T.IsZero() || !nz.Times.Accessed.T.IsZero() || !nz.Times.Created.T.IsZero() {
		t.Errorf("invalid timestamps must be absent: %+v", nz.Times)
	}
	if v, ok := attr(nz, "bad_timestamps"); !ok || !strings.Contains(v, "modified") || !strings.Contains(v, "accessed") {
		t.Errorf("bad_timestamps = %q", v)
	}
	if _, ok := attr(nz, "checksum"); ok {
		t.Errorf("checksum flagged: %v", nz.Attrs)
	}
	// An out-of-range 10 ms increment is ignored, not added.
	inc := bytes.Clone(img)
	incLoc := lay.Find("/nozone", false)
	inc[incLoc.Offset+21] = 255
	exfattest.FixSetChecksum(inc, incLoc.Offset)
	h := openImg(t, inc)
	es3, _ := h.ReadDir(h.Root())
	if m := byName(t, es3, "nozone").Times.Modified; !m.T.Equal(wall(1, 2, 4, 0)) && !m.T.Equal(wall(1, 2, 5, 0)) {
		t.Errorf("bad increment shifted the time: %v", m.T)
	}
}

func TestExfatUnallocatedBitmap(t *testing.T) {
	files := []exfattest.File{
		{Path: "/keep.bin", Data: pattern(9000, 1)},
		{Path: "/gone.bin", Data: pattern(20000, 2), Deleted: true},
		{Path: "/frag.bin", Data: pattern(5*4096, 3), Fragmented: true},
		{Path: "/d", Dir: true},
		{Path: "/d/x", Data: []byte("x")},
	}
	for _, cc := range []int{256, 250, 99} { // 250 and 99 are not multiples of 8: padding bits
		img, lay := exfattest.BuildLayout(exfattest.Options{ClusterCount: cc}, files)
		f := openImg(t, img)
		got, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		var want []filesys.Run
		for _, c := range lay.FreeClusters() {
			want = append(want, filesys.Run{Offset: lay.ClusterOffset(c), Length: int64(lay.ClusterSize)})
		}
		want = filesys.MergeRuns(want)
		if !slices.Equal(got, want) {
			t.Errorf("cc=%d: Unallocated = %v\nwant %v", cc, got, want)
		}
		if len(got) < 3 {
			t.Errorf("cc=%d: expected several free runs, got %v", cc, got)
		}
		// The deleted file's clusters are in there.
		del := lay.Find("/gone.bin", true)
		in := false
		for _, r := range got {
			if r.Offset <= lay.ClusterOffset(del.FirstCluster) && lay.ClusterOffset(del.FirstCluster)+int64(lay.ClusterSize) <= r.Offset+r.Length {
				in = true
			}
		}
		if !in {
			t.Errorf("cc=%d: deleted file's cluster not reported free", cc)
		}
		// Nothing past the heap end, sorted and merged.
		end := lay.HeapOffset + int64(cc)*int64(lay.ClusterSize)
		for i, r := range got {
			if r.Offset < lay.HeapOffset || r.Offset+r.Length > end || r.Length <= 0 || (i > 0 && r.Offset <= got[i-1].Offset+got[i-1].Length) {
				t.Errorf("cc=%d: bad run %d: %v", cc, i, r)
			}
		}
		if len(f.Info().Warnings) != 0 {
			t.Errorf("cc=%d: warnings %v", cc, f.Info().Warnings)
		}
	}
}

func TestExfatHostile(t *testing.T) {
	base := func(files ...exfattest.File) ([]byte, *exfattest.Layout) {
		return exfattest.BuildLayout(exfattest.Options{Label: "H"}, files)
	}
	// walkAndRead exercises everything and fails the test on a panic or hang.
	exercise := func(t *testing.T, img []byte) *exfat.FS {
		t.Helper()
		f, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			if !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUnsupported) {
				t.Fatalf("Open: unexpected error %v", err)
			}
			return nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, err error) error {
				if err == nil && e.Type == filesys.TypeFile && !e.Deleted {
					if fl, err := f.Open(e); err == nil {
						buf := make([]byte, 4096)
						for off := int64(0); off < fl.Size() && off < 1<<20; off += 4096 {
							_, _ = fl.ReadAt(buf, off)
						}
					}
				}
				return nil
			})
			_, _ = f.Unallocated()
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the reader did not finish (loop on hostile input?)")
		}
		return f
	}

	t.Run("shifts", func(t *testing.T) {
		img, _ := base()
		for _, sh := range [][2]byte{{0, 0}, {8, 3}, {13, 3}, {255, 255}, {9, 17}, {12, 14}, {9, 26}} {
			bad := bytes.Clone(img)
			bad[108], bad[109] = sh[0], sh[1]
			exfattest.FixBootChecksum(bad)
			if exfat.Probe(bytes.NewReader(bad), int64(len(bad))) {
				t.Errorf("Probe true for shifts %v", sh)
			}
			if _, err := exfat.Open(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("Open(shifts %v) = %v, want corrupt", sh, err)
			}
		}
		bad := bytes.Clone(img)
		bad[110] = 3 // NumberOfFats
		if _, err := exfat.Open(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("Open(3 FATs) = %v", err)
		}
	})

	t.Run("geometry", func(t *testing.T) {
		img, _ := base()
		type patch struct {
			name string
			off  int
			v32  uint32
			v64  uint64
		}
		for _, p := range []patch{
			{"FatOffset 0", 80, 0, 0},
			{"FatLength 0", 84, 0, 0},
			{"FatLength huge", 84, 0xFFFFFFFF, 0},
			{"HeapOffset 0", 88, 0, 0},
			{"HeapOffset beyond image", 88, 0xFFFFFFF0, 0},
			{"HeapOffset before FAT end", 88, 25, 0},
			{"ClusterCount 0", 92, 0, 0},
			{"Root 0", 96, 0, 0},
			{"Root 1", 96, 1, 0},
			{"Root beyond", 96, 0xFFFFFFFF, 0},
			{"VolumeLength 0", 72, 0, 0},
			{"VolumeLength tiny", 72, 0, 3},
		} {
			bad := bytes.Clone(img)
			if p.off == 72 {
				le64(bad, 72, p.v64)
			} else {
				le32(bad, p.off, p.v32)
			}
			exfattest.FixBootChecksum(bad)
			if f := exercise(t, bad); f == nil {
				continue // refused with a corrupt error: fine
			}
		}
	})

	t.Run("clustercount-beyond-volume", func(t *testing.T) {
		img, lay := base(exfattest.File{Path: "/a.txt", Data: []byte("still readable")})
		bad := bytes.Clone(img)
		le32(bad, 92, 0xFFFFFF) // ClusterCount far beyond what the image holds
		exfattest.FixBootChecksum(bad)
		f := openImg(t, bad)
		if !hasWarn(f, "ClusterCount") {
			t.Errorf("no ClusterCount warning: %v", f.Info().Warnings)
		}
		if f.ClusterCount() != lay.ClusterCount {
			t.Errorf("ClusterCount clamped to %d, want %d", f.ClusterCount(), lay.ClusterCount)
		}
		e, err := f.Lookup("/a.txt")
		if err != nil || string(readAll(t, f, e)) != "still readable" {
			t.Errorf("Lookup after clamp = %v", err)
		}
		runs, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Offset+r.Length > int64(len(bad)) {
				t.Errorf("free run %v beyond the image", r)
			}
		}
		// Beyond the spec maximum too.
		le32(bad, 92, 0xFFFFFFFF)
		exfattest.FixBootChecksum(bad)
		exercise(t, bad)
	})

	t.Run("volume-length-beyond-image", func(t *testing.T) {
		img, _ := base()
		bad := bytes.Clone(img)
		le64(bad, 72, 1<<62)
		exfattest.FixBootChecksum(bad)
		f := openImg(t, bad)
		if !hasWarn(f, "VolumeLength") || f.Info().Size != int64(len(bad)) {
			t.Errorf("warnings %v size %d", f.Info().Warnings, f.Info().Size)
		}
		// A shorter image than the volume claims (a truncated acquisition).
		cut := img[:len(img)-5*4096]
		g, err := exfat.Open(bytes.NewReader(cut), int64(len(cut)))
		if err != nil {
			t.Fatal(err)
		}
		if g.Info().Size != int64(len(cut)) || !hasWarn(g, "VolumeLength") {
			t.Errorf("truncated: size %d warnings %v", g.Info().Size, g.Info().Warnings)
		}
		exercise(t, cut)
	})

	t.Run("bitmap-cluster-out-of-range", func(t *testing.T) {
		img, lay := base(exfattest.File{Path: "/a", Data: []byte("a")})
		for _, v := range []uint32{0, 1, 0xFFFFFFF0, lay.ClusterCount + 2, 0xFFFFFFFF} {
			bad := bytes.Clone(img)
			le32(bad, int(lay.ClusterOffset(lay.RootCluster))+20, v) // the bitmap entry is the first root entry
			f := openImg(t, bad)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatalf("cluster %d: Unallocated error %v", v, err)
			}
			if len(runs) != 0 {
				t.Errorf("cluster %d: free runs reported from an unusable bitmap: %v", v, runs)
			}
			if !hasWarn(f, "allocation bitmap") {
				t.Errorf("cluster %d: no allocation bitmap warning: %v", v, f.Info().Warnings)
			}
			if e, err := f.Lookup("/a"); err != nil {
				t.Errorf("Lookup: %v %v", e, err)
			}
		}
		// A bitmap shorter than the cluster count: the part it does not cover
		// is never reported free.
		bad := bytes.Clone(img)
		le64(bad, int(lay.ClusterOffset(lay.RootCluster))+24, 4)
		f := openImg(t, bad)
		runs, err := f.Unallocated()
		if err != nil || !hasWarn(f, "allocation bitmap") {
			t.Errorf("short bitmap: %v %v", err, f.Info().Warnings)
		}
		for _, r := range runs {
			if r.Offset+r.Length > lay.ClusterOffset(2)+4*8*int64(lay.ClusterSize) {
				t.Errorf("free run %v beyond the bitmap's coverage", r)
			}
		}
	})

	t.Run("secondary-count-255", func(t *testing.T) {
		img, lay := base(
			exfattest.File{Path: "/before", Data: []byte("1")},
			exfattest.File{Path: "/victim", Data: []byte("2")},
			exfattest.File{Path: "/after", Data: []byte("3")},
		)
		bad := bytes.Clone(img)
		bad[lay.Find("/victim", false).Offset+1] = 255
		f := openImg(t, bad)
		es, err := f.ReadDir(f.Root())
		if err != nil {
			t.Fatal(err)
		}
		got := names(es)
		if !slices.Contains(got, "before") || !slices.Contains(got, "after") {
			t.Errorf("neighbours lost: %q", got)
		}
		if slices.Contains(got, "victim") {
			t.Errorf("the set with SecondaryCount 255 was listed: %q", got)
		}
		if !hasWarn(f, "SecondaryCount") {
			t.Errorf("no SecondaryCount warning: %v", f.Info().Warnings)
		}
		// 19 is one more than any valid File set (1 stream + 17 names).
		bad[lay.Find("/victim", false).Offset+1] = 19
		g := openImg(t, bad)
		es, _ = g.ReadDir(g.Root())
		if slices.Contains(names(es), "victim") || !slices.Contains(names(es), "after") {
			t.Errorf("SecondaryCount 19: %q", names(es))
		}
		// A deleted set with SecondaryCount 255 is skipped too.
		bad[lay.Find("/victim", false).Offset] = 0x05
		bad[lay.Find("/victim", false).Offset+1] = 255
		h := openImg(t, bad)
		es, _ = h.ReadDir(h.Root())
		if !slices.Contains(names(es), "after") {
			t.Errorf("deleted SecondaryCount 255: %q", names(es))
		}
	})

	t.Run("name-length-beyond-name-entries", func(t *testing.T) {
		img, lay := base(
			exfattest.File{Path: "/fifteen-chars-ab", Data: []byte("1")}, // 17 units: two Name entries
			exfattest.File{Path: "/abcdefghijklmno", Data: []byte("2")},
			exfattest.File{Path: "/after", Data: []byte("3")},
		)
		for _, nl := range []byte{40, 255} {
			bad := bytes.Clone(img)
			loc := lay.Find("/abcdefghijklmno", false)
			bad[loc.Offset+32+3] = nl // NameLength, but only one Name entry follows
			exfattest.FixSetChecksum(bad, loc.Offset)
			f := openImg(t, bad)
			es, err := f.ReadDir(f.Root())
			if err != nil {
				t.Fatal(err)
			}
			if len(es) != 3 {
				t.Fatalf("NameLength %d: %q", nl, names(es))
			}
			short := byName(t, es, "abcdefghijklmno")
			if !hasAttr(short, "checksum", "bad") || !hasAttr(short, "name", "truncated") {
				t.Errorf("NameLength %d: truncated set must be flagged: %v", nl, short.Attrs)
			}
			if !slices.Contains(names(es), "after") {
				t.Errorf("neighbour lost: %q", names(es))
			}
		}
		// A name shorter than its entries is a mismatch too.
		bad := bytes.Clone(img)
		loc := lay.Find("/fifteen-chars-ab", false)
		bad[loc.Offset+32+3] = 3
		exfattest.FixSetChecksum(bad, loc.Offset)
		f := openImg(t, bad)
		es, _ := f.ReadDir(f.Root())
		var e filesys.Entry
		for _, x := range es {
			if strings.HasPrefix(x.Name, "fif") {
				e = x
			}
		}
		if e.Name != "fif" || !hasAttr(e, "checksum", "bad") {
			t.Errorf("short NameLength: %+v", e)
		}
		// NameLength 0.
		bad2 := bytes.Clone(img)
		bad2[loc.Offset+32+3] = 0
		exfattest.FixSetChecksum(bad2, loc.Offset)
		exercise(t, bad2)
	})

	t.Run("cyclic-fat-chain", func(t *testing.T) {
		data := pattern(3*4096, 9)
		files := []exfattest.File{
			{Path: "/loop.bin", Data: data, FatChain: true},
			{Path: "/self.bin", Data: data, FatChain: true},
			{Path: "/dir", Dir: true, FatChain: true},
		}
		for i := 0; i < 120; i++ { // three directory clusters
			files = append(files, exfattest.File{Path: fmt.Sprintf("/dir/f%03d", i)})
		}
		img, lay := base(files...)
		bad := bytes.Clone(img)
		lp := lay.Find("/loop.bin", false).Clusters
		le32(bad, int(lay.FatEntryOffset(lp[1])), lp[0]) // 0 -> 1 -> 0
		sf := lay.Find("/self.bin", false).Clusters
		le32(bad, int(lay.FatEntryOffset(sf[1])), sf[1]) // 1 -> 1
		f := openImg(t, bad)
		es, _ := f.ReadDir(f.Root())
		for _, name := range []string{"loop.bin", "self.bin"} {
			if _, err := f.Open(byName(t, es, name)); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("Open(%s) = %v, want a corrupt error", name, err)
			}
		}
		// A cycle in a directory chain ends the listing with a warning.
		dc := lay.Find("/dir", false).Clusters
		if len(dc) != 3 {
			t.Fatalf("directory has %d clusters", len(dc))
		}
		le32(bad, int(lay.FatEntryOffset(dc[1])), dc[0]) // 0 -> 1 -> 0
		g := openImg(t, bad)
		sub := readDir(t, g, "/dir")
		if len(sub) == 0 || len(sub) >= 120 {
			t.Errorf("/dir lists %d entries, want those of the two clusters before the loop", len(sub))
		}
		if !hasWarn(g, "cycle") {
			t.Errorf("no cycle warning: %v", g.Info().Warnings)
		}
		// The root directory's chain looping on itself.
		bad2 := bytes.Clone(img)
		le32(bad2, int(lay.FatEntryOffset(lay.RootCluster)), lay.RootCluster)
		h := openImg(t, bad2)
		if _, err := h.ReadDir(h.Root()); err != nil {
			t.Errorf("ReadDir(root) with a self-loop: %v", err)
		}
		if !hasWarn(h, "cycle") {
			t.Errorf("no cycle warning: %v", h.Info().Warnings)
		}
	})

	t.Run("bad-chain-entries", func(t *testing.T) {
		data := pattern(3*4096, 5)
		img, lay := base(exfattest.File{Path: "/f.bin", Data: data, FatChain: true})
		cl := lay.Find("/f.bin", false).Clusters
		for name, v := range map[string]uint32{"free": 0, "reserved": 1, "bad cluster": 0xFFFFFFF7, "beyond": lay.ClusterCount + 2, "huge": 0xFFFFFFF0} {
			bad := bytes.Clone(img)
			le32(bad, int(lay.FatEntryOffset(cl[0])), v)
			f := openImg(t, bad)
			e, err := f.Lookup("/f.bin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("%s: Open = %v, want a corrupt error", name, err)
			}
		}
		// End of chain too early: DataLength exceeds the chain capacity.
		bad := bytes.Clone(img)
		le32(bad, int(lay.FatEntryOffset(cl[1])), 0xFFFFFFFF)
		f := openImg(t, bad)
		e, _ := f.Lookup("/f.bin")
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("short chain: Open = %v", err)
		}
		// Any of the EOC values ends a chain.
		for _, eoc := range []uint32{0xFFFFFFF8, 0xFFFFFFFE, 0xFFFFFFFF} {
			ok := bytes.Clone(img)
			le32(ok, int(lay.FatEntryOffset(cl[2])), eoc)
			g := openImg(t, ok)
			e, _ := g.Lookup("/f.bin")
			if got := readAll(t, g, e); !bytes.Equal(got, data) {
				t.Errorf("EOC %#x: content differs", eoc)
			}
		}
	})

	t.Run("extent-validation", func(t *testing.T) {
		data := pattern(2*4096, 6)
		img, lay := base(
			exfattest.File{Path: "/nofat.bin", Data: data},
			exfattest.File{Path: "/chain.bin", Data: data, FatChain: true},
		)
		patch := func(path string, f func(entry []byte)) filesys.Entry {
			t.Helper()
			bad := bytes.Clone(img)
			loc := lay.Find(path, false)
			f(bad[loc.Offset+32 : loc.Offset+64])
			exfattest.FixSetChecksum(bad, loc.Offset)
			g := openImg(t, bad)
			e, err := g.Lookup(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.Open(e)
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("%s: Open = %v, want a corrupt error", path, err)
			}
			return e
		}
		for _, path := range []string{"/nofat.bin", "/chain.bin"} {
			patch(path, func(s []byte) { le64(s[24:], 0, 1<<40) })      // DataLength beyond the volume
			patch(path, func(s []byte) { le64(s[24:], 0, 1<<63) })      // beyond int64
			patch(path, func(s []byte) { le64(s[24:], 0, ^uint64(0)) }) // all ones
			patch(path, func(s []byte) { le64(s[8:], 0, 2*4096+1) })    // ValidDataLength > DataLength
			patch(path, func(s []byte) { le64(s[8:], 0, ^uint64(0)) })  // ValidDataLength all ones
			patch(path, func(s []byte) { le32(s[20:], 0, 0) })          // no cluster but data
			patch(path, func(s []byte) { le32(s[20:], 0, 1) })          // reserved cluster
			patch(path, func(s []byte) { le32(s[20:], 0, 0xFFFFFFFF) }) // beyond the heap
			patch(path, func(s []byte) { le32(s[20:], 0, lay.ClusterCount+2) })
		}
		// A NoFatChain file that runs off the end of the heap.
		patch("/nofat.bin", func(s []byte) { le32(s[20:], 0, lay.ClusterCount+1) })
		// DataLength just beyond the chain.
		patch("/chain.bin", func(s []byte) { le64(s[24:], 0, 3*4096) })
		// Spare capacity is fine: DataLength less than the chain holds.
		bad := bytes.Clone(img)
		loc := lay.Find("/chain.bin", false)
		le64(bad[loc.Offset+32:], 24, 4096+1)
		le64(bad[loc.Offset+32:], 8, 4096+1)
		exfattest.FixSetChecksum(bad, loc.Offset)
		g := openImg(t, bad)
		e, _ := g.Lookup("/chain.bin")
		if got := readAll(t, g, e); !bytes.Equal(got, data[:4097]) {
			t.Errorf("shortened file differs")
		}
		fl, _ := g.Open(e)
		if err := filesys.CheckRuns(fl.Runs(), fl.Size(), g.Info().Size); err != nil {
			t.Error(err)
		}
	})

	t.Run("directory-problems", func(t *testing.T) {
		img, lay := base(
			exfattest.File{Path: "/sub", Dir: true},
			exfattest.File{Path: "/sub/f", Data: []byte("f")},
			exfattest.File{Path: "/outside", Dir: true},
			exfattest.File{Path: "/loopdir", Dir: true},
		)
		// A directory whose first cluster is out of range: ReadDir reports it.
		bad := bytes.Clone(img)
		loc := lay.Find("/sub", false)
		le32(bad, int(loc.Offset)+32+20, 0xFFFFFF00)
		exfattest.FixSetChecksum(bad, loc.Offset)
		f := openImg(t, bad)
		d, err := f.Lookup("/sub")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadDir(d); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir(bad cluster) = %v, want corrupt", err)
		}
		// A subdirectory that is the root: Walk reports a cycle and returns.
		bad2 := bytes.Clone(img)
		l2 := lay.Find("/loopdir", false)
		le32(bad2, int(l2.Offset)+32+20, lay.RootCluster)
		exfattest.FixSetChecksum(bad2, l2.Offset)
		g := openImg(t, bad2)
		cycles := 0
		_ = filesys.Walk(g, g.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
			if errors.Is(err, filesys.ErrCorrupt) {
				cycles++
			}
			return nil
		})
		if cycles == 0 {
			t.Error("directory pointing at the root was not reported as a cycle")
		}
		// An absurd directory size is capped (256 MiB) without allocating it.
		bad3 := bytes.Clone(img)
		l3 := lay.Find("/outside", false)
		le64(bad3[l3.Offset+32:], 24, 1<<50)
		le64(bad3[l3.Offset+32:], 8, 1<<50)
		exfattest.FixSetChecksum(bad3, l3.Offset)
		exercise(t, bad3)
		h := openImg(t, bad3)
		if es := readDir(t, h, "/outside"); len(es) != 0 {
			t.Errorf("/outside = %q", names(es))
		}
		// A directory with no end marker and a non-multiple-of-32 size.
		bad4 := bytes.Clone(img)
		l4 := lay.Find("/sub", false)
		le64(bad4[l4.Offset+32:], 24, 100)
		exfattest.FixSetChecksum(bad4, l4.Offset)
		exercise(t, bad4)
	})

	t.Run("upcase", func(t *testing.T) {
		img, lay := base(exfattest.File{Path: "/Name", Data: []byte("n")})
		entry := int(lay.ClusterOffset(lay.RootCluster)) + 32 // the up-case entry
		for name, mut := range map[string]func(b []byte){
			"oversized":    func(b []byte) { le64(b, entry+24, 128<<10+2) },
			"odd length":   func(b []byte) { le64(b, entry+24, 515) },
			"bad checksum": func(b []byte) { le32(b, entry+4, 12345) },
			"bad cluster":  func(b []byte) { le32(b, entry+20, 0xFFFFFFF0) },
			"huge":         func(b []byte) { le64(b, entry+24, 1<<60) },
		} {
			bad := bytes.Clone(img)
			mut(bad)
			f := openImg(t, bad)
			if f.UpcaseLoaded() {
				t.Errorf("%s: table used", name)
			}
			if !hasWarn(f, "up-case") {
				t.Errorf("%s: no up-case warning: %v", name, f.Info().Warnings)
			}
			if _, err := f.Lookup("/NAME"); err != nil {
				t.Errorf("%s: fallback lookup: %v", name, err)
			}
		}
		// A table with a truncated compression marker still decodes.
		bad := bytes.Clone(img)
		uc := int(lay.ClusterOffset(lay.UpcaseCluster))
		le32(bad, entry+4, 0)
		le64(bad, entry+24, 6)
		le16(bad, uc, 0xFFFF) // 0xFFFF, count... then the table ends
		le16(bad, uc+2, 0xFFFF)
		le16(bad, uc+4, 0xFFFF)
		tab := bad[uc : uc+6]
		le32(bad, entry+4, exfat.TableChecksum(tab))
		exercise(t, bad)
	})

	t.Run("truncated-and-short-fat", func(t *testing.T) {
		files := []exfattest.File{{Path: "/a", Data: pattern(20000, 1)}, {Path: "/d", Dir: true}, {Path: "/d/b", Data: pattern(9000, 2), FatChain: true}}
		img, lay := base(files...)
		for _, cut := range []int{512, 11 * 512, 12 * 512, int(lay.FatOffset), int(lay.FatOffset) + 100, int(lay.HeapOffset), int(lay.ClusterOffset(lay.RootCluster)), int(lay.ClusterOffset(lay.RootCluster)) + 40, len(img) - 1} {
			exercise(t, img[:cut])
		}
		// A FAT shorter than the cluster count needs.
		bad := bytes.Clone(img)
		le32(bad, 84, 1)
		exfattest.FixBootChecksum(bad)
		f := exercise(t, bad)
		if f != nil && !hasWarn(f, "FAT") {
			t.Errorf("short FAT not reported: %v", f.Info().Warnings)
		}
	})

	t.Run("label-and-bitmap-entries", func(t *testing.T) {
		img, lay := base()
		for _, c := range []byte{12, 40, 255} { // CharacterCount beyond the 11 units
			bad := bytes.Clone(img)
			bad[int(lay.ClusterOffset(lay.RootCluster))+2*32+1] = c
			f := openImg(t, bad)
			if len(f.Info().Label) > 11 {
				t.Errorf("label %q longer than 11 units", f.Info().Label)
			}
		}
	})
}

func TestExfatOpenAndIDs(t *testing.T) {
	files := []exfattest.File{
		{Path: "/f.txt", Data: []byte("content")},
		{Path: "/d", Dir: true},
		{Path: "/gone", Data: []byte("x"), Deleted: true},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	f := openImg(t, img)
	es, _ := f.ReadDir(f.Root())
	file, dir, gone := byName(t, es, "f.txt"), byName(t, es, "d"), byName(t, es, "gone")

	if _, err := f.Open(dir); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(dir) = %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(f.Root()); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(root) = %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(gone); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(deleted) = %v", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "garbage", Deleted: true}); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("a deleted entry is ErrDeleted whatever its ID: %v", err)
	}
	if _, err := f.ReadDir(file); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(file) = %v, want ErrUnsupported", err)
	}
	if _, err := f.ReadDir(gone); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("ReadDir(deleted) = %v", err)
	}

	rc := strconv.Itoa(int(lay.RootCluster))
	for _, id := range []string{
		"", "inode:5", "dirent", "dirent:", "dirent:" + rc, "dirent:" + rc + ":", "dirent::3", "dirent:" + rc + ":3:1",
		"dirent:0" + rc + ":3", "dirent:" + rc + ":03", "dirent:+" + rc + ":3", "dirent:-" + rc + ":3", "dirent: " + rc + ":3",
		"dirent:" + rc + ":-3", "dirent:" + rc + ":0x3", "dirent:99999999999:3", "dirent:" + rc + ":99999999999", "DIRENT:" + rc + ":3",
	} {
		e := file
		e.ID = id
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(ID %q) = %v, want ErrNotFound", id, err)
		}
	}
	for _, id := range []string{"", "dir", "dir:", "dir:" + rc + ":", "dir:0" + rc, "dir:-1", "dir:+" + rc, "dir: " + rc, "dir:0x4", "dir:99999999999", "dirent:1", "nid:3"} {
		e := dir
		e.ID = id
		if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(ID %q) = %v, want ErrNotFound", id, err)
		}
	}
	// A well-formed ID of a cluster that cannot hold a directory is a corrupt
	// volume, not "not found".
	for _, id := range []string{"dir:0", "dir:1", "dir:4294967295", fmt.Sprintf("dir:%d", lay.ClusterCount+2)} {
		e := dir
		e.ID = id
		if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir(ID %q) = %v, want a corrupt error", id, err)
		}
	}
	// An entry that did not come from ReadDir carries no extent: refused.
	bare := filesys.Entry{ID: file.ID, Type: filesys.TypeFile, Size: 7}
	if _, err := f.Open(bare); err == nil {
		t.Error("Open of an entry without its attributes succeeded")
	}
	// Forged attributes never panic.
	for _, kv := range [][]filesys.KV{
		{{Key: "first_cluster", Value: "-1"}},
		{{Key: "first_cluster", Value: "x"}},
		{{Key: "first_cluster", Value: "99999999999999999999"}},
		{{Key: "first_cluster", Value: "4"}, {Key: "valid_data_length", Value: "zz"}},
		{{Key: "first_cluster", Value: "4"}, {Key: "valid_data_length", Value: "99999999"}},
	} {
		e := file
		e.Attrs = kv
		if _, err := f.Open(e); err == nil {
			t.Errorf("Open with attrs %v succeeded", kv)
		}
	}
	// A negative size.
	neg := file
	neg.Size = -5
	if _, err := f.Open(neg); err == nil {
		t.Error("Open with a negative size succeeded")
	}
	// The directory hop is stable across runs and independent of the instance.
	g := openImg(t, img)
	es2, _ := g.ReadDir(g.Root())
	for i := range es {
		if es[i].ID != es2[i].ID {
			t.Errorf("IDs differ across instances: %q vs %q", es[i].ID, es2[i].ID)
		}
	}
}

func TestInfoWarningsAccumulateAcrossReads(t *testing.T) {
	data := pattern(3*4096, 9)
	files := []exfattest.File{
		{Path: "/ok.txt", Data: []byte("fine")},
		{Path: "/loop.bin", Data: data, FatChain: true},
		{Path: "/sc", Data: []byte("x")},
		{Path: "/d", Dir: true, FatChain: true},
	}
	for i := 0; i < 120; i++ { // three directory clusters
		files = append(files, exfattest.File{Path: fmt.Sprintf("/d/f%03d", i)})
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	lp := lay.Find("/loop.bin", false).Clusters
	le32(img, int(lay.FatEntryOffset(lp[1])), lp[0])
	img[lay.Find("/sc", false).Offset+1] = 200
	dc := lay.Find("/d", false).Clusters
	le32(img, int(lay.FatEntryOffset(dc[1])), dc[0]) // the directory chain loops back

	f := openImg(t, img)
	if n := len(f.Info().Warnings); n != 0 {
		t.Fatalf("warnings at Open: %v", f.Info().Warnings)
	}
	before := f.Info()
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	afterRoot := f.Info().Warnings
	if len(afterRoot) <= len(before.Warnings) || !hasWarn(f, "SecondaryCount") {
		t.Errorf("ReadDir added no warning: %v", afterRoot)
	}
	// The earlier snapshot is unaffected, and mutating a snapshot changes nothing.
	if len(before.Warnings) != 0 {
		t.Errorf("snapshot mutated: %v", before.Warnings)
	}
	snap := f.Info()
	if len(snap.Warnings) > 0 {
		snap.Warnings[0] = "tampered"
		if f.Info().Warnings[0] == "tampered" {
			t.Error("Info().Warnings aliases the live list")
		}
	}
	if _, err := f.Open(byName(t, es, "loop.bin")); err == nil {
		t.Fatal("cyclic chain opened")
	}
	n1 := len(f.Info().Warnings)
	if _, err := f.ReadDir(byName(t, es, "d")); err != nil {
		t.Fatal(err)
	}
	n2 := len(f.Info().Warnings)
	if n2 <= n1 {
		t.Errorf("reading a damaged directory added no warning: %v", f.Info().Warnings)
	}
	// Repeating the reads adds nothing: identical messages are recorded once.
	_, _ = f.ReadDir(f.Root())
	_, _ = f.ReadDir(byName(t, es, "d"))
	_, _ = f.Unallocated()
	if n3 := len(f.Info().Warnings); n3 != n2 {
		t.Errorf("duplicates recorded: %d -> %d: %v", n2, n3, f.Info().Warnings)
	}
}

func TestExfatWarningCapAndConcurrency(t *testing.T) {
	img := exfattest.Build(exfattest.Options{}, nil)
	f := openImg(t, img)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				f.Warn("distinct warning %d", i)
				_ = f.Info()
			}
		}()
	}
	wg.Wait()
	if n := len(f.Info().Warnings); n != 300 {
		t.Errorf("%d warnings, want 300 distinct", n)
	}
	f.Warn("literal 100%% text") // no arguments: taken literally, not formatted
	if !slices.Contains(f.Info().Warnings, "literal 100%% text") {
		t.Errorf("a warning without arguments must be kept literally: %q", f.Info().Warnings[len(f.Info().Warnings)-1])
	}
	for i := 0; i < 1500; i++ {
		f.Warn("another %d", i)
	}
	w := f.Info().Warnings
	if len(w) != 1001 || w[1000] != "further warnings suppressed" {
		t.Errorf("%d warnings, last %q", len(w), w[len(w)-1])
	}
}

func TestExfatDirRecordCap(t *testing.T) {
	var files []exfattest.File
	for i := 0; i < 50; i++ {
		files = append(files, exfattest.File{Path: fmt.Sprintf("/f%02d", i), Data: []byte("x")})
	}
	files = append(files, exfattest.File{Path: "/f49", Data: []byte("dup"), Deleted: true})
	img := exfattest.Build(exfattest.Options{}, files)
	f := openImg(t, img)
	f.SetDirRecordCap(10)
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 10 {
		t.Fatalf("ReadDir = %d entries, %v", len(es), err)
	}
	if !hasWarn(f, "more than 10") {
		t.Errorf("no cap warning: %v", f.Info().Warnings)
	}
	// Lookup is bounded by the same cap: an entry beyond it is not found.
	if _, err := f.Lookup("/f05"); err != nil {
		t.Errorf("Lookup within cap: %v", err)
	}
	if _, err := f.Lookup("/f40"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup beyond cap = %v, want ErrNotFound", err)
	}
}

func TestExfatBigGeometries(t *testing.T) {
	for _, g := range []struct {
		bps, spc uint8
		cc       int
	}{{9, 0, 100}, {10, 2, 300}, {12, 0, 64}, {12, 3, 40}, {9, 7, 128}} {
		files := []exfattest.File{
			{Path: "/a.bin", Data: pattern(100000, 1)},
			{Path: "/d", Dir: true},
			{Path: "/d/b.bin", Data: pattern(33333, 2), FatChain: true},
			{Path: "/x", Data: pattern(70000, 3), Fragmented: true},
		}
		img, lay := exfattest.BuildLayout(exfattest.Options{BytesPerSectorShift: g.bps, SectorsPerClusterShift: g.spc, ClusterCount: g.cc}, files)
		f, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatalf("%+v: %v", g, err)
		}
		if f.Info().BlockSize != lay.ClusterSize || len(f.Info().Warnings) != 0 {
			t.Errorf("%+v: Info %+v", g, f.Info())
		}
		for path, want := range map[string][]byte{"/a.bin": pattern(100000, 1), "/d/b.bin": pattern(33333, 2), "/x": pattern(70000, 3)} {
			e, err := f.Lookup(path)
			if err != nil {
				t.Fatalf("%+v %s: %v", g, path, err)
			}
			if got := readAll(t, f, e); !bytes.Equal(got, want) {
				t.Errorf("%+v %s differs", g, path)
			}
		}
	}
}

// TestExfatMutationsNeverPanic flips bytes in the metadata of a rich image and
// runs the whole reader over the result: it must not panic or hang.
func TestExfatMutationsNeverPanic(t *testing.T) {
	t.Parallel()
	vdl := int64(5000)
	files := []exfattest.File{
		{Path: "/a.txt", Data: pattern(6000, 1), ValidLength: &vdl},
		{Path: "/long-name-spanning-several-entries.dat", Data: pattern(9000, 2), FatChain: true},
		{Path: "/frag", Data: pattern(5*4096, 3), Fragmented: true},
		{Path: "/gone.bin", Data: pattern(3000, 4), Deleted: true},
		{Path: "/dir", Dir: true},
		{Path: "/dir/inner", Data: []byte("inner")},
		{Path: "/dir/sub", Dir: true, FatChain: true},
		{Path: "/dir/sub/deep", Data: []byte("deep")},
		{Path: "/emoji-😀", RawName: []uint16{'a', 0xD800}, Data: []byte("e")},
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{Label: "MUT"}, files)
	type region struct{ off, n int }
	regions := []region{{0, 128}, {int(lay.FatOffset), 160}, {int(lay.ClusterOffset(lay.BitmapCluster)), 40}}
	for c := range lay.Entries {
		regions = append(regions, region{int(lay.Entries[c].Offset), 96})
	}
	regions = append(regions, region{int(lay.ClusterOffset(lay.RootCluster)), 160})
	seed := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	run := func(b []byte) {
		f, err := exfat.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		_ = f.Info()
		_ = filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err == nil && !e.Deleted && e.Type == filesys.TypeFile {
				if fl, err := f.Open(e); err == nil {
					buf := make([]byte, 4096)
					for off := int64(0); off < fl.Size() && off < 1<<18; off += 4096 {
						_, _ = fl.ReadAt(buf, off)
					}
					_ = filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size)
				}
			}
			return nil
		})
		_, _ = f.Lookup("/dir/sub/deep")
		_, _ = f.Unallocated()
	}
	iters := 1500
	if testing.Short() {
		iters = 500
	}
	for i := 0; i < iters; i++ {
		b := bytes.Clone(img)
		for k := int(next()%6) + 1; k > 0; k-- {
			r := regions[next()%uint64(len(regions))]
			pos := r.off + int(next()%uint64(r.n))
			if pos < len(b) {
				b[pos] = byte(next())
			}
		}
		if next()%8 == 0 {
			b = b[:next()%uint64(len(b))]
		}
		run(b)
	}
}
