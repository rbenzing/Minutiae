package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// slackMarked returns n bytes whose every block starts with a marker unique to (seed, block index), so a test can
// find each block in the image bytes without asking the reader.
func slackMarked(bs, n, seed int) []byte {
	b := bytes.Repeat([]byte{byte(0xC0 + seed)}, n)
	for i := 0; i*bs < n; i++ {
		if n-i*bs >= slackMarkLen { // a shorter tail keeps the file's fill byte as its marker
			copy(b[i*bs:], fmt.Appendf(nil, "MRK%03d-%05d|", seed, i))
		}
	}
	return b
}

const slackMarkLen = 13

// slackBlockRuns finds the whole-block runs of the listed content blocks of data by scanning img for their
// markers; it reads nothing through the reader.
func slackBlockRuns(t *testing.T, img []byte, bs int, data []byte, blocks []int) []filesys.Run {
	t.Helper()
	var out []filesys.Run
	for _, i := range blocks {
		m := data[i*bs : i*bs+min(slackMarkLen, len(data)-i*bs)]
		var at []int
		for off := 0; off+bs <= len(img); off += bs {
			if bytes.HasPrefix(img[off:], m) {
				at = append(at, off)
			}
		}
		if len(at) != 1 {
			t.Fatalf("the marker of block %d is in the image %d times, want 1", i, len(at))
		}
		out = append(out, filesys.Run{Offset: int64(at[0]), Length: int64(bs)})
	}
	return out
}

func slackMerge(in []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range in {
		if k := len(out); k > 0 && out[k-1].Offset+out[k-1].Length == r.Offset {
			out[k-1].Length += r.Length
			continue
		}
		out = append(out, r)
	}
	return out
}

func slackOpen(t *testing.T, fsys *ext4.FS, path string) filesys.File {
	t.Helper()
	e, err := fsys.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", path, err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return f
}

func slackAlloc(t *testing.T, f filesys.File) []filesys.Run {
	t.Helper()
	a, ok := filesys.AllocatedRunsOf(f)
	if !ok {
		t.Fatal("the file does not implement filesys.AllocatedRunner")
	}
	return a
}

func slackSeq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func TestExt4AllocatedRunsWholeBlocks(t *testing.T) {
	for _, bs := range []int{1024, 4096} {
		for _, extents := range []bool{true, false} {
			t.Run(fmt.Sprintf("bs%d-extents%v", bs, extents), func(t *testing.T) {
				var files []ext4test.File
				datas := map[string][]byte{}
				for i, n := range []int{1, bs - 1, bs, bs + 1, 3*bs + 5} {
					p := fmt.Sprintf("/f%d.bin", i)
					datas[p] = slackMarked(bs, n, i)
					files = append(files, ext4test.File{Path: p, Data: datas[p]})
				}
				sc := slackMarked(bs, 6*bs, 7)
				var ps []ext4test.Piece
				for i := range 3 {
					ps = append(ps, ext4test.Piece{Block: 2 * i, Data: sc[2*i*bs : (2*i+2)*bs]})
				}
				files = append(files, ext4test.File{Path: "/scatter.bin", Pieces: ps, Scatter: true})
				datas["/scatter.bin"] = sc
				if extents {
					lv := slackMarked(bs, 8*bs, 8)
					var lp []ext4test.Piece
					for i := range 4 {
						lp = append(lp, ext4test.Piece{Block: 3 * i, Data: lv[2*i*bs : (2*i+2)*bs]})
					}
					files = append(files, ext4test.File{Path: "/leaves.bin", Pieces: lp, Scatter: true, ExtentLeaves: 2})
					datas["/leaves.bin"] = lv
				}
				files = append(files, ext4test.File{Path: "/empty.bin"})
				img := ext4test.Build(ext4test.Options{BlockSize: bs, Extents: extents}, files)
				fsys := mustOpen(t, img)
				if !filesys.ReportsSlack(fsys) {
					t.Error("the filesystem does not declare SlackReporter")
				}
				for p, data := range datas {
					want := slackMerge(slackBlockRuns(t, img, bs, data, slackSeq((len(data)+bs-1)/bs)))
					fl := slackOpen(t, fsys, p)
					got := slackAlloc(t, fl)
					if !slices.Equal(slackMerge(got), want) {
						t.Errorf("%s: AllocatedRuns = %v, the image holds the blocks %v", p, got, want)
					}
					if (p == "/scatter.bin" || p == "/leaves.bin") && len(want) < 3 {
						t.Errorf("%s: only %d physical runs: the test lost its point", p, len(want))
					}
					if len(got) > 0 {
						orig := slices.Clone(got)
						got[0] = filesys.Run{Offset: -1, Length: -1}
						if again := slackAlloc(t, fl); !slices.Equal(again, orig) {
							t.Errorf("%s: a second call gave %v after the first answer was changed, want %v", p, again, orig)
						}
					}
				}
				if got := slackAlloc(t, slackOpen(t, fsys, "/empty.bin")); got != nil {
					t.Errorf("a 0-byte file reports %v, want nil", got)
				}
			})
		}
	}
}

func TestExt4InlineFileHasNoAllocation(t *testing.T) {
	img := ext4test.Build(ext4test.Options{BlockSize: 4096, Extents: true, InlineData: true}, []ext4test.File{
		{Path: "/tiny", Data: []byte("inline"), Inline: true},
		{Path: "/link", Symlink: "tiny"},
	})
	fsys := mustOpen(t, img)
	for _, p := range []string{"/tiny", "/link"} {
		if got := slackAlloc(t, slackOpen(t, fsys, p)); got != nil {
			t.Errorf("%s: AllocatedRuns = %v, want nil", p, got)
		}
	}
	fl := slackOpen(t, fsys, "/tiny")
	if _, out, _ := filesys.TailSlack(fl.Size(), fl.Runs(), slackAlloc(t, fl), 4096, fsys.Info().Size); out != filesys.SlackNoRuns {
		t.Errorf("inline file outcome %q, want no-runs", out)
	}
}

func TestExt4SparseTailIsTrailingHole(t *testing.T) {
	const bs = 1024
	for _, extents := range []bool{true, false} {
		t.Run(fmt.Sprintf("extents%v", extents), func(t *testing.T) {
			data := slackMarked(bs, 2*bs+100, 3)
			d := slackMarked(bs, 2*bs, 4)
			img := ext4test.Build(ext4test.Options{BlockSize: bs, Extents: extents}, []ext4test.File{
				{Path: "/sparse", Data: data, Size: int64(len(data)) + 5*bs},
				{Path: "/partial", Pieces: []ext4test.Piece{{Block: 0, Data: d[:bs]}, {Block: 3, Data: d[bs : bs+20]}}},
			})
			fsys := mustOpen(t, img)
			size := fsys.Info().Size
			f := slackOpen(t, fsys, "/sparse")
			if _, out, why := filesys.TailSlack(f.Size(), f.Runs(), slackAlloc(t, f), bs, size); out != filesys.SlackTrailingHole {
				t.Errorf("sparse tail: outcome %q (%s), want trailing-hole", out, why)
			}
			// a hole in the middle with data in the last block: the slack of that block
			f = slackOpen(t, fsys, "/partial")
			if f.Size() != 3*bs+20 {
				t.Fatalf("size %d", f.Size())
			}
			last := slackBlockRuns(t, img, bs, d, []int{1})[0]
			run, out, why := filesys.TailSlack(f.Size(), f.Runs(), slackAlloc(t, f), bs, size)
			want := filesys.Run{Offset: last.Offset + 20, Length: bs - 20}
			if out != filesys.SlackFound || run != want {
				t.Errorf("TailSlack = %v %q (%s), want %v slack", run, out, why, want)
			}
		})
	}
}

func TestExt4UninitExtentIsNotAllocatedSlack(t *testing.T) {
	const bs = 1024
	d := slackMarked(bs, 3*bs, 6)
	img := ext4test.Build(ext4test.Options{BlockSize: bs, Extents: true}, []ext4test.File{
		{Path: "/pre", Pieces: []ext4test.Piece{{Block: 0, Data: d[:bs]}, {Block: 1, Data: d[bs : 2*bs], Uninit: true}}, Size: 2*bs + 7},
	})
	fsys := mustOpen(t, img)
	f := slackOpen(t, fsys, "/pre")
	alloc := slackAlloc(t, f)
	if first := slackBlockRuns(t, img, bs, d, []int{0}); !slices.Equal(alloc, first) {
		t.Errorf("AllocatedRuns = %v, want only the initialized block %v (preallocated blocks are never reported)", alloc, first)
	}
	if _, out, why := filesys.TailSlack(f.Size(), f.Runs(), alloc, bs, fsys.Info().Size); out != filesys.SlackTrailingHole {
		t.Errorf("outcome %q (%s), want trailing-hole", out, why)
	}
}

func TestExt4TruncatedImageAllocatedRunsArePrefixUnits(t *testing.T) {
	img, _ := ttScatterImage()
	runs := ttRuns(t, img, "/f")
	cut := runs[2].Offset + ttBS + 100
	f, fl := ttOpenCut(t, img, cut, "/f")
	alloc := slackAlloc(t, fl)
	if want := filesys.UnitRuns(fl.Runs(), ttBS); len(want) == 0 || !slices.Equal(alloc, want) {
		t.Errorf("AllocatedRuns = %v, want the unit runs of the prefix %v", alloc, want)
	}
	if len(alloc) == 0 {
		t.Fatal("no allocation reported for the prefix")
	}
	if last := alloc[len(alloc)-1]; last.Offset+last.Length > cut {
		t.Errorf("the allocation ends at %d, past the cut %d", last.Offset+last.Length, cut)
	}
	if _, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), alloc, ttBS, f.Info().Size); out != filesys.SlackPrefix {
		t.Errorf("outcome %q (%s), want prefix", out, why)
	}
}

func ext4ContractRun(t *testing.T, img []byte) {
	t.Helper()
	fsys := mustOpen(t, img)
	fstest.AllocatedRunsContract(t, fsys)
}

func TestExt4AllocatedRunsContract(t *testing.T) {
	names := []string{"ext4-4k-csum", "ext4-1k-blockmap-ext2", "ext4-inline", "ext4-1k-metabg-uninit"}
	seen := 0
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		if !slices.Contains(names, name) {
			continue
		}
		seen++
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != want.Generator.ImageSHA256 {
				t.Fatal("the image does not match the oracle's generator.image_sha256: regenerate the fixtures")
			}
			ext4ContractRun(t, img)
		})
	}
	if seen != len(names) {
		t.Fatalf("%d of %d fixtures found", seen, len(names))
	}
	t.Run("builder", func(t *testing.T) {
		const bs = 1024
		d := slackMarked(bs, 5*bs+3, 9)
		ext4ContractRun(t, ext4test.Build(ext4test.Options{BlockSize: bs, Extents: true, Groups: 2}, []ext4test.File{
			{Path: "/d", Dir: true},
			{Path: "/d/a", Data: d},
			{Path: "/d/s", Data: d, Size: int64(len(d)) + 4*bs},
			{Path: "/d/p", Pieces: []ext4test.Piece{{Block: 0, Data: d[:bs]}, {Block: 1, Data: d[bs : 2*bs], Uninit: true}}},
			{Path: "/l", Symlink: "d/a"},
		}))
	})
	t.Run("blockmap", func(t *testing.T) {
		d := slackMarked(1024, 20*1024+9, 2)
		ext4ContractRun(t, ext4test.Build(ext4test.Options{BlockSize: 1024}, []ext4test.File{{Path: "/f", Data: d}}))
	})
}
