package apfs_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // bytes read from the start of each file (larger files are read up to here)
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzLookups    = 64      // Lookup calls per input (each walks the directories on its path)
	fuzzReadChunk  = 64 << 10

	// fuzzSeedMax bounds a seed. The fuzz engine hands each corpus entry to its
	// worker in a text form that spends up to four bytes per byte (a zero byte is
	// written as a four-character escape) in a 100 MiB shared buffer, so a seed
	// above about 25 MiB makes the engine panic before it runs anything.
	fuzzSeedMax = 23 << 20
)

var errFuzzBudget = errors.New("walk budget reached")

// fuzzFiles is a volume tree that touches every kind of object and record the
// reader handles: nested directories, an empty file, small and multi-block data,
// a fragmented file with a hole and a sparse tail, a clone, a symlink (inline
// and stream), hard links (by file id and by sibling id), extended attributes
// (inline and stream), a 255-byte name and a compressed-flag file.
func fuzzFiles() []apfstest.File {
	return []apfstest.File{
		{Path: "/docs", Dir: true, Mode: 0o750, UID: 501, GID: 20},
		{Path: "/docs/a.txt", Data: []byte("hello"), Mode: 0o640, UID: 501, GID: 20, Times: apfstest.Times{Create: 1_600_000_000_000_000_001, Modify: 1_600_000_000_000_000_002, Change: 1_600_000_000_000_000_003, Access: 1_600_000_000_000_000_004}},
		{Path: "/docs/b.bin", Data: pattern(9*bs+17, 2)},
		{Path: "/docs/link", LinkTo: "/docs/a.txt"},
		{Path: "/docs/sibling", LinkTo: "/docs/a.txt", LinkSibling: true},
		{Path: "/sym", Symlink: "docs/a.txt"},
		{Path: "/symstream", Symlink: "docs/b.bin", SymlinkStream: true},
		{Path: "/empty"},
		{Path: "/frag.bin", Data: pattern(8*bs, 3), Fragments: 4},
		{Path: "/split.bin", Data: pattern(6*bs, 4), SplitContig: 3},
		{Path: "/sparse.bin", Data: pattern(3*bs, 5), Fragments: 2, Holes: [][2]int64{{bs, bs}}, SparseTail: 2 * bs},
		{Path: "/orig.bin", Data: pattern(4*bs, 6)},
		{Path: "/clone.bin", Clone: "/orig.bin"},
		{Path: "/x/y/z", Dir: true},
		{Path: "/x/y/attrs", Data: []byte("a"), Xattrs: []apfstest.Xattr{{Name: "com.apple.quarantine", Value: []byte("q")}, {Name: "stream", Value: []byte("v"), Stream: true}}},
		{Path: "/" + strings.Repeat("n", 255), Data: []byte("long name")},
		{Path: "/compressed", Data: pattern(50, 7), CompressedFlag: true, UncompressedSize: 9999, Xattrs: []apfstest.Xattr{{Name: "com.apple.decmpfs", Value: decmpfs(3)}}},
	}
}

// fuzzSeedOptions are the builder options of the seeds: synthetic containers covering the on-disk variants:
// several volumes (plain and hashed directory-record keys, case-insensitive and
// -sensitive), an encrypted volume, snapshots, every file layout above, a
// multi-level file-system tree, a sealed volume, two checkpoints with a CIB
// address layer, 64 KiB blocks, and a two-chunk space manager.
func fuzzSeedOptions() []apfstest.Options {
	rich := apfstest.Volume{
		Name: "Data", UUID: uuidOf(1), Role: roleData, CaseInsensitive: true, NormInsensitive: true, HashedKeys: true,
		Files: fuzzFiles(), TreeMaxKeys: 5,
		Snapshots: []apfstest.Snapshot{
			{Name: "S", Files: []apfstest.File{{Path: "/docs/a.txt", Data: []byte("old")}, {Path: "/gone.bin", Data: pattern(2*bs, 8)}}},
			{Name: "T", Files: []apfstest.File{{Path: "/t", Data: pattern(bs, 9)}}},
		},
	}
	plain := apfstest.Volume{Name: "Plain", UUID: uuidOf(2), Role: roleSystem, Files: []apfstest.File{{Path: "/Ünï.txt", Data: []byte("u")}, {Path: "/Dir", Dir: true}, {Path: "/Dir/f", Data: pattern(2*bs, 10)}}}
	enc := apfstest.Volume{Name: "Enc", UUID: uuidOf(3), Encrypted: true, Files: []apfstest.File{{Path: "/secret", Data: pattern(bs, 11)}}}
	sealed := apfstest.Volume{Name: "Sealed", UUID: uuidOf(4), Sealed: true, Files: []apfstest.File{{Path: "/s", Data: []byte("sealed")}}}

	return []apfstest.Options{
		{Blocks: 2048, Xid: 40, Volumes: []apfstest.Volume{rich, plain, enc}},
		{Blocks: 2048, Xid: 40, Checkpoints: 2, ChunksPerCIB: 1, CibsPerCAB: 1, Volumes: []apfstest.Volume{rich, sealed}},
		{BlockSize: 65536, Blocks: 256, Volumes: []apfstest.Volume{plain}},
		{Blocks: 2048, Volumes: []apfstest.Volume{{Name: "Hostile", UUID: uuidOf(5), Files: []apfstest.File{
			{Path: "/badhash", Data: []byte("b"), BadHash: true}, {Path: "/ghost", NoInode: true}, {Path: "/ok", Data: []byte("ok")},
		}}}},
		{Blocks: 32768 + 1000, Xid: 40, ChunksPerCIB: 1, CibsPerCAB: 1, Volumes: []apfstest.Volume{rich}}, // two chunks; the second has no bitmap block
	}
}

// fuzzBuilderSeeds are the seeds built from fuzzSeedOptions, each cut after its
// last non-zero byte.
func fuzzBuilderSeeds() [][]byte {
	var out [][]byte
	for _, o := range fuzzSeedOptions() {
		out = append(out, trimTail(apfstest.Build(o)))
	}
	return out
}

// fuzzForgedIDs are canonical-looking IDs that were never produced by a listing:
// inodes at the edges of the id space and the reserved range, views of snapshots
// that may not exist, and volume slots past the container. Open and ReadDir must
// reject or serve them without a panic whatever the image holds.
var fuzzForgedIDs = []string{
	"apfs:root", "snaps:0", "snaps:1", "snaps:99",
	"n:0:0:1", "n:0:0:2", "n:0:0:3", "n:0:0:16", "n:0:0:17", "n:0:0:1000000", "n:0:0:1152921504606846975",
	"n:0:2:2", "n:0:5:2", "n:0:18446744073709551615:2", "n:1:0:2", "n:2:0:2", "n:99:0:2", "n:0:0:0",
}

// fuzzWalk is the whole exercise of one image: Walk (volumes and .snapshots
// included) with Open and a bounded read of every file, Lookup of the paths it
// found, forged IDs, a snapshot path and Unallocated. t fails on a panic of the
// reader (callers use the reader directly, not through detect) or a violated
// contract.
func fuzzWalk(t *testing.T, fsys *apfs.FS) {
	t.Helper()
	_ = fsys.Info()
	visited, readTotal, lookups := 0, int64(0), 0
	_ = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			return nil
		}
		if visited++; visited > fuzzWalkBudget {
			return errFuzzBudget
		}
		if lookups < fuzzLookups {
			lookups++
			if got, lerr := fsys.Lookup(p); lerr == nil && got.ID == "" {
				t.Fatalf("Lookup(%q) returned an entry without an ID", p)
			}
		}
		if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
			return nil
		}
		file, err := fsys.Open(e)
		if err != nil || readTotal >= fuzzReadBudget {
			return nil
		}
		checkFuzzRuns(t, e.Name, file, fsys.Info().Size)
		end := min(file.Size(), fuzzFileMax) // only the first 1 MiB of a large file
		buf := make([]byte, min(end, fuzzReadChunk))
		for off := int64(0); off < end; off += int64(len(buf)) {
			n, rerr := file.ReadAt(buf, off)
			readTotal += int64(n)
			if rerr != nil {
				break
			}
		}
		return nil
	})
	for _, id := range fuzzForgedIDs {
		_, _ = fsys.Open(filesys.Entry{ID: id})
		_, _ = fsys.ReadDir(filesys.Entry{ID: id})
	}
	_, _ = fsys.SnapshotPath("/Data/docs/a.txt", "S")
	_, _ = fsys.SnapshotPath("/", "2")
	runs, err := fsys.Unallocated()
	if err != nil {
		return
	}
	size := fsys.Info().Size
	for _, r := range runs {
		if r.Length <= 0 || r.Offset < 0 || r.Offset+r.Length > size {
			t.Fatalf("unallocated run %+v outside the %d-byte filesystem", r, size)
		}
	}
}

// FuzzAPFSOpen runs the reader over arbitrary bytes: Open, a bounded Walk
// (volumes and .snapshots included), Open and read of every file (the first
// 1 MiB of a large one), Lookup of the paths the walk found, forged IDs, a
// snapshot path and Unallocated. It calls the apfs functions directly (not
// through detect.OpenWith), so a panic fails the fuzz.
//
// Run the 60 s gate as
//
//	go test ./internal/filesys/apfs -run '^$' -fuzz FuzzAPFSOpen -fuzztime 60s -fuzzminimizetime 0 -parallel 3
func FuzzAPFSOpen(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, name := range orcFixtures {
		f.Add(trimmedFixture(f, name))
	}
	f.Add(make([]byte, 4096))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := apfs.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		fuzzWalk(t, fsys)
	})
}

// checkFuzzRuns checks the File.Runs contract of a file the reader opened: the
// runs cover exactly [0, Size()) or, when the allocation is truncated or
// corrupt, a valid strict prefix of it, in which case a read at the end of the
// prefix must fail with an error wrapping filesys.ErrCorrupt. A file with no run
// at all is either inline (its bytes live elsewhere, so the read succeeds) or
// corrupt from its first byte (ErrCorrupt).
func checkFuzzRuns(t *testing.T, name string, file filesys.File, fsSize int64) {
	t.Helper()
	runs, size := file.Runs(), file.Size()
	covered, err := filesys.CheckRunsPrefix(runs, size, fsSize)
	if err != nil {
		t.Fatalf("runs of %s violate the File.Runs contract: %v", name, err)
	}
	if covered == size {
		if err := filesys.CheckRuns(runs, size, fsSize); err != nil { // the exact-cover rule
			t.Fatalf("runs of %s violate the File.Runs contract: %v", name, err)
		}
		return
	}
	n, err := file.ReadAt(make([]byte, 1), covered)
	if len(runs) == 0 && n == 1 && err == nil {
		return // data that has no runs of its own (for instance a symlink target in an attribute)
	}
	if n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("read of %s at the end of its %d-byte run prefix (size %d) = %d, %v; want 0 bytes and ErrCorrupt", name, covered, size, n, err)
	}
}

// The seeds (builder images and trimmed real fixtures) must be images the reader
// accepts and lists, or the fuzz
// would start from nothing.
func TestFuzzBuilderSeedsOpen(t *testing.T) {
	for i, s := range fuzzBuilderSeeds() {
		fsys, err := apfs.Open(bytes.NewReader(s), int64(len(s)))
		if err != nil {
			t.Errorf("seed %d: %v", i, err)
			continue
		}
		n := 0
		if err := filesys.Walk(fsys, fsys.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
			if err != nil && !errors.Is(err, filesys.ErrEncrypted) {
				return err
			}
			n++
			return nil
		}); err != nil || n < 5 {
			t.Errorf("seed %d: walk visited %d entries, err %v", i, n, err)
		}
		if i == 0 && n < 30 {
			t.Errorf("seed 0 (the rich one) lists only %d entries", n)
		}
	}
	// The trimmed real fixtures are seeds too: they must still open (a fixture change
	// that stopped them would leave dead seeds) and list their volume.
	for _, name := range orcFixtures {
		orcSkipShort(t, name)
		s := trimmedFixture(t, name)
		fsys, err := apfs.Open(bytes.NewReader(s), int64(len(s)))
		if err != nil {
			t.Errorf("trimmed fixture %s: %v", name, err)
			continue
		}
		if es, err := fsys.ReadDir(fsys.Root()); err != nil || len(es) == 0 {
			t.Errorf("trimmed fixture %s: root lists %d entries, err %v", name, len(es), err)
		}
	}
}

// gunzipTestdata returns the decompressed fixture testdata/<name>.img.gz.
func gunzipTestdata(t testing.TB, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// trimmedFixture returns a real fixture cut after its last non-zero byte (plus
// a block of slack) and in any case after fuzzSeedMax bytes. The reader accepts
// an image shorter than its block count, so the cut image is still a valid seed.
func trimmedFixture(t testing.TB, name string) []byte {
	t.Helper()
	img := gunzipTestdata(t, name)
	return trimTail(img[:min(len(img), fuzzSeedMax)])
}

// trimTail cuts an image after its last non-zero byte plus a block's worth of
// slack: whatever follows is unused space, and the fuzz engine's throughput
// collapses on multi-megabyte inputs.
func trimTail(img []byte) []byte {
	end := len(img)
	for end > 0 && img[end-1] == 0 {
		end--
	}
	return img[:min(len(img), end+4096)]
}

// mutateBlocks damages the object blocks of img (those with a valid checksum:
// B-tree nodes, object maps, the space manager, volume superblocks) one at a
// time, in order: a few bytes of the block are replaced, two thirds of them
// within the header and the first records where the structure lives, and, when
// seal is set, the block's checksum is recomputed so the damage gets past the
// checksum to the parsers behind it. fn is called with each mutated copy
// (reused: it must not keep it).
func mutateBlocks(img []byte, bsz int, rng *rand.Rand, perBlock int, seal bool, fn func(blk uint64, b []byte)) {
	work := bytes.Clone(img)
	for blk := 0; (blk+1)*bsz <= len(img); blk++ {
		src := img[blk*bsz : (blk+1)*bsz]
		copy(work[blk*bsz:(blk+1)*bsz], src)
		apfstest.Seal(work, bsz, uint64(blk))
		if !bytes.Equal(work[blk*bsz:blk*bsz+8], src[:8]) || !slices.ContainsFunc(src[8:], func(c byte) bool { return c != 0 }) {
			copy(work[blk*bsz:(blk+1)*bsz], src)
			continue // not an object block (file data, a bitmap, or unused)
		}
		for range perBlock {
			copy(work[blk*bsz:(blk+1)*bsz], src)
			b := work[blk*bsz : (blk+1)*bsz]
			for range 1 + rng.IntN(3) {
				off := rng.IntN(len(b))
				if rng.IntN(3) != 0 {
					off = 8 + rng.IntN(min(len(b)-8, 256))
				}
				b[off] = byte(rng.IntN(256))
			}
			if seal {
				apfstest.Seal(work, bsz, uint64(blk))
			}
			fn(uint64(blk), work)
		}
		copy(work[blk*bsz:(blk+1)*bsz], src)
	}
}

// Damage to any block of a container, volumes and snapshots included, is a
// warning or an error, never a panic or a runaway: every non-zero block of the
// rich builder container is damaged in turn (half of the mutations re-seal the
// block's checksum) and the whole container walked.
func TestOpenMutatedNeverPanics(t *testing.T) {
	seed := apfstest.Build(fuzzSeedOptions()[0])
	rng := rand.New(rand.NewPCG(21, 22)) //nolint:gosec // deterministic test input, not security
	perBlock := 4
	if testing.Short() {
		perBlock = 1
	}
	n, opened := 0, 0
	for _, seal := range []bool{false, true} {
		mutateBlocks(seed, bs, rng, perBlock, seal, func(_ uint64, img []byte) {
			n++
			fsys, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				return
			}
			opened++
			fuzzWalk(t, fsys)
		})
	}
	if n < 200 || opened < n/4 {
		t.Errorf("%d mutations, only %d of them opened: the test is not reaching the parsers", n, opened)
	}
}

// outcome classifies how far the reader got with a damaged image: "rejected by
// a checksum" (the damage was caught at the block that holds it), "deep" (the
// block's checksum was valid and a parser behind it found the damage: a corrupt
// error that is not a checksum error, or a warning), or "silent".
func outcome(t *testing.T, img []byte) string {
	t.Helper()
	fsys, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		if _, ok := apfs.IsChecksumError(err); ok {
			return "checksum"
		}
		return "deep"
	}
	sawChecksum, deep := false, false
	_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
		if err != nil {
			if errors.Is(err, filesys.ErrEncrypted) {
				return nil // the builder's encrypted volume
			}
			if _, ok := apfs.IsChecksumError(err); ok {
				sawChecksum = true
			} else {
				deep = true
			}
			return nil
		}
		if e.Type == filesys.TypeFile {
			if fl, oerr := fsys.Open(e); oerr == nil {
				_, _ = fl.ReadAt(make([]byte, 64), 0)
			}
		}
		return nil
	})
	for _, w := range fsys.Info().Warnings {
		switch {
		case strings.Contains(w, "checksum"):
			sawChecksum = true
		case strings.Contains(w, "truncated image"):
		default:
			deep = true
		}
	}
	switch {
	case deep:
		return "deep"
	case sawChecksum:
		return "checksum"
	}
	return "silent"
}

// The mutator must reach the parsers behind the checksum: without re-sealing
// almost every damaged metadata block is rejected at its checksum and nothing
// else is exercised; with re-sealing a large share of the damage is found by
// the deeper parsers (B-tree nodes, records, extents, the space manager) and
// the checksum rejects (almost) nothing.
func TestFuzzSealedMutationsReachDeepParsers(t *testing.T) {
	seed := apfstest.Build(fuzzSeedOptions()[0])
	count := func(seal bool) (res map[string]int) {
		res = map[string]int{}
		rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // deterministic test input, not security
		mutateBlocks(seed, bs, rng, 4, seal, func(_ uint64, img []byte) { res[outcome(t, img)]++ })
		return res
	}
	unsealed, sealed := count(false), count(true)
	total := func(m map[string]int) int { return m["checksum"] + m["deep"] + m["silent"] }
	t.Logf("unsealed: %v; sealed: %v", unsealed, sealed)
	if total(unsealed) < 150 || total(sealed) < 150 {
		t.Fatalf("too few mutations: %d unsealed, %d sealed", total(unsealed), total(sealed))
	}
	if unsealed["checksum"]*100 < total(unsealed)*40 {
		t.Errorf("without re-sealing only %d of %d mutations were rejected by a checksum", unsealed["checksum"], total(unsealed))
	}
	if sealed["deep"]*100 < total(sealed)*15 {
		t.Errorf("with re-sealing only %d of %d mutations reached a parser behind the checksum", sealed["deep"], total(sealed))
	}
	if sealed["checksum"] > unsealed["checksum"]/4 {
		t.Errorf("re-sealing left %d checksum rejections (%d without it)", sealed["checksum"], unsealed["checksum"])
	}
}
