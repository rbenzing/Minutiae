package examine_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// F2FS recovery (3D Task 8) through the real reader and the shared skip switch of the planner. The images are
// builder images with two node segments and summaries; the tests patch image bytes with the builder's helpers
// and never use a reader seam.

// Byte offsets inside an inode block without an extra attribute header, and of a dentry block.
const (
	f2oInline = 3
	f2oName   = 92
	f2oPino   = 84
	f2oAddr   = 360
	f2dSlots  = 214
	f2dArray  = 30 // first dentry of a dentry block
	f2dSize   = 11 // hash 4, ino 4, name length 2, type 1
)

var f2le = binary.LittleEndian

func f2Opts(o f2fstest.Options) f2fstest.Options {
	if o.NodeSegs == 0 {
		o.NodeSegs = 2
	}
	o.SSA = true
	return o
}

// f2Stamp: every block repeats "<seed>:<logical block>", so content tells which block it is.
func f2Stamp(n int, seed string) []byte {
	out := make([]byte, n)
	for i := range out {
		rec := fmt.Sprintf("%s:%d", seed, i/bs)
		rec += strings.Repeat(" ", 32-len(rec))
		out[i] = rec[i%32]
	}
	return out
}

func f2Blk(img []byte, a uint32) []byte { return img[int(a)*bs : (int(a)+1)*bs] }

// f2Build builds the files with the recovery options and applies the patches.
func f2Build(o f2fstest.Options, files []f2fstest.File, patch ...func(img []byte, tr *f2fstest.Tree)) ([]byte, *f2fstest.Tree) {
	img, tr := f2fstest.BuildTree(f2Opts(o), files)
	for _, p := range patch {
		p(img, tr)
	}
	return img, tr
}

// f2Session imports the image as the one partition of a disk and opens it with the default registry.
func f2Session(t *testing.T, o f2fstest.Options, files []f2fstest.File, patch ...func(img []byte, tr *f2fstest.Tree)) (*examine.Session, []byte, *f2fstest.Tree) {
	t.Helper()
	img, tr := f2Build(o, files, patch...)
	s, d := imageSession(t, img)
	return s, d, tr
}

func f2Gone(data []byte) []f2fstest.File {
	return []f2fstest.File{{Path: "/d/gone.bin", Data: data, Deleted: true, Unlinked: true}}
}

// f2PatchInode applies mod to the stale inode block of file p and re-seals its checksum.
func f2PatchInode(o f2fstest.Options, p string, mod func(b []byte)) func([]byte, *f2fstest.Tree) {
	o = f2Opts(o)
	return func(img []byte, tr *f2fstest.Tree) {
		a := tr.Addr[tr.NID[p]]
		mod(f2Blk(img, a))
		f2fstest.ResealInode(o, img, a)
	}
}

func f2SetAddrs(b []byte, at int, addrs ...uint32) {
	for i, a := range addrs {
		f2le.PutUint32(b[at+4*i:], a)
	}
}

// f2PointSlot points the deleted dentry slot of directory dir whose inode number is old at inode number ino.
func f2PointSlot(t *testing.T, img []byte, tr *f2fstest.Tree, dir string, old, ino uint32) {
	t.Helper()
	for _, a := range tr.DirBlocks[dir] {
		b := f2Blk(img, a)
		for s := range f2dSlots {
			if b[s/8]>>(s%8)&1 == 0 && f2le.Uint32(b[f2dArray+s*f2dSize+4:]) == old {
				f2le.PutUint32(b[f2dArray+s*f2dSize+4:], ino)
				return
			}
		}
	}
	t.Fatalf("no deleted slot naming inode %d in %s", old, dir)
}

// f2PlantCopy writes a copy of the block at src to free node slot j of the last node segment of a
// 16-segment image, with footer version ver and its summary naming nid.
func f2PlantCopy(o f2fstest.Options, img []byte, src, j, nid uint32, ver uint64, mod func(b []byte)) uint32 {
	o = f2Opts(o)
	l := f2fstest.Geometry(o)
	a := l.Main + 15*f2fstest.BlocksPerSeg + j
	copy(f2Blk(img, a), f2Blk(img, src))
	if mod != nil {
		mod(f2Blk(img, a))
	}
	f2fstest.SetFooter(img, a, f2fstest.Footer{NID: nid, Ino: nid, CPVer: ver})
	f2fstest.SetSSAEntry(img, o, a, nid)
	f2fstest.ResealInode(o, img, a)
	return a
}

// f2Art returns the single recovered artifact whose path ends in name.
func f2Art(t *testing.T, sum examine.RecoverSummary, name string) evidence.ManifestRecord {
	t.Helper()
	var got []evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.Recovery != nil && path.Base(a.Source.Derived.FSPath) == name {
			got = append(got, a)
		}
	}
	return only(t, got)
}

// f2Skips returns the entry-level skips of the plan by entry name.
func f2Skips(t *testing.T, s *examine.Session) map[string]examine.PlanSkip {
	t.Helper()
	plan, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]examine.PlanSkip{}
	for _, it := range plan.Items {
		for _, sk := range it.Skips {
			out[it.Name] = sk
		}
	}
	return out
}

func TestRecoverF2FSUnlinkedFile(t *testing.T) {
	data := f2Stamp(5*bs-100, "a")
	s, disk, _ := f2Session(t, f2fstest.Options{InodeChksum: true, ExtraAttr: true}, f2Gone(data))
	sum := recoverAll(t, s)
	rec := only(t, sum.Artifacts)
	rv := recoveryOf(rec)
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) || rec.Incomplete || rv.Method != "f2fs-node-scan" ||
		*rv.Confidence != 75 || rv.Class != evidence.ClassDeletedFile {
		t.Fatalf("%+v %+v", rec, rv)
	}
	if !hasBasis(rv, "node:nid=") || !hasBasis(rv, "inode-name=match") {
		t.Errorf("basis %v lacks the node block and the name match", rv.Basis)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A data block of the unlinked file that a live file now holds cuts the map: the free prefix only.
func TestRecoverF2FSReusedBlockKeepsFreePrefix(t *testing.T) {
	data, live := f2Stamp(4*bs, "g"), f2Stamp(2*bs, "L")
	files := append(f2Gone(data), f2fstest.File{Path: "/live.bin", Data: live})
	s, disk, _ := f2Session(t, f2fstest.Options{}, files, func(img []byte, tr *f2fstest.Tree) {
		b := f2Blk(img, tr.Addr[tr.NID["/d/gone.bin"]])
		f2SetAddrs(b, f2oAddr+4*2, tr.Data["/live.bin"][0])
	})
	sum := recoverAll(t, s)
	rec := f2Art(t, sum, "gone.bin")
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:2*bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*bs, rec.Incomplete, *rv.Confidence)
	}
	if !hasAssumption(rv, "cut-state=allocated") || !hasAssumption(rv, "prefix-only") {
		t.Errorf("assumptions %v lack cut-state=allocated and prefix-only", rv.Assumptions)
	}
	if bytes.Contains(got, live[:32]) {
		t.Error("the artifact holds a byte of the live file")
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A hole in the block map stops the chain: the prefix before it, flagged, with the stop named in both places.
func TestRecoverF2FSSparseFileIsPrefixCapped(t *testing.T) {
	data := f2Stamp(5*bs, "s")
	s, disk, _ := f2Session(t, f2fstest.Options{}, f2Gone(data),
		f2PatchInode(f2fstest.Options{}, "/d/gone.bin", func(b []byte) { f2SetAddrs(b, f2oAddr+4*2, 0) }))
	sum := recoverAll(t, s)
	rec := only(t, sum.Artifacts)
	rv := recoveryOf(rec)
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, data[:2*bs]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*bs, rec.Incomplete, *rv.Confidence)
	}
	if !hasAssumption(rv, "stale-chain-stop=hole@2") || !strings.Contains(rec.Error, "stale-chain-stop=hole@2") {
		t.Errorf("assumptions %v, error %q: the stop is not named in both", rv.Assumptions, rec.Error)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A stop at the first block leaves a map with no runs: skip no-runs with the reader's stop in the detail. It is
// a decided skip: the run does not fail.
func TestRecoverF2FSStopAtFirstBlockIsNoRunsSkip(t *testing.T) {
	s, _, _ := f2Session(t, f2fstest.Options{}, f2Gone(f2Stamp(3*bs, "h")),
		f2PatchInode(f2fstest.Options{}, "/d/gone.bin", func(b []byte) { f2SetAddrs(b, f2oAddr, 0) }))
	plan, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			detail = c.Skip + "|" + c.SkipDetail()
		}
	}
	if !strings.HasPrefix(detail, "no-runs|") || !strings.Contains(detail, "stale-chain-stop=hole@0") {
		t.Errorf("candidate skip %q, want no-runs naming stale-chain-stop=hole@0", detail)
	}
	sum := recoverAll(t, s) // fails the test on any error: the exit code stays unchanged
	if sum.SkippedBy["no-runs"] != 1 || sum.SkippedBy["undecided"] != 0 || len(sum.Artifacts) != 0 {
		t.Errorf("SkippedBy %v, %d artifacts; want one no-runs skip and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// Two deleted dentries name one unlinked inode whose name and parent match the first only: the second is
// not-found. With two deleted slots in ONE directory that both match, both are written and flagged overlap.
func TestRecoverF2FSTwoDentriesOneInodeOverlap(t *testing.T) {
	data := f2Stamp(3*bs, "o")
	t.Run("other directory", func(t *testing.T) {
		files := []f2fstest.File{
			{Path: "/d1/gone.bin", Data: data, Deleted: true, Unlinked: true},
			{Path: "/d2/other.bin", Deleted: true, NoInode: true},
		}
		s, disk, _ := f2Session(t, f2fstest.Options{}, files, func(img []byte, tr *f2fstest.Tree) {
			f2PointSlot(t, img, tr, "/d2", tr.NID["/d2/other.bin"], tr.NID["/d1/gone.bin"])
		})
		sum := recoverAll(t, s)
		rec := only(t, sum.Artifacts)
		if !bytes.Equal(readArtifact(t, s.Case, rec), data) || *recoveryOf(rec).Confidence != 75 || hasAssumptionPrefix(recoveryOf(rec), "overlap=") {
			t.Errorf("the first entry: %+v", recoveryOf(rec))
		}
		if sum.SkippedBy["not-found"] != 1 || len(sum.SkippedBy) != 1 {
			t.Errorf("SkippedBy %v, want exactly one not-found (the second dentry)", sum.SkippedBy)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	})
	t.Run("same directory", func(t *testing.T) {
		files := []f2fstest.File{
			{Path: "/d/gone.bin", Data: data, Deleted: true, Unlinked: true},
			{Path: "/d/dup", RawName: []byte("gone.bin"), Deleted: true, NoInode: true},
		}
		s, disk, _ := f2Session(t, f2fstest.Options{}, files, func(img []byte, tr *f2fstest.Tree) {
			f2PointSlot(t, img, tr, "/d", tr.NID["/d/dup"], tr.NID["/d/gone.bin"])
		})
		sum := recoverAll(t, s)
		if len(sum.SkippedBy) != 0 || sum.Recovered != 2 || sum.Overlap != 2 {
			t.Fatalf("SkippedBy %v, recovered %d, overlap %d; want both written and flagged", sum.SkippedBy, sum.Recovered, sum.Overlap)
		}
		for _, rec := range sum.Artifacts {
			if rec.Source.Kind != evidence.KindRecover {
				continue
			}
			rv := recoveryOf(rec)
			if !hasAssumptionPrefix(rv, "overlap=") || *rv.Confidence != 30 {
				t.Errorf("%s: confidence %d, assumptions %v; want overlap= and 30", rec.ID, *rv.Confidence, rv.Assumptions)
			}
			assertEqualsRuns(t, s.Case, rec, disk)
		}
		verifyOK(t, s.Case)
	})
}

// capFS answers Recoverable for one entry name with the ErrLimit shape of a capped node index, and defers to
// the real F2FS reader for every other entry. The real cap (2^21 stale node blocks) cannot be built in a
// test and the cap is index-wide in the reader, so the reader's own capped answer is covered at its level
// (TestRecoverableIndexLimitPassesThrough); this stand-in pins what the planner does with that answer.
type capFS struct {
	filesys.FileSystem
	name string
}

func (f capFS) Underlying() filesys.FileSystem { return f.FileSystem }

func (f capFS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	if e.Name == f.name {
		return nil, fmt.Errorf("f2fs node index: more than 8 stale node blocks were found, the index is capped and cannot answer for inode 7: %w", filesys.ErrLimit)
	}
	inner, ok := filesys.As[filesys.Recoverer](f.FileSystem)
	if !ok {
		return nil, filesys.ErrNoRecovery
	}
	return inner.Recoverable(e)
}

func capDrivers(name string) []detect.Driver {
	out := slices.Clone(detect.Drivers)
	for i := range out {
		if out[i].Name != "f2fs" {
			continue
		}
		open := out[i].Open
		out[i].Open = func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := open(r, size)
			if err != nil {
				return nil, err
			}
			return capFS{FileSystem: fsys, name: name}, nil
		}
	}
	return out
}

// An undecided answer is a counted skip, never an aborted run: the tied pair, the evicted nid and the capped
// index each skip `undecided` with their cause in the detail, and the other entries are still recovered.
func TestRecoverF2FSUndecidedIsSkipNotAbort(t *testing.T) {
	o := f2fstest.Options{Version: 20}
	ok := f2Stamp(2*bs, "k")
	files := []f2fstest.File{
		{Path: "/d/ok.bin", Data: ok, Deleted: true, Unlinked: true},
		{Path: "/d/other.bin", Data: f2Stamp(bs, "o")},
		{Path: "/d/tie.bin", Data: f2Stamp(bs, "t"), Deleted: true, Unlinked: true},
		{Path: "/d/ev.bin", Data: f2Stamp(bs, "e"), Deleted: true, Unlinked: true},
		{Path: "/d/cap.bin", Data: f2Stamp(bs, "c"), Deleted: true, Unlinked: true},
	}
	var tieA, tieB uint32
	img, _ := f2Build(o, files, func(img []byte, tr *f2fstest.Tree) {
		tie := tr.NID["/d/tie.bin"]
		tieA = f2PlantCopy(o, img, tr.Addr[tie], 10, tie, 1, nil)
		tieB = f2PlantCopy(o, img, tr.Addr[tie], 11, tie, 1, func(b []byte) { f2SetAddrs(b, f2oAddr, tr.Data["/d/other.bin"]...) })
		ev := tr.NID["/d/ev.bin"]
		f2PatchInode(o, "/d/ev.bin", func(b []byte) { b[f2oName] ^= 0x20 })(img, tr)
		for i := uint32(1); i < 9; i++ {
			f2PlantCopy(o, img, tr.Addr[ev], 20+i, ev, uint64(i), nil)
		}
	})
	c := newCase(t)
	recs := importImage(t, c, disk(img), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: capDrivers("cap.bin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	skips := f2Skips(t, s)
	for name, cause := range map[string][]string{
		"tie.bin": {fmt.Sprintf("0x%x", tieA), fmt.Sprintf("0x%x", tieB)},
		"ev.bin":  {"evicted"},
		"cap.bin": {"capped"},
	} {
		sk, found := skips[name]
		if !found || sk.Reason != "undecided" {
			t.Errorf("%s: skip %+v, want undecided", name, sk)
			continue
		}
		for _, want := range cause {
			if !strings.Contains(sk.Detail, want) {
				t.Errorf("%s: detail %q does not name %q", name, sk.Detail, want)
			}
		}
	}
	sum := recoverAll(t, s)
	if sum.SkippedBy["undecided"] != 3 || sum.Recovered != 1 || len(sum.SkippedBy) != 1 {
		t.Fatalf("SkippedBy %v, recovered %d; want undecided 3 and the decided entry recovered", sum.SkippedBy, sum.Recovered)
	}
	rec := f2Art(t, sum, "ok.bin")
	if !bytes.Equal(readArtifact(t, s.Case, rec), ok) {
		t.Error("the decided entry's bytes differ from the source")
	}
	verifyOK(t, s.Case)
}

// An inode number a live file holds (in use, a rename's stale slot, a hard link) is a decided skip `reused`:
// not counted as undecided, and the detail ends with the inode and its NAT address.
func TestRecoverF2FSInodeLiveIsReused(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/d/inuse.bin", Data: f2Stamp(bs, "u"), Deleted: true},
		{Path: "/n/new.bin", Data: f2Stamp(bs, "n")},
		{Path: "/d/old.bin", Deleted: true, NoInode: true},
		{Path: "/e/other.bin", Data: f2Stamp(bs, "h")},
		{Path: "/d/old2.bin", Deleted: true, NoInode: true},
	}
	var want map[string]string
	s, _, _ := f2Session(t, f2fstest.Options{}, files, func(img []byte, tr *f2fstest.Tree) {
		f2PointSlot(t, img, tr, "/d", tr.NID["/d/old.bin"], tr.NID["/n/new.bin"])
		f2PointSlot(t, img, tr, "/d", tr.NID["/d/old2.bin"], tr.NID["/e/other.bin"])
		suffix := func(p string) string { return fmt.Sprintf("inode %d is live (NAT %d)", tr.NID[p], tr.Addr[tr.NID[p]]) }
		want = map[string]string{"inuse.bin": suffix("/d/inuse.bin"), "old.bin": suffix("/n/new.bin"), "old2.bin": suffix("/e/other.bin")}
	})
	skips := f2Skips(t, s)
	for name, suffix := range want {
		if sk := skips[name]; sk.Reason != "reused" || !strings.HasSuffix(sk.Detail, suffix) {
			t.Errorf("%s: skip %+v, want reused ending %q", name, sk, suffix)
		}
	}
	sum := recoverAll(t, s)
	if sum.SkippedBy["reused"] != 3 || sum.SkippedBy["undecided"] != 0 || len(sum.SkippedBy) != 1 || len(sum.Artifacts) != 0 {
		t.Errorf("SkippedBy %v, %d artifacts; want reused 3 and nothing else", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

func TestRecoverF2FSInlineIsUnsupportedSkip(t *testing.T) {
	s, _, _ := f2Session(t, f2fstest.Options{}, f2Gone(f2Stamp(2*bs, "i")),
		f2PatchInode(f2fstest.Options{}, "/d/gone.bin", func(b []byte) { b[f2oInline] |= 0x02 }))
	sum := recoverAll(t, s)
	if sum.SkippedBy["unsupported"] != 1 || len(sum.SkippedBy) != 1 || len(sum.Artifacts) != 0 {
		t.Errorf("SkippedBy %v, %d artifacts; want one unsupported skip", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

// Discarded data reads as zeros: an all-zero file is not a finding, it is skipped as uniform (audited, counted)
// and written only on request.
func TestRecoverF2FSDiscardedDataIsUniformSkip(t *testing.T) {
	s, disk, _ := f2Session(t, f2fstest.Options{}, f2Gone(make([]byte, 3*bs)))
	sum := recoverAll(t, s)
	if sum.SkippedBy["uniform"] != 1 || sum.Uniform != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, Uniform %d, %d artifacts", sum.SkippedBy, sum.Uniform, len(sum.Artifacts))
	}
	if !anyContains(warnReasons(t, s.Case), "uniform") {
		t.Errorf("no audited uniform warning: %v", warnReasons(t, s.Case))
	}
	kept, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, KeepUniform: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := f2Art(t, kept, "gone.bin")
	if recoveryOf(rec).Content != "uniform" {
		t.Errorf("Content %q, want uniform", recoveryOf(rec).Content)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// An encrypted file is recovered as ciphertext: the bytes are the image's, the artifact says so.
func TestRecoverF2FSEncryptedIsCiphertext(t *testing.T) {
	data := f2Stamp(2*bs, "x")
	files := []f2fstest.File{{Path: "/d/gone.bin", Data: data, Encrypted: true, Deleted: true, Unlinked: true}}
	s, disk, _ := f2Session(t, f2fstest.Options{Encrypt: true}, files)
	sum := recoverAll(t, s)
	rec := only(t, sum.Artifacts)
	if !rec.Source.Derived.Encrypted {
		t.Errorf("artifact not flagged encrypted: %+v", rec.Source.Derived)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// The list plans exactly what a run writes.
func TestRecoverF2FSListMatchesRun(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/d/ok.bin", Data: f2Stamp(3*bs, "a"), Deleted: true, Unlinked: true},
		{Path: "/d/cut.bin", Data: f2Stamp(4*bs, "b"), Deleted: true, Unlinked: true},
		{Path: "/d/sparse.bin", Data: f2Stamp(5*bs, "c"), Deleted: true, Unlinked: true},
		{Path: "/live.bin", Data: f2Stamp(bs, "L")},
	}
	s, _, _ := f2Session(t, f2fstest.Options{}, files, func(img []byte, tr *f2fstest.Tree) {
		f2SetAddrs(f2Blk(img, tr.Addr[tr.NID["/d/cut.bin"]]), f2oAddr+4*1, tr.Data["/live.bin"][0])
		f2SetAddrs(f2Blk(img, tr.Addr[tr.NID["/d/sparse.bin"]]), f2oAddr+4*3, 0)
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
	}
	planned := map[string]shape{}
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned[it.ID] = shape{c.Method, c.Confidence, c.Runs, c.Size}
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
		written[a.Source.Derived.FSID] = shape{rv.Method, *rv.Confidence, a.Source.Derived.Runs, a.Size}
	}
	if len(planned) != 3 || !reflect.DeepEqual(planned, written) {
		t.Fatalf("planned %+v\nwritten %+v", planned, written)
	}
}

// Recovery reads the image and writes only to the case: the imported image artifact is unchanged.
func TestRecoverF2FSNeverModifiesSource(t *testing.T) {
	files := append(f2Gone(f2Stamp(4*bs, "g")), f2fstest.File{Path: "/live.bin", Data: f2Stamp(bs, "L")})
	s, disk, _ := f2Session(t, f2fstest.Options{}, files)
	before := sha256hex(readArtifact(t, s.Case, s.Parent))
	if before != sha256hex(disk) {
		t.Fatal("the imported artifact differs from the source before the run")
	}
	recoverAll(t, s)
	if after := sha256hex(readArtifact(t, s.Case, s.Parent)); after != before {
		t.Errorf("the image artifact changed: %s -> %s", before, after)
	}
	verifyOK(t, s.Case)
}
