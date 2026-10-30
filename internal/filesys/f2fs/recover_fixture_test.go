package f2fs_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// The recovery fixtures (testdata/recover/) are F2FS volumes written and deleted by the KERNEL f2fs driver inside
// the QEMU guest (tools/fixtures/f2fs_recover.sh). Their oracle (testdata/recover/*.expect.json) comes from
// tools/fixtures/f2fs_recover_oracle.py ("snap" after the write stages, "final" after the delete stage), an
// independent decoder of the finished images cross-checked against fsck.f2fs and dump.f2fs: never Minutiae. They
// live in a subdirectory so the globs and the old-schema oracle of fixture_test.go never see them. The names of
// the types and the loader do not collide with that file's oracle, loadOracle and fixturePaths.

type recoverOracle struct {
	Generator struct {
		ImageSHA256 string         `json:"image_sha256"`
		Variant     string         `json:"variant"`
		Packages    []any          `json:"packages"`
		Commands    []string       `json:"commands"`
		Stages      []string       `json:"stages"`
		FsckExit    map[string]int `json:"fsck_exit"`
	} `json:"generator"`
	CheckpointVersion uint64         `json:"checkpoint_version"`
	BlockSize         int            `json:"block_size"`
	FreeBlocks        [][2]int64     `json:"free_blocks"`
	Live              []recLive      `json:"live"`
	DeletedEntryCount int            `json:"deleted_entry_count"`
	Deleted           []recDeleted   `json:"deleted"`
	Families          map[string]int `json:"families"`
}

// recLive is a file that stays live. Blocks are inclusive absolute block numbers in file order; Holes are
// inclusive LOGICAL block ranges (file offset / 4096) that read as holes.
type recLive struct {
	Path     string     `json:"path"`
	Size     int64      `json:"size"`
	SHA256   string     `json:"sha256"`
	Blocks   [][2]int64 `json:"blocks"`
	Holes    [][2]int64 `json:"holes"`
	Indirect bool       `json:"indirect"`
	Inline   bool       `json:"inline"`
}

type recStale struct {
	Addr     uint32 `json:"addr"`
	CPVerLow uint32 `json:"cp_ver_low"`
	Intact   bool   `json:"intact"`
}

type recExpect struct {
	Outcome     string     `json:"outcome"`
	Skip        string     `json:"skip"`
	Method      string     `json:"method"`
	Captured    [][2]int64 `json:"captured"`
	Cut         bool       `json:"cut"`
	Prefix      bool       `json:"prefix"`
	Stop        string     `json:"stop"`
	EqualSource bool       `json:"equal_source"`
	Uniform     bool       `json:"uniform"`
	Confidence  int        `json:"confidence"`
}

type recDeleted struct {
	Path       string     `json:"path"`
	Family     string     `json:"family"`
	Listed     bool       `json:"listed"`
	DirNID     uint32     `json:"dir_nid"`
	Ino        uint32     `json:"ino"`
	Size       int64      `json:"size"`
	SHA256     string     `json:"sha256"`
	TrueBlocks [][2]int64 `json:"true_blocks"`
	Stale      recStale   `json:"stale"`
	Expect     recExpect  `json:"expect"`
}

const recoverFixtureDir = "testdata/recover"

// loadRecoverFixture returns the decompressed image of testdata/recover/<name>.img.gz and its oracle, after
// checking the image sha256 against generator.image_sha256 (a stale oracle fails here, first).
func loadRecoverFixture(t testing.TB, name string) ([]byte, recoverOracle) {
	t.Helper()
	img := gunzipFixture(t, filepath.Join(recoverFixtureDir, name+".img.gz"))
	raw, err := os.ReadFile(filepath.Join(recoverFixtureDir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o recoverOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match generator.image_sha256: regenerate image and oracle together", name)
	}
	return img, o
}

// The two images and the families each must hold (3D-R10). The discard image runs the same write and delete
// phases without the families that need more boots (renamed, nidreuse) or a fill (reuse).
var (
	recoverImages  = []string{"f2fs-recover-nodiscard", "f2fs-recover-discard"}
	commonFamilies = []string{"contig", "twofrag", "direct", "indirect", "holes", "inline", "empty", "gone"}
	g6Families     = []string{"renamed", "nidreuse", "reuse"}
)

func requiredFamilies(image string) []string {
	if strings.HasSuffix(image, "-nodiscard") {
		return slices.Concat(commonFamilies, g6Families)
	}
	return commonFamilies
}

// imageBlocks returns the bytes of the inclusive absolute block ranges, in order, clipped to size.
func imageBlocks(t testing.TB, img []byte, ranges [][2]int64, size int64) []byte {
	t.Helper()
	var b []byte
	for _, r := range ranges {
		if r[0] < 0 || r[1] < r[0] || (r[1]+1)*4096 > int64(len(img)) {
			t.Fatalf("block range %v is outside the %d-byte image", r, len(img))
		}
		b = append(b, img[r[0]*4096:(r[1]+1)*4096]...)
	}
	if int64(len(b)) > size {
		b = b[:size]
	}
	return b
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func rangeBlocks(rs [][2]int64) int64 {
	var n int64
	for _, r := range rs {
		n += r[1] - r[0] + 1
	}
	return n
}

func inRanges(rs [][2]int64, blk int64) bool {
	return slices.ContainsFunc(rs, func(r [2]int64) bool { return blk >= r[0] && blk <= r[1] })
}

// footerOf reads the node footer (nid, ino, flag, cp_ver) of the block at addr.
func footerOf(img []byte, addr uint32) (nid, ino, flag uint32, cpVer uint64) {
	b := img[int64(addr)*4096 : int64(addr)*4096+4096]
	return binary.LittleEndian.Uint32(b[4072:]), binary.LittleEndian.Uint32(b[4076:]),
		binary.LittleEndian.Uint32(b[4080:]), binary.LittleEndian.Uint64(b[4084:])
}

// iNID returns i_nid[j] of the inode block at addr (offset 4052 of the block).
func iNID(img []byte, addr uint32, j int) uint32 {
	return binary.LittleEndian.Uint32(img[int64(addr)*4096+4052+int64(4*j):])
}

type deletedKey struct {
	dir  uint32
	ino  uint32
	name string
}

// listedDeleted returns every deleted entry the reader lists under the live directories, by (directory inode,
// inode attr, name), with the entry. dir ID "nid:<n>" gives the directory inode.
func listedDeleted(t testing.TB, fsys *f2fs.FS) map[deletedKey]filesys.Entry {
	t.Helper()
	out := map[deletedKey]filesys.Entry{}
	err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if !e.Deleted {
			return nil
		}
		dir, ok := strings.CutPrefix(e.ID, "dentry:")
		if !ok {
			t.Errorf("%s: deleted entry with ID %q", p, e.ID)
			return nil
		}
		d, err := strconv.ParseUint(strings.SplitN(dir, ":", 2)[0], 10, 32)
		if err != nil {
			t.Errorf("%s: deleted entry ID %q: %v", p, e.ID, err)
			return nil
		}
		ino := uint64(0)
		for _, kv := range e.Attrs {
			if kv.Key == "inode" {
				ino, _ = strconv.ParseUint(kv.Value, 10, 32)
			}
		}
		k := deletedKey{uint32(d), uint32(ino), path.Base(p)}
		if _, dup := out[k]; dup {
			t.Errorf("%s: deleted entry listed twice for %+v", p, k)
		}
		out[k] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestF2FSRecoverFixturesHoldWhatTheyClaim(t *testing.T) {
	discardEvidence := 0
	for _, name := range recoverImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadRecoverFixture(t, name) // the pin is checked first
			if o.BlockSize != 4096 {
				t.Errorf("block_size %d", o.BlockSize)
			}
			if want := strings.TrimPrefix(name, "f2fs-recover-"); o.Generator.Variant != want {
				t.Errorf("generator.variant %q, want %q", o.Generator.Variant, want)
			}
			if len(o.Generator.Stages) < 2 || o.Generator.Stages[0] != "w" || o.Generator.Stages[len(o.Generator.Stages)-1] != "d" {
				t.Errorf("generator.stages %v: want w first and d last", o.Generator.Stages)
			}
			for _, s := range o.Generator.Stages {
				if code, ok := o.Generator.FsckExit[s]; !ok || code != 0 {
					t.Errorf("fsck.f2fs -f --dry-run after stage %q exited %d (recorded: %v)", s, code, ok)
				}
			}
			if len(o.Generator.Commands) == 0 {
				t.Error("generator.commands is empty")
			}

			fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			if got := fsys.Checkpoint().Version; got != o.CheckpointVersion {
				t.Errorf("checkpoint version %d, oracle %d", got, o.CheckpointVersion)
			}

			// free_blocks is exactly what Unallocated reports
			runs, err := fsys.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			var want []filesys.Run
			for _, r := range o.FreeBlocks {
				want = append(want, filesys.Run{Offset: r[0] * 4096, Length: (r[1] - r[0] + 1) * 4096})
			}
			if len(want) == 0 || !slices.Equal(runs, want) {
				t.Errorf("Unallocated differs from the oracle's free blocks:\n got  %v\n want %v", runs, want)
			}

			// every required family is present: the self-check cannot go vacuous
			seen := map[string]int{}
			for _, d := range o.Deleted {
				seen[d.Family]++
			}
			for _, fam := range requiredFamilies(name) {
				if seen[fam] == 0 {
					t.Errorf("family %q has no deleted entry: the generator should have exited 1", fam)
				}
				if o.Families[fam] == 0 {
					t.Errorf("families[%q] is %d", fam, o.Families[fam])
				}
			}

			// every listed deleted entry is found by ReadDir of its directory with the oracle's directory nid and
			// inode; the reader lists nothing the oracle does not explain
			got := listedDeleted(t, fsys)
			explained := map[deletedKey]bool{}
			nListed := 0
			for _, d := range o.Deleted {
				if !d.Listed {
					if _, ok := got[deletedKey{d.DirNID, d.Ino, path.Base(d.Path)}]; ok {
						t.Errorf("%s: the oracle says it is not listed (its directory is gone), the reader lists it", d.Path)
					}
					continue
				}
				nListed++
				k := deletedKey{d.DirNID, d.Ino, path.Base(d.Path)}
				e, ok := got[k]
				if !ok {
					t.Errorf("%s: dir nid %d inode %d: not listed by the reader", d.Path, d.DirNID, d.Ino)
					continue
				}
				explained[k] = true
				if !strings.HasPrefix(e.ID, fmt.Sprintf("dentry:%d:", d.DirNID)) {
					t.Errorf("%s: ID %q does not name directory %d", d.Path, e.ID, d.DirNID)
				}
				parent, err := fsys.Lookup(path.Dir(d.Path))
				if err != nil || parent.ID != fmt.Sprintf("nid:%d", d.DirNID) {
					t.Errorf("%s: parent %q resolves to %q, %v; oracle dir nid %d", d.Path, path.Dir(d.Path), parent.ID, err, d.DirNID)
				}
			}
			if nListed != o.DeletedEntryCount {
				t.Errorf("deleted_entry_count %d, listed entries in deleted[] %d", o.DeletedEntryCount, nListed)
			}
			unexplained := 0
			for k := range got {
				if !explained[k] {
					unexplained++
					t.Errorf("the reader lists a deleted entry the oracle does not explain: %+v", k)
				}
			}
			t.Logf("%s: %d deleted entries listed, %d unexplained", name, len(got), unexplained)
			if len(got) != o.DeletedEntryCount {
				t.Errorf("filesys.Walk lists %d deleted entries, oracle deleted_entry_count %d", len(got), o.DeletedEntryCount)
			}

			// facts about the bytes of the image that the oracle claims, checked with no reader code
			freeBlk := func(b int64) bool { return inRanges(o.FreeBlocks, b) }
			for _, d := range o.Deleted {
				e := d.Expect
				switch e.Outcome {
				case "recover":
					if e.Method != f2fs.MethodNodeScan {
						t.Errorf("%s: method %q", d.Path, e.Method)
					}
				case "skip":
					if e.Skip == "" {
						t.Errorf("%s: skip without a reason", d.Path)
					}
				default:
					t.Errorf("%s: outcome %q", d.Path, e.Outcome)
				}
				for _, r := range e.Captured {
					for _, b := range []int64{r[0], r[1]} {
						if !freeBlk(b) {
							t.Errorf("%s: captured block %d is not free in the oracle's free_blocks", d.Path, b)
						}
					}
				}
				if e.Outcome == "recover" {
					full := rangeBlocks(e.Captured) == rangeBlocks(d.TrueBlocks)
					switch {
					case e.Prefix != (e.Stop != ""):
						t.Errorf("%s: prefix=%v with stop %q", d.Path, e.Prefix, e.Stop)
					case !e.Cut && !e.Prefix && !full:
						t.Errorf("%s: neither cut nor prefix, but %d of %d blocks are captured", d.Path, rangeBlocks(e.Captured), rangeBlocks(d.TrueBlocks))
					case e.Cut && rangeBlocks(e.Captured) >= rangeBlocks(d.TrueBlocks):
						t.Errorf("%s: cut, but every block is captured", d.Path)
					}
					body := imageBlocks(t, img, e.Captured, d.Size)
					if e.EqualSource != (hexSum(body) == d.SHA256 && !e.Cut && !e.Prefix) {
						t.Errorf("%s: equal_source=%v but the captured bytes hash to %s (source %s)", d.Path, e.EqualSource, hexSum(body), d.SHA256)
					}
					if e.Uniform {
						if len(body) == 0 || bytes.Count(body, body[:1]) != len(body) {
							t.Errorf("%s: uniform=true but the captured bytes are not one value", d.Path)
						}
					}
					if (e.Cut || e.Prefix) && e.Confidence > 20 {
						t.Errorf("%s: cut or prefix with confidence %d", d.Path, e.Confidence)
					}
					if !e.Cut && !e.Prefix && e.Confidence != 75 && e.Confidence != 30 {
						t.Errorf("%s: complete map with confidence %d", d.Path, e.Confidence)
					}
				}
				if d.Stale.Intact {
					nid, ino, flag, ver := footerOf(img, d.Stale.Addr)
					if nid != d.Ino || ino != d.Ino || flag>>3 != 0 || uint32(ver) != d.Stale.CPVerLow {
						t.Errorf("%s: stale.intact but the block at %d has footer nid %d ino %d ofs %d cp_ver %#x", d.Path, d.Stale.Addr, nid, ino, flag>>3, ver)
					}
					if uint64(d.Stale.CPVerLow) >= o.CheckpointVersion&0xFFFFFFFF {
						t.Errorf("%s: stale copy version %d is not below the checkpoint %d", d.Path, d.Stale.CPVerLow, o.CheckpointVersion)
					}
				}
			}

			// the shape of each family, from the bytes of the image
			byFam := map[string][]recDeleted{}
			for _, d := range o.Deleted {
				byFam[d.Family] = append(byFam[d.Family], d)
			}
			noDiscard := strings.HasSuffix(name, "-nodiscard")
			for _, d := range byFam["contig"] {
				if len(d.TrueBlocks) != 1 {
					t.Errorf("contig: %d true extents, want 1", len(d.TrueBlocks))
				}
				if noDiscard && (!d.Stale.Intact || d.Expect.Outcome != "recover" || !d.Expect.EqualSource || d.Expect.Confidence != 75) {
					t.Errorf("contig: %+v %+v, want an intact stale inode and an equal full map at 75", d.Stale, d.Expect)
				}
			}
			for _, d := range byFam["twofrag"] {
				if len(d.TrueBlocks) < 2 {
					t.Errorf("twofrag: %d true extents, want at least 2", len(d.TrueBlocks))
				}
			}
			for _, d := range byFam["direct"] {
				if d.Stale.Intact && (iNID(img, d.Stale.Addr, 0) == 0 || iNID(img, d.Stale.Addr, 2) != 0) {
					t.Errorf("direct: i_nid = %d %d %d, want i_nid[0] only", iNID(img, d.Stale.Addr, 0), iNID(img, d.Stale.Addr, 1), iNID(img, d.Stale.Addr, 2))
				}
			}
			for _, d := range byFam["indirect"] {
				if d.Stale.Intact && iNID(img, d.Stale.Addr, 2) == 0 {
					t.Errorf("indirect: i_nid[2] is 0, want an indirect node")
				}
			}
			for _, d := range byFam["holes"] {
				if d.Expect.Outcome == "recover" && (d.Expect.Stop != "hole@1" || !d.Expect.Prefix || d.Expect.Confidence != 20) {
					t.Errorf("holes: %+v, want a prefix to hole@1 at 20", d.Expect)
				}
			}
			for _, d := range byFam["inline"] {
				if d.Expect.Outcome != "skip" || (noDiscard && d.Expect.Skip != "unsupported") {
					t.Errorf("inline: %+v, want skip unsupported", d.Expect)
				}
			}
			for _, d := range byFam["empty"] {
				if d.Size != 0 || d.Expect.Outcome != "skip" || (noDiscard && d.Expect.Skip != "unsupported") {
					t.Errorf("empty: size %d %+v: the kernel stores a 0-byte file with the inline-data flag, so the stale inode is skipped unsupported (the empty skip is builder-only)", d.Size, d.Expect)
				}
			}
			dirSkipped, innerUnlisted := 0, 0
			for _, d := range byFam["gone"] {
				if d.Listed && d.Expect.Skip == "not-a-file" {
					dirSkipped++
				}
				if !d.Listed {
					innerUnlisted++
				}
			}
			if dirSkipped != 1 || innerUnlisted != 1 {
				t.Errorf("gone: %d directory skipped not-a-file and %d unlisted entries, want 1 and 1", dirSkipped, innerUnlisted)
			}
			if !noDiscard {
				// G5 = gone: with discard the kernel zeroed the stale inode blocks too, so no file is found
				for _, fam := range []string{"contig", "twofrag", "direct", "indirect", "holes", "inline", "empty"} {
					for _, d := range byFam[fam] {
						if d.Stale.Intact || d.Expect.Outcome != "skip" || d.Expect.Skip != "not-found" {
							t.Errorf("%s: discard image: stale intact=%v, %+v; want the zeroed inode gone (skip not-found)", d.Path, d.Stale.Intact, d.Expect)
						}
						discardEvidence++
					}
				}
			}
			if noDiscard {
				renamed := 0
				for _, d := range byFam["renamed"] {
					if d.Expect.Outcome == "recover" && d.Expect.Confidence == 30 {
						renamed++
					}
				}
				if renamed != 2 || len(byFam["renamed"]) != 2 {
					t.Errorf("renamed: %d of %d entries recover at 30 (the old and the new name of one inode share their blocks), want 2 of 2", renamed, len(byFam["renamed"]))
				}
				for _, d := range byFam["nidreuse"] {
					if d.Expect.Outcome != "skip" || d.Expect.Skip != "reused" {
						t.Errorf("nidreuse: %+v, want skip reused", d.Expect)
					}
				}
				for _, d := range byFam["reuse"] {
					taken := 0
					for _, r := range d.TrueBlocks {
						for b := r[0]; b <= r[1]; b++ {
							if !freeBlk(b) {
								taken++
							}
						}
					}
					if taken == 0 || !d.Stale.Intact {
						t.Errorf("reuse: %d of the file's blocks are in use again and the stale inode intact=%v: want a block reused while the inode is intact", taken, d.Stale.Intact)
					}
					noFree := d.Expect.Outcome == "skip" && d.Expect.Skip == "no-free-bytes"
					cutPrefix := d.Expect.Outcome == "recover" && d.Expect.Cut && d.Expect.Confidence == 20
					if !noFree && !cutPrefix {
						t.Errorf("reuse: %+v, want skip no-free-bytes or a cut prefix at 20", d.Expect)
					}
				}
			}
		})
	}
	if discardEvidence == 0 {
		t.Error("the discard image shows nothing about discard")
	}
}

func TestF2FSRecoverFixtureLiveFilesMatchOracle(t *testing.T) {
	for _, name := range recoverImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadRecoverFixture(t, name)
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
			need := map[string]bool{"/live/indirect.bin": false, "/live/holes.bin": false, "/live/inline.txt": false}
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
					t.Errorf("%s is not among the live files: the holes, indirect-node and inline checks would be vacuous", p)
				}
			}
			if w := fsys.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings after reading every live file of a kernel-written image: %v", w)
			}
		})
	}
}

func checkLiveFile(t *testing.T, fsys *f2fs.FS, e filesys.Entry, l recLive) {
	t.Helper()
	fl, err := fsys.Open(e)
	if err != nil {
		t.Errorf("%s: Open: %v", l.Path, err)
		return
	}
	if fl.Size() != l.Size || e.Size != l.Size {
		t.Errorf("%s: size %d (entry %d), want %d", l.Path, fl.Size(), e.Size, l.Size)
	}
	h, buf := sha256.New(), make([]byte, 64<<10)
	for off := int64(0); off < fl.Size(); {
		n, rerr := fl.ReadAt(buf[:min(int64(len(buf)), fl.Size()-off)], off)
		h.Write(buf[:n])
		off += int64(n)
		if rerr != nil && (rerr != io.EOF || off != fl.Size()) {
			t.Errorf("%s: read at %d: %v", l.Path, off, rerr)
			break
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != l.SHA256 {
		t.Errorf("%s: content sha256 %s, want %s", l.Path, got, l.SHA256)
	}
	ino := uint32(0)
	for _, kv := range e.Attrs {
		if kv.Key == "inode" {
			v, _ := strconv.ParseUint(kv.Value, 10, 32)
			ino = uint32(v)
		}
	}
	if ino == 0 {
		if rest, ok := strings.CutPrefix(e.ID, "nid:"); ok {
			v, _ := strconv.ParseUint(rest, 10, 32)
			ino = uint32(v)
		}
	}
	v, _, err := fsys.Inode(ino)
	if err != nil {
		t.Errorf("%s: inode %d: %v", l.Path, ino, err)
		return
	}
	if inline := v.Inline&0x02 != 0; inline != l.Inline {
		t.Errorf("%s: inline data flag %v, oracle %v", l.Path, inline, l.Inline)
	}
	if indirect := v.NIDs[2] != 0 || v.NIDs[3] != 0 || v.NIDs[4] != 0; indirect != l.Indirect {
		t.Errorf("%s: indirect nodes %v (i_nid %v), oracle %v", l.Path, indirect, v.NIDs, l.Indirect)
	}
	runs := coalesce(fl.Runs())
	if l.Inline {
		if len(fl.Runs()) != 0 {
			t.Errorf("%s: inline data with runs %v", l.Path, fl.Runs())
		}
		return
	}
	exp := extentRuns(l.Blocks, l.Size)
	var data []filesys.Run
	var holeRanges [][2]int64
	var pos int64 // file offset
	for _, r := range runs {
		if r.Offset >= 0 {
			data = append(data, r)
		} else {
			holeRanges = append(holeRanges, [2]int64{pos / 4096, (pos+r.Length-1)/4096 - 0})
		}
		pos += r.Length
	}
	if !slices.Equal(data, exp) {
		t.Errorf("%s: data runs differ from the oracle's extents:\n got  %v\n want %v", l.Path, data, exp)
	}
	if len(l.Holes) == 0 && len(holeRanges) != 0 {
		t.Errorf("%s: the reader reports holes %v, the oracle none", l.Path, holeRanges)
	}
	if len(l.Holes) != 0 {
		// the oracle's holes are whole logical blocks; the reader's last hole may be clipped to the file size
		got := slices.Clone(holeRanges)
		sort.Slice(got, func(i, j int) bool { return got[i][0] < got[j][0] })
		if !slices.Equal(got, l.Holes) {
			t.Errorf("%s: holes (logical blocks) %v, oracle %v", l.Path, got, l.Holes)
		}
	}
}
