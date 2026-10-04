package f2fs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// oracle is the part of testdata/*.expect.json this file reads. The oracle is
// computed by tools/fixtures (f2fs_oracle.py) from the source tree handed to
// sload.f2fs and from the reports of fsck.f2fs, dump.f2fs and blkid on the
// finished image, never by Minutiae. It also records the inode -> NAT block
// address, per-file data block and free-block expectations that later tests
// compare against the directory, file and Unallocated readers.
type oracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	UUID        string   `json:"uuid"`
	BlockSize   int      `json:"block_size"`
	Size        int64    `json:"size"`
	FeatureBits uint32   `json:"feature_bits"`
	Features    []string `json:"features"`
	Superblock  struct {
		BlockCount    uint64 `json:"block_count"`
		SegmentCount  uint32 `json:"segment_count"`
		SegCkpt       uint32 `json:"segment_count_ckpt"`
		SegSIT        uint32 `json:"segment_count_sit"`
		SegNAT        uint32 `json:"segment_count_nat"`
		SegSSA        uint32 `json:"segment_count_ssa"`
		SegMain       uint32 `json:"segment_count_main"`
		Segment0Addr  uint32 `json:"segment0_blkaddr"`
		CPAddr        uint32 `json:"cp_blkaddr"`
		SITAddr       uint32 `json:"sit_blkaddr"`
		NATAddr       uint32 `json:"nat_blkaddr"`
		SSAAddr       uint32 `json:"ssa_blkaddr"`
		MainAddr      uint32 `json:"main_blkaddr"`
		RootIno       uint32 `json:"root_ino"`
		NodeIno       uint32 `json:"node_ino"`
		MetaIno       uint32 `json:"meta_ino"`
		CPPayload     uint32 `json:"cp_payload"`
		LogBlockSize  uint32 `json:"log_blocksize"`
		LogBlocksPSeg uint32 `json:"log_blocks_per_seg"`
	} `json:"superblock"`
	Root struct {
		Ino        uint32 `json:"ino"`
		NATBlkaddr uint32 `json:"nat_blkaddr"`
	} `json:"root"`
	Inodes []struct {
		Path       string `json:"path"`
		Ino        uint32 `json:"ino"`
		NATBlkaddr uint32 `json:"nat_blkaddr"`
	} `json:"inodes"`
	Files []struct {
		Path  string `json:"path"`
		Type  string `json:"type"`
		Size  int64  `json:"size"`
		Mode  uint32 `json:"mode"`
		Mtime int64  `json:"mtime"`
	} `json:"files"`
	FileBlocks     map[string][][2]int64 `json:"file_blocks"`
	FreeBlocks     [][2]int64            `json:"free_blocks"`
	FreeBlockCount int64                 `json:"free_block_count"`
	MainBlocks     int64                 `json:"main_blocks"`
	Checkpoint     struct {
		Version      uint64 `json:"checkpoint_ver"`
		UserBlocks   uint64 `json:"user_block_count"`
		ValidBlocks  uint64 `json:"valid_block_count"`
		FreeSegs     uint32 `json:"free_segment_count"`
		Flags        uint32 `json:"ckpt_flags"`
		PackBlocks   uint32 `json:"cp_pack_total_block_count"`
		StartSum     uint32 `json:"cp_pack_start_sum"`
		NextFreeNid  uint32 `json:"next_free_nid"`
		SITBitmapLen int    `json:"sit_ver_bitmap_bytesize"`
		NATBitmapLen int    `json:"nat_ver_bitmap_bytesize"`
	} `json:"checkpoint"`
}

func fixturePaths(t testing.TB) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.img.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in testdata (glob err %v)", err)
	}
	return paths
}

// gunzipFixture returns the decompressed image of a *.img.gz fixture.
func gunzipFixture(t testing.TB, path string) []byte {
	t.Helper()
	gz, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func loadOracle(t testing.TB, imgGz string) oracle {
	t.Helper()
	raw, err := os.ReadFile(strings.TrimSuffix(imgGz, ".img.gz") + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o oracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestF2FSMatchesOracle opens each real mkfs.f2fs/sload.f2fs fixture and
// compares what the reader decoded with the oracle: Info, the superblock
// geometry, the chosen checkpoint, the inode addresses and Unallocated (exactly
// the free blocks of the SIT bitmaps). Directory and file comparisons live in
// dir_test.go and data_test.go.
func TestF2FSMatchesOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle generator.image_sha256 %s: the oracle is stale, regenerate the fixtures", got, want.Generator.ImageSHA256)
			}
			if int64(len(img)) != want.Size {
				t.Fatalf("image is %d bytes, oracle size %d", len(img), want.Size)
			}

			fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			checkInfo(t, fsys, want)
			checkGeometry(t, fsys, want)
			checkCheckpoint(t, fsys, want)
			checkInodes(t, fsys, want)
			checkUnallocated(t, fsys, want)
		})
	}
}

func checkInfo(t *testing.T, fsys *f2fs.FS, want oracle) {
	t.Helper()
	info := fsys.Info()
	if info.Type != want.Type || info.Label != want.Label || !strings.EqualFold(info.UUID, want.UUID) ||
		info.BlockSize != want.BlockSize || info.Size != want.Size {
		t.Errorf("Info = {%s %q %s block %d size %d}, want {%s %q %s block %d size %d}",
			info.Type, info.Label, info.UUID, info.BlockSize, info.Size,
			want.Type, want.Label, want.UUID, want.BlockSize, want.Size)
	}
	got, wantF := slices.Clone(info.Features), slices.Clone(want.Features)
	sort.Strings(got)
	sort.Strings(wantF)
	if !slices.Equal(got, wantF) {
		t.Errorf("features = %v, want (fsck.f2fs) %v", got, wantF)
	}
	if info.Encrypted {
		t.Error("Info.Encrypted on an image without the encrypt feature")
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings on a clean mkfs.f2fs/sload.f2fs image: %v", info.Warnings)
	}
}

func checkGeometry(t *testing.T, fsys *f2fs.FS, want oracle) {
	t.Helper()
	g, sb := fsys.Geometry(), want.Superblock
	if sb.LogBlockSize != 12 || sb.LogBlocksPSeg != 9 {
		t.Fatalf("oracle geometry is not 4 KiB blocks / 512 blocks per segment: %+v", sb)
	}
	got := []uint64{
		g.BlockCount, uint64(g.SegmentCount), uint64(g.SegCkpt), uint64(g.SegSIT), uint64(g.SegNAT), uint64(g.SegSSA), uint64(g.SegMain),
		uint64(g.Seg0), uint64(g.CP), uint64(g.SIT), uint64(g.NAT), uint64(g.SSA), uint64(g.Main),
		uint64(g.RootIno), uint64(g.NodeIno), uint64(g.MetaIno), uint64(g.CPPayload), uint64(g.Feature), uint64(g.Blocks),
	}
	wantV := []uint64{
		sb.BlockCount, uint64(sb.SegmentCount), uint64(sb.SegCkpt), uint64(sb.SegSIT), uint64(sb.SegNAT), uint64(sb.SegSSA), uint64(sb.SegMain),
		uint64(sb.Segment0Addr), uint64(sb.CPAddr), uint64(sb.SITAddr), uint64(sb.NATAddr), uint64(sb.SSAAddr), uint64(sb.MainAddr),
		uint64(sb.RootIno), uint64(sb.NodeIno), uint64(sb.MetaIno), uint64(sb.CPPayload), uint64(want.FeatureBits), sb.BlockCount,
	}
	names := []string{
		"block_count", "segment_count", "segment_count_ckpt", "segment_count_sit", "segment_count_nat", "segment_count_ssa", "segment_count_main",
		"segment0_blkaddr", "cp_blkaddr", "sit_blkaddr", "nat_blkaddr", "ssa_blkaddr", "main_blkaddr",
		"root_ino", "node_ino", "meta_ino", "cp_payload", "feature bits", "blocks held by the image",
	}
	for i := range names {
		if got[i] != wantV[i] {
			t.Errorf("superblock %s = %d, want (fsck.f2fs/dump.f2fs) %d", names[i], got[i], wantV[i])
		}
	}
}

func checkCheckpoint(t *testing.T, fsys *f2fs.FS, want oracle) {
	t.Helper()
	c, w := fsys.Checkpoint(), want.Checkpoint
	if c.Pack != 1 && c.Pack != 2 {
		t.Errorf("checkpoint pack = %d, want 1 or 2", c.Pack)
	}
	// Both packs hold the same version after sload.f2fs (it mirrors the valid
	// checkpoint), so the pack that wins the tie is not asserted.
	if c.Version != w.Version {
		t.Errorf("checkpoint version = %d, want %d", c.Version, w.Version)
	}
	if c.Flags != w.Flags {
		t.Errorf("checkpoint flags = %#x, want %#x", c.Flags, w.Flags)
	}
	if c.PackBlocks != w.PackBlocks || c.StartSum != w.StartSum {
		t.Errorf("checkpoint pack blocks/start_sum = %d/%d, want %d/%d", c.PackBlocks, c.StartSum, w.PackBlocks, w.StartSum)
	}
	if c.UserBlocks != w.UserBlocks || c.ValidBlocks != w.ValidBlocks || c.FreeSegs != w.FreeSegs || c.NextFreeNid != w.NextFreeNid {
		t.Errorf("checkpoint user/valid blocks, free segments, next nid = %d/%d/%d/%d, want %d/%d/%d/%d",
			c.UserBlocks, c.ValidBlocks, c.FreeSegs, c.NextFreeNid, w.UserBlocks, w.ValidBlocks, w.FreeSegs, w.NextFreeNid)
	}
	if len(c.SITBitmap) != w.SITBitmapLen || len(c.NATBitmap) != w.NATBitmapLen {
		t.Errorf("version bitmap sizes = %d/%d, want %d/%d", len(c.SITBitmap), len(c.NATBitmap), w.SITBitmapLen, w.NATBitmapLen)
	}
}

// checkInodes compares the NAT block address of the root and of every inode
// the tools list (fsck.f2fs -t + dump.f2fs -n), and the type, permission
// bits, size and mtime of each against the source tree.
func checkInodes(t *testing.T, fsys *f2fs.FS, want oracle) {
	t.Helper()
	if got, err := fsys.NATLookup(want.Root.Ino); err != nil || got != want.Root.NATBlkaddr {
		t.Errorf("root inode %d: NAT address %d, %v; want %d", want.Root.Ino, got, err, want.Root.NATBlkaddr)
	}
	if len(want.Inodes) == 0 || len(want.Inodes) != len(want.Files) {
		t.Fatalf("oracle has %d inodes for %d files", len(want.Inodes), len(want.Files))
	}
	byPath := map[string]int{}
	for i, f := range want.Files {
		byPath[f.Path] = i
	}
	const fmtMask = 0xF000
	for _, in := range want.Inodes {
		i, ok := byPath[in.Path]
		if !ok {
			t.Errorf("%s: inode listed but no file entry", in.Path)
			continue
		}
		f := want.Files[i]
		if got, err := fsys.NATLookup(in.Ino); err != nil || got != in.NATBlkaddr {
			t.Errorf("%s: inode %d NAT address %d, %v; want %d", in.Path, in.Ino, got, err, in.NATBlkaddr)
		}
		v, _, err := fsys.Inode(in.Ino)
		if err != nil {
			t.Errorf("%s: inode %d: %v", in.Path, in.Ino, err)
			continue
		}
		wantFmt := map[string]uint16{"file": 0x8000, "dir": 0x4000, "symlink": 0xA000}[f.Type]
		if v.Mode&fmtMask != wantFmt {
			t.Errorf("%s: mode %#o is not a %s", in.Path, v.Mode, f.Type)
		}
		if uint32(v.Mode&0o7777) != f.Mode {
			t.Errorf("%s: permission bits %#o, want %#o", in.Path, v.Mode&0o7777, f.Mode)
		}
		if f.Type != "dir" && v.Size != f.Size {
			t.Errorf("%s: size %d, want %d", in.Path, v.Size, f.Size)
		}
		if got := v.Times.Modified.T.Unix(); got != f.Mtime {
			t.Errorf("%s: mtime %d, want %d", in.Path, got, f.Mtime)
		}
	}
}

// checkUnallocated compares Unallocated exactly with the free main-area blocks
// the oracle derived from the SIT valid bitmaps by the external tools
// (ranges and count), and checks that no free run overlaps a block of a live
// file: the data blocks the oracle lists, the node blocks of every inode and
// the data runs the reader reports for every regular file.
func checkUnallocated(t *testing.T, fsys *f2fs.FS, want oracle) {
	t.Helper()
	runs, err := fsys.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	var wantRuns []filesys.Run
	var wantBlocks int64
	for _, r := range want.FreeBlocks {
		wantRuns = append(wantRuns, filesys.Run{Offset: r[0] * 4096, Length: (r[1] - r[0] + 1) * 4096})
		wantBlocks += r[1] - r[0] + 1
	}
	if wantBlocks != want.FreeBlockCount || len(wantRuns) == 0 {
		t.Fatalf("oracle free ranges hold %d blocks, its count says %d", wantBlocks, want.FreeBlockCount)
	}
	var gotBlocks int64
	for _, r := range runs {
		gotBlocks += r.Length / 4096
	}
	if gotBlocks != want.FreeBlockCount {
		t.Errorf("Unallocated holds %d blocks, oracle (SIT bitmaps via dump.f2fs) %d", gotBlocks, want.FreeBlockCount)
	}
	if !slices.Equal(runs, wantRuns) {
		t.Errorf("Unallocated differs from the oracle's free blocks:\n got  %v\n want %v", runs, wantRuns)
	}
	if w := fsys.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings after Unallocated on a clean image: %v", w)
	}

	var live []filesys.Run
	for _, ranges := range want.FileBlocks {
		for _, r := range ranges {
			live = append(live, filesys.Run{Offset: r[0] * 4096, Length: (r[1] - r[0] + 1) * 4096})
		}
	}
	live = append(live, filesys.Run{Offset: int64(want.Root.NATBlkaddr) * 4096, Length: 4096})
	for _, in := range want.Inodes {
		live = append(live, filesys.Run{Offset: int64(in.NATBlkaddr) * 4096, Length: 4096})
	}
	err = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil || e.Type != filesys.TypeFile {
			return nil
		}
		fl, err := fsys.Open(e)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		for _, r := range fl.Runs() {
			if r.Offset >= 0 {
				live = append(live, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range live {
		i := sort.Search(len(runs), func(i int) bool { return runs[i].Offset+runs[i].Length > r.Offset })
		if i < len(runs) && runs[i].Offset < r.Offset+r.Length {
			t.Errorf("free run %+v overlaps a live block run %+v", runs[i], r)
		}
	}
}
