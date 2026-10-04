package apfs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
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

// orcOmap is an object map as the independent decoder read it.
type orcOmap struct {
	Block   uint64 `json:"block"`
	Entries []struct {
		Oid   uint64 `json:"oid"`
		Xid   uint64 `json:"xid"`
		Flags uint32 `json:"flags"`
		Size  uint32 `json:"size"`
		Paddr uint64 `json:"paddr"`
	} `json:"entries"`
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
		NextOid      uint64   `json:"next_oid"`
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
	ContainerOmap orcOmap `json:"container_omap"`
	VolumeOmap    orcOmap `json:"volume_omap"`
	Volume        struct {
		Oid          uint64 `json:"oid"`
		Block        uint64 `json:"block"`
		OmapOid      uint64 `json:"omap_oid"`
		RootTreeOid  uint64 `json:"root_tree_oid"`
		RootTreeType uint32 `json:"root_tree_type"`
	} `json:"volume"`
	FsTree struct {
		RootOid   uint64 `json:"root_oid"`
		RootBlock uint64 `json:"root_block"`
		Info      struct {
			Flags     uint32 `json:"bt_flags"`
			KeyCount  uint64 `json:"bt_key_count"`
			NodeCount uint64 `json:"bt_node_count"`
		} `json:"btree_info"`
		Records []struct {
			ID          uint64 `json:"id"`
			Type        uint64 `json:"type"`
			KeyLength   int    `json:"key_length"`
			ValueLength int    `json:"value_length"`
		} `json:"records"`
	} `json:"fs_tree"`
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
		t.Fatalf("real APFS fixture %s: %v", name, err)
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
	t.Run("omap", func(t *testing.T) {
		for _, name := range orcFixtures {
			t.Run(name, func(t *testing.T) {
				orcSkipShort(t, name)
				img, exp := orcLoad(t, name)
				orcCheckOmaps(t, img, exp)
			})
		}
	})
}

// orcCheckOmaps compares the container and volume object maps and the volume's
// file-system tree with the oracle: the records of the real B-trees, the
// lookup of the volume superblock's oid, and a walk of the virtual fs tree
// root through the volume omap.
func orcCheckOmaps(t *testing.T, img []byte, exp *orcExpect) {
	t.Helper()
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, tc := range []struct {
		name  string
		paddr uint64 // 0: the container's
		want  orcOmap
	}{
		{"container omap", 0, exp.ContainerOmap},
		{"volume omap", exp.Volume.OmapOid, exp.VolumeOmap},
	} {
		got, err := f.OmapEntries(tc.paddr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(tc.want.Entries) == 0 || len(got) != len(tc.want.Entries) {
			t.Fatalf("%s: %d entries, oracle %d", tc.name, len(got), len(tc.want.Entries))
		}
		for i, w := range tc.want.Entries {
			g := got[i]
			if g.Oid != w.Oid || g.Xid != w.Xid || g.Flags != w.Flags || g.Size != w.Size || g.Paddr != w.Paddr {
				t.Errorf("%s entry %d = %+v, oracle %+v", tc.name, i, g, w)
			}
			// Every entry resolves through the lookup too (and not below its xid).
			p, size, flags, err := f.OmapLookup(tc.paddr, w.Oid, w.Xid)
			if err != nil || p != w.Paddr || size != w.Size || flags != w.Flags {
				t.Errorf("%s lookup(%d, %d) = %d/%d/%d, %v; oracle %+v", tc.name, w.Oid, w.Xid, p, size, flags, err, w)
			}
			if _, _, _, err := f.OmapLookup(tc.paddr, w.Oid, w.Xid-1); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("%s lookup(%d, %d) below the entry's xid: %v, want ErrNotFound", tc.name, w.Oid, w.Xid-1, err)
			}
		}
		if snaps, err := f.OmapSnapshots(tc.paddr); err != nil || len(snaps) != 0 {
			t.Errorf("%s snapshots = %v, %v; an empty container has none", tc.name, snaps, err)
		}
	}
	// The volume superblock is virtual: the container omap finds it.
	paddr, size, _, err := f.OmapLookup(0, exp.Volume.Oid, f.NX().Xid)
	if err != nil || paddr != exp.Volume.Block || int(size) != f.Info().BlockSize {
		t.Errorf("volume superblock oid %d -> %d/%d, %v; oracle block %d", exp.Volume.Oid, paddr, size, err, exp.Volume.Block)
	}

	// The fs tree: virtual root, one leaf, through the volume omap.
	recs, err := f.ScanVolumeTree(exp.Volume.OmapOid, exp.Volume.RootTreeOid, exp.Volume.RootTreeType)
	if err != nil {
		t.Fatalf("fs tree: %v", err)
	}
	if want := exp.FsTree.Records; len(recs) != len(want) || uint64(len(recs)) != exp.FsTree.Info.KeyCount {
		t.Fatalf("fs tree has %d records, oracle %d (bt_key_count %d)", len(recs), len(want), exp.FsTree.Info.KeyCount)
	}
	for i, w := range exp.FsTree.Records {
		k := le.Uint64(recs[i].Key)
		if id, typ := k&0x0fffffffffffffff, k>>60; id != w.ID || typ != w.Type || len(recs[i].Key) != w.KeyLength || len(recs[i].Val) != w.ValueLength {
			t.Errorf("fs record %d = id %d type %d key %d val %d; oracle %+v", i, id, typ, len(recs[i].Key), len(recs[i].Val), w)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a real container: %q", w)
	}
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
	if n.NextOid != c.NextOid || n.ReaperOid != c.ReaperOid || n.EvictOid != c.EvictMapping {
		t.Errorf("next oid %d reaper %d evict mapping %d; oracle %d %d %d", n.NextOid, n.ReaperOid, n.EvictOid, c.NextOid, c.ReaperOid, c.EvictMapping)
	}
	if len(c.Blocked) != 2 || n.BlockedStart != c.Blocked[0] || n.BlockedCount != c.Blocked[1] {
		t.Errorf("blocked-out range %d+%d; oracle %v", n.BlockedStart, n.BlockedCount, c.Blocked)
	}
	for i, want := range n.FsOids {
		var oracle uint64
		if i < len(c.FsOids) {
			oracle = c.FsOids[i]
		}
		if want != oracle {
			t.Errorf("fs oid[%d] = %d; oracle %d (list %v)", i, want, oracle, c.FsOids)
			break
		}
	}
	if n.DescNext != cp.Desc.Next || n.DataNext != cp.Data.Next || n.DataIndex != cp.Data.Index || n.DataLen != cp.Data.Len {
		t.Errorf("cursors: desc next %d, data next/index/len %d/%d/%d; oracle %+v %+v", n.DescNext, n.DataNext, n.DataIndex, n.DataLen, cp.Desc, cp.Data)
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
