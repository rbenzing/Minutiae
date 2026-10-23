package evidence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// P11 localization (5D-14k, plan decisions V14 and V15, coordinator's call CC1).
//
// The index pass (verify_ftsindex.go) says WHICH batches differ: a failing bucket is an audited batch (or a block
// of ids outside every audited batch) whose digest of the leaf pages differs from the digest of the records, or
// that holds a delete marker. Only those buckets are looked at again, with SQLite's own tokenizer, to produce the
// per-record listing the examiner reads: a hidden hit, an invented hit, a duplicate or out-of-order entry, at
// most 50 of each kind per table.
//
// A clean verify never gets here: it creates no scratch area. A failing verify takes the failing buckets in
// audited order, in rounds that hold as many buckets as fit ftsLocalizeRoundPostings expected postings (at least
// one), and for each round
//
//  1. rebuilds the records of the round's buckets into the private scratch index (the code the writer uses),
//  2. decodes the case's leaf pages again and keeps the postings and delete markers of the round's records, with
//     the age of their segment, in a second private database,
//  3. compares the vocab of the rebuild with the EFFECTIVE view of what the pages hold (per term and record the
//     newest segment wins and a delete marker removes the posting), which names the entries that differ.
//
// It stops after ftsLocalizeMaxBuckets buckets, or when the six per-item kinds (hidden, invented, order, for each
// of the two tables) hold their 50 listed items, whichever comes first. A failing bucket is never silent: every
// one that was not localized (past the bound, cut by the found-rows cap, the end of the verify context, a table
// with a segment that does not decode) is named in one line per table. A failing bucket whose comparison finds
// nothing means the tokenizer replica and SQLite disagree there, which is a PROBLEM (fail closed, V15).
//
// The scratch lives in <case>/tmp/verify-<runid> and is removed when verify ends.

var (
	// ftsLocalizeMaxBuckets is the bound of CC1: at most this many buckets are rebuilt.
	ftsLocalizeMaxBuckets = 64
	// ftsLocalizeRoundPostings is the expected postings a round may hold (V14); a bucket larger than it is a round of its own.
	ftsLocalizeRoundPostings = 4_000_000
	// ftsLocalizeFoundCap is the number of found rows (postings and delete markers) a round may keep (V14).
	ftsLocalizeFoundCap = 8_000_000
)

// errFTSLocalizeCap ends a round whose found rows pass the cap.
var errFTSLocalizeCap = errors.New("the round holds more index rows than the cap")

// ftsLocBucket is a failing bucket of one or both tables.
type ftsLocBucket struct {
	key   ftsBucketKey
	fails [2]bool
	n     uint64 // the expected postings of the bucket in both tables: its share of a round
}

// ftsLocalState is what the localization keeps between rounds and for its closing lines.
type ftsLocalState struct {
	loc *sql.DB // the found postings of a round, with their segment age (a second private database)

	rebuilt int                      // buckets rebuilt so far (the bound counts these)
	done    [2]map[ftsBucketKey]bool // per table: buckets whose comparison ran
	reason  [2]map[ftsBucketKey]string
	hit     [2]map[ftsBucketKey]bool // per table: buckets with a finding in the current round
}

func newFTSLocalState() *ftsLocalState {
	return &ftsLocalState{
		done:   [2]map[ftsBucketKey]bool{{}, {}},
		reason: [2]map[ftsBucketKey]string{{}, {}},
		hit:    [2]map[ftsBucketKey]bool{{}, {}},
	}
}

// localize looks at the failing buckets again, per round, with SQLite's own tokenizer (see the file comment).
func (r *ftsVerify) localize(failing map[string][]ftsBucketKey) {
	buckets := r.localBuckets(failing)
	if len(buckets) == 0 {
		return
	}
	if r.lc == nil {
		r.lc = newFTSLocalState()
	}
	total := min(len(buckets), ftsLocalizeMaxBuckets)
	done, add := r.c.countedPhase("fts.localize", int64(total), "batches")
	r.localizeRounds(buckets, add)
	done()
	r.reportNotLocalized(buckets)
}

// localBuckets lists the failing buckets of both tables once each, in audited order, with the tables they fail in.
func (r *ftsVerify) localBuckets(failing map[string][]ftsBucketKey) []ftsLocBucket {
	idx := map[ftsBucketKey]*ftsLocBucket{}
	var out []*ftsLocBucket
	for ti, table := range FTSTables() {
		for _, k := range failing[table] {
			b := idx[k]
			if b == nil {
				b = &ftsLocBucket{key: k}
				idx[k] = b
				out = append(out, b)
			}
			b.fails[ti] = true
			if r.ex != nil && r.ex.dig != nil {
				if s := r.ex.dig.sums[ti][k]; s != nil {
					b.n += s.n
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b *ftsLocBucket) int {
		switch {
		case ftsBucketLess(a.key, b.key):
			return -1
		case ftsBucketLess(b.key, a.key):
			return 1
		}
		return 0
	})
	res := make([]ftsLocBucket, len(out))
	for i, b := range out {
		res[i] = *b
	}
	return res
}

// kindsFull says whether every per-item kind holds its listed items already (the second stop of CC1).
func (r *ftsVerify) kindsFull() bool {
	for _, table := range FTSTables() {
		for _, base := range []string{"fts-hidden", "fts-invented", "fts-order"} {
			if r.ps.counts[ftsKind(table, base)] < verifyMaxPerKind {
				return false
			}
		}
	}
	return true
}

type ftsRoundResult int

const (
	roundDone ftsRoundResult = iota
	roundCut
	roundCapped
)

// the reasons a failing bucket is not localized (the text of the closing line)
const (
	ftsReasonCut     = "the verification ended before they were rebuilt"
	ftsReasonSegment = "a segment of this index is not a valid leaf, so what the index holds cannot be listed"
	ftsReasonOverrun = "the ids of every block past the first unaudited ones are one bucket that cannot be rebuilt"
)

func (r *ftsVerify) localizeRounds(buckets []ftsLocBucket, add func(int64)) {
	lc := r.lc
	// a table with a segment that does not decode cannot be localized: what its pages hold is not known
	var skipped [2]bool
	for ti := range skipped {
		skipped[ti] = r.segFailed[ti] > 0
	}
	takes := func(b ftsLocBucket) bool { // is any table of the bucket localizable
		return b.key != ftsOverflowBucket && (b.fails[0] && !skipped[0] || b.fails[1] && !skipped[1])
	}
	next := 0
	for next < len(buckets) && !r.dead && lc.rebuilt < ftsLocalizeMaxBuckets && !r.kindsFull() {
		if r.ctx.Err() != nil {
			r.rep.cancelCut = true
			r.dead = true
			break
		}
		var round []ftsLocBucket
		var n uint64
		for next < len(buckets) && lc.rebuilt+len(round) < ftsLocalizeMaxBuckets {
			b := buckets[next]
			if !takes(b) {
				next++
				continue
			}
			if len(round) > 0 && n+b.n > uint64(ftsLocalizeRoundPostings) {
				break
			}
			round = append(round, b)
			n += b.n
			next++
		}
		if len(round) == 0 {
			break
		}
		lc.rebuilt += len(round)
		res := r.localizeRound(round, skipped)
		add(int64(len(round)))
		for _, b := range round {
			for ti := range b.fails {
				if !b.fails[ti] || skipped[ti] {
					continue
				}
				switch res {
				case roundDone:
					lc.done[ti][b.key] = true
				case roundCapped:
					lc.reason[ti][b.key] = fmt.Sprintf("the batches hold more index rows than the cap of %d rows per round", ftsLocalizeFoundCap)
				default:
					lc.reason[ti][b.key] = ftsReasonCut
				}
			}
		}
		if res == roundCut {
			r.dead = true // an error that stopped the round (r.fail marks only the end of the context and a lost scratch database)
		}
	}
	// whatever no round localized gets its reason
	full := r.kindsFull()
	for _, b := range buckets {
		for ti := range b.fails {
			if !b.fails[ti] || lc.done[ti][b.key] || lc.reason[ti][b.key] != "" {
				continue
			}
			switch {
			case b.key == ftsOverflowBucket:
				lc.reason[ti][b.key] = ftsReasonOverrun
			case skipped[ti]:
				lc.reason[ti][b.key] = ftsReasonSegment
			case r.dead || r.ctx.Err() != nil:
				lc.reason[ti][b.key] = ftsReasonCut
			case full:
				lc.reason[ti][b.key] = fmt.Sprintf("every kind of difference already lists its %d items", verifyMaxPerKind)
			default:
				lc.reason[ti][b.key] = fmt.Sprintf("at most %d batches are rebuilt and listed", ftsLocalizeMaxBuckets)
			}
		}
	}
}

// bucketRange is the id range that holds every record of a bucket (an outside block shares its range with the
// audited batches that overlap it, so the records read from it are filtered by ftsBucketOf).
func (r *ftsVerify) bucketRange(k ftsBucketKey) (lo, hi int64) {
	if k.batch >= 0 && k.batch < len(r.sorted) {
		b := r.sorted[k.batch]
		return b.FirstID, b.lastID()
	}
	lo = k.block << 16
	return lo, lo + 0xFFFF
}

// localizeRound rebuilds the records of the round's buckets and compares them with what the pages hold.
func (r *ftsVerify) localizeRound(round []ftsLocBucket, skipped [2]bool) ftsRoundResult {
	if r.exp == nil {
		if err := r.setup(); err != nil {
			if ctxCaused(r.ctx, err) { // cancelled before the rebuild could start: a cut, reported once by verifyRun
				r.rep.cancelCut = true
			} else {
				r.rep.problemf("%s: the full-text index cannot be verified: its private rebuild could not be set up: %q", FTSWordTable, err.Error())
			}
			r.dead = true
			return roundCut
		}
		r.hook("after-open")
	}
	lc := r.lc
	set := map[ftsBucketKey]bool{}
	for _, b := range round {
		set[b.key] = true
	}
	lc.hit = [2]map[ftsBucketKey]bool{{}, {}}
	if err := r.resetRound(); err != nil {
		r.fail(FTSWordTable, err)
		return roundCut
	}
	if !r.rebuildBuckets(round) {
		return roundCut
	}
	r.hook("after-rebuild")
	var rows int64
	for ti, table := range FTSTables() {
		if skipped[ti] || !roundFails(round, ti) {
			continue
		}
		if err := r.loadFound(table, ti, set, &rows); err != nil {
			if errors.Is(err, errFTSLocalizeCap) {
				return roundCapped
			}
			r.fail(table, err)
			return roundCut
		}
	}
	for ti, table := range FTSTables() {
		if skipped[ti] || !roundFails(round, ti) {
			continue
		}
		if err := r.compareFound(table, ti); err != nil {
			r.fail(table, err)
			return roundCut
		}
		for _, b := range round {
			if b.fails[ti] && !lc.hit[ti][b.key] {
				r.add(table, "fts-digest", "%s: %s: the postings differ from the tokenizer replica's, but the index equals SQLite's own rebuild there: the tokenizer replica does not match this SQLite and the full-text check cannot pass",
					table, r.bucketName(b.key))
			}
		}
		r.hook("compared:" + table)
	}
	return roundDone
}

func roundFails(round []ftsLocBucket, ti int) bool {
	for _, b := range round {
		if b.fails[ti] {
			return true
		}
	}
	return false
}

// resetRound empties the scratch databases for the next round.
func (r *ftsVerify) resetRound() error {
	for _, table := range FTSTables() {
		if _, err := r.exp.ExecContext(r.ctx, `INSERT INTO `+table+`(`+table+`) VALUES ('delete-all')`); err != nil { //nolint:gosec // the table is one of the two FTS table constants
			return expectedErr{fmt.Errorf("empty the rebuilt %s: %w", table, err)}
		}
	}
	if _, err := r.lc.loc.ExecContext(r.ctx, `DELETE FROM found`); err != nil {
		return expectedErr{fmt.Errorf("empty the found postings: %w", err)}
	}
	r.indexed = r.indexed[:0]
	return nil
}

// rebuildBuckets streams the records of the round's buckets and writes their documents into the rebuilt index,
// a transaction per chunk. It reports whether every record was read and written.
func (r *ftsVerify) rebuildBuckets(round []ftsLocBucket) bool {
	sctx, cancel := context.WithCancelCause(r.ctx)
	defer cancel(nil)
	var (
		docs    []FTSDoc
		size    int
		stepErr error
	)
	flush := func() {
		if len(docs) == 0 || stepErr != nil {
			return
		}
		if stepErr = r.writeDocs(docs); stepErr != nil {
			cancel(stepErr)
			return
		}
		for _, d := range docs {
			r.indexed = append(r.indexed, d.ID)
		}
		docs, size = docs[:0], 0
	}
	// The record problems this stream notices (storage classes, orphan times) were reported by the record scan;
	// they are collected into a report of their own.
	side := &problemSet{rep: &VerifyReport{Problems: []string{}, Notices: []string{}}, counts: map[string]int{}}
	complete := true
	for _, b := range round {
		lo, hi := r.bucketRange(b.key)
		obs := func(_ string, rows int) {
			if r.observe != nil {
				r.observe("fts-localize", rows) // the chunks of the rebuild stream (tests count them)
			}
		}
		ok := r.c.streamRecordsBetween(sctx, side, lo, hi, verifyChunkRows, obs, func(row RecordRow, _ recordRowMeta) {
			if stepErr != nil || ftsBucketOf(r.sorted, row.ID) != b.key {
				return
			}
			if doc, has := NewFTSDoc(row.ID, row.Summary, row.Body); has {
				docs = append(docs, doc)
				size += len(doc.Summary) + len(doc.Body)
				if len(docs) >= verifyChunkRows || size >= reindexChunkBytes {
					flush()
				}
			}
		})
		flush()
		if stepErr != nil || !ok {
			complete = false
			break
		}
	}
	if stepErr != nil {
		r.fail(FTSWordTable, stepErr)
		return false
	}
	for _, p := range side.rep.Problems {
		if strings.HasPrefix(p, "records holds ") || strings.HasPrefix(p, "records unreadable") {
			complete = false
		}
	}
	if !complete && r.ctx.Err() != nil {
		r.rep.cancelCut = true // the stream ended with the context; verifyRun reports that once
		r.dead = true
		return false
	}
	if !complete {
		r.rep.problemf("%s: the records could not all be read to rebuild the index, so the index content is not verified: %q", FTSWordTable, clipText(strings.Join(side.rep.Problems, "; "), 400))
		r.dead = true
		return false
	}
	slices.Sort(r.indexed) // keyset order already; sorted again so a binary search never depends on it
	return true
}

// ftsFoundSeg is the segment loadFound is reading: its reader, and the transaction that holds its rows until it
// is known to decode completely (the ftsSink contract, D147).
type ftsFoundSeg struct {
	seg  ftsSegment
	rd   *ftsSegReader
	tx   *sql.Tx
	stmt *sql.Stmt
	bad  bool
}

func (f *ftsFoundSeg) rollback() {
	if f.tx != nil {
		_ = f.stmt.Close()
		_ = f.tx.Rollback()
		f.tx, f.stmt = nil, nil
	}
}

// loadFound decodes the leaf pages of table again and keeps, in the found database, the postings and delete
// markers of the round's records with the age of their segment. A segment that does not decode contributes
// nothing and is an error here, because it decoded the first time (the index pass skips a table that has one).
func (r *ftsVerify) loadFound(table string, ti int, set map[ftsBucketKey]bool, rows *int64) error {
	lc := r.lc
	return r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		var structure []byte
		if err := h.QueryRowContext(r.ctx, `SELECT block FROM `+table+`_data WHERE id = ?`, ftsStructureRowID).Scan(&structure); err != nil { //nolint:gosec // the table is one of the two FTS table constants
			return fmt.Errorf("read the structure record: %w", err)
		}
		segs, err := decodeFTSStructureSegments(structure)
		if err != nil {
			return err
		}
		age := ftsSegmentAge(segs)
		byID := make(map[int64]ftsSegment, len(segs))
		for _, s := range segs {
			byID[s.ID] = s
		}
		dr, err := h.QueryContext(r.ctx, `SELECT id, block FROM `+table+`_data WHERE id >= ? ORDER BY id`, int64(1)<<ftsSegidShift) //nolint:gosec // the table is one of the two FTS table constants
		if err != nil {
			return err
		}
		defer func() { _ = dr.Close() }()

		var (
			cur    *ftsFoundSeg
			insErr error
			capped bool
		)
		defer func() {
			if cur != nil {
				cur.rollback()
			}
		}()
		insert := func(term []byte, rowid int64, del, col int, off int64) {
			b := ftsBucketOf(r.sorted, rowid)
			if insErr != nil || capped || !set[b] {
				return
			}
			if del == 1 {
				lc.hit[ti][b] = true // a delete marker was reported by the index pass already
			}
			if *rows++; *rows > int64(ftsLocalizeFoundCap) {
				capped = true
				return
			}
			_, insErr = cur.stmt.ExecContext(r.ctx, ti, term, rowid, age[cur.seg.ID], del, col, off)
		}
		sink := ftsSink{
			Posting: func(term []byte, rowid int64, col int, off int64) { insert(term, rowid, 0, col, off) },
			Delete:  func(term []byte, rowid int64) { insert(term, rowid, 1, 0, 0) },
		}
		finish := func() error {
			if cur == nil {
				return nil
			}
			defer func() { cur = nil }()
			if !cur.bad {
				if err := cur.rd.Finish(); err != nil {
					cur.bad = true
				}
			}
			if cur.bad {
				cur.rollback()
				return fmt.Errorf("%w: segment %d does not decode on the second read", errFTSStructure, cur.seg.ID)
			}
			_ = cur.stmt.Close()
			tx := cur.tx
			cur.tx, cur.stmt = nil, nil
			if err := tx.Commit(); err != nil {
				return expectedErr{err}
			}
			return nil
		}
		for dr.Next() {
			var id int64
			var block sql.RawBytes
			if err := dr.Scan(&id, &block); err != nil {
				return err
			}
			seg := id >> ftsSegidShift
			if cur != nil && cur.seg.ID != seg {
				if err := finish(); err != nil {
					return err
				}
			}
			s, ok := byID[seg]
			if !ok || s.Empty() || s.ID >= ftsSegIDLimit {
				continue
			}
			local := id & ftsPageMask
			if local >= ftsLeafLimit {
				continue // a doclist-index page or an interior page: no posting
			}
			if cur == nil {
				f := &ftsFoundSeg{seg: s, rd: newFTSSegReader(s)}
				if f.tx, err = lc.loc.BeginTx(r.ctx, nil); err != nil {
					return expectedErr{err}
				}
				cur = f
				if f.stmt, err = f.tx.PrepareContext(r.ctx, `INSERT INTO found (tbl, term, doc, age, del, col, off) VALUES (?, ?, ?, ?, ?, ?, ?)`); err != nil {
					return expectedErr{err}
				}
			}
			if cur.bad {
				continue
			}
			if err := cur.rd.Page(local, block, sink); err != nil {
				cur.bad = true
			}
			switch {
			case insErr != nil:
				return expectedErr{insErr}
			case capped:
				return errFTSLocalizeCap
			}
		}
		if err := dr.Err(); err != nil {
			return err
		}
		return finish()
	})
}

// compareFound merges the vocab of the rebuild with the effective view of the found postings of a table and
// lists the duplicates across segments.
func (r *ftsVerify) compareFound(table string, ti int) error {
	const effective = `SELECT 'text', CAST(f.term AS TEXT), 'integer', f.doc, 'text', CASE f.col WHEN 0 THEN 'summary' ELSE 'body' END, 'integer', f.off
		FROM found f WHERE f.tbl = ? AND f.del = 0 AND f.age = (SELECT max(g.age) FROM found g WHERE g.tbl = f.tbl AND g.term = f.term AND g.doc = f.doc)
		ORDER BY f.term, f.doc, f.col, f.off`
	fr, err := r.lc.loc.QueryContext(r.ctx, effective, ti)
	if err != nil {
		return expectedErr{err}
	}
	defer func() { _ = fr.Close() }()
	er, err := r.exp.QueryContext(r.ctx, vocabSelect+ftsVocabName(table)) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return expectedErr{err}
	}
	defer func() { _ = er.Close() }()
	if err := r.mergeVocab(table, fr, er); err != nil {
		return err
	}
	return r.listDuplicates(table, ti)
}

// mergeVocab merges what the pages hold (found) with what the rebuild holds (expected), both in
// (term, record, column, offset) order, and lists the differences with the texts the vocab comparison had
// (a hidden hit, an invented hit).
func (r *ftsVerify) mergeVocab(table string, found, expected *sql.Rows) error {
	ti := ftsTableIndex(table)
	mark := func(doc int64) { r.lc.hit[ti][ftsBucketOf(r.sorted, doc)] = true }
	f := &vocabIter{rows: found, expected: true}
	e := &vocabIter{rows: expected, expected: true}
	if err := f.next(); err != nil {
		return err
	}
	if err := e.next(); err != nil {
		return err
	}
	for f.has || e.has {
		c := 0
		switch {
		case !f.has:
			c = 1
		case !e.has:
			c = -1
		default:
			c = cmpEntry(f.cur, e.cur)
		}
		switch {
		case c == 0:
			if err := f.next(); err != nil {
				return err
			}
			if err := e.next(); err != nil {
				return err
			}
		case c < 0:
			mark(f.cur.doc)
			r.invented(table, f.cur)
			if err := f.next(); err != nil {
				return err
			}
		default:
			mark(e.cur.doc)
			r.addRecord(table, "fts-hidden", e.cur.doc,
				"%s: record %d: the index lacks %s, which the text of the record holds (a search hit is hidden)", table, e.cur.doc, e.cur)
			if err := e.next(); err != nil {
				return err
			}
		}
	}
	return nil
}

// listDuplicates lists the (term, record) pairs that two or more segments hold without a delete marker: the
// record is indexed twice for that term. The newest segment wins in the effective view, so the pair is not a
// difference there; it is its own finding.
func (r *ftsVerify) listDuplicates(table string, ti int) error {
	const q = `SELECT CAST(g.term AS TEXT), g.doc,
		(SELECT f.col FROM found f WHERE f.tbl = g.tbl AND f.term = g.term AND f.doc = g.doc ORDER BY f.age DESC, f.col, f.off LIMIT 1),
		(SELECT f.off FROM found f WHERE f.tbl = g.tbl AND f.term = g.term AND f.doc = g.doc ORDER BY f.age DESC, f.col, f.off LIMIT 1)
		FROM (SELECT tbl, term, doc FROM found WHERE tbl = ? GROUP BY tbl, term, doc HAVING count(DISTINCT age) >= 2 AND sum(del) = 0) g
		ORDER BY g.term, g.doc`
	rows, err := r.lc.loc.QueryContext(r.ctx, q, ti)
	if err != nil {
		return expectedErr{err}
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e ftsEntry
		if err := rows.Scan(&e.term, &e.doc, &e.col, &e.off); err != nil {
			return expectedErr{err}
		}
		r.lc.hit[ti][ftsBucketOf(r.sorted, e.doc)] = true
		r.add(table, "fts-order", "%s: record %d: the index lists %s twice", table, e.doc, e)
	}
	if err := rows.Err(); err != nil {
		return expectedErr{err}
	}
	return nil
}

// bucketName names a bucket for a PROBLEM line.
func (r *ftsVerify) bucketName(k ftsBucketKey) string {
	switch {
	case k == ftsOverflowBucket:
		return fmt.Sprintf("the ids of every block past the first %d unaudited ones", ftsMaxOutsideBuckets)
	case k.batch >= 0 && k.batch < len(r.sorted):
		b := r.sorted[k.batch]
		return fmt.Sprintf("batch %d of ingest %q (ids %d..%d)", b.BatchNo, b.IngestID, b.FirstID, b.lastID())
	}
	return fmt.Sprintf("ids %d..%d outside every audited batch", k.block<<16, k.block<<16+0xFFFF)
}

// reportNotLocalized names, per table, the failing buckets that were not localized, with the reason. A failing
// bucket is never silent: this line is the whole point of the bound.
func (r *ftsVerify) reportNotLocalized(buckets []ftsLocBucket) {
	lc := r.lc
	for ti, table := range FTSTables() {
		var names, reasons []string
		for _, b := range buckets {
			if !b.fails[ti] || lc.done[ti][b.key] {
				continue
			}
			names = append(names, r.bucketName(b.key))
			why := lc.reason[ti][b.key]
			if why == "" {
				why = ftsReasonCut
			}
			if !slices.Contains(reasons, why) {
				reasons = append(reasons, why)
			}
		}
		if len(names) == 0 {
			continue
		}
		shown := names
		if len(shown) > verifyMaxPerKind {
			shown = append(slices.Clone(names[:verifyMaxPerKind]), fmt.Sprintf("and %d more", len(names)-verifyMaxPerKind))
		}
		r.add(table, "fts-localize", "%s: %d batches whose index postings differ from their records were not localized (%s): %s",
			table, len(names), strings.Join(reasons, "; "), strings.Join(shown, ", "))
	}
}

// localizedAll says whether every failing bucket of every table was localized (the comparison ran to its end).
func (r *ftsVerify) localizedAll() bool {
	for ti := range r.failing {
		for _, k := range r.failing[ti] {
			if r.lc == nil || !r.lc.done[ti][k] {
				return false
			}
		}
	}
	return true
}
