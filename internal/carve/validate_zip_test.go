package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func init() { implementedValidators["zip"] = true }

func zipWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

func zipVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateZIP(zipWindow(buf, newBudget(1<<30)))
}

func zipEntries(names ...string) []carvetest.ZIPEntry {
	var es []carvetest.ZIPEntry
	for i, n := range names {
		es = append(es, carvetest.ZIPEntry{Name: n, Data: bytes.Repeat([]byte{byte('a' + i)}, 40+i)})
	}
	return es
}

func TestZIPValidatesBuilderVariants(t *testing.T) {
	for name, c := range map[string]struct {
		es      []carvetest.ZIPEntry
		comment string
		want    string
	}{
		"one entry":         {zipEntries("a.txt"), "", "entries=1"},
		"three entries":     {zipEntries("a.txt", "dir/b.txt", "c"), "", "entries=3"},
		"archive comment":   {zipEntries("a.txt", "b.txt"), "an archive comment", "entries=2"},
		"empty entry data":  {[]carvetest.ZIPEntry{{Name: "e"}}, "", "entries=1"},
		"comment with PK56": {zipEntries("a.txt"), "PK\x05\x06 inside", "entries=1"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.ZIP(c.es, c.comment)
			// Fill after the object is excluded.
			v := zipVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != int64(len(obj)) || !slices.Contains(v.Basis, c.want) {
				t.Fatalf("verdict = %+v, want validated length %d with %s", v, len(obj), c.want)
			}
			if v.Type != "zip" || v.Ext != "zip" {
				t.Errorf("identity = %q/%q, want zip/zip", v.Type, v.Ext)
			}
		})
	}
}

func TestZIPIdentityFromStructure(t *testing.T) {
	for _, c := range []struct {
		name  string
		names []string
		want  string
	}{
		{"apk", []string{"AndroidManifest.xml", "classes.dex", "META-INF/MANIFEST.MF"}, "apk"},
		{"apk without a jar manifest", []string{"classes.dex", "AndroidManifest.xml"}, "apk"},
		{"only the manifest of an apk", []string{"AndroidManifest.xml"}, "zip"},
		{"only classes.dex", []string{"classes.dex"}, "zip"},
		{"jar", []string{"META-INF/MANIFEST.MF", "a/B.class"}, "jar"},
		{"docx", []string{"[Content_Types].xml", "word/document.xml"}, "docx"},
		{"xlsx", []string{"[Content_Types].xml", "xl/workbook.xml"}, "xlsx"},
		{"pptx", []string{"[Content_Types].xml", "ppt/presentation.xml"}, "pptx"},
		{"content types only", []string{"[Content_Types].xml"}, "zip"},
		{"office part without content types", []string{"word/document.xml"}, "zip"},
		{"content types with an unrelated part", []string{"[Content_Types].xml", "other/x.xml"}, "zip"},
		{"plain", []string{"readme.txt", "data/x.bin"}, "zip"},
		{"near miss names", []string{"androidmanifest.xml", "META-INF/MANIFEST.MF.bak", "word"}, "zip"},
		{"long name", []string{string(bytes.Repeat([]byte("x"), 60000))}, "zip"},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := carvetest.ZIP(zipEntries(c.names...), "")
			v := zipVerdict(obj, 64)
			if v.Outcome != Validated || v.Type != c.want || v.Ext != c.want {
				t.Fatalf("verdict = %+v, want validated %s", v, c.want)
			}
		})
	}
}

func TestZIPRejects(t *testing.T) {
	good := carvetest.ZIP(zipEntries("a.txt", "b.txt", "c.txt"), "")
	eocd := len(good) - 22
	cd := int(binary.LittleEndian.Uint32(good[eocd+16:]))
	patch := func(f func(b []byte)) []byte { b := slices.Clone(good); f(b); return b }
	put16 := func(b []byte, at int, v uint16) { binary.LittleEndian.PutUint16(b[at:], v) }
	put32 := func(b []byte, at int, v uint32) { binary.LittleEndian.PutUint32(b[at:], v) }
	zip64Locator := slices.Concat([]byte("PK\x06\x07"), make([]byte, 16))
	withComment := carvetest.ZIP(zipEntries("a.txt"), "0123456789")
	// An archive whose directory is empty (no entries claimed, C == P).
	emptyDir := slices.Concat(good[:cd], []byte("PK\x05\x06"), make([]byte, 8),
		binary.LittleEndian.AppendUint32(nil, 0), binary.LittleEndian.AppendUint32(nil, uint32(cd)), []byte{0, 0})
	// A local-header offset past the directory that does hold a local-header
	// signature: the first entry points into the archive comment.
	const trap = "PK\x03\x04 trap"
	commentOffset := carvetest.ZIP(zipEntries("a.txt"), trap)
	cd1 := int(binary.LittleEndian.Uint32(commentOffset[len(commentOffset)-len(trap)-22+16:]))
	put32(commentOffset, cd1+42, uint32(len(commentOffset)-len(trap)))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"C + S != P (size)", patch(func(b []byte) { put32(b, eocd+12, 1) }), "no-eocd"},
		{"C + S != P (offset)", patch(func(b []byte) { put32(b, eocd+16, uint32(cd)-1) }), "no-eocd"},
		{"C < 30", patch(func(b []byte) { put32(b, eocd+16, 29) }), "no-eocd"},
		{"local offset on data", patch(func(b []byte) { put32(b, cd+42, 40) }), "no-eocd"},
		{"local offset at C", patch(func(b []byte) { put32(b, cd+42, uint32(cd)) }), "no-eocd"},
		{"local offset past C", patch(func(b []byte) { put32(b, cd+42, uint32(cd)+9) }), "no-eocd"},
		{"second local offset on data", patch(func(b []byte) { put32(b, cd+46+5+42, 50) }), "no-eocd"},
		{"bad central header signature", patch(func(b []byte) { b[cd+3] = 9 }), "no-eocd"},
		{"zip64 total entries", patch(func(b []byte) { put16(b, eocd+10, 0xFFFF); put16(b, eocd+8, 0xFFFF) }), "zip64"},
		{"zip64 cd size", patch(func(b []byte) { put32(b, eocd+12, 0xFFFFFFFF) }), "zip64"},
		{"zip64 cd offset", patch(func(b []byte) { put32(b, eocd+16, 0xFFFFFFFF) }), "zip64"},
		{"zip64 locator before the EOCD", slices.Concat(good[:eocd], zip64Locator, good[eocd:]), "zip64"},
		{"disk number 1", patch(func(b []byte) { put16(b, eocd+4, 1) }), "multi-disk"},
		{"cd disk 1", patch(func(b []byte) { put16(b, eocd+6, 1) }), "multi-disk"},
		{"entries on this disk differ", patch(func(b []byte) { put16(b, eocd+8, 2) }), "no-eocd"},
		{"count too high (walk overshoots)", patch(func(b []byte) { put16(b, eocd+8, 4); put16(b, eocd+10, 4) }), "no-eocd"},
		{"count too low (walk stops early)", patch(func(b []byte) { put16(b, eocd+8, 2); put16(b, eocd+10, 2) }), "no-eocd"},
		{"zero entries", patch(func(b []byte) { put16(b, eocd+8, 0); put16(b, eocd+10, 0) }), "no-eocd"},
		{"empty directory", emptyDir, "no-eocd"},
		{"local offset in the archive comment", commentOffset, "no-eocd"},
		{"name length overshoots", patch(func(b []byte) { put16(b, cd+28, 60000) }), "no-eocd"},
		{"extra length overshoots", patch(func(b []byte) { put16(b, cd+30, 60000) }), "no-eocd"},
		{"comment length overshoots", patch(func(b []byte) { put16(b, cd+32, 60000) }), "no-eocd"},
		{"no EOCD", good[:eocd], "no-eocd"},
		{"EOCD cut short", good[:len(good)-5], "no-eocd"},
		{"comment cut short", withComment[:len(withComment)-3], "truncated"},
		{"bad signature", patch(func(b []byte) { b[2] = 9 }), "bad-signature"},
		{"empty", nil, "truncated"},
		{"three bytes", []byte("PK\x03"), "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateZIP(zipWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := zipVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestZIPInconsistentCandidateIsSkipped: a PK\5\6 inside an entry (a stored
// archive inside the archive) whose offsets do not hold is skipped and the
// search goes on to the real record.
func TestZIPInconsistentCandidateIsSkipped(t *testing.T) {
	inner := carvetest.ZIP(zipEntries("in.txt"), "")
	outer := carvetest.ZIP([]carvetest.ZIPEntry{{Name: "inner.zip", Data: inner}, {Name: "x", Data: []byte("x")}}, "")
	v := zipVerdict(outer, 100)
	if v.Outcome != Validated || v.Length != int64(len(outer)) || !slices.Contains(v.Basis, "entries=2") {
		t.Fatalf("verdict = %+v, want the outer archive of length %d", v, len(outer))
	}
}

// TestZIPNeverInflates: the deflated entry of the real archive is never
// interpreted: corrupting its bytes changes nothing, no read leaves the
// window, and the media is read once for the search plus the headers and name
// prefixes the walk re-reads (a forward search for the end record has to
// pass over the entry data, so "read" here means "searched", never decoded).
func TestZIPNeverInflates(t *testing.T) {
	obj, err := os.ReadFile(filepath.Join("testdata", "objects", "zip-mixed.zip"))
	if err != nil {
		t.Fatal(err)
	}
	// The deflated payload follows the second local header (30 + name).
	hdr2 := bytes.Index(obj[4:], []byte("PK\x03\x04")) + 4
	nameLen := int(binary.LittleEndian.Uint16(obj[hdr2+26:]))
	csize := int(binary.LittleEndian.Uint32(obj[hdr2+18:]))
	if csize < 1 || binary.LittleEndian.Uint16(obj[hdr2+8:]) != 8 {
		t.Fatalf("fixture: second entry is not deflated (method %d, csize %d)", binary.LittleEndian.Uint16(obj[hdr2+8:]), csize)
	}
	data := hdr2 + 30 + nameLen
	corrupt := slices.Clone(obj)
	for i := data; i < data+csize; i++ {
		corrupt[i] = 0xFF
	}
	for _, o := range [][]byte{obj, corrupt} {
		cr := &countReader{data: o}
		w := newWindow(context.Background(), cr, 0, int64(len(o)), newBudget(1<<30))
		v := validateZIP(w)
		if v.Outcome != Validated || v.Length != int64(len(o)) || w.outsideReads() != 0 {
			t.Fatalf("verdict = %+v outside = %d", v, w.outsideReads())
		}
		// One pass for the search, then 46 + a 32-byte name prefix and 4 bytes
		// of local header per entry, plus the EOCD.
		if limit := int64(len(o)) + 2*(46+32+4) + 22; cr.total > limit {
			t.Errorf("read %d bytes of a %d-byte archive, limit %d", cr.total, len(o), limit)
		}
	}
}

type countReader struct {
	data  []byte
	total int64
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[off:])
	c.total += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestZIPWindowBoundsAndBudget: a search over hostile data stays inside the
// window, a search that outruns the budget and a cancelled context are
// rejected "unreadable" (ruling 3E-C1), and a claimed 65535-entry directory
// allocates nothing from the count.
func TestZIPWindowBoundsAndBudget(t *testing.T) {
	good := carvetest.ZIP(zipEntries("a.txt"), "")
	buf := slices.Concat([]byte("PK\x03\x04"), bytes.Repeat([]byte("PK\x05\x06"), 1<<18))
	if v := validateZIP(zipWindow(buf, newBudget(1<<30))); v.Outcome != Rejected || (v.Reason != "multi-disk" && v.Reason != "no-eocd") {
		t.Fatalf("hostile verdict = %+v", v)
	}
	b := newBudget(128 << 10)
	v := validateZIP(zipWindow(buf, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(good), 0, int64(len(good)), newBudget(1<<30))
	if v := validateZIP(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
	cd := bytes.Repeat(append([]byte("PK\x01\x02"), make([]byte, 42)...), 3)
	eo := []byte("PK\x05\x06\x00\x00\x00\x00\xff\xff\xfe\xff")
	eo = binary.LittleEndian.AppendUint32(eo, uint32(len(cd)))
	eo = binary.LittleEndian.AppendUint32(eo, 30)
	eo = append(eo, 0, 0)
	hostile := slices.Concat([]byte("PK\x03\x04"), make([]byte, 26), cd, eo)
	if v := validateZIP(zipWindow(hostile, newBudget(1<<30))); v.Outcome != Rejected {
		t.Fatalf("claimed-count verdict = %+v", v)
	}
}

// TestZIPReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// signature, in the search, in the EOCD, in the central directory and at a
// local header is rejected "unreadable" and recorded.
func TestZIPReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.ZIP(zipEntries("a.txt", "b.txt", "c.txt"), "")
	eocd := int64(len(obj) - 22)
	cd := int64(binary.LittleEndian.Uint32(obj[eocd+16:]))
	big := carvetest.ZIP([]carvetest.ZIPEntry{{Name: "big", Data: make([]byte, 200000)}}, "")
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at signature", obj, 0},
		{"mid search", big, 70000},
		{"in the EOCD", obj, eocd + 10},
		{"in the central directory", obj, cd + 50},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateZIP(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
				t.Fatalf("verdict = %+v readErr = %v, want rejected unreadable", v, w.readErr)
			}
		})
	}
}

func TestZIPRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family, Type string
			Size               int64
			Facts              struct{ Namelist []string }
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range exp.Objects {
		if o.Family != "zip" {
			continue
		}
		seen[o.Type] = true
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := zipVerdict(obj, 4096)
			want := "entries=" + strconv.Itoa(len(o.Facts.Namelist))
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size ||
				v.Type != o.Type || v.Ext != o.Type || !slices.Contains(v.Basis, want) || len(o.Facts.Namelist) == 0 {
				t.Fatalf("verdict = %+v, want validated length %d type %s with %s", v, o.Size, o.Type, want)
			}
		})
	}
	for _, ty := range []string{"zip", "apk", "docx"} {
		if !seen[ty] {
			t.Errorf("no %s object in the expectations", ty)
		}
	}
}
