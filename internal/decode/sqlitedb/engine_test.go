package sqlitedb_test

// Engine oracles for the row layer (spec 10.2 and 14): the layer's rows are
// compared with what the SQLite engine returns for a COPY of the same files.

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// knownEngineRefusals names the fixtures on which the engine itself refuses the
// files (or a table); the oracle's live section is the reference there. Every
// entry needs a reason.
var knownEngineRefusals = map[string]string{}

// engineDump returns the engine's rows of every ordinary table of eng, or an
// error when the engine refuses.
func engineDump(t testing.TB, eng *sql.DB, d *sqlitedb.DB) (map[string][][]ev, error) {
	t.Helper()
	names, err := engineTables(eng)
	if err != nil {
		return nil, err
	}
	out := map[string][][]ev{}
	ltables, err := d.Tables(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for n := range names {
		if !slices.Contains(ltables, n) {
			t.Errorf("table %q is in the engine's schema but not in the layer's", n)
			continue
		}
		tb, err := d.Table(t.Context(), n, nil, nil)
		if err != nil {
			t.Errorf("table %q: %v", n, err)
			continue
		}
		_, cols := visibleCols(tb)
		rows, err := engineRows(eng, n, cols, tb.WithoutRowid())
		if err != nil {
			return nil, err
		}
		out[n] = rows
	}
	for _, n := range ltables {
		if !names[n] {
			t.Errorf("table %q is in the layer's schema but not in the engine's", n)
		}
	}
	return out, nil
}

// compareWithEngine asserts the layer's rows equal the engine's, table by
// table, and returns the number of tables compared. refused is true when the
// engine refused the files (nothing was compared).
func compareWithEngine(t *testing.T, eng *sql.DB, d *sqlitedb.DB) (tables int, refused error) {
	t.Helper()
	want, err := engineDump(t, eng, d)
	if err != nil {
		return 0, err
	}
	got := layerDump(t, d, func(n string, err error) { t.Errorf("layer cannot resolve table %q: %v", n, err) })
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if msg := diffRows(want[n], got[n]); msg != "" {
			t.Errorf("table %s: %s", n, msg)
		}
	}
	return len(names), nil
}

// compareWithOracle asserts the layer's rows equal the fixture oracle's live
// section.
func compareWithOracle(t *testing.T, exp *fixtureExpect, d *sqlitedb.DB) {
	t.Helper()
	got := layerDump(t, d, func(n string, err error) { t.Errorf("layer cannot resolve table %q: %v", n, err) })
	for n, lt := range exp.Live {
		want, err := oracleRows(lt)
		if err != nil {
			t.Fatal(err)
		}
		if msg := diffRows(want, got[n]); msg != "" {
			t.Errorf("table %s against the oracle: %s", n, msg)
		}
	}
	for n := range got {
		if _, ok := exp.Live[n]; !ok {
			t.Errorf("table %s is not in the oracle's live section", n)
		}
	}
}

func TestSQLiteDBMatchesEngine(t *testing.T) {
	names := []string{
		"basic-utf8-4k", "utf16le-1k", "utf16be-512", "sqlite3-utf16", "addcolumn-short", "overflow-wide",
		"without-rowid", "autovacuum-incr", "secure-delete", "freelist-dropped", "sqlite3-freelist",
		// the database file alone of the companion fixtures (the brief's eleven hold 21 tables)
		"wal-uncheckpointed", "wal-stale-generation", "hot-journal", "persist-journal", "builder-hot-journal",
	}
	fixtures, tables := 0, 0
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			db, _, _, exp, err := readFixture(fixtureDir, name)
			if err != nil {
				t.Fatal(err)
			}
			d := openBytes(t, db, nil, nil, bigBudget())
			eng, _ := engineOnCopy(t, db, nil, nil)
			n, refused := compareWithEngine(t, eng, d)
			if refused != nil {
				reason, ok := knownEngineRefusals[name]
				if !ok {
					t.Fatalf("the engine refuses %s and it is not a known refusal: %v", name, refused)
				}
				t.Logf("engine refuses (%s): %v", reason, refused)
				compareWithOracle(t, exp, d)
				return
			}
			fixtures++
			tables += n
		})
	}
	if fixtures < 10 || tables < 25 {
		t.Errorf("compared %d fixtures and %d tables with the engine, want at least 10 and 25", fixtures, tables)
	}
}

func TestLiveWALRowsMatchEngine(t *testing.T) {
	for _, name := range []string{"wal-uncheckpointed", "wal-stale-generation", "sqlite3-wal-killed", "builder-wal-generations"} {
		t.Run(name, func(t *testing.T) {
			db, wal, _, exp, err := readFixture(fixtureDir, name)
			if err != nil {
				t.Fatal(err)
			}
			if wal == nil {
				t.Fatal("the fixture has no WAL")
			}
			d := openBytes(t, db, wal, nil, bigBudget())
			eng, path := engineOnCopy(t, db, wal, nil)
			n, refused := compareWithEngine(t, eng, d)
			if refused != nil {
				reason, ok := knownEngineRefusals[name]
				if !ok {
					t.Fatalf("the engine refuses %s and it is not a known refusal: %v", name, refused)
				}
				t.Logf("engine refuses (%s): %v; falling back to the oracle", reason, refused)
				compareWithOracle(t, exp, d)
				return
			}
			if n == 0 {
				t.Fatal("no table compared")
			}
			// The checkpoint form: the engine moves the committed frames into the
			// database file, which is then opened alone.
			if _, err := eng.Exec("pragma wal_checkpoint(truncate)"); err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}
			alone := openEngine(t, path)
			if _, err := os.Stat(path + "-wal"); err == nil {
				if fi, _ := os.Stat(path + "-wal"); fi != nil && fi.Size() != 0 {
					t.Fatalf("the WAL still holds %d bytes after the checkpoint", fi.Size())
				}
			}
			if _, refused := compareWithEngine(t, alone, d); refused != nil {
				t.Fatalf("the engine refuses the checkpointed file: %v", refused)
			}
		})
	}
}

func TestHotJournalReportedAndRolledBackInLive(t *testing.T) {
	for _, name := range []string{"hot-journal", "sqlite3-hot-journal-killed", "builder-hot-journal", "builder-journal-multiseg"} {
		t.Run(name, func(t *testing.T) {
			db, _, journal, exp, err := readFixture(fixtureDir, name)
			if err != nil {
				t.Fatal(err)
			}
			if journal == nil {
				t.Fatal("the fixture has no journal")
			}
			d := openBytes(t, db, nil, journal, bigBudget())
			j := d.Info().Journal
			if j == nil || !j.Hot || !j.Applied {
				t.Fatalf("Info().Journal = %+v, want Hot and Applied", j)
			}
			warned := false
			for _, w := range d.Warnings() {
				if w.Code == "journal-hot" {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no journal-hot warning among %v", d.Warnings())
			}
			eng, _ := engineOnCopy(t, db, nil, journal)
			if _, refused := compareWithEngine(t, eng, d); refused != nil {
				reason, ok := knownEngineRefusals[name]
				if !ok {
					t.Fatalf("the engine refuses %s and it is not a known refusal: %v", name, refused)
				}
				t.Logf("engine refuses (%s): %v; falling back to the oracle", reason, refused)
				compareWithOracle(t, exp, d)
			}
			// Non-vacuity: without the journal the rows are not the same.
			raw := openBytes(t, db, nil, nil, bigBudget())
			a := layerDump(t, d, func(n string, err error) { t.Errorf("table %q: %v", n, err) })
			b := layerDump(t, raw, func(n string, err error) { t.Errorf("table %q: %v", n, err) })
			differs := false
			for n := range a {
				if diffRows(a[n], b[n]) != "" {
					differs = true
				}
			}
			if !differs {
				t.Error("the rolled-back rows equal the raw database's: the journal changes nothing, the test would be vacuous")
			}
			// The cells the rollback restored lie in the journal, or the oracle says
			// which pages differ between its live and as-found views.
			journalCells := 0
			for n := range a {
				tb, err := d.Table(t.Context(), n, nil, nil)
				if err != nil {
					continue
				}
				_ = tb.Scan(t.Context(), func(r sqlitedb.Row) error {
					if _, role := r.Range(); role == parse.RoleJournal {
						journalCells++
					}
					return nil
				})
			}
			if journalCells == 0 && len(exp.AsFound) == 0 {
				t.Error("no row lies in the journal and the oracle has no as_found view to explain the difference")
			}
			if journalCells == 0 && len(exp.AsFound) > 0 {
				t.Logf("no journal-role cell (rolled-back pages are database pages); the oracle's as_found differs from live")
			}
		})
	}
}

func TestShortRecordsMatchEngineDefaults(t *testing.T) {
	db, _, _, _, err := readFixture(fixtureDir, "addcolumn-short")
	if err != nil {
		t.Fatal(err)
	}
	d := openBytes(t, db, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "acct", []string{"status", "score", "ratio", "memo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := engineOn(t, db)
	status, score, ratio, memo := tb.Col("status"), tb.Col("score"), tb.Col("ratio"), tb.Col("memo")
	short := 0
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.State(status) != sqlitedb.StateDefaulted {
			return
		}
		short++
		id, _ := r.Rowid()
		var es, em sql.NullString
		var esc int64
		var er float64
		if err := eng.QueryRow("select status, score, ratio, memo from acct where id = ?", id).Scan(&es, &esc, &er, &em); err != nil {
			t.Fatal(err)
		}
		if s, ok := r.Text(status); !ok || string(s) != "new" || !es.Valid || es.String != "new" {
			t.Errorf("rowid %d: status = %q %v, engine %v", id, s, ok, es)
		}
		if v, ok := r.Int(score); !ok || v != 10 || esc != 10 {
			t.Errorf("rowid %d: score = %d %v, engine %d", id, v, ok, esc)
		}
		if v, ok := r.Float(ratio); !ok || v != 0.5 || er != 0.5 {
			t.Errorf("rowid %d: ratio = %v %v, engine %v", id, v, ok, er)
		}
		if !r.IsNull(memo) || em.Valid {
			t.Errorf("rowid %d: memo is null = %v, engine valid = %v", id, r.IsNull(memo), em.Valid)
		}
	})
	if short == 0 {
		t.Fatal("the fixture holds no short record")
	}
}

func TestRowidAliasAndPrimaryKeyShapesMatchEngine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.db")
	w, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	shapes := []struct {
		name, ddl, ins string
		without        bool
	}{
		{"a", `create table a(id integer primary key, v)`, `insert into a(id, v) values(NULL, 'nullid'), (7, 'seven'), (NULL, 'again')`, false},
		{"b", `create table b(id integer primary key desc, v)`, `insert into b(id, v) values(NULL, 'nullid'), (7, 'seven'), (NULL, 'again')`, false},
		{"c", `create table c(id int primary key, v)`, `insert into c(id, v) values(NULL, 'nullid'), (7, 'seven'), (NULL, 'again')`, false},
		{"d", `create table d(id integer, v, primary key(id))`, `insert into d(id, v) values(NULL, 'nullid'), (7, 'seven'), (NULL, 'again')`, false},
		{"e", `create table e(id integer primary key, v) without rowid`, `insert into e values(5, 'x'), (2, 'y')`, true},
		{"Mixed Case", `create table "Mixed Case"("Col Name" text)`, `insert into "Mixed Case" values('p'), ('q')`, false},
		{"é", `create table "é"(x)`, `insert into "é" values('p'), ('q')`, false},
	}
	for _, s := range shapes {
		if _, err := w.Exec(s.ddl); err != nil {
			t.Fatalf("%s: %v", s.ddl, err)
		}
		if _, err := w.Exec(s.ins); err != nil {
			t.Fatalf("%s: %v", s.ins, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	eng := engineOn(t, data)
	d := openBytes(t, data, nil, nil, bigBudget())
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			tb, err := d.Table(t.Context(), s.name, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, cols := visibleCols(tb)
			want, err := engineRows(eng, s.name, cols, tb.WithoutRowid())
			if err != nil {
				t.Fatal(err)
			}
			if msg := diffRows(want, layerRows(t, tb)); msg != "" {
				t.Errorf("rows: %s", msg)
			}
			// Skipped here: the engine's NULL-id answer is not consulted for the
			// quoted-name shapes (their id column is not the alias) and for the
			// WITHOUT ROWID shapes (no rowid exists to compare); both are only
			// required not to be reported as a rowid alias.
			if s.name == "Mixed Case" || s.name == "é" || s.without {
				if tb.Cols()[0].RowidAlias {
					t.Errorf("%s: column 0 reported as a rowid alias", s.name)
				}
				return
			}
			// The engine's own answer: when the stored id is NULL, does the select
			// return the rowid (an alias) or NULL?
			var id sql.NullInt64
			var rowid int64
			if err := eng.QueryRow(`select id, rowid from `+quoteIdent(s.name)+` where v = 'nullid'`).Scan(&id, &rowid); err != nil { //nolint:gosec // quoted identifier in a scratch database
				t.Fatal(err)
			}
			engineAlias := id.Valid && id.Int64 == rowid
			if got := tb.Cols()[0].RowidAlias; got != engineAlias {
				t.Errorf("%s: RowidAlias = %v, the engine returns id=%v for a NULL id with rowid %d", s.name, got, id, rowid)
			}
		})
	}
}
