package apfs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/apfs"
)

// orcFixtures are the real empty containers made by an independent tool
// (testdata/*.img.gz) with the facts an independent decoder read from them
// (testdata/*.expect.json).
var orcFixtures = []string{"apfs-ci", "apfs-cs", "apfs-multichunk"}

type orcMapping struct {
	Type    uint32 `json:"cpm_type"`
	Subtype uint32 `json:"cpm_subtype"`
	Size    uint32 `json:"cpm_size"`
	Oid     uint64 `json:"cpm_oid"`
	Paddr   uint64 `json:"cpm_paddr"`
}

type orcArea struct {
	Base   uint64 `json:"base"`
	Blocks uint32 `json:"blocks"`
	Next   uint32 `json:"next"`
	Index  uint32 `json:"index"`
	Len    uint32 `json:"len"`
}

type orcRingEntry struct {
	Index    int          `json:"index"`
	Block    uint64       `json:"block"`
	Kind     string       `json:"kind"`
	Xid      uint64       `json:"xid"`
	Flags    uint32       `json:"flags"`
	Count    uint32       `json:"count"`
	Mappings []orcMapping `json:"mappings"`
}

type orcExpect struct {
	Container struct {
		BlockSize    uint32   `json:"block_size"`
		BlockCount   uint64   `json:"block_count"`
		UUID         string   `json:"uuid"`
		Features     uint64   `json:"features"`
		RoCompat     uint64   `json:"readonly_compatible_features"`
		Incompat     uint64   `json:"incompatible_features"`
		Flags        uint64   `json:"flags"`
		NextXid      uint64   `json:"next_xid"`
		MaxFS        uint32   `json:"max_file_systems"`
		FsOids       []uint64 `json:"fs_oids"`
		SpacemanOid  uint64   `json:"spaceman_oid"`
		OmapOid      uint64   `json:"omap_oid"`
		ReaperOid    uint64   `json:"reaper_oid"`
		Blocked      []uint64 `json:"blocked_out_prange"`
		EvictMapping uint64   `json:"evict_mapping_tree_oid"`
	} `json:"container"`
	Checkpoint struct {
		NewestXid uint64         `json:"newest_xid"`
		RingIndex int            `json:"superblock_ring_index"`
		Desc      orcArea        `json:"desc"`
		Data      orcArea        `json:"data"`
		Ring      []orcRingEntry `json:"ring"`
	} `json:"checkpoint"`
	Verified  []uint64 `json:"verified_object_blocks"`
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
}

// orcLoad gunzips a fixture, checks its sha256 against the oracle first, and
// returns the image with the decoded oracle.
func orcLoad(t *testing.T, name string) ([]byte, *orcExpect) {
	t.Helper()
	dir := filepath.Join("testdata")
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Skipf("no real APFS fixture %s: %v", name, err)
	}
	var exp orcExpect
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("%s oracle: %v", name, err)
	}
	f, err := os.Open(filepath.Join(dir, name+".img.gz"))
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
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != exp.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 %s, oracle says %s", name, got, exp.Generator.ImageSHA256)
	}
	return img, &exp
}

func orcSkipShort(t *testing.T, name string) {
	t.Helper()
	if testing.Short() && strings.HasSuffix(name, "multichunk") {
		t.Skip("large fixture skipped under -short")
	}
}

func TestAPFSMatchesOracle(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		for _, name := range orcFixtures {
			t.Run(name, func(t *testing.T) {
				orcSkipShort(t, name)
				img, exp := orcLoad(t, name)
				orcCheckContainer(t, img, exp)
			})
		}
	})
}

func orcCheckContainer(t *testing.T, img []byte, exp *orcExpect) {
	t.Helper()
	if !apfs.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatal("Probe rejects a real container")
	}
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, cp := &exp.Container, &exp.Checkpoint
	n := f.NX()
	if n.BlockSize != c.BlockSize || n.BlockCount != c.BlockCount {
		t.Errorf("block size/count %d/%d, oracle %d/%d", n.BlockSize, n.BlockCount, c.BlockSize, c.BlockCount)
	}
	if n.Features != c.Features || n.RoCompat != c.RoCompat || n.Incompat != c.Incompat || n.Flags != c.Flags {
		t.Errorf("features %#x/%#x/%#x flags %#x, oracle %#x/%#x/%#x flags %#x",
			n.Features, n.RoCompat, n.Incompat, n.Flags, c.Features, c.RoCompat, c.Incompat, c.Flags)
	}
	if got := f.Info().UUID; got != c.UUID {
		t.Errorf("uuid %s, oracle %s", got, c.UUID)
	}
	if n.MaxFS != c.MaxFS || n.SpacemanOid != c.SpacemanOid || n.OmapOid != c.OmapOid || n.NextXid != c.NextXid {
		t.Errorf("max fs %d spaceman %d omap %d next xid %d; oracle %d %d %d %d",
			n.MaxFS, n.SpacemanOid, n.OmapOid, n.NextXid, c.MaxFS, c.SpacemanOid, c.OmapOid, c.NextXid)
	}
	if n.Xid != cp.NewestXid || f.SuperblockIndex() != cp.RingIndex {
		t.Errorf("checkpoint xid %d ring index %d; oracle %d %d", n.Xid, f.SuperblockIndex(), cp.NewestXid, cp.RingIndex)
	}
	if n.DescBase != cp.Desc.Base || n.DescBlocks != cp.Desc.Blocks || n.DescIndex != cp.Desc.Index || n.DescLen != cp.Desc.Len {
		t.Errorf("descriptor area %d+%d index %d len %d; oracle %+v", n.DescBase, n.DescBlocks, n.DescIndex, n.DescLen, cp.Desc)
	}
	if n.DataBase != cp.Data.Base || n.DataBlocks != cp.Data.Blocks {
		t.Errorf("data area %d+%d; oracle %+v", n.DataBase, n.DataBlocks, cp.Data)
	}

	// The ring walk: the oracle's map entry (the blocks before the superblock)
	// gives the ephemeral map the reader must have built.
	want := map[uint64]orcMapping{}
	maps := 0
	for _, e := range cp.Ring {
		if e.Kind == "map" && e.Xid == cp.NewestXid {
			maps++
			if e.Count != uint32(len(e.Mappings)) {
				t.Errorf("oracle ring block %d: count %d, %d mappings", e.Block, e.Count, len(e.Mappings))
			}
			for _, m := range e.Mappings {
				want[m.Oid] = m
			}
		}
	}
	if maps != int(cp.Desc.Len)-1 {
		t.Fatalf("oracle lists %d map blocks for the newest checkpoint, descriptor length %d", maps, cp.Desc.Len)
	}
	if f.EphemeralCount() != len(want) {
		t.Errorf("%d ephemeral objects, oracle %d", f.EphemeralCount(), len(want))
	}
	for oid, m := range want {
		paddr, size, typ, ok := f.Ephemeral(oid)
		if !ok || paddr != m.Paddr || size != m.Size || typ != m.Type {
			t.Errorf("ephemeral oid %d = (%d, %d, %#x, %v); oracle %+v", oid, paddr, size, typ, ok, m)
		}
	}

	in := f.Info()
	if in.Type != "apfs" || in.BlockSize != int(c.BlockSize) || in.Size != int64(c.BlockCount)*int64(c.BlockSize) {
		t.Errorf("Info = %+v", in)
	}
	if len(in.Warnings) != 0 {
		t.Errorf("warnings on a real container: %q", in.Warnings)
	}
}

// Every object block the independent decoder verified verifies in the reader
// too, with the checksum the tool wrote; the builder agrees on all of them.
func TestFletcher64RealFixtureBlocks(t *testing.T) {
	for _, name := range orcFixtures {
		t.Run(name, func(t *testing.T) {
			orcSkipShort(t, name)
			img, exp := orcLoad(t, name)
			bs := int(exp.Container.BlockSize)
			if len(exp.Verified) == 0 {
				t.Fatal("oracle lists no verified blocks")
			}
			for _, blk := range exp.Verified {
				b := img[int(blk)*bs : (int(blk)+1)*bs]
				if !apfs.ChecksumOK(b) {
					t.Errorf("block %d: reader does not verify the checksum the tool wrote", blk)
				}
				if got := apfs.Fletcher64(b); got != leU64(b) {
					t.Errorf("block %d: Fletcher64 %#x, stored %#x", blk, got, leU64(b))
				}
			}
			zero := make([]byte, bs)
			if apfs.ChecksumOK(zero) {
				t.Error("a zero block verifies")
			}
		})
	}
}

func leU64(b []byte) uint64 { return le.Uint64(b) }
