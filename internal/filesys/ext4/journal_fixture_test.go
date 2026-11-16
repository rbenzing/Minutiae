package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// The journal fixtures (testdata/journal/) are three images whose jbd2 journal was written by the KERNEL ext4 driver in the QEMU
// guest (tools/fixtures/ext4_journal.sh, plan 3G Task 13): ext4-journal-clean (clean unmounts only: no live region, stale
// fragments), ext4-journal-crash (the last boot ended with a no-sync reboot: needs_recovery, a live region) and ext3-journal-clean
// (block maps, no journal checksums). Their oracle (*.expect.json) comes from tools/fixtures/ext4_journal_oracle.py: an independent
// jbd2 decoder, cross-checked with logdump, dumpe2fs and debugfs, and the kernel's replay of a copy of the crash image. These tests
// hold the fixtures to their own claims with raw byte parsing of their own and never run the journal reader; Task 14 holds the
// reader to the oracle.

type journalFixtureRuns = recoverRuns

type journalFixtureTag struct {
	FSBlock int64  `json:"fsblock"`
	JBlk    int64  `json:"jblk"`
	Flags   int    `json:"flags"`
	Copy    string `json:"copy"`
	Revoked bool   `json:"revoked"`
	Escaped bool   `json:"escaped"`
}

type journalFixtureTxn struct {
	Seq       uint32              `json:"seq"`
	Region    string              `json:"region"`
	StartJBlk int64               `json:"start_jblk"`
	CommitJ   int64               `json:"commit_jblk"`
	Committed bool                `json:"committed"`
	Checksum  string              `json:"checksum"`
	Revoked   []int64             `json:"revoked"`
	LogDump   bool                `json:"logdump"`
	Tags      []journalFixtureTag `json:"tags"`
}

type journalFixtureExpect struct {
	Outcome          string             `json:"outcome"`
	Skip             string             `json:"skip"`
	Method           string             `json:"method"`
	Seq              uint32             `json:"seq"`
	Region           string             `json:"region"`
	Size             int64              `json:"size"`
	CapturedBlocks   journalFixtureRuns `json:"captured_blocks"`
	Cut              bool               `json:"cut"`
	Incomplete       bool               `json:"incomplete"`
	Confidence       int                `json:"confidence"`
	Assumptions      []string           `json:"assumptions"`
	EqualSource      string             `json:"equal_source"`
	Stop             string             `json:"stop"`
	CandidateMethods []string           `json:"candidate_methods"`
	Overlaps         []string           `json:"overlaps"`
	TreeSources      []string           `json:"tree_sources"`
}

type journalFixtureCopies struct {
	Copies              int   `json:"copies"`
	Revoked             int   `json:"revoked"`
	OtherGeneration     int   `json:"other_generation"`
	DeletedState        int   `json:"deleted_state"`
	LiveState           int   `json:"live_state"`
	NewestLiveStateSize int64 `json:"newest_live_state_size"`
	NewestLiveStateSeq  int64 `json:"newest_live_state_seq"`
}

type journalFixtureDeleted struct {
	Path          string                          `json:"path"`
	Name          string                          `json:"name"`
	Family        string                          `json:"family"`
	Inode         uint32                          `json:"inode"`
	Generation    uint32                          `json:"generation"`
	SourceSHA256  string                          `json:"source_sha256"`
	SourceSize    int64                           `json:"source_size"`
	TrueBasis     string                          `json:"true_basis"`
	TrueBlocks    journalFixtureRuns              `json:"true_blocks"`
	TreeBlocks    []int64                         `json:"tree_blocks"`
	KernelState   recoverKernelState              `json:"kernel_state"`
	Expect        map[string]journalFixtureExpect `json:"expect"`
	JournalCopies journalFixtureCopies            `json:"journal_copies"`
}

type journalFixtureOnly struct {
	Path             string     `json:"path"`
	Family           string     `json:"family"`
	Inode            uint32     `json:"inode"`
	ReplayedSize     int64      `json:"replayed_size"`
	ReplayedSHA256   string     `json:"replayed_sha256"`
	AsFoundPresent   bool       `json:"as_found_present"`
	AsFoundSize      int64      `json:"as_found_size"`
	AsFoundSHA256    string     `json:"as_found_sha256"`
	InodeTableCopies [][2]int64 `json:"inode_table_copies"`
	CopySeqs         []uint32   `json:"copy_seqs"`
}

type journalFixtureLive struct {
	Path         string `json:"path"`
	Family       string `json:"family"`
	Inode        uint32 `json:"inode"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	SourceSHA256 string `json:"source_sha256"`
	SourceSize   int64  `json:"source_size"`
}

type journalFixtureOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
		Kernel      string `json:"kernel"`
		Packages    []struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"packages"`
		Commands     []string          `json:"commands"`
		FsckExit     int               `json:"fsck_exit"`
		Gates        map[string]string `json:"gates"`
		LogdumpOld   bool              `json:"logdump_old"`
		ReplayOracle bool              `json:"replay_oracle"`
	} `json:"generator"`
	FSType            string `json:"fs_type"`
	BlockSize         int64  `json:"block_size"`
	DeletedEntryCount int    `json:"deleted_entry_count"`
	UnlistedCount     int    `json:"unlisted_count"`
	Totals            map[string]struct {
		Recovered int            `json:"recovered"`
		SkippedBy map[string]int `json:"skipped_by"`
		Undecided int            `json:"undecided"`
	} `json:"totals"`
	Journal struct {
		Inode           int      `json:"inode"`
		Version         int      `json:"version"`
		BlockSize       int64    `json:"block_size"`
		Blocks          int64    `json:"blocks"`
		First           int64    `json:"first"`
		Sequence        uint32   `json:"sequence"`
		Start           int64    `json:"start"`
		Features        []string `json:"features"`
		FeatureCompat   uint32   `json:"feature_compat"`
		FeatureIncompat uint32   `json:"feature_incompat"`
		NeedsRecovery   bool     `json:"needs_recovery"`
		LiveStop        string   `json:"live_stop"`
		CsumType        int      `json:"csum_type"`
		TagSize         int      `json:"tag_size"`
	} `json:"journal"`
	Txns        []journalFixtureTxn     `json:"txns"`
	Deleted     []journalFixtureDeleted `json:"deleted"`
	JournalOnly []journalFixtureOnly    `json:"journal_only"`
	Live        []journalFixtureLive    `json:"live"`
	Families    map[string]struct {
		Kind  string `json:"kind"`
		Count int    `json:"count"`
	} `json:"families"`
	Replay *struct {
		Files map[string]struct {
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
		Dmesg []string `json:"dmesg"`
	} `json:"replay"`
}

// journalFixtureImages and the families each must hold (the generator exits 1 naming a missing one; the test makes a regeneration
// that loses one fail too, so the self-check cannot go vacuous). jsameboot is builder-only (3G probe gate J7).
var journalFixtureImages = []string{"ext4-journal-clean", "ext4-journal-crash", "ext3-journal-clean"}

var requiredJournalFamilies = map[string][]string{
	"ext4-journal-clean": {"jsmall", "jdeep", "jtrunc", "jgone", "jreuse", "jhardlink", "jinline", "jswap", "live-file"},
	"ext4-journal-crash": {"crash-live-copy", "crash-created", "crash-update", "live-file"},
	"ext3-journal-clean": {"bm-small", "bm-indirect"},
}

// loadJournalFixture returns the decompressed image of testdata/journal/<name>.img.gz and its oracle, after the gzip budget and the
// image sha256 against generator.image_sha256 (a stale oracle fails here, first). The oracle is parsed with an unknown-field check.
func loadJournalFixture(t testing.TB, name string) ([]byte, *journalFixtureOracle) {
	t.Helper()
	dir := filepath.Join("testdata", "journal")
	gz := filepath.Join(dir, name+".img.gz")
	st, err := os.Stat(gz)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxRecoverFixtureGz {
		t.Fatalf("%s is %d bytes gzipped, over the %d byte budget", gz, st.Size(), maxRecoverFixtureGz)
	}
	img := gunzipFixture(t, gz)
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o journalFixtureOracle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("%s oracle: %v", name, err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, &o
}

// jfExt is the little a self-check needs of an ext2/3/4 image: the superblock fields and the block map of one inode, read from the
// bytes with code of its own.
type jfExt struct {
	img      []byte
	bs       int64
	first    int64
	bpg, ipg int64
	isz      int64
	incompat uint32
	compat   uint32
	dsz      int64
	jinum    uint32
}

func jfLE16(b []byte) int64 { return int64(binary.LittleEndian.Uint16(b)) }
func jfLE32(b []byte) int64 { return int64(binary.LittleEndian.Uint32(b)) }
func jfBE32(b []byte) int64 { return int64(binary.BigEndian.Uint32(b)) }

func newJFExt(t *testing.T, img []byte) *jfExt {
	t.Helper()
	sb := img[1024:2048]
	if jfLE16(sb[0x38:]) != 0xEF53 {
		t.Fatal("not an ext image")
	}
	r := &jfExt{
		img: img, bs: 1024 << jfLE32(sb[0x18:]), first: jfLE32(sb[0x14:]), bpg: jfLE32(sb[0x20:]), ipg: jfLE32(sb[0x28:]), isz: jfLE16(sb[0x58:]),
		compat: uint32(jfLE32(sb[0x5C:])), incompat: uint32(jfLE32(sb[0x60:])), jinum: uint32(jfLE32(sb[0xE0:])), dsz: 32,
	}
	if r.incompat&0x80 != 0 {
		r.dsz = jfLE16(sb[0xFE:])
	}
	return r
}

func (r *jfExt) block(n int64) []byte {
	if n < 0 || (n+1)*r.bs > int64(len(r.img)) {
		return nil
	}
	return r.img[n*r.bs : (n+1)*r.bs]
}

// inode returns the raw slot of inode n.
func (r *jfExt) inode(t *testing.T, n int64) []byte {
	t.Helper()
	g, idx := (n-1)/r.ipg, (n-1)%r.ipg
	d := r.img[(r.first+1)*r.bs+g*r.dsz:]
	it := jfLE32(d[8:])
	if r.dsz >= 64 {
		it |= jfLE32(d[0x28:]) << 32
	}
	off := it*r.bs + idx*r.isz
	return r.img[off : off+r.isz]
}

// blockMap returns logical block -> physical block of the inode slot (extents of any depth, or the 12+1+1+1 block map).
func (r *jfExt) blockMap(t *testing.T, slot []byte) map[int64]int64 {
	t.Helper()
	out := map[int64]int64{}
	ib := slot[0x28:0x64]
	if uint32(jfLE32(slot[0x20:]))&0x80000 != 0 {
		var walk func(node []byte, level int)
		walk = func(node []byte, level int) {
			if level > 5 || jfLE16(node) != 0xF30A {
				t.Fatalf("bad extent node at level %d", level)
			}
			ent, depth := int(jfLE16(node[2:])), int(jfLE16(node[6:]))
			for i := range min(ent, (len(node)-12)/12) {
				e := node[12+12*i : 24+12*i]
				if depth == 0 {
					lb, ln, hi, lo := jfLE32(e), jfLE16(e[4:]), jfLE16(e[6:]), jfLE32(e[8:])
					if ln > 32768 {
						ln -= 32768
					}
					for k := range ln {
						out[lb+k] = hi<<32 | lo + k
					}
					continue
				}
				child := jfLE32(e[4:]) | jfLE16(e[8:])<<32
				walk(r.block(child), level+1)
			}
		}
		walk(ib, 0)
		return out
	}
	per := r.bs / 4
	for i := range int64(12) {
		if p := jfLE32(ib[4*i:]); p != 0 {
			out[i] = p
		}
	}
	var ind func(blk int64, level int, base int64)
	ind = func(blk int64, level int, base int64) {
		if blk == 0 {
			return
		}
		b := r.block(blk)
		span := int64(1)
		for range level - 1 {
			span *= per
		}
		for i := range per {
			p := jfLE32(b[4*i:])
			if p == 0 {
				continue
			}
			if level == 1 {
				out[base+i] = p
			} else {
				ind(p, level-1, base+i*span)
			}
		}
	}
	ind(jfLE32(ib[48:]), 1, 12)
	ind(jfLE32(ib[52:]), 2, 12+per)
	ind(jfLE32(ib[56:]), 3, 12+per+per*per)
	return out
}

// jfJournal is the journal of an image as raw bytes.
type jfJournal struct {
	r                *jfExt
	m                map[int64]int64
	maxlen, first    int64
	sequence         uint32
	start            int64
	version          int
	compat, incompat uint32
	uuid             []byte
	csumV3           bool
	f64              bool
	seed             uint32
	blockSize        int64
}

func (v *jfJournal) read(t *testing.T, j int64) []byte {
	t.Helper()
	fs, ok := v.m[j]
	if !ok {
		t.Fatalf("journal block %d is not mapped", j)
	}
	return v.r.block(fs)
}

func (v *jfJournal) wrap(j int64) int64 {
	if j >= v.maxlen {
		return j - (v.maxlen - v.first)
	}
	return j
}

var jfCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// jfRawCRC is crc32c with the raw register: no initial and no final inversion (the callers pass the seed).
func jfRawCRC(seed uint32, p []byte) uint32 { return ^crc32.Update(^seed, jfCastagnoli, p) }

func newJFJournal(t *testing.T, img []byte) *jfJournal {
	t.Helper()
	r := newJFExt(t, img)
	if r.jinum != 8 {
		t.Fatalf("s_journal_inum %d, want 8", r.jinum)
	}
	m := r.blockMap(t, r.inode(t, 8))
	v := &jfJournal{r: r, m: m}
	sb := v.read(t, 0)
	if uint32(jfBE32(sb)) != 0xC03B3998 {
		t.Fatalf("the journal superblock has no magic")
	}
	switch bt := jfBE32(sb[4:]); bt {
	case 3:
		v.version = 1
	case 4:
		v.version = 2
	default:
		t.Fatalf("journal superblock block type %d", bt)
	}
	v.blockSize, v.maxlen, v.first = jfBE32(sb[0xC:]), jfBE32(sb[0x10:]), jfBE32(sb[0x14:])
	v.sequence, v.start = uint32(jfBE32(sb[0x18:])), jfBE32(sb[0x1C:])
	if v.version == 2 {
		v.compat, v.incompat = uint32(jfBE32(sb[0x24:])), uint32(jfBE32(sb[0x28:]))
		v.uuid = sb[0x30:0x40]
		v.csumV3 = v.incompat&0x10 != 0
		v.f64 = v.incompat&0x2 != 0
		v.seed = jfRawCRC(0xFFFFFFFF, v.uuid)
	}
	return v
}

// walkTxn parses the transaction that starts at journal block start, from the bytes: its header blocks and tags. It returns the tags
// (fs block, data slot, flags, stored checksum, the recomputed one for v3), the commit block (-1 none) and whether every header had
// the transaction's sequence.
type jfRawTag struct {
	fs, slot int64
	flags    int
	stored   uint32
	computed uint32
}

func (v *jfJournal) walkTxn(t *testing.T, start int64, seq uint32) (tags []jfRawTag, revokes []int64, commit int64, ok bool) {
	t.Helper()
	commit = -1
	b := start
	for lap := int64(0); lap < v.maxlen; lap++ {
		blk := v.read(t, b)
		if uint32(jfBE32(blk)) != 0xC03B3998 || uint32(jfBE32(blk[8:])) != seq {
			return tags, revokes, commit, commit >= 0
		}
		switch jfBE32(blk[4:]) {
		case 5: // revoke
			rc := jfBE32(blk[12:])
			limit := v.blockSize
			if v.csumV3 || v.incompat&0x8 != 0 {
				limit -= 4
			}
			rec := int64(4)
			if v.f64 {
				rec = 8
			}
			for off := int64(16); off+rec <= rc && off+rec <= limit; off += rec {
				if rec == 8 {
					revokes = append(revokes, int64(binary.BigEndian.Uint64(blk[off:])))
				} else {
					revokes = append(revokes, jfBE32(blk[off:]))
				}
			}
			b = v.wrap(b + 1)
		case 1: // descriptor
			tsz := int64(8)
			switch {
			case v.csumV3:
				tsz = 16
			case v.f64:
				tsz = 12
			}
			limit := v.blockSize
			if v.csumV3 {
				limit -= 4
			}
			off := int64(12)
			slot := v.wrap(b + 1)
			for off+tsz <= limit {
				tg := blk[off : off+tsz]
				rt := jfRawTag{fs: jfBE32(tg), slot: slot}
				if v.csumV3 {
					rt.flags = int(jfBE32(tg[4:]))
					rt.stored = uint32(jfBE32(tg[12:]))
					rt.fs |= jfBE32(tg[8:]) << 32
					rt.computed = jfRawCRC(jfRawCRC(v.seed, binary.BigEndian.AppendUint32(nil, seq)), v.read(t, slot))
				} else {
					rt.flags = int(binary.BigEndian.Uint16(tg[6:]))
					if v.f64 {
						rt.fs |= jfBE32(tg[8:]) << 32
					}
				}
				tags = append(tags, rt)
				off += tsz
				if rt.flags&2 == 0 {
					off += 16
				}
				slot = v.wrap(slot + 1)
				if rt.flags&8 != 0 {
					break
				}
			}
			b = slot
		case 2: // commit
			return tags, revokes, b, true
		default:
			return tags, revokes, commit, false
		}
	}
	return tags, revokes, commit, false
}

func TestExt4JournalFixturesHoldWhatTheyClaim(t *testing.T) {
	kinds := map[string]int{}
	for _, name := range journalFixtureImages {
		img, o := loadJournalFixture(t, name)
		if o.Generator.Kernel != "5.10.0-32-amd64" || len(o.Generator.Commands) == 0 || o.Generator.Gates["J9"] == "" {
			t.Errorf("%s: generator section %+v: want the 5.10 kernel, its commands and the J9 gate", name, o.Generator)
		}
		if o.DeletedEntryCount != len(o.Deleted) {
			t.Errorf("%s: deleted_entry_count %d, %d records", name, o.DeletedEntryCount, len(o.Deleted))
		}
		crash := name == "ext4-journal-crash"
		if !crash && o.Generator.FsckExit != 0 {
			t.Errorf("%s: e2fsck -fn exit %d: every clean image fscks clean (the crash image's exit is recorded, never asserted)", name, o.Generator.FsckExit)
		}
		if o.Generator.ReplayOracle != crash || o.Generator.LogdumpOld == crash {
			t.Errorf("%s: replay_oracle %v, logdump_old %v", name, o.Generator.ReplayOracle, o.Generator.LogdumpOld)
		}

		// the journal, from the bytes of the image itself
		v := newJFJournal(t, img)
		j := o.Journal
		if j.Inode != 8 || j.Version != v.version || j.BlockSize != v.blockSize || j.Blocks != v.maxlen || j.First != v.first ||
			j.Sequence != v.sequence || j.Start != v.start || j.FeatureCompat != v.compat || j.FeatureIncompat != v.incompat ||
			j.NeedsRecovery != (v.r.incompat&0x4 != 0) {
			t.Errorf("%s: the oracle's journal %+v differs from the journal superblock (version %d, bs %d, %d blocks, first %d, seq %d, start %d, features %#x/%#x)",
				name, j, v.version, v.blockSize, v.maxlen, v.first, v.sequence, v.start, v.compat, v.incompat)
		}
		if j.First != 1 || j.Blocks != 1024 {
			t.Errorf("%s: journal of %d blocks starting at %d, want the 1024-block (-J size=1) journal with first 1", name, j.Blocks, j.First)
		}
		if crash {
			if !j.NeedsRecovery || j.Start == 0 {
				t.Errorf("%s: needs_recovery %v, start %d: a crash image has both", name, j.NeedsRecovery, j.Start)
			}
		} else if j.NeedsRecovery || j.Start != 0 {
			t.Errorf("%s: needs_recovery %v, start %d: a clean image has neither", name, j.NeedsRecovery, j.Start)
		}
		if name == "ext3-journal-clean" {
			if j.TagSize != 8 || v.csumV3 || v.f64 {
				t.Errorf("%s: tag size %d, features %v: an ext3 journal has 8-byte tags and no checksum or 64bit", name, j.TagSize, j.Features)
			}
		} else if j.TagSize != 16 || !v.csumV3 || !slices.Contains(j.Features, "csum_v3") {
			t.Errorf("%s: tag size %d, features %v: an ext4 journal has checksum v3 and 16-byte tags", name, j.TagSize, j.Features)
		}

		// every transaction: its header blocks, tags, revokes and commit block as the oracle lists them, parsed from the bytes
		live, stale, logdumped, shortened := 0, 0, 0, 0
		for _, tx := range o.Txns {
			switch tx.Region {
			case "live":
				live++
			case "stale":
				stale++
			default:
				t.Errorf("%s: transaction %d region %q", name, tx.Seq, tx.Region)
			}
			tags, revokes, commit, ok := v.walkTxn(t, tx.StartJBlk, tx.Seq)
			if !ok && tx.Committed {
				t.Errorf("%s: seq %d at journal block %d: the bytes hold no commit block", name, tx.Seq, tx.StartJBlk)
				continue
			}
			if tx.Committed && commit != tx.CommitJ {
				t.Errorf("%s: seq %d: commit block %d, the bytes say %d", name, tx.Seq, tx.CommitJ, commit)
			}
			if tx.Committed && v.csumV3 {
				cb := slices.Clone(v.read(t, commit))
				stored := uint32(jfBE32(cb[0x10:]))
				clear(cb[0x10:0x14])
				if (jfRawCRC(v.seed, cb) == stored) != (tx.Checksum == "ok") {
					t.Errorf("%s: seq %d: checksum %q, the recomputed commit checksum disagrees", name, tx.Seq, tx.Checksum)
				}
			}
			if tx.Committed && !v.csumV3 && tx.Checksum != "not-recorded" {
				t.Errorf("%s: seq %d: a journal without checksums reports %q", name, tx.Seq, tx.Checksum)
			}
			cmpTags := len(tx.Tags)
			if len(tags) != len(tx.Tags) {
				// a stale fragment can end where a later boot overwrote a data slot; the oracle then lists fewer tags than
				// the bytes still parse. Counted and bounded (M2 of the Task 13 review), and the common prefix is still compared.
				if tx.Region == "live" || len(tags) < len(tx.Tags) {
					t.Errorf("%s: seq %d: %d tags in the oracle, %d in the bytes", name, tx.Seq, len(tx.Tags), len(tags))
					continue
				}
				shortened++
				cmpTags = len(tx.Tags)
			}
			if !slices.Equal(revokes, tx.Revoked) && (tx.Region == "live" || !tx.Committed) && cmpTags == len(tags) {
				t.Errorf("%s: seq %d: revoked %v, the bytes say %v", name, tx.Seq, tx.Revoked, revokes)
			}
			for i, tg := range tx.Tags[:cmpTags] {
				rt := tags[i]
				if rt.fs != tg.FSBlock || rt.slot != tg.JBlk || rt.flags != tg.Flags {
					t.Errorf("%s: seq %d tag %d: oracle (fs %d, slot %d, flags %#x), bytes (fs %d, slot %d, flags %#x)", name, tx.Seq, i,
						tg.FSBlock, tg.JBlk, tg.Flags, rt.fs, rt.slot, rt.flags)
				}
				if v.csumV3 {
					if (tg.Copy == "verified") != (rt.computed == rt.stored) && tg.Copy != "missing" {
						t.Errorf("%s: seq %d tag %d: copy %q, but the recomputed tag checksum %#x against stored %#x", name, tx.Seq, i, tg.Copy, rt.computed, rt.stored)
					}
				} else if tg.Copy != "unverified" && tg.Copy != "missing" {
					t.Errorf("%s: seq %d tag %d: copy %q on a journal without checksums", name, tx.Seq, i, tg.Copy)
				}
				if tg.Escaped != (tg.Flags&1 != 0) {
					t.Errorf("%s: seq %d tag %d: escaped %v, flags %#x", name, tx.Seq, i, tg.Escaped, tg.Flags)
				}
			}
			if tx.LogDump {
				logdumped++
				if len(tx.Tags) == 0 && len(tx.Revoked) == 0 && !tx.Committed {
					t.Errorf("%s: seq %d is logdump-confirmed but holds no tags, revokes or commit", name, tx.Seq)
				}
			}
		}
		if live == 0 && crash || live != 0 && !crash || stale == 0 {
			t.Errorf("%s: %d live and %d stale transactions", name, live, stale)
		}
		if logdumped == 0 {
			t.Errorf("%s: no transaction confirmed by logdump", name)
		}
		// none of the committed images has such a fragment (measured: 0); a regeneration that makes one must say so here
		if shortened != 0 {
			t.Errorf("%s: %d stale fragments are shorter in the oracle than in the bytes (measured 0: re-rule the bound)", name, shortened)
		}

		// the recoveries the oracle derives: the captured bytes hold what equal_source says, in every mode
		bs := o.BlockSize
		seen := map[string]bool{}
		for _, d := range o.Deleted {
			seen[d.Family] = true
			for _, mode := range []string{"none", "committed", "stale"} {
				e, okm := d.Expect[mode]
				if !okm {
					t.Errorf("%s: %s has no %q expectation", name, d.Path, mode)
					continue
				}
				if e.Outcome != "recover" {
					if e.Outcome != "skip" || e.Skip == "" {
						t.Errorf("%s: %s %s: outcome %q skip %q", name, d.Path, mode, e.Outcome, e.Skip)
					}
					continue
				}
				art := recoverBlocks(t, img, bs, e.CapturedBlocks, e.Size)
				if int64(len(art)) != e.Size {
					t.Errorf("%s: %s %s: captured blocks hold %d bytes, the record says %d", name, d.Path, mode, len(art), e.Size)
				}
				if !strings.HasPrefix(d.Family, "aux") && d.Family != "unlisted" {
					kinds[e.EqualSource]++
				}
				switch e.EqualSource {
				case "full":
					if sha256Hex(art) != d.SourceSHA256 {
						t.Errorf("%s: %s %s: equal_source full, but the artifact bytes do not hash to the source", name, d.Path, mode)
					}
				case "prefix-of-artifact":
					if int64(len(art)) <= d.SourceSize || sha256Hex(art[:d.SourceSize]) != d.SourceSHA256 {
						t.Errorf("%s: %s %s: equal_source prefix-of-artifact, but the source is not the leading %d bytes of the %d artifact bytes", name, d.Path, mode, d.SourceSize, len(art))
					}
				case "artifact-is-prefix":
					if int64(len(art)) >= d.SourceSize {
						t.Errorf("%s: %s %s: equal_source artifact-is-prefix, but the artifact (%d bytes) is not shorter than the source (%d)", name, d.Path, mode, len(art), d.SourceSize)
					}
				case "no":
					if int64(len(art)) >= d.SourceSize && sha256Hex(art[:d.SourceSize]) == d.SourceSHA256 {
						t.Errorf("%s: %s %s: equal_source no, but the artifact starts with the source", name, d.Path, mode)
					}
				default:
					t.Errorf("%s: %s %s: unknown equal_source %q", name, d.Path, mode, e.EqualSource)
				}
				if e.Method == "ext4-journal-inode" && (e.Seq == 0 || (e.Region != "live" && e.Region != "stale")) {
					t.Errorf("%s: %s %s: a journal recovery names no sequence or region (%d, %q)", name, d.Path, mode, e.Seq, e.Region)
				}
			}
			switch d.Family {
			case "jdeep":
				if len(d.TreeBlocks) < 1 || len(d.TrueBlocks) < 20 || !d.KernelState.LeafIntact {
					t.Errorf("%s: %s: %d tree blocks, %d fragments, leaf intact %v: want a depth-1 tree, at least 20 fragments and an intact home leaf", name, d.Path, len(d.TreeBlocks), len(d.TrueBlocks), d.KernelState.LeafIntact)
				}
			case "jsmall", "jreuse", "crash-live-copy", "bm-small", "bm-indirect":
				if len(d.Expect["none"].CapturedBlocks) != 0 {
					t.Errorf("%s: %s: the kernel cleared the map, so 3C recovers nothing (family %s)", name, d.Path, d.Family)
				}
			}
		}
		for _, fam := range requiredJournalFamilies[name] {
			f, okf := o.Families[fam]
			if !okf || f.Count == 0 {
				t.Errorf("%s: family %q is missing: the self-check cannot go vacuous", name, fam)
			}
		}
		for _, jo := range o.JournalOnly {
			if jo.ReplayedSHA256 == "" || len(jo.InodeTableCopies) == 0 {
				t.Errorf("%s: %s: no replayed content or no inode-table copy", name, jo.Path)
			}
			switch jo.Family {
			case "crash-created":
				if jo.AsFoundPresent {
					t.Errorf("%s: %s is present in the as-found view, but it only exists in the journal", name, jo.Path)
				}
			case "crash-update":
				if !jo.AsFoundPresent || jo.AsFoundSHA256 == jo.ReplayedSHA256 || jo.AsFoundSize >= jo.ReplayedSize {
					t.Errorf("%s: %s: the as-found file must be the old, shorter content", name, jo.Path)
				}
			default:
				t.Errorf("%s: unknown journal-only family %q", name, jo.Family)
			}
		}
		if crash {
			if o.Replay == nil || len(o.Replay.Files) == 0 {
				t.Errorf("%s: no replay listing", name)
			} else if !slices.ContainsFunc(o.Replay.Dmesg, func(s string) bool { return strings.Contains(s, "recovery complete") }) {
				t.Errorf("%s: the kernel did not log a completed recovery: %v", name, o.Replay.Dmesg)
			}
		}
	}
	for _, k := range []string{"full", "prefix-of-artifact", "artifact-is-prefix", "no"} {
		if kinds[k] == 0 {
			t.Errorf("equal_source %q occurs in no recovered record of the three images", k)
		}
	}
}

// The live files of the as-found view open, with the live reader, with the bytes their source had (the crash image's `live` file is
// the old one: its appended content is only in the journal).
func TestExt4JournalFixturesLiveFilesMatchSource(t *testing.T) {
	total := 0
	for _, name := range journalFixtureImages {
		img, o := loadJournalFixture(t, name)
		if len(o.Live) == 0 {
			continue
		}
		f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, lf := range o.Live {
			e, err := f.Lookup(lf.Path)
			if err != nil {
				t.Errorf("%s: %s: %v", name, lf.Path, err)
				continue
			}
			fl, err := f.Open(e)
			if err != nil {
				t.Errorf("%s: %s: %v", name, lf.Path, err)
				continue
			}
			got, err := io.ReadAll(io.NewSectionReader(fl, 0, fl.Size()))
			if err != nil {
				t.Errorf("%s: %s: %v", name, lf.Path, err)
				continue
			}
			if int64(len(got)) != lf.Size || sha256Hex(got) != lf.SHA256 {
				t.Errorf("%s: %s: %d bytes (sha256 %s), the oracle says %d bytes (%s)", name, lf.Path, len(got), sha256Hex(got), lf.Size, lf.SHA256)
			}
			if lf.Size == lf.SourceSize && lf.SHA256 != lf.SourceSHA256 {
				t.Errorf("%s: %s: the oracle's live content is not its source", name, lf.Path)
			}
			total++
		}
	}
	if total < 3 {
		t.Errorf("only %d live files were checked", total)
	}
}
