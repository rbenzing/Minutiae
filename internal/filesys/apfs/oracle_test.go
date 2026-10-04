package apfs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
		Name         string `json:"name"`
		Oid          uint64 `json:"oid"`
		Block        uint64 `json:"block"`
		UUID         string `json:"uuid"`
		FsIndex      uint32 `json:"fs_index"`
		Features     uint64 `json:"features"`
		RoCompat     uint64 `json:"readonly_compatible_features"`
		Incompat     uint64 `json:"incompatible_features"`
		FsFlags      uint64 `json:"fs_flags"`
		Role         uint16 `json:"role"`
		CaseInsens   bool   `json:"case_insensitive"`
		NormInsens   bool   `json:"normalization_insensitive"`
		Encrypted    bool   `json:"encrypted"`
		LastModTime  uint64 `json:"last_mod_time"`
		NumSnapshots uint64 `json:"num_snapshots"`
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
			Drec        *struct {
				ParentID uint64 `json:"parent_id"`
				Name     string `json:"name"`
				KeyForm  string `json:"key_form"`
				FileID   uint64 `json:"file_id"`
				Flags    uint16 `json:"flags"`
			} `json:"drec"`
			Inode *struct {
				ParentID         uint64 `json:"parent_id"`
				PrivateID        uint64 `json:"private_id"`
				CreateTime       uint64 `json:"create_time"`
				ModTime          uint64 `json:"mod_time"`
				ChangeTime       uint64 `json:"change_time"`
				AccessTime       uint64 `json:"access_time"`
				InternalFlags    uint64 `json:"internal_flags"`
				NChildrenOrNlink int32  `json:"nchildren_or_nlink"`
				ProtectionClass  uint32 `json:"default_protection_class"`
				BsdFlags         uint32 `json:"bsd_flags"`
				Owner            uint32 `json:"owner"`
				Group            uint32 `json:"group"`
				Mode             uint16 `json:"mode"`
				UncompressedSize uint64 `json:"uncompressed_size"`
			} `json:"inode"`
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
	t.Run("volume", func(t *testing.T) {
		for _, name := range orcFixtures {
			t.Run(name, func(t *testing.T) {
				orcSkipShort(t, name)
				img, exp := orcLoad(t, name)
				orcCheckVolume(t, img, exp)
			})
		}
	})
}

type orcInode struct {
	ParentID, PrivateID, CreateTime, ModTime, ChangeTime, AccessTime, InternalFlags uint64
	Links                                                                           int32
	Prot, Bsd, Owner, Group                                                         uint32
	Mode                                                                            uint16
	Uncompressed                                                                    uint64
}

type orcDrec struct {
	Name   string
	FileID uint64
	Flags  uint16
	Parent uint64
	Form   string
}

// orcCheckVolume compares the volume superblock, the root listing and the
// root and private-dir inodes and directory records with the oracle.
func orcCheckVolume(t *testing.T, img []byte, exp *orcExpect) {
	t.Helper()
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ov := &exp.Volume
	v, ok := f.VolumeFields(int(ov.FsIndex))
	if !ok || !v.Readable {
		t.Fatalf("volume %d missing or unreadable", ov.FsIndex)
	}
	if string(v.Name) != ov.Name || v.Display != ov.Name || v.RawName != nil {
		t.Errorf("volume name %q (display %q), oracle %q", v.Name, v.Display, ov.Name)
	}
	if got := fmt.Sprintf("%x-%x-%x-%x-%x", v.UUID[0:4], v.UUID[4:6], v.UUID[6:8], v.UUID[8:10], v.UUID[10:16]); got != ov.UUID {
		t.Errorf("volume uuid %s, oracle %s", got, ov.UUID)
	}
	if v.FsIndex != ov.FsIndex || v.Features != ov.Features || v.RoCompat != ov.RoCompat || v.Incompat != ov.Incompat || v.FsFlags != ov.FsFlags || v.Role != ov.Role {
		t.Errorf("volume index/features/ro/incompat/flags/role %d/%#x/%#x/%#x/%#x/%d, oracle %d/%#x/%#x/%#x/%#x/%d",
			v.FsIndex, v.Features, v.RoCompat, v.Incompat, v.FsFlags, v.Role, ov.FsIndex, ov.Features, ov.RoCompat, ov.Incompat, ov.FsFlags, ov.Role)
	}
	if v.CaseInsensitive != ov.CaseInsens || v.NormInsn != ov.NormInsens || v.Encrypted != ov.Encrypted || v.Sealed {
		t.Errorf("volume case/normalization/encrypted/sealed %v/%v/%v/%v, oracle %v/%v/%v", v.CaseInsensitive, v.NormInsn, v.Encrypted, v.Sealed, ov.CaseInsens, ov.NormInsens, ov.Encrypted)
	}
	if v.Snapshots != ov.NumSnapshots || v.LastMod != ov.LastModTime || v.OmapOid != ov.OmapOid || v.RootOid != ov.RootTreeOid || v.RootType != ov.RootTreeType {
		t.Errorf("volume snapshots/last mod/omap/root %d/%d/%d/%d/%#x, oracle %d/%d/%d/%d/%#x",
			v.Snapshots, v.LastMod, v.OmapOid, v.RootOid, v.RootType, ov.NumSnapshots, ov.LastModTime, ov.OmapOid, ov.RootTreeOid, ov.RootTreeType)
	}

	in := f.Info()
	if want := []string{ov.Name}; !slices.Equal(in.Volumes, want) || in.Encrypted {
		t.Errorf("Info.Volumes %q encrypted %v, oracle %q", in.Volumes, in.Encrypted, want)
	}
	line := fmt.Sprintf("vol[%d] %q: role=none", ov.FsIndex, ov.Name)
	if ov.Role != 0 {
		t.Fatalf("oracle role %d: the expected feature line needs a role name", ov.Role)
	}
	if ov.CaseInsens {
		line += ", case-insensitive"
	}
	if ov.NormInsens {
		line += ", normalization-insensitive"
	}
	line += fmt.Sprintf(", unencrypted, snapshots=%d", ov.NumSnapshots)
	if !contains(in.Features, line) {
		t.Errorf("Info.Features lacks %q: %q", line, in.Features)
	}

	// The oracle's inode records, by number.
	inodes := map[uint64]*orcInode{}
	var drecs []orcDrec
	for _, r := range exp.FsTree.Records {
		switch {
		case r.Inode != nil:
			i := r.Inode
			inodes[r.ID] = &orcInode{i.ParentID, i.PrivateID, i.CreateTime, i.ModTime, i.ChangeTime, i.AccessTime, i.InternalFlags, i.NChildrenOrNlink, i.ProtectionClass, i.BsdFlags, i.Owner, i.Group, i.Mode, i.UncompressedSize}
		case r.Drec != nil:
			d := r.Drec
			drecs = append(drecs, orcDrec{d.Name, d.FileID, d.Flags, d.ParentID, d.KeyForm})
		}
	}
	if len(inodes) != 2 || inodes[2] == nil || inodes[3] == nil {
		t.Fatalf("oracle inodes: %v", inodes)
	}
	wantForm := "plain"
	if ov.CaseInsens || ov.NormInsens {
		wantForm = "hashed"
	}
	for _, d := range drecs {
		if d.Form != wantForm {
			t.Errorf("oracle drec %q has the %s key form, the case flags say %s", d.Name, d.Form, wantForm)
		}
	}

	for _, ino := range []uint64{2, 3} {
		w := inodes[ino]
		g, err := f.Inode(int(ov.FsIndex), 0, ino)
		if err != nil {
			t.Fatalf("Inode(%d): %v", ino, err)
		}
		if g.ParentID != w.ParentID || g.PrivateID != w.PrivateID || g.Create != w.CreateTime || g.Mod != w.ModTime || g.Change != w.ChangeTime || g.Access != w.AccessTime ||
			g.InternalFlags != w.InternalFlags || g.Links != w.Links || g.ProtClass != w.Prot || g.BsdFlags != w.Bsd || g.UID != w.Owner || g.GID != w.Group ||
			g.Mode != w.Mode || g.UncompressedSize != w.Uncompressed {
			t.Errorf("inode %d = %+v, oracle %+v", ino, g, *w)
		}
		if g.HasDstream || g.XattrCount != 0 {
			t.Errorf("inode %d has a dstream or xattrs: %+v", ino, g)
		}
	}

	// The directory records of the root parent (inode 1, which has no inode
	// record of its own).
	recs, err := f.DirRecords(int(ov.FsIndex), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != len(drecs) {
		t.Fatalf("%d directory records, oracle %d", len(recs), len(drecs))
	}
	for i, d := range drecs {
		if d.Parent != 1 || string(recs[i].Name) != d.Name || recs[i].FileID != d.FileID || recs[i].Flags != d.Flags {
			t.Errorf("record %d = %q -> %d (%#x), oracle %+v", i, recs[i].Name, recs[i].FileID, recs[i].Flags, d)
		}
	}

	// The root listing, the volume entry and the (empty) directories.
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 1 {
		t.Fatalf("root listing = %v, %v", es, err)
	}
	e := es[0]
	r := inodes[2]
	if e.Name != ov.Name || e.ID != "n:0:0:2" || e.Type != filesys.TypeDir || e.Encrypted || e.Mode != uint32(r.Mode) {
		t.Errorf("volume entry = %+v", e)
	}
	for k, want := range map[string]uint64{"modified": r.ModTime, "accessed": r.AccessTime, "changed": r.ChangeTime, "created": r.CreateTime} {
		ts := map[string]filesys.Timestamp{"modified": e.Times.Modified, "accessed": e.Times.Accessed, "changed": e.Times.Changed, "created": e.Times.Created}[k]
		if uint64(ts.T.UnixNano()) != want || !ts.ZoneKnown {
			t.Errorf("volume %s = %d, oracle %d", k, ts.T.UnixNano(), want)
		}
	}
	wantAttrs := []filesys.KV{
		{Key: "volume", Value: "0"},
		{Key: "uuid", Value: ov.UUID},
		{Key: "role", Value: "none"},
		{Key: "case_insensitive", Value: fmt.Sprint(ov.CaseInsens)},
		{Key: "normalization_insensitive", Value: fmt.Sprint(ov.NormInsens)},
		{Key: "encrypted", Value: "false"},
		{Key: "snapshots", Value: fmt.Sprint(ov.NumSnapshots)},
	}
	if !slices.Equal(e.Attrs, wantAttrs) {
		t.Errorf("volume attrs = %v, want %v", e.Attrs, wantAttrs)
	}
	if lk, err := f.Lookup("/" + ov.Name); err != nil || lk.ID != e.ID || lk.Name != e.Name {
		t.Errorf("Lookup(volume) = %+v, %v", lk, err)
	}
	// mkapfs writes no entries below the root; the reader adds the synthetic
	// .snapshots directory (this volume has no snapshots).
	kids, err := f.ReadDir(e)
	if err != nil || len(kids) != 1 || !filesys.IsSnapshotsDir(kids[0]) || kids[0].Name != ".snapshots" {
		t.Errorf("the root directory lists %v, %v; want only the synthetic .snapshots", kids, err)
	} else if v, _ := attr(kids[0], "snapshots"); v != fmt.Sprint(ov.NumSnapshots) {
		t.Errorf(".snapshots reports %q snapshots, the volume has %d", v, ov.NumSnapshots)
	}
	if _, err := f.Lookup("/" + ov.Name + "/root"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup below the root = %v, want ErrNotFound", err)
	}
	// The private directory is a directory too; it is not under the root.
	priv := filesys.Entry{ID: "n:0:0:3"}
	if kids, err := f.ReadDir(priv); err != nil || len(kids) != 0 {
		t.Errorf("the private directory lists %v, %v", kids, err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "n:0:0:1"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("inode 1 has no record: ReadDir = %v", err)
	}
	var walked int
	if err := filesys.Walk(f, f.Root(), "/", func(string, filesys.Entry, error) error { walked++; return nil }); err != nil || walked != 2 {
		t.Errorf("Walk visited %d entries (the volume and its .snapshots), %v", walked, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a real container: %q", w)
	}
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
