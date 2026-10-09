package dbrecover

// Engine scenarios (Task 13): databases written by an independent SQL engine
// (modernc.org/sqlite, a test dependency; the code under test links none).
// The generator logs every tuple it ever writes; the oracle of every recovered
// row is that log. Scenario tables are named gen_* so none is taken for a
// record table. The WAL is copied while a connection is open, as in plan 3I.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func engineOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func engineExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any,
) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("engine exec %q: %v", q, err)
	}
}

// readWithRetry reads a file the engine may hold open (a Windows sharing
// violation is retried, anything else fails).
func readWithRetry(t *testing.T, p string) []byte {
	t.Helper()
	var data []byte
	var err error
	for attempt := range 50 {
		data, err = os.ReadFile(p)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return data
}

// genLog is every tuple the generator wrote: key "<rowid>|<a>|<b>" and
// "<a>|<b>" (an index entry or a fit-named row may lose the rowid).
type genLog struct{ rows, tuples map[string]bool }

func newGenLog() *genLog { return &genLog{rows: map[string]bool{}, tuples: map[string]bool{}} }

func (l *genLog) add(rowid, a int64, b string) {
	l.rows[fmt.Sprintf("%d|%d|%s", rowid, a, b)] = true
	l.tuples[fmt.Sprintf("%d|%s", a, b)] = true
}

// engineFiles are the bytes the scenario left.
type engineFiles struct {
	db, wal []byte
	log     *genLog
	deleted map[int64]string // rowid -> the text of a row the scenario deleted last
}

func genMarker(rowid int64, ver int) string {
	return fmt.Sprintf("EV-%06d-v%d-%s", rowid, ver, "pad-pad-pad-pad-pad-pad")
}

// scenario describes one generated database.
type scenario struct {
	pragmas   []string // run before the table is created
	rows      int64
	text      func(rowid int64, ver int) string
	deleteTo  int64 // delete rowid <= deleteTo
	updateAll bool  // WAL scenarios: rewrite every row (version 1) before the delete
	wal       bool
	vacuum    bool
	pageSize  int
}

func (s scenario) build(t *testing.T) engineFiles {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gen.db")
	db := engineOpen(t, path)
	ps := s.pageSize
	if ps == 0 {
		ps = 1024
	}
	engineExec(t, db, fmt.Sprintf("pragma page_size=%d", ps))
	for _, p := range s.pragmas {
		engineExec(t, db, "pragma "+p)
	}
	if s.wal {
		var mode string
		if err := db.QueryRow("pragma journal_mode=wal").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("journal_mode = %q, %v", mode, err)
		}
		engineExec(t, db, "pragma wal_autocheckpoint=0")
	} else {
		engineExec(t, db, "pragma journal_mode=delete")
	}
	text := s.text
	if text == nil {
		text = genMarker
	}
	f := engineFiles{log: newGenLog(), deleted: map[int64]string{}}
	engineExec(t, db, "create table gen_t(a integer, b text)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	last := map[int64]string{}
	for id := int64(1); id <= s.rows; id++ {
		a, b := id*1000, text(id, 0)
		engineExec(t, tx, "insert into gen_t(rowid,a,b) values(?,?,?)", id, a, b)
		f.log.add(id, a, b)
		last[id] = b
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if s.updateAll {
		tx, err = db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for id := int64(1); id <= s.rows; id++ {
			a, b := id*1000+1, text(id, 1)
			engineExec(t, tx, "update gen_t set a=?, b=? where rowid=?", a, b, id)
			f.log.add(id, a, b)
			last[id] = b
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	engineExec(t, db, "delete from gen_t where rowid <= ?", s.deleteTo)
	for id := int64(1); id <= s.deleteTo; id++ {
		f.deleted[id] = last[id]
	}
	if s.vacuum {
		engineExec(t, db, "vacuum")
	}
	if s.wal {
		f.db = readWithRetry(t, path)
		f.wal = readWithRetry(t, path+"-wal") // copied while the connection is open
		return f
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db = readWithRetry(t, path)
	return f
}

// genRecord is a recovered row read back from the case.
type genRecord struct {
	full    records.Full
	payload map[string]any
	rowid   *json.Number
	a       *json.Number
	b       string
	bOK     bool
	cols    []map[string]any
}

func decodeGen(t *testing.T, recs []records.Full) []genRecord {
	t.Helper()
	var out []genRecord
	for _, f := range recs {
		p, err := decodeJSONNumbers(f.Payload)
		if err != nil {
			t.Fatal(err)
		}
		g := genRecord{full: f, payload: p, cols: columnsOf(p)}
		if n, ok := p["rowid"].(json.Number); ok {
			g.rowid = &n
		}
		if len(g.cols) == 2 {
			if n, ok := g.cols[0]["value"].(json.Number); ok {
				g.a = &n
			}
			g.b, g.bOK = g.cols[1]["value"].(string)
		}
		out = append(out, g)
	}
	return out
}

func (g genRecord) key() string {
	a := "?"
	if g.a != nil {
		a = g.a.String()
	}
	if g.rowid != nil {
		return fmt.Sprintf("%s|%s|%s", g.rowid, a, g.b)
	}
	return fmt.Sprintf("%s|%s", a, g.b)
}

// runEngine stores the scenario files as sibling artifacts and runs the recovery.
func runEngine(t *testing.T, f engineFiles) ([]genRecord, Result) {
	t.Helper()
	c := recordstest.NewCase(t)
	db := addFile(t, c, "/data/gen.db", f.db)
	if f.wal != nil {
		addFile(t, c, "/data/gen.db-wal", f.wal)
	}
	res, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return decodeGen(t, storedRecords(t, c)), res
}

// requireWritten is the precision check: every recovered two-column row was
// written by the generator (every column present, none invented).
func requireWritten(t *testing.T, recs []genRecord, l *genLog) (checked int) {
	t.Helper()
	for _, g := range recs {
		if g.a == nil || !g.bOK {
			continue // not a row of the scenario table's shape: nothing to judge by the log
		}
		checked++
		known := l.tuples
		if g.rowid != nil {
			known = l.rows
		}
		if !known[g.key()] {
			t.Errorf("record %d (%s) holds %q that the generator never wrote", g.full.ID, g.full.Method, g.key())
		}
	}
	return checked
}

func TestEngineFreelistRowsWereWritten(t *testing.T) {
	f := scenario{rows: 300, deleteTo: 250}.build(t)
	recs, res := runEngine(t, f)
	if res.Counts.ByMethod["sqlite-freelist"] == 0 {
		t.Fatalf("no sqlite-freelist row recovered: %v", res.Counts.ByMethod)
	}
	checked := requireWritten(t, recs, f.log)
	if checked < 50 {
		t.Errorf("only %d recovered rows could be judged", checked)
	}
	// recall against the engine's own history: deleted rows were recovered
	seen := map[string]bool{}
	for _, g := range recs {
		seen[g.b] = true
	}
	n := 0
	for _, b := range f.deleted {
		if seen[b] {
			n++
		}
	}
	if n < 50 {
		t.Errorf("only %d of %d deleted rows were recovered", n, len(f.deleted))
	}
}

func TestEngineWALRowsWereWritten(t *testing.T) {
	f := scenario{rows: 200, deleteTo: 150, updateAll: true, wal: true}.build(t)
	if len(f.wal) == 0 {
		t.Fatal("the scenario left no WAL")
	}
	recs, res := runEngine(t, f)
	if checked := requireWritten(t, recs, f.log); checked < 20 {
		t.Errorf("only %d recovered rows could be judged", checked)
	}
	fromWAL := 0
	for _, g := range recs {
		if g.payload["file"] == "wal" {
			fromWAL++
		}
	}
	if fromWAL == 0 {
		t.Errorf("no record comes from the WAL: %v", res.Counts.ByMethod)
	}
}

// TestRecoverSecureDeleteInventsNothing (the fixture and the engine): with secure_delete on
// the engine zeroes what it frees; a recovered row would have to be invented.
func TestRecoverSecureDeleteInventsNothing(t *testing.T) {
	t.Run("fixture", checkSecureDeleteFixture)
	t.Run("engine", checkSecureDeleteEngine)
}

func checkSecureDeleteEngine(t *testing.T) {
	f := scenario{pragmas: []string{"secure_delete=on"}, rows: 300, deleteTo: 250}.build(t)
	recs, _ := runEngine(t, f)
	requireWritten(t, recs, f.log)
	requireNoDeleted(t, recs, f)
}

// TestRecoverVacuumLeavesNothing: a vacuum leaves no deleted row. The rebuild
// does leave stale copies of LIVE rows in the gaps of the new pages (and renumbers
// their rowids): the slack carver delivers those, and each is annotated as equal
// to a live row, never as a deleted version.
func TestRecoverVacuumLeavesNothing(t *testing.T) {
	f := scenario{rows: 300, deleteTo: 250, vacuum: true}.build(t)
	recs, res := runEngine(t, f)
	requireNoDeleted(t, recs, f)
	for _, g := range recs {
		if g.full.Method != "sqlite-page-slack" {
			t.Errorf("record %d (%s) after a vacuum, want only slack copies of live rows", g.full.ID, g.full.Method)
		}
		if g.a != nil && g.bOK && !f.log.tuples[fmt.Sprintf("%s|%s", g.a.String(), g.b)] {
			t.Errorf("record %d holds %q that the generator never wrote", g.full.ID, g.key())
		}
		if notes, _ := g.payload["notes"].([]any); !slices.Contains(notes, any("equals-live-row")) {
			t.Errorf("record %d: notes %v, want equals-live-row (a copy of a live row)", g.full.ID, notes)
		}
	}
	if int64(len(recs)) != res.Counts.Written {
		t.Errorf("%d records stored, Written %d", len(recs), res.Counts.Written)
	}
}

// requireNoDeleted: no record carries the text of a row the scenario deleted.
func requireNoDeleted(t *testing.T, recs []genRecord, f engineFiles) {
	t.Helper()
	for _, g := range recs {
		all := g.full.Summary + g.full.Body + string(g.full.Payload)
		for id, b := range f.deleted {
			if strings.Contains(all, b) {
				t.Errorf("record %d (%s) carries the deleted row %d (%q)", g.full.ID, g.full.Method, id, b)
			}
		}
	}
}

func TestUTF16DatabaseRowsKeepTextExact(t *testing.T) {
	for _, enc := range []string{"UTF-16le", "UTF-16be"} {
		t.Run(enc, func(t *testing.T) {
			f := scenario{
				pragmas: []string{"encoding='" + enc + "'"}, rows: 120, deleteTo: 100, pageSize: 1024,
				text: func(id int64, ver int) string {
					return fmt.Sprintf("café-漢字-\U0001F600-%06d-v%d-%s", id, ver, strings.Repeat("ü", 12))
				},
			}.build(t)
			if got := string(f.db[56:60]); got == "\x01\x00\x00\x00" {
				t.Fatal("the scenario database is UTF-8")
			}
			recs, _ := runEngine(t, f)
			checked := requireWritten(t, recs, f.log)
			if checked < 20 {
				t.Fatalf("only %d recovered rows could be judged", checked)
			}
			for _, g := range recs {
				if g.bOK {
					if e, _ := g.cols[1]["enc"].(string); !strings.Contains(e, "16") {
						t.Errorf("record %d: text column encoding %q, want a UTF-16 form", g.full.ID, e)
					}
				}
			}
		})
	}
}

func TestOverflowRowsCarryChainProvenance(t *testing.T) {
	long := func(id int64, ver int) string {
		var b strings.Builder
		for b.Len() < 2600 {
			fmt.Fprintf(&b, "ov-%06d-v%d-%d|", id, ver, b.Len())
		}
		return b.String()
	}
	f := scenario{rows: 60, deleteTo: 55, text: long}.build(t)
	recs, _ := runEngine(t, f)
	requireWritten(t, recs, f.log)
	const ps = 1024
	verified := 0
	for _, g := range recs {
		ov, ok := g.payload["overflow"].(map[string]any)
		if !ok {
			continue
		}
		cell := f.db[g.full.Range.Offset : g.full.Range.Offset+g.full.Range.Length]
		first := binary.BigEndian.Uint32(cell[len(cell)-4:])
		var chain []uint32
		for pg := first; pg != 0 && len(chain) < 64; {
			chain = append(chain, pg)
			if int64(pg)*ps > int64(len(f.db)) {
				break
			}
			pg = binary.BigEndian.Uint32(f.db[int64(pg-1)*ps:])
		}
		parts, _ := ov["pages"].([]any)
		if len(parts) == 0 || len(parts) > len(chain) {
			t.Errorf("record %d: %d overflow pages recorded, the raw chain has %d", g.full.ID, len(parts), len(chain))
			continue
		}
		for i, p := range parts {
			m, _ := p.(map[string]any)
			if n, _ := m["page"].(json.Number); n.String() != fmt.Sprint(chain[i]) || m["file"] != "db" {
				t.Errorf("record %d: overflow part %d is %v, the raw chain says page %d of the db", g.full.ID, i, m, chain[i])
			}
		}
		if n, _ := ov["total"].(json.Number); n.String() != fmt.Sprint(len(parts)) && g.payload["truncated"] != true {
			t.Errorf("record %d: overflow total %v, %d pages listed", g.full.ID, ov["total"], len(parts))
		}
		if g.payload["truncated"] != true && ov["mixed"] != true && len(parts) == len(chain) && g.bOK {
			verified++
			if !f.log.rows[g.key()] {
				t.Errorf("record %d: the value rebuilt from the chain was never written", g.full.ID)
			}
		}
	}
	if verified < 3 {
		t.Errorf("only %d recovered rows with a verified overflow chain", verified)
	}
}
