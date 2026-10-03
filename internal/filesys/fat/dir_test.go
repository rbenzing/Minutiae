package fat_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

var allTypes = []int{12, 16, 32}

// attr returns the value of the attribute key, "" when absent.
func attr(e filesys.Entry, key string) string {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

func hasAttr(e filesys.Entry, key string) bool {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return true
		}
	}
	return false
}

func readDir(t testing.TB, f *fat.FS, p string) []filesys.Entry {
	t.Helper()
	d, err := f.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	es, err := f.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", p, err)
	}
	return es
}

func byName(es []filesys.Entry, name string) (filesys.Entry, bool) {
	for _, e := range es {
		if e.Name == name {
			return e, true
		}
	}
	return filesys.Entry{}, false
}

func names(es []filesys.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func readAll(t testing.TB, f *fat.FS, e filesys.Entry) []byte {
	t.Helper()
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", e.Name, err)
	}
	b := make([]byte, fl.Size())
	if n, err := fl.ReadAt(b, 0); n != len(b) || (err != nil && err != io.EOF) {
		t.Fatalf("ReadAt(%q) = %d, %v", e.Name, n, err)
	}
	return b
}

// rootRegion returns the byte offset of the FAT12/16 root directory, or of the
// first FAT32 root cluster.
func rootRegion(g fattest.Geometry) int {
	if g.Type == 32 {
		return int(g.ClusterOffset(2))
	}
	return int(g.FATStart(g.NumFATs)) * g.SectorSize
}

// entryOffsets returns the offsets of the 32-byte entries in img[lo:hi] that pred accepts.
func entryOffsets(img []byte, lo, hi int, pred func(e []byte) bool) []int {
	var out []int
	for o := lo; o+32 <= hi; o += 32 {
		if pred(img[o : o+32]) {
			out = append(out, o)
		}
	}
	return out
}

func isLFNEntry(e []byte) bool { return e[11] == 0x0F }

func TestReadDirLongAndShortNames(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			content := bytes.Repeat([]byte("0123456789abcdef"), 100)
			img := build(fattest.Options{Type: typ, Label: "VOL"},
				fattest.File{Path: "README.TXT", Data: []byte("upper")},
				fattest.File{Path: "readme2.txt", Data: []byte("lower")},
				fattest.File{Path: "Mixed.Txt", Data: []byte("m"), LongName: true},
				fattest.File{Path: "A Long File Name.txt", Data: content, LongName: true},
				fattest.File{Path: "dir.d", Dir: true},
				fattest.File{Path: "dir.d/inner file.txt", Data: []byte("inner"), LongName: true},
				fattest.File{Path: "ünï-€.txt", Data: []byte("uni"), LongName: true},
				fattest.File{Path: "RO.TXT", Data: []byte("ro"), Attr: 0x01 | 0x02 | 0x04},
			)
			f := openImg(t, img)
			root := readDir(t, f, "/")
			got := names(root)
			want := []string{"README.TXT", "readme2.txt", "Mixed.Txt", "A Long File Name.txt", "dir.d", "ünï-€.txt", "RO.TXT"}
			if !slices.Equal(got, want) {
				t.Fatalf("root names = %q\nwant %q (label, '.' and '..' are not entries)", got, want)
			}
			long, _ := byName(root, "A Long File Name.txt")
			if long.Type != filesys.TypeFile || long.Size != int64(len(content)) || long.Deleted || long.RawName != nil {
				t.Errorf("long entry = %+v", long)
			}
			if attr(long, "short_name") != "ALONGF~1.TXT" || hasAttr(long, "lfn") {
				t.Errorf("long attrs = %+v, want short_name ALONGF~1.TXT and no lfn flag", long.Attrs)
			}
			if attr(long, "attr") != "A" && attr(long, "attr") != "-" {
				t.Errorf("attr = %q", attr(long, "attr"))
			}
			if c, err := strconv.Atoi(attr(long, "first_cluster")); err != nil || c < 2 {
				t.Errorf("first_cluster = %q", attr(long, "first_cluster"))
			}
			if !strings.HasPrefix(long.ID, "dirent:") || long.Mode != 0o644 || long.UID != 0 || long.GID != 0 {
				t.Errorf("ID %q mode %o uid %d gid %d", long.ID, long.Mode, long.UID, long.GID)
			}
			// NTRes case flags apply to short names without a long name.
			lower, _ := byName(root, "readme2.txt")
			if attr(lower, "short_name") != "readme2.txt" && attr(lower, "short_name") != "README2.TXT" {
				t.Errorf("short_name = %q", attr(lower, "short_name"))
			}
			ro, _ := byName(root, "RO.TXT")
			if ro.Mode != 0o444 || attr(ro, "attr") != "RHS" {
				t.Errorf("read-only entry: mode %o attr %q", ro.Mode, attr(ro, "attr"))
			}
			d, _ := byName(root, "dir.d")
			if d.Type != filesys.TypeDir || d.Mode != 0o755 || !strings.HasPrefix(d.ID, "dir:") || !strings.Contains(attr(d, "dirent"), ":") {
				t.Errorf("dir entry = %+v", d)
			}
			if d.ID != "dir:"+attr(d, "first_cluster") {
				t.Errorf("dir ID %q, first_cluster %q", d.ID, attr(d, "first_cluster"))
			}
			// Content and nested lookup, case-insensitively through long and short names.
			for _, p := range []string{"/a long file name.TXT", "/ALONGF~1.TXT", "/alongf~1.txt", "/A Long File Name.txt"} {
				e, err := f.Lookup(p)
				if err != nil {
					t.Errorf("Lookup(%q): %v", p, err)
					continue
				}
				if !bytes.Equal(readAll(t, f, e), content) {
					t.Errorf("Lookup(%q): wrong content", p)
				}
			}
			if e, err := f.Lookup("/DIR.D/INNER FILE.TXT"); err != nil || string(readAll(t, f, e)) != "inner" {
				t.Errorf("nested lookup: %v", err)
			}
			if e, err := f.Lookup("/ÜNÏ-€.TXT"); err != nil || e.Name != "ünï-€.txt" {
				t.Errorf("unicode lookup: %v %q", err, e.Name)
			}
			if _, err := f.Lookup("/missing"); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("missing: %v", err)
			}
			if _, err := f.Lookup("/README.TXT/x"); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("below a file: %v", err)
			}
			if e, err := f.Lookup("/"); err != nil || e.ID != f.Root().ID || e.Type != filesys.TypeDir {
				t.Errorf("Lookup(/) = %+v, %v", e, err)
			}
		})
	}
}

func TestLFNChecksumMismatchFallsBack(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := build(o,
		fattest.File{Path: "A Long File Name.txt", Data: []byte("x"), LongName: true},
		fattest.File{Path: "Other Long Name.txt", Data: []byte("y"), LongName: true})
	root := rootRegion(g)
	lfns := entryOffsets(img, root, root+512*32, isLFNEntry)
	if len(lfns) != 4 {
		t.Fatalf("setup: %d LFN entries, want 4", len(lfns))
	}
	img[lfns[1]+13]++ // the checksum of the first file's second (ordinal 1) entry
	f := openImg(t, img)
	es := readDir(t, f, "/")
	if got := names(es); !slices.Equal(got, []string{"ALONGF~1.TXT", "Other Long Name.txt"}) {
		t.Fatalf("names = %q, want the 8.3 fallback for the first and the long name for the second", got)
	}
	if attr(es[0], "lfn") != "orphan" || hasAttr(es[1], "lfn") {
		t.Errorf("lfn attrs: %+v / %+v", es[0].Attrs, es[1].Attrs)
	}
	if _, err := f.Lookup("/A Long File Name.txt"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("the discarded long name is still found: %v", err)
	}
	if _, err := f.Lookup("/ALONGF~1.TXT"); err != nil {
		t.Errorf("the short name is not found: %v", err)
	}
}

func TestReadDirFlagsDeletedEntries(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			o := fattest.Options{Type: typ}
			g := fattest.Layout(o)
			img := build(o,
				fattest.File{Path: "KEEP.TXT", Data: []byte("keep")},
				fattest.File{Path: "Secret Plan.doc", Data: []byte("secret data"), LongName: true, Deleted: true},
				fattest.File{Path: "OLD.TXT", Data: []byte("old"), Deleted: true},
				fattest.File{Path: "Second Secret.doc", Data: []byte("two"), LongName: true, Deleted: true},
				fattest.File{Path: "GONE.D", Dir: true, Deleted: true},
			)
			f := openImg(t, img)
			es := readDir(t, f, "/")
			want := []struct {
				name    string
				deleted bool
			}{{"KEEP.TXT", false}, {"Secret Plan.doc", true}, {"_LD.TXT", true}, {"Second Secret.doc", true}, {"_ONE.D", true}}
			if len(es) != len(want) {
				t.Fatalf("names = %q", names(es))
			}
			ids := map[string]bool{}
			for i, w := range want {
				e := es[i]
				if e.Name != w.name || e.Deleted != w.deleted {
					t.Errorf("entry %d = %q deleted=%v, want %q deleted=%v", i, e.Name, e.Deleted, w.name, w.deleted)
				}
				if ids[e.ID] {
					t.Errorf("duplicate ID %q", e.ID)
				}
				ids[e.ID] = true
				if w.deleted {
					if !strings.HasPrefix(e.ID, "dirent:") {
						t.Errorf("deleted entry ID %q", e.ID)
					}
					if c, err := strconv.Atoi(attr(e, "first_cluster")); err != nil || c < 2 {
						t.Errorf("deleted %q: first_cluster %q", e.Name, attr(e, "first_cluster"))
					}
					if _, err := f.Open(e); !errors.Is(err, filesys.ErrDeleted) {
						t.Errorf("Open(deleted %q) = %v, want ErrDeleted", e.Name, err)
					}
				}
			}
			if es[1].Size != int64(len("secret data")) || attr(es[1], "size") != strconv.Itoa(len("secret data")) {
				t.Errorf("deleted file size %d / attr %q", es[1].Size, attr(es[1], "size"))
			}
			if es[4].Type != filesys.TypeDir || !strings.HasPrefix(es[4].ID, "dirent:") {
				t.Errorf("deleted dir = %+v", es[4])
			}
			if _, err := f.ReadDir(es[4]); !errors.Is(err, filesys.ErrDeleted) {
				t.Errorf("ReadDir(deleted dir) = %v, want ErrDeleted", err)
			}
			// Deleted entries are never found by path.
			for _, p := range []string{"/Secret Plan.doc", "/OLD.TXT", "/_LD.TXT"} {
				if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
					t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
				}
			}

			// A deleted long name whose checksum does not match is not used.
			root := rootRegion(g)
			lfns := entryOffsets(img, root, root+512*32, func(e []byte) bool { return e[0] == 0xE5 && e[11] == 0x0F })
			if len(lfns) != 4 {
				t.Fatalf("setup: %d deleted LFN entries", len(lfns))
			}
			img[lfns[1]+13]++ // ordinal 1 of "Secret Plan.doc"
			es = readDir(t, openImg(t, img), "/")
			if es[1].Name != "_ECRET~1.DOC" || !es[1].Deleted {
				t.Errorf("mismatched deleted LFN: name %q, want the _ECRET~1.DOC fallback", es[1].Name)
			}
			if es[3].Name != "Second Secret.doc" {
				t.Errorf("the next deleted set must be unaffected, got %q", es[3].Name)
			}
		})
	}
}

func TestFAT1216FixedRootAndFAT32ChainRoot(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			var files []fattest.File
			for i := range 100 { // 100 entries: more than one 512-byte root cluster/sector holds
				files = append(files, fattest.File{Path: fmt.Sprintf("F%03d.TXT", i), Data: []byte{byte(i)}})
			}
			f := openImg(t, build(fattest.Options{Type: typ, Label: "LBL"}, files...))
			root := f.Root()
			wantID := "dir:0"
			if typ == 32 {
				wantID = "dir:2" // RootClus
			}
			if root.ID != wantID || root.Type != filesys.TypeDir || root.Name != "" || root.Deleted {
				t.Errorf("Root = %+v, want ID %q", root, wantID)
			}
			es, err := f.ReadDir(root)
			if err != nil || len(es) != 100 {
				t.Fatalf("ReadDir(root) = %d entries, %v", len(es), err)
			}
			for i, e := range es {
				if e.Name != fmt.Sprintf("F%03d.TXT", i) {
					t.Fatalf("entry %d = %q", i, e.Name)
				}
				if want := fmt.Sprintf("dirent:%s:", wantID[4:]); !strings.HasPrefix(e.ID, want) {
					t.Fatalf("ID %q, want prefix %q", e.ID, want)
				}
			}
			// The directory the root ID names is the root itself.
			again, err := f.ReadDir(filesys.Entry{ID: wantID, Type: filesys.TypeDir})
			if err != nil || len(again) != 100 {
				t.Errorf("ReadDir by ID: %d, %v", len(again), err)
			}
			if f.Info().Label != "LBL" {
				t.Errorf("label %q", f.Info().Label)
			}
			if e, err := f.Lookup("/f050.txt"); err != nil || readAll(t, f, e)[0] != 50 {
				t.Errorf("Lookup deep in the root: %v", err)
			}
		})
	}
}

func TestFragmentedFileRunsReproduceContent(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			data := make([]byte, 7*512+123)
			for i := range data {
				data[i] = byte(i*31 + i/512)
			}
			img := build(fattest.Options{Type: typ},
				fattest.File{Path: "FRAG.BIN", Data: data, Fragmented: true},
				fattest.File{Path: "FILL.BIN", Data: make([]byte, 7*512)},
				fattest.File{Path: "EMPTY.TXT"},
				fattest.File{Path: "EXACT.BIN", Data: bytes.Repeat([]byte{9}, 1024)},
			)
			f := openImg(t, img)
			for _, name := range []string{"FRAG.BIN", "EXACT.BIN"} {
				e, err := f.Lookup("/" + name)
				if err != nil {
					t.Fatal(err)
				}
				fl, err := f.Open(e)
				if err != nil {
					t.Fatal(err)
				}
				runs := fl.Runs()
				if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
					t.Fatalf("%s: %v (runs %v)", name, err, runs)
				}
				var got []byte
				for _, r := range runs {
					if r.Offset < 0 || r.Length <= 0 {
						t.Fatalf("%s: bad run %+v", name, r)
					}
					got = append(got, img[r.Offset:r.Offset+r.Length]...)
				}
				want := readAll(t, f, e)
				if !bytes.Equal(got, want) {
					t.Errorf("%s: runs do not reproduce the content", name)
				}
				if name == "FRAG.BIN" {
					if !bytes.Equal(want, data) {
						t.Error("FRAG.BIN content differs from the source")
					}
					if len(runs) < 4 {
						t.Errorf("FRAG.BIN has %d runs, want a fragmented file", len(runs))
					}
					if runs[len(runs)-1].Length != 123 {
						t.Errorf("last run length %d, want it trimmed to 123", runs[len(runs)-1].Length)
					}
				} else if len(runs) != 1 || runs[0].Length != 1024 {
					t.Errorf("EXACT.BIN runs = %v, want one 1024-byte run (adjacent clusters merge)", runs)
				}
				// Random-access reads, including across run boundaries and at the end.
				for _, c := range []struct{ off, n int }{{0, 1}, {511, 2}, {500, 600}, {len(want) - 1, 1}, {len(want) - 5, 5}} {
					if c.off+c.n > len(want) {
						continue
					}
					b := make([]byte, c.n)
					if n, err := fl.ReadAt(b, int64(c.off)); n != c.n || err != nil && err != io.EOF || !bytes.Equal(b, want[c.off:c.off+c.n]) {
						t.Errorf("%s: ReadAt(%d,%d) = %d, %v", name, c.off, c.n, n, err)
					}
				}
				if n, err := fl.ReadAt(make([]byte, 4), fl.Size()); n != 0 || err != io.EOF {
					t.Errorf("%s: ReadAt at EOF = %d, %v", name, n, err)
				}
				if n, err := fl.ReadAt(make([]byte, 10), fl.Size()-3); n != 3 || err != io.EOF {
					t.Errorf("%s: short read at the end = %d, %v", name, n, err)
				}
				if _, err := fl.ReadAt(make([]byte, 1), -1); err == nil {
					t.Errorf("%s: negative offset accepted", name)
				}
			}
			e, _ := f.Lookup("/EMPTY.TXT")
			fl, err := f.Open(e)
			if err != nil || fl.Size() != 0 || len(fl.Runs()) != 0 {
				t.Errorf("empty file: %v size %d runs %v", err, fl.Size(), fl.Runs())
			}
			// Runs is a copy.
			e, _ = f.Lookup("/FRAG.BIN")
			fl, _ = f.Open(e)
			fl.Runs()[0].Length = 1
			if fl.Runs()[0].Length == 1 {
				t.Error("Runs aliases the file's own list")
			}
			// Concurrent reads are safe.
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 20 {
						b := make([]byte, len(data))
						if _, err := fl.ReadAt(b, 0); err != nil && err != io.EOF || !bytes.Equal(b, data) {
							t.Error("concurrent read mismatch")
							return
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestFileChainShorterThanSize(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	data := make([]byte, 5*512)
	for i := range data {
		data[i] = byte(i%251) + 1 // never zero, so zero-filling is visible
	}
	base := build(o,
		fattest.File{Path: "SHORT.BIN", Data: data},
		fattest.File{Path: "LONG.BIN", Data: data[:2*512+10]})
	c0 := firstCluster(16) // SHORT.BIN: clusters 2..6

	cases := []struct {
		name  string
		patch func(img []byte)
		avail int
	}{
		{"end of chain after two clusters", func(img []byte) { setFAT(img, g, 0, c0+1, 0xFFFF) }, 2 * 512},
		{"free cluster in the chain", func(img []byte) { setFAT(img, g, 0, c0+2, 0) }, 2 * 512},
		{"bad cluster in the chain", func(img []byte) { setFAT(img, g, 0, c0+3, 0xFFF7) }, 3 * 512},
		{"loop", func(img []byte) { setFAT(img, g, 0, c0+3, c0+1) }, 4 * 512},
		{"pointer outside the volume", func(img []byte) { setFAT(img, g, 0, c0+1, 0xFFF0) }, 2 * 512},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := slices.Clone(base)
			c.patch(img)
			f := openImg(t, img)
			e, err := f.Lookup("/SHORT.BIN")
			if err != nil {
				t.Fatal(err)
			}
			fl, err := f.Open(e)
			if err != nil {
				t.Fatalf("Open must succeed with the available runs: %v", err)
			}
			if fl.Size() != int64(len(data)) {
				t.Errorf("Size = %d, want the directory entry's %d", fl.Size(), len(data))
			}
			var total int64
			for _, r := range fl.Runs() {
				total += r.Length
			}
			if total != int64(c.avail) {
				t.Errorf("runs cover %d bytes, want %d (%v)", total, c.avail, fl.Runs())
			}
			if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err == nil {
				t.Error("CheckRuns accepts a short chain; examine must see it fail")
			}
			// The available part reads normally; the rest is an error, never zeros.
			buf := make([]byte, len(data))
			n, err := fl.ReadAt(buf, 0)
			if n != c.avail || !isCorrupt(err) || !strings.Contains(err.Error(), "chain shorter than file size") {
				t.Fatalf("ReadAt = %d, %v; want %d bytes and a CorruptError about the chain", n, err, c.avail)
			}
			if !bytes.Equal(buf[:n], data[:n]) {
				t.Error("the available prefix is wrong")
			}
			if n, err := fl.ReadAt(make([]byte, 10), int64(c.avail)); n != 0 || !isCorrupt(err) {
				t.Errorf("read inside the missing tail = %d, %v", n, err)
			}
			if n, err := fl.ReadAt(make([]byte, 100), int64(c.avail)-50); n != 50 || !isCorrupt(err) {
				t.Errorf("read straddling the end of the chain = %d, %v", n, err)
			}
			if w := f.Info().Warnings; len(w) == 0 || !strings.Contains(strings.Join(w, "\n"), "chain") {
				t.Errorf("no live warning about the chain: %q", w)
			}
			// A file with a longer chain than its size is cut at the size.
			le, _ := f.Lookup("/LONG.BIN")
			lfl, err := f.Open(le)
			if err != nil || lfl.Size() != 2*512+10 {
				t.Fatalf("LONG.BIN: %v", err)
			}
			if err := filesys.CheckRuns(lfl.Runs(), lfl.Size(), f.Info().Size); err != nil {
				t.Errorf("LONG.BIN runs: %v", err)
			}
		})
	}
	t.Run("size without a first cluster", func(t *testing.T) {
		img := slices.Clone(base)
		root := rootRegion(g)
		e := entryOffsets(img, root, root+32*512, func(e []byte) bool { return bytes.HasPrefix(e, []byte("SHORT   BIN")) })[0]
		put16(img, e+26, 0)
		f := openImg(t, img)
		en, _ := f.Lookup("/SHORT.BIN")
		fl, err := f.Open(en)
		if err != nil || len(fl.Runs()) != 0 {
			t.Fatalf("Open = %v, runs %v", err, fl.Runs())
		}
		if n, err := fl.ReadAt(make([]byte, 8), 0); n != 0 || !isCorrupt(err) {
			t.Errorf("ReadAt = %d, %v", n, err)
		}
	})
	t.Run("chain ends badly after the last needed cluster", func(t *testing.T) {
		img := slices.Clone(base)
		setFAT(img, g, 0, c0+7, 0xFFF0) // LONG.BIN's third (last needed) cluster points outside the volume: all the data is there
		f := openImg(t, img)
		le, _ := f.Lookup("/LONG.BIN")
		lfl, err := f.Open(le)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, lfl.Size())
		if n, err := lfl.ReadAt(b, 0); n != len(b) || err != nil && err != io.EOF {
			t.Errorf("complete file reported short: %d, %v", n, err)
		}
	})
}

func TestTimesLocalNoZone(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			created := time.Date(2020, 3, 4, 5, 6, 7, 370_000_000, time.UTC)
			modified := time.Date(2021, 12, 31, 23, 59, 59, 0, time.UTC) // odd second: stored with 2 s precision
			accessed := time.Date(2022, 2, 2, 12, 30, 0, 0, time.UTC)    // only the date is stored
			img := build(fattest.Options{Type: typ},
				fattest.File{Path: "T.TXT", Data: []byte("t"), Times: [3]time.Time{created, modified, accessed}},
				fattest.File{Path: "NOTIME.TXT", Data: []byte("t")})
			f := openImg(t, img)
			es := readDir(t, f, "/")
			tm := es[0].Times
			if got := tm.Created; !got.T.Equal(created) || got.ZoneKnown {
				t.Errorf("Created = %v zoneKnown=%v, want %v (10 ms precision, no zone)", got.T, got.ZoneKnown, created)
			}
			if got := tm.Modified; !got.T.Equal(modified.Add(-time.Second)) || got.ZoneKnown {
				t.Errorf("Modified = %v zoneKnown=%v, want %v (2 s precision, no zone)", got.T, got.ZoneKnown, modified.Add(-time.Second))
			}
			if got := tm.Accessed; !got.T.Equal(time.Date(2022, 2, 2, 0, 0, 0, 0, time.UTC)) || got.ZoneKnown {
				t.Errorf("Accessed = %v zoneKnown=%v, want the date at 00:00", got.T, got.ZoneKnown)
			}
			if !tm.Changed.T.IsZero() || !tm.Deleted.T.IsZero() {
				t.Errorf("Changed/Deleted = %v / %v, FAT has neither", tm.Changed.T, tm.Deleted.T)
			}
			if z := es[1].Times; !z.Created.T.IsZero() || !z.Modified.T.IsZero() || !z.Accessed.T.IsZero() {
				t.Errorf("zero fields must be absent, got %+v", z)
			}
		})
	}
	t.Run("invalid fields are absent", func(t *testing.T) {
		o := fattest.Options{Type: 16}
		g := fattest.Layout(o)
		img := build(o, fattest.File{Path: "T.TXT", Data: []byte("t"), Times: [3]time.Time{time.Now(), time.Now(), time.Now()}})
		e := rootRegion(g)
		put16(img, e+24, 13<<5|1)        // month 13
		put16(img, e+22, 25<<11)         // hour 25
		put16(img, e+16, 2<<5|31|40<<9)  // 31 February
		put16(img, e+14, 12<<11|63<<5|1) // minute 63
		tm := readDir(t, openImg(t, img), "/")[0].Times
		if !tm.Modified.T.IsZero() || !tm.Created.T.IsZero() {
			t.Errorf("impossible dates were accepted: %+v", tm)
		}
	})
}

func TestUnallocatedFreeClusters(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			o := fattest.Options{Type: typ, TotalSectors: map[int]uint32{12: 2048, 16: 8192, 32: 4096}[typ]}
			g := fattest.Layout(o)
			img := build(o,
				fattest.File{Path: "A.BIN", Data: make([]byte, 3*512)},
				fattest.File{Path: "DEL.BIN", Data: make([]byte, 4*512), Deleted: true},
				fattest.File{Path: "FRAG.BIN", Data: make([]byte, 6*512), Fragmented: true},
				fattest.File{Path: "D", Dir: true},
				fattest.File{Path: "D/B.BIN", Data: make([]byte, 512)},
			)
			f := openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			cs := int64(512)
			// Independent expectation: free clusters from the FAT as read entry by entry.
			wantFree := map[uint32]bool{}
			for c := uint32(2); c <= f.ClusterCount()+1; c++ {
				v, err := f.Entry(c)
				if err != nil {
					t.Fatal(err)
				}
				if v == 0 {
					wantFree[c] = true
				}
			}
			// ... and from the layout: root (FAT32 only) + A 3 + FRAG 6 + D 1 + B 1 are allocated.
			used := 3 + 6 + 1 + 1
			if typ == 32 {
				used++
			}
			if len(wantFree) != int(f.ClusterCount())-used {
				t.Fatalf("setup: %d free clusters, want %d", len(wantFree), int(f.ClusterCount())-used)
			}
			gotFree := map[uint32]bool{}
			var prevEnd int64 = -1
			for _, r := range runs {
				if r.Offset < 0 || r.Length <= 0 || r.Offset%cs != 0 || r.Length%cs != 0 {
					t.Fatalf("bad run %+v", r)
				}
				if r.Offset <= prevEnd {
					t.Fatalf("runs are not sorted and merged: %v", runs)
				}
				prevEnd = r.Offset + r.Length
				for off := r.Offset; off < prevEnd; off += cs {
					c := uint32((off-g.ClusterOffset(2))/cs) + 2
					gotFree[c] = true
				}
			}
			if prevEnd > f.Info().Size {
				t.Errorf("a run ends at %d, beyond the %d-byte volume", prevEnd, f.Info().Size)
			}
			if len(gotFree) != len(wantFree) {
				t.Fatalf("%d free clusters in %d runs, want %d", len(gotFree), len(runs), len(wantFree))
			}
			for c := range wantFree {
				if !gotFree[c] {
					t.Errorf("cluster %d is free but not reported", c)
				}
			}
			// The deleted file's clusters are free space (its chain was released).
			de := readDir(t, f, "/")
			var del filesys.Entry
			for _, e := range de {
				if e.Deleted {
					del = e
				}
			}
			dc, _ := strconv.Atoi(attr(del, "first_cluster"))
			if !gotFree[uint32(dc)] {
				t.Errorf("the deleted file's first cluster %d is not reported free", dc)
			}
		})
	}
}

// Unallocated must not scan clusters the image does not hold.
func TestUnallocatedClampedToImage(t *testing.T) {
	o := fattest.Options{Type: 16}
	img := build(o)
	short := img[:len(img)/2]
	f, err := fat.Open(bytes.NewReader(short), int64(len(short)))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Offset+r.Length > int64(len(short)) {
			t.Fatalf("run %+v lies beyond the %d-byte image", r, len(short))
		}
	}
	if len(runs) == 0 {
		t.Error("no free space reported in the readable half")
	}
}

// ---- hostile input ------------------------------------------------------

type countFS struct {
	*fat.FS
	mu    sync.Mutex
	calls int
	ids   map[string]int
}

func (c *countFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	c.mu.Lock()
	c.calls++
	if c.ids == nil {
		c.ids = map[string]int{}
	}
	c.ids[d.ID]++
	c.mu.Unlock()
	return c.FS.ReadDir(d)
}

type rawEnt []byte

func short11(name string) []byte {
	b := []byte(strings.Repeat(" ", 11))
	copy(b, name)
	return b
}

func mkShort(name string, attr byte, cluster uint32, size uint32) rawEnt {
	e := make([]byte, 32)
	copy(e, short11(name))
	e[11] = attr
	binary.LittleEndian.PutUint16(e[20:], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(cluster))
	binary.LittleEndian.PutUint32(e[28:], size)
	return e
}

func lfnSum(name11 []byte) byte {
	var s byte
	for _, c := range name11 {
		s = (s&1)<<7 + s>>1 + c
	}
	return s
}

// mkLFN encodes one long-name entry holding units (padded with 0 then 0xFFFF).
func mkLFN(ord byte, sum byte, units []uint16) rawEnt {
	e := make([]byte, 32)
	e[0], e[11], e[13] = ord, 0x0F, sum
	pos := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
	for i, p := range pos {
		u := uint16(0xFFFF)
		switch {
		case i < len(units):
			u = units[i]
		case i == len(units):
			u = 0
		}
		binary.LittleEndian.PutUint16(e[p:], u)
	}
	return e
}

// lfnSet returns the entries of a full long-name set for name, highest ordinal
// first, followed by nothing (the caller appends the short entry).
func lfnSet(name string, sum byte) []rawEnt {
	u := utf16.Encode([]rune(name))
	n := (len(u) + 12) / 13
	var out []rawEnt
	for ord := n; ord >= 1; ord-- {
		chunk := u[(ord-1)*13 : min(len(u), ord*13)]
		o := byte(ord)
		if ord == n {
			o |= 0x40
		}
		out = append(out, mkLFN(o, sum, chunk))
	}
	return out
}

// setRoot writes the entries to the start of the FAT16 root directory of img
// (and zeroes the rest of the first 64 entries, so the listing ends after them).
func setRoot(img []byte, g fattest.Geometry, ents ...rawEnt) {
	root := rootRegion(g)
	clear(img[root : root+64*32])
	for i, e := range ents {
		copy(img[root+i*32:], e)
	}
}

func TestDirHostile(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)

	t.Run("directory chain cycle", func(t *testing.T) {
		// BIG holds "." + ".." + 30 files = exactly two 512-byte clusters with no
		// end marker, so the listing runs off the end of the chain.
		files := []fattest.File{{Path: "BIG", Dir: true}}
		for i := range 30 {
			files = append(files, fattest.File{Path: fmt.Sprintf("BIG/F%02d.TXT", i), Data: []byte{1}})
		}
		img := build(o, files...)
		f := openImg(t, img)
		big, err := f.Lookup("/BIG")
		if err != nil {
			t.Fatal(err)
		}
		chain, err := f.Chain(mustCluster(t, big))
		if err != nil || len(chain) != 2 {
			t.Fatalf("setup: chain %v, %v", chain, err)
		}
		setFAT(img, g, 0, chain[1], chain[0])
		f = openImg(t, img)
		es, err := f.ReadDir(big)
		if err != nil {
			t.Fatalf("ReadDir: %v (the entries before the loop must still be listed)", err)
		}
		if len(es) != 30 {
			t.Errorf("listed %d entries, want the 30 of the two clusters read once", len(es))
		}
		if w := strings.Join(f.Info().Warnings, "\n"); !strings.Contains(w, "loops") {
			t.Errorf("no warning about the loop: %q", w)
		}
		// Walk terminates too.
		n := 0
		_ = filesys.Walk(f, f.Root(), "/", func(string, filesys.Entry, error) error { n++; return nil })
		if n != 31 {
			t.Errorf("Walk visited %d entries, want 31", n)
		}
	})

	t.Run("cross-linked directories are read once", func(t *testing.T) {
		const n = 6
		files := []fattest.File{}
		for i := range n {
			files = append(files, fattest.File{Path: fmt.Sprintf("D%d", i), Dir: true})
		}
		files = append(files, fattest.File{Path: "D0/IN.TXT", Data: []byte("in")})
		img := build(o, files...)
		root := rootRegion(g)
		ents := entryOffsets(img, root, root+64*32, func(e []byte) bool { return e[11]&0x10 != 0 && e[0] == 'D' })
		if len(ents) != n {
			t.Fatalf("setup: %d dir entries", len(ents))
		}
		target := binary.LittleEndian.Uint16(img[ents[0]+26:])
		for _, en := range ents[1:] {
			put16(img, en+26, target)
		}
		cf := &countFS{FS: openImg(t, img)}
		var cycles, files2 int
		err := filesys.Walk(cf, cf.Root(), "/", func(p string, e filesys.Entry, err error) error {
			switch {
			case err != nil:
				if !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "dir:") {
					t.Errorf("%s: %v", p, err)
				}
				cycles++
			case e.Type == filesys.TypeFile:
				files2++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if cycles != n-1 || files2 != 1 {
			t.Errorf("cycles = %d, files = %d; want %d and 1", cycles, files2, n-1)
		}
		if cf.calls > 2 { // distinct directory clusters reachable: the root and D0's
			t.Errorf("ReadDir called %d times for 2 distinct directories", cf.calls)
		}
		for id, c := range cf.ids {
			if c > 1 {
				t.Errorf("directory %q read %d times", id, c)
			}
		}
	})

	t.Run("directory pointing at the root", func(t *testing.T) {
		for _, typ := range []int{16, 32} {
			ot := fattest.Options{Type: typ}
			gt := fattest.Layout(ot)
			img := build(ot, fattest.File{Path: "UP", Dir: true}, fattest.File{Path: "UP/X.TXT", Data: []byte("x")})
			root := rootRegion(gt)
			e := entryOffsets(img, root, root+16*32, func(e []byte) bool { return bytes.HasPrefix(e, []byte("UP      ")) })[0]
			rc := uint16(0)
			if typ == 32 {
				rc = 2
			}
			put16(img, e+26, rc) // UP now points at the root directory
			f := openImg(t, img)
			var errs int
			err := filesys.Walk(f, f.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
				if err != nil {
					if !errors.Is(err, filesys.ErrCorrupt) {
						t.Errorf("fat%d: %v", typ, err)
					}
					errs++
				}
				return nil
			})
			if err != nil || errs != 1 {
				t.Errorf("fat%d: Walk = %v with %d cycle reports, want 1", typ, err, errs)
			}
		}
	})

	t.Run("LFN sequences", func(t *testing.T) {
		good := short11("GOOD    TXT")
		sum := lfnSum(good)
		cjk := strings.Repeat("日", 255)

		type tc struct {
			name  string
			ents  []rawEnt
			want  string // name of the first listed entry
			flag  string // expected lfn attr
			count int
		}
		var bigBad []rawEnt
		for ord := byte(25); ord >= 1; ord-- { // 25 entries: more than the 20 allowed
			o := ord
			if ord == 25 {
				o |= 0x40
			}
			bigBad = append(bigBad, mkLFN(o, sum, []uint16{'a'}))
		}
		cases := []tc{
			{"20 entries, 255 characters", append(lfnSet(cjk, sum), mkShort("GOOD    TXT", 0x20, 0, 0)), cjk, "", 1},
			{"25 entries", append(bigBad, mkShort("GOOD    TXT", 0x20, 0, 0)), "GOOD.TXT", "orphan", 1},
			{"ordinal 0 (0x40)", []rawEnt{mkLFN(0x40, sum, []uint16{'a'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"ordinal 0x20 stray bit", []rawEnt{mkLFN(0x41|0x20, sum, []uint16{'a'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"sequence without the last-entry flag", []rawEnt{mkLFN(0x01, sum, []uint16{'a'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"missing ordinal", []rawEnt{mkLFN(0x43, sum, []uint16{'a'}), mkLFN(0x01, sum, []uint16{'b'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"checksum changes mid-sequence", []rawEnt{mkLFN(0x42, sum, []uint16{'a'}), mkLFN(0x01, sum+1, []uint16{'b'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"extra entry after ordinal 1", []rawEnt{mkLFN(0x41, sum, []uint16{'a'}), mkLFN(0x01, sum, []uint16{'b'}), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "orphan", 1},
			{"dangling set before the end", []rawEnt{mkLFN(0x41, sum, []uint16{'a'})}, "", "", 0},
			{"set before a volume label", []rawEnt{mkLFN(0x41, sum, []uint16{'a'}), mkShort("LABEL      ", 0x08, 0, 0), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "", 1},
			{"set before a dot entry", []rawEnt{mkLFN(0x41, sum, []uint16{'a'}), mkShort(".          ", 0x10, 2, 0), mkShort("GOOD    TXT", 0x20, 0, 0)}, "GOOD.TXT", "", 1},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				img := build(o)
				setRoot(img, g, c.ents...)
				f := openImg(t, img)
				es := readDir(t, f, "/")
				if len(es) != c.count {
					t.Fatalf("%d entries %q, want %d", len(es), names(es), c.count)
				}
				if c.count == 0 {
					return
				}
				if es[0].Name != c.want || attr(es[0], "lfn") != c.flag {
					t.Errorf("name %q lfn=%q, want %q lfn=%q", es[0].Name, attr(es[0], "lfn"), c.want, c.flag)
				}
			})
		}
	})

	t.Run("invalid long names are shown raw", func(t *testing.T) {
		sum := lfnSum(short11("RAW     TXT"))
		lone := []uint16{'a', 0xD800, 'b'}     // unpaired high surrogate
		lowFirst := []uint16{0xDC00, 'a', 'b'} // unpaired low surrogate
		for _, tc := range []struct {
			name  string
			units []uint16
		}{{"lone high surrogate", lone}, {"lone low surrogate", lowFirst}, {"slash", utf16.Encode([]rune("a/b"))}, {"dotdot", []uint16{'.', '.'}}} {
			t.Run(tc.name, func(t *testing.T) {
				img := build(o)
				setRoot(img, g, mkLFN(0x41, sum, tc.units), mkShort("RAW     TXT", 0x20, 0, 0))
				f := openImg(t, img)
				es := readDir(t, f, "/")
				if len(es) != 1 {
					t.Fatalf("%d entries", len(es))
				}
				raw := make([]byte, 2*len(tc.units))
				for i, u := range tc.units {
					binary.LittleEndian.PutUint16(raw[2*i:], u)
				}
				want := "~raw~" + base64.RawURLEncoding.EncodeToString(raw)
				if es[0].Name != want || !bytes.Equal(es[0].RawName, raw) {
					t.Errorf("Name %q RawName %x, want %q / %x", es[0].Name, es[0].RawName, want, raw)
				}
				if strings.Contains(es[0].Name, "/") {
					t.Error("a name with a slash escaped the raw form")
				}
				// The display form finds the entry again.
				if e, err := f.Lookup("/" + want); err != nil || e.ID != es[0].ID {
					t.Errorf("Lookup(display form) = %v, %v", e.ID, err)
				}
			})
		}
	})

	t.Run("random damage never panics", func(t *testing.T) {
		files := []fattest.File{
			{Path: "A Long File Name.txt", Data: make([]byte, 700), LongName: true},
			{Path: "SUB", Dir: true},
			{Path: "SUB/Inner Long Name.txt", Data: make([]byte, 3*512), LongName: true, Fragmented: true},
			{Path: "SUB/DEL.TXT", Data: []byte("d"), Deleted: true},
			{Path: "Deleted Long.txt", Data: []byte("d"), LongName: true, Deleted: true},
		}
		for _, typ := range allTypes {
			ot := fattest.Options{Type: typ, TotalSectors: 4096}
			gt := fattest.Layout(ot)
			base := build(ot, files...)
			rng := rand.New(rand.NewPCG(11, uint64(typ))) //nolint:gosec // deterministic test input, not security
			lo := rootRegion(gt)
			span := 6 * 32
			if typ == 32 {
				span = 4 * 32
			}
			for i := range 1500 {
				img := slices.Clone(base)
				for range 1 + rng.IntN(5) {
					pos := lo + rng.IntN(span+512)
					if rng.IntN(3) == 0 {
						pos = int(gt.ClusterOffset(3)) + rng.IntN(5*32) // subdirectory area
					}
					img[pos] = byte(rng.IntN(256))
				}
				f, err := fat.Open(bytes.NewReader(img), int64(len(img)))
				if err != nil {
					continue
				}
				walked := 0
				_ = filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, err error) error {
					walked++
					if walked > 10000 {
						t.Fatalf("fat%d iteration %d: walk does not terminate", typ, i)
					}
					if err == nil && e.Type == filesys.TypeFile && !e.Deleted {
						if fl, err := f.Open(e); err == nil {
							buf := make([]byte, min(fl.Size(), 4096))
							_, _ = fl.ReadAt(buf, 0)
							_ = fl.Runs()
						}
					}
					return nil
				})
				_, _ = f.Unallocated()
				_, _ = f.Lookup("/SUB/INNER LONG NAME.TXT")
			}
		}
	})

	t.Run("directory cap", func(t *testing.T) {
		og := fattest.Options{Type: 32, TotalSectors: 12000}
		gg := fattest.Layout(og)
		img := build(og)
		const clusters = 4200 // 16 entries each: more than 65536 entries
		for c := uint32(2); c < 2+clusters; c++ {
			next := c + 1
			if c == 1+clusters {
				next = 0x0FFFFFFF
			}
			for n := range gg.NumFATs {
				setFAT(img, gg, n, c, next)
			}
		}
		for i := range clusters * 16 {
			e := mkShort("FILLER  BIN", 0x20, 0, 0)
			copy(img[int(gg.ClusterOffset(2))+i*32:], e)
		}
		f := openImg(t, img)
		es, err := f.ReadDir(f.Root())
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 65536 {
			t.Errorf("listed %d entries, want the 65536 cap", len(es))
		}
		if w := strings.Join(f.Info().Warnings, "\n"); !strings.Contains(w, "65536") {
			t.Errorf("no warning about the cap: %q", w)
		}
	})
}

func mustCluster(t *testing.T, e filesys.Entry) uint32 {
	t.Helper()
	c, err := strconv.ParseUint(attr(e, "first_cluster"), 10, 32)
	if err != nil {
		t.Fatalf("first_cluster %q: %v", attr(e, "first_cluster"), err)
	}
	return uint32(c)
}

func TestReadDirAndOpenKindErrors(t *testing.T) {
	f := openImg(t, build(fattest.Options{Type: 16},
		fattest.File{Path: "F.TXT", Data: []byte("x")}, fattest.File{Path: "D", Dir: true}))
	file, _ := f.Lookup("/F.TXT")
	dir, _ := f.Lookup("/D")
	if _, err := f.ReadDir(file); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(file) = %v", err)
	}
	if _, err := f.Open(dir); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(dir) = %v", err)
	}
	if _, err := f.Open(f.Root()); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(root) = %v", err)
	}
	for _, id := range []string{"", "x", "dir:", "dir:-1", "dir:99999999999", "dirent:1", "dirent:a:b", "dirent:0:-1", "dirent:0:99999999"} {
		if _, err := f.ReadDir(filesys.Entry{ID: id}); err == nil {
			t.Errorf("ReadDir(%q) succeeded", id)
		}
		if _, err := f.Open(filesys.Entry{ID: id}); err == nil {
			t.Errorf("Open(%q) succeeded", id)
		}
	}
	// A FAT32 directory cluster of 0 is not a directory.
	f32 := openImg(t, build(fattest.Options{Type: 32}))
	if _, err := f32.ReadDir(filesys.Entry{ID: "dir:0"}); !isCorrupt(err) {
		t.Errorf("ReadDir(dir:0) on FAT32 = %v", err)
	}
	// An out-of-range directory cluster fails instead of listing garbage.
	if _, err := f.ReadDir(filesys.Entry{ID: "dir:60000"}); !isCorrupt(err) {
		t.Errorf("ReadDir(dir:60000) = %v", err)
	}
	// Open re-reads the directory entry, so a file entry whose slot became a deleted one is refused.
	g := fattest.Layout(fattest.Options{Type: 16})
	img := build(fattest.Options{Type: 16}, fattest.File{Path: "F.TXT", Data: []byte("x")})
	f = openImg(t, img)
	e, _ := f.Lookup("/F.TXT")
	img[rootRegion(g)] = 0xE5
	f = openImg(t, img)
	if _, err := f.Open(e); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(stale entry) = %v, want ErrDeleted", err)
	}
}

func TestInfoWarningsAccumulateAcrossReads(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := build(o,
		fattest.File{Path: "SHORT.BIN", Data: make([]byte, 4*512)},
		fattest.File{Path: "BIG", Dir: true})
	setFAT(img, g, 0, 3, 0xFFFF)
	f := openImg(t, img)
	before := len(f.Info().Warnings)
	e, _ := f.Lookup("/SHORT.BIN")
	if _, err := f.Open(e); err != nil {
		t.Fatal(err)
	}
	after := f.Info().Warnings
	if len(after) != before+1 {
		t.Fatalf("warnings %d -> %d, want one more after Open", before, len(after))
	}
	for range 3 {
		_, _ = f.Open(e)
	}
	if len(f.Info().Warnings) != len(after) {
		t.Error("the same warning is recorded repeatedly")
	}
	if strings.Contains(strings.Join(after, "\n"), "SHORT") {
		t.Errorf("warning carries a name: %q", after)
	}
}

func TestDeletedEntryIDsAreUnique(t *testing.T) {
	f := openImg(t, build(fattest.Options{Type: 16},
		fattest.File{Path: "Same Long Name.txt", Data: []byte("1"), LongName: true, Deleted: true},
		fattest.File{Path: "Same Long Name.txt", Data: []byte("2"), LongName: true, Deleted: true},
		fattest.File{Path: "Same Long Name.txt", Data: []byte("3"), LongName: true}))
	es := readDir(t, f, "/")
	if len(es) != 3 {
		t.Fatalf("%d entries", len(es))
	}
	ids := map[string]bool{}
	for _, e := range es {
		if e.Name != "Same Long Name.txt" || ids[e.ID] {
			t.Errorf("entry %q ID %q (duplicate=%v)", e.Name, e.ID, ids[e.ID])
		}
		ids[e.ID] = true
	}
	// Lookup finds only the live one; the deleted ones are reachable by ID alone.
	live, err := f.Lookup("/same long name.TXT")
	if err != nil || live.Deleted || string(readAll(t, f, live)) != "3" {
		t.Errorf("Lookup = %+v, %v", live, err)
	}
	for _, e := range es[:2] {
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrDeleted) {
			t.Errorf("Open(%s) = %v", e.ID, err)
		}
	}
}

// Open and ReadDir re-derive everything from the volume: forged Entry fields
// and attributes change nothing.
func TestForgedEntryFieldsAreIgnored(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			data := []byte("real file content")
			img := build(fattest.Options{Type: typ},
				fattest.File{Path: "F.TXT", Data: data},
				fattest.File{Path: "OTHER.TXT", Data: []byte("other")},
				fattest.File{Path: "GONE.TXT", Data: []byte("deleted"), Deleted: true},
				fattest.File{Path: "D", Dir: true},
				fattest.File{Path: "D/IN.TXT", Data: []byte("in")},
				fattest.File{Path: "E", Dir: true})
			f := openImg(t, img)
			es := readDir(t, f, "/")
			byN := func(n string) filesys.Entry { e, _ := byName(es, n); return e }
			file, gone, dir, other := byN("F.TXT"), byN("_ONE.TXT"), byN("D"), byN("OTHER.TXT")

			forge := func(e filesys.Entry, edit func(*filesys.Entry)) filesys.Entry {
				c := e
				c.Attrs = slices.Clone(e.Attrs)
				edit(&c)
				return c
			}
			setAttr := func(e *filesys.Entry, k, v string) {
				for i := range e.Attrs {
					if e.Attrs[i].Key == k {
						e.Attrs[i].Value = v
					}
				}
			}
			// A forged size, first cluster and type do not change what Open returns.
			bad := forge(file, func(e *filesys.Entry) {
				e.Size, e.Type, e.Mode = 1<<30, filesys.TypeDir, 0
				setAttr(e, "first_cluster", attr(other, "first_cluster"))
				setAttr(e, "size", "5")
			})
			if got := readAll(t, f, bad); !bytes.Equal(got, data) {
				t.Errorf("forged file entry changed the content: %q", got)
			}
			// Deleted=false on a deleted entry does not make it openable.
			undel := forge(gone, func(e *filesys.Entry) { e.Deleted = false; e.Type = filesys.TypeFile })
			if _, err := f.Open(undel); !errors.Is(err, filesys.ErrDeleted) {
				t.Errorf("Open(undeleted forgery) = %v, want ErrDeleted", err)
			}
			// Deleted=true on a live entry does not hide it either.
			redel := forge(file, func(e *filesys.Entry) { e.Deleted = true })
			if got := readAll(t, f, redel); !bytes.Equal(got, data) {
				t.Error("Deleted=true forgery changed the result")
			}
			// A directory is a directory whatever the entry says.
			if _, err := f.Open(forge(dir, func(e *filesys.Entry) { e.Type = filesys.TypeFile })); !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("Open(dir forged as file) = %v", err)
			}
			inner, err := f.ReadDir(forge(dir, func(e *filesys.Entry) {
				e.Type, e.Deleted = filesys.TypeFile, false
				setAttr(e, "first_cluster", "9999")
				setAttr(e, "dirent", "0:77777")
			}))
			if err != nil || len(inner) != 1 || inner[0].Name != "IN.TXT" {
				t.Errorf("ReadDir(dir with forged attrs) = %v, %v", names(inner), err)
			}
			// A fresh volume has met no directory: a bad hint is not trusted, the tree is searched.
			f2 := openImg(t, img)
			inner, err = f2.ReadDir(forge(dir, func(e *filesys.Entry) { setAttr(e, "dirent", "0:77777") }))
			if err != nil || len(inner) != 1 {
				t.Errorf("ReadDir with a forged hint on a fresh volume = %v, %v", names(inner), err)
			}
			f3 := openImg(t, img)
			if inner, err = f3.ReadDir(filesys.Entry{ID: dir.ID}); err != nil || len(inner) != 1 {
				t.Errorf("ReadDir without any attrs on a fresh volume = %v, %v", names(inner), err)
			}
			// A "directory" ID that is not a live directory of the volume (a file's cluster) fails.
			f4 := openImg(t, img)
			if _, err := f4.ReadDir(filesys.Entry{ID: "dir:" + attr(file, "first_cluster"), Type: filesys.TypeDir}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("ReadDir(dir:<file cluster>) = %v, want ErrNotFound", err)
			}
			// ... and so does a file ID in a directory that is not one.
			f5 := openImg(t, img)
			if _, err := f5.Open(filesys.Entry{ID: "dirent:" + attr(file, "first_cluster") + ":0"}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("Open(dirent:<file cluster>:0) = %v, want ErrNotFound", err)
			}
			// ReadDir accepts the dirent ID of a live directory entry; a deleted one is ErrDeleted.
			f6 := openImg(t, img)
			_, idx, _ := strings.Cut(attr(dir, "dirent"), ":")
			if inner, err := f6.ReadDir(filesys.Entry{ID: "dirent:" + strings.TrimSuffix(attr(dir, "dirent"), ":"+idx) + ":" + idx}); err != nil || len(inner) != 1 {
				t.Errorf("ReadDir(dirent id of a live dir) = %v, %v", names(inner), err)
			}
			if _, err := f6.ReadDir(gone); !errors.Is(err, filesys.ErrDeleted) {
				t.Errorf("ReadDir(deleted) = %v", err)
			}
		})
	}
}
