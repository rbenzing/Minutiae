package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// An entry at or beyond the directory's first 0x00 end marker is not part of
// the directory: its dirent ID names nothing, even if the bytes there look like
// a live entry (stale data after the end of a directory must not be opened).
func TestEntryBeyondEndMarkerIsNotFound(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			o := fattest.Options{Type: typ}
			g := fattest.Layout(o)
			img := build(o,
				fattest.File{Path: "A.TXT", Data: []byte("alpha")},
				fattest.File{Path: "D", Dir: true},
				fattest.File{Path: "D/IN.TXT", Data: []byte("inner")})
			f := openImg(t, img)
			root := rootRegion(g)
			var fileEnt, dirEnt, inEnt []byte
			for i := range 4 { // the live entries at the start of the root
				e := img[root+i*32 : root+i*32+32]
				switch {
				case bytes.HasPrefix(e, []byte("A       TXT")):
					fileEnt = slices.Clone(e)
				case bytes.HasPrefix(e, []byte("D       ")):
					dirEnt = slices.Clone(e)
				}
			}
			if fileEnt == nil || dirEnt == nil {
				t.Fatal("setup: entries not found")
			}
			d, err := f.Lookup("/D")
			if err != nil {
				t.Fatal(err)
			}
			dc := attr(d, "first_cluster")
			dbase := int(g.ClusterOffset(mustCluster(t, d)))
			for i := range 8 {
				if e := img[dbase+i*32 : dbase+i*32+32]; bytes.HasPrefix(e, []byte("IN      TXT")) {
					inEnt = slices.Clone(e)
				}
			}
			if inEnt == nil {
				t.Fatal("setup: D/IN.TXT entry not found")
			}
			// Entries 2.. of the root are free (the end marker is entry 2); copy
			// live-looking entries to indexes 5 and 6, and one into D past its end.
			copy(img[root+5*32:], fileEnt)
			copy(img[root+6*32:], dirEnt)
			copy(img[dbase+6*32:], inEnt)
			f = openImg(t, img)
			rootFirst := strings.TrimPrefix(f.Root().ID, "dir:")
			// Controls: the real entries open.
			if _, err := f.Open(filesys.Entry{ID: "dirent:" + rootFirst + ":0"}); err != nil {
				t.Fatalf("control Open(A.TXT): %v", err)
			}
			if _, err := f.Open(filesys.Entry{ID: "dirent:" + dc + ":2"}); err != nil {
				t.Fatalf("control Open(D/IN.TXT): %v", err)
			}
			for _, id := range []string{"dirent:" + rootFirst + ":5", "dirent:" + dc + ":6"} {
				if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
					t.Errorf("Open(%s) beyond the end marker = %v, want ErrNotFound", id, err)
				}
			}
			if _, err := f.ReadDir(filesys.Entry{ID: "dirent:" + rootFirst + ":6"}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("ReadDir(dirent:%s:6) beyond the end marker = %v, want ErrNotFound", rootFirst, err)
			}
			// The listing itself stops at the marker.
			if got := names(readDir(t, f, "/")); !slices.Equal(got, []string{"A.TXT", "D"}) {
				t.Errorf("root lists %v", got)
			}
		})
	}
}

func TestDifferingFATCopiesWarn(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			o := fattest.Options{Type: typ, NumFATs: 2}
			g := fattest.Layout(o)
			img := build(o, fattest.File{Path: "A.BIN", Data: make([]byte, 3*512)})
			f := openImg(t, img)
			if _, err := f.Unallocated(); err != nil {
				t.Fatal(err)
			}
			if hasWarning(f, "FAT copy") {
				t.Errorf("identical FAT copies warned: %q", f.Info().Warnings)
			}
			bad := slices.Clone(img)
			setFAT(bad, g, 1, firstCluster(typ)+20, 0x5A5) // a free cluster of the second copy is used
			f = openImg(t, bad)
			for range 2 {
				if _, err := f.Unallocated(); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			for _, w := range f.Info().Warnings {
				if strings.Contains(w, "FAT copy") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d FAT-copy warnings, want exactly one: %q", n, f.Info().Warnings)
			}
			// One FAT copy: nothing to compare.
			one := fattest.Options{Type: typ, NumFATs: 1}
			f = openImg(t, build(one))
			if _, err := f.Unallocated(); err != nil || hasWarning(f, "FAT copy") {
				t.Errorf("single FAT: %v %q", err, f.Info().Warnings)
			}
		})
	}
}

// Entry IDs are parsed strictly: canonical decimals only (no sign, no leading
// zeros, no spaces), so one directory entry has exactly one ID.
func TestEntryIDsMustBeCanonical(t *testing.T) {
	for _, typ := range allTypes {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			img := build(fattest.Options{Type: typ},
				fattest.File{Path: "F.TXT", Data: []byte("x")},
				fattest.File{Path: "D", Dir: true},
				fattest.File{Path: "D/IN.TXT", Data: []byte("in")})
			es := readDir(t, openImg(t, img), "/")
			dir, _ := byName(es, "D")
			file, _ := byName(es, "F.TXT")
			_, dirent, _ := strings.Cut(file.ID, "dirent:")
			p, i, _ := strings.Cut(dirent, ":")
			c := strings.TrimPrefix(dir.ID, "dir:")

			dirIDs := map[string]bool{"dir:" + c: true}
			direntIDs := map[string]bool{"dirent:" + p + ":" + i: true}
			for _, v := range []string{"0" + c, "00" + c, "+" + c, " " + c, c + " ", "-" + c, "0x" + c, c + "_0", c + "\n", ""} {
				dirIDs["dir:"+v] = false
			}
			for _, v := range [][2]string{{"0" + p, i}, {"+" + p, i}, {" " + p, i}, {p, "0" + i}, {p, "+" + i}, {p, " " + i}, {p, i + " "}, {p, "-0"}, {p, ""}, {"", i}, {p, "00"}} {
				direntIDs["dirent:"+v[0]+":"+v[1]] = false
			}
			for id, ok := range dirIDs {
				_, err := openImg(t, img).ReadDir(filesys.Entry{ID: id})
				if ok != (err == nil) || (!ok && !errors.Is(err, filesys.ErrNotFound)) {
					t.Errorf("ReadDir(%q) = %v, canonical = %v", id, err, ok)
				}
			}
			for id, ok := range direntIDs {
				_, err := openImg(t, img).Open(filesys.Entry{ID: id})
				if ok != (err == nil) || (!ok && !errors.Is(err, filesys.ErrNotFound)) {
					t.Errorf("Open(%q) = %v, canonical = %v", id, err, ok)
				}
			}
		})
	}
}

// Bytes of an 8.3 name at or above 0x80 are decoded as code page 437; the 11
// on-disk bytes stay in RawName.
func TestShortNameDecodesCodePage437(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	for _, c := range []struct{ raw, want string }{
		{"CAF\x82    TXT", "CAFé.TXT"},
		{"\x90T\x90     TXT", "ÉTÉ.TXT"},
		{"\x80\x9f\xa8\xe1\xe5\xfb\xff DAT", "Çƒ¿ßσ√ .DAT"},
		{"A       \x82\xe1\x9b", "A.éß¢"},
		{"\x05BC     TXT", "σBC.TXT"}, // 0x05 stands for 0xE5
	} {
		img := build(o)
		setRoot(img, g, mkShort(c.raw, 0x20, 0, 0))
		f := openImg(t, img)
		es := readDir(t, f, "/")
		if len(es) != 1 || es[0].Name != c.want {
			t.Errorf("% x: entries %q, want the name %q", c.raw, names(es), c.want)
			continue
		}
		if !bytes.Equal(es[0].RawName, []byte(c.raw)) {
			t.Errorf("% x: RawName % x, want the 11 on-disk bytes", c.raw, es[0].RawName)
		}
		if got := attr(es[0], "short_name"); got != c.want {
			t.Errorf("% x: short_name %q, want %q", c.raw, got, c.want)
		}
		if _, err := f.Lookup("/" + c.want); err != nil {
			t.Errorf("% x: Lookup(%q): %v", c.raw, c.want, err)
		}
	}
}
