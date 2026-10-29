package examine_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// The ext4 recovery policy (the free-space cut, the caps, overlap, the uniform skip) is 3A's; these tests drive it
// through the real ext4 reader on builder images, so a reader answer that the planner would mishandle shows here.

const e4bs = 1024 // the builder's default block size

var e4opts = ext4test.Options{Extents: true, MetadataCsum: true}

// e4ID is the entry id of the deleted record of path.
func e4ID(l *ext4test.Layout, path string) string {
	f := l.File(path)
	return fmt.Sprintf("dirent:%d:%d", f.DirentBlock, f.DirentOffset)
}

// e4Session builds files, unlinks every Deleted one the way a debugfs rm does, applies mutate and opens the image.
func e4Session(t *testing.T, o ext4test.Options, files []ext4test.File, mutate func(img []byte, l *ext4test.Layout)) (*examine.Session, []byte, *ext4test.Layout) {
	t.Helper()
	img, l := ext4test.BuildLayout(o, files)
	for _, f := range files {
		if f.Deleted {
			l.Unlink(img, f.Path, ext4test.DebugfsRm)
		}
	}
	if mutate != nil {
		mutate(img, l)
	}
	s, disk := imageSession(t, img)
	return s, disk, l
}

func e4Gone(data []byte) []ext4test.File {
	return []ext4test.File{{Path: "/keep", Data: []byte("keep")}, {Path: "/gone.bin", Data: data, Deleted: true}}
}

func TestRecoverExt4IntactFile(t *testing.T) {
	data := pattern(5*e4bs+300, 51)
	f := ext4test.File{ // two scattered pieces: two runs, so a reordered map shows
		Path: "/gone.bin", Deleted: true, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*e4bs]}, {Block: 3, Data: data[3*e4bs:]}},
	}
	s, disk, l := e4Session(t, e4opts, []ext4test.File{{Path: "/keep", Data: []byte("keep")}, f}, nil)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, e4ID(l, "/gone.bin"))
	rv := recoveryOf(rec)
	if sha256hex(readArtifact(t, s.Case, rec)) != sha256hex(data) || rec.Incomplete ||
		rv.Method != "ext-inode-intact" || *rv.Confidence != 80 || rv.Class != evidence.ClassDeletedFile {
		t.Fatalf("%+v %+v", rec, rv)
	}
	if !strings.Contains(rec.Path, "recovered/p1-ext4/") {
		t.Errorf("artifact path %q is not under recovered/p1-ext4/", rec.Path)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A data block that a new file took cuts the map: only the free prefix is written, flagged incomplete at 20, the cut
// named, and no byte of the new owner is in the artifact.
func TestRecoverExt4ReusedDataBlockKeepsFreePrefix(t *testing.T) {
	data := pattern(6*e4bs, 52)
	live := pattern(e4bs, 99)
	var cutBlk int
	s, disk, l := e4Session(t, e4opts, e4Gone(data), func(img []byte, l *ext4test.Layout) {
		cutBlk = l.File("/gone.bin").Data[3]
		l.SetBlockFree(img, cutBlk, false)
		copy(img[cutBlk*e4bs:], live) // what the new owner wrote there
	})
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, e4ID(l, "/gone.bin"))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:3*e4bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 3*e4bs, rec.Incomplete, *rv.Confidence)
	}
	if !hasAssumption(rv, "prefix-only") || !hasAssumption(rv, "cut-state=allocated") {
		t.Errorf("assumptions %v lack prefix-only and cut-state=allocated", rv.Assumptions)
	}
	if bytes.Contains(got, live) {
		t.Error("the artifact holds the bytes of the new owner")
	}
	var cut int64
	for _, x := range rv.Excluded {
		cut += x.Length
	}
	if cut != 3*e4bs || len(rv.Excluded) == 0 || rv.Excluded[0].Offset != partStart+int64(cutBlk)*e4bs {
		t.Errorf("Excluded %+v does not name the 3 cut blocks from block %d", rv.Excluded, cutBlk)
	}
	if !strings.Contains(rec.Error, "runs not captured") {
		t.Errorf("Error %q does not say which runs were not captured", rec.Error)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverExt4FirstBlockReusedWritesNothing(t *testing.T) {
	s, _, _ := e4Session(t, e4opts, e4Gone(pattern(3*e4bs, 53)), func(img []byte, l *ext4test.Layout) {
		l.SetBlockFree(img, l.File("/gone.bin").Data[0], false)
	})
	sum := recoverAll(t, s)
	if sum.SkippedBy["no-free-bytes"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts; want one no-free-bytes skip and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

func e4TwoLeaf() (ext4test.File, []byte) {
	data := pattern(6*e4bs, 54)
	return ext4test.File{
		Path: "/gone.bin", Deleted: true, ExtentLeaves: 2, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*e4bs]}, {Block: 3, Data: data[3*e4bs:]}},
	}, data
}

// An extent tree block that is not free holds someone else's pointers: the map stops before it, so the second leaf's
// blocks are never read, whatever is in them.
func TestRecoverExt4ReusedTreeBlockCapsAt20(t *testing.T) {
	f, data := e4TwoLeaf()
	s, disk, l := e4Session(t, e4opts, []ext4test.File{{Path: "/keep", Data: []byte("k")}, f}, func(img []byte, l *ext4test.Layout) {
		l.SetBlockFree(img, l.File("/gone.bin").Tree[1], false)
	})
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, e4ID(l, "/gone.bin"))
	rv := recoveryOf(rec)
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, data[:3*e4bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 3*e4bs, rec.Incomplete, *rv.Confidence)
	}
	if !hasAssumption(rv, "stale-chain-stop=map-block-not-free@3") || !hasAssumption(rv, "prefix-only") {
		t.Errorf("assumptions %v", rv.Assumptions)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverExt4ReusedInodeIsReused(t *testing.T) {
	s, _, _ := e4Session(t, e4opts, e4Gone(pattern(3*e4bs, 55)), func(img []byte, l *ext4test.Layout) {
		l.SetInodeFree(img, l.File("/gone.bin").Inode, false) // allocated again
	})
	sum := recoverAll(t, s)
	if sum.SkippedBy["reused"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// The inode is still allocated with a link: the file was never deleted from it, or the name is a stale copy.
func TestRecoverExt4HardLinkedInodeIsReused(t *testing.T) {
	s, _, _ := e4Session(t, e4opts, e4Gone(pattern(3*e4bs, 56)), func(img []byte, l *ext4test.Layout) {
		l.SetInodeFree(img, l.File("/gone.bin").Inode, false)
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) {
			raw[0x1A], raw[0x1B] = 1, 0 // i_links_count
			clear(raw[0x14:0x18])       // dtime
		})
	})
	sum := recoverAll(t, s)
	if sum.SkippedBy["reused"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// A hole stops the map: the artifact is the bytes before it, capped at 20, and nothing fills the gap.
func TestRecoverExt4SparseFileStopsAtHole(t *testing.T) {
	data := pattern(5*e4bs, 57)
	f := ext4test.File{
		Path: "/gone.bin", Deleted: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*e4bs]}, {Block: 6, Data: data[3*e4bs:]}},
	}
	for _, o := range []ext4test.Options{e4opts, {}} { // an extent tree and a block map
		s, disk, l := e4Session(t, o, []ext4test.File{{Path: "/keep", Data: []byte("k")}, f}, nil)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, e4ID(l, "/gone.bin"))
		rv := recoveryOf(rec)
		if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, data[:3*e4bs]) || !rec.Incomplete || *rv.Confidence != 20 {
			t.Fatalf("extents=%v: artifact %d bytes (want %d), incomplete %v, confidence %d", o.Extents, len(got), 3*e4bs, rec.Incomplete, *rv.Confidence)
		}
		if !hasAssumption(rv, "stale-chain-stop=hole@3") {
			t.Errorf("extents=%v: assumptions %v lack stale-chain-stop=hole@3", o.Extents, rv.Assumptions)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	}
}

// A hole or an uninitialized extent at block 0 leaves no run: the entry is skipped as no-runs, and the reader's stop
// is in the audited reason (the cut is explained, not silent).
func TestRecoverExt4StopBeforeFirstRunIsNoRunsSkip(t *testing.T) {
	data := pattern(2*e4bs, 58)
	for _, tc := range []struct {
		name  string
		piece ext4test.Piece
		stop  string
	}{
		{"hole", ext4test.Piece{Block: 2, Data: data}, "hole@0"},
		{"uninit", ext4test.Piece{Block: 0, Data: data, Uninit: true}, "uninit@0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ext4test.File{Path: "/gone.bin", Deleted: true, Pieces: []ext4test.Piece{tc.piece}}
			s, _, _ := e4Session(t, e4opts, []ext4test.File{{Path: "/keep", Data: []byte("k")}, f}, nil)
			plan, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
			if err != nil {
				t.Fatal(err)
			}
			var detail string
			for _, it := range plan.Items {
				for _, sk := range it.Skips {
					if sk.Reason == "no-runs" {
						detail = sk.Detail
					}
				}
			}
			if !strings.Contains(detail, "stale-chain-stop="+tc.stop) {
				t.Errorf("the no-runs detail %q does not carry stale-chain-stop=%s", detail, tc.stop)
			}
			sum := recoverAll(t, s)
			if sum.SkippedBy["no-runs"] != 1 || len(sum.Artifacts) != 0 {
				t.Fatalf("SkippedBy %v, %d artifacts; want one no-runs skip and nothing written", sum.SkippedBy, len(sum.Artifacts))
			}
			verifyOK(t, s.Case)
		})
	}
}

func TestRecoverExt4InlineIsUnsupported(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, InlineData: true}
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone.txt", Data: []byte("tiny inline file"), Inline: true, Deleted: true}}
	s, _, _ := e4Session(t, o, files, nil)
	sum := recoverAll(t, s)
	if sum.SkippedBy["unsupported"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// The first record of a directory block loses its inode number when it is deleted: no candidate, undecided.
func TestRecoverExt4FirstRecordIsUndecided(t *testing.T) {
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone.bin", Data: pattern(e4bs, 59), Deleted: true, NewBlock: true}}
	s, _, _ := e4Session(t, e4opts, files, nil)
	sum := recoverAll(t, s)
	if sum.SkippedBy["undecided"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// Two deleted entries whose maps name the same blocks both claim them: both are written, both at 30, naming each
// other.
func TestRecoverExt4TwoEntriesOneInodeOverlap(t *testing.T) {
	a, b := pattern(3*e4bs, 60), pattern(3*e4bs, 61)
	files := []ext4test.File{
		{Path: "/keep", Data: []byte("k")},
		{Path: "/a.bin", Data: a, Deleted: true},
		{Path: "/b.bin", Data: b, Deleted: true},
	}
	s, disk, l := e4Session(t, e4opts, files, func(img []byte, l *ext4test.Layout) {
		src := append([]byte(nil), img[l.File("/a.bin").InodeOffset+0x28:][:60]...)
		l.PatchInode(img, l.File("/b.bin").Inode, func(raw []byte) { copy(raw[0x28:0x28+60], src) }) // b's map is a's
	})
	sum := recoverAll(t, s)
	if sum.Overlap != 2 || len(sum.Artifacts) < 2 {
		t.Fatalf("Overlap %d, %d artifacts; want 2 and both written", sum.Overlap, len(sum.Artifacts))
	}
	ia, ib := e4ID(l, "/a.bin"), e4ID(l, "/b.bin")
	for _, x := range []struct{ mine, other string }{{ia, ib}, {ib, ia}} {
		rec := artifactOf(t, sum, x.mine)
		rv := recoveryOf(rec)
		if *rv.Confidence != 30 || !hasAssumption(rv, "overlap="+x.other) {
			t.Errorf("%s: confidence %d, assumptions %v; want 30 and overlap=%s", x.mine, *rv.Confidence, rv.Assumptions, x.other)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
	}
	verifyOK(t, s.Case)
}

func TestRecoverExt4DeletedDirectoryIsNotAFile(t *testing.T) {
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gonedir", Dir: true, Deleted: true}}
	s, _, _ := e4Session(t, e4opts, files, nil)
	sum := recoverAll(t, s)
	if sum.SkippedBy["not-a-file"] != 1 || len(sum.Artifacts) != 0 || sum.Recovered != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// An encrypted file is recovered as ciphertext and flagged.
func TestRecoverExt4EncryptedFlagged(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, Encrypt: true}
	data := pattern(3*e4bs, 62)
	files := []ext4test.File{{Path: "/enc", Dir: true}, {Path: "/enc/x", Data: data, Deleted: true}}
	s, disk, l := e4Session(t, o, files, nil)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, e4ID(l, "/enc/x"))
	if !rec.Source.Derived.Encrypted {
		t.Error("Derived.Encrypted is false for an encrypted file")
	}
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) {
		t.Error("the artifact is not the on-disk (cipher) bytes")
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// The stale inode itself carries the encrypt flag although its directory does not (the listing and the candidate both
// report it).
func TestRecoverExt4EncryptedInodeInPlainDirectoryFlagged(t *testing.T) {
	data := pattern(3*e4bs, 69)
	s, disk, l := e4Session(t, e4opts, e4Gone(data), func(img []byte, l *ext4test.Layout) {
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) { raw[0x20+1] |= 0x08 }) // i_flags bit 0x800
	})
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, e4ID(l, "/gone.bin"))
	if !rec.Source.Derived.Encrypted {
		t.Error("Derived.Encrypted is false for an inode with the encrypt flag")
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// An i_size of 2^62 is declared, never clamped to the filesystem: the candidate is an invalid map and nothing is
// written (3C-R27).
func TestRecoverExt4HugeInodeSizeIsInvalidMap(t *testing.T) {
	s, _, _ := e4Session(t, e4opts, e4Gone(pattern(3*e4bs, 63)), func(img []byte, l *ext4test.Layout) {
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) {
			clear(raw[0x4:0x8])
			raw[0x6C+3] = 0x40 // i_size_high = 1<<30: the size is 2^62
		})
	})
	sum := recoverAll(t, s)
	if sum.SkippedBy["invalid-map"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// The list (PlanRecovery) plans exactly what a run writes: method, confidence, runs and size.
func TestRecoverExt4ListMatchesRun(t *testing.T) {
	f2, _ := e4TwoLeaf()
	f2.Path = "/two.bin"
	files := []ext4test.File{
		{Path: "/keep", Data: []byte("k")},
		{Path: "/ok.bin", Data: pattern(3*e4bs, 64), Deleted: true},
		{Path: "/reused.bin", Data: pattern(5*e4bs, 65), Deleted: true},
		{Path: "/a.bin", Data: pattern(2*e4bs, 66), Deleted: true},
		{Path: "/b.bin", Data: pattern(2*e4bs, 67), Deleted: true},
		f2,
	}
	s, _, l := e4Session(t, e4opts, files, func(img []byte, l *ext4test.Layout) {
		l.SetBlockFree(img, l.File("/reused.bin").Data[2], false)
		src := append([]byte(nil), img[l.File("/a.bin").InodeOffset+0x28:][:60]...)
		l.PatchInode(img, l.File("/b.bin").Inode, func(raw []byte) { copy(raw[0x28:0x28+60], src) })
	})
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
		Overlap    bool
	}
	planned := map[string]shape{}
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned[it.ID] = shape{c.Method, c.Confidence, c.Runs, c.Size, hasOverlapAssumption(c.Assumptions)}
			}
		}
	}
	sum, err := s.Recover(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]shape{}
	for _, a := range sum.Artifacts {
		if a.Source.Kind != evidence.KindRecover {
			continue
		}
		rv := recoveryOf(a)
		written[a.Source.Derived.FSID] = shape{rv.Method, *rv.Confidence, a.Source.Derived.Runs, a.Size, hasAssumptionPrefix(rv, "overlap=")}
	}
	if len(planned) != 5 || !reflect.DeepEqual(planned, written) {
		t.Fatalf("planned %+v\nwritten %+v", planned, written)
	}
	if !planned[e4ID(l, "/a.bin")].Overlap || !planned[e4ID(l, "/b.bin")].Overlap {
		t.Errorf("the list does not show the overlap: %+v", planned)
	}
}

func hasOverlapAssumption(as []string) bool {
	for _, a := range as {
		if strings.HasPrefix(a, "overlap=") {
			return true
		}
	}
	return false
}

func TestRecoverExt4NeverModifiesSource(t *testing.T) {
	s, _, _ := e4Session(t, e4opts, e4Gone(pattern(3*e4bs, 68)), nil)
	before := sha256hex(readArtifact(t, s.Case, s.Parent))
	if sum := recoverAll(t, s); sum.Recovered != 1 {
		t.Fatalf("Recovered %d", sum.Recovered)
	}
	if after := sha256hex(readArtifact(t, s.Case, s.Parent)); after != before {
		t.Fatalf("the source artifact changed: %s -> %s", before, after)
	}
	verifyOK(t, s.Case)
}
