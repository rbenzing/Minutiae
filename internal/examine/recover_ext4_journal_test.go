package examine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// These images are the ext4 journal builders of internal/filesys/ext4: a file built Deleted, the inode-table block of
// its live state copied with InodeCopy before Unlink applies the kernel's effects, and that copy logged in a committed
// jbd2 transaction (3G-R11). The kernel's unlink leaves no intact map and no leaf, so the journal is the only source.

const jrnPath = "/gone.bin"

var (
	jrnUUID   = [16]byte{0xA1, 0xB2, 0xC3, 0xD4, 0xE5, 0xF6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	jrnKernel = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true}
)

// jrnImg is a built image and the file in it.
type jrnImg struct {
	img  []byte
	l    *ext4test.Layout
	data []byte
}

type jrnSpec struct {
	clean   bool                                 // the journal is clean (s_start 0): its transactions are stale
	unclean bool                                 // set needs_recovery
	post    func(img []byte, l *ext4test.Layout) // after the journal is written
	txns    func(c jrnCtx) []ext4test.JTxn       // the transactions; nil: a committed copy of the live inode at 10
	opts    func(o *ext4test.Options)            // edit the filesystem options
}

type jrnCtx struct {
	img []byte
	l   *ext4test.Layout
	loc ext4test.FileLoc
}

func (c jrnCtx) itable() uint64 {
	blk, _ := c.l.InodeTableBlock(c.loc.Inode)
	return uint64(blk)
}

// live is the inode as it was before the delete: linked, no dtime.
func (c jrnCtx) live() []byte {
	return c.l.InodeCopy(c.img, c.loc.Inode, func(raw []byte) {
		binary.LittleEndian.PutUint16(raw[0x1A:], 1)
		binary.LittleEndian.PutUint32(raw[0x14:], 0)
	})
}

func (c jrnCtx) tag(data []byte) ext4test.JTag { return ext4test.JTag{FSBlock: c.itable(), Data: data} }

func jrnTxn(seq uint32, tags ...ext4test.JTag) ext4test.JTxn {
	return ext4test.JTxn{Seq: seq, At: -1, Tags: tags}
}

func buildJrn(t *testing.T, s jrnSpec) jrnImg {
	t.Helper()
	o := ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: 128, BlocksPerGroup: 2048}
	if s.opts != nil {
		s.opts(&o)
	}
	data := pat(1, 5*1024+300)
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: jrnPath, Data: data, Deleted: true, Generation: 7}})
	c := jrnCtx{img: img, l: l, loc: l.File(jrnPath)}
	var txns []ext4test.JTxn
	if o.JournalBlocks > 0 {
		txns = []ext4test.JTxn{jrnTxn(10, c.tag(c.live()))}
		if s.txns != nil {
			txns = s.txns(c)
		}
	}
	l.Unlink(img, jrnPath, jrnKernel)
	if o.JournalBlocks > 0 {
		j := ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10, Start: 1, UUID: jrnUUID, Txns: txns}
		if s.clean {
			j.Sequence, j.Start = 10+uint32(len(txns)), 0
		}
		l.WriteJournal(img, j)
	}
	if s.unclean {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	if s.post != nil {
		s.post(img, l)
	}
	return jrnImg{img: img, l: l, data: data}
}

func recoverJournal(t *testing.T, s *examine.Session, use filesys.JournalUse) examine.RecoverSummary {
	t.Helper()
	sum, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: use})
	if err != nil {
		t.Fatalf("Recover(%q): %v", use, err)
	}
	return sum
}

func TestRecoverExt4JournalInodeSmallFile(t *testing.T) {
	j := buildJrn(t, jrnSpec{})
	before := sha256.Sum256(j.img)
	s, disc := imageSession(t, j.img)
	sum := recoverJournal(t, s, filesys.JournalUseCommitted)
	rec := only(t, sum.Artifacts)
	rv := recoveryOf(rec)
	if *rv.Confidence != 65 || rv.Class != evidence.ClassDeletedFile || rv.Method != "ext4-journal-inode" || rec.Incomplete {
		t.Fatalf("confidence %d class %q method %q incomplete %v; want 65, deleted-file, ext4-journal-inode, complete", *rv.Confidence, rv.Class, rv.Method, rec.Incomplete)
	}
	if want := (evidence.JournalRef{Seq: 10, Committed: true, Region: "live"}); rv.Journal == nil || *rv.Journal != want {
		t.Errorf("Journal = %+v; want %+v", rv.Journal, want)
	}
	if rv.Params["use_journal"] != "committed" {
		t.Errorf("Params[use_journal] = %q; want committed", rv.Params["use_journal"])
	}
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, j.data) {
		t.Error("the artifact is not the file")
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
	if sha256.Sum256(j.img) != before {
		t.Error("the source image changed")
	}
}

func TestRecoverExt4JournalStaleCapsAt40(t *testing.T) {
	j := buildJrn(t, jrnSpec{clean: true})
	s, disc := imageSession(t, j.img)
	rec := only(t, recoverJournal(t, s, filesys.JournalUseStale).Artifacts)
	rv := recoveryOf(rec)
	if *rv.Confidence != 40 || !hasAssumption(rv, "journal-stale-region") || rv.Journal == nil || rv.Journal.Region != "stale" {
		t.Fatalf("confidence %d assumptions %q journal %+v; want 40, journal-stale-region, region stale", *rv.Confidence, rv.Assumptions, rv.Journal)
	}
	if rv.Params["use_journal"] != "stale" {
		t.Errorf("Params[use_journal] = %q; want stale", rv.Params["use_journal"])
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)

	// With committed only, a stale copy is not a source.
	s2, _ := imageSession(t, j.img)
	if sum := recoverJournal(t, s2, filesys.JournalUseCommitted); len(sum.Artifacts) != 0 {
		t.Errorf("use committed wrote %d artifacts from a stale journal; want none", len(sum.Artifacts))
	}
}

func TestRecoverExt4JournalUncleanCapsAt40(t *testing.T) {
	j := buildJrn(t, jrnSpec{unclean: true})
	s, _ := imageSession(t, j.img)
	rv := recoveryOf(only(t, recoverJournal(t, s, filesys.JournalUseCommitted).Artifacts))
	if *rv.Confidence != 40 || !hasAssumption(rv, "journal-not-replayed") {
		t.Errorf("confidence %d assumptions %q; want 40 with journal-not-replayed", *rv.Confidence, rv.Assumptions)
	}
	verifyOK(t, s.Case)
}

func TestRecoverExt4JournalCutAt20(t *testing.T) {
	j := buildJrn(t, jrnSpec{post: func(img []byte, l *ext4test.Layout) {
		l.SetBlockFree(img, l.File(jrnPath).Data[3], false) // a data block a new file took
	}})
	s, disc := imageSession(t, j.img)
	rec := only(t, recoverJournal(t, s, filesys.JournalUseCommitted).Artifacts)
	rv := recoveryOf(rec)
	if !rec.Incomplete || *rv.Confidence != 20 || rv.Method != "ext4-journal-inode" {
		t.Fatalf("incomplete %v confidence %d method %q; want incomplete, 20, ext4-journal-inode", rec.Incomplete, *rv.Confidence, rv.Method)
	}
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, j.data[:3*1024]) {
		t.Errorf("the artifact is %d bytes; want the 3 free blocks before the reused one", len(got))
	}
	assertEqualsRuns(t, s.Case, rec, disc)
	verifyOK(t, s.Case)
}

// The default is 3C: "" and none give the same plan, and the audit says nothing of the journal.
func TestRecoverExt4JournalNoneIsUnchanged(t *testing.T) {
	img, _, _ := uncleanImage(t, false)
	plan := func(use filesys.JournalUse) *examine.RecoverPlan {
		s, _ := imageSession(t, img)
		p, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: use})
		if err != nil {
			t.Fatalf("PlanRecovery(%q): %v", use, err)
		}
		return p
	}
	if a, b := plan(""), plan(filesys.JournalUseNone); !reflect.DeepEqual(a, b) {
		t.Errorf("the plan with use none differs from the plan with no use:\n%+v\n%+v", a, b)
	}
	s, _ := imageSession(t, img)
	recoverJournal(t, s, filesys.JournalUseNone)
	start := auditByAction(t, s.Case, "analysis.start")
	end := auditByAction(t, s.Case, "analysis.end")
	if len(start) != 1 || len(end) != 1 {
		t.Fatalf("%d analysis.start and %d analysis.end entries; want one each", len(start), len(end))
	}
	if _, has := start[0].Details["use_journal"]; has {
		t.Errorf("analysis.start has use_journal with use none: %v", start[0].Details)
	}
	if _, has := end[0].Details["journal_candidates"]; has {
		t.Errorf("analysis.end has journal_candidates with use none: %v", end[0].Details)
	}
	recs, merr := s.Case.Manifest()
	if merr != nil {
		t.Fatal(merr)
	}
	for _, rec := range recs {
		if rv := recoveryOfOrNil(rec); rv != nil && (rv.Journal != nil || rv.Params["use_journal"] != "") {
			t.Errorf("artifact %s carries journal provenance with use none: %+v", rec.ID, rv)
		}
	}
}

func recoveryOfOrNil(rec evidence.ManifestRecord) *evidence.Recovery {
	if rec.Source.Derived == nil {
		return nil
	}
	return rec.Source.Derived.Recovery
}

func TestRecoverJournalOnFATIsUnavailable(t *testing.T) {
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: pattern(3*blk, 3), Deleted: true}})
	s, _ := imageSession(t, img)
	before := len(auditEntries(t, s.Case))
	_, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: filesys.JournalUseCommitted})
	if !errors.Is(err, examine.ErrNoJournalRecovery) || !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("Recover = %v; want examine.ErrNoJournalRecovery (wrapping ErrUnsupported)", err)
	}
	if !strings.Contains(err.Error(), "fat") {
		t.Errorf("%q does not name the filesystem", err)
	}
	if _, perr := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: filesys.JournalUseStale}); !errors.Is(perr, examine.ErrNoJournalRecovery) {
		t.Errorf("PlanRecovery = %v; want ErrNoJournalRecovery", perr)
	}
	if after := auditEntries(t, s.Case); len(after) != before {
		t.Errorf("%d audit entries were written before the refusal; want none", len(after)-before)
	}
}

func TestRecoverJournalBadValueRejected(t *testing.T) {
	j := buildJrn(t, jrnSpec{})
	s, _ := imageSession(t, j.img)
	before := len(auditEntries(t, s.Case))
	o := examine.RecoverOptions{Partition: -1, All: true, UseJournal: "everything"}
	if _, err := s.Recover(context.Background(), o); err == nil || !strings.Contains(err.Error(), "everything") {
		t.Errorf("Recover = %v; want a refusal naming the value", err)
	}
	if _, err := s.PlanRecovery(context.Background(), o); err == nil {
		t.Error("PlanRecovery accepted a bad use_journal")
	}
	if after := auditEntries(t, s.Case); len(after) != before {
		t.Errorf("%d audit entries were written; want none", len(after)-before)
	}
}

func TestRecoverJournalWithRollForwardRejected(t *testing.T) {
	j := buildJrn(t, jrnSpec{clean: true})
	s, _ := imageSession(t, j.img)
	before := len(auditEntries(t, s.Case))
	o := examine.RecoverOptions{Partition: -1, All: true, RollForward: true, UseJournal: filesys.JournalUseStale}
	_, err := s.Recover(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "roll-forward") || !strings.Contains(err.Error(), "journal") {
		t.Errorf("Recover = %v; want a refusal naming roll-forward and the journal", err)
	}
	if _, err := s.PlanRecovery(context.Background(), o); err == nil {
		t.Error("PlanRecovery accepted roll-forward together with use_journal")
	}
	if after := auditEntries(t, s.Case); len(after) != before {
		t.Errorf("%d audit entries were written; want none", len(after)-before)
	}
}

func TestRecoverJournalListPrepares(t *testing.T) {
	j := buildJrn(t, jrnSpec{clean: true})
	s, _ := imageSession(t, j.img)
	plan, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: filesys.JournalUseStale})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || len(plan.Items[0].Candidates) != 1 {
		t.Fatalf("plan %+v; want one item with one candidate", plan.Items)
	}
	pc := plan.Items[0].Candidates[0]
	if pc.Method != "ext4-journal-inode" || pc.Confidence != 40 || pc.Skip != "" || pc.Journal == nil || pc.Journal.Region != "stale" {
		t.Fatalf("candidate %+v; want ext4-journal-inode at 40 from the stale region", pc)
	}
	rec := only(t, recoverJournal(t, s, filesys.JournalUseStale).Artifacts)
	if rv := recoveryOf(rec); rv.Method != pc.Method || *rv.Confidence != pc.Confidence || rec.Size != pc.Size {
		t.Errorf("the run (%s, %d, %d bytes) differs from the list (%s, %d, %d bytes)", rv.Method, *rv.Confidence, rec.Size, pc.Method, pc.Confidence, pc.Size)
	}
	// Plain planning does not read the journal.
	s2, _ := imageSession(t, j.img)
	plain, err := s2.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plain.Items {
		if len(it.Candidates) != 0 {
			t.Errorf("a plan without use_journal holds candidates: %+v", it.Candidates)
		}
	}
}

func TestRecoverJournalUnusableIsUndecided(t *testing.T) {
	j := buildJrn(t, jrnSpec{post: func(img []byte, l *ext4test.Layout) {
		fs := l.JournalFSBlock(0)
		img[fs*1024] ^= 0xFF // the journal superblock magic
	}})
	s, _ := imageSession(t, j.img)
	sum := recoverJournal(t, s, filesys.JournalUseCommitted)
	if sum.SkippedBy["undecided"] != 1 || len(sum.Artifacts) != 0 {
		t.Errorf("SkippedBy %v, %d artifacts; want one undecided and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
}

func TestRecoverExt4JournalAuditDetails(t *testing.T) {
	j := buildJrn(t, jrnSpec{})
	s, _ := imageSession(t, j.img)
	recoverJournal(t, s, filesys.JournalUseCommitted)
	start := only(t, auditByAction(t, s.Case, "analysis.start"))
	end := only(t, auditByAction(t, s.Case, "analysis.end"))
	if start.Details["use_journal"] != "committed" {
		t.Errorf("analysis.start use_journal = %v; want committed", start.Details["use_journal"])
	}
	if got := fmt.Sprint(end.Details["journal_candidates"]); got != "1" {
		t.Errorf("analysis.end journal_candidates = %v; want 1", end.Details["journal_candidates"])
	}
}

func TestRecoverExt4JournalNeverModifiesSource(t *testing.T) {
	j := buildJrn(t, jrnSpec{unclean: true})
	before := slices.Clone(j.img)
	s, disc := imageSession(t, j.img)
	dbefore := slices.Clone(disc)
	recoverJournal(t, s, filesys.JournalUseStale)
	if !bytes.Equal(j.img, before) || !bytes.Equal(disc, dbefore) {
		t.Error("recovery with the journal changed the source image")
	}
	verifyOK(t, s.Case)
}

// A filesystem with no journal gets one plan-level answer, not one undecided answer per entry (3G C13). The refusal comes inside buildPlan,
// after analysis.start, on purpose: preparing the journal is a read-only act.
func TestRecoverJournalOnExt4WithoutJournalIsOneRefusal(t *testing.T) {
	j := buildJrn(t, jrnSpec{opts: func(o *ext4test.Options) { o.JournalBlocks = 0 }})
	s, _ := imageSession(t, j.img)
	sum, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: filesys.JournalUseCommitted})
	if !errors.Is(err, filesys.ErrNoJournal) || !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("Recover = %v; want one refusal wrapping filesys.ErrNoJournal", err)
	}
	if len(sum.Artifacts) != 0 || sum.SkippedBy["undecided"] != 0 {
		t.Errorf("%d artifacts, SkippedBy %v; want nothing written and no per-entry answers", len(sum.Artifacts), sum.SkippedBy)
	}
	if w := auditByAction(t, s.Case, "analysis.warning"); len(w) != 0 {
		t.Errorf("%d per-entry warnings were audited: %v", len(w), w)
	}
	if _, perr := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, UseJournal: filesys.JournalUseStale}); !errors.Is(perr, filesys.ErrNoJournal) {
		t.Errorf("PlanRecovery = %v; want filesys.ErrNoJournal", perr)
	}
	verifyOK(t, s.Case)
}
