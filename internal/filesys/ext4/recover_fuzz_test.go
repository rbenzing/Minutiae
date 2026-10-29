package ext4_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	fuzzRecoverSeedMax = 64 << 10 // bytes per seed
	fuzzRecoverEntries = 200      // Recoverable calls per input
)

// fuzzRecoverOpts is a 64-block, 1 KiB-block filesystem: small enough for the engine to mutate usefully.
func fuzzRecoverOpts(o ext4test.Options) ext4test.Options {
	o.BlockSize, o.Groups, o.BlocksPerGroup, o.InodesPerGroup = 1024, 1, 56, 32
	return o
}

// fuzzRecoverSeeds are builder images with deleted files in the shapes the recovery reads: debugfs-style and
// kernel-style deletes, a depth-1 tree, a block map with an indirect block, an inline directory, a cross-linked
// directory and a physically overlapping extent pair.
func fuzzRecoverSeeds(t testing.TB) [][]byte {
	t.Helper()
	kernel := ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true, ZeroBlockPointers: true}
	build := func(o ext4test.Options, files []ext4test.File, edit func(img []byte, l *ext4test.Layout)) []byte {
		img, l := ext4test.BuildLayout(fuzzRecoverOpts(o), files)
		if edit != nil {
			edit(img, l)
		}
		if len(img) > fuzzRecoverSeedMax {
			t.Fatalf("seed of %d bytes exceeds %d", len(img), fuzzRecoverSeedMax)
		}
		return img
	}
	rm := func(path string, e ext4test.UnlinkEffects) func([]byte, *ext4test.Layout) {
		return func(img []byte, l *ext4test.Layout) { l.Unlink(img, path, e) }
	}
	gone := func(data []byte) []ext4test.File {
		return []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone", Data: data, Deleted: true}}
	}
	two := ext4test.File{
		Path: "/gone", Deleted: true, ExtentLeaves: 2, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: pattern(2048, 41)}, {Block: 2, Data: pattern(2048, 42)}},
	}
	overlap := ext4test.File{
		Path: "/gone", Deleted: true, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: pattern(3072, 43)}, {Block: 3, Data: pattern(3072, 44)}},
	}
	xo := ext4test.Options{Extents: true, MetadataCsum: true}
	return [][]byte{
		build(xo, gone(pattern(3000, 45)), rm("/gone", ext4test.DebugfsRm)),
		build(xo, gone(pattern(3000, 46)), rm("/gone", kernel)),
		build(ext4test.Options{}, gone(pattern(3000, 47)), rm("/gone", ext4test.DebugfsRm)),
		build(xo, []ext4test.File{two}, rm("/gone", ext4test.DebugfsRm)),
		build(xo, []ext4test.File{two}, rm("/gone", kernel)),                                   // a kernel unlink of a depth-1 tree: only the free leaves remain
		build(ext4test.Options{}, gone(pattern(16*1024, 48)), rm("/gone", ext4test.DebugfsRm)), // an indirect block
		build(ext4test.Options{Extents: true, InlineData: true, MetadataCsum: true}, []ext4test.File{
			{Path: "/idir", Dir: true, Inline: true},
			{Path: "/idir/n1", Data: []byte("n1")},
			{Path: "/idir/n2", Data: []byte("n2"), Deleted: true},
		}, nil),
		build(ext4test.Options{Extents: true}, []ext4test.File{
			{Path: "/a", Dir: true},
			{Path: "/a/a1", Data: []byte("a1")},
			{Path: "/b", Dir: true},
			{Path: "/b/b1", Data: []byte("b1")},
			{Path: "/b/bgone", Data: pattern(1500, 49), Deleted: true},
		}, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/b/bgone", ext4test.DebugfsRm)
			l.ShareDirBlock(img, "/a", "/b")
		}),
		build(xo, []ext4test.File{overlap}, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/gone", ext4test.DebugfsRm)
			l.PatchInode(img, l.File("/gone").Inode, func(raw []byte) {
				putLE32(raw, rawExtEnt0+12+8, uint32(l.File("/gone").Data[0]+1)) // the second extent starts inside the first
			})
		}),
	}
}

// checkRecoverCandidate is the invariant every candidate must keep: it passes CheckCandidate, or it fails only because
// its Size exceeds the filesystem while every run lies inside it (3C-R27).
func checkRecoverCandidate(t *testing.T, c filesys.Candidate, fsSize int64) {
	t.Helper()
	_, err := filesys.CheckCandidate(c, fsSize)
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("candidate %+v fails CheckCandidate: %v", c, err)
	}
	for _, r := range c.Runs {
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > fsSize {
			t.Fatalf("candidate %+v: run %+v outside the filesystem", c, r)
		}
	}
}

func sameAnswer(a []filesys.Candidate, aerr error, b []filesys.Candidate, berr error) bool {
	return reflect.DeepEqual(a, b) && fmt.Sprint(aerr) == fmt.Sprint(berr)
}

// FuzzExt4Recoverable asks Recoverable about the deleted entries of arbitrary bytes: a typed error or valid
// candidates, the same answer twice and for a forged Entry, and no new filesystem warning. The reader is called
// directly, not through detect.
func FuzzExt4Recoverable(f *testing.F) {
	for _, s := range fuzzRecoverSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := ext4.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		_ = fsys.PrepareRecovery(context.Background()) // an I/O error leaves the index unbuilt: the leaf source is then undecided
		var deleted []filesys.Entry
		visited := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > fuzzWalkBudget {
				return errFuzzBudget
			}
			if e.Deleted && len(deleted) < fuzzRecoverEntries {
				deleted = append(deleted, e)
			}
			return nil
		})
		size := fsys.Info().Size
		warnings := len(fsys.Info().Warnings)
		for _, e := range deleted {
			cs, err := fsys.Recoverable(e)
			if err != nil && cs != nil {
				t.Fatalf("%s: candidates and an error together: %v", e.ID, err)
			}
			if len(cs) > filesys.MaxCandidatesPerEntry {
				t.Fatalf("%s: %d candidates", e.ID, len(cs))
			}
			for _, c := range cs {
				checkRecoverCandidate(t, c, size)
			}
			again, aerr := fsys.Recoverable(e)
			if !sameAnswer(cs, err, again, aerr) {
				t.Fatalf("%s: a second call differs: %v %v, then %v %v", e.ID, cs, err, again, aerr)
			}
			forged, ferr := fsys.Recoverable(filesys.Entry{ID: e.ID})
			if !sameAnswer(cs, err, forged, ferr) {
				t.Fatalf("%s: a forged Entry changes the answer: %v %v, then %v %v", e.ID, cs, err, forged, ferr)
			}
			if err != nil && !errors.Is(err, filesys.ErrNotFound) && !errors.Is(err, filesys.ErrNotDeleted) &&
				!errors.Is(err, filesys.ErrReused) && !errors.Is(err, filesys.ErrUndecided) && !errors.Is(err, filesys.ErrLimit) &&
				!errors.Is(err, filesys.ErrIncomplete) && !errors.Is(err, filesys.ErrUnsupported) && !errors.Is(err, filesys.ErrCorrupt) {
				t.Logf("%s: error outside the typed set: %v", e.ID, err) // an I/O error from the bounded reader is allowed
			}
		}
		if w := fsys.Info().Warnings; len(w) != warnings {
			t.Fatalf("Recoverable added warnings: %v", w[warnings:])
		}
	})
}

// The seeds must reach the candidate code, or the fuzz target checks nothing.
func TestFuzzRecoverSeedsEmitCandidates(t *testing.T) {
	withCands := 0
	leaf := false
	for i, s := range fuzzRecoverSeeds(t) {
		fsys, err := ext4.Open(bytes.NewReader(s), int64(len(s)))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		_ = fsys.PrepareRecovery(context.Background())
		got := false
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err == nil && e.Deleted {
				if cs, _ := fsys.Recoverable(e); len(cs) > 0 {
					got = true
					leaf = leaf || slices.ContainsFunc(cs, func(c filesys.Candidate) bool { return c.Method == ext4.MethodExtentLeaf })
				}
			}
			return nil
		})
		if got {
			withCands++
		}
	}
	if !leaf {
		t.Error("no seed yields an ext4-extent-leaf candidate")
	}
	if withCands < 5 {
		t.Errorf("only %d seeds yield a candidate", withCands)
	}
}
