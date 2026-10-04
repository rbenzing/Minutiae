package hfsplus_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

const (
	nameDecmpfs  = "com.apple.decmpfs"
	nameCProtect = "com.apple.system.cprotect"
	ufCompressed = 0x20
)

// decmpfsValue is a com.apple.decmpfs attribute value: magic "fpmc", the type
// and the uncompressed size (little-endian), then the payload.
func decmpfsValue(typ uint32, size uint64, payload []byte) []byte {
	v := make([]byte, 16, 16+len(payload))
	copy(v, "fpmc")
	binary.LittleEndian.PutUint32(v[4:], typ)
	binary.LittleEndian.PutUint64(v[8:], size)
	return append(v, payload...)
}

func zlibBytes(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// rsrcZlib builds the resource fork of a type-4 file: a 0x100-byte header whose
// first word is 0x100 (big-endian), then the data size (big-endian), the block
// count (little-endian) and a table of (offset, size) pairs (little-endian;
// offsets are relative to the start of the block count), then the blocks of up
// to 64 KiB each, zlib-compressed or, when raw lists the block, stored after a
// 0xFF marker, then a footer.
func rsrcZlib(content []byte, raw ...int) []byte {
	const blk = 65536
	nb := (len(content) + blk - 1) / blk
	blocks := make([][]byte, nb)
	for i := range blocks {
		chunk := content[i*blk : min(len(content), (i+1)*blk)]
		if slices.Contains(raw, i) {
			blocks[i] = append([]byte{0xFF}, chunk...)
		} else {
			blocks[i] = zlibBytes(chunk)
		}
	}
	table := 4 + 8*nb
	dataSize := table
	for _, b := range blocks {
		dataSize += len(b)
	}
	out := make([]byte, 0x100+4+dataSize+50)
	binary.BigEndian.PutUint32(out[0:], 0x100)
	binary.BigEndian.PutUint32(out[4:], uint32(0x100+4+dataSize))
	binary.BigEndian.PutUint32(out[8:], uint32(dataSize))
	binary.BigEndian.PutUint32(out[12:], 0x32)
	binary.BigEndian.PutUint32(out[0x100:], uint32(dataSize))
	binary.LittleEndian.PutUint32(out[0x104:], uint32(nb))
	off := table
	for i, b := range blocks {
		binary.LittleEndian.PutUint32(out[0x108+8*i:], uint32(off))
		binary.LittleEndian.PutUint32(out[0x108+8*i+4:], uint32(len(b)))
		copy(out[0x104+off:], b)
		off += len(b)
	}
	return out
}

func compressible(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = "hello, world "[i%13]
	}
	return b
}

func attrOf(e filesys.Entry, key string) string {
	v, _ := attr(e, key)
	return v
}

func attrValues(e filesys.Entry, key string) []string {
	var out []string
	for _, kv := range e.Attrs {
		if kv.Key == key {
			out = append(out, kv.Value)
		}
	}
	return out
}

func TestCompressedFileUnsupportedNotEmpty(t *testing.T) {
	files := []hfsplustest.File{
		// UF_COMPRESSED with an empty data fork: the content lives elsewhere.
		{Path: "/lzvn", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(7, 1234, []byte("xxxx"))}}},
		{Path: "/lzfse", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(11, 99, nil)}}},
		// The flag without any attribute.
		{Path: "/flagonly", OwnerFlags: ufCompressed},
		// An unreadable header: still compressed.
		{Path: "/badhdr", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: []byte("fpmc-short")}}},
		{Path: "/badmagic", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: append([]byte("XXXX"), make([]byte, 12)...)}}},
		{Path: "/plain", Data: []byte("plain")},
	}
	_, _, f := buildTree(t, hfsplustest.Options{}, files)
	root := readDir(t, f, f.Root())
	for _, tc := range []struct {
		name, typ string
		size      int64
	}{{"lzvn", "7", 1234}, {"lzfse", "11", 99}} {
		e := child(t, root, tc.name)
		if e.Size != tc.size || attrOf(e, "compressed") != tc.typ || attrOf(e, "decmpfs_type") != tc.typ || attrOf(e, "uncompressed_size") != strconv.FormatInt(tc.size, 10) {
			t.Errorf("%s: size %d attrs %+v", tc.name, e.Size, e.Attrs)
		}
		fl, err := f.Open(e)
		if !errors.Is(err, filesys.ErrUnsupported) || fl != nil {
			t.Errorf("%s: Open = %v, %v; want ErrUnsupported, never an empty file", tc.name, fl, err)
		}
	}
	for _, n := range []string{"flagonly", "badhdr", "badmagic"} {
		e := child(t, root, n)
		if attrOf(e, "compressed") != "unknown" {
			t.Errorf("%s: attrs %+v", n, e.Attrs)
		}
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("%s: Open = %v", n, err)
		}
	}
	if !hasWarning(f.Info(), "decmpfs") {
		t.Errorf("no decmpfs warning in %q", f.Info().Warnings)
	}
	if _, ok := attr(child(t, root, "plain"), "compressed"); ok {
		t.Error("a plain file is flagged compressed")
	}
}

func TestDecmpfsZlibInline(t *testing.T) {
	content := compressible(5000)
	raw := []byte("stored as is")
	files := []hfsplustest.File{
		{Path: "/z3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, uint64(len(content)), zlibBytes(content))}}},
		{Path: "/raw3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, uint64(len(raw)), append([]byte{0xFF}, raw...))}}},
		{Path: "/empty3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 0, zlibBytes(nil))}}},
		{Path: "/short3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 9000, zlibBytes(content))}}},         // claims more than the stream holds
		{Path: "/long3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 100, zlibBytes(content))}}},           // stream holds more than claimed
		{Path: "/bomb3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 1<<45, zlibBytes(content))}}},         // implausible size
		{Path: "/rawlen3", OwnerFlags: ufCompressed, Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, 3, append([]byte{0xFF}, raw...))}}}, // raw length differs
	}
	_, _, f := buildTree(t, hfsplustest.Options{}, files)
	e, fl := openPath(t, f, "/z3")
	if e.Size != 5000 || fl.Size() != 5000 || len(fl.Runs()) != 0 || attrOf(e, "compressed") != "zlib" || attrOf(e, "decmpfs_type") != "3" {
		t.Errorf("z3: entry %+v size %d runs %v", e.Attrs, fl.Size(), fl.Runs())
	}
	if !bytes.Equal(readAll(t, fl), content) {
		t.Error("z3 content differs")
	}
	// Partial and out-of-range reads.
	buf := make([]byte, 100)
	if n, err := fl.ReadAt(buf, 4950); n != 50 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:50], content[4950:]) {
		t.Errorf("ReadAt near the end = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf, 5000); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	_, fl = openPath(t, f, "/raw3")
	if !bytes.Equal(readAll(t, fl), raw) {
		t.Error("raw3 content differs")
	}
	_, fl = openPath(t, f, "/empty3")
	if fl.Size() != 0 || len(fl.Runs()) != 0 {
		t.Errorf("empty3 size %d", fl.Size())
	}
	for _, n := range []string{"short3", "long3", "bomb3", "rawlen3"} {
		e, err := f.Lookup("/" + n)
		if err != nil {
			t.Fatal(err)
		}
		if fl, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
			t.Errorf("%s: Open = %v, %v; want ErrCorrupt", n, fl, err)
		}
	}
}

func TestDecmpfsZlibResourceFork(t *testing.T) {
	big := make([]byte, 3*65536+777) // four blocks: a mix of compressible and incompressible data
	copy(big, compressible(65536))
	copy(big[65536:], pattern(65536, 3))
	copy(big[2*65536:], compressible(65536))
	copy(big[3*65536:], pattern(777, 9))
	mk := func(path string, content []byte, rsrc []byte) hfsplustest.File {
		return hfsplustest.File{
			Path: path, OwnerFlags: ufCompressed, Rsrc: rsrc,
			Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(4, uint64(len(content)), nil)}},
		}
	}
	one := compressible(1000)
	files := []hfsplustest.File{
		mk("/big", big, rsrcZlib(big, 1)), // block 1 is stored raw
		mk("/one", one, rsrcZlib(one)),
		// Damaged variants.
		mk("/wrongsize", append(slices.Clone(big), make([]byte, 100)...), rsrcZlib(big)), // the last block is 100 bytes shorter than the header says
		mk("/toomany", append(slices.Clone(big), make([]byte, 65536)...), rsrcZlib(big)), // needs 5 blocks, the table has 4
		mk("/norsrc", big, nil),                                              // no resource fork at all
		mk("/shortblock", big[:70000], rsrcZlib(big[:70000-100])),            // the last block decompresses to fewer bytes than claimed
		mk("/layout", one, append([]byte{0, 0, 0, 1}, make([]byte, 400)...)), // unknown header size
	}
	img, lay, f := buildTree(t, hfsplustest.Options{Blocks: 512}, files)
	_ = img
	e, fl := openPath(t, f, "/big")
	if e.Size != int64(len(big)) || fl.Size() != int64(len(big)) || len(fl.Runs()) != 0 || attrOf(e, "compressed") != "zlib" || attrOf(e, "decmpfs_type") != "4" {
		t.Fatalf("big: attrs %+v size %d runs %v", e.Attrs, fl.Size(), fl.Runs())
	}
	if !bytes.Equal(readAll(t, fl), big) {
		t.Error("big content differs")
	}
	buf := make([]byte, 70000)
	if n, err := fl.ReadAt(buf, 60000); n != len(buf) || err != nil || !bytes.Equal(buf, big[60000:130000]) {
		t.Errorf("read across blocks = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf[:500], int64(len(big))-100); n != 100 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:100], big[len(big)-100:]) {
		t.Errorf("read across the end = %d, %v", n, err)
	}
	_, fl = openPath(t, f, "/one")
	if !bytes.Equal(readAll(t, fl), one) {
		t.Error("one content differs")
	}
	for _, n := range []string{"wrongsize", "toomany", "norsrc", "shortblock"} {
		e, err := f.Lookup("/" + n)
		if err != nil {
			t.Fatal(err)
		}
		fl, err := f.Open(e)
		if err == nil {
			// The damage may only show when the content is read.
			_, err = fl.ReadAt(make([]byte, fl.Size()), 0)
		}
		if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v; want ErrCorrupt", n, err)
		}
	}
	if e, _ := f.Lookup("/layout"); true {
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("unknown resource fork layout: %v", err)
		}
	}
	_ = lay
}

func TestDecmpfsResourceForkBlockTableHostile(t *testing.T) {
	content := compressible(70000)
	good := rsrcZlib(content)
	cases := map[string]func(b []byte){
		"block count huge":     func(b []byte) { binary.LittleEndian.PutUint32(b[0x104:], 0xFFFFFFF0) },
		"block count wrong":    func(b []byte) { binary.LittleEndian.PutUint32(b[0x104:], 5) },
		"offset past the fork": func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 0x7FFFFFF0) },
		"size past the fork":   func(b []byte) { binary.LittleEndian.PutUint32(b[0x10C:], 0x7FFFFFF0) },
		"offset overflows":     func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 0xFFFFFFFF) },
		"offset inside table":  func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 2) },
		"zero-size block":      func(b []byte) { binary.LittleEndian.PutUint32(b[0x10C:], 0) },
		"garbage block": func(b []byte) {
			off := int(binary.LittleEndian.Uint32(b[0x108:])) + 0x104
			for i := 0; i < 8; i++ {
				b[off+i] ^= 0x5A
			}
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			rs := slices.Clone(good)
			patch(rs)
			_, _, f := buildTree(t, hfsplustest.Options{Blocks: 512}, []hfsplustest.File{{
				Path: "/h", OwnerFlags: ufCompressed, Rsrc: rs,
				Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(4, uint64(len(content)), nil)}},
			}})
			e, err := f.Lookup("/h")
			if err != nil {
				t.Fatal(err)
			}
			fl, err := f.Open(e)
			if err == nil {
				_, err = fl.ReadAt(make([]byte, fl.Size()), 0)
			}
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("err = %v; want ErrCorrupt", err)
			}
		})
	}
}

func TestResourceForkOnlyInAttrs(t *testing.T) {
	data := pattern(300, 14)
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/r.bin", Data: data, Rsrc: pattern(5000, 15)},
		{Path: "/ronly", Rsrc: pattern(10, 16)},
	})
	e, fl := openPath(t, f, "/r.bin")
	if attrOf(e, "rsrc_size") != "5000" || attrOf(e, "rsrc_blocks") != "2" {
		t.Errorf("attrs = %+v", e.Attrs)
	}
	if !bytes.Equal(readAll(t, fl), data) || e.Size != 300 {
		t.Error("Open must return the data fork only")
	}
	e, fl = openPath(t, f, "/ronly")
	if e.Size != 0 || fl.Size() != 0 || attrOf(e, "rsrc_size") != "10" {
		t.Errorf("resource-only file: size %d attrs %+v", e.Size, e.Attrs)
	}
}

func TestOtherTypesAreUnsupported(t *testing.T) {
	files := []hfsplustest.File{
		{Path: "/chr", Mode: 0o20644, Special: 0x0102},
		{Path: "/blk", Mode: 0o60644},
		{Path: "/fifo", Mode: 0o10644},
		{Path: "/sock", Mode: 0o140644},
		{Path: "/reg", Data: []byte("x")},
	}
	_, _, f := buildTree(t, hfsplustest.Options{}, files)
	root := readDir(t, f, f.Root())
	for _, n := range []string{"chr", "blk", "fifo", "sock"} {
		e := child(t, root, n)
		if e.Type != filesys.TypeOther {
			t.Errorf("%s: type %v", n, e.Type)
		}
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("%s: Open = %v", n, err)
		}
	}
	if _, err := f.Open(child(t, root, "reg")); err != nil {
		t.Errorf("regular file: %v", err)
	}
}

// attrLeafRecords lists, for the first attributes leaf, the image offset of
// each record with its fileID and name.
type attrRec struct {
	off    int64 // image offset of the record (its keyLength field)
	id     uint32
	name   string
	keyLen int
}

func attrRecords(t testing.TB, img []byte, lay *hfsplustest.Layout) []attrRec {
	t.Helper()
	if lay.AttrBlocks == 0 {
		t.Fatal("no attributes file")
	}
	ns := int64(lay.NodeSize)
	var out []attrRec
	for _, num := range lay.AttrLevels[0] {
		base := lay.AttrOffset(int64(num) * ns)
		n := img[base : base+ns]
		nrec := int(be.Uint16(n[10:]))
		for i := 0; i < nrec; i++ {
			off := int(be.Uint16(n[int(ns)-2*(i+1):]))
			kl := int(be.Uint16(n[off:]))
			nl := int(be.Uint16(n[off+12:]))
			u := make([]uint16, nl)
			for j := range u {
				u[j] = be.Uint16(n[off+14+2*j:])
			}
			out = append(out, attrRec{off: base + int64(off), id: be.Uint32(n[off+4:]), name: string(utf16.Decode(u)), keyLen: kl})
		}
	}
	return out
}

func TestAttributesTreeXattrs(t *testing.T) {
	var many []hfsplustest.Attr
	for i := range 40 {
		many = append(many, hfsplustest.Attr{Name: fmt.Sprintf("user.attr%02d", i), Value: []byte{byte(i)}})
	}
	files := []hfsplustest.File{
		{Path: "/a", Attrs: []hfsplustest.Attr{{Name: "com.apple.quarantine", Value: []byte("0001;5e;Safari;")}, {Name: "com.apple.FinderInfo", Value: make([]byte, 32)}}},
		{Path: "/many", Attrs: many},
		{Path: "/none"},
		{Path: "/exactly32", Attrs: many[:32]},
		{Path: "/dir", Dir: true, Attrs: []hfsplustest.Attr{{Name: "dir.attr", Value: []byte("d")}}},
		{Path: "/forkattr", Attrs: []hfsplustest.Attr{{Name: "big.attr", Value: make([]byte, 100000), ForkData: true}}},
	}
	img, lay, f := buildTree(t, hfsplustest.Options{}, files)
	root := readDir(t, f, f.Root())
	if got := attrValues(child(t, root, "a"), "xattr"); !slices.Equal(got, []string{"com.apple.FinderInfo", "com.apple.quarantine"}) {
		t.Errorf("a xattrs = %v (tree order: binary by name)", got)
	}
	e := child(t, root, "many")
	got := attrValues(e, "xattr")
	if len(got) != 32 || got[0] != "user.attr00" || got[31] != "user.attr31" || attrOf(e, "xattr_more") != "8" {
		t.Errorf("many: %d xattrs, more=%q (%v...)", len(got), attrOf(e, "xattr_more"), got[:min(3, len(got))])
	}
	if e := child(t, root, "exactly32"); len(attrValues(e, "xattr")) != 32 || attrOf(e, "xattr_more") != "" {
		t.Errorf("exactly32: %+v", e.Attrs)
	}
	if e := child(t, root, "none"); len(attrValues(e, "xattr")) != 0 {
		t.Errorf("none: %+v", e.Attrs)
	}
	if got := attrValues(child(t, root, "dir"), "xattr"); !slices.Equal(got, []string{"dir.attr"}) {
		t.Errorf("a folder's xattrs = %v", got)
	}
	if got := attrValues(child(t, root, "forkattr"), "xattr"); !slices.Equal(got, []string{"big.attr"}) {
		t.Errorf("a fork-data attribute contributes its name: %v", got)
	}
	// The attribute values are never loaded into entries.
	for _, e := range root {
		for _, kv := range e.Attrs {
			if kv.Key == "xattr" && len(kv.Value) > 64 {
				t.Errorf("%s: %q", e.Name, kv.Value)
			}
		}
	}
	recs := attrRecords(t, img, lay)
	if len(recs) != 2+40+32+1+1 || recs[0].id != lay.CNIDs["/a"] {
		t.Errorf("builder wrote %d attribute records", len(recs))
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestCProtectMarksEncrypted(t *testing.T) {
	data := pattern(1000, 17)
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/prot.bin", Data: data, Attrs: []hfsplustest.Attr{{Name: nameCProtect, Value: []byte{4, 0, 0, 0, 0, 0, 0, 0}}}},
		{Path: "/open.bin", Data: data},
	})
	e, fl := openPath(t, f, "/prot.bin")
	if !e.Encrypted || attrOf(e, "cprotect") != "present" {
		t.Errorf("entry = %+v", e)
	}
	if !bytes.Equal(readAll(t, fl), data) {
		t.Error("the raw content must be readable")
	}
	if e, _ := f.Lookup("/open.bin"); e.Encrypted || attrOf(e, "cprotect") != "" {
		t.Errorf("an unprotected entry: %+v", e)
	}
	if f.Info().Encrypted {
		t.Error("Info.Encrypted must stay false")
	}
}

func TestAttributesHostile(t *testing.T) {
	files := []hfsplustest.File{
		{Path: "/a", Attrs: []hfsplustest.Attr{{Name: "aaa", Value: []byte("1234")}, {Name: "bbb", Value: []byte("5678")}, {Name: "ccc", Value: []byte("9")}}},
		{Path: "/flood"},
	}
	t.Run("oversized inline record", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		r := attrRecords(t, img, lay)[1] // "bbb"
		be.PutUint32(img[r.off+2+int64((r.keyLen+1)&^1)+12:], 0xFFFFFFF0)
		f := open(t, img)
		e := child(t, readDir(t, f, f.Root()), "a")
		if got := attrValues(e, "xattr"); !slices.Equal(got, []string{"aaa", "ccc"}) {
			t.Errorf("xattrs = %v, want the record with the lying size skipped", got)
		}
		if !hasWarning(f.Info(), "attribute") {
			t.Errorf("warnings = %q", f.Info().Warnings)
		}
	})
	t.Run("name past the key", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		r := attrRecords(t, img, lay)[0]
		be.PutUint16(img[r.off+12:], 120) // 120 units do not fit the key
		f := open(t, img)
		e := child(t, readDir(t, f, f.Root()), "a")
		if got := attrValues(e, "xattr"); slices.Contains(got, "aaa") {
			t.Errorf("xattrs = %v", got)
		}
		if !hasWarning(f.Info(), "attribute") {
			t.Errorf("warnings = %q", f.Info().Warnings)
		}
	})
	t.Run("name longer than 127 units", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		r := attrRecords(t, img, lay)[0]
		be.PutUint16(img[r.off+12:], 200)
		be.PutUint16(img[r.off:], 14+2*200+2) // key length claims to hold them
		f := open(t, img)
		if _, err := f.ReadDir(f.Root()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("record too short for its type", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		r := attrRecords(t, img, lay)[2]                         // "ccc": 16+1 bytes of data
		be.PutUint32(img[r.off+2+int64((r.keyLen+1)&^1):], 0x20) // a fork-data record needs 88 bytes
		f := open(t, img)
		e := child(t, readDir(t, f, f.Root()), "a")
		if got := attrValues(e, "xattr"); slices.Contains(got, "ccc") {
			t.Errorf("xattrs = %v", got)
		}
	})
	t.Run("a run of 5000 records", func(t *testing.T) {
		var flood []hfsplustest.Attr
		for i := range 5000 {
			flood = append(flood, hfsplustest.Attr{Name: fmt.Sprintf("flood.%05d", i), Value: []byte{1}})
		}
		_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/flood", Attrs: flood}, {Path: "/other", Attrs: flood[:3]}})
		root := readDir(t, f, f.Root())
		e := child(t, root, "flood")
		if got := attrValues(e, "xattr"); len(got) != 32 || attrOf(e, "xattr_more") != strconv.Itoa(4096-32) {
			t.Errorf("%d xattrs shown, more=%q; the scan must stop after 4096 records", len(got), attrOf(e, "xattr_more"))
		}
		if !hasWarning(f.Info(), "4096") {
			t.Errorf("warnings = %q", f.Info().Warnings)
		}
		if got := attrValues(child(t, root, "other"), "xattr"); len(got) != 3 {
			t.Errorf("other: %v", got)
		}
	})
	t.Run("a damaged attributes tree", func(t *testing.T) {
		img, lay, _ := buildTree(t, hfsplustest.Options{}, files)
		img[lay.AttrOffset(8)] = 0x55 // node 0's kind
		f := open(t, img)
		root := readDir(t, f, f.Root())
		if len(root) != 2 {
			t.Errorf("listing = %v", nameList(root))
		}
		if !hasWarning(f.Info(), "attributes") {
			t.Errorf("warnings = %q", f.Info().Warnings)
		}
	})
}

var _ = hfsplus.Probe
