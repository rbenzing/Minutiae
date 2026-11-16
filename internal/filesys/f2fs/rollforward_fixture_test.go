package f2fs_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// The roll-forward fixture (testdata/rollforward/f2fs-rollforward.img.gz) is an F2FS volume the KERNEL f2fs
// driver left in a crash state inside the QEMU guest (tools/fixtures/f2fs_recover.sh, variant rollforward):
// files written and fsynced after a checkpoint, then QEMU quit with no checkpoint and no unmount. Its oracle
// (f2fs-rollforward.expect.json) comes from tools/fixtures/f2fs_recover_oracle.py, subcommand rollforward: an
// independent Python decoder of the finished image, of the crash image's warm-node log and of the kernel's own
// replay of a copy of it. These tests check that the fixture holds what its oracle claims, with RAW BYTE code
// of their own (the rfx* helpers below): they never call the roll-forward reader (RollForward,
// PostCheckpointFiles, Journal*); Task 13 compares the reader with the oracle. What only the oracle (at
// generation time) checks is listed in tools/fixtures/README.md, "F2FS roll-forward fixture (3H)": the kernel
// replay's hashes (replayed_sha256) and, for the indirect family, that the captured bytes are a prefix of the
// replayed file. The names do not collide with oracle, loadOracle, recoverOracle, loadRecoverFixture.

type rfNode struct {
	Block     uint32 `json:"block"`
	NID       uint32 `json:"nid"`
	Ino       uint32 `json:"ino"`
	Ofs       uint32 `json:"ofs"`
	Kind      string `json:"kind"`
	Flag      uint32 `json:"flag"`
	Fsync     bool   `json:"fsync"`
	Dentry    bool   `json:"dentry"`
	Next      uint32 `json:"next"`
	LiveIno   bool   `json:"live_ino"`
	WouldRepl bool   `json:"would_replay"`
}

type rfChain struct {
	Start    uint32   `json:"start"`
	Stop     string   `json:"stop"`
	Complete bool     `json:"complete"`
	Nodes    []rfNode `json:"nodes"`
}

type rfExpect struct {
	Outcome        string     `json:"outcome"`
	Skip           string     `json:"skip"`
	Captured       [][2]int64 `json:"captured"`
	CapturedSHA256 string     `json:"captured_sha256"`
	Prefix         bool       `json:"prefix"`
	Cut            bool       `json:"cut"`
	Stop           string     `json:"stop"`
	EqualSource    bool       `json:"equal_source"`
	Confidence     int        `json:"confidence"`
}

type rfFile struct {
	Path           string     `json:"path"`
	Family         string     `json:"family"`
	Ino            uint32     `json:"ino"`
	PIno           uint32     `json:"pino"`
	Kind           string     `json:"kind"`
	Size           int64      `json:"size"`
	ReplayedSHA256 string     `json:"replayed_sha256"`
	WrittenSHA256  string     `json:"written_sha256"`
	MapBlocks      [][2]int64 `json:"map_blocks"`
	Expect         rfExpect   `json:"expect"`
}

type rfOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
		Variant     string `json:"variant"`
		Packages    []struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"packages"`
		Commands            []string       `json:"commands"`
		Stages              []string       `json:"stages"`
		FsckExit            map[string]int `json:"fsck_exit"`
		ReplayOracle        bool           `json:"replay_oracle"`
		FsckChainCrosscheck bool           `json:"fsck_chain_crosscheck"`
	} `json:"generator"`
	CheckpointVersion     uint64         `json:"checkpoint_version"`
	BaseCheckpointVersion uint64         `json:"base_checkpoint_version"`
	CheckpointPack        int            `json:"checkpoint_pack"`
	CheckpointFlags       uint32         `json:"checkpoint_flags"`
	CheckpointCRC         uint32         `json:"checkpoint_crc"`
	CRCRecovery           bool           `json:"crc_recovery"`
	NoCRCRecovery         bool           `json:"nocrc_recovery"`
	FooterVersion         string         `json:"footer_version"`
	BlockSize             int            `json:"block_size"`
	Chain                 rfChain        `json:"chain"`
	FreeBlocks            [][2]int64     `json:"free_blocks"`
	Live                  []recLive      `json:"live"`
	Files                 []rfFile       `json:"files"`
	Families              map[string]int `json:"families"`
	Unmeasured            []string       `json:"unmeasured"`
}

const rfFixtureDir = "testdata/rollforward"

// loadRFFixture returns the decompressed image and the oracle of testdata/rollforward/<name>. The sha256 pin is
// checked first, then the oracle is decoded with DisallowUnknownFields (a field the schema does not know fails).
func loadRFFixture(t testing.TB, name string) ([]byte, rfOracle) {
	t.Helper()
	img := gunzipFixture(t, filepath.Join(rfFixtureDir, name+".img.gz"))
	raw, err := os.ReadFile(filepath.Join(rfFixtureDir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o rfOracle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("%s.expect.json: %v", name, err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match generator.image_sha256: regenerate image and oracle together", name)
	}
	return img, o
}

// ---------------------------------------------------------------- raw decoding (the test's own)

const (
	rfxMagic        = 0xF2F52010
	rfxBPS          = 512
	rfxCRCRecovery  = 0x40
	rfxNoCRCRecover = 0x200
	rfxUmount       = 0x1
	rfxXattrOfs     = 0xFFFFFFFF >> 3
)

func rfxRawCRC(seed uint32, b []byte) uint32 { return ^crc32.Update(^seed, crc32.IEEETable, b) }

type rfxCP struct {
	blk, flags, crc uint32
	ver             uint64
	warmSeg         uint32
	warmOff         uint16
}

type rfxSB struct{ cpAddr, mainAddr, segMain, rootIno uint32 }

func rfxSuper(t testing.TB, img []byte) rfxSB {
	t.Helper()
	raw := img[1024 : 1024+3072]
	if binary.LittleEndian.Uint32(raw) != rfxMagic {
		t.Fatal("bad superblock magic")
	}
	return rfxSB{
		cpAddr:   binary.LittleEndian.Uint32(raw[76:]),
		mainAddr: binary.LittleEndian.Uint32(raw[92:]),
		segMain:  binary.LittleEndian.Uint32(raw[68:]),
		rootIno:  binary.LittleEndian.Uint32(raw[96:]),
	}
}

// rfxCheckpoint picks the newer valid pack: crc right, header and last block of the pack carry one version.
func rfxCheckpoint(t testing.TB, img []byte, sb rfxSB) (best rfxCP, pack int) {
	t.Helper()
	for i, blk := range []uint32{sb.cpAddr, sb.cpAddr + rfxBPS} {
		h := img[int64(blk)*4096 : int64(blk)*4096+4096]
		ver := binary.LittleEndian.Uint64(h)
		off := binary.LittleEndian.Uint32(h[164:])
		total := binary.LittleEndian.Uint32(h[136:])
		if off < 192 || off > 4092 || total < 5 || total > rfxBPS {
			continue
		}
		crc := rfxRawCRC(rfxMagic, h[:off])
		if off+4 < 4092 {
			crc = rfxRawCRC(crc, h[off+4:4092])
		}
		if binary.LittleEndian.Uint32(h[off:]) != crc {
			continue
		}
		last := img[int64(blk+total-1)*4096:]
		if binary.LittleEndian.Uint64(last) != ver {
			continue
		}
		if pack == 0 || ver > best.ver {
			best = rfxCP{
				blk: blk, flags: binary.LittleEndian.Uint32(h[132:]), crc: crc, ver: ver,
				warmSeg: binary.LittleEndian.Uint32(h[36+4:]), warmOff: binary.LittleEndian.Uint16(h[68+2:]),
			}
			pack = i + 1
		}
	}
	if pack == 0 {
		t.Fatal("no valid checkpoint pack in the image")
	}
	return best, pack
}

func rfxKind(ofs uint32) string {
	switch {
	case ofs == rfxXattrOfs:
		return "xattr"
	case ofs == 0:
		return "inode"
	case ofs == 1 || ofs == 2:
		return "direct"
	case ofs == 3 || ofs == 1022:
		return "indirect"
	case (ofs >= 4 && ofs <= 1021) || (ofs >= 1023 && ofs <= 2040):
		return "direct"
	case ofs == 2041:
		return "dindirect"
	case (ofs-2042)%1019 == 0:
		return "indirect"
	}
	return "direct"
}

// rfxChainWalk walks the warm-node log of the chosen checkpoint with the kernel's rule: from the warm cursor,
// following next_blkaddr while the footer version equals the expected one. The walk is bounded.
func rfxChainWalk(t testing.TB, img []byte, sb rfxSB, cp rfxCP) (nodes []rfNode, stop string) {
	t.Helper()
	expect := cp.ver
	if cp.flags&rfxNoCRCRecover == 0 && cp.flags&rfxCRCRecovery != 0 {
		expect = cp.ver | uint64(cp.crc)<<32
	}
	end := min(uint32(len(img)/4096), sb.mainAddr+sb.segMain*rfxBPS)
	blk := sb.mainAddr + cp.warmSeg*rfxBPS + uint32(cp.warmOff)
	seen := map[uint32]bool{}
	for len(nodes) < 1<<16 {
		if blk < sb.mainAddr || blk >= end {
			return nodes, fmt.Sprintf("outside-main@%d", blk)
		}
		if seen[blk] {
			return nodes, fmt.Sprintf("cycle@%d", blk)
		}
		nid, ino, flag, ver := footerOf(t, img, blk)
		if ver != expect {
			return nodes, fmt.Sprintf("version@%d", blk)
		}
		if nid == 0 || ino == 0 {
			return nodes, fmt.Sprintf("footer@%d", blk)
		}
		seen[blk] = true
		next := binary.LittleEndian.Uint32(img[int64(blk)*4096+4092:])
		ofs := flag >> 3
		nodes = append(nodes, rfNode{
			Block: blk, NID: nid, Ino: ino, Ofs: ofs, Kind: rfxKind(ofs), Flag: flag,
			Fsync: flag&2 != 0, Dentry: flag&4 != 0, Next: next,
		})
		blk = next
	}
	t.Fatalf("the chain walk did not end within %d nodes", len(nodes))
	return nil, ""
}

func rfxIsInode(n rfNode) bool { return n.Ofs == 0 && n.NID == n.Ino }
func rfxIsDnode(n rfNode) bool { return rfxIsInode(n) || n.Kind == "direct" || n.Kind == "xattr" }

// rfxReplay restates the replay rules for the self-check: created, lastFsync and would_replay per node.
func rfxReplay(nodes []rfNode, live func(ino uint32) bool) (created map[uint32]bool, lastFsync map[uint32]int, would []bool) {
	created, lastFsync, would = map[uint32]bool{}, map[uint32]int{}, make([]bool, len(nodes))
	for i, n := range nodes {
		if rfxIsInode(n) && n.Fsync && n.Dentry && !live(n.Ino) {
			created[n.Ino] = true
		}
		if rfxIsDnode(n) && n.Fsync {
			lastFsync[n.Ino] = i
		}
	}
	for i, n := range nodes {
		last, ok := lastFsync[n.Ino]
		would[i] = rfxIsDnode(n) && (live(n.Ino) || created[n.Ino]) && ok && i <= last
	}
	return created, lastFsync, would
}

type rfxMap struct {
	size     int64
	inline   uint8
	pino     uint32
	nids     [5]uint32
	addrs    []uint32 // the first ceil(size/4096) logical block addresses the map reaches
	stop     string   // node-missing@<nid> when the map ends at a node the chain cannot serve
	inodeIdx int
}

// rfxFileMap builds the map of file ino from the chain: the last inode node at or before the last fsync node,
// its address slots, then the last chain direct node (index <= last fsync) for i_nid[0] (ofs 1) and i_nid[1]
// (ofs 2); i_nid[2] (indirect) is never served from the chain.
func rfxFileMap(t testing.TB, img []byte, nodes []rfNode, ino uint32, last int) rfxMap {
	t.Helper()
	m := rfxMap{inodeIdx: -1}
	for i, n := range nodes {
		if i <= last && n.Ino == ino && rfxIsInode(n) {
			m.inodeIdx = i
		}
	}
	if m.inodeIdx < 0 {
		t.Fatalf("ino %d: no inode node at or before the last fsync node %d", ino, last)
	}
	b := img[int64(nodes[m.inodeIdx].Block)*4096 : int64(nodes[m.inodeIdx].Block)*4096+4096]
	m.size = int64(binary.LittleEndian.Uint64(b[16:]))
	m.inline = b[3]
	m.pino = binary.LittleEndian.Uint32(b[84:])
	for j := range m.nids {
		m.nids[j] = binary.LittleEndian.Uint32(b[4052+4*j:])
	}
	start := 360
	if m.inline&0x20 != 0 {
		start += int(binary.LittleEndian.Uint16(b[360:]))
	}
	cnt := (4052 - start) / 4
	if m.inline&0x01 != 0 {
		cnt -= 50
	}
	need := (m.size + 4095) / 4096
	emit := func(a uint32) bool {
		m.addrs = append(m.addrs, a)
		return int64(len(m.addrs)) == need
	}
	if need == 0 {
		return m
	}
	for k := range cnt {
		if emit(binary.LittleEndian.Uint32(b[start+4*k:])) {
			return m
		}
	}
	dnode := func(nid, ofs uint32) bool { // true when the map is complete or stopped
		if nid == 0 {
			for range 1018 {
				if emit(0) {
					return true
				}
			}
			return false
		}
		src := -1
		for i, n := range nodes {
			if i <= last && n.NID == nid && n.Ino == ino && n.Ofs == ofs && n.Kind == "direct" {
				src = i
			}
		}
		if src < 0 {
			m.stop = fmt.Sprintf("node-missing@%d", nid)
			return true
		}
		d := img[int64(nodes[src].Block)*4096:]
		for k := range 1018 {
			if emit(binary.LittleEndian.Uint32(d[4*k:])) {
				return true
			}
		}
		return false
	}
	if dnode(m.nids[0], 1) || dnode(m.nids[1], 2) {
		return m
	}
	if m.nids[2] != 0 {
		m.stop = fmt.Sprintf("node-missing@%d", m.nids[2])
		return m
	}
	t.Fatalf("ino %d: the map needs %d blocks and the tree gave %d with no stop", ino, need, len(m.addrs))
	return m
}

func rfxHole(a uint32) bool { return a == 0 || a == 0xFFFFFFFF }

// rfxExtents is the inclusive absolute ranges of the data blocks in file order; a hole ends a range.
func rfxExtents(addrs []uint32) [][2]int64 {
	var out [][2]int64
	prev := int64(-1)
	for _, a := range addrs {
		if rfxHole(a) {
			prev = -1
			continue
		}
		if prev >= 0 && int64(a) == prev+1 {
			out[len(out)-1][1] = int64(a)
		} else {
			out = append(out, [2]int64{int64(a), int64(a)})
		}
		prev = int64(a)
	}
	return out
}

func rfxRanges(blocks []int64) [][2]int64 {
	var out [][2]int64
	for _, b := range blocks {
		if n := len(out); n > 0 && out[n-1][1] == b-1 {
			out[n-1][1] = b
		} else {
			out = append(out, [2]int64{b, b})
		}
	}
	return out
}

// rfWant pins, per family, what the fixture must say (the outcome, not only that the family is present).
type rfWantFamily struct {
	path        string
	kind        string // the file's kind
	outcome     string
	skip        string
	prefix      bool
	stop        string // prefix of expect.stop
	equalSource bool
	confidence  int
	size        int64
}

var rfWant = map[string]rfWantFamily{
	"small":      {path: "/rf/small/target.bin", kind: "post-checkpoint", outcome: "recover", equalSource: true, confidence: 60, size: 20 << 10},
	"direct":     {path: "/rf/direct/target.bin", kind: "post-checkpoint", outcome: "recover", equalSource: true, confidence: 60, size: 5 << 20},
	"indirect":   {path: "/rf/indirect/target.bin", kind: "post-checkpoint", outcome: "recover", prefix: true, stop: "node-missing@", confidence: 20, size: 12 << 20},
	"holes":      {path: "/rf/holes/target.bin", kind: "post-checkpoint", outcome: "recover", prefix: true, stop: "hole@1", confidence: 20, size: 16 << 10},
	"inline":     {path: "/rf/inline/target.txt", kind: "post-checkpoint", outcome: "skip", skip: "unsupported", size: 100},
	"twice":      {path: "/rf/twice/target.bin", kind: "post-checkpoint", outcome: "recover", equalSource: true, confidence: 60, size: 16 << 10},
	"late":       {path: "/rf/late/target.bin", kind: "post-checkpoint", outcome: "recover", equalSource: true, confidence: 60, size: 8 << 10},
	"nofsync":    {path: "/rf/nofsync/target.bin", kind: "absent|not-replayed", outcome: "report-only"},
	"append":     {path: "/upd/append/target.bin", kind: "live-update", outcome: "report-only", size: 28 << 10},
	"appendpast": {path: "/upd/appendpast/target.bin", kind: "live-update", outcome: "report-only", size: 4<<20 + 8<<10},
}

func TestF2FSRollForwardFixtureHoldsWhatItClaims(t *testing.T) {
	img, o := loadRFFixture(t, "f2fs-rollforward") // the sha256 pin and the strict schema come first
	if o.BlockSize != 4096 {
		t.Errorf("block_size %d", o.BlockSize)
	}
	g := o.Generator
	if g.Variant != "rollforward" {
		t.Errorf("generator.variant %q", g.Variant)
	}
	if !slices.Equal(g.Stages, []string{"w", "c"}) {
		t.Errorf("generator.stages %v, want [w c]", g.Stages)
	}
	if code, ok := g.FsckExit["w"]; !ok || code != 0 {
		t.Errorf("fsck.f2fs after stage w: %d (recorded %v)", code, ok)
	}
	if _, ok := g.FsckExit["c"]; !ok {
		t.Errorf("generator.fsck_exit lacks the crash stage: %v", g.FsckExit)
	}
	if code, ok := g.FsckExit["r"]; ok && code != 0 {
		t.Errorf("fsck.f2fs on the replayed copy exited %d", code)
	}
	if !g.ReplayOracle || g.FsckChainCrosscheck {
		t.Errorf("replay_oracle %v fsck_chain_crosscheck %v: the kernel replay is the content oracle and fsck.f2fs prints no chain (H3)", g.ReplayOracle, g.FsckChainCrosscheck)
	}
	if len(g.Commands) == 0 || len(g.Packages) == 0 {
		t.Error("generator.commands or generator.packages is empty")
	}

	// the checkpoint: the oracle's fields against this test's own decoder and the live reader's
	sb := rfxSuper(t, img)
	cp, pack := rfxCheckpoint(t, img, sb)
	if cp.ver != o.CheckpointVersion || pack != o.CheckpointPack || cp.flags != o.CheckpointFlags || cp.crc != o.CheckpointCRC {
		t.Errorf("checkpoint raw: version %d pack %d flags %#x crc %#x; oracle %d %d %#x %#x",
			cp.ver, pack, cp.flags, cp.crc, o.CheckpointVersion, o.CheckpointPack, o.CheckpointFlags, o.CheckpointCRC)
	}
	if o.CRCRecovery != (cp.flags&rfxCRCRecovery != 0) || o.NoCRCRecovery != (cp.flags&rfxNoCRCRecover != 0) || !o.CRCRecovery || o.NoCRCRecovery {
		t.Errorf("crc_recovery %v nocrc_recovery %v with flags %#x: the kernel wrote a CRC-recovery checkpoint", o.CRCRecovery, o.NoCRCRecovery, cp.flags)
	}
	// the checkpoint the crash ends on is the stage-w UNMOUNT checkpoint: the base sync wrote none (H5), so the crash is the
	// log of fsynced nodes that follows it, not an unclean-checkpoint flag
	if cp.flags&rfxUmount == 0 {
		t.Errorf("checkpoint flags %#x lack CP_UMOUNT_FLAG: the crash image should end on the stage-w unmount checkpoint", cp.flags)
	}
	if want := fmt.Sprintf("0x%016x", cp.ver|uint64(cp.crc)<<32); o.FooterVersion != want {
		t.Errorf("footer_version %s, version|crc<<32 is %s", o.FooterVersion, want)
	}
	if o.BaseCheckpointVersion != o.CheckpointVersion {
		t.Errorf("a checkpoint was written after the base sync: base %d, crash image %d (H5)", o.BaseCheckpointVersion, o.CheckpointVersion)
	}
	fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if lcp := fsys.Checkpoint(); lcp.Version != o.CheckpointVersion || lcp.Flags != o.CheckpointFlags {
		t.Errorf("live reader: checkpoint version %d flags %#x; oracle %d %#x", lcp.Version, lcp.Flags, o.CheckpointVersion, o.CheckpointFlags)
	}
	feats := fsys.Info().Features
	for _, f := range []string{"extra_attr", "inode_checksum", "sb_checksum"} {
		if !slices.Contains(feats, f) {
			t.Errorf("Info().Features %v lacks %s", feats, f)
		}
	}

	// free_blocks is exactly what Unallocated reports
	runs, err := fsys.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	var wantRuns []filesys.Run
	for _, r := range o.FreeBlocks {
		wantRuns = append(wantRuns, filesys.Run{Offset: r[0] * 4096, Length: (r[1] - r[0] + 1) * 4096})
	}
	if len(wantRuns) == 0 || !slices.Equal(runs, wantRuns) {
		t.Errorf("Unallocated differs from the oracle's free blocks:\n got  %v\n want %v", runs, wantRuns)
	}

	// the chain: this test's own walk equals the oracle's, node by node
	nodes, stop := rfxChainWalk(t, img, sb, cp)
	if sb.mainAddr+sb.segMain*rfxBPS > uint32(len(img)/4096) {
		t.Fatal("the image is shorter than the main area")
	}
	wantStart := sb.mainAddr + cp.warmSeg*rfxBPS + uint32(cp.warmOff)
	if o.Chain.Start != wantStart {
		t.Errorf("chain.start %d, warm cursor says %d", o.Chain.Start, wantStart)
	}
	if o.Chain.Stop != stop || !strings.HasPrefix(stop, "version@") || !o.Chain.Complete {
		t.Errorf("chain.stop %q complete %v; raw walk stops at %q (a kernel chain ends with a version mismatch)", o.Chain.Stop, o.Chain.Complete, stop)
	}
	if len(nodes) == 0 || len(nodes) != len(o.Chain.Nodes) {
		t.Fatalf("chain: raw walk finds %d nodes, oracle %d", len(nodes), len(o.Chain.Nodes))
	}
	live := func(ino uint32) bool {
		_, _, err := fsys.Inode(ino)
		if err == nil {
			return true
		}
		if !errors.Is(err, filesys.ErrNotFound) {
			t.Fatalf("live reader: inode %d: %v (neither found nor not-found)", ino, err)
		}
		return false
	}
	created, lastFsync, would := rfxReplay(nodes, live)
	for i, n := range nodes {
		on := o.Chain.Nodes[i]
		if on.Block != n.Block || on.NID != n.NID || on.Ino != n.Ino || on.Ofs != n.Ofs || on.Kind != n.Kind || on.Flag != n.Flag ||
			on.Fsync != n.Fsync || on.Dentry != n.Dentry || on.Next != n.Next {
			t.Errorf("chain[%d]: raw %+v, oracle %+v", i, n, on)
		}
		if on.LiveIno != live(n.Ino) || on.WouldRepl != would[i] {
			t.Errorf("chain[%d] (blk %d nid %d): live_ino %v would_replay %v; this test says %v %v", i, n.Block, n.NID, on.LiveIno, on.WouldRepl, live(n.Ino), would[i])
		}
	}

	// every chain inode is explained by exactly one file of the oracle, and every file's claims hold
	byFam := map[string]rfFile{}
	for _, f := range o.Files {
		if _, dup := byFam[f.Family]; dup {
			t.Errorf("family %q listed twice", f.Family)
		}
		byFam[f.Family] = f
	}
	explained := map[uint32]string{}
	for fam, want := range rfWant {
		f, present := byFam[fam]
		if fam == "appendpast" && !present {
			if !slices.Equal(o.Unmeasured, []string{"appendpast"}) || o.Families[fam] != 0 {
				t.Errorf("appendpast is absent but unmeasured is %v and families[appendpast] is %d", o.Unmeasured, o.Families[fam])
			}
			continue
		}
		if !present {
			t.Errorf("family %q is missing: the generator should have exited 1", fam)
			continue
		}
		if fam == "appendpast" && len(o.Unmeasured) != 0 {
			t.Errorf("appendpast is present and also unmeasured: %v", o.Unmeasured)
		}
		if o.Families[fam] != 1 {
			t.Errorf("families[%q] is %d", fam, o.Families[fam])
		}
		checkRFFamily(t, img, fsys, o, nodes, created, lastFsync, fam, want, f)
		if f.Ino != 0 {
			explained[f.Ino] = fam
		}
	}
	for fam := range o.Families {
		if _, ok := rfWant[fam]; !ok {
			t.Errorf("families lists %q, which the test does not know", fam)
		}
	}
	for _, n := range nodes {
		if _, ok := explained[n.Ino]; !ok {
			t.Errorf("chain node blk %d (nid %d ino %d) belongs to no file of the oracle", n.Block, n.NID, n.Ino)
		}
	}
	// the post-checkpoint inode numbers have no NAT entry in the crash image: the live reader cannot see them
	for _, f := range o.Files {
		if f.Kind == "post-checkpoint" && live(f.Ino) {
			t.Errorf("%s: inode %d is reachable through the NAT of the crash image", f.Path, f.Ino)
		}
	}
	if t.Failed() {
		t.Logf("chain stop %q, %d nodes", stop, len(nodes))
	}
}

// checkRFFamily checks one file entry against the raw image and the pinned expectation of its family.
func checkRFFamily(t *testing.T, img []byte, fsys *f2fs.FS, o rfOracle, nodes []rfNode, created map[uint32]bool, lastFsync map[uint32]int, fam string, want rfWantFamily, f rfFile) {
	t.Helper()
	name := fam + " " + f.Path
	if f.Path != want.path {
		t.Errorf("%s: path %q, want %q", fam, f.Path, want.path)
	}
	if !slices.Contains(strings.Split(want.kind, "|"), f.Kind) {
		t.Errorf("%s: kind %q, want %q", name, f.Kind, want.kind)
	}
	if f.Expect.Outcome != want.outcome {
		t.Errorf("%s: outcome %q, want %q", name, f.Expect.Outcome, want.outcome)
	}
	if f.Expect.Skip != want.skip {
		t.Errorf("%s: skip %q, want %q", name, f.Expect.Skip, want.skip)
	}
	// the directory is live; the file is a tree entry exactly when it was live before the crash
	dir, derr := fsys.Lookup(path.Dir(f.Path))
	if derr != nil {
		t.Errorf("%s: the parent directory is not live: %v", name, derr)
	}
	_, ferr := fsys.Lookup(f.Path)
	switch f.Kind {
	case "live-update":
		if ferr != nil {
			t.Errorf("%s: a live file is not found by Lookup: %v", name, ferr)
		}
	default:
		if !errors.Is(ferr, filesys.ErrNotFound) {
			t.Errorf("%s: Lookup of a file that exists only in the roll-forward chain gave %v, want not-found", name, ferr)
		}
	}
	if f.Kind == "absent" || f.Kind == "not-replayed" {
		if f.Expect.Outcome != "report-only" || len(f.Expect.Captured) != 0 || f.Expect.Confidence != 0 {
			t.Errorf("%s: a %s file must be report-only with nothing captured: %+v", name, f.Kind, f.Expect)
		}
		if f.Kind == "absent" {
			// no chain inode node carries this name under this parent
			for _, n := range nodes {
				if rfxIsInode(n) && inodeNamed(img, n.Block, path.Base(f.Path)) && inodePino(img, n.Block) == dirIno(dir) {
					t.Errorf("%s: kind absent, yet chain node blk %d is an inode named %s in the parent", name, n.Block, path.Base(f.Path))
				}
			}
		} else if created[f.Ino] {
			t.Errorf("%s: kind not-replayed but the replay rules create inode %d", name, f.Ino)
		}
		return
	}
	// every other kind has chain nodes of its inode
	last, ok := lastFsync[f.Ino]
	if !ok {
		t.Fatalf("%s: inode %d has no fsync dnode in the chain", name, f.Ino)
	}
	var mine int
	for _, n := range nodes {
		if n.Ino == f.Ino {
			mine++
		}
	}
	m := rfxFileMap(t, img, nodes, f.Ino, last)
	if m.size != f.Size || (want.size != 0 && f.Size != want.size) {
		t.Errorf("%s: size %d (raw inode %d), want %d", name, f.Size, m.size, want.size)
	}
	if dirIno(dir) != f.PIno || m.pino != f.PIno {
		t.Errorf("%s: pino %d (raw inode %d, live parent %d)", name, f.PIno, m.pino, dirIno(dir))
	}
	if f.Kind == "live-update" {
		if created[f.Ino] || !hasNode(nodes, func(n rfNode) bool { return n.Ino == f.Ino }) {
			t.Errorf("%s: a live update must not be a created inode and must have chain nodes", name)
		}
		if fam == "appendpast" && !hasNode(nodes, func(n rfNode) bool { return n.Ino == f.Ino && n.Kind == "direct" }) {
			t.Errorf("%s: the family needs a direct node in the chain beside the inode", name)
		}
		if fam == "append" && mine != 1 {
			t.Errorf("%s: %d chain nodes, want only the inode", name, mine)
		}
		return
	}
	if !created[f.Ino] {
		t.Errorf("%s: kind post-checkpoint but the replay rules do not create inode %d", name, f.Ino)
	}
	// family shapes
	switch fam {
	case "small":
		if mine != 1 || m.nids[0] != 0 {
			t.Errorf("%s: %d chain nodes, i_nid %v: want the inode alone", name, mine, m.nids)
		}
	case "direct":
		if m.nids[0] == 0 || m.nids[2] != 0 {
			t.Errorf("%s: i_nid %v: want i_nid[0] and no i_nid[2]", name, m.nids)
		}
	case "indirect":
		if m.nids[2] == 0 {
			t.Errorf("%s: i_nid %v: want i_nid[2]", name, m.nids)
		}
	case "inline":
		if m.inline&0x02 == 0 {
			t.Errorf("%s: inline flags %#x: the file is not stored inline", name, m.inline)
		}
	case "twice":
		inodes := 0
		for _, n := range nodes {
			if n.Ino == f.Ino && rfxIsInode(n) {
				inodes++
			}
		}
		if inodes < 2 {
			t.Errorf("%s: %d inode nodes in the chain, want at least 2", name, inodes)
		}
	}
	if m.inline&0x02 != 0 {
		if f.Expect.Outcome != "skip" || f.Expect.Skip != "unsupported" || len(f.Expect.Captured) != 0 {
			t.Errorf("%s: inline data must be skipped as unsupported: %+v", name, f.Expect)
		}
		return
	}
	// the map: addresses from the inode and the chain's direct nodes, until a hole or a node the chain lacks
	if got := rfxExtents(m.addrs); !slices.Equal(got, f.MapBlocks) {
		t.Errorf("%s: map_blocks %v, raw decode %v", name, f.MapBlocks, got)
	}
	var lead []int64
	stopTok := m.stop
	for lb, a := range m.addrs {
		if rfxHole(a) {
			stopTok = fmt.Sprintf("hole@%d", lb)
			break
		}
		lead = append(lead, int64(a))
	}
	var captured []int64
	for _, b := range lead {
		if !inRanges(o.FreeBlocks, b) {
			break
		}
		captured = append(captured, b)
	}
	cut := len(captured) < len(lead)
	if want.prefix != (stopTok != "") || (want.prefix && !strings.HasPrefix(stopTok, want.stop)) {
		t.Errorf("%s: the map stops with %q, want prefix=%v %q", name, stopTok, want.prefix, want.stop)
	}
	if f.Expect.Stop != stopTok || f.Expect.Prefix != (stopTok != "") || f.Expect.Cut != cut {
		t.Errorf("%s: expect stop %q prefix %v cut %v; raw decode %q %v %v", name, f.Expect.Stop, f.Expect.Prefix, f.Expect.Cut, stopTok, stopTok != "", cut)
	}
	if got := rfxRanges(captured); !slices.Equal(got, f.Expect.Captured) || len(captured) == 0 {
		t.Errorf("%s: captured %v, raw decode %v", name, f.Expect.Captured, got)
	}
	body := imageBlocks(t, img, f.Expect.Captured, f.Size)
	if got := hexSum(body); got != f.Expect.CapturedSHA256 {
		t.Errorf("%s: captured_sha256 %s, the image's captured blocks hash to %s", name, f.Expect.CapturedSHA256, got)
	}
	equal := !f.Expect.Prefix && !cut && hexSum(body) == f.WrittenSHA256
	if f.Expect.EqualSource != equal || f.Expect.EqualSource != want.equalSource {
		t.Errorf("%s: equal_source %v; the captured bytes %v the written bytes, want %v", name, f.Expect.EqualSource, equal, want.equalSource)
	}
	if f.WrittenSHA256 == "" || f.ReplayedSHA256 == "" {
		t.Errorf("%s: written_sha256 %q or replayed_sha256 %q is empty", name, f.WrittenSHA256, f.ReplayedSHA256)
	}
	if !f.Expect.Prefix && f.ReplayedSHA256 != f.WrittenSHA256 {
		t.Errorf("%s: a complete map but the kernel replay (%s) differs from the bytes written (%s)", name, f.ReplayedSHA256, f.WrittenSHA256)
	}
	// confidence: 60, 20 for a prefix or a cut (the chain is complete, checked above), 30 where files share a block
	conf := 60
	if f.Expect.Prefix || cut {
		conf = 20
	}
	for _, other := range o.Files {
		if other.Path != f.Path && other.Expect.Outcome == "recover" && slices.ContainsFunc(f.Expect.Captured, func(r [2]int64) bool {
			return slices.ContainsFunc(other.Expect.Captured, func(q [2]int64) bool { return r[0] <= q[1] && q[0] <= r[1] })
		}) {
			conf = min(conf, 30)
		}
	}
	if f.Expect.Confidence != conf || conf != want.confidence {
		t.Errorf("%s: confidence %d, rules say %d, family pins %d", name, f.Expect.Confidence, conf, want.confidence)
	}
}

func hasNode(nodes []rfNode, pred func(rfNode) bool) bool { return slices.ContainsFunc(nodes, pred) }

// dirIno returns the inode number of a live directory entry ("nid:<n>").
func dirIno(e filesys.Entry) uint32 {
	rest, ok := strings.CutPrefix(e.ID, "nid:")
	if !ok {
		return 0
	}
	v, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

func inodePino(img []byte, blk uint32) uint32 {
	return binary.LittleEndian.Uint32(img[int64(blk)*4096+84:])
}

func inodeNamed(img []byte, blk uint32, name string) bool {
	b := img[int64(blk)*4096 : int64(blk)*4096+4096]
	n := binary.LittleEndian.Uint32(b[88:])
	return n <= 255 && string(b[92:92+n]) == name
}

func TestF2FSRollForwardFixtureLiveFilesMatchOracle(t *testing.T) {
	img, o := loadRFFixture(t, "f2fs-rollforward")
	fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]recLive{}
	for _, l := range o.Live {
		byPath[l.Path] = l
	}
	if len(byPath) != len(o.Live) || len(byPath) == 0 {
		t.Fatalf("oracle live list: %d entries, %d distinct", len(o.Live), len(byPath))
	}
	need := map[string]bool{
		"/live/indirect.bin": false, "/live/holes.bin": false, "/live/inline.txt": false,
		"/upd/append/target.bin": false, "/upd/appendpast/target.bin": false,
	}
	seen := map[string]bool{}
	err = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if e.Deleted || e.Type != filesys.TypeFile {
			return nil
		}
		l, ok := byPath[p]
		if !ok {
			t.Errorf("%s: a live file the oracle does not list", p)
			return nil
		}
		seen[p] = true
		checkLiveFile(t, fsys, e, l)
		if _, tracked := need[p]; tracked {
			need[p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := range byPath {
		if !seen[p] {
			t.Errorf("%s: in the oracle, not found live by the reader", p)
		}
	}
	for p, ok := range need {
		if !ok {
			t.Errorf("%s is not among the live files: the check would be vacuous", p)
		}
	}
	// the append family reads as of stage w: 20 KiB, though the kernel replay would make it 28 KiB
	if l := byPath["/upd/append/target.bin"]; l.Size != 20<<10 {
		t.Errorf("/upd/append/target.bin: live size %d, want the stage-w 20 KiB", l.Size)
	}
	if l := byPath["/upd/appendpast/target.bin"]; l.Size != 4<<20 {
		t.Errorf("/upd/appendpast/target.bin: live size %d, want the stage-w 4 MiB", l.Size)
	}
	small, err := fsys.Lookup("/live/small")
	if err != nil {
		t.Fatal(err)
	}
	if v, _, err := fsys.Inode(dirIno(small)); err != nil || v.Inline&0x04 == 0 {
		t.Errorf("/live/small: inline flags %#x, %v: the directory is not stored with inline dentries", v.Inline, err)
	}
	// an unmount checkpoint is no warning of the live reader, and the roll-forward chain adds none (3H-R2)
	if w := fsys.Info().Warnings; len(w) != 0 {
		t.Errorf("Info().Warnings after reading every live file: %v, want none", w)
	}
}

// TestF2FSRollForwardFixtureLiveViewSignalsTheChain: a clean-looking live view (no unclean-checkpoint warning, because
// the crash image ends on the stage-w UNMOUNT checkpoint) must not imply that nothing followed the checkpoint. The
// reader's Journal() view is the signal. This is the one test of this file that calls the roll-forward reader (only
// Journal(), only for these two facts); the claims test above stays reader-independent.
func TestF2FSRollForwardFixtureLiveViewSignalsTheChain(t *testing.T) {
	img, o := loadRFFixture(t, "f2fs-rollforward")
	fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if w := fsys.Info().Warnings; len(w) != 0 {
		t.Errorf("Info().Warnings %v: the chain must add no filesystem warning (3H-R2)", w)
	}
	ji, err := fsys.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if !ji.NeedsRecovery {
		t.Errorf("Journal().NeedsRecovery is false on an image whose chain holds %d nodes, %d of them would be replayed", len(o.Chain.Nodes), countWould(o))
	}
	if ji.Blocks != int64(len(o.Chain.Nodes)) {
		t.Errorf("Journal().Blocks %d, oracle chain %d nodes", ji.Blocks, len(o.Chain.Nodes))
	}
	if !slices.ContainsFunc(ji.Warnings, func(w string) bool {
		return strings.Contains(w, "unmount checkpoint is followed by a roll-forward chain")
	}) {
		t.Errorf("Journal().Warnings %v lack the notice that an unmount checkpoint is followed by a roll-forward chain", ji.Warnings)
	}
}

func countWould(o rfOracle) int {
	n := 0
	for _, c := range o.Chain.Nodes {
		if c.WouldRepl {
			n++
		}
	}
	return n
}
