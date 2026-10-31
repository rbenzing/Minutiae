package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The recovery fixtures (testdata/recover/) are ext4 and ext3 images written and deleted by the KERNEL ext4 driver in
// the QEMU guest (tools/fixtures/ext4_recover.sh). Their oracle (testdata/recover/*.expect.json) comes from
// tools/fixtures/ext4_recover_oracle.py, an independent reader of the finished image, the stage images before it, the
// source files the guest was fed, debugfs and dumpe2fs: never Minutiae. They live in a subdirectory so the globs and
// the oracle schema of fixture_test.go never see them. The positive images are written by the 5.10 guest kernel, which
// keeps a deleted directory record (ruling 3C-C1); ext4-recover-wiped is written by 6.1, which wipes it.

// recoverRuns is a list of [first block, count] runs.
type recoverRuns [][2]int64

type recoverCandidate struct {
	Method      string      `json:"method"`
	Size        int64       `json:"size"`
	Blocks      recoverRuns `json:"blocks"`
	Stop        string      `json:"stop"`
	Assumptions []string    `json:"assumptions"`
}

type recoverExpect struct {
	Outcome          string      `json:"outcome"`
	Skip             string      `json:"skip"`
	Method           string      `json:"method"`
	Size             int64       `json:"size"`
	CapturedBlocks   recoverRuns `json:"captured_blocks"`
	Cut              bool        `json:"cut"`
	Incomplete       bool        `json:"incomplete"`
	Confidence       int         `json:"confidence"`
	Assumptions      []string    `json:"assumptions"`
	EqualSource      string      `json:"equal_source"`
	CandidateMethods []string    `json:"candidate_methods"`
	Overlaps         []string    `json:"overlaps"`
}

type recoverKernelState struct {
	ISize       int64 `json:"i_size"`
	RootEntries int   `json:"root_entries"`
	LeafIntact  bool  `json:"leaf_intact"`
	InodeFree   bool  `json:"inode_free"`
}

type recoverDeleted struct {
	Path         string             `json:"path"`
	Name         string             `json:"name"`
	Family       string             `json:"family"`
	Inode        uint32             `json:"inode"`
	Generation   uint32             `json:"generation"`
	SourceSHA256 string             `json:"source_sha256"`
	SourceSize   int64              `json:"source_size"`
	TrueBasis    string             `json:"true_basis"`
	TrueBlocks   recoverRuns        `json:"true_blocks"`
	TreeBlocks   []int64            `json:"tree_blocks"`
	ReaderBlocks recoverRuns        `json:"reader_blocks"`
	ReaderStop   string             `json:"reader_stop"`
	Candidates   []recoverCandidate `json:"candidates"`
	KernelState  recoverKernelState `json:"kernel_state"`
	Expect       recoverExpect      `json:"expect"`
}

type recoverOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
		Kernel      string `json:"kernel"`
		Packages    []struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"packages"`
		Commands []string          `json:"commands"`
		FsckExit int               `json:"fsck_exit"`
		Gates    map[string]string `json:"gates"`
	} `json:"generator"`
	FSType            string `json:"fs_type"`
	BlockSize         int64  `json:"block_size"`
	DeletedEntryCount int    `json:"deleted_entry_count"`
	UnlistedCount     int    `json:"unlisted_count"`
	Totals            struct {
		Recovered int            `json:"recovered"`
		SkippedBy map[string]int `json:"skipped_by"`
		Undecided int            `json:"undecided"`
	} `json:"totals"`
	Deleted []recoverDeleted `json:"deleted"`
	// Wiped lists the files a wiping kernel removed: no record is left in their directory, and their inode is free.
	Wiped []struct {
		Path            string `json:"path"`
		Name            string `json:"name"`
		Inode           uint32 `json:"inode"`
		InodeFree       bool   `json:"inode_free"`
		NameInDirectory bool   `json:"name_in_directory"`
	} `json:"wiped"`
}

// maxRecoverFixtureGz is the budget of every fixture image: 400 KiB gzipped.
const maxRecoverFixtureGz = 400 << 10

// loadRecoverFixture returns the decompressed image of testdata/recover/<name>.img.gz and its oracle, after checking the
// gzip size budget and the image sha256 against generator.image_sha256 (a stale oracle fails here, first). The oracle
// is parsed with an unknown-field check, so a field the test does not know about is a failure, not a silent skip.
func loadRecoverFixture(t testing.TB, name string) ([]byte, recoverOracle) {
	t.Helper()
	dir := filepath.Join("testdata", "recover")
	gz := filepath.Join(dir, name+".img.gz")
	if st, err := os.Stat(gz); err != nil {
		t.Fatal(err)
	} else if st.Size() > maxRecoverFixtureGz {
		t.Fatalf("%s is %d bytes gzipped, over the %d byte budget", gz, st.Size(), maxRecoverFixtureGz)
	}
	img := gunzipFixture(t, gz)
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o recoverOracle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("%s oracle: %v", name, err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

// recoverBlocks returns the image bytes of the runs, in order, cut to size.
func recoverBlocks(t *testing.T, img []byte, bs int64, runs recoverRuns, size int64) []byte {
	t.Helper()
	var b []byte
	for _, r := range runs {
		lo, hi := r[0]*bs, (r[0]+r[1])*bs
		if r[0] < 0 || r[1] <= 0 || hi > int64(len(img)) {
			t.Fatalf("run %v lies outside the %d byte image", r, len(img))
		}
		b = append(b, img[lo:hi]...)
	}
	if size >= 0 && int64(len(b)) > size {
		b = b[:size]
	}
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// requiredRecoverFamilies are the scenario families of 3C-R13 (the generator exits 1 naming a missing one; the test
// makes a regeneration that loses one fail too, so the self-check cannot go vacuous).
var requiredRecoverFamilies = map[string][]string{
	"ext4-recover":          ext4RecoverFamilies,
	"ext4-recover-sameboot": {"deep-same-boot"},
	"ext3-recover":          {"blockmap-kernel-direct", "blockmap-kernel-indirect", "intact-debugfs-indirect", "intact-debugfs-reused", "first-record"},
}

var ext4RecoverFamilies = []string{
	"small-kernel", "deep-kernel", "deep-reuse-tail", "deep-reuse-head", "leaf-reused", "inode-reused",
	"hardlink", "inline", "zero-size", "sparse-kernel", "intact-debugfs-small", "intact-debugfs-deep",
}

func TestExt4RecoverFixturesHoldWhatTheyClaim(t *testing.T) {
	kinds := map[string]int{}
	for _, name := range []string{"ext4-recover", "ext3-recover", "ext4-recover-sameboot"} {
		img, o := loadRecoverFixture(t, name)
		bs := o.BlockSize
		if o.Generator.Gates["L"] != "open" || o.Generator.FsckExit != 0 || o.Generator.Kernel == "" {
			t.Errorf("%s: generator section %+v: want gate L open, a clean e2fsck and a kernel", name, o.Generator)
		}
		if o.DeletedEntryCount != len(o.Deleted) {
			t.Errorf("%s: deleted_entry_count %d, %d records", name, o.DeletedEntryCount, len(o.Deleted))
		}
		seen := map[string]bool{}
		for _, d := range o.Deleted {
			seen[d.Family] = true
			e := d.Expect
			if e.Outcome == "recover" {
				art := recoverBlocks(t, img, bs, e.CapturedBlocks, e.Size)
				if int64(len(art)) != e.Size {
					t.Errorf("%s: %s: captured blocks hold %d bytes, the record says %d", name, d.Path, len(art), e.Size)
				}
				kinds[e.EqualSource]++
				switch e.EqualSource {
				case "full":
					if sha256Hex(art) != d.SourceSHA256 {
						t.Errorf("%s: %s: equal_source full, but the artifact bytes do not hash to the source", name, d.Path)
					}
				case "prefix-of-artifact":
					if int64(len(art)) <= d.SourceSize || sha256Hex(art[:d.SourceSize]) != d.SourceSHA256 {
						t.Errorf("%s: %s: equal_source prefix-of-artifact, but the source is not the leading %d bytes of the %d artifact bytes", name, d.Path, d.SourceSize, len(art))
					}
				case "artifact-is-prefix":
					if int64(len(art)) >= d.SourceSize {
						t.Errorf("%s: %s: equal_source artifact-is-prefix, but the artifact (%d bytes) is not shorter than the source (%d)", name, d.Path, len(art), d.SourceSize)
					}
				case "no":
					if int64(len(art)) >= d.SourceSize && sha256Hex(art[:d.SourceSize]) == d.SourceSHA256 {
						t.Errorf("%s: %s: equal_source no, but the artifact starts with the source", name, d.Path)
					}
				default:
					t.Errorf("%s: %s: unknown equal_source %q", name, d.Path, e.EqualSource)
				}
			} else if e.Outcome != "skip" || e.Skip == "" {
				t.Errorf("%s: %s: outcome %q skip %q", name, d.Path, e.Outcome, e.Skip)
			}
			switch d.Family {
			case "deep-kernel": // gate L is open: the freed leaves still hold the extents and verify under (inode, generation)
				if !d.KernelState.LeafIntact || len(d.TreeBlocks) < 2 || len(d.TrueBlocks) < 90 {
					t.Errorf("%s: %s: leaf_intact %v, %d leaves, %d fragments: want intact, two leaves, at least 90 fragments", name, d.Path, d.KernelState.LeafIntact, len(d.TreeBlocks), len(d.TrueBlocks))
				}
			case "deep-same-boot":
				// ruling 3C-C2 (amended): after a same-boot delete the home leaves are older versions; whatever the leaf method
				// builds from them is a correct prefix capped at 20, or nothing, never a mis-assembled map
				if d.TrueBasis != "content-scan" || len(d.TrueBlocks) < 20 {
					t.Errorf("%s: %s: true map by %q with %d fragments: want a content scan and at least 20 fragments", name, d.Path, d.TrueBasis, len(d.TrueBlocks))
				}
				if e.Outcome == "recover" && (e.Method != "ext4-extent-leaf" || e.Confidence > 20 || e.EqualSource == "no") {
					t.Errorf("%s: %s: recovers %+v: a same-boot leaf chain must be a correct prefix at 20 or less", name, d.Path, e)
				}
			case "intact-debugfs-small", "intact-debugfs-deep", "intact-debugfs-indirect":
				// the map and the blocks of a debugfs rm are untouched: the true blocks hold the source
				if got := recoverBlocks(t, img, bs, d.TrueBlocks, d.SourceSize); sha256Hex(got) != d.SourceSHA256 {
					t.Errorf("%s: %s: the true blocks do not hold the source bytes", name, d.Path)
				}
			}
		}
		for _, fam := range requiredRecoverFamilies[name] {
			if !seen[fam] {
				t.Errorf("%s: scenario family %q is missing: the self-check cannot go vacuous", name, fam)
			}
		}
	}
	for _, k := range []string{"full", "prefix-of-artifact", "artifact-is-prefix", "no"} {
		if kinds[k] == 0 {
			t.Errorf("equal_source %q occurs in no recovered record of the two images", k)
		}
	}
}

// ext4-recover-wiped is written by the 6.1 kernel, which wipes a deleted directory record: the oracle lists nothing, the
// removed names are nowhere in their directory (the journal may still hold older copies of its block), and their inodes
// are free. The image is a negative control: a reader that listed a deleted entry here would be inventing it.
func TestExt4RecoverWipedImageListsNothing(t *testing.T) {
	_, o := loadRecoverFixture(t, "ext4-recover-wiped")
	if o.DeletedEntryCount != 0 || len(o.Deleted) != 0 || o.Totals.Recovered != 0 || len(o.Totals.SkippedBy) != 0 {
		t.Errorf("a wiped image must list nothing, got %d records and totals %+v", o.DeletedEntryCount, o.Totals)
	}
	if o.Generator.Kernel != "6.1.0-53-amd64" {
		t.Errorf("generator kernel %q, want the 6.1 kernel", o.Generator.Kernel)
	}
	if len(o.Wiped) < 2 {
		t.Fatalf("the oracle names %d wiped files, want at least 2", len(o.Wiped))
	}
	for _, w := range o.Wiped {
		if w.NameInDirectory || !w.InodeFree {
			t.Errorf("%s: name in its directory %v, inode %d free %v: a wiped record must leave no name and a free inode", w.Path, w.NameInDirectory, w.Inode, w.InodeFree)
		}
	}
}
