package ext4_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Offsets inside the raw inode: i_block at 0x28, an extent header of 12 bytes, then 12-byte entries.
const (
	rawIBlock  = 0x28
	rawExtEnt0 = rawIBlock + 12
)

type hostileRow struct {
	name  string
	build func(t *testing.T) (*ext4.FS, []byte)
	// stop is the stop the row must report (a prefix of the token); "" means any outcome that is typed and bounded.
	stop string
	// bigSize marks the i_size 2^62 row (3C-R27).
	bigSize bool
}

// scattered is a 6-block file in two scattered pieces: two extents in i_block.
func scattered() ext4test.File {
	data := pattern(6*1024, 21)
	return ext4test.File{
		Path: "/gone.bin", Deleted: true, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*1024]}, {Block: 3, Data: data[3*1024:]}},
	}
}

func extentRow(name, stop string, edit func(raw []byte, l *ext4test.Layout)) hostileRow {
	return hostileRow{name: name, stop: stop, build: func(t *testing.T) (*ext4.FS, []byte) {
		f, img, _ := recImageWith(t, csumExt, []ext4test.File{scattered()}, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
			l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) { edit(raw, l) })
		})
		return f, img
	}}
}

func blockMapRow(name, stop string, nblocks int, edit func(img []byte, l *ext4test.Layout)) hostileRow {
	return hostileRow{name: name, stop: stop, build: func(t *testing.T) (*ext4.FS, []byte) {
		files := []ext4test.File{{Path: "/gone.bin", Data: pattern(nblocks*1024, 22), Deleted: true}}
		f, img, _ := recImageWith(t, ext4test.Options{}, files, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
			edit(img, l)
		})
		return f, img
	}}
}

func hostileRows() []hostileRow {
	return []hostileRow{
		extentRow("tree depth 9", "map-damaged@0", func(raw []byte, _ *ext4test.Layout) { putLE16(raw, rawIBlock+6, 9) }),
		extentRow("entries beyond eh_max", "map-damaged@", func(raw []byte, _ *ext4test.Layout) { putLE16(raw, rawIBlock+2, 200) }),
		extentRow("extent beyond the filesystem", "map-damaged@3", func(raw []byte, _ *ext4test.Layout) {
			putLE32(raw, rawExtEnt0+12+8, 0x7FFFFFF0) // the second extent's ee_start_lo
		}),
		extentRow("extents overlapping physically", "map-overlap@3", func(raw []byte, l *ext4test.Layout) {
			putLE32(raw, rawExtEnt0+12+8, uint32(l.File("/gone.bin").Data[0]+1)) // the second extent starts inside the first
		}),
		extentRow("lblk going backwards", "map-damaged@3", func(raw []byte, _ *ext4test.Layout) { putLE32(raw, rawExtEnt0+12, 1) }),
		extentRow("extent length 65535 (uninitialized)", "uninit@0", func(raw []byte, _ *ext4test.Layout) { putLE16(raw, rawExtEnt0+4, 65535) }),
		extentRow("zero-length extent", "map-damaged@0", func(raw []byte, _ *ext4test.Layout) { putLE16(raw, rawExtEnt0+4, 0) }),
		blockMapRow("indirect block lists itself", "map-overlap@", 20, func(img []byte, l *ext4test.Layout) {
			ind := l.File("/gone.bin").Tree[0]
			putLE32(img[ind*1024:], 0, uint32(ind))
		}),
		blockMapRow("indirect block named twice", "map-overlap@", 600, func(img []byte, l *ext4test.Layout) {
			dind, child := l.File("/gone.bin").Tree[1], l.File("/gone.bin").Tree[2]
			putLE32(img[dind*1024:], 4, uint32(child)) // dind[1] = dind[0]
		}),
		blockMapRow("two indirect blocks in a loop", "map-", 600, func(img []byte, l *ext4test.Layout) {
			dind, child := l.File("/gone.bin").Tree[1], l.File("/gone.bin").Tree[2]
			putLE32(img[dind*1024:], 4, uint32(dind)) // dind[1] = dind
			_ = child
		}),
		blockMapRow("pointer beyond the filesystem", "map-damaged@", 20, func(img []byte, l *ext4test.Layout) {
			putLE32(img[l.File("/gone.bin").Tree[0]*1024:], 8, 0x7FFFFFF0)
		}),
		{name: "i_size 2^62", bigSize: true, build: func(t *testing.T) (*ext4.FS, []byte) {
			f, img, _ := recImageWith(t, csumExt, []ext4test.File{scattered()}, func(img []byte, l *ext4test.Layout) {
				l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
				l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) {
					putLE32(raw, 0x4, 0)
					putLE32(raw, 0x6C, 1<<30) // i_size_high: the size is 2^62
				})
			})
			return f, img
		}},
	}
}

func TestRecoverableHostileStaleMaps(t *testing.T) {
	for _, r := range hostileRows() {
		t.Run(r.name, func(t *testing.T) {
			f, _ := r.build(t)
			e := delEntry(t, f, "/gone.bin")
			info := f.Info()
			fsBlocks := info.Size / int64(info.BlockSize)
			cs, err := f.Recoverable(e)
			if w := f.MapWork(); w > 16*fsBlocks {
				t.Errorf("walk work %d exceeds 16 x %d blocks", w, fsBlocks)
			}
			if err != nil {
				if !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUndecided) && !errors.Is(err, filesys.ErrLimit) {
					t.Fatalf("untyped error %v", err)
				}
				return
			}
			if len(cs) == 0 {
				t.Fatalf("%s: no candidate and no error: a damaged map must say what stopped it", r.name)
			}
			for _, c := range cs {
				covered, cerr := filesys.CheckCandidate(c, info.Size)
				if r.bigSize {
					if c.Size != 1<<62 {
						t.Errorf("size = %d, want 2^62 (not clamped)", c.Size)
					}
					if cerr == nil || !strings.Contains(cerr.Error(), "exceeds") {
						t.Errorf("CheckCandidate = %v, want the size-exceeds-the-filesystem failure", cerr)
					}
					for _, run := range c.Runs {
						if run.Offset < 0 || run.Offset+run.Length > info.Size {
							t.Errorf("run %+v lies outside the filesystem", run)
						}
					}
					continue
				}
				if cerr != nil {
					t.Fatalf("CheckCandidate: %v (%+v)", cerr, c)
				}
				if covered > c.Size {
					t.Errorf("covered %d exceeds size %d", covered, c.Size)
				}
				if r.stop != "" && !strings.HasPrefix(stopOf(c), r.stop) {
					t.Errorf("stop = %q, want prefix %q", stopOf(c), r.stop)
				}
			}
		})
	}
}

func TestRecoverableAddsNoFilesystemWarnings(t *testing.T) {
	for _, r := range hostileRows() {
		t.Run(r.name, func(t *testing.T) {
			f, _ := r.build(t)
			var deleted []filesys.Entry
			for _, e := range readDir(t, f, "/") {
				if e.Deleted {
					deleted = append(deleted, e)
				}
			}
			if len(deleted) == 0 {
				t.Fatal("no deleted entry")
			}
			before := slices.Clone(f.Info().Warnings)
			for _, e := range deleted {
				_, _ = f.Recoverable(e)
			}
			if after := f.Info().Warnings; strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Errorf("warnings changed:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

// An extent map that outgrows the run cap stops with runs-cap, never grows without bound.
func TestRecoverableRunCapStops(t *testing.T) {
	// 8 pieces under a depth-1 tree: more extents than the lowered cap of 3 runs
	var ps []ext4test.Piece
	for i := range 8 {
		ps = append(ps, ext4test.Piece{Block: i, Data: pattern(1024, byte(30+i))})
	}
	f := ext4test.File{Path: "/gone.bin", Deleted: true, Scatter: true, Pieces: ps, ExtentLeaves: 2}
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	fsys.SetMapStaleCaps(3)
	c := oneCandidate(t, fsys, "/gone.bin")
	if !strings.HasPrefix(stopOf(c), "runs-cap@3") || len(c.Runs) != 3 {
		t.Fatalf("stop %q with %d runs, want runs-cap@3 with 3", stopOf(c), len(c.Runs))
	}
}

// Concurrent callers share one free-space set and one directory index and agree.
func TestRecoverableConcurrentCallsAgree(t *testing.T) {
	f, _ := twoLeafFile()
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) { l.Unlink(img, "/gone.bin", ext4test.DebugfsRm) })
	e := delEntry(t, fsys, "/gone.bin")
	want, err := fsys.Recoverable(e)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan string, 8)
	for range 8 {
		go func() {
			got, err := fsys.Recoverable(e)
			if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
				errs <- fmt.Sprintf("%v %+v", err, got)
				return
			}
			errs <- ""
		}()
	}
	for range 8 {
		if s := <-errs; s != "" {
			t.Error(s)
		}
	}
}

// The returned slices belong to the caller.
func TestRecoverableReturnsFreshSlices(t *testing.T) {
	f, _ := twoLeafFile()
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) { l.Unlink(img, "/gone.bin", ext4test.DebugfsRm) })
	e := delEntry(t, fsys, "/gone.bin")
	a, _ := fsys.Recoverable(e)
	if len(a) == 0 {
		t.Fatal("no candidate")
	}
	a[0].Runs[0].Offset = -5
	a[0].Assumptions[0] = "scribble"
	b, _ := fsys.Recoverable(e)
	if len(b) == 0 {
		t.Fatal("no candidate")
	}
	if b[0].Runs[0].Offset < 0 || b[0].Assumptions[0] == "scribble" {
		t.Fatal("a caller's edit reached the next answer")
	}
}

// The free-space set comes from Unallocated, which warns about a damaged bitmap on the live path; the recovery path
// must not add that warning again (3C-R5), and the damaged group's blocks are not free, so the map stops there.
func TestRecoverableFreeSetAddsNoFilesystemWarning(t *testing.T) {
	f, _ := twoLeafFile()
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		img[l.BlockBitmap[0]*l.BlockSize+60] ^= 0x01 // a bit of the block bitmap, checksum not refreshed
	})
	before := slices.Clone(fsys.Info().Warnings)
	c := oneCandidate(t, fsys, "/gone.bin")
	if after := fsys.Info().Warnings; strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("warnings changed:\nbefore %q\nafter  %q", before, after)
	}
	if got := stopOf(c); !strings.HasPrefix(got, "map-block-not-free@") {
		t.Errorf("stop = %q: with no usable bitmap no map block is known to be free", got)
	}
}
