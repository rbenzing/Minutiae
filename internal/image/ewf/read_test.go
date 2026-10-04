package ewf_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// mixedMedia returns n bytes where every third 4 KiB block is zeros (it
// compresses) and the rest is pattern data (it does not), so a CompressMixed
// image stores both kinds of chunk.
func mixedMedia(n int) []byte {
	b := pattern(n)
	for i := 0; i < n; i += 4096 {
		if (i/4096)%3 == 0 {
			clear(b[i:min(n, i+4096)])
		}
	}
	return b
}

func readAll(t *testing.T, r *ewf.Reader) []byte {
	t.Helper()
	got := make([]byte, r.Size())
	n, err := r.ReadAt(got, 0)
	if n != len(got) || err != nil {
		t.Fatalf("ReadAt(whole media) = %d, %v; want %d, nil", n, err, len(got))
	}
	return got
}

func tableSections(seg []byte, typ string) []ewftest.Section {
	var out []ewftest.Section
	for _, s := range ewftest.Sections(seg) {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// fixTable recomputes the header checksum and, when present, the entries
// footer of a table payload.
func fixTable(p []byte) {
	n := int(binary.LittleEndian.Uint32(p))
	binary.LittleEndian.PutUint32(p[20:], adler32.Checksum(p[:20]))
	if 24+4*n+4 <= len(p) {
		binary.LittleEndian.PutUint32(p[24+4*n:], adler32.Checksum(p[24:24+4*n]))
	}
}

// patchTable applies fn to the payload of the k-th section of type typ in
// seg (typ is "table" or "table2").
func patchTable(t *testing.T, seg []byte, typ string, k int, fn func(p []byte)) {
	t.Helper()
	ss := tableSections(seg, typ)
	if k >= len(ss) {
		t.Fatalf("no %s #%d", typ, k)
	}
	s := ss[k]
	fn(seg[s.Offset+76 : s.Offset+s.Size])
}

// patchBoth applies fn to the k-th table and table2 and fixes both.
func patchBoth(t *testing.T, seg []byte, k int, fn func(p []byte)) {
	t.Helper()
	for _, typ := range []string{"table", "table2"} {
		patchTable(t, seg, typ, k, func(p []byte) { fn(p); fixTable(p) })
	}
}

func entryAt(p []byte, i int) uint32 { return binary.LittleEndian.Uint32(p[24+4*i:]) }
func setEntry(p []byte, i int, v uint32) {
	binary.LittleEndian.PutUint32(p[24+4*i:], v)
}

func TestEWFReadAtMatchesMedia(t *testing.T) {
	type size struct {
		name string
		fn   func(chunk, bps int) int
	}
	sizes := []size{
		{"exact", func(chunk, _ int) int { return 9 * chunk }},
		{"one-sector-over", func(chunk, bps int) int { return 8*chunk + bps }},
		{"shorter-than-chunk", func(_, bps int) int { return 3 * bps }},
	}
	comps := map[string]ewftest.Compress{"none": ewftest.CompressNone, "all": ewftest.CompressAll, "mixed": ewftest.CompressMixed}
	bases := map[string]ewftest.BaseMode{"descriptor": ewftest.BaseSectorsDescriptor, "zero": ewftest.BaseZero}
	for cname, comp := range comps {
		for _, bps := range []int{512, 4096} {
			for _, spc := range []int{8, 64} {
				for bname, base := range bases {
					for _, noFooter := range []bool{false, true} {
						for _, sz := range sizes {
							for _, perSeg := range []int{0, 3} {
								for _, perTable := range []int{1, 7, 100} {
									name := fmt.Sprintf("%s/bps%d/spc%d/base-%s/nofooter=%v/%s/perSeg%d/perTable%d", cname, bps, spc, bname, noFooter, sz.name, perSeg, perTable)
									t.Run(name, func(t *testing.T) {
										t.Parallel()
										media := mixedMedia(sz.fn(bps*spc, bps))
										files := ewftest.Build(ewftest.Options{
											BytesPerSector: bps, SectorsPerChunk: spc, Compress: comp, Base: base,
											NoTableFooter: noFooter, ChunksPerSegment: perSeg, ChunksPerTable: perTable,
										}, media)
										r := mustOpen(t, files)
										if w := r.Warnings(); len(w) != 0 {
											t.Fatalf("warnings %q", w)
										}
										if !bytes.Equal(readAll(t, r), media) {
											t.Fatal("whole-media read differs from the media")
										}
										rng := rand.New(rand.NewPCG(uint64(len(media)+perTable), 1)) //nolint:gosec // deterministic test input, not security
										for range 20 {
											off := rng.IntN(len(media))
											n := 1 + rng.IntN(3*bps*spc)
											p := make([]byte, n)
											got, err := r.ReadAt(p, int64(off))
											want := media[off:min(len(media), off+n)]
											if !bytes.Equal(p[:got], want) {
												t.Fatalf("ReadAt(%d bytes at %d) differs from the media", n, off)
											}
											if (got < n) != (err == io.EOF) || (got == n && err != nil) {
												t.Fatalf("ReadAt(%d at %d) = %d, %v", n, off, got, err)
											}
										}
									})
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestEWFReadAtContract(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(6*cs + 512) // 7 chunks, the last one a single sector
	files := ewftest.Build(ewftest.Options{ChunksPerSegment: 2, Compress: ewftest.CompressMixed}, media)
	if len(files) != 4 {
		t.Fatalf("%d segments", len(files))
	}
	r := mustOpen(t, files)
	size := int64(len(media))

	if n, err := r.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("empty read at 0 = %d, %v", n, err)
	}
	if n, err := r.ReadAt(make([]byte, 0), size+5); n != 0 || err != nil {
		t.Fatalf("empty read past the end = %d, %v", n, err)
	}
	if n, err := r.ReadAt(make([]byte, 4), -1); n != 0 || err == nil || err == io.EOF {
		t.Fatalf("negative offset = %d, %v", n, err)
	}
	if n, err := r.ReadAt(make([]byte, 4), math.MinInt64); n != 0 || err == nil {
		t.Fatalf("MinInt64 offset = %d, %v", n, err)
	}
	for _, off := range []int64{size, size + 1, size + cs, math.MaxInt64, math.MaxInt64 - 3} {
		p := bytes.Repeat([]byte{0xAA}, 16)
		if n, err := r.ReadAt(p, off); n != 0 || err != io.EOF {
			t.Fatalf("offset %d = %d, %v; want 0, EOF", off, n, err)
		}
		if !bytes.Equal(p, bytes.Repeat([]byte{0xAA}, 16)) {
			t.Fatalf("offset %d modified p", off)
		}
	}
	// A read spanning the end is short and EOF.
	p := make([]byte, 600)
	n, err := r.ReadAt(p, size-100)
	if n != 100 || err != io.EOF || !bytes.Equal(p[:n], media[size-100:]) {
		t.Fatalf("spanning read = %d, %v", n, err)
	}
	// A huge buffer near the end must not allocate or misbehave.
	huge := make([]byte, 8<<20)
	n, err = r.ReadAt(huge, size-10)
	if n != 10 || err != io.EOF || !bytes.Equal(huge[:10], media[size-10:]) {
		t.Fatalf("huge read = %d, %v", n, err)
	}
	// Exactly to the end: full count, no error.
	p = make([]byte, 100)
	if n, err := r.ReadAt(p, size-100); n != 100 || err != nil {
		t.Fatalf("read ending at Size = %d, %v", n, err)
	}
	// Reads straddling every chunk boundary (segment boundaries are chunk
	// boundaries here).
	for b := int64(cs); b < size; b += cs {
		p := make([]byte, 10)
		if n, err := r.ReadAt(p, b-5); n != 10 || err != nil || !bytes.Equal(p, media[b-5:b+5]) {
			t.Fatalf("boundary %d: %d, %v", b, n, err)
		}
	}
	// One read over several segments.
	p = make([]byte, 5*cs)
	if n, err := r.ReadAt(p, 10); n != len(p) || err != nil || !bytes.Equal(p, media[10:10+5*cs]) {
		t.Fatalf("multi-segment read = %d, %v", n, err)
	}
	// One byte at a time over a chunk boundary and byte 0.
	one := make([]byte, 1)
	for _, off := range []int64{0, cs - 1, cs, size - 1} {
		if n, err := r.ReadAt(one, off); n != 1 || err != nil || one[0] != media[off] {
			t.Fatalf("byte %d: %d, %v", off, n, err)
		}
	}
}

func TestEWFReadAtEmptyMedia(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{}, nil))
	if r.Size() != 0 {
		t.Fatalf("size %d", r.Size())
	}
	if n, err := r.ReadAt(make([]byte, 8), 0); n != 0 || err != io.EOF {
		t.Fatalf("read of an empty media = %d, %v", n, err)
	}
}

// flipMiddle flips a byte in the middle of chunk c as stored.
func flipMiddle(t *testing.T, files [][]byte, c int) ewftest.ChunkLoc {
	t.Helper()
	locs := ewftest.ChunkLocs(files)
	l := locs[c]
	files[l.Segment-1][l.Offset+l.Length/2] ^= 0x5A
	return l
}

func wantChunkError(t *testing.T, err error, chunk int64) {
	t.Helper()
	_ = asChunkError(t, err, chunk)
}

func asChunkError(t *testing.T, err error, chunk int64) *ewf.ChunkError {
	t.Helper()
	var ce *ewf.ChunkError
	if err == nil || !errors.Is(err, ewf.ErrChunkCorrupt) || !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ChunkError matching ErrChunkCorrupt", err)
	}
	if ce.Chunk != chunk {
		t.Fatalf("ChunkError.Chunk = %d, want %d (%v)", ce.Chunk, chunk, err)
	}
	return ce
}

func TestEWFCorruptChunkIsError(t *testing.T) {
	const (
		cs     = 64 * 512
		chunks = 20
		bad    = 10
	)
	media := pattern(chunks * cs)
	for name, comp := range map[string]ewftest.Compress{"compressed": ewftest.CompressAll, "uncompressed": ewftest.CompressNone} {
		t.Run(name, func(t *testing.T) {
			files := ewftest.Build(ewftest.Options{Compress: comp, ChunksPerSegment: 7, ChunksPerTable: 5}, media)
			l := flipMiddle(t, files, bad)
			if l.Compressed != (comp == ewftest.CompressAll) {
				t.Fatalf("chunk %d compressed = %v", bad, l.Compressed)
			}
			r := mustOpen(t, files)

			sentinel := bytes.Repeat([]byte{0xAA}, cs+300)
			p := bytes.Clone(sentinel)
			off := int64(bad*cs - 100)
			n, err := r.ReadAt(p, off)
			if n != 100 {
				t.Fatalf("n = %d, want the 100 bytes before the bad chunk", n)
			}
			ce := asChunkError(t, err, bad)
			if ce.Segment != l.Segment || ce.Offset != l.Offset {
				t.Fatalf("ChunkError location segment %d offset %d, want %d, %d", ce.Segment, ce.Offset, l.Segment, l.Offset)
			}
			if !bytes.Equal(p[:100], media[off:off+100]) {
				t.Fatal("bytes before the bad chunk are wrong")
			}
			if !bytes.Equal(p[100:], sentinel[100:]) {
				t.Fatal("the rest of p was touched (zero-filled?)")
			}

			// Wholly inside the bad chunk.
			p = bytes.Clone(sentinel[:64])
			n, err = r.ReadAt(p, int64(bad*cs+1000))
			wantChunkError(t, err, bad)
			if n != 0 || !bytes.Equal(p, sentinel[:64]) {
				t.Fatalf("read inside the bad chunk: n = %d, p touched = %v", n, !bytes.Equal(p, sentinel[:64]))
			}
			// Neighbours still read, also with the bad chunk's neighbours cached.
			for _, c := range []int{bad - 1, bad + 1, 0, chunks - 1} {
				p := make([]byte, cs)
				if n, err := r.ReadAt(p, int64(c*cs)); n != cs || err != nil || !bytes.Equal(p, media[c*cs:(c+1)*cs]) {
					t.Fatalf("chunk %d: %d, %v", c, n, err)
				}
			}
			// Errors are not cached: the same read fails the same way.
			_, err1 := r.ReadAt(make([]byte, 10), int64(bad*cs))
			_, err2 := r.ReadAt(make([]byte, 10), int64(bad*cs))
			wantChunkError(t, err1, bad)
			if err1.Error() != err2.Error() {
				t.Fatalf("repeated read: %v vs %v", err1, err2)
			}
			// The whole-media read fails with the same chunk, returning the prefix.
			all := make([]byte, len(media))
			n, err = r.ReadAt(all, 0)
			wantChunkError(t, err, bad)
			if n != bad*cs || !bytes.Equal(all[:n], media[:n]) {
				t.Fatalf("whole-media read returned %d bytes", n)
			}
		})
	}
}

func zlibOf(p []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	_, _ = zw.Write(p)
	_ = zw.Close()
	return b.Bytes()
}

func TestEWFBadZlibStreamsAreChunkErrors(t *testing.T) {
	const cs = 64 * 512
	media := pattern(5 * cs)
	good := zlibOf(media[2*cs : 3*cs])
	cases := map[string][]byte{
		"truncated":        good[:len(good)-6],
		"truncated-header": good[:1],
		"too-long":         zlibOf(append(bytes.Clone(media[2*cs:3*cs]), 0)),
		"too-short":        zlibOf(media[2*cs : 3*cs-1]),
		"empty-stream":     zlibOf(nil),
		"bad-trailer":      append(bytes.Clone(good[:len(good)-1]), good[len(good)-1]^1),
		"not-zlib":         bytes.Repeat([]byte{0xFF}, 100),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			files := ewftest.Build(ewftest.Options{Compress: ewftest.CompressAll, Override: map[int]ewftest.RawChunk{2: {Data: data, Compressed: true}}}, media)
			r := mustOpen(t, files)
			p := make([]byte, cs)
			n, err := r.ReadAt(p, 2*cs)
			wantChunkError(t, err, 2)
			if n != 0 {
				t.Fatalf("n = %d", n)
			}
			// Neighbours are fine.
			if n, err := r.ReadAt(p, 3*cs); n != cs || err != nil || !bytes.Equal(p, media[3*cs:4*cs]) {
				t.Fatalf("neighbour: %d, %v", n, err)
			}
		})
	}
	t.Run("uncompressed-bad-adler", func(t *testing.T) {
		raw := append(bytes.Clone(media[2*cs:3*cs]), 1, 2, 3, 4)
		r := mustOpen(t, ewftest.Build(ewftest.Options{Override: map[int]ewftest.RawChunk{2: {Data: raw}}}, media))
		_, err := r.ReadAt(make([]byte, 1), 2*cs)
		wantChunkError(t, err, 2)
	})
}

func TestEWFZlibOutputCappedAtChunkSize(t *testing.T) {
	const cs = 64 * 512
	media := pattern(5 * cs)
	bomb := zlibOf(make([]byte, 32<<20)) // inflates to 1024 chunks
	if len(bomb) > 2*cs {
		t.Fatalf("bomb stream is %d bytes", len(bomb))
	}
	files := ewftest.Build(ewftest.Options{Compress: ewftest.CompressAll, Override: map[int]ewftest.RawChunk{2: {Data: bomb, Compressed: true}}}, media)
	r := mustOpen(t, files)
	p := make([]byte, cs)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := r.ReadAt(p, 2*cs)
	runtime.ReadMemStats(&after)
	wantChunkError(t, err, 2)
	if !strings.Contains(err.Error(), "more than") {
		t.Fatalf("expected the output cap to trigger: %v", err)
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 16*cs {
		t.Fatalf("decoding the bomb allocated %d bytes (chunk size %d)", used, cs)
	}
}

// wantAllUnreadable asserts that every chunk of the media fails with a
// ChunkError (no bytes returned) and that exactly the chunks below ok read.
func wantUnreadableFrom(t *testing.T, r *ewf.Reader, media []byte, cs int, ok int) {
	t.Helper()
	p := make([]byte, cs)
	for c := range len(media) / cs {
		n, err := r.ReadAt(p, int64(c*cs))
		if c < ok {
			if n != cs || err != nil || !bytes.Equal(p, media[c*cs:(c+1)*cs]) {
				t.Fatalf("chunk %d before the gap: %d, %v", c, n, err)
			}
			continue
		}
		if n != 0 {
			t.Fatalf("chunk %d: %d bytes served from an unproven location", c, n)
		}
		wantChunkError(t, err, int64(c))
	}
}

// TestEWFChunkOffsetOutsideSegmentIsChunkError: an entry outside its segment
// breaks rule (d): the group is untrusted and, as the numbering after it is
// then unknowable, so is every later chunk.
func TestEWFChunkOffsetOutsideSegmentIsChunkError(t *testing.T) {
	const cs = 64 * 512
	media := pattern(10 * cs)
	t.Run("entry past the file", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
		patchBoth(t, files[0], 0, func(p []byte) { setEntry(p, 3, 0x7FFFFFF0) })
		r := mustOpen(t, files)
		if !hasWarning(r, "points outside the segment") || !hasWarning(r, "chunks from index 0 onward unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 0)
	})
	t.Run("entry past the file in the second group", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
		patchBoth(t, files[0], 1, func(p []byte) { setEntry(p, 3, 0x7FFFFFF0) })
		r := mustOpen(t, files)
		if !hasWarning(r, "group 2") || !hasWarning(r, "chunks from index 5 onward unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 5)
	})
	t.Run("base overflows", func(t *testing.T) {
		for name, base := range map[string]uint64{
			"int64 overflow":  math.MaxInt64 - 10,
			"uint64 overflow": math.MaxUint64 - 10,
			"past 2^47":       1 << 47,
		} {
			t.Run(name, func(t *testing.T) {
				files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
				patchBoth(t, files[0], 0, func(p []byte) { binary.LittleEndian.PutUint64(p[8:], base) })
				r := mustOpen(t, files)
				if !hasWarning(r, "points outside the segment") {
					t.Fatalf("warnings %q", r.Warnings())
				}
				wantUnreadableFrom(t, r, media, cs, 0)
			})
		}
	})
}

// wantGroupUncovered asserts the outcome of a chunk table group (1-based) whose
// table and table2 both failed their own checks: Open succeeded, one warning
// names the group and the first uncovered chunk, the chunks before it read
// byte-identically, every chunk from it on fails with a ChunkError (no bytes),
// and Verify cannot verify the stored hashes.
func wantGroupUncovered(t *testing.T, r *ewf.Reader, media []byte, group int, from int) {
	t.Helper()
	const cs = 64 * 512
	var named []string
	for _, w := range r.Warnings() {
		if strings.Contains(w, fmt.Sprintf("chunk table group %d:", group)) {
			named = append(named, w)
		}
	}
	if len(named) != 1 || !strings.Contains(named[0], fmt.Sprintf("chunks from index %d onward unreadable", from)) {
		t.Fatalf("want exactly one warning naming group %d and index %d, got %q", group, from, r.Warnings())
	}
	wantUnreadableFrom(t, r, media, cs, from)
	res, err := r.Verify(context.Background(), nil)
	if err != nil || res.BadChunk != int64(from) || res.Result() != "unverified" || res.MD5.Status != ewf.HashUnverified {
		t.Fatalf("Verify = %+v, %v; want unverified at chunk %d", res, err, from)
	}
}

func TestEWFTable2FallbackWhenTableCorrupt(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10 * cs)
	build := func() [][]byte {
		return ewftest.Build(ewftest.Options{ChunksPerTable: 4, Compress: ewftest.CompressMixed}, media)
	}
	oneWarning := func(t *testing.T, r *ewf.Reader, sub string) {
		t.Helper()
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], sub) {
			t.Fatalf("warnings %q, want exactly one containing %q", w, sub)
		}
	}
	corruptions := map[string]func(p []byte){
		"header checksum": func(p []byte) { p[20] ^= 0xFF },
		"entry (footer checksum)": func(p []byte) {
			p[24+4] ^= 0x01 // flips a bit of entry 1 without fixing the footer
		},
		"entry_count huge": func(p []byte) {
			binary.LittleEndian.PutUint32(p, 0xFFFFFFFF)
			binary.LittleEndian.PutUint32(p[20:], adler32.Checksum(p[:20]))
		},
		"entry_count too small": func(p []byte) {
			binary.LittleEndian.PutUint32(p, 2)
			binary.LittleEndian.PutUint32(p[20:], adler32.Checksum(p[:20]))
		},
	}
	for name, corrupt := range corruptions {
		t.Run("table "+name, func(t *testing.T) {
			files := build()
			patchTable(t, files[0], "table", 1, corrupt) // the middle group
			r := mustOpen(t, files)
			oneWarning(t, r, "using table2")
			if !bytes.Equal(readAll(t, r), media) {
				t.Fatal("ReadAt differs from the media after the table2 fallback")
			}
		})
		t.Run("table2 "+name, func(t *testing.T) {
			// The documented choice: the table is used and ONE warning names
			// the unreadable table2.
			files := build()
			patchTable(t, files[0], "table2", 1, corrupt)
			r := mustOpen(t, files)
			oneWarning(t, r, "table2")
			if !strings.Contains(r.Warnings()[0], "using table") || strings.Contains(r.Warnings()[0], "using table2") {
				t.Fatalf("warning %q", r.Warnings())
			}
			if !bytes.Equal(readAll(t, r), media) {
				t.Fatal("ReadAt differs from the media")
			}
		})
		t.Run("both "+name, func(t *testing.T) {
			files := build()
			patchTable(t, files[0], "table", 1, corrupt)
			patchTable(t, files[0], "table2", 1, corrupt)
			// A damaged table must not make the image unopenable: the group
			// (chunks 4..7) and everything after it is uncovered.
			wantGroupUncovered(t, mustOpen(t, files), media, 2, 4)
		})
	}
	t.Run("lone corrupt table", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 4, NoTable2: true}, media)
		patchTable(t, files[0], "table", 1, corruptions["header checksum"])
		wantGroupUncovered(t, mustOpen(t, files), media, 2, 4)
	})
	t.Run("lone good table2", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 4}, media)
		// sectors, table2, dropped: the copy right after the sectors section is
		// the only table of the group.
		s := tableSections(files[0], "table")[1]
		s.Type = "table2"
		ewftest.FixDescriptor(files[0], s)
		s = tableSections(files[0], "table2")[2] // [1] is the one just made
		s.Type = "dropped"
		ewftest.FixDescriptor(files[0], s)
		r := mustOpen(t, files)
		oneWarning(t, r, "has no table")
		if !bytes.Equal(readAll(t, r), media) {
			t.Fatal("ReadAt differs from the media")
		}
	})
}

// dropFooter removes the entries footer (the last 4 bytes) of the table or
// table2 section at off in a one-group segment and shifts every later section
// back, so the result is a table without its Adler-32 footer.
func dropFooter(seg []byte, sec ewftest.Section) []byte {
	secs := ewftest.Sections(seg)
	cut := sec.Offset + sec.Size - 4
	out := append(append([]byte{}, seg[:cut]...), seg[cut+4:]...)
	for _, s := range secs {
		switch {
		case s.Offset == sec.Offset:
			s.Size -= 4
			s.Next -= 4
		case s.Offset > sec.Offset:
			s.Offset -= 4
			s.Next -= 4
			if s.Type == "next" || s.Type == "done" {
				s.Next = s.Offset
			}
		default:
			if s.Next > sec.Offset {
				s.Next -= 4
			}
		}
		ewftest.FixDescriptor(out, s)
	}
	return out
}

// TestEWFTablesDifferFooterDecides: table and table2 both satisfy the rules but
// differ; the copy whose entries footer verified wins, and when both are
// equally trustworthy the differing chunks are unreadable.
func TestEWFTablesDifferFooterDecides(t *testing.T) {
	const cs = 64 * 512
	media := pattern(10 * cs)
	build := func() [][]byte { return ewftest.Build(ewftest.Options{ChunksPerTable: 10}, media) }
	shift := func(p []byte) { setEntry(p, 2, entryAt(p, 2)+1); fixTable(p) }
	t.Run("table2 lacks a footer", func(t *testing.T) {
		files := build()
		files[0] = dropFooter(files[0], tableSections(files[0], "table2")[0])
		patchTable(t, files[0], "table2", 0, shift)
		r := mustOpen(t, files)
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "differ") || !strings.Contains(w[0], "using table, whose entries checksum verified") {
			t.Fatalf("warnings %q", w)
		}
		if !bytes.Equal(readAll(t, r), media) {
			t.Fatal("the copy with a verified footer must win")
		}
	})
	t.Run("table lacks a footer", func(t *testing.T) {
		files := build()
		files[0] = dropFooter(files[0], tableSections(files[0], "table")[0])
		patchTable(t, files[0], "table", 0, shift)
		r := mustOpen(t, files)
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "using table2, whose entries checksum verified") {
			t.Fatalf("warnings %q", w)
		}
		if !bytes.Equal(readAll(t, r), media) {
			t.Fatal("the copy with a verified footer must win")
		}
	})
	t.Run("equally trustworthy", func(t *testing.T) {
		files := build()
		patchTable(t, files[0], "table2", 0, shift)
		r := mustOpen(t, files)
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "differ") || !strings.Contains(w[0], "first at entry 2") || !strings.Contains(w[0], "1 differing chunk(s) cannot be read") {
			t.Fatalf("warnings %q", w)
		}
		p := make([]byte, cs)
		for c := range 10 {
			n, err := r.ReadAt(p, int64(c*cs))
			if c == 2 {
				wantChunkError(t, err, 2)
				if n != 0 {
					t.Fatalf("n = %d", n)
				}
			} else if n != cs || err != nil || !bytes.Equal(p, media[c*cs:(c+1)*cs]) {
				t.Fatalf("chunk %d: %d, %v", c, n, err)
			}
		}
	})
}

func TestEWFTableCountMismatch(t *testing.T) {
	const cs = 64 * 512
	media := pattern(10 * cs)
	t.Run("last segment removed", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4}, media) // 4+4+2
		r := mustOpen(t, files[:2])
		if !hasWarning(r, "covers 8 of 10 chunks") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		p := make([]byte, 4*cs)
		if n, err := r.ReadAt(p, 4*cs); n != len(p) || err != nil || !bytes.Equal(p, media[4*cs:8*cs]) {
			t.Fatalf("covered chunks: %d, %v", n, err)
		}
		for i := range p {
			p[i] = 0xAA
		}
		n, err := r.ReadAt(p, 7*cs+cs/2)
		wantChunkError(t, err, 8)
		if n != cs/2 || !bytes.Equal(p[:n], media[7*cs+cs/2:8*cs]) || p[n] != 0xAA {
			t.Fatalf("read into the uncovered chunk: n = %d", n)
		}
		if n, err := r.ReadAt(p[:10], 9*cs); n != 0 {
			wantChunkError(t, err, 9)
		}
	})
	t.Run("last table dropped", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 4}, media)
		for _, typ := range []string{"table", "table2"} {
			s := tableSections(files[0], typ)[2]
			s.Type = "dropped"
			ewftest.FixDescriptor(files[0], s)
		}
		r := mustOpen(t, files)
		if !hasWarning(r, "covers 8 of 10 chunks") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		p := make([]byte, 8*cs)
		if n, err := r.ReadAt(p, 0); n != len(p) || err != nil || !bytes.Equal(p, media[:8*cs]) {
			t.Fatalf("covered chunks: %d, %v", n, err)
		}
		n, err := r.ReadAt(make([]byte, 100), 8*cs)
		wantChunkError(t, err, 8)
		if n != 0 {
			t.Fatalf("n = %d", n)
		}
	})
	t.Run("more entries than chunks", func(t *testing.T) {
		// Give the 10-chunk tables a volume section that claims 5 chunks.
		a := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
		b := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media[:5*cs])
		va, vb := section(t, a[0], "volume"), section(t, b[0], "volume")
		if va.Offset != vb.Offset || va.Size != vb.Size {
			t.Fatal("volume sections are not interchangeable")
		}
		copy(a[0][va.Offset:va.Offset+va.Size], b[0][vb.Offset:vb.Offset+vb.Size])
		// The surplus table (group 2) is a count failure: it is uncovered with a
		// warning instead of failing Open; the 5 chunks the volume has read.
		r := mustOpen(t, a)
		if !hasWarning(r, "chunk table group 2:") || !hasWarning(r, "chunks from index 5 onward unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media[:5*cs], cs, 5)
	})
}

func TestEWFHostileTable(t *testing.T) {
	const cs = 64 * 512
	t.Run("entry_count 0xFFFFFFFF", func(t *testing.T) {
		media := pattern(10 * cs)
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
		patchBoth(t, files[0], 0, func(p []byte) { binary.LittleEndian.PutUint32(p, 0xFFFFFFFF) })
		start := time.Now()
		r := mustOpen(t, files)
		if time.Since(start) > 10*time.Second {
			t.Fatal("too slow")
		}
		wantGroupUncovered(t, r, media, 1, 0)
	})
	t.Run("entries not increasing", func(t *testing.T) {
		// Reversed entries break rule (b) (the first entry is not the payload
		// start) and (c): the group is uncovered, with every chunk after it.
		media := mixedMedia(10 * cs)
		for name, comp := range map[string]ewftest.Compress{"none": ewftest.CompressNone, "all": ewftest.CompressAll} {
			files := ewftest.Build(ewftest.Options{ChunksPerTable: 10, Compress: comp}, media)
			patchBoth(t, files[0], 0, func(p []byte) {
				for i, j := 0, 9; i < j; i, j = i+1, j-1 {
					a, b := entryAt(p, i), entryAt(p, j)
					setEntry(p, i, b)
					setEntry(p, j, a)
				}
			})
			r := mustOpen(t, files)
			if !hasWarning(r, "segment 1, chunk table group 1:") || !hasWarning(r, "rule (b)") || !hasWarning(r, "chunks from index 0 onward unreadable") {
				t.Fatalf("%s: warnings %q", name, r.Warnings())
			}
			wantUnreadableFrom(t, r, media, cs, 0)
		}
	})
	t.Run("entries increasing but not from the payload start", func(t *testing.T) {
		// Only the middle of the group is out of order: rule (c).
		media := pattern(10 * cs)
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 10}, media)
		patchBoth(t, files[0], 0, func(p []byte) {
			a, b := entryAt(p, 4), entryAt(p, 5)
			setEntry(p, 4, b)
			setEntry(p, 5, a)
		})
		r := mustOpen(t, files)
		if !hasWarning(r, "rule (c)") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 0)
	})
	t.Run("base shifted by whole strides", func(t *testing.T) {
		// Every entry still points at a valid, checksum-passing chunk, but one
		// stride too far: chunk i would read chunk i+1's data. Rule (b) (the
		// first entry is the payload start) rejects it, as does (d).
		media := pattern(10 * cs)
		for _, strides := range []uint64{1, 2, 9} {
			files := ewftest.Build(ewftest.Options{ChunksPerTable: 10}, media)
			patchBoth(t, files[0], 0, func(p []byte) {
				binary.LittleEndian.PutUint64(p[8:], binary.LittleEndian.Uint64(p[8:])+strides*(cs+4))
			})
			r := mustOpen(t, files)
			if !hasWarning(r, "rule (b)") && !hasWarning(r, "rule (d)") {
				t.Fatalf("strides %d: warnings %q", strides, r.Warnings())
			}
			wantUnreadableFrom(t, r, media, cs, 0)
		}
	})
	t.Run("base shifted back by whole strides", func(t *testing.T) {
		media := pattern(10 * cs)
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5, ChunksPerSegment: 5}, media)
		patchBoth(t, files[1], 0, func(p []byte) {
			binary.LittleEndian.PutUint64(p[8:], binary.LittleEndian.Uint64(p[8:])-(cs+4))
		})
		r := mustOpen(t, files)
		if !hasWarning(r, "segment 2, chunk table group 1:") || !hasWarning(r, "chunks from index 5 onward unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 5)
	})
	t.Run("thousands of entries at one compressed chunk", func(t *testing.T) {
		// Duplicate offsets break rule (c): no chunk is served from them.
		const n = 4000
		media := make([]byte, n*512) // one-sector chunks
		copy(media, pattern(512))
		files := ewftest.Build(ewftest.Options{SectorsPerChunk: 1, ChunksPerTable: n, Compress: ewftest.CompressAll}, media)
		patchBoth(t, files[0], 0, func(p []byte) {
			e := entryAt(p, 0)
			for i := range n {
				setEntry(p, i, e)
			}
		})
		start := time.Now()
		r := mustOpen(t, files)
		if !hasWarning(r, "rule (c)") || !hasWarning(r, "chunks from index 0 onward unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		p := make([]byte, 512)
		for c := range n {
			got, err := r.ReadAt(p, int64(c)*512)
			if got != 0 {
				t.Fatalf("chunk %d: %d bytes served", c, got)
			}
			wantChunkError(t, err, int64(c))
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("too slow")
		}
	})
	t.Run("table size and count disagree", func(t *testing.T) {
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, pattern(10*cs))
		patchTable(t, files[0], "table", 0, func(p []byte) {
			binary.LittleEndian.PutUint32(p, 3) // header checksum fixed below
			fixTable(p)
		})
		r := mustOpen(t, files) // table2 is intact
		if !hasWarning(r, "using table2") {
			t.Fatalf("warnings %q", r.Warnings())
		}
	})
}

// failingReaderAt returns err for every read overlapping [from, to).
type failingReaderAt struct {
	data     []byte
	from, to int64
	err      error
}

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < f.to && off+int64(len(p)) > f.from {
		return 0, f.err
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

func TestEWFSegmentReadErrorIsWrapped(t *testing.T) {
	const cs = 64 * 512
	media := pattern(4 * cs)
	files := ewftest.Build(ewftest.Options{ChunksPerTable: 10}, media)
	locs := ewftest.ChunkLocs(files)
	boom := errors.New("boom")
	segs := segsOf(files)
	segs[0].R = failingReaderAt{data: files[0], from: locs[2].Offset, to: locs[2].Offset + locs[2].Length, err: boom}
	r, err := ewf.Open(segs)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 3*cs)
	n, err := r.ReadAt(p, 0)
	if n != 2*cs {
		t.Fatalf("n = %d", n)
	}
	wantChunkError(t, err, 2)
	if !errors.Is(err, boom) {
		t.Fatalf("the I/O error is not reachable with errors.Is: %v", err)
	}
	// A segment that shrinks after Open: the short read is an unexpected EOF.
	segs = segsOf(files)
	sh := &shrinkable{data: files[0]}
	sh.limit.Store(int64(len(files[0])))
	segs[0].R = sh
	r, err = ewf.Open(segs)
	if err != nil {
		t.Fatal(err)
	}
	sh.limit.Store(locs[3].Offset + 10)
	_, err = r.ReadAt(make([]byte, 10), 3*cs)
	wantChunkError(t, err, 3)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%v", err)
	}
}

// shrinkable serves only the first limit bytes of data, as a file truncated
// after Open would.
type shrinkable struct {
	data  []byte
	limit atomic.Int64
}

func (s *shrinkable) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(s.data[:s.limit.Load()]).ReadAt(p, off)
}

func TestEWFReadAfterClose(t *testing.T) {
	const cs = 64 * 512
	media := pattern(4 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{}, media))
	p := make([]byte, 100)
	if n, err := r.ReadAt(p, 0); n != 100 || err != nil {
		t.Fatal(n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		p := bytes.Repeat([]byte{0xAA}, 100)
		n, err := r.ReadAt(p, 0)
		if n != 0 || !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("read after Close = %d, %v", n, err)
		}
		if p[0] != 0xAA {
			t.Fatal("p was touched")
		}
	}
	if r.CacheLen() != 0 {
		t.Fatal("Close must empty the cache")
	}
}

func TestEWFConcurrentReadAt(t *testing.T) {
	const (
		cs     = 64 * 512
		chunks = 40
		bad    = 17
	)
	media := mixedMedia(chunks*cs + 512)
	files := ewftest.Build(ewftest.Options{Compress: ewftest.CompressMixed, ChunksPerSegment: 9, ChunksPerTable: 6}, media)
	flipMiddle(t, files, bad)
	r := mustOpen(t, files)
	r.SetCacheCapacity(5) // force plenty of eviction
	var wg sync.WaitGroup
	errc := make(chan error, 32)
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1)) //nolint:gosec // deterministic test input, not security
			for range 150 {
				off := rng.IntN(len(media) + 100)
				n := 1 + rng.IntN(4*cs)
				p := make([]byte, n)
				got, err := r.ReadAt(p, int64(off))
				end := min(len(media), off+n)
				hitsBad := off < (bad+1)*cs && end > bad*cs && off < len(media)
				switch {
				case !hitsBad:
					want := 0
					if off < len(media) {
						want = end - off
					}
					if got != want || (off+n > len(media)) != (err == io.EOF) || (off+n <= len(media) && err != nil) ||
						(want > 0 && !bytes.Equal(p[:got], media[off:end])) {
						errc <- fmt.Errorf("read %d at %d: %d, %v", n, off, got, err)
						return
					}
				default:
					wantN := max(0, bad*cs-off)
					var ce *ewf.ChunkError
					if got != wantN || !errors.As(err, &ce) || ce.Chunk != bad || !bytes.Equal(p[:got], media[off:off+got]) {
						errc <- fmt.Errorf("read %d at %d over the bad chunk: %d, %v", n, off, got, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}

// dropTables retypes the k-th table and table2 of seg so the opener no longer
// sees them: the sectors section they describe has no chunk table.
func dropTables(t *testing.T, seg []byte, k int) {
	t.Helper()
	for _, typ := range []string{"table", "table2"} {
		ss := tableSections(seg, typ)
		if k >= len(ss) {
			t.Fatalf("no %s #%d", typ, k)
		}
		s := ss[k]
		s.Type = "dropped"
		ewftest.FixDescriptor(seg, s)
	}
}

// TestEWFMissingTableMakesLaterChunksUnreadable: when a chunk table is gone
// from the middle of the set, the chunk numbering after it is unknowable; later
// tables must not be attached to guessed indexes, or those chunks would read
// other, checksum-valid data. Every chunk from the first uncovered index on
// fails; the ones before it still read.
func TestEWFMissingTableMakesLaterChunksUnreadable(t *testing.T) {
	const cs = 64 * 512
	media := pattern(12 * cs) // every chunk differs
	cases := []struct {
		name string
		opt  ewftest.Options
		seg  int // 0-based segment whose group is dropped
		grp  int
		k    int64 // first unreadable chunk
	}{
		{"first group of a middle segment", ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, 1, 0, 4},
		{"last group of a middle segment", ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, 1, 1, 6},
		{"whole table of a middle segment", ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 4}, 1, 0, 4},
		{"middle group of one segment", ewftest.Options{ChunksPerTable: 3}, 0, 1, 3},
		{"compressed", ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2, Compress: ewftest.CompressAll}, 1, 0, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := ewftest.Build(c.opt, media)
			dropTables(t, files[c.seg], c.grp)
			r := mustOpen(t, files)
			want := fmt.Sprintf("segment %d, chunk table group %d: the sectors section at offset ", c.seg+1, c.grp+1)
			tail := fmt.Sprintf("has no chunk table after it; chunks from index %d onward unreadable", c.k)
			n := 0
			for _, w := range r.Warnings() {
				if strings.HasPrefix(w, want) && strings.HasSuffix(w, tail) {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("warnings %q, want exactly one %q ... %q", r.Warnings(), want, tail)
			}
			p := make([]byte, cs)
			for i := range int64(12) {
				got, err := r.ReadAt(p, i*cs)
				if i < c.k {
					if got != cs || err != nil || !bytes.Equal(p, media[i*cs:(i+1)*cs]) {
						t.Fatalf("chunk %d before the gap: %d, %v", i, got, err)
					}
					continue
				}
				if got != 0 {
					t.Fatalf("chunk %d after the gap returned %d bytes", i, got)
				}
				ce := asChunkError(t, err, i)
				if !strings.Contains(ce.Error(), "chunk table group") {
					t.Fatalf("chunk %d: %v", i, err)
				}
			}
			// A read spanning the gap returns the bytes before it and then the error.
			big := make([]byte, 12*cs)
			got, err := r.ReadAt(big, 0)
			wantChunkError(t, err, c.k)
			if int64(got) != c.k*cs || !bytes.Equal(big[:got], media[:got]) {
				t.Fatalf("spanning read returned %d bytes", got)
			}
		})
	}
	t.Run("only the table is missing", func(t *testing.T) {
		// A table2 right after its sectors section alone numbers the chunks
		// (sectors, table2, dropped): no gap.
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, media)
		s := tableSections(files[1], "table")[0]
		s.Type = "table2"
		ewftest.FixDescriptor(files[1], s)
		s = tableSections(files[1], "table2")[1] // [0] is the one just made
		s.Type = "dropped"
		ewftest.FixDescriptor(files[1], s)
		r := mustOpen(t, files)
		if hasWarning(r, "chunks from index") || !hasWarning(r, "has no table") || !bytes.Equal(readAll(t, r), media) {
			t.Fatalf("warnings %q", r.Warnings())
		}
	})
	t.Run("table not directly after its sectors section", func(t *testing.T) {
		// sectors, dropped, table2: the table2 does not follow the sectors
		// section, so rule (a) fails and everything from there is unreadable.
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, media)
		s := tableSections(files[1], "table")[0]
		s.Type = "dropped"
		ewftest.FixDescriptor(files[1], s)
		r := mustOpen(t, files)
		if !hasWarning(r, "segment 2, chunk table group 1: the sectors section at offset") || !hasWarning(r, "has no chunk table after it") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 4)
	})
	t.Run("table group without a sectors section", func(t *testing.T) {
		// dropped, table, table2: rule (a) fails for the group.
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, media)
		s := tableSections(files[1], "table")[1] // the second group's table; its sectors section precedes it
		sectors := ewftest.Sections(files[1])
		for i, x := range sectors {
			if x.Offset == s.Offset {
				sectors[i-1].Type = "dropped"
				ewftest.FixDescriptor(files[1], sectors[i-1])
			}
		}
		r := mustOpen(t, files)
		if !hasWarning(r, "segment 2, chunk table group 2: the table section at offset") || !hasWarning(r, "does not directly follow a sectors section") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		wantUnreadableFrom(t, r, media, cs, 6)
	})
}

// TestEWFDisagreeingTablesNeverServeUnprovenChunks: table and table2 both pass
// their checksums but differ. The copy that satisfies the structural rules is
// used; when both do, the differing chunks are unreadable.
func TestEWFDisagreeingTablesNeverServeUnprovenChunks(t *testing.T) {
	const cs = 64 * 512
	media := pattern(10 * cs)
	build := func() [][]byte {
		return ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
	}
	shift := func(i int, by uint32) func(p []byte) {
		return func(p []byte) { setEntry(p, i, entryAt(p, i)+by); fixTable(p) }
	}
	t.Run("table breaks a rule, table2 passes", func(t *testing.T) {
		files := build()
		patchTable(t, files[0], "table", 0, func(p []byte) { setEntry(p, 2, entryAt(p, 1)); fixTable(p) }) // duplicate
		r := mustOpen(t, files)
		if !hasWarning(r, "rule (c)") || !hasWarning(r, "using table2") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		if !bytes.Equal(readAll(t, r), media) {
			t.Fatal("table2 satisfies the rules and must be used")
		}
	})
	t.Run("table2 breaks a rule, table passes", func(t *testing.T) {
		files := build()
		patchTable(t, files[0], "table2", 1, func(p []byte) { setEntry(p, 2, entryAt(p, 4)); fixTable(p) }) // not increasing
		r := mustOpen(t, files)
		if !hasWarning(r, "rule (c)") || !hasWarning(r, "using table") || !bytes.Equal(readAll(t, r), media) {
			t.Fatalf("warnings %q", r.Warnings())
		}
	})
	t.Run("both satisfy the rules but differ", func(t *testing.T) {
		files := build()
		patchTable(t, files[0], "table", 1, shift(2, 1))
		patchTable(t, files[0], "table2", 1, shift(2, 2))
		r := mustOpen(t, files)
		w := r.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "differ") || !strings.Contains(w[0], "1 differing chunk(s) cannot be read") {
			t.Fatalf("warnings %q", w)
		}
		p := make([]byte, cs)
		for c := range int64(10) {
			got, err := r.ReadAt(p, c*cs)
			if c == 7 { // entry 2 of the second table
				if got != 0 {
					t.Fatalf("disputed chunk returned %d bytes", got)
				}
				ce := asChunkError(t, err, 7)
				if ce.Offset != -1 || !strings.Contains(ce.Error(), "disagree") {
					t.Fatalf("%v", ce)
				}
				continue
			}
			if got != cs || err != nil || !bytes.Equal(p, media[c*cs:(c+1)*cs]) {
				t.Fatalf("chunk %d: %d, %v", c, got, err)
			}
		}
	})
	t.Run("entry counts disagree", func(t *testing.T) {
		// Without footers, table says 4 entries and table2 says 5, both
		// trusted: the numbering after the group is unknown, so chunk 4 and
		// everything after it is unreadable.
		files := ewftest.Build(ewftest.Options{ChunksPerTable: 5, NoTableFooter: true}, media)
		patchTable(t, files[0], "table", 0, func(p []byte) {
			binary.LittleEndian.PutUint32(p, 4)
			fixTable(p)
		})
		r := mustOpen(t, files)
		if !hasWarning(r, "disagree on the entry count (4 and 5), so chunks from index 4 onward are unreadable") {
			t.Fatalf("warnings %q", r.Warnings())
		}
		p := make([]byte, cs)
		for c := range int64(10) {
			got, err := r.ReadAt(p, c*cs)
			if c >= 4 {
				if got != 0 {
					t.Fatalf("chunk %d returned %d bytes", c, got)
				}
				wantChunkError(t, err, c)
				continue
			}
			if got != cs || err != nil || !bytes.Equal(p, media[c*cs:(c+1)*cs]) {
				t.Fatalf("chunk %d: %d, %v", c, got, err)
			}
		}
	})
}

// TestEWFUncompressedChunkBoundedToItsSectorsSection: a raw chunk and its
// checksum must lie inside the sectors section that holds them.
func TestEWFUncompressedChunkBoundedToItsSectorsSection(t *testing.T) {
	const cs = 64 * 512
	media := pattern(5 * cs)
	files := ewftest.Build(ewftest.Options{ChunksPerTable: 5}, media)
	// The last entry stays increasing and inside the payload, but 8 bytes too
	// far for its chunk to fit before the section ends.
	patchBoth(t, files[0], 0, func(p []byte) { setEntry(p, 4, entryAt(p, 4)+8) })
	r := mustOpen(t, files)
	wantUnreadableFrom(t, r, media, cs, 4)
	p := make([]byte, cs)
	_, err := r.ReadAt(p, 4*cs)
	if !strings.Contains(fmt.Sprint(err), "does not fit inside its sectors section") {
		t.Fatalf("%v", err)
	}
}

// TestEWFBytesAfterZlibStreamWarn: a compressed chunk followed, inside its
// table-delimited extent, by bytes that are not part of the stream still reads
// (the stream's own checksum passed) but is flagged.
func TestEWFBytesAfterZlibStreamWarn(t *testing.T) {
	const cs = 64 * 512
	media := pattern(3 * cs)
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	_, _ = zw.Write(media[cs : 2*cs])
	_ = zw.Close()
	stored := append(zb.Bytes(), 0xDE, 0xAD, 0xBE)
	files := ewftest.Build(ewftest.Options{
		Compress: ewftest.CompressAll,
		Override: map[int]ewftest.RawChunk{1: {Data: stored, Compressed: true}},
	}, media)
	r := mustOpen(t, files)
	if len(r.Warnings()) != 0 {
		t.Fatalf("open warnings %q", r.Warnings())
	}
	if !bytes.Equal(readAll(t, r), media) {
		t.Fatal("ReadAt differs from the media")
	}
	if !hasWarning(r, "chunk 1 (segment 1, offset ") || !hasWarning(r, "3 byte(s) follow the end of its zlib stream") {
		t.Fatalf("warnings %q", r.Warnings())
	}
}
