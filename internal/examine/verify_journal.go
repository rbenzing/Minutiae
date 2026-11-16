package examine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/volume"
)

// journalMaxProblems is how many R13 problems are listed; the rest is one line.
const journalMaxProblems = 50

// methodJournalInode is the method of a file recovered from an inode copy in the ext4 journal (3G-R11).
const methodJournalInode = "ext4-journal-inode"

// Basis keys of an ext4 journal block copy (copyJBD2Block).
const (
	jbBasisAllocBasis = "alloc-basis=journal-inode"
	jbAssumeUnconfirm = "bitmap-unconfirmed"
)

// JournalCheck is R13 (3G-R18): every ext4 journal artifact is recomputed from the image with the reader that wrote
// it, never trusted from its record. A report is rendered again (same limit and stale setting, from its recorded
// parameters) and its SHA-256, size and incomplete flag compared; a block copy must be a tag of a fresh transaction
// with the recorded sequence, region, commit state and revoke state, at the fresh log slot, with the unknown-bitmap
// marker (ruling C17) exactly when a fresh view says the slot's bytes are unknown and no byte of the slot in free
// space (ruling C15); a file recovered from an inode copy must be a journal candidate of a fresh run of the recorded
// journal use with the recorded sequence and region, its size the candidate's (or declared) and its runs a prefix of
// the candidate's. The bytes of block copies and files are reproduced by R6, and the runs of files lie in free space
// by R9. drivers are the filesystem drivers (nil = detect.Drivers). No option disables it. The reports it rendered
// again are passed to MarkJournalReportsChecked; one it could not render keeps the notice.
func JournalCheck(drivers []detect.Driver) evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		verifyJournal(ctx, c, recs, rep, drivers)
	}
}

// jrRole is what R13 recomputes an artifact as.
type jrRole int

const (
	jrNone jrRole = iota
	jrReport
	jrBlock
	jrFile
	jrCrafted // an ext4-journal method on a kind and class that never carry it
)

func jrRoleOf(rec evidence.ManifestRecord) jrRole {
	d := rec.Source.Derived
	if d == nil || d.Recovery == nil || !strings.HasPrefix(d.Recovery.Method, evidence.Ext4JournalMethod) {
		return jrNone
	}
	rv := d.Recovery
	switch kind := rec.Source.Kind; {
	case kind == evidence.KindReport && rv.Class == evidence.ClassJournalReport && rv.Method == evidence.Ext4JournalMethod:
		return jrReport
	case kind == evidence.KindJournal && rv.Class == evidence.ClassJournalBlock && rv.Method == evidence.Ext4JournalMethod:
		return jrBlock
	case kind == evidence.KindRecover && rv.Class == evidence.ClassDeletedFile && rv.Method == methodJournalInode:
		return jrFile
	case kind == evidence.KindReport || kind == evidence.KindJournal || kind == evidence.KindRecover:
		return jrCrafted
	}
	return jrNone
}

type jrChecker struct {
	ctx     context.Context
	c       *evidence.Case
	rep     *evidence.VerifyReport
	byID    map[string]evidence.ManifestRecord
	listed  int
	dropped int
	marked  []string
	add     func(n int64)
}

func (k *jrChecker) problem(rec evidence.ManifestRecord, format string, args ...any) {
	if k.listed >= journalMaxProblems {
		k.dropped++
		return
	}
	k.listed++
	k.rep.AddProblem("journal: %s (artifact %q, %q)", fmt.Sprintf(format, args...), rec.ID, rec.Path)
}

func verifyJournal(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport, drivers []detect.Driver) {
	k := &jrChecker{ctx: ctx, c: c, rep: rep, byID: make(map[string]evidence.ManifestRecord, len(recs)), add: func(int64) {}}
	groups := map[rfGroup][]evidence.ManifestRecord{}
	var total int64
	for _, rec := range recs {
		if _, dup := k.byID[rec.ID]; dup {
			continue // a duplicate id is reported once by the manifest checks
		}
		k.byID[rec.ID] = rec
		switch jrRoleOf(rec) {
		case jrNone:
			continue
		case jrCrafted:
			rv := rec.Source.Derived.Recovery
			k.problem(rec, "kind %q, class %q and method %q are not an ext4 journal artifact this build writes, so it is not recomputed", rec.Source.Kind, rv.Class, rv.Method)
			continue
		}
		d := rec.Source.Derived
		g := rfGroup{parent: d.ParentID, partition: d.Partition, offset: d.PartitionOffset, fsType: d.FSType}
		groups[g] = append(groups[g], rec)
		total++
	}
	done, add := c.VerifyPhase("journal", total, "artifacts")
	defer done()
	k.add = add
	keys := make([]rfGroup, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, cmpRFGroup)
	var (
		sess   *Session
		sessID string
		sessEr error
	)
	closeSess := func() {
		if sess != nil {
			_ = sess.Close()
			sess = nil
		}
	}
	defer closeSess()
	for _, g := range keys {
		if ctx.Err() != nil {
			break
		}
		members := groups[g]
		slices.SortFunc(members, func(x, y evidence.ManifestRecord) int {
			if r := strings.Compare(x.Path, y.Path); r != 0 {
				return r
			}
			return strings.Compare(x.ID, y.ID)
		})
		if sessID != g.parent || (sess == nil && sessEr == nil) {
			closeSess()
			sess, sessEr = OpenContainer(c, g.parent, Options{Drivers: drivers})
			if sessEr == nil {
				if perr := sess.ReadPartitions(); errors.Is(perr, evidence.ErrIntegrity) {
					sessEr = perr // an integrity failure is never read as an unreadable table
					closeSess()
				}
			}
			sessID = g.parent
		}
		k.group(sess, sessEr, g, members)
	}
	if k.dropped > 0 {
		rep.AddProblem("journal: %d further problems are not listed", k.dropped)
	}
	rep.MarkJournalReportsChecked(k.marked)
}

// group rechecks the artifacts of one partition.
func (k *jrChecker) group(s *Session, openErr error, g rfGroup, members []evidence.ManifestRecord) {
	skip := func(format string, args ...any) {
		k.problem(members[0], format+" (%d artifact(s) not rechecked)", append(args, len(members))...)
		k.add(int64(len(members)))
	}
	if openErr == nil && s.Table == nil {
		openErr = s.tableErr
	}
	if openErr != nil {
		skip("parent %q cannot be opened: %v", g.parent, openErr)
		return
	}
	fsys, part, err := s.FS(g.partition)
	if err != nil {
		skip("partition %d of parent %q cannot be opened: %v", g.partition, g.parent, err)
		return
	}
	if part.Start != g.offset {
		skip("partition %d starts at %d, the artifact records %d", g.partition, part.Start, g.offset)
		return
	}
	if t := fsys.Info().Type; t != g.fsType {
		skip("the filesystem is %q, the artifact records %q", t, g.fsType)
		return
	}
	j, err := journalerOf(fsys)
	if err != nil {
		skip("the filesystem of partition %d has no journal analysis in this build", g.partition)
		return
	}
	info, err := j.Journal()
	if err != nil {
		skip("the journal of partition %d cannot be read: %v", g.partition, err)
		return
	}
	ch := &jrPart{k: k, s: s, fsys: fsys, part: part, j: j, info: info}
	for _, rec := range members {
		if k.ctx.Err() != nil {
			return
		}
		ok := false
		switch jrRoleOf(rec) {
		case jrReport:
			ok = ch.report(rec)
		case jrBlock:
			ok = ch.block(rec)
		case jrFile:
			ok = ch.file(rec)
		}
		if ok {
			k.rep.JournalRechecked++
		}
		k.add(1)
	}
}

// jbKey names a transaction the way a block copy's basis does.
type jbKey struct {
	seq    uint32
	region string
	start  int64
}

// jrPart is the freshly read journal of one partition.
type jrPart struct {
	k    *jrChecker
	s    *Session
	fsys filesys.FileSystem
	part volume.Partition
	j    filesys.Journaler
	info filesys.JournalInfo

	txns    map[jbKey]filesys.JournalTxn
	txnsErr error
	txnsSet bool

	free, unknown *freeMap
	freeErr       error
	freeSet       bool

	preparedUse filesys.JournalUse
}

// report renders the report again and compares its SHA-256, size and incomplete flag with the recorded ones. It
// reports whether the artifact was recomputed and equal; a rendering that was made (equal or not) is marked.
func (ch *jrPart) report(rec evidence.ManifestRecord) bool {
	k := ch.k
	p := rec.Source.Derived.Recovery.Params
	limit, err := strconv.ParseInt(p["max_bytes"], 10, 64)
	if err != nil || limit <= 0 {
		k.problem(rec, "report records no valid max_bytes parameter (%q), so it cannot be rendered again", p["max_bytes"])
		return false
	}
	if p["format"] != JBD2ReportFormat {
		k.problem(rec, "report records the format %q, this build renders %q, so it cannot be rendered again", p["format"], JBD2ReportFormat)
		return false
	}
	var noStale bool
	switch p["stale"] {
	case "on":
	case "off":
		noStale = true
	default:
		k.problem(rec, "report records the stale parameter %q (want on or off), so it cannot be rendered again", p["stale"])
		return false
	}
	sink := &hashSink{h: sha256.New()}
	info, _, cut, err := renderJBD2Report(sink, ch.part.Index, ch.j, noStale, limit)
	var planned *reportCut
	if err != nil && !errors.As(err, &planned) {
		k.problem(rec, "report cannot be rendered again: %v", err)
		return false
	}
	k.marked = append(k.marked, rec.ID)
	got := hex.EncodeToString(sink.h.Sum(nil))
	ok := true
	if got != rec.SHA256 || sink.n != rec.Size {
		k.problem(rec, "report differs from a fresh rendering (recorded %d bytes sha256 %s, rendered %d bytes sha256 %s)", rec.Size, rec.SHA256, sink.n, got)
		ok = false
	}
	if want := cut || info.Incomplete; rec.Incomplete != want {
		k.problem(rec, "report is recorded incomplete=%v, a fresh rendering is incomplete=%v", rec.Incomplete, want)
		ok = false
	}
	return ok
}

// transactions lists the fresh transactions once.
func (ch *jrPart) transactions() (map[jbKey]filesys.JournalTxn, error) {
	if !ch.txnsSet {
		ch.txnsSet = true
		ch.txns = map[jbKey]filesys.JournalTxn{}
		ch.txnsErr = ch.j.JournalTransactions(func(t filesys.JournalTxn) bool {
			ch.txns[jbKey{seq: t.Seq, region: t.Region, start: t.StartBlock}] = t
			return true
		})
	}
	return ch.txns, ch.txnsErr
}

// views returns the two views of the partition's free space a block copy is held to: the one that calls a byte of
// the log slot free or not (a slot is owned by the journal inode, so only free is a disagreement), and the one the
// recovery checks use, in which any non-free byte is unknown when the filesystem carries a warning.
func (ch *jrPart) views() (free, unknown *freeMap, err error) {
	if !ch.freeSet {
		ch.freeSet = true
		runs, _, uerr := ch.s.unallocatedRunsNote(ch.fsys, ch.part, false)
		if uerr != nil {
			ch.freeErr = fmt.Errorf("the unallocated space of partition %d cannot be listed: %w", ch.part.Index, uerr)
		} else {
			ch.free = newFreeMap(runs, false)
			ch.unknown = newFreeMap(runs, len(ch.fsys.Info().Warnings) > 0)
		}
	}
	return ch.free, ch.unknown, ch.freeErr
}

// basisValue is the value of the "key=" entry of a basis.
func basisValue(basis []string, key string) (string, bool) {
	for _, b := range basis {
		if v, ok := strings.CutPrefix(b, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// block checks one block copy against the fresh journal.
func (ch *jrPart) block(rec evidence.ManifestRecord) bool {
	k := ch.k
	rv := rec.Source.Derived.Recovery
	var (
		x        jbKey
		tag, fsb int64
		jblk     int64
	)
	parse := func(key string, bits int, dst *int64) bool {
		v, ok := basisValue(rv.Basis, key)
		if !ok {
			return false
		}
		n, err := strconv.ParseInt(v, 10, bits)
		*dst = n
		return err == nil
	}
	seq, start := int64(0), int64(0)
	region, regOK := basisValue(rv.Basis, "region")
	if !parse("jbd2:seq", 33, &seq) || !regOK || !parse("txn-start", 63, &start) || !parse("tag", 32, &tag) || !parse("fsblk", 63, &fsb) || !parse("jblk", 63, &jblk) || seq < 0 {
		k.problem(rec, "block copy's basis %v does not name a transaction, tag, filesystem block and journal block", rv.Basis)
		return false
	}
	x = jbKey{seq: uint32(seq), region: region, start: start}
	txns, err := ch.transactions()
	if err != nil {
		k.problem(rec, "the transactions of the journal cannot be listed: %v", err)
		return false
	}
	t, ok := txns[x]
	if !ok {
		k.problem(rec, "block copy's basis names no transaction of the journal now (sequence %d, %s region, start block %d)", x.seq, x.region, x.start)
		return false
	}
	if tag < 0 || tag >= int64(len(t.Blocks)) || int64(t.Blocks[tag].FSBlock) != fsb || int64(t.Blocks[tag].JournalBlock) != jblk {
		k.problem(rec, "block copy's basis names no tag of transaction %d (%s region, start block %d): tag %d, filesystem block %d, journal block %d", x.seq, x.region, x.start, tag, fsb, jblk)
		return false
	}
	g := t.Blocks[tag]
	good := true
	if r := rv.Journal; r == nil || r.Seq != t.Seq || r.Committed != t.Committed || r.Region != t.Region || r.Revoked != g.Revoked {
		k.problem(rec, "block copy's journal reference %+v differs from the fresh tag (sequence %d, %s region, committed %v, revoked %v)", r, t.Seq, t.Region, t.Committed, g.Revoked)
		good = false
	}
	if !copyUsable(g.Copy) || g.Copy == "mismatch" {
		k.problem(rec, "the logged copy of the tag is %q now, so a copy of it is not reproducible", g.Copy)
		return false
	}
	_, run, err := ch.j.JournalBlock(t, int(tag))
	if err != nil {
		k.problem(rec, "the logged copy of the tag cannot be read: %v", err)
		return false
	}
	off, ok := filesys.AddOK(ch.part.Start, run.Offset)
	if !ok || run.Length <= 0 {
		k.problem(rec, "the log slot of the tag has an image offset that overflows (%d + %d)", ch.part.Start, run.Offset)
		return false
	}
	runs, err := k.c.DerivedRuns(rec, k.byID)
	if err != nil {
		k.problem(rec, "runs unreadable: %v", err)
		return false
	}
	partial := rvHasPrefix(rv.Assumptions, "read-failed-after=")
	if len(runs) != 1 || runs[0].Offset != off || runs[0].Length <= 0 || runs[0].Length > run.Length || (runs[0].Length != run.Length && !partial) {
		if len(runs) == 0 && partial {
			return good // a copy that read nothing has no run to compare
		}
		k.problem(rec, "block copy's runs %v differ from the fresh log slot (image offset %d, %d bytes)", runs, off, run.Length)
		return false
	}
	free, unknown, err := ch.views()
	if err != nil {
		k.problem(rec, "%v", err)
		return false
	}
	if free.touchesFree(off, run.Length) {
		k.problem(rec, "the log slot at image offset %d lies in space the filesystem calls free although the journal inode owns it (a copy of it is not written)", off)
		good = false
	}
	wantMark := unknown.nonFree == "unknown"
	hasBasis := slices.Contains(rv.Basis, jbBasisAllocBasis)
	hasAssume := slices.Contains(rv.Assumptions, jbAssumeUnconfirm)
	if hasBasis != wantMark || hasAssume != wantMark {
		k.problem(rec, "block copy's %s marker (%s %v, %s %v) differs from a fresh view of the bitmap (slot bytes unknown: %v)", jbAssumeUnconfirm, jbBasisAllocBasis, hasBasis, jbAssumeUnconfirm, hasAssume, wantMark)
		good = false
	}
	return good
}

func rvHasPrefix(list []string, prefix string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// file checks one file recovered from an inode copy in the journal against a fresh run of the recoverer.
func (ch *jrPart) file(rec evidence.ManifestRecord) bool {
	k := ch.k
	d := rec.Source.Derived
	rv := d.Recovery
	use, err := filesys.ParseJournalUse(rv.Params["use_journal"])
	if err != nil || use == filesys.JournalUseNone {
		k.problem(rec, "file records the journal use %q (want committed or stale in its use_journal parameter), so it cannot be recovered again", rv.Params["use_journal"])
		return false
	}
	if rv.Journal == nil {
		k.problem(rec, "file records no journal reference")
		return false
	}
	if ch.preparedUse != use {
		if err := filesys.PrepareJournalRecovery(k.ctx, ch.fsys, use); err != nil {
			if k.ctx.Err() == nil {
				k.problem(rec, "the journal cannot be prepared for recovery with use %s: %v", use, err)
			}
			return false
		}
		ch.preparedUse = use
	}
	r, ok := filesys.As[filesys.Recoverer](ch.fsys)
	if !ok {
		k.problem(rec, "the filesystem of partition %d has no recovery in this build", ch.part.Index)
		return false
	}
	cands, err := r.Recoverable(filesys.Entry{ID: d.FSID})
	if err != nil {
		if k.ctx.Err() == nil {
			k.problem(rec, "%q cannot be recovered again from the journal now: %v", d.FSID, err)
		}
		return false
	}
	recorded, err := k.c.DerivedRuns(rec, k.byID)
	if err != nil {
		k.problem(rec, "runs unreadable: %v", err)
		return false
	}
	var (
		seen    []string
		matched bool
		reason  string
	)
	for _, cand := range cands {
		if cand.Method != methodJournalInode || cand.Journal == nil {
			continue
		}
		seen = append(seen, fmt.Sprintf("sequence %d, %s region", cand.Journal.Seq, cand.Journal.Region))
		if cand.Journal.Seq != rv.Journal.Seq || cand.Journal.Region != rv.Journal.Region {
			continue
		}
		matched = true
		if why := ch.fileFits(rec, recorded, cand); why != "" {
			if reason == "" {
				reason = why
			}
			continue
		}
		return true
	}
	switch {
	case !matched && len(seen) == 0:
		k.problem(rec, "the journal gives no candidate for %q now", d.FSID)
	case !matched:
		k.problem(rec, "recorded journal reference (sequence %d, %s region) is none of the journal candidates %q gives now (%s)", rv.Journal.Seq, rv.Journal.Region, d.FSID, strings.Join(seen, "; "))
	default:
		k.problem(rec, "%s", reason)
	}
	return false
}

// fileFits reports why the recorded size and runs are not those of the fresh candidate; "" when they are.
func (ch *jrPart) fileFits(rec evidence.ManifestRecord, recorded []evidence.Run, cand filesys.Candidate) string {
	d := rec.Source.Derived
	switch {
	case rec.Size > cand.Size:
		return fmt.Sprintf("recorded size %d exceeds the %d bytes the journal gives now for %q", rec.Size, cand.Size, d.FSID)
	case rec.Size < cand.Size && !slices.Contains(d.Recovery.Assumptions, fmt.Sprintf("size-declared=%d", cand.Size)):
		return fmt.Sprintf("recorded size %d is short of the %d bytes the journal gives now for %q and does not declare it (size-declared=%d)", rec.Size, cand.Size, d.FSID, cand.Size)
	}
	fresh, ok := freshRuns(ch.part.Start, cand.Runs)
	if !ok {
		return fmt.Sprintf("the map the journal gives now for %q has a run whose image offset overflows", d.FSID)
	}
	if !runsArePrefix(recorded, fresh) {
		return fmt.Sprintf("recorded runs are not a prefix of the map the journal gives now for %q", d.FSID)
	}
	return ""
}
