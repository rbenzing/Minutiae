package evidence

import (
	"fmt"
	"math"
	"time"
)

// What the records say the full-text indexes hold (5D-14i), computed inside the record stream that
// P4 already runs (the records are read once). For every indexable record, with the Go replica of each
// FTS5 tokenizer (fts5tok.go), it
//
//   - folds every posting (term, record, column, position) into the digest of its audited batch
//     (verify_ftsdigest.go);
//   - counts the tokens of each column: the expected _docsize blob of the record, and the expected
//     totals record (_data row 1) of the table, which are compared with the stored ones.
//
// The _docsize rows are compared in windows of ftsDocsizeWindow indexable records (V10). A window covers
// the id range since the previous one, so a size entry for a record that does not exist, or has no
// indexable text, is found wherever it is: the first window starts at MinInt64, the last ends at MaxInt64.

// ftsDocsizeWindow is the number of indexable records between two _docsize comparisons.
const ftsDocsizeWindow = 5000

// ftsPendingSize is the expected _docsize row of one record: the id and the blob varint(tokens of the
// summary) || varint(tokens of the body), at most two 9-byte varints.
type ftsPendingSize struct {
	id int64
	n  uint8
	b  [18]byte
}

// ftsExpect is the expected side of P11 for one run.
type ftsExpect struct {
	r      *ftsVerify
	sorted []auditedBatch
	dig    *ftsDigester
	tok    [2]func(string, func([]byte, int))

	// the posting being folded (set before a tokenizer runs, read by emit)
	curTable  int
	curBucket ftsBucketKey
	curID     int64
	curCol    int
	curCount  uint64
	emitFn    func([]byte, int)

	pend         [2][]ftsPendingSize
	sinceCompare int
	nextLo       int64 // the first id of the next _docsize window
	exhausted    bool  // a window already ended at MaxInt64

	docs   int64
	tokens [2][2]uint64 // tokens summed per table and column (the totals record)

	enabled  bool // false: the tokenizer replica did not pass its self-test, nothing is folded
	stopped  bool // the verify context ended: nothing more is folded
	complete bool // the whole stream was folded and every window compared
	timed    bool
	busy     time.Duration
}

// newFTSExpect makes the expected side. enabled is false when the tokenizer self-test failed: the
// postings, sizes and totals are then not compared, and nothing is tokenized.
func (r *ftsVerify) newFTSExpect(sorted []auditedBatch, key [16]byte, enabled bool) (*ftsExpect, error) {
	e := &ftsExpect{r: r, sorted: sorted, nextLo: math.MinInt64, enabled: enabled, timed: r.c.verifyPhaseHook != nil}
	if !enabled {
		return e, nil
	}
	d, err := newFTSDigester(key)
	if err != nil {
		return nil, err
	}
	e.dig = d
	for i, t := range FTSTables() {
		tok := ftsTokenizer(t)
		if tok == nil {
			return nil, fmt.Errorf("no tokenizer replica for %s", t)
		}
		e.tok[i] = tok
	}
	e.emitFn = func(term []byte, pos int) {
		e.dig.add(e.curTable, e.curBucket, term, e.curID, e.curCol, int64(pos))
		e.curCount++
	}
	return e, nil
}

// row folds one record. It is called from the records.stream consumer, after the chunk's ReadTx has closed.
func (e *ftsExpect) row(row RecordRow) {
	if !e.enabled || e.stopped {
		return
	}
	if e.r.ctx.Err() != nil || e.r.dead {
		e.stopped = true
		return
	}
	var t0 time.Time
	if e.timed {
		t0 = time.Now()
		defer func() { e.busy += time.Since(t0) }()
	}
	doc, has := NewFTSDoc(row.ID, row.Summary, row.Body)
	if !has {
		return
	}
	e.curID, e.curBucket = doc.ID, ftsBucketOf(e.sorted, doc.ID)
	for t := range e.tok {
		e.curTable = t
		var counts [2]uint64
		for col, text := range [2]string{doc.Summary, doc.Body} {
			e.curCol, e.curCount = col, 0
			e.tok[t](text, e.emitFn)
			counts[col] = e.curCount
			e.tokens[t][col] += e.curCount
		}
		p := ftsPendingSize{id: doc.ID}
		blob := appendSQLiteVarint(appendSQLiteVarint(p.b[:0], counts[0]), counts[1])
		p.n = uint8(len(blob))
		e.pend[t] = append(e.pend[t], p)
	}
	e.docs++
	if e.sinceCompare++; e.sinceCompare >= ftsDocsizeWindow {
		e.compareWindow(doc.ID)
	}
}

// compareWindow compares the _docsize rows of the ids from nextLo to hi with the pending sizes.
func (e *ftsExpect) compareWindow(hi int64) {
	if e.exhausted {
		return
	}
	for t, table := range FTSTables() {
		if e.r.dead {
			break
		}
		if err := e.r.compareDocsizeWindow(table, e.nextLo, hi, e.pend[t]); err != nil {
			e.r.fail(table, err)
		}
		e.pend[t] = e.pend[t][:0]
	}
	e.sinceCompare = 0
	if hi == math.MaxInt64 {
		e.exhausted = true
		return
	}
	e.nextLo = hi + 1
}

// finish compares the last window (up to MaxInt64) when the whole stream was read, and says whether the
// expected side is complete. It is called once, after the stream, whether or not it was cut.
func (e *ftsExpect) finish(streamed bool) {
	if !e.enabled || e.stopped || !streamed {
		return
	}
	e.compareWindow(math.MaxInt64)
	e.complete = !e.r.dead && !e.stopped
}

// compareDocsizeWindow merges the _docsize rows of the case's index with ids in [lo, hi] with the expected
// sizes of the same range, both in id order.
func (r *ftsVerify) compareDocsizeWindow(table string, lo, hi int64, want []ftsPendingSize) error {
	q := `SELECT typeof(id), id, typeof(sz), sz FROM ` + table + `_docsize WHERE id >= ? AND id <= ? ORDER BY id` //nolint:gosec // the table is one of the two FTS table constants
	return r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		rows, err := h.QueryContext(r.ctx, q, lo, hi)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		type row struct {
			id int64
			sz cell
		}
		next := func() (row, bool, error) {
			for rows.Next() {
				var ti, ts string
				var id, sz any
				if err := rows.Scan(&ti, &id, &ts, &sz); err != nil {
					return row{}, false, err
				}
				n, ok := id.(int64)
				if ti != "integer" || !ok {
					r.add(table, "fts-unreadable", "%s_docsize: a row has an id of storage class %s", table, ti)
					continue
				}
				return row{n, cellOf(ts, sz)}, true, nil
			}
			return row{}, false, rows.Err()
		}
		cur, has, err := next()
		if err != nil {
			return err
		}
		for i := 0; has || i < len(want); {
			switch {
			case has && (i >= len(want) || cur.id < want[i].id):
				r.addRecord(table, "fts-size", cur.id, "%s_docsize: the index holds a size entry for record %d, which does not exist or has no indexable text", table, cur.id)
				if cur, has, err = next(); err != nil {
					return err
				}
			case i < len(want) && (!has || want[i].id < cur.id):
				r.addRecord(table, "fts-size", want[i].id, "%s_docsize: record %d: the size entry is missing", table, want[i].id)
				i++
			default:
				exp := cell{typ: "blob", repr: string(want[i].b[:want[i].n])}
				switch {
				case cur.sz.typ != exp.typ:
					r.addRecord(table, "fts-size", cur.id, "%s_docsize: record %d: the size entry is of storage class %s, a rebuild holds %s", table, cur.id, cur.sz.typ, exp.typ)
				case cur.sz.repr != exp.repr:
					r.addRecord(table, "fts-size", cur.id, "%s_docsize: record %d: the size entry differs from a rebuild (found %s, a rebuild holds %s)", table, cur.id, cur.sz, exp)
				}
				i++
				if cur, has, err = next(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// expectedTotals is the totals record (_data row 1) the records produce: varint(indexed documents),
// varint(tokens of the summary column), varint(tokens of the body column) (_fts5StorageSaveTotals). A
// table without a document holds an empty record, as a fresh table and a table after 'delete-all' do.
func (e *ftsExpect) expectedTotals(ti int) []byte {
	if e.docs == 0 {
		return []byte{}
	}
	b := appendSQLiteVarint(nil, uint64(e.docs))
	b = appendSQLiteVarint(b, e.tokens[ti][0])
	return appendSQLiteVarint(b, e.tokens[ti][1])
}

// compareTotalsRecomputed compares the totals record (row 1 of _data: the document and token counts bm25
// uses), byte for byte, with the one the records produce.
func (r *ftsVerify) compareTotalsRecomputed(table string) error {
	if r.ex == nil || !r.ex.complete {
		return nil
	}
	q := `SELECT typeof(id), id, typeof(block), block FROM ` + table + `_data WHERE id = 1 LIMIT 2` //nolint:gosec // the table is one of the two FTS table constants
	var found []keyedRow
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		rows, err := h.QueryContext(r.ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		found, err = scanKeyed(rows, 2, func(e error) error { return e })
		return err
	})
	if err != nil {
		return err
	}
	want := keyedRow{cellOf("integer", int64(ftsAveragesRowID)), cell{typ: "blob", repr: string(r.ex.expectedTotals(ftsTableIndex(table)))}}
	if len(found) != 1 || found[0] != want {
		r.add(table, "fts-totals", "%s: the index totals differ from a rebuild (ranking is unreliable)", table)
	}
	return nil
}

// ftsTableIndex is 0 for the word index and 1 for the substring index.
func ftsTableIndex(table string) int {
	if table == FTSSubTable {
		return 1
	}
	return 0
}

// phaseSpan reports a phase that was timed by summing pieces of work (not one start and one end) to the
// phase hook, as a frame that opens and closes at once inside the running parent, so that a snapshot taken by
// the hook names it like any other phase.
func (c *Case) phaseSpan(name string, d time.Duration) {
	h, p := c.verifyPhaseHook, c.verifyProgress
	var f *progressFrame
	if p != nil {
		f = p.begin(name, false, 0, "")
	}
	if h != nil {
		h(name, d)
	}
	if p != nil {
		p.end(f)
	}
}
