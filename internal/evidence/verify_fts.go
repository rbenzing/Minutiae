package evidence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// a pure function of them (NormalizeText, the two FTS5 tokenizers). Verify RECOMPUTES the index content
// inside the record stream (verify_ftsexpect.go: the digests of the postings per audited batch, the
// _docsize blobs and the totals record, from Go replicas of the tokenizers that a self-test proves against
// the running SQLite) and compares it with what the strict decoder reads from the leaf pages
// (verify_ftsindex.go). That is the index pass, run as the phase fts.index. The index structures the vocab
// cannot see (the shadow table <table>_idx, the doclist indexes, the page footers) are recomputed from the
// leaf pages inside the same pass and compared exactly (fts5idx.go); a difference is confirmed by asking the
// case index for the first term of the affected page.
//
// A clean verify ends there and creates no scratch area. Only when a bucket fails (an audited batch whose digests
// differ, or that holds a delete marker) does verify look at the failing buckets again, with SQLite's own
// tokenizer, to list the records that differ (verify_ftslocal.go, the phase fts.localize): at most 64 buckets,
// in rounds, and every bucket that is not localized is named, never silent.
//
// The case's own database is read through ReadTx, as everywhere else. EXCEPTION to the "read
// everything through ReadHandle" rule: the localization writes two private scratch databases (the
// rebuild by SQLite, and the postings the pages hold with their segment age; unnamed SQLite databases, one
// pinned connection each); they are not the case's database, hold no evidence the case does not hold already,
// and are discarded at the end.
//
// Evidence-derived text never leaves the case directory: the scratch databases and every spill file
// of SQLite live under <case>/tmp/verify-<runid>/ (created 0700, removed when verify ends, on
// every path). SQLite's temp directory is process-wide state (PRAGMA temp_store_directory): it is
// set before the first localization round and reset in a defer. The command line runs one command per process.

const (
	// ftsTmpDir is the directory of the case that holds the private files of a verification.
	ftsTmpDir = "tmp"
	// ftsExpectedCacheKiB is the page cache of the rebuilt index: it stays in memory up to this size and
	// spills to a file in the run directory beyond it.
	ftsExpectedCacheKiB = 262144
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
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
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
	// tmpCreated is set when this run created tmpRoot: only then is it removed again (R65).
	tmpCreated bool
	dir        string // <case>/tmp/verify-<runid>
	tempSet    bool   // SQLite's temp directory was pointed at dir
	exp        *sql.DB
	// readers is the read-only pool of a concurrent verify (verify_readers.go), nil in the serial order
	readers *verifyReaders

	// indexed is the sorted ids of the records that have a document in the rebuild.
	indexed []int64
	// dead is set when the rebuilt index failed: nothing more can be compared.
	dead bool
	// listed holds the records already listed under a problem kind (so one record is listed once).
	listed map[string]map[int64]bool

	// the recomputation (5D-14i): the audited batches, the expected side (filled by the record stream),
	// the postings the leaf pages hold, and what the index pass concluded
	sorted  []auditedBatch
	observe verifyChunkObserver
	ref     ftsReference // the fresh _config rows of each table, from the tokenizer self-test
	haveRef bool
	ex      *ftsExpect
	found   *ftsDigester
	// unrefOps counts the shadow rows and range probes checkUnreferenced touched (TestCheckUnreferencedIsLinear).
	unrefOps int
	// bad holds, per table, the buckets of a delete marker: they are looked at again whatever the sums say
	bad     [2]ftsBucketSet
	failing [2][]ftsBucketKey // the failing buckets of each table, once its pass is done
	// bucketsCompared counts the buckets the digests were compared for (tests read it)
	bucketsCompared int
	// segFailed counts, per table, the segments that did not decode in the index pass (D153): the localization skips such a table
	segFailed [2]int
	// indexFailed is set when the index pass could not read a table
	indexFailed bool
	// lc is the state of the localization (verify_ftslocal.go), nil until a bucket fails
	lc *ftsLocalState
	// idxHits are the differences of the _idx and doclist-index recomputation kept for the confirmation by search
	idxHits []ftsLostHit
}

// newFTSVerify makes the state of one P11 run. It owns nothing that needs cleanup until setup runs.
func (c *Case) newFTSVerify(ctx context.Context, ps *problemSet, sorted []auditedBatch, observe verifyChunkObserver) *ftsVerify {
	return &ftsVerify{
		c: c, ctx: ctx, ps: ps, rep: ps.rep, listed: map[string]map[int64]bool{},
		sorted: sorted, observe: observe,
	}
}

// startFTS runs the tokenizer self-test (before the record stream, in a private in-memory SQLite) and makes
// the expected side the stream consumer feeds. A self-test that fails is a PROBLEM, never a pass: the
// postings, sizes and totals are then not compared (the expected side is disabled and nothing is tokenized),
// and the checks that do not depend on a tokenizer still run.
func (c *Case) startFTS(ctx context.Context, ps *problemSet, sorted []auditedBatch, observe verifyChunkObserver) *ftsVerify {
	r := c.newFTSVerify(ctx, ps, sorted, observe)
	done := c.phase("fts.selftest")
	ref, err := ftsSelfTestCached(ctx)
	done()
	enabled := true
	switch {
	case err == nil:
	case ctxCaused(ctx, err):
		r.rep.cancelCut = true // a cut, reported once by verifyRun
		enabled = false
	case errors.Is(err, errFTSReplica):
		r.rep.problemf("%s: tokenizer replica does not match this SQLite: %s; the postings, sizes and totals of the index are not verified", FTSWordTable, clipText(err.Error(), 400))
		enabled = false
	default:
		r.rep.problemf("%s: the tokenizer replica could not be checked against this SQLite: %q; the postings, sizes and totals of the index are not verified", FTSWordTable, clipText(err.Error(), 400))
		enabled = false
	}
	if ref.config != nil {
		r.ref, r.haveRef = ref, true
	}
	var key [16]byte
	if enabled {
		if _, err := rand.Read(key[:]); err != nil {
			r.rep.problemf("%s: the digest key could not be made, so the postings, sizes and totals of the index are not verified: %q", FTSWordTable, err.Error())
			enabled = false
		}
	}
	ex, err := r.newFTSExpect(sorted, key, enabled)
	if err != nil {
		r.rep.problemf("%s: the expected index could not be set up, so the postings, sizes and totals of the index are not verified: %q", FTSWordTable, err.Error())
		ex, _ = r.newFTSExpect(sorted, key, false)
	}
	r.ex = ex
	if ex.enabled {
		r.found = ex.dig.fork()
	}
	return r
}

// verifyFTS finishes P11 after the record stream: the index pass (strict decoding of the leaf pages, the
// digests compared, totals, configuration and unreferenced rows), then, only when a bucket fails, the
// localization of the failing buckets with SQLite's own tokenizer (verify_ftslocal.go). A clean verify creates
// no scratch area.
func (c *Case) verifyFTS(r *ftsVerify) {
	defer r.cleanup() // runs on a panic too
	if c.verifyFTSDone != nil {
		defer func() { c.verifyFTSDone(r) }()
	}
	doneIndex := c.phase("fts.index")
	r.indexTables()                    // together on the read-only pool of a concurrent verify, in table order otherwise
	if r.ctx.Err() != nil && !r.dead { // the verify ended during the last step: it may have been cut short
		r.rep.cancelCut = true
		r.dead = true
	}
	// the content comparison ran to its end only when the expected side is complete, every table was read, no
	// segment failed to decode and every failing bucket was localized (D153)
	doneIndex()
	r.hook("after-index")
	if c.verifyFTSInspect != nil {
		c.verifyFTSInspect(r)
	}
	complete := r.ex != nil && r.ex.complete && !r.dead && !r.indexFailed && r.segFailed == [2]int{}
	if r.dead {
		// the pass ended early (the context, a lost scratch database): the buckets already known to fail are
		// still named, so a cancel never turns a finding into a clean cut (D162)
		r.reportKnownUnlocalized()
		return
	}
	r.localize(map[string][]ftsBucketKey{FTSWordTable: r.failing[0], FTSSubTable: r.failing[1]})
	if complete && !r.dead && r.localizedAll() {
		r.rep.FTSDocsChecked = int(r.ex.docs)
	}
}

// indexTable runs the steps of the index pass on one table: the leaf pages and their digests, the totals
// record, the configuration and the rows no structure references.
func (r *ftsVerify) indexTable(table string) {
	step := func(name string, f func(string) error) bool {
		done := r.c.phase("fts.index/" + table + "/" + name)
		err := f(table)
		done()
		if err != nil {
			r.fail(table, err)
		}
		return !r.dead
	}
	// the decode step owns its counted phase, because the page total is only known once the structure
	// record is read
	if err := r.indexPass(table); err != nil {
		r.fail(table, err)
		r.indexFailed = true
		if r.dead {
			return
		}
	} else {
		r.failing[ftsTableIndex(table)] = r.failingBuckets(table)
		if slices.Contains(r.failing[ftsTableIndex(table)], ftsOverflowBucket) {
			r.add(table, "fts-digest", "%s: postings sit in more than %d id blocks outside every audited batch (too many unaudited id blocks): the blocks past that are counted together and cannot be told apart", table, ftsMaxOutsideBuckets)
		}
	}
	if !step("totals", r.compareTotalsRecomputed) || !step("config", r.compareConfig) {
		return
	}
	done := r.c.phase("fts.index/" + table + "/unreferenced")
	r.checkUnreferenced(table) // P11: shadow rows no structure references
	done()
}

func (r *ftsVerify) hook(point string) {
	if r.c.verifyFTSHook != nil {
		r.c.verifyFTSHook(point, r.exp)
	}
}

// setup creates the run directory, points SQLite's temp directory at it and creates the rebuilt
// index (both tables and their vocab tables).
func (r *ftsVerify) setup() error {
	abs, err := filepath.Abs(r.c.Dir)
	if err != nil {
		return err
	}
	tmpRoot := filepath.Join(abs, ftsTmpDir)
	switch fi, err := os.Lstat(tmpRoot); {
	case err == nil:
		if !fi.Mode().IsDir() {
			return errTmpNotPlain
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(tmpRoot, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", tmpRoot, err)
		}
		r.tmpCreated = true
	default:
		return fmt.Errorf("inspect %s: %w", tmpRoot, err)
	}
	r.tmpRoot = tmpRoot // only a directory this run checked is ever removed again
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	dir := filepath.Join(r.tmpRoot, "verify-"+hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	r.dir = dir
	if fi, err := os.Lstat(dir); err != nil || !fi.Mode().IsDir() {
		return errTmpNotPlain
	}
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
		for _, stmt := range []string{ddl, vocab} {
			if _, err := r.exp.ExecContext(r.ctx, stmt); err != nil {
				return fmt.Errorf("create the rebuilt %s: %w", table, err)
			}
		}
	}
	// the found postings of a round live in a second private database of the same run directory
	if r.lc == nil {
		r.lc = newFTSLocalState()
	}
	r.lc.loc = sql.OpenDB(&expectedConnector{cacheKiB: r.c.verifyFTSCacheKiB})
	r.lc.loc.SetMaxOpenConns(1)
	r.lc.loc.SetMaxIdleConns(1)
	r.lc.loc.SetConnMaxLifetime(0)
	r.lc.loc.SetConnMaxIdleTime(0)
	for _, stmt := range []string{
		`CREATE TABLE found (tbl INTEGER NOT NULL, term BLOB NOT NULL, doc INTEGER NOT NULL, age INTEGER NOT NULL, del INTEGER NOT NULL, col INTEGER NOT NULL, off INTEGER NOT NULL)`,
		`CREATE INDEX found_k ON found (tbl, term, doc, age)`,
	} {
		if _, err := r.lc.loc.ExecContext(r.ctx, stmt); err != nil {
			return fmt.Errorf("create the found postings: %w", err)
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
	if r.lc != nil && r.lc.loc != nil {
		_ = r.lc.loc.Close()
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
	if r.tmpRoot != "" && r.tmpCreated {
		_ = os.Remove(r.tmpRoot) // only a directory this run created, and only when it is empty
	}
}

// errTmpNotPlain is the refusal of a <case>/tmp (or run directory) that is a symlink, a junction or
// not a directory: nothing is created or listed through it.
var errTmpNotPlain = errors.New("case tmp is not a plain directory")

// checkTmp reports everything left in <case>/tmp/: a verification that was interrupted leaves its
// private files there, and they hold text derived from the records. Like a leftover staging
// directory it is a problem until an examiner removes it; verify never removes what it did not create.
func (c *Case) checkTmp(rep *VerifyReport) {
	fi, err := os.Lstat(filepath.Join(c.Dir, ftsTmpDir))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil && !fi.Mode().IsDir() {
		rep.problemf("%s: %s is a symlink, a junction or not a directory (nothing is created or listed through it; remove it)", errTmpNotPlain, ftsTmpDir)
		return
	}
	d, err := os.Open(filepath.Join(c.Dir, ftsTmpDir))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		rep.problemf("temporary directory %s unreadable: %v", ftsTmpDir, err)
		return
	}
	defer func() { _ = d.Close() }()
	// at most verifyMaxPerKind entries are listed (and read): a hostile tmp of a million entries is
	// one more line, not a million (R65)
	entries, err := d.ReadDir(verifyMaxPerKind + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		rep.problemf("temporary directory %s unreadable: %v", ftsTmpDir, err)
		return
	}
	for i, e := range entries {
		if i == verifyMaxPerKind {
			rep.problemf("further leftover entries of %s are not listed (more than %d)", ftsTmpDir, verifyMaxPerKind)
			break
		}
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
		return
	}
	if len(set) < verifyMaxPerKind {
		set[doc] = true
	}
	r.add(table, base, format, a...)
}

// fail reports an error that stopped a step. An error of the rebuilt index ends the whole check.
func (r *ftsVerify) fail(table string, err error) {
	if ctxCaused(r.ctx, err) {
		r.dead = true
		r.rep.cancelCut = true
		return
	}
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

// compareConfig compares the _config rows (k, v) with the fresh rows of a table made by this SQLite (the
// tokenizer self-test reads them before it indexes anything). Without them (the self-test could not run) the
// step is skipped: its PROBLEM is already in the report.
func (r *ftsVerify) compareConfig(table string) error {
	if !r.haveRef {
		return nil
	}
	want := r.ref.config[table]
	q := `SELECT typeof(k), k, typeof(v), v FROM ` + table + `_config ORDER BY k LIMIT ` + strconv.Itoa(ftsMaxConfigRows+1) //nolint:gosec // the table is one of the two FTS table constants
	var found []keyedRow
	err := r.readTx(func(h ReadHandle) error {
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

// ftsQuote renders s as one FTS5 string (a phrase of the tokens of s): quotes are doubled.
func ftsQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// addUnlisted counts n problems of a kind that are not listed one by one (the caller has already listed
// verifyMaxPerKind of them with add): they appear in the "further ... problems are not listed" line.
func (r *ftsVerify) addUnlisted(table, base string, n int) {
	r.ps.counts[ftsKind(table, base)] += n
}
