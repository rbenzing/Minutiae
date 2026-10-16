package hfsplus_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/decmpfs"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

var _ filesys.XattrReader = (*hfsplus.FS)(nil)

func decmpfsVal(typ uint32, size uint64, payload []byte) []byte {
	v := make([]byte, 16, 16+len(payload))
	copy(v, "fpmc")
	binary.LittleEndian.PutUint32(v[4:], typ)
	binary.LittleEndian.PutUint64(v[8:], size)
	return append(v, payload...)
}

const (
	attrDecmpfs    = "com.apple.decmpfs"
	attrRsrc       = "com.apple.ResourceFork"
	flagCompressed = 0x20
)

// An unsupported compression type loses nothing: the typed error names the
// type and the attribute and resource-fork bytes are still returned.
func TestUnsupportedCompressionKeepsRawBytesReachable(t *testing.T) {
	rsrc := bytes.Repeat([]byte{0xAB}, 300)
	attr := decmpfsVal(7, 1234, []byte("lzvn-payload"))
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{
		Path: "/lzvn", OwnerFlags: flagCompressed, Rsrc: rsrc,
		Attrs: []hfsplustest.Attr{{Name: attrDecmpfs, Value: attr}},
	}})
	e := lookupEntry(t, f, "/lzvn")
	fl, err := f.Open(e)
	var ue *decmpfs.UnsupportedError
	if !errors.As(err, &ue) || ue.Type != 7 || fl != nil || !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("Open = %v, %v; want *decmpfs.UnsupportedError{Type: 7}", fl, err)
	}
	xs, err := f.Xattrs(e)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]filesys.Xattr{}
	for _, x := range xs {
		got[x.Name] = x
	}
	if x := got[attrDecmpfs]; x.Size != int64(len(attr)) || x.Storage != filesys.XattrInline {
		t.Errorf("decmpfs attribute listed as %+v", x)
	}
	if x := got[attrRsrc]; x.Size != int64(len(rsrc)) || x.Storage != filesys.XattrFork {
		t.Errorf("resource fork listed as %+v", x)
	}
	raw, err := f.OpenXattr(e, attrDecmpfs)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(io.NewSectionReader(raw, 0, raw.Size())); !bytes.Equal(b, attr) {
		t.Errorf("raw decmpfs attribute = %x", b)
	}
	rf, err := f.OpenXattr(e, attrRsrc)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(io.NewSectionReader(rf, 0, rf.Size())); !bytes.Equal(b, rsrc) {
		t.Errorf("raw resource fork differs (%d bytes)", len(b))
	}
	if len(rf.Runs()) == 0 {
		t.Error("the resource fork lies in image blocks and must report their runs")
	}
	if err := filesys.CheckRuns(rf.Runs(), rf.Size(), f.Info().Size); err != nil {
		t.Errorf("runs: %v", err)
	}
}

func TestOpenXattrNamesAndLimits(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a", Data: []byte("a"), Attrs: []hfsplustest.Attr{{Name: "com.example.other", Value: []byte("v")}}},
		{Path: "/plain", Data: []byte("p")},
	})
	a, plain := lookupEntry(t, f, "/a"), lookupEntry(t, f, "/plain")
	if _, err := f.OpenXattr(a, "com.example.other"); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("a listed attribute whose value is not extracted: %v, want ErrUnsupported (never an empty value)", err)
	}
	if _, err := f.OpenXattr(a, "com.example.nothing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an unknown attribute: %v, want ErrNotFound", err)
	}
	if _, err := f.OpenXattr(plain, attrDecmpfs); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("no decmpfs attribute: %v, want ErrNotFound", err)
	}
	if _, err := f.OpenXattr(plain, attrRsrc); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("no resource fork: %v, want ErrNotFound", err)
	}
	xs, err := f.Xattrs(a)
	if err != nil || len(xs) != 1 || xs[0].Name != "com.example.other" || xs[0].Size != -1 {
		t.Errorf("Xattrs = %+v, %v; want one attribute of unknown size", xs, err)
	}
	if _, err := f.Xattrs(filesys.Entry{ID: "cnid:0x10"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a non-canonical id: %v, want ErrNotFound", err)
	}
}

func TestXattrsOverTheNameCapIsErrXattrCap(t *testing.T) {
	var attrs []hfsplustest.Attr
	for i := range 40 { // maxAttrShown is 32
		attrs = append(attrs, hfsplustest.Attr{Name: "com.example.n" + string(rune('A'+i%26)) + string(rune('a'+i/26)), Value: []byte("v")})
	}
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/many", Data: []byte("m"), Attrs: attrs}})
	if xs, err := f.Xattrs(lookupEntry(t, f, "/many")); !errors.Is(err, filesys.ErrXattrCap) || xs != nil {
		t.Errorf("Xattrs = %+v, %v; want ErrXattrCap and no partial list", xs, err)
	}
}

// A damaged or short resource fork is an error through XattrReader, never a prefix.
func TestOpenXattrResourceForkIsAllOrNothing(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/r", Data: []byte("d"), Rsrc: bytes.Repeat([]byte{7}, 3*4096)},
	})
	// The catalog claims a larger resource fork than its extents map.
	p := findRec(t, img, lay, 2, units("r"))
	be.PutUint64(img[p.data+168:], 5*4096) // HFSPlusForkData.logicalSize of the resource fork
	f := open(t, img)
	e := lookupEntry(t, f, "/r")
	fl, err := f.OpenXattr(e, attrRsrc)
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("a short resource fork must not be offered as a prefix")
	}
	if xs, lerr := f.Xattrs(e); lerr != nil {
		t.Fatalf("Xattrs: %v", lerr)
	} else if len(xs) == 0 || xs[len(xs)-1].Name != attrRsrc {
		t.Errorf("the damaged fork must still be listed: %+v", xs)
	}
}

// A damaged attribute record is never reported as an absent attribute.
func TestXattrsDamagedAttributeRecordIsAnError(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a", Data: []byte("a"), Attrs: []hfsplustest.Attr{{Name: "com.example.one", Value: []byte("1")}}},
	})
	f := open(t, img)
	e := lookupEntry(t, f, "/a")
	if xs, err := f.Xattrs(e); err != nil || len(xs) != 1 {
		t.Fatalf("control: %+v, %v", xs, err)
	}
	// Make the attribute record's type unknown (the first u32 of its data).
	// The record's data follows its key, which ends with the UTF-16BE name; the
	// bare type bytes would also match the key's file id (16).
	region := img[lay.AttrOffset(0):]
	nm := bytes.Index(region, utf16BE("com.example.one"))
	if nm < 0 {
		t.Fatal("cannot locate the attribute name in the builder image: a skipped damage test would be vacuous")
	}
	at := bytes.Index(region[nm:], []byte{0, 0, 0, 0x10}) // kHFSPlusAttrInlineData
	if at < 0 {
		t.Fatal("cannot locate the inline attribute record in the builder image: a skipped damage test would be vacuous")
	}
	region[nm+at+3] = 0x7F
	g := open(t, img)
	if xs, err := g.Xattrs(lookupEntry(t, g, "/a")); !errors.Is(err, filesys.ErrCorrupt) || xs != nil {
		t.Errorf("Xattrs with a damaged attribute record = %+v, %v; want ErrCorrupt and no list", xs, err)
	}
}

func utf16BE(s string) []byte {
	var b []byte
	for _, u := range units(s) {
		b = append(b, byte(u>>8), byte(u))
	}
	return b
}

// A record that sits under another file id is not this file's attribute: no
// record exists under this id, so the list is empty and the error nil. Damage
// the reader can detect (the record type above) is ErrCorrupt instead; the two
// must not be confused with each other.
func TestXattrsRecordMovedToAnotherIDGivesAnEmptyList(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a", Data: []byte("a"), Attrs: []hfsplustest.Attr{{Name: "com.example.one", Value: []byte("1")}}},
	})
	region := img[lay.AttrOffset(0):]
	nm := bytes.Index(region, utf16BE("com.example.one"))
	if nm < 10 {
		t.Fatal("cannot locate the attribute name in the builder image: a skipped damage test would be vacuous")
	}
	// Key: keyLength(2) pad(2) fileID(4) startBlock(4) nameLength(2) name.
	idAt := nm - 2 - 4 - 4
	id := be.Uint32(region[idAt:])
	if id != lay.CNIDs["/a"] {
		t.Fatalf("the located key id is %d, want %d", id, lay.CNIDs["/a"])
	}
	be.PutUint32(region[idAt:], id+1000)
	f := open(t, img)
	xs, err := f.Xattrs(lookupEntry(t, f, "/a"))
	if err != nil || len(xs) != 0 {
		t.Errorf("Xattrs with the record under another id = %+v, %v; want an empty list and no error", xs, err)
	}
}

// A listed com.apple.decmpfs attribute whose value is not held in memory (a
// fork-data attribute) has an unknown size, and its raw value is refused, never
// presented as empty.
func TestDecmpfsAttributeWithNoValueIsUnknown(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{
		Path: "/fork", Data: []byte("d"),
		Attrs: []hfsplustest.Attr{{Name: attrDecmpfs, Value: bytes.Repeat([]byte{1}, 40), ForkData: true}},
	}})
	e := lookupEntry(t, f, "/fork")
	xs, err := f.Xattrs(e)
	if err != nil || len(xs) != 1 || xs[0].Name != attrDecmpfs || xs[0].Size != -1 {
		t.Fatalf("Xattrs = %+v, %v; want the decmpfs attribute with Size -1", xs, err)
	}
	if fl, err := f.OpenXattr(e, attrDecmpfs); !errors.Is(err, filesys.ErrUnsupported) || fl != nil {
		t.Errorf("OpenXattr = %v, %v; want ErrUnsupported", fl, err)
	}
}
