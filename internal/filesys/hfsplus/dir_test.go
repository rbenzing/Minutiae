package hfsplus_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

const epoch = hfsplustest.HFSEpoch

func idOf(n uint32) string { return "cnid:" + strconv.FormatUint(uint64(n), 10) }

func attr(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

func nameList(es []filesys.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func child(t testing.TB, es []filesys.Entry, name string) filesys.Entry {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", name, nameList(es))
	return filesys.Entry{}
}

func readDir(t testing.TB, f *hfsplus.FS, dir filesys.Entry) []filesys.Entry {
	t.Helper()
	es, err := f.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir.ID, err)
	}
	return es
}

// recPos is where a catalog record lies in the image.
type recPos struct{ key, data int64 }

// findRec locates the leaf record keyed (parent, name) in a builder image.
func findRec(t testing.TB, img []byte, lay *hfsplustest.Layout, parent uint32, name []uint16) recPos {
	t.Helper()
	ns := int64(lay.NodeSize)
	for _, num := range lay.CatalogLevels[0] {
		base := lay.CatalogOffset(int64(num) * ns)
		n := img[base : base+ns]
		nrec := int(be.Uint16(n[10:]))
		for i := 0; i < nrec; i++ {
			off := int(be.Uint16(n[int(ns)-2*(i+1):]))
			kl := int(be.Uint16(n[off:]))
			if be.Uint32(n[off+2:]) != parent {
				continue
			}
			nl := int(be.Uint16(n[off+6:]))
			got := make([]uint16, nl)
			for j := range got {
				got[j] = be.Uint16(n[off+8+2*j:])
			}
			if slices.Equal(got, name) {
				return recPos{key: base + int64(off), data: base + int64(off) + int64((2+kl+1)&^1)}
			}
		}
	}
	t.Fatalf("no catalog record (%d, %v)", parent, name)
	return recPos{}
}

func fileTimes(raw ...uint32) *hfsplustest.Times {
	return &hfsplustest.Times{Create: raw[0], ContentMod: raw[1], AttrMod: raw[2], Access: raw[3], Backup: raw[4]}
}

var sampleTimes = fileTimes(epoch+1000, epoch+2000, epoch+3000, epoch+4000, epoch+5000)

func sampleTree() []hfsplustest.File {
	return []hfsplustest.File{
		{Path: "/docs", Dir: true, Mode: 0o40750, UID: 501, GID: 20, Times: sampleTimes},
		{Path: "/docs/readme.txt", Mode: 0o100640, UID: 501, GID: 20, Times: sampleTimes, FileType: "TEXT", FileCreator: "ttxt", DataLogical: 1234, RsrcLogical: 77, RsrcBlocks: 1},
		{Path: "/docs/sub", Dir: true},
		{Path: "/docs/sub/deep.bin", DataLogical: 5},
		{Path: "/link", Mode: 0o120777, DataLogical: 11},
		{Path: "/dev", Mode: 0o20644},
		{Path: "/old", ZeroMode: true, Times: &hfsplustest.Times{}},
		{Path: "/classic-link", ZeroMode: true, FileType: "slnk", FileCreator: "rhap"},
	}
}

func buildTree(t testing.TB, o hfsplustest.Options, files []hfsplustest.File) ([]byte, *hfsplustest.Layout, *hfsplus.FS) {
	t.Helper()
	if o.Label == "" {
		o.Label = "VOL"
	}
	img, lay := hfsplustest.BuildLayout(o, files)
	return img, lay, open(t, img)
}

func TestReadDirLiveEntries(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	if f.Root().ID != "cnid:2" || f.Root().Type != filesys.TypeDir {
		t.Fatalf("Root = %+v", f.Root())
	}
	root := readDir(t, f, f.Root())
	if got, want := nameList(root), []string{"classic-link", "dev", "docs", "link", "old"}; !slices.Equal(sortedCopy(got), want) {
		t.Fatalf("root names = %v, want %v", got, want)
	}
	docs := child(t, root, "docs")
	if docs.ID != idOf(lay.CNIDs["/docs"]) || docs.Type != filesys.TypeDir || docs.Mode != 0o40750 || docs.UID != 501 || docs.GID != 20 || docs.Size != 0 {
		t.Errorf("docs = %+v", docs)
	}
	wantTimes := filesys.Times{
		Created:  filesys.Timestamp{T: time.Unix(1000, 0).UTC(), ZoneKnown: true},
		Modified: filesys.Timestamp{T: time.Unix(2000, 0).UTC(), ZoneKnown: true},
		Changed:  filesys.Timestamp{T: time.Unix(3000, 0).UTC(), ZoneKnown: true},
		Accessed: filesys.Timestamp{T: time.Unix(4000, 0).UTC(), ZoneKnown: true},
	}
	if !reflect.DeepEqual(docs.Times, wantTimes) {
		t.Errorf("docs times = %+v, want %+v", docs.Times, wantTimes)
	}
	for k, want := range map[string]string{"valence": "2", "flags": "0x0000", "backup_date": "1970-01-01T01:23:20Z"} {
		if v, ok := attr(docs, k); !ok || v != want {
			t.Errorf("docs attr %s = %q (%v), want %q", k, v, ok, want)
		}
	}
	if _, ok := attr(docs, "thread"); ok {
		t.Error("a folder with a thread has no thread attr")
	}

	link := child(t, root, "link")
	if link.Type != filesys.TypeSymlink || link.Size != 11 || link.Mode != 0o120777 {
		t.Errorf("link = %+v", link)
	}
	if dev := child(t, root, "dev"); dev.Type != filesys.TypeOther || dev.Mode != 0o20644 {
		t.Errorf("dev = %+v", dev)
	}
	old := child(t, root, "old")
	if old.Type != filesys.TypeFile || old.Mode != 0 || old.UID != 0 || (old.Times != filesys.Times{}) {
		t.Errorf("old (classic, no BSD info, no dates) = %+v", old)
	}
	if cl := child(t, root, "classic-link"); cl.Type != filesys.TypeSymlink {
		t.Errorf("a mode-0 file of type 'slnk' is a symlink: %+v", cl)
	}

	subs := readDir(t, f, docs)
	if got := sortedCopy(nameList(subs)); !slices.Equal(got, []string{"readme.txt", "sub"}) {
		t.Fatalf("docs names = %v", got)
	}
	rd := child(t, subs, "readme.txt")
	if rd.Type != filesys.TypeFile || rd.Size != 1234 || rd.Mode != 0o100640 || rd.ID != idOf(lay.CNIDs["/docs/readme.txt"]) {
		t.Errorf("readme.txt = %+v", rd)
	}
	for k, want := range map[string]string{"file_type": `"TEXT"`, "file_creator": `"ttxt"`, "rsrc_size": "77", "rsrc_blocks": "1"} {
		if v, ok := attr(rd, k); !ok || v != want {
			t.Errorf("readme attr %s = %q (%v), want %q", k, v, ok, want)
		}
	}
	if _, ok := attr(rd, "valence"); ok {
		t.Error("a file has no valence")
	}
	if _, ok := attr(child(t, subs, "sub"), "rsrc_size"); ok {
		t.Error("a folder has no resource fork attrs")
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a clean volume: %v", w)
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func TestEntryIDsMustBeCanonical(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	before := f.DirBudget()
	for _, id := range []string{
		"", "cnid:", "2", "cnid", "cnid:+2", "cnid:-2", "cnid:02", "cnid:016", "cnid: 2", "cnid:2 ", "cnid:0x10", "cnid:1e1",
		"cnid:4294967296", "cnid:18446744073709551616", "cnid:99999999999999999999999999", "CNID:2", "Cnid:2", "cnid:2x", "cnid:2\n",
		"nid:2", "inode:2", "cnid:２", "cnid:2:3", "cnid::2", "id:cnid:2", strings.Repeat("9", 1<<16),
		"cnid:0", "cnid:1", "cnid:3", "cnid:4", "cnid:5", "cnid:6", "cnid:7", "cnid:8", "cnid:9", "cnid:10", "cnid:11", "cnid:12", "cnid:13", "cnid:14", "cnid:15",
	} {
		_, err := f.ReadDir(filesys.Entry{ID: id, Type: filesys.TypeDir})
		if !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(%.40q) err = %v, want ErrNotFound", id, err)
		}
	}
	if f.DirBudget() != before {
		t.Errorf("rejected ids spent %d bytes of the directory budget", before-f.DirBudget())
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "cnid:2"}); err != nil {
		t.Errorf("cnid:2: %v", err)
	}
}

func TestForgedEntryFieldsAreIgnored(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	genuine := readDir(t, f, child(t, readDir(t, f, f.Root()), "docs"))
	forged := filesys.Entry{
		ID: idOf(lay.CNIDs["/docs"]), Name: "elsewhere", Type: filesys.TypeFile, Size: 99, Mode: 0o100777, Deleted: true, Encrypted: true,
		LinkTarget: "/etc/passwd", Attrs: []filesys.KV{{Key: "valence", Value: "0"}},
	}
	got, err := f.ReadDir(forged)
	if err != nil || !reflect.DeepEqual(got, genuine) {
		t.Errorf("forged dir entry: err=%v got=%+v want=%+v", err, got, genuine)
	}
	// A file ID is not a directory whatever the entry says.
	_, err = f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/docs/readme.txt"]), Type: filesys.TypeDir})
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir of a file id: %v, want ErrUnsupported", err)
	}
	// Nor does a forged Deleted flag hide a live directory.
	if _, err := f.ReadDir(filesys.Entry{ID: "cnid:2", Deleted: true}); err != nil {
		t.Errorf("forged Deleted on the root: %v", err)
	}
}

func TestForgedIDsAreRejectedQuickly(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	before := f.DirBudget()
	for _, id := range []string{"cnid:99999", "cnid:4294967295", "cnid:16000"} {
		es, err := f.ReadDir(filesys.Entry{ID: id})
		if !errors.Is(err, filesys.ErrNotFound) || es != nil {
			t.Errorf("ReadDir(%s) = %v, %v; want ErrNotFound", id, es, err)
		}
	}
	if f.DirBudget() != before {
		t.Errorf("forged ids spent %d bytes of the directory budget", before-f.DirBudget())
	}
}

func TestEntryBuiltOutsideReadDirOpens(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	es, err := f.ReadDir(filesys.Entry{ID: "cnid:" + strconv.Itoa(int(lay.CNIDs["/docs/sub"]))})
	if err != nil || len(es) != 1 || es[0].Name != "deep.bin" {
		t.Fatalf("ReadDir of a hand-made entry = %v, %v", es, err)
	}
}

func TestRootAndThreadMismatchIsCorrupt(t *testing.T) {
	files := []hfsplustest.File{{Path: "/aaa", Dir: true}, {Path: "/bbb", Dir: true}, {Path: "/f"}}
	t.Run("root thread names another volume name", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{Label: "VOL1"}, files)
		p := findRec(t, img, lay, 2, nil)
		be.PutUint16(img[p.data+10+2*3:], '2') // "VOL1" -> "VOL2"
		f := open(t, img)
		_, err := f.ReadDir(f.Root())
		wantCorrupt(t, err)
		if !strings.Contains(err.Error(), "thread/record mismatch") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("root thread parent", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		p := findRec(t, img, lay, 2, nil)
		be.PutUint32(img[p.data+4:], 7)
		f := open(t, img)
		_, err := f.ReadDir(f.Root())
		wantCorrupt(t, err)
	})
	t.Run("thread names a different folder", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, files)
		p := findRec(t, img, lay, lay.CNIDs["/aaa"], nil)
		for i, c := range "bbb" {
			be.PutUint16(img[p.data+10+2*int64(i):], uint16(c))
		}
		f := open(t, img)
		_, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/aaa"])})
		wantCorrupt(t, err)
		if _, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/bbb"])}); err != nil {
			t.Errorf("the other folder still lists: %v", err)
		}
	})
	t.Run("thread names a file record", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, []hfsplustest.File{{Path: "/aaa", Dir: true}, {Path: "/aab"}})
		p := findRec(t, img, lay, lay.CNIDs["/aaa"], nil)
		be.PutUint16(img[p.data+10+2*2:], 'b')
		f := open(t, img)
		_, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/aaa"])})
		wantCorrupt(t, err)
	})
}

func TestMissingThreadIsFlagged(t *testing.T) {
	files := []hfsplustest.File{{Path: "/legacy", NoThread: true}, {Path: "/normal"}, {Path: "/nodir", Dir: true, NoThread: true}}
	_, lay, f := buildTree(t, hfsplustest.Options{}, files)
	root := readDir(t, f, f.Root())
	if v, ok := attr(child(t, root, "legacy"), "thread"); !ok || v != "missing" {
		t.Errorf("legacy attrs = %+v", child(t, root, "legacy").Attrs)
	}
	if _, ok := attr(child(t, root, "normal"), "thread"); ok {
		t.Error("a file with a thread is not flagged")
	}
	// A folder without a thread cannot be listed by id.
	if _, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/nodir"])}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir of a folder without a thread: %v", err)
	}
}

func TestLookupNested(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	e, err := f.Lookup("/docs/sub/deep.bin")
	if err != nil || e.ID != idOf(lay.CNIDs["/docs/sub/deep.bin"]) || e.Name != "deep.bin" || e.Size != 5 {
		t.Fatalf("Lookup = %+v, %v", e, err)
	}
	for _, p := range []string{"/docs//sub/", "docs/sub", "//docs/sub//deep.bin/"} {
		if _, err := f.Lookup(p); err != nil {
			t.Errorf("Lookup(%q): %v", p, err)
		}
	}
	for _, p := range []string{"/nope", "/docs/nope", "/docs/readme.txt/x", "/link/x", "/docs/../docs", "/.", "/docs/."} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) err = %v, want ErrNotFound", p, err)
		}
	}
	r, err := f.Lookup("/")
	if err != nil || r.ID != "cnid:2" {
		t.Errorf("Lookup(/) = %+v, %v", r, err)
	}
	// The entry found by Lookup lists like the one found by ReadDir.
	d, _ := f.Lookup("/docs")
	if got := sortedCopy(nameList(readDir(t, f, d))); !slices.Equal(got, []string{"readme.txt", "sub"}) {
		t.Errorf("docs = %v", got)
	}
}

func TestLookupCaseSensitivityHFSXvsHFSPlus(t *testing.T) {
	files := []hfsplustest.File{{Path: "/A"}, {Path: "/a"}, {Path: "/readme"}}
	t.Run("hfsx binary", func(t *testing.T) {
		_, lay, f := buildTree(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, files)
		if !hasFeature(f.Info(), "case-sensitive") {
			t.Errorf("features = %v", f.Info().Features)
		}
		a, errA := f.Lookup("/A")
		l, errL := f.Lookup("/a")
		if errA != nil || errL != nil || a.ID == l.ID || a.ID != idOf(lay.CNIDs["/A"]) || l.ID != idOf(lay.CNIDs["/a"]) || a.Name != "A" || l.Name != "a" {
			t.Errorf("A = %+v (%v), a = %+v (%v)", a, errA, l, errL)
		}
		for _, p := range []string{"/B", "/README", "/Readme"} {
			if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("Lookup(%s) = %v, want ErrNotFound", p, err)
			}
		}
		if _, err := f.Lookup("/readme"); err != nil {
			t.Error(err)
		}
	})
	one := []hfsplustest.File{{Path: "/readme"}, {Path: "/Straße"}}
	for name, o := range map[string]hfsplustest.Options{
		"hfs+":              {},
		"hfsx case folding": {HFSX: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, lay, f := buildTree(t, o, one)
			if !hasFeature(f.Info(), "case-insensitive") {
				t.Errorf("features = %v", f.Info().Features)
			}
			for _, p := range []string{"/readme", "/README", "/ReadMe"} {
				e, err := f.Lookup(p)
				if err != nil || e.ID != idOf(lay.CNIDs["/readme"]) || e.Name != "readme" {
					t.Errorf("Lookup(%s) = %+v, %v", p, e, err)
				}
			}
			if e, err := f.Lookup("/STRAßE"); err != nil || e.Name != "Straße" {
				t.Errorf("Lookup(/STRAßE) = %+v, %v", e, err)
			}
			if _, err := f.Lookup("/readme2"); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("miss = %v", err)
			}
		})
	}
}

func TestLookupPrefersExactOverFolded(t *testing.T) {
	// A forged volume with two names that fold together (a real one cannot
	// have them): the exact spelling wins whatever the order on disk.
	for _, order := range [][]string{{"Readme", "README", "readme"}, {"readme", "README", "Readme"}} {
		var files []hfsplustest.File
		for _, n := range order {
			files = append(files, hfsplustest.File{Path: "/" + n})
		}
		_, lay, f := buildTree(t, hfsplustest.Options{}, files)
		for _, n := range order {
			e, err := f.Lookup("/" + n)
			if err != nil || e.Name != n || e.ID != idOf(lay.CNIDs["/"+n]) {
				t.Errorf("order %v: Lookup(/%s) = %+v, %v", order, n, e, err)
			}
		}
	}
}

func TestLookupFallbackScanIsChargedToBudget(t *testing.T) {
	// "f1" + U+200F + "50" folds to "f150" (U+200F is ignorable) but the builder
	// orders it by raw units, after f199, so the tree descent for "f150" misses
	// it and the linear fallback scan of the folder finds it.
	var files []hfsplustest.File
	files = append(files, hfsplustest.File{Path: "/d", Dir: true})
	for i := 0; i < 300; i++ {
		if i != 150 {
			files = append(files, hfsplustest.File{Path: fmt.Sprintf("/d/f%03d", i)})
		}
	}
	hidden := "f1" + string(rune(0x200F)) + "50" // U+200F between "f1" and "50"
	files = append(files, hfsplustest.File{Path: "/d/hidden", NameUnits: units(hidden)})
	_, _, f := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, files)
	before := f.DirBudget()
	e, err := f.Lookup("/d/f150")
	if err != nil || e.Name != hidden {
		t.Fatalf("Lookup = %+v, %v", e, err)
	}
	if spent := before - f.DirBudget(); spent <= 0 || spent%1024 != 0 {
		t.Errorf("the fallback scan spent %d bytes of the budget, want a positive whole number of 1024-byte nodes", spent)
	}
	// A miss is charged too, and with the budget gone it is not "not found".
	f.SetDirBudget(2048)
	if _, err := f.Lookup("/d/zzz"); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a miss whose scan was cut by the budget = %v, want a CorruptError", err)
	}
	f.SetDirBudget(1 << 30)
	if _, err := f.Lookup("/d/zzz"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a plain miss = %v, want ErrNotFound", err)
	}
	// An exact name is found by the descent alone.
	f.SetDirBudget(0)
	if _, err := f.Lookup("/d/f007"); err != nil {
		t.Errorf("exact lookup with an empty budget: %v", err)
	}
}

func TestUnicodeNamesDecomposedAsStored(t *testing.T) {
	nfd := "é.txt" // "e" + U+0301, decomposed (NFD)
	nfc := "é.txt"  // U+00E9, composed (NFC)
	for name, o := range map[string]hfsplustest.Options{
		"hfs+":        {},
		"hfsx binary": {HFSX: true, CaseSensitive: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, lay, f := buildTree(t, o, []hfsplustest.File{{Path: "/n", NameUnits: units(nfd)}})
			es := readDir(t, f, f.Root())
			if len(es) != 1 || es[0].Name != nfd || es[0].RawName != nil {
				t.Fatalf("listing = %+v, want the name exactly as stored", es)
			}
			if es[0].ID != idOf(lay.CNIDs["/n"]) {
				t.Errorf("id = %s", es[0].ID)
			}
			if _, err := f.Lookup("/" + nfc); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("NFC lookup = %v, want ErrNotFound (no normalization)", err)
			}
			if e, err := f.Lookup("/" + nfd); err != nil || e.ID != es[0].ID {
				t.Errorf("stored-form lookup = %+v, %v", e, err)
			}
		})
	}
}

func rawForm(u []uint16) (string, []byte) {
	b := make([]byte, 2*len(u))
	for i, c := range u {
		be.PutUint16(b[2*i:], c)
	}
	return "~raw~" + base64.RawURLEncoding.EncodeToString(b), b
}

func TestNamePolicies(t *testing.T) {
	cases := []struct {
		path  string
		units []uint16
		raw   bool
	}{
		{"/slash", units("a/b"), true},
		{"/colon", units("a:b"), false},
		{"/dot", units("."), true},
		{"/dotdot", units(".."), true},
		{"/nul", units("n\x00l"), true},
		{"/lone", []uint16{'x', 0xD800}, true},
		{"/lone2", []uint16{0xDC00, 'y'}, true},
		{"/emoji", units("a\U0001F600b"), false},
		{"/dots", units("..."), false},
		{"/looks", units("~raw~AAAA"), false},
	}
	var files []hfsplustest.File
	for _, c := range cases {
		files = append(files, hfsplustest.File{Path: c.path, NameUnits: c.units})
	}
	_, lay, f := buildTree(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, files)
	es := readDir(t, f, f.Root())
	if len(es) != len(cases) {
		t.Fatalf("%d entries, want %d: %v", len(es), len(cases), nameList(es))
	}
	byID := map[string]filesys.Entry{}
	for _, e := range es {
		byID[e.ID] = e
	}
	for _, c := range cases {
		e, ok := byID[idOf(lay.CNIDs[c.path])]
		if !ok {
			t.Fatalf("%s not listed", c.path)
		}
		if c.raw {
			display, raw := rawForm(c.units)
			if e.Name != display || !bytes.Equal(e.RawName, raw) {
				t.Errorf("%s: name %q raw %x, want %q raw %x", c.path, e.Name, e.RawName, display, raw)
			}
		} else if e.Name != decodeStr(c.units) || e.RawName != nil {
			t.Errorf("%s: name %q raw %x, want %q and no raw", c.path, e.Name, e.RawName, decodeStr(c.units))
		}
		// The displayed form round-trips through Lookup.
		got, err := f.Lookup("/" + e.Name)
		if err != nil || got.ID != e.ID {
			t.Errorf("%s: Lookup(%q) = %+v, %v", c.path, e.Name, got, err)
		}
	}
	// A raw name is not reachable by its literal spelling.
	for _, p := range []string{"/.", "/..", "/n\x00l"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	// Aliases must be canonical and stand only for names shown that way.
	slash, _ := rawForm(units("a/b"))
	_, slashRaw := rawForm(units("a/b"))
	plain, _ := rawForm(units("a:b"))
	for label, p := range map[string]string{
		"padded":            "/" + slash + "=",
		"standard alphabet": "/~raw~" + strings.NewReplacer("-", "+", "_", "/").Replace(base64.RawStdEncoding.EncodeToString(slashRaw)) + "=",
		"plain name":        "/" + plain,
		"odd length":        "/~raw~" + base64.RawURLEncoding.EncodeToString([]byte{0, 'a', 0}),
		"not base64":        "/~raw~!!!",
		"empty":             "/~raw~",
		"newline inside":    "/" + slash[:8] + "\n" + slash[8:],
	} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%s: Lookup(%q) = %v, want ErrNotFound", label, p, err)
		}
	}
	// A real name that looks like a display form is found literally.
	if e, err := f.Lookup("/~raw~AAAA"); err != nil || e.ID != idOf(lay.CNIDs["/looks"]) {
		t.Errorf("literal ~raw~ name = %+v, %v", e, err)
	}
	// On an HFS+ volume the display policy is the same.
	_, lay2, f2 := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/slash", NameUnits: units("a/b")}, {Path: "/colon", NameUnits: units("a:b")}})
	es2 := readDir(t, f2, f2.Root())
	if e := child(t, es2, slash); e.ID != idOf(lay2.CNIDs["/slash"]) {
		t.Errorf("hfs+ raw name: %+v", es2)
	}
	child(t, es2, "a:b")
}

func decodeStr(u []uint16) string { return string(utf16.Decode(u)) }

func bigDir(n int) []hfsplustest.File {
	files := []hfsplustest.File{{Path: "/big", Dir: true}}
	for i := 0; i < n; i++ {
		files = append(files, hfsplustest.File{Path: fmt.Sprintf("/big/f%04d", i)})
	}
	return files
}

func TestMultiLevelDirectory(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, bigDir(600))
	if len(lay.CatalogLevels[0]) < 4 || lay.CatalogDepth < 3 {
		t.Fatalf("the catalog has %d leaves, depth %d: not multi-level", len(lay.CatalogLevels[0]), lay.CatalogDepth)
	}
	big, err := f.Lookup("/big")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := attr(big, "valence"); v != "600" {
		t.Errorf("valence = %s", v)
	}
	es := readDir(t, f, big)
	if len(es) != 600 {
		t.Fatalf("%d entries, want 600", len(es))
	}
	seen := map[string]bool{}
	for _, e := range es {
		if seen[e.ID] || !strings.HasPrefix(e.Name, "f") {
			t.Fatalf("duplicate or foreign entry %+v", e)
		}
		seen[e.ID] = true
	}
	for i := 0; i < 600; i += 37 {
		if e, err := f.Lookup(fmt.Sprintf("/big/f%04d", i)); err != nil || !seen[e.ID] {
			t.Errorf("Lookup %d: %+v, %v", i, e, err)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestDirectoryReadBudget(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, bigDir(600))
	big, _ := f.Lookup("/big")
	full := f.DirBudget()
	readDir(t, f, big)
	nodes := full - f.DirBudget()
	if nodes < 4*1024 || nodes%1024 != 0 {
		t.Fatalf("a listing of 600 entries was charged %d bytes", nodes)
	}

	// Cut part way: a partial listing and one warning.
	f.SetDirBudget(3 * 1024)
	es, err := f.ReadDir(big)
	if err != nil || len(es) == 0 || len(es) >= 600 {
		t.Fatalf("partial listing: %d entries, %v", len(es), err)
	}
	if !hasWarning(f.Info(), "budget") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
	// Exhausted before the first entry: an error, never an empty listing.
	f.SetDirBudget(0)
	es, err = f.ReadDir(big)
	wantCorrupt(t, err)
	if es != nil {
		t.Errorf("entries returned with the error: %v", es)
	}
	// Single-entry lookups are not charged to the listing budget.
	if e, err := f.Lookup("/big/f0123"); err != nil || e.Name != "f0123" {
		t.Errorf("Lookup with an empty budget: %+v, %v", e, err)
	}
	if f.DirBudget() != 0 {
		t.Errorf("budget = %d", f.DirBudget())
	}
}

func TestDirectoryEntryCap(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, bigDir(100))
	big, _ := f.Lookup("/big")
	f.SetDirEntryCap(10)
	es := readDir(t, f, big)
	if len(es) != 10 {
		t.Fatalf("%d entries with a cap of 10", len(es))
	}
	if !hasWarning(f.Info(), "more than 10 entries") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

func TestInfoWarningsAccumulateAcrossReads(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, sampleTree())
	for _, p := range []string{"/docs", "/docs/sub"} {
		pos := findRec(t, img, lay, parentOf(lay, p), units(p[strings.LastIndex(p, "/")+1:]))
		be.PutUint32(img[pos.data+4:], 9) // valence lies
	}
	f := open(t, img)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings before any read: %v", w)
	}
	readDir(t, f, filesys.Entry{ID: idOf(lay.CNIDs["/docs"])})
	w1 := f.Info().Warnings
	if len(w1) != 1 || !strings.Contains(w1[0], "valence") {
		t.Fatalf("after the first read: %v", w1)
	}
	readDir(t, f, filesys.Entry{ID: idOf(lay.CNIDs["/docs/sub"])})
	w2 := f.Info().Warnings
	if len(w2) != 2 || w2[0] != w1[0] {
		t.Fatalf("after the second read: %v", w2)
	}
	readDir(t, f, filesys.Entry{ID: idOf(lay.CNIDs["/docs"])})
	if w3 := f.Info().Warnings; !slices.Equal(w3, w2) {
		t.Errorf("a repeated read changed the warnings: %v", w3)
	}
	w2[0] = "mutated"
	if f.Info().Warnings[0] == "mutated" {
		t.Error("Info().Warnings shares its backing array")
	}
}

func parentOf(lay *hfsplustest.Layout, p string) uint32 {
	return lay.CNIDs[p[:strings.LastIndex(p, "/")+1][:max(1, strings.LastIndex(p, "/"))]]
}

func TestDirHostile(t *testing.T) {
	t.Run("valence lies", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, sampleTree())
		pos := findRec(t, img, lay, 2, units("docs"))
		be.PutUint32(img[pos.data+4:], 0xFFFFFFFF)
		f := open(t, img)
		es := readDir(t, f, filesys.Entry{ID: idOf(lay.CNIDs["/docs"])})
		if len(es) != 2 || !hasWarning(f.Info(), "valence") {
			t.Errorf("entries %d, warnings %v", len(es), f.Info().Warnings)
		}
		d, _ := f.Lookup("/docs")
		if v, _ := attr(d, "valence"); v != "4294967295" {
			t.Errorf("valence attr = %q", v)
		}
	})
	t.Run("folder id equals an ancestor", func(t *testing.T) {
		files := []hfsplustest.File{{Path: "/a", Dir: true}, {Path: "/a/b", Dir: true}, {Path: "/a/b/c"}}
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		pos := findRec(t, img, lay, lay.CNIDs["/a"], units("b"))
		be.PutUint32(img[pos.data+8:], lay.CNIDs["/a"])
		f := open(t, img)
		es := readDir(t, f, filesys.Entry{ID: idOf(lay.CNIDs["/a"])})
		if len(es) != 1 || es[0].ID != idOf(lay.CNIDs["/a"]) || es[0].Type != filesys.TypeDir {
			t.Fatalf("entries = %+v", es)
		}
	})
	t.Run("thread parent chain loops", func(t *testing.T) {
		files := []hfsplustest.File{{Path: "/a", Dir: true}, {Path: "/b", Dir: true}}
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		a, b := lay.CNIDs["/a"], lay.CNIDs["/b"]
		be.PutUint32(img[findRec(t, img, lay, a, nil).data+4:], b)
		be.PutUint32(img[findRec(t, img, lay, b, nil).data+4:], a)
		f := open(t, img)
		for _, id := range []uint32{a, b} {
			_, err := f.ReadDir(filesys.Entry{ID: idOf(id)})
			wantCorrupt(t, err)
		}
	})
	t.Run("name length past the record", func(t *testing.T) {
		files := []hfsplustest.File{{Path: "/d", Dir: true}, {Path: "/d/ok1"}, {Path: "/d/bad"}, {Path: "/d/ok2"}}
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		d := lay.CNIDs["/d"]
		be.PutUint16(img[findRec(t, img, lay, d, units("bad")).key+6:], 255)
		f := open(t, img)
		es := readDir(t, f, filesys.Entry{ID: idOf(d)})
		if got := sortedCopy(nameList(es)); !slices.Equal(got, []string{"ok1", "ok2"}) {
			t.Errorf("entries = %v", got)
		}
		if !hasWarning(f.Info(), "damaged") {
			t.Errorf("warnings = %v", f.Info().Warnings)
		}
		// A thread whose name overruns its record cannot resolve the folder.
		be.PutUint16(img[findRec(t, img, lay, d, nil).data+8:], 255)
		f = open(t, img)
		_, err := f.ReadDir(filesys.Entry{ID: idOf(d)})
		wantCorrupt(t, err)
	})
	t.Run("records of unknown or short type", func(t *testing.T) {
		o := hfsplustest.Options{RawRecords: []hfsplustest.RawRecord{
			{Parent: 2, Name: units("short"), Data: []byte{0, 1}},
			{Parent: 2, Name: units("tiny"), Data: []byte{7}},
			{Parent: 2, Name: units("weird"), Data: make([]byte, 100)},
		}}
		_, _, f := buildTree(t, o, []hfsplustest.File{{Path: "/ok"}})
		es := readDir(t, f, f.Root())
		if got := nameList(es); !slices.Equal(got, []string{"ok"}) {
			t.Errorf("entries = %v", got)
		}
	})
	t.Run("a million junk records", func(t *testing.T) {
		if testing.Short() {
			t.Skip("builds a 16 MiB catalog")
		}
		const n = 1 << 20
		junk := make([]hfsplustest.RawRecord, n)
		for i := range junk {
			junk[i] = hfsplustest.RawRecord{Parent: 2, Name: []uint16{uint16(i >> 16), uint16(i)}, Data: []byte{0, 1}} // a folder record cut short
		}
		img, lay := hfsplustest.BuildLayout(hfsplustest.Options{HFSX: true, CaseSensitive: true, Label: "J", Blocks: 6000, RawRecords: junk}, []hfsplustest.File{{Path: "/real"}})
		f := open(t, img)
		before := f.DirBudget()
		es, err := f.ReadDir(f.Root())
		if err != nil {
			t.Fatal(err)
		}
		if len(es) > 1 {
			t.Errorf("%d entries", len(es))
		}
		if spent := before - f.DirBudget(); spent <= 0 || spent > int64(len(lay.CatalogLevels[0]))*int64(lay.NodeSize) {
			t.Errorf("the listing read %d bytes, the catalog's leaves hold %d", spent, int64(len(lay.CatalogLevels[0]))*int64(lay.NodeSize))
		}
		if !hasWarning(f.Info(), "damaged") {
			t.Errorf("warnings = %v", f.Info().Warnings)
		}
		// The descent still finds a real name.
		if e, err := f.Lookup("/real"); err != nil || e.Name != "real" {
			t.Errorf("Lookup(/real) = %+v, %v", e, err)
		}
	})
}

func TestWalkWholeTree(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	var paths []string
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("%s: %v", p, err)
		}
		paths = append(paths, p+" "+e.Type.String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/classic-link symlink", "/dev other", "/docs dir", "/docs/readme.txt file", "/docs/sub dir", "/docs/sub/deep.bin file", "/link symlink", "/old file"}
	if !slices.Equal(paths, want) {
		t.Errorf("walk = %v\nwant %v", paths, want)
	}

	t.Run("cross-linked folder is a cycle", func(t *testing.T) {
		files := []hfsplustest.File{{Path: "/a", Dir: true}, {Path: "/a/f"}, {Path: "/a/b", Dir: true}, {Path: "/c", Dir: true}}
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		pos := findRec(t, img, lay, lay.CNIDs["/a"], units("b"))
		be.PutUint32(img[pos.data+8:], lay.CNIDs["/a"])
		f := open(t, img)
		var cycles []string
		var visited []string
		err := filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, err error) error {
			if err != nil {
				if !errors.Is(err, filesys.ErrCorrupt) {
					t.Errorf("%s: %v", p, err)
				}
				cycles = append(cycles, p)
				return nil
			}
			visited = append(visited, p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cycles, []string{"/a/b"}) {
			t.Errorf("cycles = %v (visited %v)", cycles, visited)
		}
		if !strings.Contains(strings.Join(visited, " "), "/a/f") {
			t.Errorf("visited = %v", visited)
		}
	})
}

// --- real mkfs.hfsplus fixtures ---------------------------------------------

type expectFile struct {
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	CNID     uint32  `json:"cnid"`
	Mode     uint32  `json:"mode"`
	FileMode uint32  `json:"file_mode"`
	UID      uint32  `json:"uid"`
	GID      uint32  `json:"gid"`
	Created  *int64  `json:"created"`
	Modified *int64  `json:"modified"`
	Changed  *int64  `json:"changed"`
	Accessed *int64  `json:"accessed"`
	Size     int64   `json:"size"`
	Valence  *uint32 `json:"valence"`
}

type expectation struct {
	Type  string       `json:"type"`
	Label string       `json:"label"`
	Files []expectFile `json:"files"`
}

func loadFixture(t testing.TB, name string) ([]byte, expectation) {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join("testdata", name+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex expectation
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	return img, ex
}

func ts(v *int64) filesys.Timestamp {
	if v == nil {
		return filesys.Timestamp{}
	}
	return filesys.Timestamp{T: time.Unix(*v, 0).UTC(), ZoneKnown: true}
}

func TestFixtureRootTree(t *testing.T) {
	for _, name := range []string{"hfsplus-empty", "hfsx-empty", "hfsplus-journal", "hfsplus-1k", "hfsplus-wrapped"} {
		t.Run(name, func(t *testing.T) {
			img, ex := loadFixture(t, name)
			f := open(t, img)
			if got := f.Info().Label; got != ex.Label {
				t.Errorf("label = %q, want %q", got, ex.Label)
			}
			byPath := map[string]expectFile{}
			for _, e := range ex.Files {
				byPath[e.Path] = e
			}
			root := byPath["/"]
			rootEntry := f.Root()
			if rootEntry.ID != idOf(root.CNID) || rootEntry.Type != filesys.TypeDir {
				t.Errorf("Root = %+v", rootEntry)
			}
			es := readDir(t, f, rootEntry)
			if uint32(len(es)) != *root.Valence {
				t.Errorf("%d root entries, the oracle says valence %d", len(es), *root.Valence)
			}
			check := func(e filesys.Entry, want expectFile) {
				t.Helper()
				wt := filesys.Times{Created: ts(want.Created), Modified: ts(want.Modified), Changed: ts(want.Changed), Accessed: ts(want.Accessed)}
				wantType := filesys.TypeFile
				if want.Type == "dir" {
					wantType = filesys.TypeDir
				}
				if e.ID != idOf(want.CNID) || e.Type != wantType || e.Size != want.Size || e.Mode != want.FileMode || e.Mode&0o7777 != want.Mode ||
					e.UID != want.UID || e.GID != want.GID || e.Times != wt || e.Deleted || e.Encrypted {
					t.Errorf("%s = %+v\nwant %+v times %+v", want.Path, e, want, wt)
				}
				if want.Valence != nil {
					if v, _ := attr(e, "valence"); v != strconv.Itoa(int(*want.Valence)) {
						t.Errorf("%s valence attr = %q", want.Path, v)
					}
				}
			}
			check(rootEntry, root)
			for _, e := range es {
				want, ok := byPath["/"+e.Name]
				if !ok {
					t.Errorf("unexpected entry %q", e.Name)
					continue
				}
				check(e, want)
				got, err := f.Lookup("/" + e.Name)
				if err != nil || !reflect.DeepEqual(got, e) {
					t.Errorf("Lookup(/%s) = %+v, %v", e.Name, got, err)
				}
			}
			var walked []string
			if err := filesys.Walk(f, rootEntry, "/", func(p string, _ filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("walk %s: %v", p, err)
				}
				walked = append(walked, p)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(walked) != len(ex.Files)-1 {
				t.Errorf("walked %v, the oracle lists %d files besides the root", walked, len(ex.Files)-1)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a real volume: %v", w)
			}
		})
	}
}

// TestWriteBuilderImages writes the builder's images for the generator-time
// fsck cross-check (tools/fixtures/hfsplus.sh check-builder); it does nothing
// unless MINUTIAE_WRITE_BUILDER_IMAGES names a directory.
func TestWriteBuilderImages(t *testing.T) {
	dir := os.Getenv("MINUTIAE_WRITE_BUILDER_IMAGES")
	if dir == "" {
		t.Skip("MINUTIAE_WRITE_BUILDER_IMAGES is not set")
	}
	tree := []hfsplustest.File{
		{Path: "/docs", Dir: true, Mode: 0o40750, UID: 501, GID: 20, Times: sampleTimes},
		{Path: "/docs/readme.txt", Mode: 0o100640, UID: 501, GID: 20, Times: sampleTimes, FileType: "TEXT", FileCreator: "ttxt"},
		{Path: "/docs/sub", Dir: true},
		{Path: "/docs/sub/deep.bin"},
		{Path: "/link", Mode: 0o120777},
		{Path: "/Zed"},
		{Path: "/accent", NameUnits: units("é.txt")},
	}
	for _, c := range []struct {
		name string
		o    hfsplustest.Options
		tree []hfsplustest.File
	}{
		{"builder-hfsplus-tree", hfsplustest.Options{Label: "BUILDER"}, tree},
		{"builder-hfsx-cs-tree", hfsplustest.Options{Label: "BUILDER", HFSX: true, CaseSensitive: true}, tree},
		{"builder-hfsx-ci-tree", hfsplustest.Options{Label: "BUILDER", HFSX: true}, tree},
		{"builder-hfsplus-empty", hfsplustest.Options{Label: "BUILDER"}, nil},
		{"builder-hfsx-empty", hfsplustest.Options{Label: "BUILDER", HFSX: true, CaseSensitive: true}, nil},
		{"builder-hfsplus-wrapped", hfsplustest.Options{Label: "BUILDER", Wrapper: true}, tree},
		{"builder-hfsplus-big", hfsplustest.Options{Label: "BUILDER", NodeSize: 1024, Blocks: 512}, bigDir(600)},
		{"builder-hfsx-big", hfsplustest.Options{Label: "BUILDER", HFSX: true, CaseSensitive: true, NodeSize: 1024, Blocks: 512}, bigDir(600)},
	} {
		if err := os.WriteFile(filepath.Join(dir, c.name+".img"), hfsplustest.Build(c.o, c.tree), 0o600); err != nil { //nolint:gosec // developer-chosen output directory for a manual generator step
			t.Fatal(err)
		}
	}
}
