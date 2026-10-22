package evidence

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The index pass of P11 (5D-14i): the leaf pages of each FTS table are decoded strictly (fts5seg.go) in
// the order of their _data rows, in one read transaction per table, and every posting found is folded into
// the digest of its audited batch (verify_ftsdigest.go). The digest of the leaf pages is then compared,
// bucket by bucket, with the digest of what the records produce (verify_ftsexpect.go); a bucket whose
// two sums differ is a failing bucket.
//
// Rows of _data: the averages record (id 1), the structure record (id 10), the leaf pages of each
// segment (id = segid<<37 + page, height 0) and the doclist-index pages (the same with bit 36 or a
// height). Only the leaf pages are decoded here; the doclist index belongs to 14j. A row of a segment the
// structure record does not list is the business of checkUnreferenced.
//
// The ftsSink contract (D147): the decoder reports postings while it decodes a page, before it knows that
// the page, or a later page of the segment, is valid. Everything a segment contributes (its postings, its
// delete markers) is therefore collected in the segment's own state and used only when its last page and
// Finish have succeeded. A segment that fails is one PROBLEM (`is not a valid leaf`) and contributes
// nothing, so the buckets its postings belong to fail and are looked at again.
//
// A segment the structure lists with pages 0 to 0 (Empty, D143) is not read, whatever rows of it exist: a
// genuine index never holds any, and the rows left behind by a tampered one cannot be reached by any search
// (they belong to no segment of the structure), so they can cause neither a hidden nor an invented hit. Their
// content is not verified (limitations.md, D149); checkUnreferenced still names them as unreferenced rows.

const (
	// ftsPagesPerTick is the number of leaf pages between two updates of the page counter and two looks at
	// the verify context.
	ftsPagesPerTick = 1024
	// ftsPageMask selects the part of a _data id below the segment id: height, doclist-index bit and page.
	ftsPageMask = int64(1)<<ftsSegidShift - 1
	// ftsLeafLimit is the first local id that is not a leaf page: the page number has 31 bits.
	ftsLeafLimit = int64(1) << 31
)

// ftsSegRun is the state of one segment while the pass reads it.
type ftsSegRun struct {
	seg        ftsSegment
	rd         *ftsSegReader
	dig        *ftsDigester // this segment's postings; merged into the table's on success
	failed     bool
	idx        *ftsIdxSeg // the recomputation of the _idx rows and doclist indexes of this segment (14j); nil when it has none
	leavesDone bool

	dels    []int64 // the first record ids of delete markers (listed), at most verifyMaxPerKind
	nDel    int
	delBkts map[ftsBucketKey]struct{}
}

// indexPass decodes every leaf page of table and folds the postings into r.found. It returns an error only
// for a failure to read the case (or the end of the verify context); a segment that does not decode is
// reported and skipped.
func (r *ftsVerify) indexPass(table string) error {
	ti := ftsTableIndex(table)
	var done func()
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		var structure []byte
		if err := h.QueryRowContext(r.ctx, `SELECT block FROM `+table+`_data WHERE id = ?`, ftsStructureRowID).Scan(&structure); err != nil { //nolint:gosec // the table is one of the two FTS table constants
			return fmt.Errorf("read the structure record: %w", err)
		}
		segs, err := decodeFTSStructureSegments(structure)
		if err != nil {
			return err
		}
		slices.SortStableFunc(segs, func(a, b ftsSegment) int {
			switch {
			case a.ID < b.ID:
				return -1
			case a.ID > b.ID:
				return 1
			}
			return 0
		})
		var total int64
		for i, s := range segs {
			if i > 0 && segs[i-1].ID == s.ID {
				return fmt.Errorf("%w: segment %d is listed twice", errFTSStructure, s.ID)
			}
			if !s.Empty() {
				total += s.Last - s.First + 1
			}
		}
		d, add := r.c.countedPhase("fts.index/"+table+"/decode", total, "pages")
		done = d
		pgsz, err := ftsIdxPgsz(r.ctx, h, table)
		if err != nil {
			return err
		}
		return r.readLeaves(h, table, ti, segs, add, pgsz)
	})
	if done != nil {
		done()
	}
	if err != nil {
		r.idxHits = nil
		return err
	}
	return r.confirmIdxHits(table)
}

// readLeaves is the body of indexPass inside its read transaction.
func (r *ftsVerify) readLeaves(h ReadHandle, table string, ti int, segs []ftsSegment, add func(int64), pgsz int) error {
	ir := &ftsIdxRun{ctx: r.ctx, q: h, table: table, report: func(f ftsIdxFinding) { r.idxFinding(table, f) }, pgsz: pgsz}
	rows, err := h.QueryContext(r.ctx, `SELECT id, block FROM `+table+`_data WHERE id >= ? ORDER BY id`, int64(1)<<ftsSegidShift) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	digest := r.ex != nil && r.ex.enabled
	var cur *ftsSegRun
	var idxErr error // the first failure to read the case in the recomputation
	defer func() {
		if cur != nil && cur.idx != nil {
			cur.idx.close()
		}
	}()
	si := 0 // the segment the rows are at
	start := func(s ftsSegment) *ftsSegRun {
		run := &ftsSegRun{seg: s, rd: newFTSSegReader(s)}
		if !s.Empty() && idxErr == nil {
			var err error
			if run.idx, err = ir.beginSegment(s, run.rd); err != nil {
				idxErr = err
			}
		}
		if digest {
			run.dig = r.found.fork()
		}
		return run
	}
	sinkOf := func(run *ftsSegRun) ftsSink {
		var sk ftsSink
		if digest {
			sk.Posting = func(term []byte, rowid int64, col int, off int64) {
				run.dig.add(ti, ftsBucketOf(r.sorted, rowid), term, rowid, col, off)
			}
		}
		if run.idx != nil {
			sk.Leaf = run.idx.leaf
		}
		sk.Delete = func(_ []byte, rowid int64) {
			run.nDel++
			if len(run.dels) < verifyMaxPerKind {
				run.dels = append(run.dels, rowid)
			}
			if run.delBkts == nil {
				run.delBkts = map[ftsBucketKey]struct{}{}
			}
			run.delBkts[ftsBucketOf(r.sorted, rowid)] = struct{}{}
		}
		return sk
	}
	var sink ftsSink
	finish := func() {
		if cur == nil {
			cur = start(segs[si])
		}
		if err := r.endSegment(table, ti, cur); err != nil && idxErr == nil {
			idxErr = err
		}
		cur = nil
		si++
	}

	var pages, ticked int64
	for rows.Next() {
		var id int64
		var block sql.RawBytes // valid until the next row: the decoder keeps nothing of a page
		if err := rows.Scan(&id, &block); err != nil {
			return err
		}
		seg := id >> ftsSegidShift
		for si < len(segs) && segs[si].ID < seg {
			finish()
		}
		if si >= len(segs) || segs[si].ID != seg {
			continue // not referenced by the structure: checkUnreferenced reports it
		}
		local := id & ftsPageMask
		if segs[si].Empty() {
			continue // a row of an emptied segment (D143, D149)
		}
		if cur == nil {
			cur = start(segs[si])
			sink = sinkOf(cur)
		}
		if idxErr != nil {
			return idxErr
		}
		if local >= ftsLeafLimit {
			// a doclist-index page: it follows every leaf of its segment, which are over now
			if err := r.closeLeaves(table, cur); err != nil {
				return err
			}
			if cur.idx != nil && !cur.failed && local&ftsDlidxFlag != 0 {
				cur.idx.dlidx(id, block)
			}
			continue
		}
		pages++
		if pages-ticked >= ftsPagesPerTick {
			add(pages - ticked)
			ticked = pages
			if r.observe != nil {
				r.observe("fts-data", ftsPagesPerTick)
			}
			if err := r.ctx.Err(); err != nil {
				return err
			}
		}
		if cur.failed {
			continue
		}
		if err := cur.rd.Page(local, block, sink); err != nil {
			cur.failed = true
			r.reportSegmentError(table, err)
		} else if cur.idx != nil && cur.idx.cmpErr != nil {
			return cur.idx.cmpErr
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for si < len(segs) {
		finish()
	}
	if idxErr != nil {
		return idxErr
	}
	add(pages - ticked)
	if r.observe != nil && pages > ticked {
		r.observe("fts-data", int(pages-ticked))
	}
	return nil
}

// endSegment closes a segment: a segment that decoded completely contributes its postings and its delete
// markers, any other contributes nothing.
func (r *ftsVerify) endSegment(table string, ti int, run *ftsSegRun) error {
	err := r.closeLeaves(table, run)
	if run.idx != nil {
		if err == nil && !run.failed {
			run.idx.endDlidx()
		}
		run.idx.close()
		run.idx = nil
	}
	if err != nil || run.failed {
		return err
	}
	if run.dig != nil {
		r.found.merge(run.dig)
	}
	for _, id := range run.dels {
		r.add(table, "fts-delete", "%s: segment %d holds a delete marker for record %d (a genuine index of this case never holds one)", table, run.seg.ID, id)
	}
	if rest := run.nDel - len(run.dels); rest > 0 {
		r.addUnlisted(table, "fts-delete", rest)
	}
	for b := range run.delBkts {
		r.bad[ti][b] = true
	}
	return nil
}

// closeLeaves ends the leaf pages of a segment: the reader must have seen every page, and the recomputation
// compares its last rows. A segment that fails contributes nothing (D147).
func (r *ftsVerify) closeLeaves(table string, run *ftsSegRun) error {
	if run.leavesDone {
		return nil
	}
	run.leavesDone = true
	if !run.failed {
		if err := run.rd.Finish(); err != nil {
			run.failed = true
			r.reportSegmentError(table, err)
		}
	}
	if run.idx != nil && !run.failed {
		return run.idx.endLeaves()
	}
	return nil
}

// reportSegmentError reports a segment that does not decode: one PROBLEM per segment.
func (r *ftsVerify) reportSegmentError(table string, err error) {
	var le *ftsLeafError
	if errors.As(err, &le) {
		r.add(table, "fts-leaf", "%s: segment %d page %d is not a valid leaf: %s", table, le.Seg, le.Page, clipText(le.Reason, 200))
		return
	}
	r.add(table, "fts-unreadable", "%s: the index cannot be read: %q", table, clipText(err.Error(), 300))
}

// failingBuckets lists, in audited order, the buckets whose digest of the leaf pages differs from the digest
// of the records, plus the buckets of a delete marker (a genuine index holds none, so the effective postings
// of the bucket are looked at again whatever the sums say). It is meaningful only when the expected side is
// complete and the pass read the whole table.
func (r *ftsVerify) failingBuckets(table string) []ftsBucketKey {
	ti := ftsTableIndex(table)
	if r.ex == nil || !r.ex.complete {
		return nil
	}
	want, got := r.ex.dig.sums[ti], r.found.sums[ti]
	seen := map[ftsBucketKey]bool{}
	var out []ftsBucketKey
	check := func(k ftsBucketKey) {
		if seen[k] {
			return
		}
		seen[k] = true
		w, g := want[k], got[k]
		var ws, gs ftsSum
		if w != nil {
			ws = *w
		}
		if g != nil {
			gs = *g
		}
		r.bucketsCompared++
		if ws != gs || r.bad[ti][k] {
			out = append(out, k)
		}
	}
	for k := range want {
		check(k)
	}
	for k := range got {
		check(k)
	}
	for k := range r.bad[ti] {
		check(k)
	}
	slices.SortFunc(out, func(a, b ftsBucketKey) int {
		switch {
		case ftsBucketLess(a, b):
			return -1
		case ftsBucketLess(b, a):
			return 1
		}
		return 0
	})
	return out
}

// describeBuckets names up to five buckets for a PROBLEM line.
func (r *ftsVerify) describeBuckets(keys []ftsBucketKey) string {
	var parts []string
	for i, k := range keys {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(keys)-5))
			break
		}
		if k.batch >= 0 && k.batch < len(r.sorted) {
			b := r.sorted[k.batch]
			parts = append(parts, fmt.Sprintf("batch %d of ingest %q (ids %d..%d)", b.BatchNo, b.IngestID, b.FirstID, b.lastID()))
		} else {
			parts = append(parts, fmt.Sprintf("the ids %d..%d outside every audited batch", k.block<<16, k.block<<16+0xFFFF))
		}
	}
	return strings.Join(parts, ", ")
}
