package sqlitefile_test

// Engine oracle, history (plan 3I, Task 13): databases written by the engine
// with unique marker values; the generator logs every row it ever wrote. Recall:
// every marker the files physically still hold, that the live state no longer
// shows, is delivered as a recovered row or lies inside a free-space span of a
// page image. Precision: every recovered row is a tuple the generator wrote.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// wrow is one row the generator wrote.
type wrow struct {
	table string
	rowid int64
	vals  []any // as stored: int64 and string
}

type writeLog struct {
	rows   []wrow
	byText map[string][]wrow
}

func newWriteLog() *writeLog { return &writeLog{byText: map[string][]wrow{}} }

func (l *writeLog) add(table string, rowid int64, vals ...any) {
	w := wrow{table, rowid, vals}
	l.rows = append(l.rows, w)
	if len(vals) > 1 {
		if s, ok := vals[1].(string); ok {
			l.byText[s] = append(l.byText[s], w)
		}
	}
}

// has reports whether a row with these values was ever written (to table when
// table is not empty).
func (l *writeLog) has(table string, rowid *int64, vals []any) bool {
	for _, w := range l.rows {
		if table != "" && w.table != table {
			continue
		}
		if rowid != nil && w.rowid != *rowid {
			continue
		}
		if len(w.vals) != len(vals) {
			continue
		}
		same := true
		for i := range vals {
			if !sameValue(w.vals[i], vals[i]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// histGen drives an engine connection and logs what it writes.
type histGen struct {
	t   *testing.T
	db  *sql.DB
	tx  *sql.Tx
	log *writeLog
}

func (g *histGen) ex() execer {
	if g.tx != nil {
		return g.tx
	}
	return g.db
}

func (g *histGen) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.ex().Exec(q, args...); err != nil {
		g.t.Fatalf("engine exec %q: %v", q, err)
	}
}

func (g *histGen) begin() {
	g.t.Helper()
	tx, err := g.db.Begin()
	if err != nil {
		g.t.Fatal(err)
	}
	g.tx = tx
}

func (g *histGen) commit() {
	g.t.Helper()
	if err := g.tx.Commit(); err != nil {
		g.t.Fatal(err)
	}
	g.tx = nil
}

// marker is the unique text of a row version.
func marker(table string, rowid int64, ver int) string {
	return fmt.Sprintf("MK-%s-%06d-v%d-%s", table, rowid, ver, "pad-pad-pad-pad-pad")
}

// ins writes row rowid of version ver into table (columns a integer, b text and
// optionally c integer).
func (g *histGen) ins(table string, rowid int64, ver int, withC bool) {
	g.t.Helper()
	a, b := rowid*1000+int64(ver), marker(table, rowid, ver)
	if withC {
		g.exec(mustSQL(insertABSQL, table), rowid, a, b, int64(7))
		g.log.add(table, rowid, a, b, int64(7))
		return
	}
	g.exec(mustSQL(insertABSQL, table), rowid, a, b)
	g.log.add(table, rowid, a, b)
}

func (g *histGen) upd(table string, rowid int64, ver int) {
	g.t.Helper()
	a, b := rowid*1000+int64(ver), marker(table, rowid, ver)
	g.exec(mustSQL(updateABSQL, table), a, b, rowid)
	g.log.add(table, rowid, a, b)
}

func (g *histGen) del(table string, rowid int64) {
	g.exec(mustSQL(deleteSQL, table), rowid)
}

// histFixture is the files of a scenario and what the generator wrote.
type histFixture struct {
	name                 string
	db, wal, journal     []byte
	log                  *writeLog
	live                 map[string][]engineRow // the engine's view of the files
	expectMethodsPresent []string               // methods that must deliver at least one row
}

func newHistGen(t *testing.T, name string, ps int, mode string) (*histGen, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	e := openEngine(t, path)
	mustExec(t, e, fmt.Sprintf("pragma page_size=%d", ps))
	if mode == "wal" {
		if got := pragmaString(t, e, "journal_mode=wal"); got != "wal" {
			t.Fatalf("journal_mode = %q", got)
		}
		mustExec(t, e, "pragma wal_autocheckpoint=0")
	} else {
		mustExec(t, e, "pragma journal_mode=delete")
	}
	return &histGen{t: t, db: e, log: newWriteLog()}, path
}

func finishFixture(t *testing.T, f *histFixture) *histFixture {
	t.Helper()
	live, err := engineCopyDump(t, f.name+".db", f.db, f.wal, f.journal)
	if err != nil {
		t.Fatalf("%s: the engine cannot read its own files: %v", f.name, err)
	}
	f.live = live
	return f
}

func readAll(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// scenarios ------------------------------------------------------------------

// scenDeleteRollback: rollback mode, secure_delete off. Most rows are deleted
// (whole leaves are freed), two rows are deleted from the middle of a leaf.
func scenDeleteRollback(t *testing.T, secure bool) *histFixture {
	g, path := newHistGen(t, "del", 1024, "delete")
	if secure {
		mustExec(t, g.db, "pragma secure_delete=on")
	} else {
		mustExec(t, g.db, "pragma secure_delete=off")
	}
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 400; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	g.begin()
	for id := int64(1); id <= 330; id++ {
		g.del("t", id)
	}
	g.commit()
	g.del("t", 345)
	g.del("t", 352)
	_ = g.db.Close()
	name := "del"
	if secure {
		name = "delsecure"
	}
	return finishFixture(t, &histFixture{name: name, db: readAll(t, path), log: g.log})
}

// scenDrop: dropped tables. gone3 has a unique shape; gl has the shape of two
// live tables.
func scenDrop(t *testing.T) *histFixture {
	g, path := newHistGen(t, "drop", 1024, "delete")
	g.exec("create table t(a integer, b text)")
	g.exec("create table u(a integer, b text)")
	g.exec("create table pad(z)")
	g.exec("create table gone3(a integer, b text, c integer)")
	g.exec("create table gl(a integer, b text)")
	g.begin()
	g.exec("insert into pad values(1)")
	for id := int64(1); id <= 40; id++ {
		g.ins("t", id, 0, false)
		g.ins("u", id, 0, false)
		g.ins("gone3", id, 0, true)
		g.ins("gl", id, 0, false)
	}
	g.commit()
	g.exec("drop table pad")
	g.exec("drop table gone3")
	g.exec("drop table gl")
	_ = g.db.Close()
	return finishFixture(t, &histFixture{name: "drop", db: readAll(t, path), log: g.log, expectMethodsPresent: []string{sqlitefile.MethodFreelist}})
}

// scenWALPrior: WAL mode, autocheckpoint off. Updates leave older versions in
// older frames; a row inserted and deleted before any checkpoint stays in the
// WAL.
func scenWALPrior(t *testing.T) *histFixture {
	g, path := newHistGen(t, "walprior", 1024, "wal")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 60; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	g.begin()
	for id := int64(1); id <= 10; id++ {
		g.upd("t", id, 1)
	}
	g.commit()
	g.begin()
	g.ins("t", 100, 0, false)
	g.commit()
	g.begin()
	g.del("t", 100)
	g.commit()
	g.begin()
	for id := int64(1); id <= 5; id++ {
		g.upd("t", id, 2)
	}
	g.commit()
	f := copyWALFixture(t, "walprior", path, g, []string{sqlitefile.MethodWALPrior})
	_ = g.db.Close() // a connection left open enlarges the shared page-cache budget and keeps later transactions from spilling
	return f
}

func copyWALFixture(t *testing.T, name, path string, g *histGen, methods []string) *histFixture {
	t.Helper()
	dir := t.TempDir()
	mode := copyFiles(t, dir, path, path+"-wal")
	if mode != copyDirect {
		t.Logf("%s: copy-while-open fallback used: %s", name, mode)
	}
	f := &histFixture{name: name, db: readAll(t, filepath.Join(dir, filepath.Base(path))), wal: readAll(t, filepath.Join(dir, filepath.Base(path)+"-wal")), log: g.log, expectMethodsPresent: methods}
	return finishFixture(t, f)
}

// scenWALStale: after wal_checkpoint(RESTART) and one more transaction the
// WAL holds a new generation followed by the frames of the old one.
func scenWALStale(t *testing.T) *histFixture {
	g, path := newHistGen(t, "walstale", 1024, "wal")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 40; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	g.begin()
	for id := int64(1); id <= 12; id++ {
		g.upd("t", id, 1)
	}
	g.commit()
	var busy, a, b int
	if err := g.db.QueryRow("pragma wal_checkpoint(restart)").Scan(&busy, &a, &b); err != nil || busy != 0 {
		t.Fatalf("checkpoint: busy %d err %v", busy, err)
	}
	g.begin()
	g.upd("t", 40, 1)
	g.commit()
	f := copyWALFixture(t, "walstale", path, g, []string{sqlitefile.MethodWALStale})
	_ = g.db.Close()
	return f
}

// scenWALUncommitted: a small cache and a large open transaction spill
// uncommitted frames into the WAL.
func scenWALUncommitted(t *testing.T) *histFixture {
	g, path := newHistGen(t, "waluncommitted", 1024, "wal")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 300; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	mustExec(t, g.db, "pragma cache_size=10")
	mustExec(t, g.db, "pragma cache_spill=10")
	g.begin()
	for id := int64(1); id <= 300; id++ {
		g.upd("t", id, 1)
	}
	for id := int64(301); id <= 900; id++ {
		g.ins("t", id, 0, false)
	}
	f := copyWALFixture(t, "waluncommitted", path, g, []string{sqlitefile.MethodWALUncommitted})
	g.commit() // after the copy: the engine needs the transaction closed
	_ = g.db.Close()
	return f
}

// scenHotJournal: a hot rollback journal; the database file as found holds the
// uncommitted values.
func scenHotJournal(t *testing.T) *histFixture {
	g, path := newHistGen(t, "hotjournal", 1024, "delete")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 1500; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	mustExec(t, g.db, "pragma cache_size=10")
	mustExec(t, g.db, "pragma cache_spill=10")
	g.begin()
	for id := int64(1); id <= 1500; id++ {
		g.upd("t", id, 1)
	}
	dir := t.TempDir()
	copyFiles(t, dir, path, path+"-journal")
	f := &histFixture{
		name: "hotjournal", db: readAll(t, filepath.Join(dir, filepath.Base(path))), journal: readAll(t, filepath.Join(dir, filepath.Base(path)+"-journal")),
		log: g.log, expectMethodsPresent: []string{sqlitefile.MethodJournalRolledBack},
	}
	g.commit()
	_ = g.db.Close()
	if !bytes.Contains(f.db, []byte("-v1-")) || len(f.journal) < 2048 {
		t.Fatalf("the engine did not spill the open transaction before the copy (db holds v1: %v, journal %d bytes)", bytes.Contains(f.db, []byte("-v1-")), len(f.journal))
	}
	return finishFixture(t, f)
}

// checks ----------------------------------------------------------------------

type occurrence struct {
	file kindFile
	pos  int64
}

type kindFile struct {
	kind sqlitefile.FileKind
	data []byte
}

func occurrences(data []byte, kind sqlitefile.FileKind, text string) []occurrence {
	var out []occurrence
	for off := 0; ; {
		i := bytes.Index(data[off:], []byte(text))
		if i < 0 {
			return out
		}
		out = append(out, occurrence{kindFile{kind, data}, int64(off + i)})
		off += i + 1
	}
}

// rowKey is the identity of a delivered row for the recall check.
func rowText(r sqlitefile.RecoveredRow) string {
	if len(r.Values) > 1 {
		if s, ok := r.Values[1].Text(); ok {
			return s
		}
	}
	return ""
}

func vals(r sqlitefile.RecoveredRow) []any {
	out := make([]any, len(r.Values))
	for i, v := range r.Values {
		out[i] = normValue(v)
	}
	return out
}

// collectHistory returns the recovered rows and the spans of every page image.
func collectHistory(t *testing.T, d *sqlitefile.DB) (rows []sqlitefile.RecoveredRow, spans map[sqlitefile.FileKind][]sqlitefile.Span, st sqlitefile.RowStats, sum sqlitefile.HistSummary) {
	t.Helper()
	h := d.History()
	t.Cleanup(h.Release)
	ctx := context.Background()
	spans = map[sqlitefile.FileKind][]sqlitefile.Span{}
	if err := h.Pages(ctx, func(p sqlitefile.PageImage) bool {
		spans[p.Loc.File] = append(spans[p.Loc.File], p.Spans...)
		return true
	}); err != nil {
		t.Fatalf("history pages: %v", err)
	}
	var err error
	st, err = h.Rows(ctx, func(r sqlitefile.RecoveredRow) bool {
		r.Values = append([]sqlitefile.Value(nil), r.Values...)
		rows = append(rows, r)
		return true
	})
	if err != nil {
		t.Fatalf("history rows: %v", err)
	}
	if sum, err = h.Summary(ctx); err != nil {
		t.Fatalf("history summary: %v", err)
	}
	return rows, spans, st, sum
}

func liveTexts(live map[string][]engineRow) map[string]bool {
	out := map[string]bool{}
	for _, rows := range live {
		for _, r := range rows {
			for _, v := range r.Vals {
				if s, ok := v.(string); ok {
					out[s] = true
				}
			}
		}
	}
	return out
}

// checkHistory runs the recall and the precision checks on a fixture.
func checkHistory(t *testing.T, f *histFixture) (rows []sqlitefile.RecoveredRow) {
	t.Helper()
	d := openLibrary(t, f.db, f.wal, f.journal)
	rows, spans, st, _ := collectHistory(t, d)
	byMethod := map[string]int{}
	delivered := map[string]bool{}
	for _, r := range rows {
		byMethod[r.Method]++
		if s := rowText(r); s != "" {
			delivered[s] = true
		}
	}
	rel := map[string]int{}
	basis := map[string]int{}
	for _, r := range rows {
		rel[r.Method+"/"+string(r.Relation)]++
		basis[string(r.TableBasis)]++
	}
	t.Logf("%s: %d rows %v relations %v bases %v, stats %+v", f.name, len(rows), byMethod, rel, basis, st)
	for _, m := range f.expectMethodsPresent {
		if byMethod[m] == 0 {
			t.Errorf("%s: no row of method %s", f.name, m)
		}
	}
	// precision: every row is a tuple the generator wrote, to its table when the
	// basis is schema or fit
	for _, r := range rows {
		table := ""
		if r.TableBasis == sqlitefile.BasisSchema || r.TableBasis == sqlitefile.BasisFit {
			table = r.Table
		}
		if (r.TableBasis == sqlitefile.BasisFit || r.TableBasis == sqlitefile.BasisGuess) &&
			(r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly)) {
			// never a comparison with a live row (ruling C47: exactly unknown; uncommitted is the origin's fact)
			t.Errorf("%s: a %s label carries relation %s and notes %v (rulings C44, C46: no live comparison, identity-by-fit-only)", f.name, r.TableBasis, r.Relation, r.Notes)
		}
		if r.Index != "" {
			continue // index entries are not rows of a table
		}
		if !f.log.has(table, r.Rowid, vals(r)) {
			t.Errorf("%s: a recovered row was never written (table %q basis %s rowid %v values %v, %s page %d)", f.name, r.Table, r.TableBasis, r.Rowid, vals(r), r.Method, r.Loc.Page)
		}
	}
	// recall: every written marker that is physically in a file and not live
	live := liveTexts(f.live)
	files := []kindFile{{sqlitefile.FileDB, f.db}, {sqlitefile.FileWAL, f.wal}, {sqlitefile.FileJournal, f.journal}}
	var texts []string
	for s := range f.log.byText {
		texts = append(texts, s)
	}
	sort.Strings(texts)
	missed, reachable := 0, 0
	for _, s := range texts {
		if live[s] {
			continue
		}
		for _, kf := range files {
			for _, oc := range occurrences(kf.data, kf.kind, s) {
				reachable++
				if delivered[s] {
					continue
				}
				covered := false
				for _, sp := range spans[kf.kind] {
					if oc.pos >= sp.FileOffset && oc.pos+int64(len(s)) <= sp.FileOffset+int64(sp.Length) {
						covered = true
						break
					}
				}
				if !covered {
					missed++
					if missed <= 5 {
						t.Errorf("%s: %q lies at %s offset %d (page %d), is not live, and is neither delivered nor inside a free-space span",
							f.name, s, kf.kind, oc.pos, oc.pos/1024+1)
					}
				}
			}
		}
	}
	t.Logf("%s: %d reachable occurrences of deleted or superseded markers, %d missed", f.name, reachable, missed)
	if reachable == 0 {
		t.Errorf("%s: the scenario left no physical trace: the recall check is vacuous", f.name)
	}
	return rows
}

// TestHistoryFindsDeletedRowsOracle: recall and precision on engine-written
// scenarios. HistoryExpectation is what each scenario can reach (see the table
// below); a marker the files hold that is neither delivered nor in a span is a
// failure of the library, never an adjusted expectation.
func TestHistoryFindsDeletedRowsOracle(t *testing.T) {
	for _, mk := range []func(*testing.T) *histFixture{
		func(t *testing.T) *histFixture { return scenDeleteRollback(t, false) },
		scenDrop, scenWALPrior, scenWALStale, scenWALUncommitted, scenHotJournal,
	} {
		f := mk(t)
		t.Run(f.name, func(t *testing.T) {
			t.Logf("reachable: %s", historyExpectation[f.name])
			checkHistory(t, f)
		})
	}
}

// historyExpectation lists what is reachable per scenario (documentation the
// tests above enforce).
var historyExpectation = map[string]string{
	"del":            "rows of freed leaves: sqlite-freelist rows; rows deleted inside a live leaf: bytes inside a free-space span (carving is 3J)",
	"drop":           "rows of dropped tables: sqlite-freelist rows; unique shape: fit, lookalike shape: none",
	"walprior":       "old versions in older frames: sqlite-wal-prior; the inserted-then-deleted row: absent from live",
	"walstale":       "old generation frames: sqlite-wal-stale",
	"waluncommitted": "spilled uncommitted frames: sqlite-wal-uncommitted, relation uncommitted",
	"hotjournal":     "the as-found database pages under a hot journal: sqlite-journal-rolledback, relation uncommitted; Live() shows the pre-transaction values",
}

// TestHistoryRowsAreSubsetOfWritten (precision): no recovered row is invented or
// mislabelled, on every scenario (checkHistory) and, here, explicitly for the
// labels: rows of an emptied table that is the only one of its shape are named
// by the fit and equal the table the generator wrote them to; rows of a dropped
// table (no live table has its shape) or of a lookalike are named by nothing.
func TestHistoryRowsAreSubsetOfWritten(t *testing.T) {
	t.Run("emptied table", func(t *testing.T) {
		rows := checkHistory(t, scenDeleteRollback(t, false))
		n := 0
		for _, r := range rows {
			if r.Method != sqlitefile.MethodFreelist {
				continue
			}
			n++
			if r.TableBasis != sqlitefile.BasisFit || r.Table != "t" {
				t.Errorf("freelist row of t: basis %s table %q", r.TableBasis, r.Table)
			}
		}
		if n == 0 {
			t.Error("no freelist row")
		}
	})
	t.Run("dropped tables", func(t *testing.T) {
		rows := checkHistory(t, scenDrop(t))
		var gone3, lookalike int
		for _, r := range rows {
			if r.Method != sqlitefile.MethodFreelist || r.Index != "" {
				continue
			}
			if len(r.Values) == 3 {
				gone3++
			} else if strings.Contains(rowText(r), "-gl-") {
				lookalike++
			}
			if r.TableBasis != sqlitefile.BasisNone || r.Table != "" {
				t.Errorf("a row of a dropped table was named: basis %s table %q (%s)", r.TableBasis, r.Table, rowText(r))
			}
		}
		if gone3 == 0 || lookalike == 0 {
			t.Errorf("rows of gone3: %d, of the lookalike: %d", gone3, lookalike)
		}
	})
}

// findRow returns the delivered row with this marker text and method.
func findRow(rows []sqlitefile.RecoveredRow, text, method string) (sqlitefile.RecoveredRow, bool) {
	for _, r := range rows {
		if rowText(r) == text && r.Method == method {
			return r, true
		}
	}
	return sqlitefile.RecoveredRow{}, false
}

// TestHistoryOracleRelations: the relation of specific rows the generator wrote
// and the engine confirms: an older version of an updated row is a superseded
// version (BasisSchema: the owner of its page, read in the state of the commit
// that wrote the page, is the live table), a row inserted and deleted before any
// checkpoint is absent from live, uncommitted and rolled-back rows are
// uncommitted.
func TestHistoryOracleRelations(t *testing.T) {
	f := scenWALPrior(t)
	rows := checkHistory(t, f)
	for _, c := range []struct {
		text string
		rel  sqlitefile.Relation
	}{
		{marker("t", 1, 0), sqlitefile.RelSupersededVersion},
		{marker("t", 1, 1), sqlitefile.RelSupersededVersion},
		{marker("t", 7, 0), sqlitefile.RelSupersededVersion},
		{marker("t", 100, 0), sqlitefile.RelAbsentFromLive},
	} {
		r, ok := findRow(rows, c.text, sqlitefile.MethodWALPrior)
		if !ok {
			t.Errorf("%s: not delivered as sqlite-wal-prior", c.text)
			continue
		}
		if r.Relation != c.rel || r.TableBasis != sqlitefile.BasisSchema || r.Table != "t" {
			t.Errorf("%s: relation %s basis %s table %q, want %s schema t", c.text, r.Relation, r.TableBasis, r.Table, c.rel)
		}
	}
	u := checkHistory(t, scenWALUncommitted(t))
	if r, ok := findRow(u, marker("t", 5, 1), sqlitefile.MethodWALUncommitted); !ok || r.Relation != sqlitefile.RelUncommitted {
		t.Errorf("uncommitted row 5: %+v %v", r.Relation, ok)
	}
	j := checkHistory(t, scenHotJournal(t))
	if r, ok := findRow(j, marker("t", 5, 1), sqlitefile.MethodJournalRolledBack); !ok || r.Relation != sqlitefile.RelUncommitted || r.Journal == nil || !r.Journal.Applied {
		t.Errorf("rolled-back row 5: %+v %v", r.Relation, ok)
	}
	s := checkHistory(t, scenWALStale(t))
	stale := 0
	for _, r := range s {
		if r.Method == sqlitefile.MethodWALStale {
			stale++
		}
	}
	if stale == 0 {
		t.Error("no stale row")
	}
}

// TestHistoryHotJournalLiveShowsPreTransactionValues: under a hot journal Live()
// presents the values before the open transaction (the engine's view), AsFound()
// the file as the transaction left it, and the history delivers the as-found
// values as rolled-back rows.
func TestHistoryHotJournalLiveShowsPreTransactionValues(t *testing.T) {
	f := scenHotJournal(t)
	d := openLibrary(t, f.db, nil, f.journal)
	v := d.Live()
	got, err := liveDumpRowsErr(t, v)
	v.Release()
	if err != nil {
		t.Fatal(err)
	}
	if diff, _ := diffDumps(got, f.live); diff != "" {
		t.Fatalf("Live() differs from the engine: %s", diff)
	}
	for _, r := range got["t"] {
		if s, _ := r.Vals[1].(string); !strings.Contains(s, "-v0-") {
			t.Fatalf("Live() shows %q: not a pre-transaction value", s)
		}
	}
	af := d.AsFound()
	defer af.Release()
	asFound, aerr := liveDumpRowsErr(t, af)
	if aerr == nil {
		if diff, _ := diffDumps(asFound, f.live); diff == "" {
			t.Error("AsFound() equals the engine's view: it must show the interrupted transaction")
		}
		nv1 := 0
		for _, r := range asFound["t"] {
			if s, _ := r.Vals[1].(string); strings.Contains(s, "-v1-") {
				nv1++
			}
		}
		if nv1 == 0 {
			t.Error("AsFound() shows no uncommitted value")
		}
	}
}

// TestHistoryOwnerReadAtTheEndOfTheTransaction: in a transaction that builds a
// deep tree the interior pages are written after (page numbers above) their
// children, so the state between the frames of that transaction does not reach
// the leaves; the owner of a superseded leaf image is read after the commit.
func TestHistoryOwnerReadAtTheEndOfTheTransaction(t *testing.T) {
	g, path := newHistGen(t, "deep", 512, "wal")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 1500; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	g.begin()
	for id := int64(1); id <= 1500; id += 7 {
		g.upd("t", id, 1)
	}
	g.commit()
	f := copyWALFixture(t, "deep", path, g, []string{sqlitefile.MethodWALPrior})
	_ = g.db.Close()
	rows := checkHistory(t, f)
	n, schema := 0, 0
	for _, r := range rows {
		if r.Method == sqlitefile.MethodWALPrior && r.Origin == sqlitefile.OriginWALSuperseded {
			n++
			if r.TableBasis == sqlitefile.BasisSchema {
				schema++
			}
		}
	}
	if n == 0 || schema != n {
		t.Errorf("%d of %d superseded-frame rows are BasisSchema", schema, n)
	}
}
