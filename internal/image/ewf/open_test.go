package ewf_test

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // MD5 is a stored value of the format, not a security control
	"crypto/sha1" //nolint:gosec // SHA-1 is a stored value of the format, not a security control
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// pattern returns n deterministic, non-repeating-looking bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	x := uint32(12345)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func segsOf(files [][]byte) []ewf.Segment {
	out := make([]ewf.Segment, len(files))
	for i, f := range files {
		out[i] = ewf.Segment{Name: fmt.Sprintf("img.E%02d", i+1), R: bytes.NewReader(f), Size: int64(len(f))}
	}
	return out
}

// openWithin opens files and fails when Open takes longer than 5 s (a walk
// loop would hang here).
func openWithin(t *testing.T, files [][]byte) (*ewf.Reader, error) {
	t.Helper()
	type res struct {
		r   *ewf.Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := ewf.Open(segsOf(files))
		ch <- res{r, err}
	}()
	select {
	case v := <-ch:
		return v.r, v.err
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not return within 5s")
		return nil, nil
	}
}

func mustOpen(t *testing.T, files [][]byte) *ewf.Reader {
	t.Helper()
	r, err := openWithin(t, files)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

func openErr(t *testing.T, files [][]byte) error {
	t.Helper()
	_, err := openWithin(t, files)
	return err
}

func meta(r *ewf.Reader) map[string]string {
	m := map[string]string{}
	for _, kv := range r.Metadata() {
		m[kv.Key] = kv.Value
	}
	return m
}

func keys(r *ewf.Reader) []string {
	var k []string
	for _, kv := range r.Metadata() {
		k = append(k, kv.Key)
	}
	return k
}

func section(t *testing.T, seg []byte, typ string) ewftest.Section {
	t.Helper()
	for _, s := range ewftest.Sections(seg) {
		if s.Type == typ {
			return s
		}
	}
	t.Fatalf("no %q section", typ)
	return ewftest.Section{}
}

func wantCorrupt(t *testing.T, err error) {
	t.Helper()
	_ = asCorrupt(t, err)
}

func asCorrupt(t *testing.T, err error) *ewf.CorruptError {
	t.Helper()
	if err == nil {
		t.Fatal("Open succeeded, want a CorruptError")
	}
	var ce *ewf.CorruptError
	if !errors.As(err, &ce) || !errors.Is(err, ewf.ErrCorrupt) {
		t.Fatalf("error %v is not a CorruptError", err)
	}
	return ce
}

func hasWarning(r *ewf.Reader, sub string) bool {
	return slices.ContainsFunc(r.Warnings(), func(w string) bool { return strings.Contains(w, sub) })
}

func TestOpenMinimalImage(t *testing.T) {
	m := pattern(3 * 64 * 512)
	files := ewftest.Build(ewftest.Options{
		Case: "C-1", Evidence: "E-1", Description: "a disk", Examiner: "Ex", Notes: "some notes",
		Acquired: "1700000000",
	}, m)
	r := mustOpen(t, files)
	if r.Size() != int64(len(m)) || r.SectorSize() != 512 || r.ChunkSize() != 32768 || r.Chunks() != 3 {
		t.Fatalf("Size %d SectorSize %d ChunkSize %d Chunks %d", r.Size(), r.SectorSize(), r.ChunkSize(), r.Chunks())
	}
	if w := r.Warnings(); len(w) != 0 {
		t.Fatalf("warnings: %q", w)
	}
	sum5, sum1 := md5.Sum(m), sha1.Sum(m) //nolint:gosec // format-stored hashes
	want := []ewf.KV{
		{Key: "format", Value: "EWF v1"},
		{Key: "segments", Value: "1"},
		{Key: "segment.1", Value: "img.E01"},
		{Key: "case_number", Value: "C-1"},
		{Key: "evidence_number", Value: "E-1"},
		{Key: "description", Value: "a disk"},
		{Key: "examiner", Value: "Ex"},
		{Key: "notes", Value: "some notes"},
		{Key: "acquired", Value: "1700000000"},
		{Key: "system_date", Value: "1700000000"},
		{Key: "compression", Value: "none"},
		{Key: "media", Value: "fixed"},
		{Key: "sectors_per_chunk", Value: "64"},
		{Key: "bytes_per_sector", Value: "512"},
		{Key: "sector_count", Value: "192"},
		{Key: "chunks", Value: "3"},
		{Key: "md5", Value: hex.EncodeToString(sum5[:])},
		{Key: "sha1", Value: hex.EncodeToString(sum1[:])},
		{Key: "header.av", Value: "20140816"},
		{Key: "header.ov", Value: "Linux"},
		{Key: "header.p", Value: "0"},
	}
	if got := r.Metadata(); !slices.Equal(got, want) {
		t.Fatalf("Metadata:\n got %q\nwant %q", got, want)
	}
	// Metadata returns a copy.
	r.Metadata()[0].Value = "x"
	if r.Metadata()[0].Value != "EWF v1" {
		t.Fatal("Metadata aliases internal state")
	}
	if err := r.Close(); err != nil || r.Close() != nil || !r.Closed() {
		t.Fatal("Close must be idempotent")
	}
}

func TestOpenMultiSegmentMetadata(t *testing.T) {
	m := pattern(7 * 64 * 512)
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 3, Compress: ewftest.CompressAll}, m)
	if len(files) != 3 {
		t.Fatalf("%d segments", len(files))
	}
	r := mustOpen(t, files)
	if got := meta(r); got["segments"] != "3" || got["segment.3"] != "img.E03" || got["compression"] != "best" {
		t.Fatalf("metadata %v", got)
	}
	if len(r.Warnings()) != 0 {
		t.Fatalf("warnings %q", r.Warnings())
	}
	kinds := r.SectionKinds()
	if kinds[0][len(kinds[0])-1] != "next" || kinds[2][len(kinds[2])-1] != "done" {
		t.Fatalf("terminal sections %v", kinds)
	}
}

func TestOpenHeaderMetadata(t *testing.T) {
	m := pattern(2 * 64 * 512)
	t.Run("header only", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader2: true, Case: "H1", Examiner: "Hex"}, m))
		if g := meta(r); g["case_number"] != "H1" || g["examiner"] != "Hex" {
			t.Fatalf("%v", g)
		}
	})
	t.Run("header2 only, non-ASCII", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader: true, Case: "H2", Examiner: "Zoë Müller \U0001F600"}, m))
		if g := meta(r); g["case_number"] != "H2" || g["examiner"] != "Zoë Müller \U0001F600" {
			t.Fatalf("%v", g)
		}
	})
	t.Run("header2 wins per field", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{
			HeaderText:  "1\nmain\nc\tn\tzz\nold-case\told-ev\tx\n\n",
			Header2Text: "1\nmain\nc\tn\tzz\nnew-case\t\ty\n\n",
		}, m))
		g := meta(r)
		// c comes from header2; n is empty in header2 so header's value survives.
		if g["case_number"] != "new-case" || g["evidence_number"] != "old-ev" {
			t.Fatalf("%v", g)
		}
		if g["header.zz"] != "y" {
			t.Fatalf("header.zz = %q", g["header.zz"])
		}
	})
	t.Run("UTF-16LE without BOM", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader: true, Header2NoBOM: true, Case: "NOBOM"}, m))
		if g := meta(r); g["case_number"] != "NOBOM" {
			t.Fatalf("%v", g)
		}
	})
	t.Run("CRLF line ends", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader2: true, HeaderText: "1\r\nmain\r\nc\tn\r\nCRLF\t7\r\n\r\n"}, m))
		if g := meta(r); g["case_number"] != "CRLF" || g["evidence_number"] != "7" {
			t.Fatalf("%v", g)
		}
	})
	t.Run("garbage zlib", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{NoHeader2: true, Case: "G"}, m)
		h := section(t, files[0], "header")
		for i := int64(0); i < 8; i++ {
			files[0][h.Offset+76+i] = 0xEE
		}
		r := mustOpen(t, files)
		if !hasWarning(r, "header section") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		if _, ok := meta(r)["case_number"]; ok {
			t.Fatal("case_number from an undecodable header")
		}
		if meta(r)["chunks"] != "2" {
			t.Fatal("geometry must still be reported")
		}
	})
	t.Run("inflate bomb", func(t *testing.T) {
		text := "1\nmain\nc\n" + strings.Repeat("A", 2<<20) + "\n\n"
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader2: true, HeaderText: text}, m))
		if !hasWarning(r, "exceeds") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		if _, ok := meta(r)["case_number"]; ok {
			t.Fatal("bomb text must be ignored")
		}
	})
	t.Run("no main category", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHeader2: true, HeaderText: "nothing here\n"}, m))
		if !hasWarning(r, "no main category") {
			t.Fatalf("warnings %q", r.Warnings())
		}
	})
	t.Run("values are length capped", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{Case: strings.Repeat("é", 400)}, m))
		v := meta(r)["case_number"]
		if len(v) > ewf.MaxMetaValue+3 || !strings.HasSuffix(v, "...") || strings.ContainsRune(strings.TrimSuffix(v, "..."), '�') {
			t.Fatalf("value %d bytes: %q", len(v), v)
		}
	})
	t.Run("repeated header that decodes differently warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{NoHeader2: true, Case: "FIRST"}, m)
		d := section(t, files[0], "data") // payload is not a zlib stream
		d.Type = "header"
		ewftest.FixDescriptor(files[0], d)
		r := mustOpen(t, files)
		if !hasWarning(r, "header section") || meta(r)["case_number"] != "FIRST" {
			t.Fatalf("%q %v", r.Warnings(), meta(r))
		}
	})
}

func TestOpenStoredHashes(t *testing.T) {
	m := pattern(2 * 64 * 512)
	s5, s1 := md5.Sum(m), sha1.Sum(m) //nolint:gosec // format-stored hashes
	md5hex, sha1hex := hex.EncodeToString(s5[:]), hex.EncodeToString(s1[:])

	t.Run("hash only", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoDigest: true}, m))
		g := meta(r)
		if g["md5"] != md5hex {
			t.Fatalf("md5 %q", g["md5"])
		}
		if _, ok := g["sha1"]; ok {
			t.Fatal("sha1 from a hash-only image")
		}
		if len(r.Warnings()) != 0 {
			t.Fatal(r.Warnings())
		}
	})
	t.Run("digest only", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHash: true}, m))
		if g := meta(r); g["md5"] != md5hex || g["sha1"] != sha1hex {
			t.Fatalf("%v", g)
		}
	})
	t.Run("both", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{}, m))
		if g := meta(r); g["md5"] != md5hex || g["sha1"] != sha1hex {
			t.Fatalf("%v", g)
		}
		if len(r.Warnings()) != 0 {
			t.Fatal(r.Warnings())
		}
	})
	t.Run("neither", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{NoHash: true, NoDigest: true}, m))
		g := meta(r)
		if _, ok := g["md5"]; ok {
			t.Fatal("md5")
		}
		if _, ok := g["sha1"]; ok {
			t.Fatal("sha1")
		}
	})
	t.Run("overrides are reported as stored", func(t *testing.T) {
		fake := bytes.Repeat([]byte{0xAB}, 16)
		r := mustOpen(t, ewftest.Build(ewftest.Options{MD5Override: fake}, m))
		if meta(r)["md5"] != hex.EncodeToString(fake) {
			t.Fatalf("%v", meta(r))
		}
	})
	t.Run("bad hash checksum is ignored with a warning", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{NoDigest: true}, m)
		h := section(t, files[0], "hash")
		files[0][h.Offset+76] ^= 0xFF
		r := mustOpen(t, files)
		if _, ok := meta(r)["md5"]; ok || !hasWarning(r, "hash section checksum") {
			t.Fatalf("md5 %v warnings %q", meta(r)["md5"], r.Warnings())
		}
	})
	t.Run("bad digest checksum is ignored with a warning", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{NoHash: true}, m)
		d := section(t, files[0], "digest")
		files[0][d.Offset+76+20] ^= 0xFF
		r := mustOpen(t, files)
		g := meta(r)
		if !hasWarning(r, "digest section checksum") {
			t.Fatalf("%v %q", g, r.Warnings())
		}
		if _, ok := g["sha1"]; ok {
			t.Fatal("sha1 must be ignored")
		}
		if _, ok := g["md5"]; ok {
			t.Fatal("md5 must be ignored")
		}
	})
	t.Run("disagreeing md5 warns and digest wins", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		h := section(t, files[0], "hash")
		files[0][h.Offset+76] ^= 0xFF
		ewftest.FixAdler(files[0], int(h.Offset)+76, int(h.Offset)+76+32, int(h.Offset)+76+32)
		r := mustOpen(t, files)
		if meta(r)["md5"] != md5hex || !hasWarning(r, "different MD5") {
			t.Fatalf("%v %q", meta(r)["md5"], r.Warnings())
		}
	})
}

func TestOpenError2Ranges(t *testing.T) {
	m := pattern(2 * 64 * 512)
	t.Run("parsed", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}, {100, 1}}}, m))
		if got := meta(r)["acquisition_errors"]; got != "2 (10+5, 100+1)" {
			t.Fatalf("acquisition_errors %q", got)
		}
		count, kept := r.ErrorRanges()
		if count != 2 || !slices.Equal(kept, [][2]uint32{{10, 5}, {100, 1}}) {
			t.Fatalf("%d %v", count, kept)
		}
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "unreadable at acquisition") || !strings.Contains(w[0], "6 sectors") {
			t.Fatalf("warnings %q", w)
		}
	})
	t.Run("count capped", func(t *testing.T) {
		n := ewf.MaxErrorRanges + 1000
		ranges := make([][2]uint32, n)
		for i := range ranges {
			ranges[i] = [2]uint32{uint32(i), 1}
		}
		r := mustOpen(t, ewftest.Build(ewftest.Options{ErrorRanges: ranges}, m))
		count, kept := r.ErrorRanges()
		if count != uint64(n) || len(kept) != ewf.MaxErrorRanges {
			t.Fatalf("count %d kept %d", count, len(kept))
		}
		if got := meta(r)["acquisition_errors"]; !strings.HasPrefix(got, fmt.Sprint(n, " (0+1, 1+1")) || !strings.HasSuffix(got, ", ...)") {
			t.Fatalf("acquisition_errors %q", got)
		}
		if w := r.Warnings(); len(w) != 1 {
			t.Fatalf("warnings %d", len(w))
		}
	})
	t.Run("damaged entries are ignored with a warning", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, m)
		e := section(t, files[0], "error2")
		files[0][e.Offset+76+520] ^= 0xFF
		r := mustOpen(t, files)
		if _, ok := meta(r)["acquisition_errors"]; ok || !hasWarning(r, "error2 entries checksum") {
			t.Fatalf("%v %q", meta(r), r.Warnings())
		}
	})
	t.Run("damaged header is ignored with a warning", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, m)
		e := section(t, files[0], "error2")
		files[0][e.Offset+76+3] ^= 0xFF
		r := mustOpen(t, files)
		if _, ok := meta(r)["acquisition_errors"]; ok || !hasWarning(r, "error2 section header checksum") {
			t.Fatalf("%v %q", meta(r), r.Warnings())
		}
	})
	t.Run("hostile count", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, m)
		e := section(t, files[0], "error2")
		ewftest.SetU32(files[0], int(e.Offset)+76, 0xFFFFFFFF)
		ewftest.FixAdler(files[0], int(e.Offset)+76, int(e.Offset)+76+516, int(e.Offset)+76+516)
		r := mustOpen(t, files)
		if !hasWarning(r, "holds room for") {
			t.Fatalf("%q", r.Warnings())
		}
	})
}

func twoSegments(t *testing.T) [][]byte {
	t.Helper()
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 2}, pattern(4*64*512))
	if len(files) != 2 {
		t.Fatalf("%d segments", len(files))
	}
	return files
}

func TestSegmentNumberMismatch(t *testing.T) {
	files := twoSegments(t)
	files[1][9] = 5
	ce := asCorrupt(t, openErr(t, files))
	if ce.Segment != 2 || !strings.Contains(ce.Reason, "number 5") || !strings.Contains(ce.Reason, "expected 2") {
		t.Fatalf("%v", ce)
	}
}

func TestSegmentsOutOfOrder(t *testing.T) {
	files := twoSegments(t)
	files[0], files[1] = files[1], files[0]
	ce := asCorrupt(t, openErr(t, files))
	if ce.Segment != 1 || !strings.Contains(ce.Reason, "number 2") || !strings.Contains(ce.Reason, "expected 1") {
		t.Fatalf("%v", ce)
	}
}

func TestMissingMiddleSegment(t *testing.T) {
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 1}, pattern(3*64*512))
	if len(files) != 3 {
		t.Fatalf("%d segments", len(files))
	}
	ce := asCorrupt(t, openErr(t, [][]byte{files[0], files[2]}))
	if ce.Segment != 2 || !strings.Contains(ce.Reason, "number 3") || !strings.Contains(ce.Reason, "expected 2") {
		t.Fatalf("%v", ce)
	}
}

func TestDoneInNonLastSegment(t *testing.T) {
	single := ewftest.Build(ewftest.Options{}, pattern(64*512))[0]
	second := bytes.Clone(single)
	second[9] = 2
	ce := asCorrupt(t, openErr(t, [][]byte{single, second}))
	if ce.Segment != 1 || ce.Section != "done" {
		t.Fatalf("%v", ce)
	}
}

func TestNextInLastSegmentIsIncomplete(t *testing.T) {
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 1}, pattern(3*64*512))
	r := mustOpen(t, files[:2]) // the last segment is missing
	w := r.Warnings()
	if len(w) != 2 || w[0] != "E01 set incomplete (no done section)" || !strings.Contains(w[1], "covers 2 of 3 chunks") {
		t.Fatalf("warnings %q", w)
	}
}

func TestNoDoneIsIncomplete(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{NoDone: true}, pattern(2*64*512)))
	w := r.Warnings()
	if len(w) != 1 || w[0] != "E01 set incomplete (no done section)" {
		t.Fatalf("warnings %q", w)
	}
}

func TestChainEndingAtEOFWithoutTerminal(t *testing.T) {
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 2}, pattern(4*64*512))
	last := files[1]
	secs := ewftest.Sections(last)
	cut := last[:secs[len(secs)-1].Offset] // the previous section's next now equals the file size
	r := mustOpen(t, [][]byte{files[0], cut})
	if !hasWarning(r, "E01 set incomplete") {
		t.Fatalf("%q", r.Warnings())
	}
	// The same truncation in a non-last segment is corrupt.
	first := files[0]
	fs := ewftest.Sections(first)
	first = first[:fs[len(fs)-1].Offset]
	wantCorrupt(t, openErr(t, [][]byte{first, files[1]}))
}

func TestEWFSectionWalkLoopIsCorrupt(t *testing.T) {
	m := pattern(2 * 64 * 512)
	cases := []struct {
		name string
		typ  string
		mut  func(s *ewftest.Section, fileLen int64)
	}{
		{"next backwards", "header", func(s *ewftest.Section, _ int64) { s.Next = 5 }},
		{"next backwards later section", "sectors", func(s *ewftest.Section, _ int64) { s.Next = 13 }},
		{"next == self non-terminal", "sectors", func(s *ewftest.Section, _ int64) { s.Next = s.Offset }},
		{"next beyond EOF", "sectors", func(s *ewftest.Section, n int64) { s.Next = n + 100 }},
		{"next overlaps own descriptor", "sectors", func(s *ewftest.Section, _ int64) { s.Next = s.Offset + 10 }},
		{"1 GiB claimed size", "sectors", func(s *ewftest.Section, _ int64) { s.Size = 1 << 30 }},
		{"size overflows", "sectors", func(s *ewftest.Section, _ int64) { s.Size = math.MaxInt64 }},
		{"size below descriptor", "sectors", func(s *ewftest.Section, _ int64) { s.Size = 10 }},
		{"next at 2^64-1", "sectors", func(s *ewftest.Section, _ int64) { s.Next = -1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := ewftest.Build(ewftest.Options{}, m)
			s := section(t, files[0], c.typ)
			c.mut(&s, int64(len(files[0])))
			ewftest.FixDescriptor(files[0], s)
			wantCorrupt(t, openErr(t, files))
		})
	}
	t.Run("cycle of two descriptors", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		secs := ewftest.Sections(files[0])
		a, b := secs[0], secs[1]
		b.Next = a.Offset
		ewftest.FixDescriptor(files[0], b)
		wantCorrupt(t, openErr(t, files))
	})
	t.Run("terminal with a stray next pointer warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "done")
		d.Next = 14
		ewftest.FixDescriptor(files[0], d)
		r := mustOpen(t, files)
		if !hasWarning(r, "not its own offset") {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("size differing from next-offset warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		s := section(t, files[0], "sectors")
		s.Size -= 4
		ewftest.FixDescriptor(files[0], s)
		r := mustOpen(t, files)
		if !hasWarning(r, "next-offset") {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("terminal size 0 is accepted", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "done")
		d.Size = 0
		ewftest.FixDescriptor(files[0], d)
		if r := mustOpen(t, files); len(r.Warnings()) != 0 {
			t.Fatalf("%q", r.Warnings())
		}
	})
}

func TestSectionDescriptorChecksum(t *testing.T) {
	files := ewftest.Build(ewftest.Options{}, pattern(2*64*512))
	s := section(t, files[0], "volume")
	files[0][s.Offset+50] ^= 1 // padding byte: only the checksum notices
	ce := asCorrupt(t, openErr(t, files))
	if !strings.Contains(ce.Reason, "checksum") {
		t.Fatalf("%v", ce)
	}
}

func TestVolumeHostile(t *testing.T) {
	m := pattern(3 * 64 * 512) // 192 sectors, 3 chunks
	type mut func(p []byte)
	u32 := func(off int, v uint32) mut { return func(p []byte) { binary.LittleEndian.PutUint32(p[off:], v) } }
	u64 := func(off int, v uint64) mut { return func(p []byte) { binary.LittleEndian.PutUint64(p[off:], v) } }
	cases := []struct {
		name string
		mut  mut
	}{
		{"bps 0", u32(12, 0)},
		{"bps 3", u32(12, 3)},
		{"bps 8192", u32(12, 8192)},
		{"spc 0", u32(8, 0)},
		{"chunk size 32 MiB", func(p []byte) { u32(12, 4096)(p); u32(8, 8192)(p) }},
		{"spc huge", u32(8, math.MaxUint32)},
		{"sector count overflow", u64(16, math.MaxUint64)},
		{"sector count overflows int64", u64(16, 1<<62)},
		{"too many chunks", func(p []byte) { u32(8, 1)(p); u64(16, 1<<40)(p); u32(4, math.MaxUint32)(p) }},
		{"chunk count too big", u32(4, 4)},
		{"chunk count too small", u32(4, 2)},
		{"chunk count zero", u32(4, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := ewftest.Build(ewftest.Options{}, m)
			v := section(t, files[0], "volume")
			p := int(v.Offset) + 76
			c.mut(files[0][p : p+1052])
			ewftest.FixAdler(files[0], p, p+1048, p+1048)
			wantCorrupt(t, openErr(t, files))
		})
	}
	t.Run("bad checksum", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		v := section(t, files[0], "volume")
		files[0][v.Offset+76+100] ^= 1
		ce := asCorrupt(t, openErr(t, files))
		if !strings.Contains(ce.Reason, "checksum") || ce.Section != "volume" {
			t.Fatalf("%v", ce)
		}
	})
	t.Run("section of another size is unsupported", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		v := section(t, files[0], "volume")
		v.Size = 1100 // next is kept, so the chain still walks
		ewftest.FixDescriptor(files[0], v)
		err := openErr(t, files)
		if !errors.Is(err, ewf.ErrUnsupported) || errors.Is(err, ewf.ErrCorrupt) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("disk section of the same layout is accepted", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		v := section(t, files[0], "volume")
		v.Type = "disk"
		ewftest.FixDescriptor(files[0], v)
		if r := mustOpen(t, files); r.Chunks() != 3 || len(r.Warnings()) != 0 {
			t.Fatalf("%d %q", r.Chunks(), r.Warnings())
		}
	})
	t.Run("a second differing volume warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "data")
		p := int(d.Offset) + 76
		binary.LittleEndian.PutUint32(files[0][p+8:], 32) // spc 32 => 6 chunks
		binary.LittleEndian.PutUint32(files[0][p+4:], 6)
		ewftest.FixAdler(files[0], p, p+1048, p+1048)
		d.Type = "volume"
		ewftest.FixDescriptor(files[0], d)
		r := mustOpen(t, files)
		if !hasWarning(r, "second volume section") || r.ChunkSize() != 32768 {
			t.Fatalf("%q chunk %d", r.Warnings(), r.ChunkSize())
		}
	})
	t.Run("an identical second volume is silent", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "data")
		d.Type = "volume"
		ewftest.FixDescriptor(files[0], d)
		if r := mustOpen(t, files); len(r.Warnings()) != 0 {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("a second corrupt volume warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "data")
		files[0][d.Offset+76+9] ^= 1
		d.Type = "volume"
		ewftest.FixDescriptor(files[0], d)
		if r := mustOpen(t, files); !hasWarning(r, "second volume section ignored") {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("a differing data section warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "data")
		p := int(d.Offset) + 76
		binary.LittleEndian.PutUint32(files[0][p+8:], 32)
		binary.LittleEndian.PutUint32(files[0][p+4:], 6)
		ewftest.FixAdler(files[0], p, p+1048, p+1048)
		if r := mustOpen(t, files); !hasWarning(r, "data section geometry differs") {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("a damaged data section warns", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		d := section(t, files[0], "data")
		files[0][d.Offset+76+9] ^= 1
		if r := mustOpen(t, files); !hasWarning(r, "data section ignored") {
			t.Fatalf("%q", r.Warnings())
		}
	})
	t.Run("no volume", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, nil)
		v := section(t, files[0], "volume")
		v.Type = "xvolume"
		ewftest.FixDescriptor(files[0], v)
		ce := asCorrupt(t, openErr(t, files))
		if !strings.Contains(ce.Reason, "no volume") {
			t.Fatalf("%v", ce)
		}
	})
	t.Run("table before volume", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{}, m)
		v := section(t, files[0], "volume")
		v.Type = "xvolume"
		ewftest.FixDescriptor(files[0], v)
		ce := asCorrupt(t, openErr(t, files))
		if !strings.Contains(ce.Reason, "precedes any volume") {
			t.Fatalf("%v", ce)
		}
	})
	t.Run("empty media opens", func(t *testing.T) {
		r := mustOpen(t, ewftest.Build(ewftest.Options{}, nil))
		if r.Size() != 0 || r.Chunks() != 0 {
			t.Fatal("empty media")
		}
	})
}

func TestUnknownSectionsAreCounted(t *testing.T) {
	files := ewftest.Build(ewftest.Options{}, pattern(2*64*512))
	s := section(t, files[0], "data")
	s.Type = "session"
	ewftest.FixDescriptor(files[0], s)
	r := mustOpen(t, files)
	if meta(r)["unknown_sections"] != "session" || len(r.Warnings()) != 0 {
		t.Fatalf("%v %q", meta(r), r.Warnings())
	}
	k := keys(r)
	if k[len(k)-1] != "unknown_sections" {
		t.Fatalf("unknown_sections must be last: %v", k)
	}
}

type countingReader struct {
	n    *atomic.Int64
	data []byte
}

func (c countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return bytes.NewReader(c.data).ReadAt(p, off)
}

func TestSegmentCountAndSizeLimits(t *testing.T) {
	t.Run("too many segments", func(t *testing.T) {
		var reads atomic.Int64
		rd := countingReader{n: &reads, data: make([]byte, 100)}
		segs := make([]ewf.Segment, ewf.MaxSegments+1000)
		for i := range segs {
			segs[i] = ewf.Segment{R: rd, Size: 100}
		}
		if _, err := ewf.Open(segs); err == nil {
			t.Fatal("66000 segments opened")
		}
		if reads.Load() != 0 {
			t.Fatalf("%d reads before the count check", reads.Load())
		}
	})
	t.Run("no segments", func(t *testing.T) {
		_, err := ewf.Open(nil)
		wantCorrupt(t, err)
	})
	t.Run("12-byte segment", func(t *testing.T) {
		seg := ewftest.Build(ewftest.Options{}, pattern(64*512))[0][:12]
		wantCorrupt(t, openErr(t, [][]byte{seg}))
	})
	t.Run("segment smaller than header plus descriptor", func(t *testing.T) {
		seg := ewftest.Build(ewftest.Options{}, pattern(64*512))[0][:88]
		wantCorrupt(t, openErr(t, [][]byte{seg}))
	})
	t.Run("negative size", func(t *testing.T) {
		_, err := ewf.Open([]ewf.Segment{{R: bytes.NewReader(nil), Size: -5}})
		wantCorrupt(t, err)
	})
	t.Run("nil reader", func(t *testing.T) {
		_, err := ewf.Open([]ewf.Segment{{Size: 1000}})
		wantCorrupt(t, err)
	})
	t.Run("bad signature", func(t *testing.T) {
		seg := ewftest.Build(ewftest.Options{}, pattern(64*512))[0]
		seg[0] = 'X'
		wantCorrupt(t, openErr(t, [][]byte{seg}))
	})
	t.Run("bad header fields", func(t *testing.T) {
		for _, mut := range []func(b []byte){
			func(b []byte) { b[8] = 2 },
			func(b []byte) { b[11] = 1 },
		} {
			seg := ewftest.Build(ewftest.Options{}, pattern(64*512))[0]
			mut(seg)
			wantCorrupt(t, openErr(t, [][]byte{seg}))
		}
	})
}

// failAfter serves data until a request reaches past ok bytes, then fails.
type failAfter struct {
	data []byte
	ok   int64
	err  error
}

func (f failAfter) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.ok {
		return 0, f.err
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

func TestIOErrorsAreNotCorrupt(t *testing.T) {
	seg := ewftest.Build(ewftest.Options{}, pattern(2*64*512))[0]
	boom := errors.New("disk on fire")
	t.Run("read error", func(t *testing.T) {
		_, err := ewf.Open([]ewf.Segment{{R: failAfter{data: seg, ok: 100, err: boom}, Size: int64(len(seg))}})
		if !errors.Is(err, boom) || errors.Is(err, ewf.ErrCorrupt) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("segment shrank", func(t *testing.T) {
		_, err := ewf.Open([]ewf.Segment{{R: bytes.NewReader(seg[:200]), Size: int64(len(seg))}})
		if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ewf.ErrCorrupt) {
			t.Fatalf("%v", err)
		}
	})
}

func TestUnsupportedSignatures(t *testing.T) {
	for name, sig := range map[string][]byte{
		"EVF2": []byte("EVF2\x0d\x0a\x81\x00"),
		"LVF":  []byte("LVF\x09\x0d\x0a\xff\x00"),
		"LEF2": []byte("LEF2\x0d\x0a\x81\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			seg := ewftest.Build(ewftest.Options{}, pattern(64*512))[0]
			copy(seg, sig)
			err := openErr(t, [][]byte{seg})
			if !errors.Is(err, ewf.ErrUnsupported) || errors.Is(err, ewf.ErrCorrupt) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func types(secs []ewftest.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Type)
	}
	return out
}

func TestBuilderRoundTripsSections(t *testing.T) {
	m := pattern(5 * 64 * 512)
	t.Run("single segment follows real output", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 3, ErrorRanges: [][2]uint32{{1, 1}}}, m)
		want := []string{"header2", "header2", "header", "volume", "sectors", "table", "table2", "sectors", "table", "table2", "data", "error2", "digest", "hash", "done"}
		if got := types(ewftest.Sections(files[0])); !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for _, s := range ewftest.Sections(files[0]) {
			switch {
			case s.Type == "done" && (s.Size != 0 || s.Next != s.Offset):
				t.Fatalf("done: %+v", s)
			case s.Type != "done" && s.Next != s.Offset+s.Size:
				t.Fatalf("%s: next %d != off %d + size %d", s.Type, s.Next, s.Offset, s.Size)
			}
		}
	})
	t.Run("options vary the layout", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{NoHeader: true, NoHeader2: true, NoTable2: true, NoHash: true, NoDigest: true, NoData: true}, m)
		want := []string{"volume", "sectors", "table", "done"}
		if got := types(ewftest.Sections(files[0])); !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
		files = ewftest.Build(ewftest.Options{HashBeforeDigest: true, TerminalSize76: true}, m)
		got := ewftest.Sections(files[0])
		n := len(got)
		if got[n-3].Type != "hash" || got[n-2].Type != "digest" || got[n-1].Size != 76 {
			t.Fatalf("%+v", got[n-3:])
		}
	})
	t.Run("multi segment follows real output", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 2}, m)
		if len(files) != 3 {
			t.Fatalf("%d segments", len(files))
		}
		want := []string{"header2", "header2", "header", "volume", "sectors", "table", "table2", "next"}
		if got := types(ewftest.Sections(files[0])); !slices.Equal(got, want) {
			t.Fatalf("%v", got)
		}
		if got := types(ewftest.Sections(files[1])); !slices.Equal(got, []string{"data", "sectors", "table", "table2", "next"}) {
			t.Fatalf("%v", got)
		}
		want = []string{"data", "sectors", "table", "table2", "digest", "hash", "done"}
		if got := types(ewftest.Sections(files[2])); !slices.Equal(got, want) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("table base conventions", func(t *testing.T) {
		for _, base := range []ewftest.BaseMode{ewftest.BaseSectorsDescriptor, ewftest.BaseZero} {
			f := ewftest.Build(ewftest.Options{Base: base}, m)[0]
			s := section(t, f, "sectors")
			tb := section(t, f, "table")
			p := tb.Offset + 76
			baseOff := int64(binary.LittleEndian.Uint64(f[p+8:]))
			first := int64(binary.LittleEndian.Uint32(f[p+24:]) & 0x7fffffff)
			if base == ewftest.BaseSectorsDescriptor && (baseOff != s.Offset || first != 76) {
				t.Fatalf("descriptor base: base %d first %d, sectors at %d", baseOff, first, s.Offset)
			}
			if base == ewftest.BaseZero && (baseOff != 0 || first != s.Offset+76) {
				t.Fatalf("zero base: base %d first %d", baseOff, first)
			}
		}
	})
	t.Run("no done writes next", func(t *testing.T) {
		got := types(ewftest.Sections(ewftest.Build(ewftest.Options{NoDone: true}, m)[0]))
		if got[len(got)-1] != "next" {
			t.Fatalf("%v", got)
		}
	})
	t.Run("chunk locations", func(t *testing.T) {
		for _, base := range []ewftest.BaseMode{ewftest.BaseZero, ewftest.BaseSectorsDescriptor} {
			files := ewftest.Build(ewftest.Options{Base: base, ChunksPerSegment: 2, ChunksPerTable: 1}, m)
			locs := ewftest.ChunkLocs(files)
			if len(locs) != 5 {
				t.Fatalf("%d locs", len(locs))
			}
			for i, l := range locs {
				if l.Compressed || l.Segment != i/2+1 || l.Length != 64*512+4 {
					t.Fatalf("base %d loc %d: %+v", base, i, l)
				}
				// The raw chunk really sits there: raw bytes then their Adler-32.
				got := files[l.Segment-1][l.Offset : l.Offset+l.Length-4]
				if !bytes.Equal(got, m[i*64*512:(i+1)*64*512]) {
					t.Fatalf("base %d chunk %d bytes differ", base, i)
				}
			}
		}
	})
	t.Run("compressed chunk locations", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{Compress: ewftest.CompressMixed}, append(make([]byte, 64*512), pattern(64*512)...))
		locs := ewftest.ChunkLocs(files)
		if len(locs) != 2 || !locs[0].Compressed || locs[1].Compressed {
			t.Fatalf("%+v", locs)
		}
	})
}

// TestOpenSurvivesMutations flips bytes of valid images (including inside
// descriptors, whose checksums are then repaired so the walk sees the
// mutated numbers) and requires Open to return promptly without panicking.
func TestOpenSurvivesMutations(t *testing.T) {
	base := ewftest.Build(ewftest.Options{
		ChunksPerSegment: 2, ChunksPerTable: 1, Compress: ewftest.CompressMixed,
		ErrorRanges: [][2]uint32{{1, 2}, {5, 6}}, Case: "x",
	}, pattern(5*64*512))
	x := uint32(99)
	next := func(n int) int {
		x = x*1664525 + 1013904223
		return int(x>>8) % n
	}
	for i := range 1500 {
		files := make([][]byte, len(base))
		for j := range base {
			files[j] = bytes.Clone(base[j])
		}
		seg := files[next(len(files))]
		for range 1 + next(3) {
			seg[next(len(seg))] = byte(next(256))
		}
		if i%2 == 0 { // repair descriptor checksums so mutated pointers are walked
			for _, s := range ewftest.Sections(seg) {
				if s.Offset >= 0 && s.Offset+76 <= int64(len(seg)) {
					ewftest.FixAdler(seg, int(s.Offset), int(s.Offset)+72, int(s.Offset)+72)
				}
			}
		}
		if r, err := openWithin(t, files); err == nil {
			_ = r.Metadata()
			_ = r.Warnings()
		}
	}
}

// TestOpenLayoutVariations covers layouts other writers may use: all must
// open cleanly with identical geometry and stored hashes.
func TestOpenLayoutVariations(t *testing.T) {
	m := pattern(5 * 64 * 512)
	for name, o := range map[string]ewftest.Options{
		"terminal size 76":     {TerminalSize76: true},
		"hash before digest":   {HashBeforeDigest: true},
		"no data sections":     {NoData: true, ChunksPerSegment: 2},
		"base 0":               {Base: ewftest.BaseZero},
		"no table2, no foot":   {NoTable2: true, NoTableFooter: true},
		"multi, compressed":    {ChunksPerSegment: 2, Compress: ewftest.CompressAll},
		"512 sectors, 1 chunk": {SectorsPerChunk: 320},
	} {
		t.Run(name, func(t *testing.T) {
			r := mustOpen(t, ewftest.Build(o, m))
			if w := r.Warnings(); (o.NoTable2 && len(w) != 1) || (!o.NoTable2 && len(w) != 0) {
				t.Fatalf("warnings %q", w)
			}
			g := meta(r)
			if r.Size() != int64(len(m)) || g["md5"] == "" || g["sha1"] == "" {
				t.Fatalf("size %d %v", r.Size(), g)
			}
		})
	}
}
