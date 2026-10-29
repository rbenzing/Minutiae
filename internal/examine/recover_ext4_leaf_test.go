package examine_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const leafBS = 1024 // block size of the leaf images

var (
	leafCsum = ext4test.Options{Extents: true, MetadataCsum: true}
	// leafKernel is what the kernel leaves on a same-boot unlink: freed blocks and inode, size and map cleared.
	leafKernel = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true, ZeroBlockPointers: true}
)

// leafTwoLeafFile is 90 one-block pieces under a depth-1 tree of two leaves.
func leafTwoLeafFile(path string, seed byte) (ext4test.File, []byte) {
	data := pat(seed, 90*leafBS)
	ps := make([]ext4test.Piece, 90)
	for i := range ps {
		ps[i] = ext4test.Piece{Block: i, Data: data[i*leafBS : (i+1)*leafBS]}
	}
	return ext4test.File{Path: path, Deleted: true, ExtentLeaves: 2, Scatter: true, Pieces: ps}, data
}

// leafBig is a deleted file of n contiguous blocks whose leaves a test writes itself.
func leafBig(path string, n int, seed byte) ext4test.File {
	return ext4test.File{Path: path, Data: pat(seed, n*leafBS), Deleted: true}
}

// putLeafBlocks writes extent leaves for path's own inode and generation into the last free blocks of its data. Each
// extent's Phys is an index into the data blocks below the leaves.
func putLeafBlocks(l *ext4test.Layout, img []byte, path string, leaves ...[]ext4test.Extent) {
	loc := l.File(path)
	data := loc.Data[:len(loc.Data)-len(leaves)]
	for i, exts := range leaves {
		ex := slices.Clone(exts)
		for k := range ex {
			ex[k].Phys = data[ex[k].Phys]
		}
		l.WriteLeaf(img, loc.Data[len(loc.Data)-1-i], loc.Inode, loc.Generation, ex, true)
	}
}

func TestRecoverExt4LeafAfterKernelUnlink(t *testing.T) {
	file, data := leafTwoLeafFile("/gone.bin", 11)
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{file})
	l.Unlink(img, "/gone.bin", leafKernel)
	s, disc := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := only(t, sum.Artifacts)
	rv := recoveryOf(rec)
	// The kernel zeroed i_size (GATE-S), so the end of the file is unknown even though both leaves survive: the method
	// base of 55 is capped at 20 (3C-C3).
	if *rv.Confidence != 20 || rv.Method != "ext4-extent-leaf" || rv.Class != evidence.ClassDeletedFile {
		t.Fatalf("confidence %d method %q class %q, want 20, ext4-extent-leaf, deleted-file", *rv.Confidence, rv.Method, rv.Class)
	}
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) {
		t.Error("the artifact is not the file")
	}
	for _, a := range []string{"owner-by-crc32c-seed", "in-inode-map-cleared", "leaf-may-be-superseded"} {
		if !hasAssumption(rv, a) {
			t.Errorf("assumptions %q lack %s", rv.Assumptions, a)
		}
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
}

func TestRecoverExt4LeafReusedTailKeepsPrefix(t *testing.T) {
	file, data := leafTwoLeafFile("/gone.bin", 12)
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{file})
	l.Unlink(img, "/gone.bin", leafKernel)
	l.SetBlockFree(img, l.File("/gone.bin").Data[60], false) // a data block of leaf 2 belongs to a new file
	s, disc := imageSession(t, img)
	rec := only(t, recoverAll(t, s).Artifacts)
	rv := recoveryOf(rec)
	if !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("incomplete %v confidence %d, want true, 20", rec.Incomplete, *rv.Confidence)
	}
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, data[:60*leafBS]) {
		t.Errorf("the artifact is %d bytes, want exactly the 60 free blocks before the reused one", len(got))
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
}

func TestRecoverExt4LeafMissingHeadIsNoRunsSkip(t *testing.T) {
	file, _ := leafTwoLeafFile("/gone.bin", 13)
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{file})
	l.Unlink(img, "/gone.bin", leafKernel)
	clear(img[l.File("/gone.bin").Tree[0]*leafBS:][:leafBS]) // only the second leaf survives
	s, _ := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.SkippedBy["no-runs"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts; want one no-runs and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
	found := false
	for _, w := range auditByAction(t, s.Case, "analysis.warning") {
		if r, _ := w.Details["reason"].(string); strings.Contains(r, "no-runs") && strings.Contains(r, "leaf-gap@0") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audited no-runs warning names leaf-gap@0: %v", auditByAction(t, s.Case, "analysis.warning"))
	}
	verifyOK(t, s.Case)
}

// A leaf in the middle is gone: the 84 blocks before the gap are written, flagged and capped, and the bytes the file
// declares past them are counted as excluded, never filled (3C-R23).
func TestRecoverExt4LeafGapBeforeLaterLeafCapsAt20(t *testing.T) {
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{leafBig("/gone.bin", 220, 14)})
	l.Unlink(img, "/gone.bin", leafKernel)
	putLeafBlocks(l, img, "/gone.bin",
		[]ext4test.Extent{{Lblk: 0, Len: 84, Phys: 0}},
		[]ext4test.Extent{{Lblk: 168, Len: 33, Phys: 168}}) // the leaf for 84-167 is gone
	s, disc := imageSession(t, img)
	rec := only(t, recoverAll(t, s).Artifacts)
	rv := recoveryOf(rec)
	if !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("incomplete %v confidence %d, want true, 20", rec.Incomplete, *rv.Confidence)
	}
	want := pat(14, 220*leafBS)[:84*leafBS]
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, want) {
		t.Errorf("the artifact is %d bytes, want exactly the 84-block prefix", len(got))
	}
	if rv.Alloc.ExcludedBytes != (201-84)*leafBS {
		t.Errorf("ExcludedBytes %d, want the %d declared bytes past the prefix", rv.Alloc.ExcludedBytes, (201-84)*leafBS)
	}
	if !hasAssumption(rv, "file-end-unknown") || !hasAssumption(rv, "stale-chain-stop=leaf-gap@84") {
		t.Errorf("assumptions %q", rv.Assumptions)
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
}

// Superseded leaves of a same-boot delete overlap: nothing is assembled from them (3C-R31), and what is written is a
// correct prefix at 20 or nothing.
func TestRecoverExt4LeafSupersededLeavesWriteNoGuess(t *testing.T) {
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{leafBig("/gone.bin", 260, 15)})
	l.Unlink(img, "/gone.bin", leafKernel)
	putLeafBlocks(l, img, "/gone.bin",
		[]ext4test.Extent{{Lblk: 0, Len: 100, Phys: 0}},
		[]ext4test.Extent{{Lblk: 0, Len: 200, Phys: 0}}) // the newer copy of the same range
	s, _ := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.SkippedBy["no-runs"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts; want one no-runs and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// --list does not run the free-space pass (3C-R25): the intact file is planned alike in both modes, the file only the
// leaves can recover is undecided in the list and recovered by the run (at 20: its end is unknown).
func TestListDiffersFromRunOnlyByLeafSearch(t *testing.T) {
	intact := ext4test.File{Path: "/intact.bin", Data: pat(16, 3*leafBS), Deleted: true}
	leafy, leafyData := leafTwoLeafFile("/leafy.bin", 17)
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{intact, leafy})
	l.Unlink(img, "/intact.bin", ext4test.DebugfsRm)
	l.Unlink(img, "/leafy.bin", leafKernel)
	s, _ := imageSession(t, img)
	opts := examine.RecoverOptions{Partition: -1, All: true}
	plan, err := s.PlanRecovery(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		Method     string
		Confidence int
		Runs       []evidence.Run
		Size       int64
	}
	planned := map[string]shape{}
	undecided := ""
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned[it.Path] = shape{c.Method, c.Confidence, c.Runs, c.Size}
			}
		}
		for _, sk := range it.Skips {
			if sk.Reason == "undecided" {
				undecided = it.Path
			}
		}
	}
	if len(planned) != 1 || undecided == "" || plan.SkippedBy["undecided"] != 1 {
		t.Fatalf("planned %v, undecided %q, SkippedBy %v; want the intact file planned and the leaf file undecided", planned, undecided, plan.SkippedBy)
	}
	sum, err := s.Recover(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Recovered != 2 || sum.SkippedBy["undecided"] != 0 {
		t.Fatalf("Recovered %d SkippedBy %v, want both files recovered", sum.Recovered, sum.SkippedBy)
	}
	foundLeaf := false
	for _, rec := range sum.Artifacts {
		rv := recoveryOf(rec)
		switch rv.Method {
		case "ext-inode-intact":
			var got shape
			for _, sh := range planned {
				got = sh
			}
			var runs []evidence.Run
			runs = append(runs, rec.Source.Derived.Runs...)
			if got.Method != rv.Method || got.Confidence != *rv.Confidence || got.Size != rec.Size || !slices.Equal(got.Runs, runs) {
				t.Errorf("the list planned %+v, the run wrote method %s confidence %d size %d runs %v", got, rv.Method, *rv.Confidence, rec.Size, runs)
			}
		case "ext4-extent-leaf":
			foundLeaf = bytes.Equal(readArtifact(t, s.Case, rec), leafyData) && *rv.Confidence == 20
		}
	}
	if !foundLeaf {
		t.Error("the run did not recover the leaf-only file at 20")
	}
	verifyOK(t, s.Case)
}

// The same kernel unlink with the last leaf overwritten: a shorter file than the real one is written, never above 20.
func TestRecoverExt4LeafZeroSizeLostLastLeafCapsAt20(t *testing.T) {
	file, data := leafTwoLeafFile("/gone.bin", 18)
	img, l := ext4test.BuildLayout(leafCsum, []ext4test.File{file})
	l.Unlink(img, "/gone.bin", leafKernel)
	clear(img[l.File("/gone.bin").Tree[1]*leafBS:][:leafBS])
	s, disc := imageSession(t, img)
	rec := only(t, recoverAll(t, s).Artifacts)
	rv := recoveryOf(rec)
	if *rv.Confidence != 20 || !hasAssumption(rv, "file-end-unknown") {
		t.Fatalf("confidence %d assumptions %q, want 20 and file-end-unknown", *rv.Confidence, rv.Assumptions)
	}
	if !bytes.Equal(readArtifact(t, s.Case, rec), data[:45*leafBS]) {
		t.Error("the artifact is not exactly the first leaf's blocks")
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
}
