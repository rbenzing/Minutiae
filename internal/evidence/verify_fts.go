package evidence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"modernc.org/sqlite"
)

// P11: the full-text indexes equal what the verified records produce.
//
// The records are authenticated by P4 (their digests are bound to the audit log), and the index is
// a pure function of them (NormalizeText, the two FTS5 tokenizers). So verify rebuilds both indexes
// from the records into a private temporary database, with the code the writer uses
// (NewFTSDoc, insertFTSDocs), and compares the logical content of every table of the case's indexes
// with the rebuild, entry by entry:
//
//   - the vocab of each index ((term, record, column, offset), in term, record, column, offset order);
//   - its _docsize rows (the size of every document), its _config rows, and its totals record
//     (_data row 1);
//   - what MATCH finds for every term of the rebuilt vocabulary: the shadow table <table>_idx (the
//     b-tree index over the leaf pages of a segment) is invisible to the vocab, to PRAGMA
//     integrity_check and to FTS5's own check when rows of it are deleted, yet MATCH then loses hits.
//     Only tables whose content already matched are examined this way (a table with a content
//     difference fails verify anyway).
//
// The case's own database is read through ReadTx, as everywhere else. EXCEPTION to the "read
// everything through ReadHandle" rule: the rebuilt index lives in a second, private database that
// verify creates and writes itself (an unnamed SQLite database, one pinned connection); it is not
// the case's database, holds no evidence the case does not hold already, and is discarded at the
// end. It is the only place two result sets are open at once (one on each database).
//
// Evidence-derived text never leaves the case directory: the private database and every spill file
// of SQLite live under <case>/tmp/verify-<runid>/ (created 0700, removed when verify ends, on
// every path). SQLite's temp directory is process-wide state (PRAGMA temp_store_directory): it is
// set before the rebuild and reset in a defer. The command line runs one command per process.

const (
	// ftsTmpDir is the directory of the case that holds the private files of a verification.
	ftsTmpDir = "tmp"
	// ftsExpectedCacheKiB is the page cache of the rebuilt index: it stays in memory up to this size and
	// spills to a file in the run directory beyond it.
	ftsExpectedCacheKiB = 262144
	// ftsReachChunk is the number of terms whose MATCH results are compared inside one read transaction.
	ftsReachChunk = 500
	// ftsMaxConfigRows bounds the _config rows read from a (possibly tampered) index.
	ftsMaxConfigRows = 1000
)

// errExpectedIndexLost is the error of every use of the rebuilt index after its connection was
// lost: a new connection would be a new, EMPTY database, which must never be compared as if it were
// the rebuild.
var errExpectedIndexLost = errors.New("expected index lost: the connection to the private rebuild was lost, and a new connection would be an empty database")

// expectedErr marks an error that comes from the rebuilt index, not from the case's index: the
// check cannot go on, and it says what it was that failed.
type expectedErr struct{ err error }

func (e expectedErr) Error() string { return e.err.Error() }
func (e expectedErr) Unwrap() error { return e.err }

// expectedConnector opens the private database of the rebuilt index exactly once. A second Connect
// (database/sql dials again after a connection was discarded) is refused with errExpectedIndexLost.
type expectedConnector struct {
	dialed   atomic.Bool
	cacheKiB int
}

func (e *expectedConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func (e *expectedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if !e.dialed.CompareAndSwap(false, true) {
		return nil, errExpectedIndexLost
	}
	conn, err := e.Driver().Open("")
	if err != nil {
		return nil, fmt.Errorf("open the private rebuild database: %w", err)
	}
	cache := e.cacheKiB
	if cache <= 0 {
		cache = ftsExpectedCacheKiB
	}
	for _, p := range []string{
		`PRAGMA journal_mode = OFF`, `PRAGMA synchronous = OFF`, `PRAGMA cache_size = -` + strconv.Itoa(cache), `PRAGMA locking_mode = EXCLUSIVE`,
	} {
		if err := runPragma(ctx, conn, p); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// runPragma runs one PRAGMA statement on a raw driver connection and drains its rows.
func runPragma(ctx context.Context, conn driver.Conn, stmt string) error {
	q, ok := conn.(driver.QueryerContext)
	if !ok {
		return errors.New("the SQLite driver cannot run queries")
	}
	rows, err := q.QueryContext(ctx, stmt, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	defer func() { _ = rows.Close() }()
	dest := make([]driver.Value, len(rows.Columns()))
	for {
		if err := rows.Next(dest); err != nil {
			break
		}
	}
	return nil
}

// setTempStoreDirectory sets SQLite's temporary-file directory, which is process-wide and cannot be
// read back; "" restores the default.
func setTempStoreDirectory(dir string) error {
	conn, err := (&sqlite.Driver{}).Open("")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return runPragma(context.Background(), conn, `PRAGMA temp_store_directory = '`+strings.ReplaceAll(dir, "'", "''")+`'`)
}

// ftsVerify is one run of P11.
type ftsVerify struct {
	c   *Case
	ctx context.Context
	ps  *problemSet
	rep *VerifyReport

	tmpRoot string // <case>/tmp
	dir     string // <case>/tmp/verify-<runid>
	tempSet bool   // SQLite's temp directory was pointed at dir
	exp     *sql.DB

	// indexed is the sorted ids of the records that have a document in the rebuild.
	indexed []int64
	// dead is set when the rebuilt index failed: nothing more can be compared.
	dead bool
	// problems counts the problems of each table (the reachability pass runs only on a table with none).
	problems map[string]int
	// listed holds the records already listed under a problem kind (so one record is listed once).
	listed map[string]map[int64]bool
}

func (c *Case) verifyFTS(ctx context.Context, ps *problemSet, observe verifyChunkObserver) {
	r := &ftsVerify{c: c, ctx: ctx, ps: ps, rep: ps.rep, problems: map[string]int{}, listed: map[string]map[int64]bool{}}
	defer r.cleanup() // runs on a panic too
	if err := r.setup(); err != nil {
		r.rep.problemf("%s: the full-text index cannot be verified: its private rebuild could not be set up: %q", FTSWordTable, err.Error())
		return
	}
	r.hook("after-open")
	if !r.rebuild(observe) {
		return
	}
	r.rep.FTSDocsChecked = len(r.indexed)
	r.hook("after-rebuild")
	for _, table := range FTSTables() {
		if r.dead {
			return
		}
		r.compareTable(table)
	}
}

func (r *ftsVerify) hook(point string) {
	if r.c.verifyFTSHook != nil {
		r.c.verifyFTSHook(point, r.exp)
	}
}

// setup creates the run directory, points SQLite's temp directory at it and creates the rebuilt
// index (both tables, their vocab tables, and the row-type vocab tables the reachability pass lists
// terms with).
func (r *ftsVerify) setup() error {
	abs, err := filepath.Abs(r.c.Dir)
	if err != nil {
		return err
	}
	r.tmpRoot = filepath.Join(abs, ftsTmpDir)
	if err := os.MkdirAll(r.tmpRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", r.tmpRoot, err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	dir := filepath.Join(r.tmpRoot, "verify-"+hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	r.dir = dir
	r.tempSet = true // reset in cleanup even when setting it fails half way
	if err := setTempStoreDirectory(dir); err != nil {
		return fmt.Errorf("point SQLite's temporary directory at %s: %w", dir, err)
	}
	// ONE pinned connection: a second connection to an unnamed database is a different, empty database
	r.exp = sql.OpenDB(&expectedConnector{cacheKiB: r.c.verifyFTSCacheKiB})
	r.exp.SetMaxOpenConns(1)
	r.exp.SetMaxIdleConns(1)
	r.exp.SetConnMaxLifetime(0)
	r.exp.SetConnMaxIdleTime(0)
	if err := r.exp.PingContext(r.ctx); err != nil {
		return err
	}
	for _, table := range FTSTables() {
		ddl, _ := FTSTableDDL(table)
		vocab, _ := FTSVocabDDL(table)
		for _, stmt := range []string{ddl, vocab, `CREATE VIRTUAL TABLE ` + table + `_vr USING fts5vocab(` + table + `, 'row')`} {
			if _, err := r.exp.ExecContext(r.ctx, stmt); err != nil {
				return fmt.Errorf("create the rebuilt %s: %w", table, err)
			}
		}
	}
	return nil
}

// cleanup closes the rebuilt index, restores SQLite's temp directory and removes the run directory
// (and <case>/tmp when nothing else is in it). Failing to remove what verify created is a problem.
func (r *ftsVerify) cleanup() {
	if r.exp != nil {
		_ = r.exp.Close()
	}
	if r.tempSet {
		if err := setTempStoreDirectory(""); err != nil {
			r.rep.problemf("%s: SQLite's temporary directory could not be reset: %q", FTSWordTable, err.Error())
		}
	}
	if r.dir != "" {
		if err := os.RemoveAll(r.dir); err != nil {
			r.rep.problemf("%s: verify could not remove its temporary directory %s (it may hold text of the records): %q", FTSWordTable, r.dir, err.Error())
		}
	}
	if r.tmpRoot != "" {
		_ = os.Remove(r.tmpRoot) // only an empty directory goes
	}
}

// checkTmp reports everything left in <case>/tmp/: a verification that was interrupted leaves its
// private files there, and they hold text derived from the records. Like a leftover staging
// directory it is a problem until an examiner removes it; verify never removes what it did not create.
func (c *Case) checkTmp(rep *VerifyReport) {
	entries, err := os.ReadDir(filepath.Join(c.Dir, ftsTmpDir))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		rep.problemf("temporary directory %s unreadable: %v", ftsTmpDir, err)
		return
	}
	for _, e := range entries {
		rep.problemf("leftover temporary directory %s/%s (an interrupted verification left text derived from the records here; remove it)", ftsTmpDir, e.Name())
	}
}

// ftsKind is the problem kind (the unit of the cap of 50 listed problems) of base in table.
func ftsKind(table, base string) string {
	if table == FTSSubTable {
		return "fts-sub-" + strings.TrimPrefix(base, "fts-")
	}
	return base
}

func (r *ftsVerify) add(table, base, format string, a ...any) {
	r.problems[table]++
	r.ps.add(ftsKind(table, base), format, a...)
}

// addRecord is add for a problem about record doc: a record is listed once per kind (the first
// difference), so an emptied index lists 50 distinct records rather than 50 postings of one.
func (r *ftsVerify) addRecord(table, base string, doc int64, format string, a ...any) {
	kind := ftsKind(table, base)
	set := r.listed[kind]
	if set == nil {
		set = map[int64]bool{}
		r.listed[kind] = set
	}
	if set[doc] {
		r.problems[table]++
		return
	}
	if len(set) < verifyMaxPerKind {
		set[doc] = true
	}
	r.add(table, base, format, a...)
}

// fail reports an error that stopped a step. An error of the rebuilt index ends the whole check.
func (r *ftsVerify) fail(table string, err error) {
	var ee expectedErr
	if errors.As(err, &ee) {
		r.dead = true
		if errors.Is(err, errExpectedIndexLost) || errors.Is(err, driver.ErrBadConn) {
			r.rep.problemf("%s: %s; the index content is not verified", FTSWordTable, errExpectedIndexLost.Error())
			return
		}
		r.rep.problemf("%s: the rebuilt index could not be read, so the index content is not verified: %q", FTSWordTable, err.Error())
		return
	}
	r.add(table, "fts-unreadable", "%s: the index cannot be read: %q", table, clipText(err.Error(), 300))
}

// rebuild streams the records (one stream, the one the record scan uses) and writes the documents of
// each chunk into the rebuilt index in a transaction of its own. It reports whether the rebuild is complete.
func (r *ftsVerify) rebuild(observe verifyChunkObserver) bool {
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
		stepErr = r.writeDocs(docs)
		if stepErr != nil {
			cancel(stepErr)
			return
		}
		for _, d := range docs {
			r.indexed = append(r.indexed, d.ID)
		}
		docs, size = docs[:0], 0
	}
	// The record problems this stream notices (storage classes, orphan times) were reported by the
	// record scan; they are collected into a report of their own.
	side := &problemSet{rep: &VerifyReport{Problems: []string{}, Notices: []string{}}, counts: map[string]int{}}
	obs := func(_ string, rows int) {
		if observe != nil {
			observe("fts", rows)
		}
	}
	ok := r.c.streamRecords(sctx, side, verifyChunkRows, obs, func(row RecordRow, _ recordRowMeta) {
		if stepErr != nil {
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
	if stepErr != nil {
		r.fail(FTSWordTable, stepErr)
		return false
	}
	incomplete := !ok
	for _, p := range side.rep.Problems {
		if strings.HasPrefix(p, "records holds ") || strings.HasPrefix(p, "records unreadable") {
			incomplete = true
		}
	}
	if incomplete {
		r.rep.problemf("%s: the records could not all be read to rebuild the index, so the index content is not verified: %q", FTSWordTable, clipText(strings.Join(side.rep.Problems, "; "), 400))
		return false
	}
	slices.Sort(r.indexed) // already ascending (keyset order); sorted again so a binary search never depends on it
	return true
}

func (r *ftsVerify) writeDocs(docs []FTSDoc) error {
	tx, err := r.exp.BeginTx(r.ctx, nil)
	if err != nil {
		return expectedErr{fmt.Errorf("begin a rebuild transaction: %w", err)}
	}
	if err := insertFTSDocs(r.ctx, tx, docs); err != nil {
		_ = tx.Rollback()
		return expectedErr{err}
	}
	if err := tx.Commit(); err != nil {
		return expectedErr{fmt.Errorf("commit a rebuild transaction: %w", err)}
	}
	return nil
}

func (r *ftsVerify) hasDoc(id int64) bool {
	_, ok := slices.BinarySearch(r.indexed, id)
	return ok
}

// compareTable compares one index with its rebuild.
func (r *ftsVerify) compareTable(table string) {
	unreadable := false
	for _, step := range []func(string) error{r.compareVocab, r.compareDocsize, r.compareConfig, r.compareTotals} {
		if err := step(table); err != nil {
			r.fail(table, err)
			if r.dead {
				return
			}
			unreadable = true
		}
	}
	r.hook("compared:" + table)
	if r.problems[table] == 0 && !unreadable {
		if err := r.checkReachability(table); err != nil {
			r.fail(table, err)
		}
		r.hook("reached:" + table)
	}
}

// cell is one value of a table together with its storage class.
type cell struct {
	typ  string
	repr string
}

func (c cell) String() string {
	switch c.typ {
	case "integer", "real":
		return c.typ + " " + c.repr
	case "text":
		return "text " + strconv.Quote(clipText(c.repr, 80))
	case "null":
		return "NULL"
	}
	b := []byte(c.repr)
	if len(b) > 32 {
		b = b[:32]
	}
	return c.typ + " x'" + hex.EncodeToString(b) + "'"
}

// cellOf renders a scanned value; the storage class comes from typeof() (the driver may convert).
func cellOf(typ string, v any) cell {
	switch x := v.(type) {
	case nil:
		return cell{typ: typ}
	case []byte:
		return cell{typ, string(x)}
	case string:
		return cell{typ, x}
	case int64:
		return cell{typ, strconv.FormatInt(x, 10)}
	case float64:
		return cell{typ, strconv.FormatFloat(x, 'g', -1, 64)}
	case bool:
		return cell{typ, strconv.FormatBool(x)}
	}
	return cell{typ, fmt.Sprint(v)}
}

// ftsEntry is one (term, record, column, offset) entry of a vocab table.
type ftsEntry struct {
	term string
	doc  int64
	col  int // 0 summary, 1 body
	off  int64
}

var ftsColumnNames = [...]string{"summary", "body"}

func cmpEntry(a, b ftsEntry) int {
	if c := strings.Compare(a.term, b.term); c != 0 {
		return c
	}
	switch {
	case a.doc != b.doc:
		if a.doc < b.doc {
			return -1
		}
		return 1
	case a.col != b.col:
		return a.col - b.col
	case a.off != b.off:
		if a.off < b.off {
			return -1
		}
		return 1
	}
	return 0
}

func (e ftsEntry) String() string {
	return fmt.Sprintf("%q (%s, offset %d)", clipText(e.term, 80), ftsColumnNames[e.col], e.off)
}

var errStop = errors.New("stop")

// vocabIter reads a vocab table in its natural order and checks it: storage classes (typeof of
// every column), and strict increase in (term, record, column, offset).
type vocabIter struct {
	rows     *sql.Rows
	expected bool
	cur      ftsEntry
	has      bool
	prev     ftsEntry
	hasPrev  bool
	onBad    func(msg string)
	onDup    func(e ftsEntry)
	onOrder  func(prev, cur ftsEntry)
}

func (it *vocabIter) wrap(err error) error {
	if it.expected {
		return expectedErr{err}
	}
	return err
}

func (it *vocabIter) next() error {
	for {
		if !it.rows.Next() {
			it.has = false
			if err := it.rows.Err(); err != nil {
				return it.wrap(err)
			}
			return nil
		}
		var tt, td, tc, to string
		var term, doc, col, off any
		if err := it.rows.Scan(&tt, &term, &td, &doc, &tc, &col, &to, &off); err != nil {
			return it.wrap(err)
		}
		e, ok := vocabEntryOf(tt, term, td, doc, tc, col, to, off)
		if !ok {
			msg := fmt.Sprintf("an entry has the storage classes (%s, %s, %s, %s) instead of (text, integer, text, integer) or a column that is neither summary nor body", tt, td, tc, to)
			if it.expected {
				return expectedErr{errors.New("the rebuilt vocab holds " + msg)}
			}
			it.onBad(msg)
			continue
		}
		if it.hasPrev {
			if c := cmpEntry(it.prev, e); c == 0 {
				if it.expected {
					return expectedErr{errors.New("the rebuilt vocab lists an entry twice")}
				}
				it.onDup(e)
				continue
			} else if c > 0 {
				if it.expected {
					return expectedErr{errors.New("the rebuilt vocab is out of order")}
				}
				it.has = false
				it.onOrder(it.prev, e)
				return errStop
			}
		}
		it.prev, it.hasPrev, it.cur, it.has = e, true, e, true
		return nil
	}
}

func vocabEntryOf(tt string, term any, td string, doc any, tc string, col any, to string, off any) (ftsEntry, bool) {
	if tt != "text" || td != "integer" || tc != "text" || to != "integer" {
		return ftsEntry{}, false
	}
	t, ok1 := term.(string)
	d, ok2 := doc.(int64)
	c, ok3 := col.(string)
	o, ok4 := off.(int64)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return ftsEntry{}, false
	}
	e := ftsEntry{term: t, doc: d, off: o}
	switch c {
	case "summary":
		e.col = 0
	case "body":
		e.col = 1
	default:
		return ftsEntry{}, false
	}
	return e, true
}

const vocabSelect = `SELECT typeof(term), term, typeof(doc), doc, typeof(col), col, typeof(offset), offset FROM `

// compareVocab merges the vocab of the case's index with the vocab of the rebuild.
func (r *ftsVerify) compareVocab(table string) error {
	q := vocabSelect + ftsVocabName(table) //nolint:gosec // the table is one of the two FTS table constants
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		fr, err := h.QueryContext(r.ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = fr.Close() }()
		er, err := r.exp.QueryContext(r.ctx, q)
		if err != nil {
			return expectedErr{err}
		}
		defer func() { _ = er.Close() }()
		f := &vocabIter{
			rows:  fr,
			onBad: func(msg string) { r.add(table, "fts-unreadable", "%s: the index lists %s", table, msg) },
			onDup: func(e ftsEntry) {
				r.add(table, "fts-order", "%s: record %d: the index lists %s twice", table, e.doc, e)
			},
			onOrder: func(prev, cur ftsEntry) {
				r.add(table, "fts-order", "%s: the index lists its entries out of order: record %d %s follows record %d %s, so its entries cannot be compared", table, cur.doc, cur, prev.doc, prev)
			},
		}
		e := &vocabIter{rows: er, expected: true}
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
				r.invented(table, f.cur)
				if err := f.next(); err != nil {
					return err
				}
			default:
				r.addRecord(table, "fts-hidden", e.cur.doc,
					"%s: record %d: the index lacks %s, which the text of the record holds (a search hit is hidden)", table, e.cur.doc, e.cur)
				if err := e.next(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

func (r *ftsVerify) invented(table string, e ftsEntry) {
	if r.hasDoc(e.doc) {
		r.addRecord(table, "fts-invented", e.doc,
			"%s: record %d: the index holds %s, which the text of the record does not hold (a search hit is invented)", table, e.doc, e)
		return
	}
	r.addRecord(table, "fts-invented", e.doc,
		"%s: the index holds %s for record %d, which does not exist or has no indexable text (a search hit is invented)", table, e, e.doc)
}

// keyedRow is a row of a small table compared by key: the key and the value cell.
type keyedRow struct {
	key cell
	val cell
}

// scanKeyed reads (typeof(k), k, typeof(v), v) rows from q on any querier of rows.
func scanKeyed(rows *sql.Rows, limit int, wrap func(error) error) ([]keyedRow, error) {
	var out []keyedRow
	for rows.Next() {
		var tk, tv string
		var k, v any
		if err := rows.Scan(&tk, &k, &tv, &v); err != nil {
			return nil, wrap(err)
		}
		out = append(out, keyedRow{cellOf(tk, k), cellOf(tv, v)})
		if len(out) > limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}
	return out, nil
}

// compareDocsize merges the _docsize rows (id, sz) of the case's index with the rebuild's, in id order.
func (r *ftsVerify) compareDocsize(table string) error {
	q := `SELECT typeof(id), id, typeof(sz), sz FROM ` + table + `_docsize ORDER BY id` //nolint:gosec // the table is one of the two FTS table constants
	return r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		fr, err := h.QueryContext(r.ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = fr.Close() }()
		er, err := r.exp.QueryContext(r.ctx, q)
		if err != nil {
			return expectedErr{err}
		}
		defer func() { _ = er.Close() }()
		type row struct {
			id int64
			sz cell
		}
		var fcur, ecur row
		var fhas, ehas bool
		advance := func(rows *sql.Rows, expected bool) (row, bool, error) {
			for rows.Next() {
				var ti, ts string
				var id, sz any
				if err := rows.Scan(&ti, &id, &ts, &sz); err != nil {
					if expected {
						return row{}, false, expectedErr{err}
					}
					return row{}, false, err
				}
				n, ok := id.(int64)
				if ti != "integer" || !ok {
					if expected {
						return row{}, false, expectedErr{errors.New("the rebuilt _docsize holds an id that is not an integer")}
					}
					r.add(table, "fts-unreadable", "%s_docsize: a row has an id of storage class %s", table, ti)
					continue
				}
				return row{n, cellOf(ts, sz)}, true, nil
			}
			if err := rows.Err(); err != nil {
				if expected {
					return row{}, false, expectedErr{err}
				}
				return row{}, false, err
			}
			return row{}, false, nil
		}
		if fcur, fhas, err = advance(fr, false); err != nil {
			return err
		}
		if ecur, ehas, err = advance(er, true); err != nil {
			return err
		}
		for fhas || ehas {
			switch {
			case fhas && (!ehas || fcur.id < ecur.id):
				r.addRecord(table, "fts-size", fcur.id, "%s_docsize: the index holds a size entry for record %d, which does not exist or has no indexable text", table, fcur.id)
				if fcur, fhas, err = advance(fr, false); err != nil {
					return err
				}
			case ehas && (!fhas || ecur.id < fcur.id):
				r.addRecord(table, "fts-size", ecur.id, "%s_docsize: record %d: the size entry is missing", table, ecur.id)
				if ecur, ehas, err = advance(er, true); err != nil {
					return err
				}
			default:
				switch {
				case fcur.sz.typ != ecur.sz.typ:
					r.addRecord(table, "fts-size", fcur.id, "%s_docsize: record %d: the size entry is of storage class %s, a rebuild holds %s", table, fcur.id, fcur.sz.typ, ecur.sz.typ)
				case fcur.sz.repr != ecur.sz.repr:
					r.addRecord(table, "fts-size", fcur.id, "%s_docsize: record %d: the size entry differs from a rebuild (found %s, a rebuild holds %s)", table, fcur.id, fcur.sz, ecur.sz)
				}
				if fcur, fhas, err = advance(fr, false); err != nil {
					return err
				}
				if ecur, ehas, err = advance(er, true); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// compareConfig compares the _config rows (k, v).
func (r *ftsVerify) compareConfig(table string) error {
	q := `SELECT typeof(k), k, typeof(v), v FROM ` + table + `_config ORDER BY k LIMIT ` + strconv.Itoa(ftsMaxConfigRows+1) //nolint:gosec // the table is one of the two FTS table constants
	var found, want []keyedRow
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		rows, err := h.QueryContext(r.ctx, q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		found, err = scanKeyed(rows, ftsMaxConfigRows, func(e error) error { return e })
		return err
	})
	if err != nil {
		return err
	}
	rows, err := r.exp.QueryContext(r.ctx, q)
	if err != nil {
		return expectedErr{err}
	}
	want, err = scanKeyed(rows, ftsMaxConfigRows, func(e error) error { return expectedErr{e} })
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(found) > ftsMaxConfigRows {
		r.add(table, "fts-config", "%s_config: holds more than %d rows", table, ftsMaxConfigRows)
		found = found[:ftsMaxConfigRows]
	}
	byKey := map[cell]cell{}
	for _, w := range want {
		byKey[w.key] = w.val
	}
	seen := map[cell]bool{}
	for _, f := range found {
		seen[f.key] = true
		w, ok := byKey[f.key]
		switch {
		case !ok:
			r.add(table, "fts-config", "%s_config: key %q is not part of a rebuild (the index holds %s)", table, clipText(f.key.repr, 80), f.val)
		case w != f.val:
			r.add(table, "fts-config", "%s_config: key %q: the index holds %s, a rebuild holds %s", table, clipText(f.key.repr, 80), f.val, w)
		}
	}
	keys := make([]cell, 0, len(want))
	for _, w := range want {
		if !seen[w.key] {
			keys = append(keys, w.key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].repr < keys[j].repr })
	for _, k := range keys {
		r.add(table, "fts-config", "%s_config: key %q is missing (a rebuild holds %s)", table, clipText(k.repr, 80), byKey[k])
	}
	return nil
}

// compareTotals compares the totals record (row 1 of _data: the document and token counts bm25 uses), byte for byte.
func (r *ftsVerify) compareTotals(table string) error {
	q := `SELECT typeof(id), id, typeof(block), block FROM ` + table + `_data WHERE id = 1 LIMIT 2` //nolint:gosec // the table is one of the two FTS table constants
	var found, want []keyedRow
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
	rows, err := r.exp.QueryContext(r.ctx, q)
	if err != nil {
		return expectedErr{err}
	}
	want, err = scanKeyed(rows, 2, func(e error) error { return expectedErr{e} })
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(want) != 1 {
		return expectedErr{errors.New("the rebuilt index has no totals record")}
	}
	if len(found) != 1 || found[0] != want[0] {
		r.add(table, "fts-totals", "%s: the index totals differ from a rebuild (ranking is unreliable)", table)
	}
	return nil
}

// ftsQuote renders s as one FTS5 string (a phrase of the tokens of s): quotes are doubled.
func ftsQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// checkReachability proves that every term of the rebuilt vocabulary finds exactly the rebuilt
// documents in the case's index through MATCH. See the comment at the top of the file for why the
// vocab comparison alone is not enough. The oracle is MATCH on the rebuilt index with the same
// expression, so a term the tokenizer would split on a second pass is judged the way a healthy
// index judges it.
func (r *ftsVerify) checkReachability(table string) error {
	after := ""
	listQ := `SELECT typeof(term), term FROM ` + table + `_vr WHERE term > ? ORDER BY term LIMIT ` + strconv.Itoa(ftsReachChunk) //nolint:gosec // the table is one of the two FTS table constants
	problems := 0
	for {
		terms, err := r.nextTerms(listQ, after)
		if err != nil {
			return err
		}
		if len(terms) == 0 {
			return nil
		}
		err = r.c.ReadTx(r.ctx, func(h ReadHandle) error {
			for _, term := range terms {
				msg, err := r.matchDiff(h, table, term)
				if err != nil {
					return err
				}
				if msg == "" {
					continue
				}
				problems++
				if problems <= verifyMaxPerKind {
					r.add(table, "fts-unreachable", "%s", msg)
				}
				if problems >= verifyMaxPerKind {
					r.rep.problemf("%s: the check of what each search finds stopped after %d terms with a wrong result; the later terms were not checked", table, problems)
					return errStop
				}
			}
			return nil
		})
		if errors.Is(err, errStop) {
			return nil
		}
		if err != nil {
			return err
		}
		after = terms[len(terms)-1]
	}
}

// nextTerms lists the next chunk of terms of the rebuild after the given one.
func (r *ftsVerify) nextTerms(q, after string) ([]string, error) {
	rows, err := r.exp.QueryContext(r.ctx, q, after)
	if err != nil {
		return nil, expectedErr{err}
	}
	defer func() { _ = rows.Close() }()
	var terms []string
	for rows.Next() {
		var typ string
		var term any
		if err := rows.Scan(&typ, &term); err != nil {
			return nil, expectedErr{err}
		}
		s, ok := term.(string)
		if typ != "text" || !ok {
			return nil, expectedErr{fmt.Errorf("the rebuilt vocab lists a term of storage class %s", typ)}
		}
		terms = append(terms, s)
	}
	if err := rows.Err(); err != nil {
		return nil, expectedErr{err}
	}
	return terms, nil
}

// matchDiff compares the MATCH results of term on the case's index (inside h) and on the rebuild.
// It returns "" when they are the same document list, otherwise the problem text; an error only for
// a failure of the rebuild (a failure of the case's index is the problem).
func (r *ftsVerify) matchDiff(h ReadHandle, table, term string) (string, error) {
	q := `SELECT typeof(rowid), rowid FROM ` + table + ` WHERE ` + table + ` MATCH ? ORDER BY rowid` //nolint:gosec // the table is one of the two FTS table constants
	match := ftsQuote(term)
	er, err := r.exp.QueryContext(r.ctx, q, match)
	if err != nil {
		return "", expectedErr{err}
	}
	defer func() { _ = er.Close() }()
	fr, err := h.QueryContext(r.ctx, q, match)
	if err != nil {
		return fmt.Sprintf("%s: a search for %q fails: %q", table, clipText(term, 80), clipText(err.Error(), 200)), nil
	}
	defer func() { _ = fr.Close() }()
	next := func(rows *sql.Rows, expected bool) (id int64, ok bool, bad string, err error) {
		if !rows.Next() {
			if e := rows.Err(); e != nil {
				if expected {
					return 0, false, "", expectedErr{e}
				}
				return 0, false, fmt.Sprintf("%s: a search for %q fails: %q", table, clipText(term, 80), clipText(e.Error(), 200)), nil
			}
			return 0, false, "", nil
		}
		var typ string
		var v any
		if e := rows.Scan(&typ, &v); e != nil {
			if expected {
				return 0, false, "", expectedErr{e}
			}
			return 0, false, fmt.Sprintf("%s: a search for %q fails: %q", table, clipText(term, 80), clipText(e.Error(), 200)), nil
		}
		n, isInt := v.(int64)
		if typ != "integer" || !isInt {
			if expected {
				return 0, false, "", expectedErr{errors.New("the rebuilt index returned a rowid that is not an integer")}
			}
			return 0, false, fmt.Sprintf("%s: a search for %q returns a rowid of storage class %s", table, clipText(term, 80), typ), nil
		}
		return n, true, "", nil
	}
	var fid, eid int64
	var fok, eok bool
	var bad string
	if fid, fok, bad, err = next(fr, false); err != nil || bad != "" {
		return bad, err
	}
	if eid, eok, bad, err = next(er, true); err != nil || bad != "" {
		return bad, err
	}
	for fok || eok {
		switch {
		case fok && (!eok || fid < eid):
			return fmt.Sprintf("%s: a search for %q finds record %d, which a rebuild does not (a search hit is invented)", table, clipText(term, 80), fid), nil
		case eok && (!fok || eid < fid):
			return fmt.Sprintf("%s: a search for %q does not find record %d (a search hit is lost: the index structure no longer reaches it)", table, clipText(term, 80), eid), nil
		}
		if fid, fok, bad, err = next(fr, false); err != nil || bad != "" {
			return bad, err
		}
		if eid, eok, bad, err = next(er, true); err != nil || bad != "" {
			return bad, err
		}
	}
	return "", nil
}
