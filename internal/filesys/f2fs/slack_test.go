package f2fs_test

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
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// slackMarked returns n bytes whose every full block starts with a marker unique to (seed, block index), so
// every block of the content differs from every other block.
func slackMarked(n, seed int) []byte {
	b := bytes.Repeat([]byte{byte(0xC0 + seed)}, n)
	for i := 0; i*bs < n; i++ {
		if n-i*bs >= 16 {
			copy(b[i*bs:], fmt.Appendf(nil, "MRK%03d-%05d|", seed, i))
		}
	}
	return b
}

// slackBuilt is one file laid out by the allocator, with the block address of each logical block the builder
// placed (independent of the reader).
type slackBuilt struct {
	fsys *f2fs.FS
	img  []byte
	addr map[int64]uint32 // logical block -> absolute block address
}

// slackFile builds an image holding one regular file (nid 5) and finds each logical block's address from the
// DataBlock the allocator emitted for it (matched by content, which differs per block).
func slackFile(t *testing.T, stride uint32, size int64, d f2fstest.FileData) slackBuilt {
	t.Helper()
	o := f2fstest.Options{Segments: 16}
	a := f2fstest.NewAlloc(o, firstNode)
	a.Stride = stride
	nodes, data := a.File(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(size)}, d)
	o.Nodes, o.Data = nodes, data
	img := f2fstest.Build(o, nil)
	addr := map[int64]uint32{}
	for l, c := range d.Blocks {
		n := 0
		for _, db := range data {
			if bytes.Equal(db.Data, c) {
				addr[l] = db.Addr
				n++
			}
		}
		if n != 1 {
			t.Fatalf("test bug: logical block %d matches %d data blocks", l, n)
		}
	}
	return slackBuilt{fsys: mustOpen(t, img), img: img, addr: addr}
}

// wantRuns lists the whole-block runs of the logical blocks, in logical order, adjacent runs merged.
func (s slackBuilt) wantRuns(blocks ...int64) []filesys.Run {
	slices.Sort(blocks)
	var out []filesys.Run
	for _, l := range blocks {
		out = append(out, filesys.Run{Offset: int64(s.addr[l]) * bs, Length: bs})
	}
	return coalesce(out)
}

func slackAlloc(t *testing.T, f filesys.File) []filesys.Run {
	t.Helper()
	a, ok := filesys.AllocatedRunsOf(f)
	if !ok {
		t.Fatal("the file does not implement filesys.AllocatedRunner")
	}
	return a
}

func seqBlocks(n int) []int64 {
	s := make([]int64, n)
	for i := range s {
		s[i] = int64(i)
	}
	return s
}

func TestF2FSAllocatedRunsWholeBlocks(t *testing.T) {
	sizes := []int{1, bs - 1, bs, bs + 1, (923+1018+5)*bs + 7} // the last needs a direct node and an indirect node
	for _, stride := range []uint32{1, 3} {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("stride%d-size%d", stride, size), func(t *testing.T) {
				content := slackMarked(size, 1)
				s := slackFile(t, stride, int64(size), f2fstest.FileData{Blocks: blocksOf(content, 0)})
				if !filesys.ReportsSlack(s.fsys) {
					t.Error("the filesystem does not declare SlackReporter")
				}
				fl := mustOpenFile(t, s.fsys, dataNID)
				got := slackAlloc(t, fl)
				want := s.wantRuns(seqBlocks((size + bs - 1) / bs)...)
				if !slices.Equal(coalesce(got), want) {
					t.Fatalf("AllocatedRuns = %v, the builder placed %v", got, want)
				}
				if stride == 3 && size > bs && len(got) < 2 {
					t.Errorf("a stride-3 file has %d allocation runs: the test lost its point", len(got))
				}
				orig := slices.Clone(got)
				got[0] = filesys.Run{Offset: -1, Length: -1}
				if again := slackAlloc(t, fl); !slices.Equal(again, orig) {
					t.Errorf("a second call gave %v after the first answer was changed, want %v", again, orig)
				}
				run, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), orig, bs, s.fsys.Info().Size)
				wantOut := filesys.SlackFound
				if size%bs == 0 {
					wantOut = filesys.SlackExactFit
				}
				if out != wantOut {
					t.Fatalf("TailSlack outcome %q (%s), want %q", out, why, wantOut)
				}
				if out == filesys.SlackFound {
					last := int64(s.addr[int64((size-1)/bs)])*bs + int64(size%bs)
					if run.Offset != last || run.Length != bs-int64(size%bs) {
						t.Errorf("slack = %v, want {%d %d}", run, last, bs-int64(size%bs))
					}
				}
			})
		}
	}
}

func TestF2FSInlineFileHasNoAllocation(t *testing.T) {
	img, _ := f2fstest.BuildTree(f2fstest.Options{}, []f2fstest.File{
		{Path: "/tiny", Data: []byte("inline"), Inline: true},
		{Path: "/link", Symlink: "tiny", Inline: true},
		{Path: "/empty"},
	})
	fsys := mustOpen(t, img)
	for _, p := range []string{"/tiny", "/link", "/empty"} {
		e, err := fsys.Lookup(p)
		if err != nil {
			t.Fatal(err)
		}
		fl, err := fsys.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		if got := slackAlloc(t, fl); got != nil {
			t.Errorf("%s: AllocatedRuns = %v, want nil", p, got)
		}
		if p == "/tiny" {
			if _, out, _ := filesys.TailSlack(fl.Size(), fl.Runs(), nil, bs, fsys.Info().Size); out != filesys.SlackNoRuns {
				t.Errorf("inline file outcome %q, want no-runs", out)
			}
		}
	}
}

// Holes (NULL_ADDR) and NEW_ADDR blocks are not data runs: they report no allocation, and a file ending in one
// has a trailing hole, not slack.
func TestF2FSHolesAndNewBlocksAreNotAllocated(t *testing.T) {
	c := slackMarked(2*bs, 2)
	d := f2fstest.FileData{Blocks: map[int64][]byte{0: c[:bs], 2: c[bs:]}, New: []int64{1, 3}}
	s := slackFile(t, 1, 4*bs, d)
	fl := mustOpenFile(t, s.fsys, dataNID)
	got := slackAlloc(t, fl)
	want := []filesys.Run{{Offset: int64(s.addr[0]) * bs, Length: bs}, {Offset: int64(s.addr[2]) * bs, Length: bs}}
	if !slices.Equal(got, want) {
		t.Errorf("AllocatedRuns = %v, want only the two written blocks %v", got, want)
	}
	if _, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), got, bs, s.fsys.Info().Size); out != filesys.SlackTrailingHole {
		t.Errorf("a file ending in a NEW_ADDR block: outcome %q (%s), want trailing-hole", out, why)
	}

	// a hole in the middle with data in the last block: the slack of that block
	c = slackMarked(2*bs, 3)
	s = slackFile(t, 1, 3*bs+20, f2fstest.FileData{Blocks: map[int64][]byte{0: c[:bs], 3: c[bs : bs+20]}, New: []int64{1}})
	fl = mustOpenFile(t, s.fsys, dataNID)
	got = slackAlloc(t, fl)
	if len(got) != 2 {
		t.Fatalf("AllocatedRuns = %v, want the two written blocks", got)
	}
	run, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), got, bs, s.fsys.Info().Size)
	if w := (filesys.Run{Offset: int64(s.addr[3])*bs + 20, Length: bs - 20}); out != filesys.SlackFound || run != w {
		t.Errorf("TailSlack = %v %q (%s), want %v slack", run, out, why, w)
	}
}

func TestF2FSAllocatedRunsMatchOracle(t *testing.T) {
	for _, name := range []string{"f2fs-default", "f2fs-extra-attr"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", name+".img.gz")
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle %s: the oracle is stale", got, want.Generator.ImageSHA256)
			}
			fsys := mustOpen(t, img)
			sizes := map[string]int64{}
			for _, f := range want.Files {
				sizes[f.Path] = f.Size
			}
			checked, slack, inline := 0, 0, 0
			err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("walk %s: %v", p, err)
					return nil
				}
				if e.Type != filesys.TypeFile || e.Deleted {
					return nil
				}
				fl, err := fsys.Open(e)
				if err != nil {
					t.Errorf("open %s: %v", p, err)
					return nil
				}
				got := slackAlloc(t, fl)
				ranges, hasBlocks := want.FileBlocks[p]
				if !hasBlocks {
					inline++
					if got != nil {
						t.Errorf("%s: stored inline per the oracle, AllocatedRuns %v", p, got)
					}
					return nil
				}
				var exp []filesys.Run
				var dataLen int64
				for _, r := range ranges {
					exp = append(exp, filesys.Run{Offset: r[0] * 4096, Length: (r[1] - r[0] + 1) * 4096})
					dataLen += (r[1] - r[0] + 1) * 4096
				}
				if !slices.Equal(coalesce(got), coalesce(exp)) {
					t.Errorf("%s: AllocatedRuns %v, the oracle's blocks are %v", p, coalesce(got), coalesce(exp))
				}
				checked++
				size := sizes[p]
				_, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), got, 4096, fsys.Info().Size)
				switch {
				case dataLen < (size+4095)/4096*4096: // the oracle lists fewer blocks than the size needs: a hole somewhere
					if out != filesys.SlackFound && out != filesys.SlackExactFit && out != filesys.SlackTrailingHole {
						t.Errorf("%s: sparse file outcome %q (%s)", p, out, why)
					}
				case size%4096 != 0:
					slack++
					if out != filesys.SlackFound {
						t.Errorf("%s: size %d is not block aligned but the outcome is %q (%s)", p, size, out, why)
					}
				default:
					if out != filesys.SlackExactFit {
						t.Errorf("%s: size %d is block aligned but the outcome is %q (%s)", p, size, out, why)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if checked == 0 || slack == 0 {
				t.Errorf("only %d files with blocks and %d with slack were checked: the test lost its point", checked, slack)
			}
			t.Logf("%d files with blocks (%d with slack), %d inline", checked, slack, inline)
		})
	}
}

// A damaged node chain leaves a trusted prefix: AllocatedRuns lists only the blocks that could be followed and
// the file has no slack (the last unit is not known).
func TestF2FSDamagedMapAllocatedRunsArePrefixUnits(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	l := f2fstest.Geometry(o)
	const freeNode = 21
	addrs := make([]uint32, 923)
	for i := range addrs {
		addrs[i] = l.Main + 100 + uint32(i)
	}
	size := uint64(923+50)*bs - 5
	o.Nodes = []f2fstest.Node{{NID: dataNID, Addr: l.Main, Block: f2fstest.InodeBlock(o, f2fstest.Inode{
		NID: dataNID, Mode: regMode, Size: size, Addrs: addrs, NIDs: [5]uint32{freeNode, 0, 0, 0, 0},
	})}}
	img := f2fstest.Build(o, nil)
	fsys := mustOpen(t, img)
	fl := mustOpenFile(t, fsys, dataNID)
	got := slackAlloc(t, fl)
	if want := []filesys.Run{{Offset: int64(l.Main+100) * bs, Length: 923 * bs}}; !slices.Equal(got, want) {
		t.Fatalf("AllocatedRuns = %v, want the 923 followed blocks %v", got, want)
	}
	if covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), int64(len(img))); err != nil || covered != 923*bs {
		t.Fatalf("runs cover %d (%v), want the 923-block prefix", covered, err)
	}
	if run, out, why := filesys.TailSlack(fl.Size(), fl.Runs(), got, bs, fsys.Info().Size); out != filesys.SlackPrefix {
		t.Errorf("TailSlack = %v %q (%s), want prefix", run, out, why)
	}
}

func TestF2FSAllocatedRunsContract(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle %s: the oracle is stale", got, want.Generator.ImageSHA256)
			}
			fstest.AllocatedRunsContract(t, mustOpen(t, img))
		})
	}
	t.Run("builder", func(t *testing.T) {
		img, _ := f2fstest.BuildTree(f2fstest.Options{}, []f2fstest.File{
			{Path: "/a.txt", Data: []byte("hello"), Inline: true},
			{Path: "/small.bin", Data: slackMarked(5000, 1)},
			{Path: "/d", Dir: true, Mode: 0o755},
			{Path: "/d/mid.bin", Data: slackMarked(60*bs+17, 2)},
			{Path: "/link", Symlink: "a.txt"},
			{Path: "/empty"},
		})
		fstest.AllocatedRunsContract(t, mustOpen(t, img))
	})
}
