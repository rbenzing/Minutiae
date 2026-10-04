package ewf_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

const (
	fuzzReadBudget   = 8 << 20 // bytes read sequentially per input
	fuzzReadChunk    = 64 << 10
	fuzzVerifyBudget = 8 << 20 // Verify is cancelled once this much has been hashed
	fuzzRandomReads  = 16
)

// fuzzSplit turns an input into 1-3 segments: the first byte selects how many,
// the rest is split evenly, so cross-references between segments are fuzzed.
func fuzzSplit(b []byte) []ewf.Segment {
	if len(b) == 0 {
		return nil
	}
	n := 1 + int(b[0])%3
	rest := b[1:]
	per := (len(rest) + n - 1) / n
	var segs []ewf.Segment
	for i := range n {
		lo, hi := min(i*per, len(rest)), min((i+1)*per, len(rest))
		part := rest[lo:hi]
		segs = append(segs, ewf.Segment{Name: fmt.Sprintf("fuzz.E%02d", i+1), R: bytes.NewReader(part), Size: int64(len(part))})
	}
	return segs
}

// fuzzJoin is the inverse of fuzzSplit for seeds: segments padded to one length.
func fuzzJoin(segs [][]byte) []byte {
	per := 0
	for _, s := range segs {
		per = max(per, len(s))
	}
	out := []byte{byte(len(segs) - 1)}
	for _, s := range segs {
		out = append(out, s...)
		out = append(out, make([]byte, per-len(s))...)
	}
	return out
}

// checkRead enforces the io.ReaderAt contract on one read and returns the
// result for the determinism check.
func checkRead(t *testing.T, r *ewf.Reader, off int64, want int) (data []byte, errText string) {
	t.Helper()
	p := make([]byte, want)
	n, err := r.ReadAt(p, off)
	switch {
	case n < 0 || n > len(p):
		t.Fatalf("ReadAt(%d bytes at %d) returned n = %d", len(p), off, n)
	case n < len(p) && err == nil:
		t.Fatalf("ReadAt(%d bytes at %d) returned a short read (%d) without an error", len(p), off, n)
	case errors.Is(err, io.EOF) && off+int64(n) < r.Size():
		t.Fatalf("ReadAt(%d bytes at %d) returned io.EOF before the end (%d of %d)", len(p), off, off+int64(n), r.Size())
	}
	if err != nil {
		errText = err.Error()
	}
	return p[:n], errText
}

// FuzzEWFOpen runs the reader over arbitrary bytes: Open of 1-3 segments, then
// ReadAt (sequential, random-looking and tail reads, each held to the
// io.ReaderAt contract and read twice for determinism) and a Verify cancelled
// after 8 MiB. A panic anywhere fails the fuzz.
func FuzzEWFOpen(f *testing.F) {
	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		segs := fuzzSplit(b)
		if segs == nil {
			return
		}
		r, err := ewf.Open(segs)
		if err != nil {
			return
		}
		defer func() { _ = r.Close() }()
		_ = r.Metadata()
		_ = r.Warnings()
		size := r.Size()
		if size < 0 {
			t.Fatalf("Size = %d", size)
		}

		reads := 0
		read := func(off int64, want int) {
			if off < 0 || want <= 0 {
				return
			}
			reads++
			d1, e1 := checkRead(t, r, off, want)
			d2, e2 := checkRead(t, r, off, want)
			if !bytes.Equal(d1, d2) || e1 != e2 {
				t.Fatalf("read of %d bytes at %d is not deterministic: %d bytes %q, then %d bytes %q", want, off, len(d1), e1, len(d2), e2)
			}
		}
		for off := int64(0); off < min(size, fuzzReadBudget); off += fuzzReadChunk {
			read(off, fuzzReadChunk)
		}
		h := fnv.New64a()
		_, _ = h.Write(b)
		x := h.Sum64()
		for range fuzzRandomReads {
			x = x*6364136223846793005 + 1442695040888963407
			if size > 0 {
				read(int64(x>>1)%size, 1+int(x>>40)%(2*fuzzReadChunk))
			}
		}
		read(max(size-512, 0), 4096)         // the last bytes of the image
		read(size, 16)                       // at the end: no bytes, io.EOF
		read(size+int64(x%1024), 16)         // past the end
		_, _ = r.ReadAt(make([]byte, 8), -1) // a negative offset must be an error, not a panic

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res, _ := r.Verify(ctx, func(done, _ int64) {
			if done >= fuzzVerifyBudget {
				cancel()
			}
		})
		if res.BytesHashed < 0 || res.BytesHashed > res.Size {
			t.Fatalf("Verify hashed %d of %d bytes", res.BytesHashed, res.Size)
		}
		if res.Size != size {
			t.Fatalf("Verify size %d, Reader size %d", res.Size, size)
		}
		for name, h := range map[string]ewf.HashCheck{"MD5": res.MD5, "SHA1": res.SHA1} {
			if (h.Status == ewf.HashMatch || h.Status == ewf.HashMismatch) && res.BytesHashed != res.Size {
				t.Fatalf("%s is %q after hashing %d of %d bytes", name, h.Status, res.BytesHashed, res.Size)
			}
		}
		if res.Result() == "match" && res.BytesHashed != res.Size {
			t.Fatalf("match after hashing %d of %d bytes", res.BytesHashed, res.Size)
		}
	})
}

// fuzzMedia is small media with structure (zeros, text, noise).
func fuzzMedia(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		switch i / 512 % 3 {
		case 0:
		case 1:
			b[i] = byte('a' + i%26)
		default:
			b[i] = byte(i*131 + i/7)
		}
	}
	return b
}

// fuzzSeedSets are the builder images the fuzzer starts from: every Options
// variant, small media, one to three segments.
func fuzzSeedSets() map[string][][]byte {
	small := fuzzMedia(8 * 512)
	multi := fuzzMedia(12 * 512)
	sets := map[string][][]byte{}
	for name, o := range map[string]ewftest.Options{
		"none":            {SectorsPerChunk: 2},
		"all":             {SectorsPerChunk: 2, Compress: ewftest.CompressAll},
		"mixed":           {SectorsPerChunk: 2, Compress: ewftest.CompressMixed},
		"no-table2":       {SectorsPerChunk: 2, NoTable2: true},
		"no-footer":       {SectorsPerChunk: 2, NoTableFooter: true},
		"base-zero":       {SectorsPerChunk: 2, Base: ewftest.BaseZero},
		"no-header":       {SectorsPerChunk: 2, NoHeader: true},
		"no-header2":      {SectorsPerChunk: 2, NoHeader2: true},
		"header2-no-bom":  {SectorsPerChunk: 2, Header2NoBOM: true},
		"no-hash":         {SectorsPerChunk: 2, NoHash: true},
		"no-digest":       {SectorsPerChunk: 2, NoDigest: true},
		"no-data":         {SectorsPerChunk: 2, NoData: true},
		"hash-first":      {SectorsPerChunk: 2, HashBeforeDigest: true},
		"terminal-76":     {SectorsPerChunk: 2, TerminalSize76: true},
		"no-done":         {SectorsPerChunk: 2, NoDone: true},
		"error-ranges":    {SectorsPerChunk: 2, ErrorRanges: [][2]uint32{{1, 1}, {3, 2}}},
		"md5-mismatch":    {SectorsPerChunk: 2, MD5Override: bytes.Repeat([]byte{1}, 16)},
		"two-per-table":   {SectorsPerChunk: 2, ChunksPerTable: 2},
		"sector-4096":     {BytesPerSector: 4096, SectorsPerChunk: 1},
		"headers":         {SectorsPerChunk: 2, Case: "C", Evidence: "E", Description: "d", Examiner: "x", Notes: "n", Acquired: "1700000000"},
		"override-bad":    {SectorsPerChunk: 2, Override: map[int]ewftest.RawChunk{1: {Data: bytes.Repeat([]byte{0x55}, 1028)}}},
		"override-zlib":   {SectorsPerChunk: 2, Compress: ewftest.CompressAll, Override: map[int]ewftest.RawChunk{2: {Data: []byte{0x78, 0x9c, 0, 1, 2}, Compressed: true}}},
		"segments-2":      {SectorsPerChunk: 2, ChunksPerSegment: 6},
		"segments-3":      {SectorsPerChunk: 2, ChunksPerSegment: 4, Compress: ewftest.CompressMixed},
		"segments-3-base": {SectorsPerChunk: 2, ChunksPerSegment: 4, Base: ewftest.BaseZero},
	} {
		media := small
		if o.ChunksPerSegment > 0 {
			media = multi
		}
		sets[name] = ewftest.Build(o, media)
	}
	sets["empty-media"] = ewftest.Build(ewftest.Options{SectorsPerChunk: 2}, nil)
	return sets
}

func fuzzSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	var seeds [][]byte
	for _, segs := range fuzzSeedSets() {
		seeds = append(seeds, fuzzJoin(segs))
	}
	seeds = append(seeds, nil, []byte{0}, append([]byte{0}, "EVF\x09\x0d\x0a\xff\x00\x01\x01\x00\x00\x00"...))
	small := gunzipFile(tb, "ewf-seed-small.E01.gz")
	seeds = append(seeds, fuzzJoin([][]byte{small}))
	return seeds
}

// TestFuzzBuilderSeedsOpen: every builder seed is a valid input for the
// splitter that opens and reads (except the deliberately truncated ones, which
// must still not panic), so the fuzzer starts from real structure.
func TestFuzzBuilderSeedsOpen(t *testing.T) {
	mustOpen := map[string]bool{}
	for name := range fuzzSeedSets() {
		mustOpen[name] = name != "no-done" // an image without its last section may be refused
	}
	for name, segs := range fuzzSeedSets() {
		t.Run(name, func(t *testing.T) {
			joined := fuzzJoin(segs)
			r, err := ewf.Open(fuzzSplit(joined))
			if err != nil {
				if mustOpen[name] {
					t.Fatalf("Open: %v", err)
				}
				return
			}
			defer func() { _ = r.Close() }()
			p := make([]byte, r.Size())
			if n, err := r.ReadAt(p, 0); name != "override-bad" && name != "override-zlib" && (int64(n) != r.Size() || (err != nil && !errors.Is(err, io.EOF))) {
				t.Fatalf("ReadAt: %d of %d bytes, %v", n, r.Size(), err)
			}
		})
	}
	// The real fixture seed opens, reads and verifies.
	small := gunzipFile(t, "ewf-seed-small.E01.gz")
	r, err := ewf.Open(fuzzSplit(fuzzJoin([][]byte{small})))
	if err != nil {
		t.Fatalf("seed-small: %v", err)
	}
	if n, err := r.ReadAt(make([]byte, r.Size()), 0); int64(n) != r.Size() || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("seed-small read: %d, %v", n, err)
	}
}
