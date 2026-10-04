package ewf_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func TestEWFCacheLRU(t *testing.T) {
	const cs = 8 * 512
	media := pattern(100 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{SectorsPerChunk: 8}, media))
	read := func(c int) {
		t.Helper()
		p := make([]byte, 16)
		if n, err := r.ReadAt(p, int64(c*cs)); n != 16 || err != nil || !bytes.Equal(p, media[c*cs:c*cs+16]) {
			t.Fatalf("chunk %d: %d, %v", c, n, err)
		}
	}
	if r.CacheCapacity() != 64 {
		t.Fatalf("default capacity %d, want 64", r.CacheCapacity())
	}
	for c := range 100 {
		read(c)
	}
	if r.CacheLen() != 64 || r.CacheHits() != 0 {
		t.Fatalf("len %d hits %d after 100 distinct chunks", r.CacheLen(), r.CacheHits())
	}
	read(99) // cached
	read(36) // the oldest survivor
	read(35) // evicted: a miss, not a hit
	if r.CacheHits() != 2 || r.CacheLen() != 64 {
		t.Fatalf("hits %d len %d", r.CacheHits(), r.CacheLen())
	}

	// Least recently used goes first.
	r.SetCacheCapacity(3)
	if r.CacheLen() != 3 {
		t.Fatalf("len %d after shrinking", r.CacheLen())
	}
	h := r.CacheHits()
	for _, c := range []int{0, 1, 2} {
		read(c)
	}
	read(0) // hit; order, most recent first: 0 2 1
	read(3) // evicts 1
	if r.CacheHits() != h+1 {
		t.Fatalf("hits %d, want %d", r.CacheHits(), h+1)
	}
	read(0)
	read(2)
	read(3)
	if r.CacheHits() != h+4 {
		t.Fatalf("0, 2 and 3 must be cached: hits %d, want %d", r.CacheHits(), h+4)
	}
	read(1) // was evicted
	if r.CacheHits() != h+4 || r.CacheLen() != 3 {
		t.Fatalf("1 must have been evicted: hits %d len %d", r.CacheHits(), r.CacheLen())
	}
	r.SetCacheCapacity(0) // never below one entry
	if r.CacheLen() != 1 || r.CacheCapacity() != 1 {
		t.Fatalf("len %d cap %d", r.CacheLen(), r.CacheCapacity())
	}
}

func TestEWFCacheCapacityShrinksForLargeChunks(t *testing.T) {
	for chunk, want := range map[int64]int{
		512: 64, 32 << 10: 64, 1 << 20: 64, 2 << 20: 64, 4 << 20: 32, 8 << 20: 16, 16 << 20: 8, 1 << 30: 1, 0: 1,
	} {
		if got := ewf.CacheCapacityFor(chunk); got != want {
			t.Errorf("capacity for %d-byte chunks = %d, want %d", chunk, got, want)
		}
	}
	// A real image with 16 MiB chunks (one 4096-byte sector, short last chunk).
	r := mustOpen(t, ewftest.Build(ewftest.Options{BytesPerSector: 4096, SectorsPerChunk: 4096}, pattern(4096)))
	if r.ChunkSize() != 16<<20 || r.CacheCapacity() != 8 {
		t.Fatalf("chunk %d capacity %d", r.ChunkSize(), r.CacheCapacity())
	}
}

func TestEWFCachedChunksAreNotAliasedToCallers(t *testing.T) {
	const cs = 64 * 512
	media := pattern(2 * cs)
	r := mustOpen(t, ewftest.Build(ewftest.Options{}, media))
	p := make([]byte, 64)
	if _, err := r.ReadAt(p, 0); err != nil {
		t.Fatal(err)
	}
	for i := range p {
		p[i] = 0 // a caller scribbling on its buffer must not reach the cache
	}
	if _, err := r.ReadAt(p, 0); err != nil || !bytes.Equal(p, media[:64]) {
		t.Fatal("cached chunk was modified through a caller's buffer")
	}
}
