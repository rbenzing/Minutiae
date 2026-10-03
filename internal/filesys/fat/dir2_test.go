package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

func attrValues(e filesys.Entry, key string) []string {
	var out []string
	for _, kv := range e.Attrs {
		if kv.Key == key {
			out = append(out, kv.Value)
		}
	}
	return out
}

func deletedSet(name string, sum byte) []rawEnt {
	set := lfnSet(name, sum)
	for _, e := range set {
		e[0] = 0xE5
	}
	return set
}

func hasWarning(f *fat.FS, sub string) bool {
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestDirectoryReadBudget(t *testing.T) {
	t.Run("overlapping chains", func(t *testing.T) {
		o := fattest.Options{Type: 16}
		g := fattest.Layout(o)
		const c = 2000 // clusters in the long chain: 16 entries each (32000 entries; unbudgeted the walk would read about 32 million)
		img := build(o)
		for n := range g.NumFATs {
			for k := uint32(2); k < 2+c; k++ {
				next := k + 1
				if k == 1+c {
					next = 0xFFFF
				}
				setFAT(img, g, n, k, next)
			}
		}
		// Entry i is a subdirectory that starts at cluster 2+i%c: each one is a
		// distinct directory overlapping the same long chain.
		base := int(g.ClusterOffset(2))
		for i := range c * 16 {
			copy(img[base+i*32:], mkShort("SUB     ", 0x10, uint32(2+i%c), 0))
		}
		setRoot(img, g, mkShort("TOP     ", 0x10, 2, 0))
		f := openImg(t, img)
		f.SetDirBudget(1<<30, 300_000) // far fewer entries than the walk would otherwise read
		start := time.Now()
		visited := 0
		err := filesys.Walk(f, f.Root(), "/", func(string, filesys.Entry, error) error { visited++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !hasWarning(f, "budget") {
			t.Errorf("no budget warning after %d visited entries in %v: %q", visited, time.Since(start), f.Info().Warnings)
		}
		if d := time.Since(start); d > 30*time.Second {
			t.Errorf("walk took %v", d)
		}
	})
	t.Run("hook", func(t *testing.T) {
		var files []fattest.File
		for i := range 200 {
			files = append(files, fattest.File{Path: fmt.Sprintf("F%03d.TXT", i), Data: []byte{1}})
		}
		f := openImg(t, build(fattest.Options{Type: 16}, files...))
		if full := readDir(t, f, "/"); len(full) != 200 || hasWarning(f, "budget") {
			t.Fatalf("%d entries, warnings %q", len(full), f.Info().Warnings)
		}
		f.SetDirBudget(1000, 1<<20) // one 512-byte root sector
		part := readDir(t, f, "/")
		if len(part) == 0 || len(part) >= 200 || !hasWarning(f, "budget") {
			t.Errorf("%d entries, warnings %q", len(part), f.Info().Warnings)
		}
		if again := readDir(t, f, "/"); len(again) != 0 {
			t.Errorf("after exhaustion %d entries are still listed", len(again))
		}
		f.SetDirBudget(1<<30, 20) // the entry budget alone
		if part := readDir(t, f, "/"); len(part) >= 200 {
			t.Errorf("entry budget ignored: %d entries", len(part))
		}
	})
}

func TestDeletedLongNameRecoveryMarkers(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	t.Run("recovered", func(t *testing.T) {
		img := build(o, fattest.File{Path: "Secret Plan.doc", Data: []byte("x"), LongName: true, Deleted: true})
		es := readDir(t, openImg(t, img), "/")
		if es[0].Name != "Secret Plan.doc" || !slices.Equal(attrValues(es[0], "lfn"), []string{"recovered"}) {
			t.Errorf("name %q lfn %q, want recovered and no unterminated flag", es[0].Name, attrValues(es[0], "lfn"))
		}
	})
	t.Run("slot reuse truncates the name", func(t *testing.T) {
		img := build(o, fattest.File{Path: "Secret Plan Document.doc", Data: []byte("x"), LongName: true, Deleted: true})
		root := rootRegion(g)
		lfns := entryOffsets(img, root, root+32*512, func(e []byte) bool { return e[0] == 0xE5 && e[11] == 0x0F })
		if len(lfns) != 2 {
			t.Fatalf("setup: %d deleted LFN entries", len(lfns))
		}
		img[lfns[0]+13]++ // the farthest slot (highest ordinal) now belongs to something else
		es := readDir(t, openImg(t, img), "/")
		if es[0].Name != "Secret Plan D" || !slices.Equal(attrValues(es[0], "lfn"), []string{"recovered", "unterminated"}) {
			t.Errorf("name %q lfn %q, want the first 13 characters flagged recovered and unterminated", es[0].Name, attrValues(es[0], "lfn"))
		}
	})
	t.Run("a neighbouring long name is not attributed", func(t *testing.T) {
		img := build(o)
		sumB := lfnSum(short11("BETALO~1TXT"))
		ents := deletedSet("Beta Long Name.txt", sumB)
		short := mkShort("ALPHA   TXT", 0x20, 5, 3)
		short[0] = 0xE5 // a deleted file with no long-name entries of its own
		setRoot(img, g, append(ents, short)...)
		es := readDir(t, openImg(t, img), "/")
		if len(es) != 1 || es[0].Name != "_LPHA.TXT" || hasAttr(es[0], "lfn") {
			t.Errorf("entries %+v, want the _LPHA.TXT fallback without an lfn flag", es)
		}
	})
	t.Run("a name that starts with a non-alphanumeric", func(t *testing.T) {
		name := "日本語.txt"
		for _, c := range []struct {
			orig   string
			wantOK bool
		}{{"~AB~1   TXT", true}, {"_AB~1   TXT", true}, {"XAB~1   TXT", false}, {"%AB~1   TXT", true}, {"1AB~1   TXT", false}} {
			img := build(o)
			sum := lfnSum([]byte(c.orig))
			short := mkShort(c.orig, 0x20, 5, 3)
			short[0] = 0xE5
			setRoot(img, g, append(deletedSet(name, sum), short)...)
			es := readDir(t, openImg(t, img), "/")
			want := "_" + c.orig[1:3] + "~1.TXT"
			if c.wantOK {
				want = name
			}
			if es[0].Name != want {
				t.Errorf("original %q: name %q, want %q", c.orig, es[0].Name, want)
			}
		}
	})
}

func TestLiveLFNInterruptedByDeletedEntry(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	sum := lfnSum(short11("GOOD    TXT"))
	good := mkShort("GOOD    TXT", 0x20, 0, 0)
	del := mkLFN(0xE5, 0x11, []uint16{'z'}) // a deleted long-name entry
	for _, c := range []struct {
		name string
		ents []rawEnt
	}{
		{"partial live set", []rawEnt{mkLFN(0x42, sum, []uint16{'a'}), del, good}},
		{"complete live set", append(lfnSet("A Long Name.txt", sum), del, good)},
	} {
		t.Run(c.name, func(t *testing.T) {
			img := build(o)
			setRoot(img, g, c.ents...)
			es := readDir(t, openImg(t, img), "/")
			if len(es) != 1 || es[0].Name != "GOOD.TXT" || !slices.Equal(attrValues(es[0], "lfn"), []string{"orphan"}) {
				t.Errorf("entries %+v, want GOOD.TXT flagged lfn=orphan", es)
			}
		})
	}
	// Deleted long-name entries alone do not make the next live entry an orphan.
	img := build(o)
	setRoot(img, g, del, good)
	es := readDir(t, openImg(t, img), "/")
	if len(es) != 1 || hasAttr(es[0], "lfn") {
		t.Errorf("entries %+v, want a plain GOOD.TXT", es)
	}
}

func TestLiveEntryFirstByte05(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := build(o)
	raw := []byte("\x05BC     TXT")
	setRoot(img, g, mkShort(string(raw), 0x20, 0, 0))
	es := readDir(t, openImg(t, img), "/")
	if len(es) != 1 || es[0].Deleted {
		t.Fatalf("entries %+v: 0x05 is not a deletion mark", es)
	}
	if es[0].Name != "åBC.TXT" || !bytes.Equal(es[0].RawName, raw) {
		t.Errorf("name %q raw %q, want the 0xE5 character and the on-disk bytes", es[0].Name, es[0].RawName)
	}
	if attr(es[0], "short_name") != "åBC.TXT" {
		t.Errorf("short_name %q", attr(es[0], "short_name"))
	}
}

func TestForgedDirentIDsAreRejectedQuickly(t *testing.T) {
	for _, typ := range []int{16, 32} {
		f := openImg(t, build(fattest.Options{Type: typ}, fattest.File{Path: "D", Dir: true}))
		start := time.Now()
		ids := []string{"dirent:4294967295:0", "dirent:1:0", "dirent:99999999:3", "dirent:4294967295:65535"}
		if typ == 32 {
			ids = append(ids, "dirent:0:0")
		}
		for _, id := range ids {
			if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("fat%d: Open(%q) = %v, want ErrNotFound", typ, id, err)
			}
			if _, err := f.ReadDir(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("fat%d: ReadDir(%q) = %v, want ErrNotFound", typ, id, err)
			}
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("fat%d: forged IDs took %v: a search was started", typ, time.Since(start))
		}
		if hasWarning(f, "budget") {
			t.Errorf("fat%d: forged IDs spent the directory budget", typ)
		}
	}
}

func TestVeryFragmentedFileIsNeverRefused(t *testing.T) {
	data := make([]byte, 3000*512)
	for i := range data {
		data[i] = byte(i / 512)
	}
	img := build(fattest.Options{Type: 16}, fattest.File{Path: "FRAG.BIN", Data: data, Fragmented: true})
	f := openImg(t, img)
	e, err := f.Lookup("/FRAG.BIN")
	if err != nil {
		t.Fatal(err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := len(fl.Runs()); n < 2900 {
		t.Errorf("%d runs, want a file fragmented into about 3000", n)
	}
	if got := readAll(t, f, e); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}
}
