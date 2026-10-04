package ewf_test

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // comparing stored hashes
	"crypto/sha1" //nolint:gosec // comparing stored hashes
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func hexHashes(media []byte) (md5hex, sha1hex string) {
	m, s := md5.Sum(media), sha1.Sum(media) //nolint:gosec // comparing stored hashes
	return hex.EncodeToString(m[:]), hex.EncodeToString(s[:])
}

func verify(t *testing.T, r *ewf.Reader) ewf.VerifyResult {
	t.Helper()
	res, err := r.Verify(context.Background(), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

func TestVerifyMatch(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10*cs + 3*512)
	for name, comp := range map[string]ewftest.Compress{"mixed": ewftest.CompressMixed, "all": ewftest.CompressAll, "none": ewftest.CompressNone} {
		t.Run(name, func(t *testing.T) {
			files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 3, Compress: comp}, media)
			if len(files) != 3 {
				t.Fatalf("%d segments", len(files))
			}
			r := mustOpen(t, files)
			res := verify(t, r)
			m, s := hexHashes(media)
			if res.Result() != "match" || res.Size != int64(len(media)) || res.BytesHashed != res.Size || res.BadChunk != -1 || res.BadChunkError != "" {
				t.Fatalf("%+v", res)
			}
			if res.MD5 != (ewf.HashCheck{Stored: m, Computed: m, Status: ewf.HashMatch}) || res.SHA1 != (ewf.HashCheck{Stored: s, Computed: s, Status: ewf.HashMatch}) {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestVerifyMismatchAndUnverified(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10 * cs)
	m, s := hexHashes(media)
	t.Run("stored md5 differs", func(t *testing.T) {
		flipped, _ := hex.DecodeString(m)
		flipped[0] ^= 1
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, MD5Override: flipped}, media)
		res := verify(t, mustOpen(t, files))
		if res.Result() != "mismatch" || res.MD5.Status != ewf.HashMismatch || res.MD5.Stored != hex.EncodeToString(flipped) || res.MD5.Computed != m {
			t.Fatalf("%+v", res)
		}
		if res.SHA1.Status != ewf.HashMatch || res.SHA1.Computed != s {
			t.Fatalf("sha1 must still match: %+v", res)
		}
	})
	t.Run("corrupt chunk", func(t *testing.T) {
		bad := bytes.Repeat([]byte{0x55}, cs+4) // right length, wrong Adler-32
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, Override: map[int]ewftest.RawChunk{5: {Data: bad}}}, media)
		res, err := mustOpen(t, files).Verify(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Result() != "unverified" || res.BadChunk != 5 || res.BadChunkError == "" || res.BytesHashed != 5*cs {
			t.Fatalf("%+v", res)
		}
		if res.MD5 != (ewf.HashCheck{Stored: m, Status: ewf.HashUnverified}) || res.SHA1 != (ewf.HashCheck{Stored: s, Status: ewf.HashUnverified}) {
			t.Fatalf("hashes of partial data must not be reported: %+v", res)
		}
	})
	t.Run("media changed with valid chunk checksums", func(t *testing.T) {
		// Every chunk decodes and checksums fine, but the stored hashes are
		// those of the old media: only the hash comparison can tell.
		other := slices.Clone(media)
		other[4*cs+17] ^= 0xFF
		om, osha := hexHashes(other)
		mb, sb := md5.Sum(media), sha1.Sum(media) //nolint:gosec // comparing stored hashes
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, MD5Override: mb[:], SHA1Override: sb[:]}, other)
		r := mustOpen(t, files)
		if !bytes.Equal(readAll(t, r), other) {
			t.Fatal("chunks must read")
		}
		res := verify(t, r)
		if res.Result() != "mismatch" || res.MD5.Status != ewf.HashMismatch || res.SHA1.Status != ewf.HashMismatch ||
			res.MD5.Computed != om || res.SHA1.Computed != osha || res.MD5.Stored != m || res.SHA1.Stored != s {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("missing table", func(t *testing.T) {
		// Chunks after a missing table are unreadable, so nothing is compared.
		files := ewftest.Build(ewftest.Options{ChunksPerSegment: 4, ChunksPerTable: 2}, media)
		dropTables(t, files[1], 0)
		res := verify(t, mustOpen(t, files))
		if res.Result() != "unverified" || res.BadChunk != 4 || res.MD5.Computed != "" || res.SHA1.Computed != "" {
			t.Fatalf("%+v", res)
		}
	})
}

func TestVerifyAbsentHashes(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(6 * cs)
	m, s := hexHashes(media)
	t.Run("no hash and no digest", func(t *testing.T) {
		res := verify(t, mustOpen(t, ewftest.Build(ewftest.Options{NoHash: true, NoDigest: true}, media)))
		if res.Result() != "absent" || res.MD5.Status != ewf.HashAbsent || res.SHA1.Status != ewf.HashAbsent || res.MD5.Stored != "" || res.SHA1.Stored != "" {
			t.Fatalf("%+v", res)
		}
		if res.MD5.Computed != m || res.SHA1.Computed != s || res.BytesHashed != res.Size {
			t.Fatalf("the computed values are still reported: %+v", res)
		}
	})
	t.Run("hash only", func(t *testing.T) {
		res := verify(t, mustOpen(t, ewftest.Build(ewftest.Options{NoDigest: true}, media)))
		if res.Result() != "match" || res.MD5.Status != ewf.HashMatch || res.SHA1.Status != ewf.HashAbsent || res.MD5.Computed != m {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("digest only", func(t *testing.T) {
		res := verify(t, mustOpen(t, ewftest.Build(ewftest.Options{NoHash: true}, media)))
		if res.Result() != "match" || res.MD5.Status != ewf.HashMatch || res.SHA1.Status != ewf.HashMatch {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("absent but unreadable", func(t *testing.T) {
		bad := bytes.Repeat([]byte{1}, cs+4)
		files := ewftest.Build(ewftest.Options{NoHash: true, NoDigest: true, Override: map[int]ewftest.RawChunk{2: {Data: bad}}}, media)
		res := verify(t, mustOpen(t, files))
		if res.Result() != "unverified" || res.BadChunk != 2 || res.MD5.Status != ewf.HashAbsent {
			t.Fatalf("%+v", res)
		}
	})
}

func TestVerifyCancel(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{ChunksPerSegment: 4}, media))
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	res, err := r.Verify(ctx, func(_, _ int64) {
		calls++
		if calls == 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if res.BytesHashed != 3*cs || res.BytesHashed >= res.Size || calls != 3 || res.BadChunk != -1 {
		t.Fatalf("%+v after %d progress calls", res, calls)
	}
	if res.Result() != "unverified" || res.MD5.Computed != "" || res.SHA1.Computed != "" || res.MD5.Status != ewf.HashUnverified {
		t.Fatalf("a partial verify must not compare anything: %+v", res)
	}
	t.Run("already cancelled", func(t *testing.T) {
		res, err := r.Verify(ctx, nil)
		if !errors.Is(err, context.Canceled) || res.BytesHashed != 0 {
			t.Fatalf("%+v, %v", res, err)
		}
	})
}

func TestVerifyProgressMonotonic(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(9*cs + 512)
	r := mustOpen(t, ewftest.Build(ewftest.Options{ChunksPerSegment: 4}, media))
	var seen []int64
	res, err := r.Verify(context.Background(), func(done, total int64) {
		if total != int64(len(media)) {
			t.Errorf("total = %d", total)
		}
		seen = append(seen, done)
	})
	if err != nil || res.Result() != "match" {
		t.Fatalf("%+v, %v", res, err)
	}
	if len(seen) != 10 || seen[len(seen)-1] != int64(len(media)) {
		t.Fatalf("progress %v", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("progress not increasing: %v", seen)
		}
	}
}

func TestVerifyDoesNotUseCache(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{ChunksPerSegment: 4}, media))
	r.SetCacheCapacity(2)
	p := make([]byte, 10)
	for _, c := range []int{7, 2} {
		if _, err := r.ReadAt(p, int64(c*cs)); err != nil {
			t.Fatal(err)
		}
	}
	lenBefore, hitsBefore := r.CacheLen(), r.CacheHits()
	if res := verify(t, r); res.Result() != "match" {
		t.Fatalf("%+v", res)
	}
	if r.CacheLen() != lenBefore || r.CacheHits() != hitsBefore {
		t.Fatalf("cache len %d -> %d, hits %d -> %d", lenBefore, r.CacheLen(), hitsBefore, r.CacheHits())
	}
	// The working set survived: both chunks are still cache hits.
	for _, c := range []int{7, 2} {
		if _, err := r.ReadAt(p, int64(c*cs)); err != nil {
			t.Fatal(err)
		}
	}
	if r.CacheHits() != hitsBefore+2 {
		t.Fatalf("hits = %d, want %d: verify evicted the working set", r.CacheHits(), hitsBefore+2)
	}
}

// TestVerifyHostileHugeSize: a volume section that claims a 4 TiB media (the
// most chunks the reader accepts) over a few chunks of data. Verify stops at
// the first chunk without a table entry (or when cancelled) and never decodes
// or allocates anything near the claimed size.
func TestVerifyHostileHugeSize(t *testing.T) {
	const bps, spc = 512, 128
	const cs = bps * spc
	const chunks = 1 << 26 // 2^26 chunks of 64 KiB = 4 TiB
	media := pattern(8 * cs)
	files := ewftest.Build(ewftest.Options{BytesPerSector: bps, SectorsPerChunk: spc, Compress: ewftest.CompressAll}, media)
	v := section(t, files[0], "volume")
	p := files[0][v.Offset+76 : v.Offset+v.Size]
	binary.LittleEndian.PutUint32(p[4:], chunks)
	binary.LittleEndian.PutUint64(p[16:], uint64(chunks)*spc)
	ewftest.FixAdler(p, 0, 1048, 1048)
	r := mustOpen(t, files)
	if r.Size() != 4<<40 || r.Chunks() != chunks {
		t.Fatalf("size %d, chunks %d", r.Size(), r.Chunks())
	}
	run := func(name string, progress func(cancel context.CancelFunc, done int64)) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			var res ewf.VerifyResult
			var err error
			if progress == nil {
				res, err = r.Verify(ctx, nil)
			} else {
				res, err = r.Verify(ctx, func(done, _ int64) { progress(cancel, done) })
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			if elapsed > 10*time.Second {
				t.Fatalf("Verify took %v", elapsed)
			}
			if used := after.TotalAlloc - before.TotalAlloc; used > 64<<20 {
				t.Fatalf("Verify allocated %d bytes", used)
			}
			if res.BytesHashed > 8*cs || res.Result() != "unverified" || res.MD5.Computed != "" {
				t.Fatalf("%+v", res)
			}
			if progress == nil && (err != nil || res.BadChunk != 8) {
				t.Fatalf("%+v, %v", res, err)
			}
			if progress != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	run("no table entries past the data", nil)
	run("cancelled from progress", func(cancel context.CancelFunc, done int64) {
		if done >= 2*cs {
			cancel()
		}
	})
}

// TestVerifyRealFixtures: Verify over real acquisition-tool output computes
// the oracle's hashes and finds the stored ones equal.
func TestVerifyRealFixtures(t *testing.T) {
	exp := loadFixtureExpect(t)
	for _, v := range exp.Variants {
		t.Run(v.Name, func(t *testing.T) {
			var segs []ewf.Segment
			for _, f := range v.Files {
				data := gunzipFile(t, f.Name+".gz")
				segs = append(segs, ewf.Segment{Name: f.Name, R: bytes.NewReader(data), Size: int64(len(data))})
			}
			r, err := ewf.Open(segs)
			if err != nil {
				t.Fatal(err)
			}
			res := verify(t, r)
			if res.Result() != "match" || res.MD5.Status != ewf.HashMatch || res.SHA1.Status != ewf.HashMatch || res.BadChunk != -1 || res.BytesHashed != v.MediaSize {
				t.Fatalf("%+v", res)
			}
			if res.MD5.Computed != v.StoredMD5 || res.MD5.Stored != v.StoredMD5 || res.SHA1.Computed != v.StoredSHA1 || res.SHA1.Stored != v.StoredSHA1 {
				t.Fatalf("%+v; oracle md5 %s sha1 %s", res, v.StoredMD5, v.StoredSHA1)
			}
			if v.MediaSize == exp.Raw.Size && (res.MD5.Computed != exp.Raw.MD5 || res.SHA1.Computed != exp.Raw.SHA1) {
				t.Fatal("computed hashes differ from the oracle's raw image hashes")
			}
			if w := r.Warnings(); len(w) != 0 {
				t.Fatalf("warnings after verifying a clean fixture: %q", w)
			}
		})
	}
}

// TestVerifyPartialIsUnverifiedWithoutStoredHashes: a cancelled run that hashed
// only part of the media is "unverified" even when the container stores no hash
// (both statuses are then absent), never "absent".
func TestVerifyPartialIsUnverifiedWithoutStoredHashes(t *testing.T) {
	const cs = 64 * 512
	media := mixedMedia(10 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{NoHash: true, NoDigest: true}, media))
	ctx, cancel := context.WithCancel(context.Background())
	res, err := r.Verify(ctx, func(done, _ int64) {
		if done >= 2*cs {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || res.BytesHashed != 2*cs || res.BadChunk != -1 {
		t.Fatalf("%+v, %v", res, err)
	}
	if res.MD5.Status != ewf.HashAbsent || res.SHA1.Status != ewf.HashAbsent {
		t.Fatalf("statuses %+v", res)
	}
	if got := res.Result(); got != "unverified" {
		t.Fatalf("Result() = %q, want unverified", got)
	}
	if full, err := r.Verify(context.Background(), nil); err != nil || full.Result() != "absent" {
		t.Fatalf("complete run: %+v, %v", full, err)
	}
}

func TestVerifyAfterCloseFails(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{}, pattern(2*64*512)))
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(context.Background(), nil); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("err = %v, want fs.ErrClosed", err)
	}
}
