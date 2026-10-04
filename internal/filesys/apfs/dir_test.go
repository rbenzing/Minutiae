package apfs_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

func TestReadDirLiveEntries(t *testing.T) {
	f, im := openOpts(t, volOpts(dataVolume(richFiles()...)))
	ino := im.g.Volumes[0].Inodes

	top := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	if got := entryNames(top); !slices.Equal(got, []string{"docs", "empty", "sym"}) {
		t.Fatalf("root of Data lists %q", got)
	}
	docs := byName(t, top, "docs")
	if docs.Type != filesys.TypeDir || docs.Mode != 0o40750 || docs.UID != 501 || docs.GID != 20 || docs.ID != fmt.Sprintf("n:0:0:%d", ino["/docs"]) {
		t.Errorf("docs = %+v", docs)
	}
	if v, _ := attr(docs, "nchildren"); v != "3" {
		t.Errorf("docs nchildren = %q", v)
	}
	if v, _ := attr(docs, "parent_id"); v != "2" {
		t.Errorf("docs parent_id = %q", v)
	}

	kids := mustReadDir(t, f, docs)
	if got := entryNames(kids); !slices.Equal(got, []string{"a.txt", "b.txt", "link"}) {
		t.Fatalf("docs lists %q", got)
	}
	a := byName(t, kids, "a.txt")
	if a.Type != filesys.TypeFile || a.Size != 5 || a.Mode != 0o100640 || a.UID != 501 || a.GID != 20 {
		t.Errorf("a.txt = %+v", a)
	}
	for name, want := range map[string]int64{"created": 1_600_000_000_000_000_001, "modified": 1_600_000_000_000_000_002, "changed": 1_600_000_000_000_000_003, "accessed": 1_600_000_000_000_000_004} {
		ts := map[string]filesys.Timestamp{"created": a.Times.Created, "modified": a.Times.Modified, "changed": a.Times.Changed, "accessed": a.Times.Accessed}[name]
		if ts.T.UnixNano() != want || !ts.ZoneKnown {
			t.Errorf("a.txt %s = %v (zone known %v), want %d", name, ts.T.UnixNano(), ts.ZoneKnown, want)
		}
	}
	if v, _ := attr(a, "nlink"); v != "2" {
		t.Errorf("a.txt nlink = %q (it has a hard link)", v)
	}
	if _, ok := attr(a, "private_id"); ok {
		t.Error("private_id shown although it equals the inode number")
	}
	b := byName(t, kids, "b.txt")
	if b.Size != 10 || b.Mode != 0o100644 || b.UID != 0 {
		t.Errorf("b.txt = %+v", b)
	}
	if v, _ := attr(b, "nlink"); v != "1" {
		t.Errorf("b.txt nlink = %q", v)
	}
	empty := byName(t, top, "empty")
	if empty.Size != 0 || empty.Type != filesys.TypeFile {
		t.Errorf("empty = %+v", empty)
	}
	sym := byName(t, top, "sym")
	if sym.Type != filesys.TypeSymlink || sym.LinkTarget != "docs/a.txt" || sym.Mode != 0o120777 {
		t.Errorf("sym = %+v", sym)
	}

	// No entry of any listing is ever Deleted.
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil {
			t.Errorf("Walk error at %s: %v", p, werr)
		}
		if e.Deleted || !e.Times.Deleted.T.IsZero() {
			t.Errorf("%s is flagged deleted", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

func TestInodeAttributes(t *testing.T) {
	files := []apfstest.File{
		{Path: "/c", Data: []byte("zz"), CompressedFlag: true, Xattrs: []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(7)}}, UncompressedSize: 7777, BsdFlags: 0x2, ProtClass: 4, PrivateID: 99, InternalFlg: 0x8, CryptoID: 5},
		{Path: "/big", Size: 1 << 40},
	}
	f, im := openOpts(t, volOpts(dataVolume(files...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	c := byName(t, es, "c")
	if c.Size != 7777 {
		t.Errorf("compressed size = %d, want the uncompressed size", c.Size)
	}
	for k, want := range map[string]string{
		"compressed": "lzvn-attr", "private_id": "99", "protection_class": "4",
		"bsd_flags": "0x22", "internal_flags": "0x40008",
	} {
		if v, ok := attr(c, k); !ok || v != want {
			t.Errorf("attr %s = %q, %v; want %q", k, v, ok, want)
		}
	}
	if got := byName(t, es, "big").Size; got != 1<<40 {
		t.Errorf("big size = %d", got)
	}
	in, err := f.Inode(0, 0, im.g.Volumes[0].Inodes["/c"])
	if err != nil || !in.HasDstream || in.Size != 2 || in.CryptoID != 5 || in.PrivateID != 99 || in.UncompressedSize != 7777 {
		t.Errorf("inode = %+v, %v", in, err)
	}
}

func TestXfieldsParse(t *testing.T) {
	blob := apfstest.XfBlob(
		apfstest.NameField([]byte("hello")),
		apfstest.DstreamField(1234, 7),
		apfstest.XField{Type: 14, Flags: 0, Data: []byte{1, 2, 3}},
	)
	got, warns := apfs.ParseXfields(blob)
	if len(warns) != 0 {
		t.Errorf("warnings: %q", warns)
	}
	if len(got) != 3 || got[0].Type != 4 || string(got[0].Data) != "hello\x00" || got[1].Type != 8 || len(got[1].Data) != 40 ||
		got[2].Type != 14 || !slices.Equal(got[2].Data, []byte{1, 2, 3}) {
		t.Errorf("fields = %+v", got)
	}
	if fs, w := apfs.ParseXfields(nil); fs != nil || w != nil {
		t.Errorf("nil blob: %v %v", fs, w)
	}
	if fs, w := apfs.ParseXfields(apfstest.XfBlob()); len(fs) != 0 || len(w) != 0 {
		t.Errorf("empty blob: %v %v", fs, w)
	}
}

func TestXfieldsMalformed(t *testing.T) {
	good := apfstest.XfBlob(apfstest.NameField([]byte("abc")), apfstest.DstreamField(5, 0))
	patch := func(fn func(b []byte) []byte) []byte { return fn(slices.Clone(good)) }
	for _, tc := range []struct {
		name   string
		blob   []byte
		want   int // fields kept
		warned string
	}{
		{"shorter than the header", []byte{1, 0, 0}, 0, "shorter"},
		{"descriptors past the blob", patch(func(b []byte) []byte { le.PutUint16(b, 5000); return b }), 0, "do not fit"},
		{"size past the blob", patch(func(b []byte) []byte { le.PutUint16(b[4+4+2:], 4000); return b }), 1, "runs past"},
		{"all sizes past the blob", patch(func(b []byte) []byte { le.PutUint16(b[4+2:], 4000); return b }), 0, "runs past"},
		{"used_data past the blob", patch(func(b []byte) []byte { le.PutUint16(b[2:], 9000); return b }), 2, "used_data 9000"},
		{"unaligned used_data", patch(func(b []byte) []byte { le.PutUint16(b[2:], le.Uint16(b[2:])-3); return b }), -1, "multiple of 8"},
		{"duplicate types", patch(func(b []byte) []byte { b[4+4] = b[4]; return b }), 1, "more than once"},
		{"truncated data", good[:len(good)-30], 1, "runs past"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := apfs.ParseXfields(tc.blob)
			if tc.want >= 0 && len(got) != tc.want {
				t.Errorf("%d fields, want %d (%+v)", len(got), tc.want, got)
			}
			if !slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, tc.warned) }) {
				t.Errorf("warnings %q lack %q", warns, tc.warned)
			}
		})
	}
	// Random garbage never panics and never returns data outside the blob.
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	for range 20000 {
		b := make([]byte, rng.IntN(80))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		if len(b) >= 4 && rng.IntN(2) == 0 {
			le.PutUint16(b, uint16(rng.IntN(8)))
		}
		fs, _ := apfs.ParseXfields(b)
		for _, x := range fs {
			if len(x.Data) > len(b) {
				t.Fatalf("field larger than the blob: %+v", x)
			}
		}
	}
}

// drecTail builds the bytes after the 8-byte key header of a hashed record.
func hashedTail(lenAndHash uint32, name string) []byte {
	b := make([]byte, 4+len(name))
	le.PutUint32(b, lenAndHash)
	copy(b[4:], name)
	return b
}

func extraDrec(dir uint64, tail []byte, fileID uint64) apfstest.FSRecord {
	return apfstest.FSRecord{ID: dir, Type: apfstest.TypeDrec, Key: tail, Val: apfstest.DrecVal(fileID, 8)}
}

func TestDrecKeyForms(t *testing.T) {
	files := []apfstest.File{{Path: "/one", Data: []byte("1")}, {Path: "/Two", Data: []byte("22")}, {Path: "/three", Data: []byte("333")}}
	for _, hashed := range []bool{true, false} {
		v := dataVolume(files...)
		v.HashedKeys = hashed
		f, _ := openOpts(t, volOpts(v))
		got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data")))
		if !slices.Equal(got, []string{"Two", "one", "three"}) {
			t.Errorf("hashed=%v lists %q", hashed, got)
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("hashed=%v warnings: %q", hashed, w)
		}
	}

	// A wrong hash is reported (attribute and one warning each) but the entry is
	// still listed and found.
	files[0].BadHash, files[1].BadHash = true, true
	f, _ := openOpts(t, volOpts(dataVolume(files...)))
	if got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data"))); !slices.Equal(got, []string{"Two", "one", "three"}) {
		t.Errorf("wrong hashes: lists %q", got)
	}
	if e := mustLookup(t, f, "/Data/one"); e.Size != 1 {
		t.Errorf("lookup with a wrong hash = %+v", e)
	}
	if w := f.Info().Warnings; len(w) != 2 || !hasWarn(f, "\"one\"", "hash") || !hasWarn(f, "\"Two\"", "hash") {
		t.Errorf("wrong hashes: warnings %q, want one per bad record", w)
	}

	// A key that fits neither form is skipped with a warning; the rest is listed.
	v := dataVolume(files...)
	v.Extra = []apfstest.FSRecord{extraDrec(2, []byte{1, 2, 3, 4, 5}, 16), extraDrec(2, nil, 16)}
	f2, _ := openOpts(t, volOpts(v))
	if got := entryNames(mustReadDir(t, f2, mustLookup(t, f2, "/Data"))); !slices.Equal(got, []string{"Two", "one", "three"}) {
		t.Errorf("lists %q", got)
	}
	if !hasWarn(f2, "directory record skipped", "does not fit") {
		t.Errorf("warnings: %q", f2.Info().Warnings)
	}
}

func TestDrecNameValidation(t *testing.T) {
	long := strings.Repeat("a", 300) + "\x00"
	for _, tc := range []struct {
		name string
		rec  apfstest.FSRecord
		warn string
	}{
		{"name_len 0", extraDrec(2, hashedTail(0, "ab\x00"), 16), "does not fit"},
		{"name_len 1 (empty name)", extraDrec(2, hashedTail(1, "\x00"), 16), "empty name"},
		{"too long", extraDrec(2, hashedTail(301, long), 16), "300-byte name"},
		{"missing NUL", extraDrec(2, hashedTail(4, "abcd"), 16), "does not fit"},
		{"embedded NUL", extraDrec(2, hashedTail(5, "a\x00bc\x00"), 16), "NUL inside"},
		{"length past the key", extraDrec(2, hashedTail(50, "abc\x00"), 16), "does not fit"},
		{"huge name_len", extraDrec(2, hashedTail(1023, "abc\x00"), 16), "does not fit"},
		{"file id 0", extraDrec(2, apfstest.DrecKey([]byte("zero"), true, 0), 0), "out of range"},
		{"file id beyond 2^60", extraDrec(2, apfstest.DrecKey([]byte("huge"), true, 0), 1<<60), "out of range"},
		{"short value", apfstest.FSRecord{ID: 2, Type: apfstest.TypeDrec, Key: apfstest.DrecKey([]byte("short"), true, 0), Val: []byte{1, 2, 3}}, "value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := dataVolume(apfstest.File{Path: "/ok1", Data: []byte("1")}, apfstest.File{Path: "/ok2", Data: []byte("2")})
			v.Extra = []apfstest.FSRecord{tc.rec}
			f, _ := openOpts(t, volOpts(v))
			if got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data"))); !slices.Equal(got, []string{"ok1", "ok2"}) {
				t.Errorf("lists %q", got)
			}
			if !hasWarn(f, "volume 0 directory 2", tc.warn) {
				t.Errorf("warnings %q lack %q", f.Info().Warnings, tc.warn)
			}
		})
	}
}

func TestHardLinks(t *testing.T) {
	files := []apfstest.File{
		{Path: "/orig", Data: []byte("data")},
		{Path: "/dir", Dir: true},
		{Path: "/dir/second", LinkTo: "/orig"},
		{Path: "/dir/sibling", LinkTo: "/orig", LinkSibling: true},
	}
	f, im := openOpts(t, volOpts(dataVolume(files...)))
	orig := byName(t, mustReadDir(t, f, mustLookup(t, f, "/Data")), "orig")
	kids := mustReadDir(t, f, mustLookup(t, f, "/Data/dir"))
	if len(kids) != 2 {
		t.Fatalf("dir lists %q", entryNames(kids))
	}
	for _, name := range []string{"second", "sibling"} {
		e := byName(t, kids, name)
		if e.ID != orig.ID || e.Size != 4 || e.Type != filesys.TypeFile {
			t.Errorf("%s = %+v, want the inode of orig (%s)", name, e, orig.ID)
		}
		if v, _ := attr(e, "nlink"); v != "3" {
			t.Errorf("%s nlink = %q, want 3", name, v)
		}
	}
	if want := fmt.Sprintf("n:0:0:%d", im.g.Volumes[0].Inodes["/orig"]); orig.ID != want {
		t.Errorf("orig ID = %s, want %s", orig.ID, want)
	}
	// The sibling-id name is not an inode number: the record's file_id has no
	// inode, which the sibling map resolves.
	recs, err := f.DirRecords(0, 0, im.g.Volumes[0].Inodes["/dir"])
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if string(r.Name) == "sibling" && r.FileID == im.g.Volumes[0].Inodes["/orig"] {
			t.Error("the sibling record names the inode directly; the fallback is not exercised")
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

func TestSymlinkTarget(t *testing.T) {
	files := []apfstest.File{
		{Path: "/embedded", Symlink: "../target"},
		{Path: "/stream", Symlink: "x", SymlinkStream: true},
		{Path: "/binary", Symlink: "caf\xe9\xff"},
	}
	f, _ := openOpts(t, volOpts(dataVolume(files...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	e := byName(t, es, "embedded")
	if e.Type != filesys.TypeSymlink || e.LinkTarget != "../target" || e.Size != int64(len("../target")) {
		t.Errorf("embedded = %+v", e)
	}
	s := byName(t, es, "stream")
	if s.LinkTarget != "" {
		t.Errorf("stream-form target = %q, want none", s.LinkTarget)
	}
	if v, ok := attr(s, "symlink"); !ok || v != "unreadable" {
		t.Errorf("stream symlink attrs = %v", s.Attrs)
	}
	b := byName(t, es, "binary")
	if b.LinkTarget != "caf\xe9\xff" {
		t.Errorf("non-UTF-8 target = %q", b.LinkTarget)
	}
}

func TestXattrNamesInAttrs(t *testing.T) {
	var xs []apfstest.Xattr
	for i := range 40 {
		xs = append(xs, apfstest.Xattr{Name: fmt.Sprintf("user.attr%02d", i), Value: []byte{byte(i)}})
	}
	files := []apfstest.File{
		{Path: "/many", Xattrs: xs},
		{Path: "/few", Xattrs: []apfstest.Xattr{{Name: "com.apple.quarantine", Value: []byte("q")}, {Name: "b\xff", Value: nil}, {Name: "big", Value: []byte("v"), Stream: true}}},
	}
	f, _ := openOpts(t, volOpts(dataVolume(files...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	many := byName(t, es, "many")
	got := attrs(many, "xattr")
	if len(got) != 32 {
		t.Fatalf("%d xattr attrs, want 32", len(got))
	}
	for _, n := range got {
		if !strings.HasPrefix(n, "user.attr") {
			t.Errorf("xattr name %q", n)
		}
	}
	if v, _ := attr(many, "xattr_count"); v != "40" {
		t.Errorf("xattr_count = %q", v)
	}
	few := byName(t, es, "few")
	names := attrs(few, "xattr")
	slices.Sort(names)
	if want := []string{"big", "com.apple.quarantine", rawName([]byte{'b', 0xff})}; !slices.Equal(names, want) {
		t.Errorf("few xattrs = %q, want %q", names, want)
	}
	if _, ok := attr(few, "xattr_count"); ok {
		t.Error("xattr_count shown without truncation")
	}
}

func lookupFiles() []apfstest.File {
	return []apfstest.File{
		{Path: "/a/b/c.txt", Data: []byte("c")},
		{Path: "/a/B", Dir: true},
		{Path: "/Readme", Data: []byte("r")},
		{Path: "/\xff\xfe.bin", Data: []byte("b")},
	}
}

func TestLookupNested(t *testing.T) {
	f, im := openOpts(t, volOpts(dataVolume(lookupFiles()...)))
	ino := im.g.Volumes[0].Inodes
	for _, tc := range []struct {
		path string
		ino  uint64
	}{
		{"/Data/a/b/c.txt", ino["/a/b/c.txt"]},
		{"/Data//a//b/", ino["/a/b"]},
		{"Data/a", ino["/a"]},
		{"/Data/Readme", ino["/Readme"]},
	} {
		e := mustLookup(t, f, tc.path)
		if want := fmt.Sprintf("n:0:0:%d", tc.ino); e.ID != want {
			t.Errorf("Lookup(%q) = %s, want %s", tc.path, e.ID, want)
		}
	}
	if e := mustLookup(t, f, "/Data/a/b/c.txt"); e.Name != "c.txt" || e.Size != 1 || e.Type != filesys.TypeFile {
		t.Errorf("c.txt = %+v", e)
	}
	// A lookup result lists like the same entry from its parent.
	b := mustLookup(t, f, "/Data/a/b")
	if got := entryNames(mustReadDir(t, f, b)); !slices.Equal(got, []string{"c.txt"}) {
		t.Errorf("b lists %q", got)
	}
	for _, p := range []string{"/Data/nothing", "/Data/a/nothing", "/Data/Readme/x", "/Data/a/b/c.txt/d", "/Nope/a", "/Data/a/b/c.txt/.."} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

func TestLookupCaseInsensitiveVolume(t *testing.T) {
	ci := dataVolume(lookupFiles()...)
	cs := apfstest.Volume{Name: "Sens", Files: lookupFiles()}
	f, _ := openOpts(t, volOpts(ci, cs))
	if e := mustLookup(t, f, "/Data/readme"); e.Name != "Readme" {
		t.Errorf("case-insensitive Lookup = %+v", e)
	}
	if e := mustLookup(t, f, "/Data/A/b/C.TXT"); e.Name != "c.txt" {
		t.Errorf("nested case-insensitive Lookup = %+v", e)
	}
	// A volume name is never folded.
	if _, err := f.Lookup("/data/readme"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("volume name folded: %v", err)
	}
	// A case-sensitive volume does not fold.
	for _, p := range []string{"/Sens/readme", "/Sens/A/b", "/Sens/a/b/C.TXT"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) on a case-sensitive volume = %v", p, err)
		}
	}
	if e := mustLookup(t, f, "/Sens/Readme"); e.Name != "Readme" {
		t.Errorf("exact Lookup = %+v", e)
	}
	// An exact match beats a folded one.
	both := dataVolume(apfstest.File{Path: "/x", Data: []byte("lower")}, apfstest.File{Path: "/X", Data: []byte("upper!")})
	f2, _ := openOpts(t, volOpts(both))
	if e := mustLookup(t, f2, "/Data/X"); e.Size != 6 {
		t.Errorf("Lookup(X) = %+v, want the exact X", e)
	}
	if e := mustLookup(t, f2, "/Data/x"); e.Size != 5 {
		t.Errorf("Lookup(x) = %+v, want the exact x", e)
	}
	// Names that are not valid UTF-8 are never folded.
	if _, err := f.Lookup("/Data/\xff\xfe.BIN"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a non-UTF-8 name was folded: %v", err)
	}
}

func TestLookupAliasesAreCanonical(t *testing.T) {
	bad := "\xff\xfe.bin"
	f, _ := openOpts(t, volOpts(dataVolume(lookupFiles()...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	var disp filesys.Entry
	for _, e := range es {
		if string(e.RawName) == bad {
			disp = e
		}
	}
	if disp.Name != rawName([]byte(bad)) || string(disp.RawName) != bad {
		t.Fatalf("invalid-UTF-8 entry = %+v", disp)
	}
	if got := mustLookup(t, f, "/Data/"+disp.Name); got.ID != disp.ID {
		t.Errorf("display form -> %s", got.ID)
	}
	// An alias of a plain name works too.
	if got := mustLookup(t, f, "/Data/"+rawName([]byte("Readme"))); got.Name != "Readme" {
		t.Errorf("alias of Readme = %+v", got)
	}
	// Non-canonical encodings and wrong payloads do not match.
	canon := strings.TrimPrefix(disp.Name, "~raw~")
	for _, p := range []string{"/Data/~raw~" + canon + "=", "/Data/~raw~" + canon + "A", "/Data/~RAW~" + canon, "/Data/~raw~" + rawName([]byte("none"))[5:]} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	// A real file that is named like an alias wins over the alias of another.
	lit := rawName([]byte("Readme"))
	f2, _ := openOpts(t, volOpts(dataVolume(apfstest.File{Path: "/Readme", Data: []byte("1")}, apfstest.File{Path: "/" + lit, Data: []byte("22")})))
	if e := mustLookup(t, f2, "/Data/"+lit); e.Size != 2 {
		t.Errorf("literal %q = %+v, want the file with that name", lit, e)
	}
}

func TestLookupCasefoldNeverFindsDots(t *testing.T) {
	files := []apfstest.File{
		{Path: "/dot", RawName: []byte("."), Data: []byte("1")},
		{Path: "/dotdot", RawName: []byte(".."), Data: []byte("2")},
		{Path: "/slash", RawName: []byte("a/b"), Data: []byte("3")},
	}
	f, _ := openOpts(t, volOpts(dataVolume(files...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	if got := entryNames(es); !slices.Equal(got, []string{rawName([]byte(".")), rawName([]byte("..")), rawName([]byte("a/b"))}) {
		t.Fatalf("lists %q", got)
	}
	for _, p := range []string{"/Data/.", "/Data/..", "/Data/a", "/Data/b"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	// They are reachable through the display forms.
	for _, e := range es {
		if got := mustLookup(t, f, "/Data/"+e.Name); got.ID != e.ID {
			t.Errorf("Lookup(%q) = %s", e.Name, got.ID)
		}
	}
}

func TestEntryFromIDOpens(t *testing.T) {
	f, im := openOpts(t, volOpts(dataVolume(richFiles()...)))
	ino := im.g.Volumes[0].Inodes
	// An entry built outside ReadDir, from an ID alone, lists and resolves.
	dir := filesys.Entry{ID: fmt.Sprintf("n:0:0:%d", ino["/docs"])}
	if got := entryNames(mustReadDir(t, f, dir)); !slices.Equal(got, []string{"a.txt", "b.txt", "link"}) {
		t.Errorf("lists %q", got)
	}
	if got := entryNames(mustReadDir(t, f, filesys.Entry{ID: "n:0:0:2"})); !slices.Equal(got, []string{"docs", "empty", "sym"}) {
		t.Errorf("volume root lists %q", got)
	}
	if got := mustReadDir(t, f, filesys.Entry{ID: "apfs:root"}); len(got) != 1 {
		t.Errorf("root lists %d", len(got))
	}
	// Open of a directory and of a file by ID is validated (file data is a later layer).
	if _, err := f.Open(dir); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(dir) = %v", err)
	}
	if _, err := f.Open(filesys.Entry{ID: fmt.Sprintf("n:0:0:%d", ino["/docs/a.txt"])}); errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open(file by ID) = %v", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "n:0:0:424242"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open(missing inode) = %v", err)
	}
}

func TestForgedEntryFieldsAreIgnored(t *testing.T) {
	enc := apfstest.Volume{Name: "Enc", Encrypted: true, Files: []apfstest.File{{Path: "/s", Data: []byte("x")}}}
	f, _ := openOpts(t, volOpts(dataVolume(richFiles()...), enc))
	orig := byName(t, mustReadDir(t, f, mustLookup(t, f, "/Data")), "docs")

	forge := func(e filesys.Entry) filesys.Entry {
		e.Name, e.RawName = "../../etc", []byte("zz")
		e.Type, e.Size, e.Deleted, e.Encrypted = filesys.TypeFile, 1<<40, true, true
		e.LinkTarget = "/etc/passwd"
		e.Attrs = []filesys.KV{{Key: "inode", Value: "unreadable"}, {Key: "encrypted", Value: "true"}}
		return e
	}
	want := entryNames(mustReadDir(t, f, orig))
	if got := entryNames(mustReadDir(t, f, forge(orig))); !slices.Equal(got, want) {
		t.Errorf("a directory forged as a file lists %q, want %q", got, want)
	}
	// A file forged as a directory is still not one.
	file := byName(t, mustReadDir(t, f, orig), "a.txt")
	forgedDir := file
	forgedDir.Type = filesys.TypeDir
	if _, err := f.ReadDir(forgedDir); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(file forged as dir) = %v, want ErrUnsupported", err)
	}
	// Forged Deleted/Encrypted fields do not change what Open decides.
	if _, err := f.Open(forge(file)); errors.Is(err, filesys.ErrDeleted) || errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("Open(forged file) = %v", err)
	}
	// An encrypted volume forged as plain stays encrypted.
	e := mustLookup(t, f, "/Enc")
	e.Encrypted, e.Type, e.Attrs = false, filesys.TypeFile, nil
	if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("ReadDir(forged plain Enc) = %v", err)
	}
	if _, err := f.Open(e); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("Open(forged plain Enc) = %v", err)
	}
	// The listing of the forged entry carries on-disk facts only.
	for _, c := range mustReadDir(t, f, forge(orig)) {
		if c.Deleted || c.Encrypted || c.LinkTarget == "/etc/passwd" || c.Name == "../../etc" {
			t.Errorf("forged field leaked into %+v", c)
		}
	}
}

func TestForgedIDsRejectedQuickly(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(richFiles()...), apfstest.Volume{Name: "B"}))
	scans, budget := f.Scans(), f.DirBudget()
	ids := []string{
		"", "x", "n:", "n:0:0", "n:0:0:2:1", "n:2:0:2", "n:99:0:2", "n:100:0:2", "n:0:7:2", "n:0:0:0",
		"n:0:0:1152921504606846976", "n:0:0:99999999999999999999", "n:00:0:2", "n:0:00:2", "n:0:0:02", "n:+0:0:2",
		"n:-1:0:2", "n: 0:0:2", "n:0x0:0:2", "N:0:0:2", "snaps:0", "snaps:1", "snaps:7", "snaps:100", "snaps:00",
		"apfs:root ", "APFS:ROOT", "inode:2", "snaps:2",
	}
	for _, id := range ids {
		e := filesys.Entry{ID: id, Type: filesys.TypeDir}
		if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(%q) = %v, want ErrNotFound", id, err)
		}
		_, err := f.Open(e)
		existingSnaps := (id == "snaps:0" || id == "snaps:1") && errors.Is(err, filesys.ErrUnsupported)
		if !errors.Is(err, filesys.ErrNotFound) && !existingSnaps {
			t.Errorf("Open(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if got := f.Scans(); got != scans {
		t.Errorf("forged IDs started %d tree scans", got-scans)
	}
	if got := f.DirBudget(); got != budget {
		t.Errorf("forged IDs spent %d bytes of the directory budget", budget-got)
	}
}

func bigDir(n int) []apfstest.File {
	var files []apfstest.File
	for i := range n {
		files = append(files, apfstest.File{Path: fmt.Sprintf("/f%03d", i)})
	}
	return files
}

func TestDirectoryReadBudget(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(bigDir(50)...)))
	vol := mustLookup(t, f, "/Data")
	full := f.DirBudget()
	if got := len(mustReadDir(t, f, vol)); got != 50 {
		t.Fatalf("lists %d", got)
	}
	spent := full - f.DirBudget()
	if spent <= 0 {
		t.Fatalf("a listing spent %d bytes", spent)
	}
	// Single-entry reads (ReadDir of a file's ID aside) do not spend it: Open of
	// an entry by ID reads the inode only.
	before := f.DirBudget()
	_, _ = f.Open(filesys.Entry{ID: "n:0:0:16"})
	if f.DirBudget() != before {
		t.Error("Open spent directory budget")
	}

	// Mid-directory: a partial listing and one warning.
	f.SetDirBudget(spent / 2)
	es, err := f.ReadDir(vol)
	if err != nil {
		t.Fatalf("partial listing failed: %v", err)
	}
	if len(es) == 0 || len(es) >= 50 {
		t.Errorf("partial listing has %d entries", len(es))
	}
	if !hasWarn(f, "directory read budget is exhausted", "partial") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	// Exhausted before the first entry: CorruptError, and it stays that way.
	f.SetDirBudget(0)
	for range 3 {
		if _, err := f.ReadDir(vol); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir with no budget = %v, want ErrCorrupt", err)
		}
	}
	if _, err := f.Lookup("/Data/f001"); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Lookup with no budget = %v, want ErrCorrupt", err)
	}
}

func TestDirEntryCap(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(bigDir(25)...)))
	f.SetMaxDirEntries(10)
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	if len(es) != 10 {
		t.Errorf("listed %d entries, cap 10", len(es))
	}
	if !hasWarn(f, "more than 10 entries") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	// An entry beyond the cap is not found.
	if _, err := f.Lookup("/Data/f024"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup beyond the cap = %v", err)
	}
	if _, err := f.Lookup("/Data/f003"); err != nil {
		t.Errorf("Lookup inside the cap: %v", err)
	}
}

func TestDirHostile(t *testing.T) {
	t.Run("missing inode and file used as directory", func(t *testing.T) {
		v := dataVolume(
			apfstest.File{Path: "/ghost", NoInode: true, Dir: true},
			apfstest.File{Path: "/plain", Data: []byte("x")},
			apfstest.File{Path: "/lies", Data: []byte("y"), DrecType: 4}, // the record says directory, the inode a file
		)
		f, _ := openOpts(t, volOpts(v))
		es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
		g := byName(t, es, "ghost")
		if g.Type != filesys.TypeDir {
			t.Errorf("ghost type = %v, want the record's", g.Type)
		}
		if v, ok := attr(g, "inode"); !ok || v != "unreadable" {
			t.Errorf("ghost attrs = %v", g.Attrs)
		}
		if _, err := f.ReadDir(g); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(ghost) = %v, want ErrNotFound", err)
		}
		if !hasWarn(f, "whose inode cannot be read") {
			t.Errorf("warnings: %q", f.Info().Warnings)
		}
		lies := byName(t, es, "lies")
		if lies.Type != filesys.TypeFile {
			t.Errorf("an inode that is a file shows as %v", lies.Type)
		}
		if _, err := f.ReadDir(lies); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("ReadDir(file inode) = %v, want ErrUnsupported", err)
		}
		if _, err := f.ReadDir(byName(t, es, "plain")); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("ReadDir(plain file) = %v", err)
		}
	})

	t.Run("directory cycle", func(t *testing.T) {
		v := dataVolume(apfstest.File{Path: "/d/e", Dir: true})
		v.Extra = []apfstest.FSRecord{
			{ID: 17, Type: apfstest.TypeDrec, Key: apfstest.DrecKey([]byte("up"), true, 0), Val: apfstest.DrecVal(2, 4)}, // /d/e/up -> the root
			{ID: 16, Type: apfstest.TypeDrec, Key: apfstest.DrecKey([]byte("self"), true, 0), Val: apfstest.DrecVal(16, 4)},
		}
		f, im := openOpts(t, volOpts(v))
		if im.g.Volumes[0].Inodes["/d"] != 16 || im.g.Volumes[0].Inodes["/d/e"] != 17 {
			t.Fatalf("inodes = %v", im.g.Volumes[0].Inodes)
		}
		var cycles []string
		err := filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, werr error) error {
			if werr != nil {
				if !errors.Is(werr, filesys.ErrCorrupt) {
					t.Errorf("%s: %v", p, werr)
				}
				cycles = append(cycles, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(cycles)
		if want := []string{"/Data/d/e/up", "/Data/d/self"}; !slices.Equal(cycles, want) {
			t.Errorf("cycles reported at %q, want %q", cycles, want)
		}
	})

	t.Run("records out of order", func(t *testing.T) {
		v := dataVolume(bigDir(60)...)
		v.TreeMaxKeys = 4
		v.Reorder = func(r []apfstest.FSRecord) []apfstest.FSRecord { slices.Reverse(r); return r }
		f, _ := openOpts(t, volOpts(v))
		// The tree is garbage to the seek; it must neither panic nor loop.
		before := f.Scans()
		_, _ = f.Lookup("/Data/f010")
		if vol, err := f.Lookup("/Data"); err == nil {
			_, _ = f.ReadDir(vol)
		}
		_, _ = f.ReadDir(filesys.Entry{ID: "n:0:0:2"})
		if got := f.Scans() - before; got > 1000 {
			t.Errorf("a garbage tree cost %d scans", got)
		}
	})

	t.Run("100k records under a tiny budget", func(t *testing.T) {
		v := dataVolume()
		v.TreeMaxKeys = 0
		for i := range 100_000 {
			v.Extra = append(v.Extra, apfstest.FSRecord{
				ID: 2, Type: apfstest.TypeDrec, Key: apfstest.DrecKey([]byte(fmt.Sprintf("n%06d", i)), true, 0), Val: apfstest.DrecVal(3, 4),
			})
		}
		o := volOpts(v)
		o.Blocks = 3000
		f, _ := openOpts(t, o)
		f.SetDirBudget(10_000)
		scans := f.Scans()
		es, err := f.ReadDir(filesys.Entry{ID: "n:0:0:2"})
		if err != nil {
			t.Fatal(err)
		}
		if len(es) == 0 || len(es) > 500 {
			t.Errorf("listed %d entries under a 10000-byte budget", len(es))
		}
		if got := f.Scans() - scans; got > int64(2*len(es)+5) { // one scan for the directory, two per entry at most
			t.Errorf("%d scans for a listing of %d entries", got, len(es))
		}
		if f.DirBudget() >= 0 {
			t.Errorf("budget left %d: the listing stopped for another reason", f.DirBudget())
		}
	})
}

func TestInodeHostile(t *testing.T) {
	inodeRec := func(id uint64, val []byte) apfstest.FSRecord {
		return apfstest.FSRecord{ID: id, Type: apfstest.TypeInode, Val: val}
	}
	fixed := apfstest.InodeVal(apfstest.InodeFields{Parent: 2, Private: 100, Mode: 0o100644, Links: 1, Times: [4]uint64{1, 2, 3, 4}}, nil)
	huge := apfstest.DstreamField(1<<63+5, 0)
	shortDs := apfstest.XField{Type: 8, Data: []byte{1, 2, 3}}
	v := dataVolume()
	v.Extra = []apfstest.FSRecord{
		inodeRec(100, fixed[:50]), // too short
		inodeRec(101, apfstest.InodeVal(apfstest.InodeFields{Mode: 0o100644, Times: [4]uint64{1, 1, 1, 1 << 63}}, apfstest.XfBlob(huge))),
		inodeRec(102, apfstest.InodeVal(apfstest.InodeFields{Mode: 0o100644}, apfstest.XfBlob(shortDs))),
		inodeRec(103, append(slices.Clone(fixed), 1, 2, 3)), // truncated xfields header
		inodeRec(104, fixed),
		inodeRec(104, apfstest.InodeVal(apfstest.InodeFields{Mode: 0o100600}, nil)), // a second inode record
		{ID: 105, Type: apfstest.TypeInode, Key: []byte{1, 2}, Val: fixed},          // a key that is not an inode key
		{ID: 106, Type: apfstest.TypeXattr, Key: []byte{9, 9, 9}, Val: []byte{1}},   // an xattr with no inode
	}
	f, _ := openOpts(t, volOpts(v))

	if _, err := f.Inode(0, 0, 100); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("short inode = %v, want ErrCorrupt", err)
	}
	in, err := f.Inode(0, 0, 101)
	if err != nil || in.Size != 1<<63-1 || !in.HasDstream {
		t.Errorf("huge dstream size = %+v, %v (want it clamped)", in, err)
	}
	if !hasWarn(f, "inode 101", "out of range") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	in, err = f.Inode(0, 0, 102)
	if err != nil || in.HasDstream {
		t.Errorf("short dstream = %+v, %v", in, err)
	}
	if !hasWarn(f, "inode 102", "too short") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	if _, err = f.Inode(0, 0, 103); err != nil {
		t.Errorf("inode with a truncated xfield header: %v", err)
	}
	in, err = f.Inode(0, 0, 104)
	if err != nil || in.Mode != 0o100644 {
		t.Errorf("duplicate inode records: %+v, %v (the first wins)", in, err)
	}
	if !hasWarn(f, "inode 104", "more than one inode record") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	for _, ino := range []uint64{105, 106, 107} {
		if _, err := f.Inode(0, 0, ino); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Inode(%d) = %v, want ErrNotFound", ino, err)
		}
	}
	for _, ino := range []uint64{0, 1 << 60, 1<<64 - 1} {
		if _, err := f.Inode(0, 0, ino); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Inode(%d) = %v, want ErrNotFound", ino, err)
		}
	}
	// A listing of directory 2 with entries for hostile inodes stays usable.
	if _, err := f.ReadDir(filesys.Entry{ID: "n:0:0:2"}); err != nil {
		t.Error(err)
	}
}

func TestWalkWholeTree(t *testing.T) {
	a := dataVolume(richFiles()...)
	b := apfstest.Volume{Name: "Other", Files: []apfstest.File{{Path: "/x/y/z", Data: []byte("z")}, {Path: "/x/w"}, {Path: "/l", Symlink: "x"}}}
	f, _ := openOpts(t, volOpts(a, b))

	var count int
	seen := map[string]bool{}
	err := filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, werr error) error {
		if werr != nil {
			t.Errorf("%s: %v", p, werr)
			return nil
		}
		if seen[p] {
			t.Errorf("%s visited twice", p)
		}
		seen[p] = true
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two volumes + the builder's entries: every file path and its ancestors.
	want := 2 + expectedEntries(a.Files) + expectedEntries(b.Files)
	if count != want {
		t.Errorf("Walk visited %d entries, the builder wrote %d", count, want)
	}
	for _, p := range []string{"/Data/docs/link", "/Data/sym", "/Other/x/y/z", "/Other/l"} {
		if !seen[p] {
			t.Errorf("Walk missed %s", p)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

// expectedEntries counts the distinct paths of files and their ancestors.
func expectedEntries(files []apfstest.File) int {
	set := map[string]bool{}
	for _, f := range files {
		p := strings.Trim(f.Path, "/")
		parts := strings.Split(p, "/")
		for i := range parts {
			set[strings.Join(parts[:i+1], "/")] = true
		}
	}
	return len(set)
}
