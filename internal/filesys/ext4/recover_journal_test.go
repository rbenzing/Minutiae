package ext4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests build images with ext4test: a file built Deleted, the inode-table block of its live state copied with
// InodeCopy before Unlink applies the kernel's effects, and that copy logged in committed jbd2 transactions (3G-R11).
// needs_recovery stays clear unless a test says otherwise.

var (
	rjKernelUnlink    = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true}
	rjKernelUnlinkMap = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ZeroBlockPointers: true}
)

const rjPath = "/gone.bin"

// rjCtx is the image as it is before the file is unlinked.
type rjCtx struct {
	img []byte
	l   *ext4test.Layout
	loc ext4test.FileLoc
}

func (c *rjCtx) itable() uint64 {
	blk, _ := c.l.InodeTableBlock(c.loc.Inode)
	return uint64(blk)
}

// copyOf is the inode-table block of the file's inode with its slot edited (the checksum recomputed).
func (c *rjCtx) copyOf(edit func(raw []byte)) []byte { return c.l.InodeCopy(c.img, c.loc.Inode, edit) }

// live is the inode as it was before the delete: linked, no dtime.
func (c *rjCtx) live(edit func(raw []byte)) []byte {
	return c.copyOf(func(raw []byte) {
		binary.LittleEndian.PutUint16(raw[0x1A:], 1)
		binary.LittleEndian.PutUint32(raw[0x14:], 0)
		if edit != nil {
			edit(raw)
		}
	})
}

func (c *rjCtx) tag(data []byte) ext4test.JTag { return ext4test.JTag{FSBlock: c.itable(), Data: data} }

// size returns an edit that sets i_size.
func rjSize(n int) func(raw []byte) {
	return func(raw []byte) {
		binary.LittleEndian.PutUint32(raw[0x4:], uint32(n))
		binary.LittleEndian.PutUint32(raw[0x6C:], 0)
	}
}

func rjGen(g uint32) func(raw []byte) {
	return func(raw []byte) { binary.LittleEndian.PutUint32(raw[0x64:], g) }
}

func rjTxn(seq uint32, tags ...ext4test.JTag) ext4test.JTxn {
	return ext4test.JTxn{Seq: seq, At: -1, Tags: tags}
}

// rjLive is a journal with a live region starting at block 1 and sequence 10.
func rjLive(txns ...ext4test.JTxn) ext4test.JBD2 {
	return ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10, Start: 1, Txns: txns}
}

// rjClean is a clean journal (s_start 0): what its transactions left is stale.
func rjClean(txns ...ext4test.JTxn) ext4test.JBD2 {
	return ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10 + uint32(len(txns)), Txns: txns}
}

type rjSpec struct {
	o       ext4test.Options
	files   []ext4test.File // default: one deleted file rjPath of 5 KiB + 300 bytes
	unlink  ext4test.UnlinkEffects
	jbd     func(c *rjCtx) ext4test.JBD2
	post    func(img []byte, l *ext4test.Layout) // after the journal is written
	unclean bool                                 // set needs_recovery
}

type rjImg struct {
	f    *ext4.FS
	img  []byte
	l    *ext4test.Layout
	data []byte
}

func rjBuild(t *testing.T, s rjSpec) rjImg {
	t.Helper()
	if s.o.JournalBlocks == 0 {
		s.o = jtOpts(128)
	}
	data := pattern(5*1024+300, 1)
	files := s.files
	if files == nil {
		files = []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7}}
	} else {
		data = files[0].Data
	}
	if s.unlink == (ext4test.UnlinkEffects{}) {
		s.unlink = rjKernelUnlink
	}
	img, l := ext4test.BuildLayout(s.o, files)
	c := &rjCtx{img: img, l: l, loc: l.File(rjPath)}
	var j ext4test.JBD2
	if s.jbd != nil {
		j = s.jbd(c) // the copies are taken before the unlink
	}
	l.Unlink(img, rjPath, s.unlink)
	if s.jbd != nil {
		j.UUID = jtUUID
		l.WriteJournal(img, j)
	}
	if s.unclean {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	if s.post != nil {
		s.post(img, l)
	}
	return rjImg{f: openImg(t, img), img: img, l: l, data: data}
}

// prep prepares the leaf source and the journal source.
func (r rjImg) prep(t *testing.T, use filesys.JournalUse) {
	t.Helper()
	if err := r.f.PrepareRecovery(context.Background()); err != nil {
		t.Fatalf("PrepareRecovery: %v", err)
	}
	if err := r.f.PrepareJournalRecovery(context.Background(), use); err != nil {
		t.Fatalf("PrepareJournalRecovery(%s): %v", use, err)
	}
}

func (r rjImg) recover(t *testing.T) ([]filesys.Candidate, error) {
	t.Helper()
	return r.f.Recoverable(delEntry(t, r.f, rjPath))
}

func rjJournalOf(cs []filesys.Candidate) []filesys.Candidate {
	var out []filesys.Candidate
	for _, c := range cs {
		if c.Method == ext4.MethodJournalInode {
			out = append(out, c)
		}
	}
	return out
}

// one prepares with use and returns the one journal candidate.
func (r rjImg) one(t *testing.T, use filesys.JournalUse) filesys.Candidate {
	t.Helper()
	r.prep(t, use)
	cs, err := r.recover(t)
	jc := rjJournalOf(cs)
	if err != nil || len(jc) != 1 {
		t.Fatalf("Recoverable = %d candidates (%d journal), %v; want one journal candidate", len(cs), len(jc), err)
	}
	return jc[0]
}

// none prepares with use and requires no candidate and no error.
func (r rjImg) none(t *testing.T, use filesys.JournalUse) {
	t.Helper()
	r.prep(t, use)
	cs, err := r.recover(t)
	if err != nil || len(cs) != 0 {
		t.Fatalf("Recoverable = %d candidates (%+v), %v; want none", len(cs), cs, err)
	}
}

// undecided prepares with use and requires ErrUndecided naming the rejection (3G C12): the only journal copy was
// found and rejected, which is unknown, never "not found".
func (r rjImg) undecided(t *testing.T, use filesys.JournalUse, reason string) {
	t.Helper()
	r.prep(t, use)
	cs, err := r.recover(t)
	if len(cs) != 0 || !errors.Is(err, filesys.ErrUndecided) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Recoverable = %+v, %v; want ErrUndecided naming %q and no candidate", cs, err, reason)
	}
}

func rjHas(ss []string, want string) bool { return slices.Contains(ss, want) }

func rjHasPrefix(ss []string, prefix string) bool {
	return slices.ContainsFunc(ss, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func TestJournalCandidateSmallFile(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	c := r.one(t, filesys.JournalUseCommitted)
	if c.Method != ext4.MethodJournalInode || c.Size != int64(len(r.data)) {
		t.Fatalf("method %q size %d; want %q and %d", c.Method, c.Size, ext4.MethodJournalInode, len(r.data))
	}
	if !bytes.Equal(concatRuns(r.img, c.Runs), r.data) {
		t.Error("the runs do not reproduce the file")
	}
	if want := (&filesys.CandidateJournal{Seq: 10, Committed: true, Region: "live", Revoked: false}); !reflect.DeepEqual(c.Journal, want) {
		t.Errorf("Journal = %+v; want %+v", c.Journal, want)
	}
	loc := r.l.File(rjPath)
	itable, _ := r.l.InodeTableBlock(loc.Inode)
	for _, b := range []string{
		fmt.Sprintf("inode=%d", loc.Inode), "generation=7", "journal:seq=10", "journal:region=live", "journal:jblk=2",
		fmt.Sprintf("journal:fsblk=%d", itable), "map=extents",
	} {
		if !rjHas(c.Basis, b) {
			t.Errorf("basis %q lacks %q", c.Basis, b)
		}
	}
	for _, a := range []string{"inode-copy-from-journal", "journal-copy-may-predate-last-change", "size-from-journal-copy", "inode-not-reused-since-entry-deleted"} {
		if !rjHas(c.Assumptions, a) {
			t.Errorf("assumptions %q lack %q", c.Assumptions, a)
		}
	}
	for _, a := range []string{"journal-stale-region", "journal-copy-unverified", "jbd2-unescaped", "journal-scan-incomplete", "journal-not-replayed"} {
		if rjHas(c.Assumptions, a) {
			t.Errorf("assumptions %q hold %q on a plain committed live copy", c.Assumptions, a)
		}
	}
	if e := delEntry(t, r.f, rjPath); c.Times.Deleted.T.IsZero() || !c.Times.Deleted.T.Equal(e.Times.Deleted.T) {
		t.Errorf("Times.Deleted = %v; want the on-disk deletion time %v", c.Times.Deleted.T, e.Times.Deleted.T)
	}
	if _, err := filesys.CheckCandidate(c, r.f.Info().Size); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
}

func TestJournalCandidateBlockMap(t *testing.T) {
	o := jtOpts(128)
	o.Extents, o.MetadataCsum = false, false // ext3 style
	data := pattern(20*1024, 3)
	r := rjBuild(t, rjSpec{
		o: o, files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true}}, unlink: rjKernelUnlinkMap,
		jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) },
	})
	c := r.one(t, filesys.JournalUseCommitted)
	if !bytes.Equal(concatRuns(r.img, c.Runs), data) || c.Size != int64(len(data)) {
		t.Fatalf("size %d, runs reproduce the file: %v; want the 20 blocks", c.Size, bytes.Equal(concatRuns(r.img, c.Runs), data))
	}
	loc := r.l.File(rjPath)
	if len(loc.Tree) != 1 || !rjHas(c.Basis, "map=blockmap") || !rjHas(c.Basis, fmt.Sprintf("tree-block=%d@home", loc.Tree[0])) {
		t.Errorf("tree %v, basis %q; want map=blockmap and the indirect block read at home", loc.Tree, c.Basis)
	}
}

// answerOf is the answer of Recoverable as a comparable value.
func answerOf(cs []filesys.Candidate, err error) string { return fmt.Sprintf("%+v | %v", cs, err) }

// PASSES ON ARRIVAL (a guard). Mutation: consult the journal without PrepareJournalRecovery.
func TestJournalCandidateNoPreparationIsUnchanged(t *testing.T) {
	spec := rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }}
	with := rjBuild(t, spec)
	if err := with.f.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs, err := with.recover(t)
	if len(cs) != 0 || err != nil {
		t.Fatalf("an unprepared journal source gave %+v, %v; want 3C's answer, nothing", cs, err)
	}
	// And the 3C answer with no journal at all.
	plain := rjBuild(t, rjSpec{o: ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true}})
	if err := plain.f.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	pc, perr := plain.recover(t)
	if answerOf(cs, err) != answerOf(pc, perr) {
		t.Errorf("with a journal, unprepared: %s; without: %s", answerOf(cs, err), answerOf(pc, perr))
	}
	// Unprepared leaf source: the same holds for the undecided answer.
	a, b := rjBuild(t, spec), rjBuild(t, rjSpec{o: ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true}})
	ac, aerr := a.recover(t)
	bc, berr := b.recover(t)
	if answerOf(ac, aerr) != answerOf(bc, berr) || !errors.Is(aerr, filesys.ErrUndecided) {
		t.Errorf("unprepared: %s; without a journal: %s; want equal and undecided", answerOf(ac, aerr), answerOf(bc, berr))
	}
}

// PASSES ON ARRIVAL (a guard). Mutation: treat use none as committed.
func TestJournalCandidateUseNoneIsUnchanged(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseNone)
	cs, err := r.recover(t)
	if len(cs) != 0 || err != nil {
		t.Fatalf("use none gave %+v, %v; want nothing", cs, err)
	}
}

func TestJournalCandidateCommittedIgnoresStale(t *testing.T) {
	spec := rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjClean(rjTxn(10, c.tag(c.live(nil)))) }}
	rjBuild(t, spec).none(t, filesys.JournalUseCommitted)
	c := rjBuild(t, spec).one(t, filesys.JournalUseStale)
	if want := (&filesys.CandidateJournal{Seq: 10, Committed: true, Region: "stale"}); !reflect.DeepEqual(c.Journal, want) {
		t.Errorf("Journal = %+v; want %+v", c.Journal, want)
	}
	if !rjHas(c.Assumptions, "journal-stale-region") || !rjHas(c.Basis, "journal:region=stale") {
		t.Errorf("assumptions %q, basis %q; want journal-stale-region and region=stale", c.Assumptions, c.Basis)
	}
}

func TestJournalCandidateSkipsOtherGeneration(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
		return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, c.tag(c.live(func(raw []byte) { rjGen(8)(raw); rjSize(1024)(raw) }))))
	}})
	c := r.one(t, filesys.JournalUseCommitted)
	if c.Journal.Seq != 10 || c.Size != int64(len(r.data)) {
		t.Errorf("seq %d size %d; want the copy of the entry's generation: 10 and %d", c.Journal.Seq, c.Size, len(r.data))
	}
	if !rjHas(c.Basis, "journal-copies-other-generation=1") {
		t.Errorf("basis %q lacks journal-copies-other-generation=1", c.Basis)
	}
}

func TestJournalCandidateSkipsDeletedStateCopies(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
		deleted := c.copyOf(func(raw []byte) { // the rm transaction: links 0, dtime set, size and map cleared
			rjSize(0)(raw)
			clear(raw[0x28+12 : 0x28+60])
			binary.LittleEndian.PutUint16(raw[0x28+2:], 0)
		})
		return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, c.tag(deleted)))
	}})
	c := r.one(t, filesys.JournalUseCommitted)
	if c.Journal.Seq != 10 || c.Size != int64(len(r.data)) || !bytes.Equal(concatRuns(r.img, c.Runs), r.data) {
		t.Errorf("seq %d size %d; want the older live copy: 10 and %d", c.Journal.Seq, c.Size, len(r.data))
	}
}

func TestJournalCandidateNewestLiveCopyWins(t *testing.T) {
	data := pattern(16*1024, 4)
	r := rjBuild(t, rjSpec{
		files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7}},
		jbd: func(c *rjCtx) ext4test.JBD2 {
			return rjLive(rjTxn(10, c.tag(c.live(rjSize(8192)))), rjTxn(11, c.tag(c.live(rjSize(16384)))))
		},
	})
	c := r.one(t, filesys.JournalUseCommitted)
	if c.Journal.Seq != 11 || c.Size != 16384 || !bytes.Equal(concatRuns(r.img, c.Runs), data) {
		t.Errorf("seq %d size %d; want the newest live copy: 11 and 16384", c.Journal.Seq, c.Size)
	}
}

// Carry-over 2: one inode number carries several incarnations inside the live region itself (the probe saw inode 13 as
// x, then as y). The entry's generation picks its own; the newest copy of it wins by sequence.
func TestJournalCandidateIncarnationsInLiveRegion(t *testing.T) {
	data := pattern(16*1024, 5)
	r := rjBuild(t, rjSpec{
		files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7}},
		jbd: func(c *rjCtx) ext4test.JBD2 {
			deleted := c.copyOf(func(raw []byte) { rjSize(0)(raw) })
			return rjLive(
				rjTxn(10, c.tag(c.live(rjSize(8192)))),
				rjTxn(11, c.tag(c.live(rjSize(16384)))),
				rjTxn(12, c.tag(deleted)),
				rjTxn(13, c.tag(c.live(func(raw []byte) { rjGen(8)(raw); rjSize(2048)(raw) }))), // y took the inode
			)
		},
	})
	c := r.one(t, filesys.JournalUseCommitted)
	if c.Journal.Seq != 11 || c.Size != 16384 || !bytes.Equal(concatRuns(r.img, c.Runs), data) {
		t.Errorf("seq %d size %d; want x's newest live copy: 11 and 16384", c.Journal.Seq, c.Size)
	}
	if !rjHas(c.Basis, "journal-copies-other-generation=1") {
		t.Errorf("basis %q lacks journal-copies-other-generation=1", c.Basis)
	}
	// Selection is by sequence, not by position: two stale fragments, the older one logged later, and the newer wins.
	r2 := rjBuild(t, rjSpec{
		files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7}},
		jbd: func(c *rjCtx) ext4test.JBD2 {
			a, b := rjTxn(11, c.tag(c.live(rjSize(16384)))), rjTxn(10, c.tag(c.live(rjSize(8192))))
			a.At, b.At = 1, 10
			return ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 12, Txns: []ext4test.JTxn{a, b}}
		},
	})
	c2 := r2.one(t, filesys.JournalUseStale)
	if c2.Journal.Seq != 11 || c2.Size != 16384 {
		t.Errorf("seq %d size %d; want the newest sequence: 11 and 16384", c2.Journal.Seq, c2.Size)
	}
}

// Each negative test below first builds its control, the same journal without the disqualifying feature, and requires a
// candidate, so a test that passes is not passing for an unrelated reason.
func TestJournalCandidateTruncatedLiveCopyIsNoCandidate(t *testing.T) {
	data := pattern(16*1024, 6)
	build := func(truncate bool) rjImg {
		return rjBuild(t, rjSpec{
			files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7}},
			jbd: func(c *rjCtx) ext4test.JBD2 {
				if !truncate {
					return rjLive(rjTxn(10, c.tag(c.live(nil))))
				}
				return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, c.tag(c.live(rjSize(0)))))
			},
		})
	}
	build(false).one(t, filesys.JournalUseCommitted)
	build(true).none(t, filesys.JournalUseCommitted)
}

func TestJournalCandidateRevokedCopyIsNoSource(t *testing.T) {
	build := func(revoke bool) rjImg {
		return rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
			if !revoke {
				return rjLive(rjTxn(10, c.tag(c.live(nil))))
			}
			rv := ext4test.JTxn{Seq: 11, At: -1, Revokes: []uint64{c.itable()}}
			return rjLive(rjTxn(10, c.tag(c.live(nil))), rv)
		}})
	}
	build(false).one(t, filesys.JournalUseCommitted)
	build(true).none(t, filesys.JournalUseCommitted)
}

func TestJournalCandidateMismatchCopyIsNoSource(t *testing.T) {
	build := func(bad bool) rjImg {
		return rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
			tg := c.tag(c.live(nil))
			tg.BadCsum = bad
			return rjLive(rjTxn(10, tg))
		}})
	}
	build(false).one(t, filesys.JournalUseCommitted)
	build(true).none(t, filesys.JournalUseCommitted)
}

func TestJournalCandidateUncommittedIsNoSource(t *testing.T) {
	build := func(uncommitted bool) rjImg {
		return rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
			tx := rjTxn(10, c.tag(c.live(nil)))
			tx.NoCommit = uncommitted
			return rjLive(tx)
		}})
	}
	build(false).one(t, filesys.JournalUseCommitted)
	build(true).none(t, filesys.JournalUseCommitted)
}

// A tag naming a data block whose bytes look like an inode table is never decoded: the identity of a copy comes from
// the block number, never from its content.
func TestJournalCandidateOnlyFromInodeTableBlocks(t *testing.T) {
	build := func(dataBlock bool) rjImg {
		return rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
			blk := c.itable()
			if dataBlock {
				blk = uint64(c.loc.Data[0])
			}
			return rjLive(rjTxn(10, ext4test.JTag{FSBlock: blk, Data: c.live(nil)}))
		}})
	}
	build(false).one(t, filesys.JournalUseCommitted)
	build(true).none(t, filesys.JournalUseCommitted)
}

func TestJournalCandidateCopyChecksumBad(t *testing.T) {
	bad := func(c *rjCtx) []byte {
		b := c.live(nil)
		_, off := c.l.InodeTableBlock(c.loc.Inode)
		b[off+0x7C] ^= 0xFF
		return b
	}
	rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(bad(c)))) }}).undecided(t, filesys.JournalUseCommitted, "checksum")

	// With an intact candidate, the reason is a warning on it.
	r := rjBuild(t, rjSpec{unlink: ext4test.DebugfsRm, jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(bad(c)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodInodeIntact {
		t.Fatalf("%d candidates, %v; want the intact one", len(cs), err)
	}
	if !hasSub(cs[0].Warnings, "journal copy") || !hasSub(cs[0].Warnings, "checksum") {
		t.Errorf("warnings %q do not say the journal copy failed its checksum", cs[0].Warnings)
	}
}

func TestJournalCandidateTypeMismatch(t *testing.T) {
	symlink := func(raw []byte) { binary.LittleEndian.PutUint16(raw[0:], 0xA1FF) } // the entry recorded a regular file
	rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
		return rjLive(rjTxn(10, c.tag(c.live(symlink))))
	}}).undecided(t, filesys.JournalUseCommitted, "file type")
	r := rjBuild(t, rjSpec{unlink: ext4test.DebugfsRm, jbd: func(c *rjCtx) ext4test.JBD2 {
		return rjLive(rjTxn(10, c.tag(c.live(symlink))))
	}})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	if err != nil || len(cs) != 1 || !hasSub(cs[0].Warnings, "file type") {
		t.Fatalf("%+v, %v; want the intact candidate with a file type warning", cs, err)
	}
}

func TestJournalCandidateUnescapes(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
		b := c.live(nil)
		copy(b[:4], []byte{0xC0, 0x3B, 0x39, 0x98}) // another inode's first word looks like the jbd2 magic: the block is escaped
		return rjLive(rjTxn(10, c.tag(b)))
	}})
	if txns, _, err := r.f.IndexTxns(); err != nil || len(txns) == 0 || len(txns[0].Tags) == 0 || !txns[0].Tags[0].Escaped {
		t.Fatalf("the builder did not escape the block: %+v, %v", txns, err)
	}
	c := r.one(t, filesys.JournalUseCommitted)
	if !rjHas(c.Basis, "unescaped") || !rjHas(c.Assumptions, "jbd2-unescaped") || !bytes.Equal(concatRuns(r.img, c.Runs), r.data) {
		t.Errorf("basis %q, assumptions %q; want unescaped and jbd2-unescaped", c.Basis, c.Assumptions)
	}
}

func TestJournalCandidateUnusableJournalIsUndecided(t *testing.T) {
	corrupt := func(img []byte, l *ext4test.Layout) { jtPatchSuper(img, l, func(sb []byte) { sb[0] ^= 0xFF }) }
	r := rjBuild(t, rjSpec{post: corrupt, jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	if len(cs) != 0 || !errors.Is(err, filesys.ErrUndecided) || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("%+v, %v; want ErrUndecided naming the journal and no candidate", cs, err)
	}
	// With another candidate the entry is answered, and the candidate carries the warning.
	r = rjBuild(t, rjSpec{post: corrupt, unlink: ext4test.DebugfsRm, jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err = r.recover(t)
	if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodInodeIntact || !rjHasPrefix(cs[0].Warnings, "journal not searched: ") {
		t.Fatalf("%+v, %v; want the intact candidate with the warning \"journal not searched: ...\"", cs, err)
	}
}

func TestJournalCandidateIncompleteJournalNoCopyIsIncomplete(t *testing.T) {
	fc := func(txns ...ext4test.JTxn) ext4test.JBD2 {
		j := rjLive(txns...)
		j.FastCommit, j.NumFC = true, 8 // the fast-commit area is not analysed: the journal result is incomplete
		return j
	}
	r := rjBuild(t, rjSpec{jbd: func(*rjCtx) ext4test.JBD2 { return fc(rjTxn(10, ext4test.JTag{FSBlock: 5, Data: pattern(1024, 1)})) }})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	if len(cs) != 0 || !errors.Is(err, filesys.ErrIncomplete) {
		t.Fatalf("%+v, %v; want ErrIncomplete and no candidate", cs, err)
	}
	// A copy found anyway is a candidate that says the scan was incomplete.
	r = rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return fc(rjTxn(10, c.tag(c.live(nil)))) }})
	c := r.one(t, filesys.JournalUseCommitted)
	if !rjHas(c.Assumptions, "journal-scan-incomplete") {
		t.Errorf("assumptions %q lack journal-scan-incomplete", c.Assumptions)
	}
}

func TestJournalCandidateUncleanImageAssumesNotReplayed(t *testing.T) {
	r := rjBuild(t, rjSpec{unclean: true, jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	c := r.one(t, filesys.JournalUseCommitted)
	if !rjHas(c.Assumptions, "journal-not-replayed") {
		t.Errorf("assumptions %q lack journal-not-replayed on a needs_recovery image", c.Assumptions)
	}
}

func TestJournalCandidateAfterIntactAndLeaf(t *testing.T) {
	data := pattern(5*1024+300, 7)
	files := []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7, ExtentLeaves: 1}}
	// A debugfs-style delete leaves the map: the intact source, the leaf source and the journal all answer.
	r := rjBuild(t, rjSpec{files: files, unlink: ext4test.DebugfsRm, jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	var got []string
	for _, c := range cs {
		got = append(got, c.Method)
	}
	if want := []string{ext4.MethodInodeIntact, ext4.MethodExtentLeaf, ext4.MethodJournalInode}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("methods %q, %v; want %q", got, err, want)
	}

	// An unprepared leaf source is unknown; a journal candidate makes the entry answered, with the leaf warning on it.
	r = rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	if err := r.f.PrepareJournalRecovery(context.Background(), filesys.JournalUseCommitted); err != nil {
		t.Fatal(err)
	}
	cs, err = r.recover(t)
	if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodJournalInode || !hasSub(cs[0].Warnings, "stale extent leaves not searched") {
		t.Fatalf("%+v, %v; want the journal candidate carrying the leaf warning", cs, err)
	}
}

func TestJournalCandidateAddsNoFilesystemWarning(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	e := delEntry(t, r.f, rjPath)
	before := slices.Clone(r.f.Info().Warnings)
	cs, err := r.f.Recoverable(e)
	if err != nil || len(rjJournalOf(cs)) != 1 {
		t.Fatalf("%+v, %v", cs, err)
	}
	if after := r.f.Info().Warnings; !slices.Equal(before, after) {
		t.Errorf("filesystem warnings changed: %q -> %q", before, after)
	}
}

func TestPrepareJournalRecoveryHonoursCancel(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.f.PrepareJournalRecovery(ctx, filesys.JournalUseCommitted); !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareJournalRecovery on a cancelled context = %v; want context.Canceled", err)
	}
	if builds, blocks := r.f.JournalStats(); builds != 0 || blocks != 0 {
		t.Errorf("a cancelled preparation built the journal index (%d builds, %d blocks read)", builds, blocks)
	}
	// Nothing was prepared: the journal is not consulted.
	if err := r.f.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cs, err := r.recover(t); len(cs) != 0 || err != nil {
		t.Errorf("after a cancelled preparation Recoverable = %+v, %v; want 3C's answer", cs, err)
	}
}

func TestPrepareJournalRecoveryIsIdempotent(t *testing.T) {
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(nil)))) }})
	r.prep(t, filesys.JournalUseCommitted)
	first, err := r.recover(t)
	if err != nil || len(first) != 1 {
		t.Fatalf("%+v, %v", first, err)
	}
	b1, n1 := r.f.JournalStats()
	if err := r.f.PrepareJournalRecovery(context.Background(), filesys.JournalUseCommitted); err != nil {
		t.Fatal(err)
	}
	if b2, n2 := r.f.JournalStats(); b2 != b1 || n2 != n1 {
		t.Errorf("a second preparation relocated or reread the journal: %d builds, %d blocks -> %d, %d", b1, n1, b2, n2)
	}
	second, err := r.recover(t)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Errorf("answers differ after a second preparation: %+v vs %+v (%v)", first, second, err)
	}
	// A different use reuses the index and takes effect.
	if err := r.f.PrepareJournalRecovery(context.Background(), filesys.JournalUseStale); err != nil {
		t.Fatal(err)
	}
	if b3, _ := r.f.JournalStats(); b3 != b1 {
		t.Errorf("a changed use rebuilt the journal (%d builds)", b3)
	}
	if err := r.f.PrepareJournalRecovery(context.Background(), "bogus"); err == nil {
		t.Error("PrepareJournalRecovery accepted an unknown use")
	}
	if err := r.f.PrepareJournalRecovery(context.Background(), filesys.JournalUseNone); err != nil {
		t.Fatal(err)
	}
	if cs, err := r.recover(t); len(cs) != 0 || err != nil {
		t.Errorf("after use none Recoverable = %+v, %v; want 3C's answer", cs, err)
	}
}

func TestJournalCandidateTreeBlockFromJournal(t *testing.T) {
	data := pattern(5*1024+300, 8)
	// The leaf is logged at 10 and the inode at 11. After the delete the leaf block is free and a later file wrote its
	// own leaf over it, so the home bytes are not this file's: the map is complete only through the journal copy.
	r := rjBuild(t, rjSpec{
		files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7, ExtentLeaves: 1}},
		jbd: func(c *rjCtx) ext4test.JBD2 {
			leaf := ext4test.JTag{FSBlock: uint64(c.loc.Tree[0]), Data: c.l.BlockCopy(c.img, c.loc.Tree[0])}
			c.l.WriteLeaf(c.img, c.loc.Tree[0], c.loc.Inode+100, 1, nil, true)
			return rjLive(rjTxn(10, leaf), rjTxn(11, c.tag(c.live(nil))))
		},
	})
	c := r.one(t, filesys.JournalUseCommitted)
	loc := r.l.File(rjPath)
	if want := fmt.Sprintf("tree-block=%d@journal:10", loc.Tree[0]); !rjHas(c.Basis, want) || !bytes.Equal(concatRuns(r.img, c.Runs), data) || stopOf(c) != "" {
		t.Errorf("basis %q stop %q; want %q and the full map", c.Basis, stopOf(c), want)
	}
	if _, err := filesys.CheckCandidate(c, r.f.Info().Size); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
}

func TestJournalCandidateTreeBlockNewerThanCopyIsAShortMap(t *testing.T) {
	data := pattern(5*1024+300, 9)
	r := rjBuild(t, rjSpec{
		files: []ext4test.File{{Path: rjPath, Data: data, Deleted: true, Generation: 7, ExtentLeaves: 1}},
		jbd: func(c *rjCtx) ext4test.JBD2 {
			leaf := ext4test.JTag{FSBlock: uint64(c.loc.Tree[0]), Data: c.l.BlockCopy(c.img, c.loc.Tree[0])}
			return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, leaf)) // the leaf was logged after the inode copy
		},
	})
	c := r.one(t, filesys.JournalUseCommitted)
	if stopOf(c) != "map-block-newer-than-copy@0" || len(c.Runs) != 0 || c.Size != int64(len(data)) {
		t.Errorf("stop %q, %d runs, size %d; want map-block-newer-than-copy@0, no run, size %d", stopOf(c), len(c.Runs), c.Size, len(data))
	}
	if _, err := filesys.CheckCandidate(c, r.f.Info().Size); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
}

// The copies one entry examines are bounded; past the bound the journal source is unknown, never "no candidate".
func TestJournalCandidateCopyCapIsUnknown(t *testing.T) {
	build := func() rjImg {
		return rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
			other := func(g uint32) []byte { return c.live(rjGen(g)) }
			return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, c.tag(other(8))), rjTxn(12, c.tag(other(9))))
		}})
	}
	r := build()
	if c := r.one(t, filesys.JournalUseCommitted); c.Journal.Seq != 10 {
		t.Fatalf("seq %d; want 10", c.Journal.Seq)
	}
	r = build()
	r.f.SetJournalCopyCap(2)
	r.prep(t, filesys.JournalUseCommitted)
	cs, err := r.recover(t)
	if len(cs) != 0 || !errors.Is(err, filesys.ErrLimit) {
		t.Errorf("%+v, %v; want ErrLimit and no candidate", cs, err)
	}
}

// The only journal copy was rejected (3G C12): the answer is undecided with the reason, not nil, nil.
func TestJournalCandidateOnlyCopyRejectedIsUndecided(t *testing.T) {
	notRegular := func(raw []byte) { binary.LittleEndian.PutUint16(raw[0:], 0x41ED) } // a directory
	r := rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 { return rjLive(rjTxn(10, c.tag(c.live(notRegular)))) }})
	r.undecided(t, filesys.JournalUseCommitted, "journal copy of inode")
	// An older intact copy is not reached around the rejected newest one: the newest live-state copy decides.
	r = rjBuild(t, rjSpec{jbd: func(c *rjCtx) ext4test.JBD2 {
		return rjLive(rjTxn(10, c.tag(c.live(nil))), rjTxn(11, c.tag(c.live(notRegular))))
	}})
	r.undecided(t, filesys.JournalUseCommitted, "sequence 11")
}
