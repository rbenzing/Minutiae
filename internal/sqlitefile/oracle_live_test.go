package sqlitefile_test

// Engine oracle, live state (plan 3I, Task 13): databases written by the
// independent engine are read by the library and compared with what the engine
// reads. The engine decides; an expectation is never adjusted to the library.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// liveScenario is one cell of the matrix.
type liveScenario struct {
	pageSize int
	enc      string // "UTF-8", "UTF-16le", "UTF-16be"
	av       int    // auto_vacuum 0 none, 1 full, 2 incremental
}

func (s liveScenario) String() string {
	return fmt.Sprintf("ps%d-%s-av%d", s.pageSize, s.enc, s.av)
}

// reducedLiveMatrix: page size {512, 4096, 65536} x encoding {UTF-8, UTF-16le}
// x auto_vacuum {none, full} = 12 scenarios (ruling 5).
func reducedLiveMatrix() []liveScenario {
	var out []liveScenario
	for _, ps := range []int{512, 4096, 65536} {
		for _, enc := range []string{"UTF-8", "UTF-16le"} {
			for _, av := range []int{0, 1} {
				out = append(out, liveScenario{ps, enc, av})
			}
		}
	}
	return out
}

func mustExec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		short := q
		if len(short) > 120 {
			short = short[:120]
		}
		t.Fatalf("engine exec %q: %v", short, err)
	}
}

// payloadBlobLen returns n such that a row (rowid alias NULL, one blob of n
// bytes) has a record payload of exactly target bytes.
func payloadBlobLen(target int) int {
	for n := target; n > 0; n-- {
		serial := 12 + 2*n
		sl := 1
		for v := serial; v >= 128; v >>= 7 {
			sl++
		}
		if 1+1+sl+n == target {
			return n
		}
	}
	panic("no blob length")
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func textOf(n int, seed byte) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		fmt.Fprintf(&sb, "t%03d-%c;", i%1000, 'a'+(int(seed)+i)%26)
	}
	return sb.String()[:n]
}

// buildLiveDB writes the scenario database through the engine and returns the
// closed file's path. Every feature the brief lists is in it.
func buildLiveDB(t testing.TB, s liveScenario) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.db")
	db := openEngine(t, path)
	mustExec(t, db, "pragma page_size="+strconv.Itoa(s.pageSize))
	mustExec(t, db, "pragma encoding='"+s.enc+"'")
	mustExec(t, db, "pragma auto_vacuum="+strconv.Itoa(s.av))
	mustExec(t, db, "pragma journal_mode=delete")
	ddl := []string{
		"create table kinds(a, b text, c blob, d real, e integer, f numeric)",
		"create table al(id integer primary key, v)",
		"create table dsc(id integer primary key desc, v)",
		"create table wr(a text, b integer, c, primary key(b, a)) without rowid",
		"create table over(id integer primary key, p)",
		"create table overt(id integer primary key, p text)",
		"create table alt(a)",
		"create table gen(a, b, s as (a + b) stored, v as (a * 2) virtual)",
		"create table rl(r real, n numeric, i integer)",
		"create table st(a integer, b text, c any) strict",
		"create table emp(a, b)",
		"create table sq(id integer primary key autoincrement, v)",
		"create table log(m)",
	}
	for _, q := range ddl {
		mustExec(t, db, q)
	}
	mustExec(t, db, "create table wide("+wideColumns(300)+")")

	// every value kind, negative and huge rowids
	kinds := []struct {
		rowid int64
		vals  []any
	}{
		{-5, []any{nil, nil, nil, nil, nil, nil}},
		{0, []any{int64(0), "", []byte{}, 0.0, int64(0), int64(0)}},
		{1, []any{int64(-1), "x", []byte{0}, 1.5, int64(127), 2.5}},
		{2, []any{int64(128), "héllo ☃ \U0001d11e", []byte{1, 2, 3}, -1e300, int64(-128), int64(32768)}},
		{3, []any{int64(math.MaxInt32) + 1, "text", pattern(10, 1), 3.0, int64(1) << 47, "12"}},
		{math.MaxInt64, []any{int64(math.MaxInt64), "max", nil, 4.0, int64(math.MaxInt64), "x"}},
		{math.MinInt64, []any{int64(math.MinInt64), "min", nil, 5.0, int64(math.MinInt64), nil}},
		{40, []any{1.25, textOf(300, 3), pattern(300, 9), 1e-300, int64(5), 7.0}},
	}
	for _, k := range kinds {
		mustExec(t, db, "insert into kinds(rowid,a,b,c,d,e,f) values(?,?,?,?,?,?,?)", append([]any{k.rowid}, k.vals...)...)
	}
	for _, id := range []int64{5, -3, 1, math.MaxInt64, math.MinInt64, 0, 100000} {
		mustExec(t, db, "insert into al(id,v) values(?,?)", id, fmt.Sprintf("al%d", id))
		mustExec(t, db, "insert into dsc(id,v) values(?,?)", id, fmt.Sprintf("dsc%d", id))
	}
	mustExec(t, db, "insert into dsc(v) values('auto')")
	// WITHOUT ROWID with a composite key inserted out of order
	for _, r := range []struct {
		a string
		b int64
		c any
	}{{"z", 3, "c1"}, {"a", 3, nil}, {"m", 1, []byte{9}}, {"b", 2, 2.5}, {"a", 1, int64(-7)}, {"", 2, "empty"}} {
		mustExec(t, db, "insert into wr values(?,?,?)", r.a, r.b, r.c)
	}
	// overflow: one byte over X, exactly X, ten pages
	x := s.pageSize - 35
	for i, n := range []int{payloadBlobLen(x - 1), payloadBlobLen(x), payloadBlobLen(x + 1), 10 * s.pageSize, 3*s.pageSize + 17} {
		mustExec(t, db, "insert into over(id,p) values(?,?)", int64(i+1), pattern(n, byte(i)))
	}
	mustExec(t, db, "insert into overt(id,p) values(1,?)", textOf(10*s.pageSize, 5))
	mustExec(t, db, "insert into overt(id,p) values(2,?)", textOf(s.pageSize/2, 6))
	// ALTER TABLE ADD COLUMN after rows exist
	for i := range 6 {
		mustExec(t, db, "insert into alt values(?)", int64(i))
	}
	mustExec(t, db, "alter table alt add column b default 5")
	mustExec(t, db, "alter table alt add column c default 'lit'")
	mustExec(t, db, "alter table alt add column d default -5")
	mustExec(t, db, "alter table alt add column f default x'ab'")
	mustExec(t, db, "alter table alt add column g default true")
	mustExec(t, db, "alter table alt add column e real default 7")
	mustExec(t, db, "insert into alt(a,b,c,d,e,f,g) values(100, 1, 'new', 4, 8.5, x'01', 0)")
	// generated columns
	for i := range 5 {
		mustExec(t, db, "insert into gen(a,b) values(?,?)", int64(i), int64(i*10))
	}
	// REAL columns holding integral values
	mustExec(t, db, "insert into rl values(3, 4.0, 5.0)")
	mustExec(t, db, "insert into rl values(-1.0, 0.0, 6)")
	// strict
	mustExec(t, db, "insert into st values(1,'s',2.5)")
	mustExec(t, db, "insert into st values(2,'t',x'00ff')")
	// wide: a row with 300 columns
	wide := make([]any, 300)
	for i := range wide {
		wide[i] = int64(i)
	}
	mustExec(t, db, "insert into wide values("+strings.TrimSuffix(strings.Repeat("?,", 300), ",")+")", wide...)
	// sqlite_sequence
	for i := range 4 {
		mustExec(t, db, "insert into sq(v) values(?)", fmt.Sprintf("sq%d", i))
	}
	mustExec(t, db, "delete from sq where id = 4") // the sequence stays at 4
	// indexes, view, trigger
	mustExec(t, db, "create index ix on kinds(a)")
	mustExec(t, db, "create index ixp on kinds(e) where e > 0")
	mustExec(t, db, "create index ixe on kinds(lower(b))")
	mustExec(t, db, "create index ixo on over(p)")
	mustExec(t, db, "create unique index ixu on al(v)")
	mustExec(t, db, "create view vw as select a from kinds")
	mustExec(t, db, "create trigger tg after insert on al begin insert into log values('al'); end")
	mustExec(t, db, "insert into al(id,v) values(9, 'fired')")
	// a freelist: a table with ten pages dropped
	mustExec(t, db, "create table tmp(p)")
	for i := range 6 {
		mustExec(t, db, "insert into tmp values(?)", pattern(2*s.pageSize, byte(i)))
	}
	mustExec(t, db, "drop table tmp")
	mustExec(t, db, "pragma user_version=-42")
	mustExec(t, db, "pragma application_id=1296125265")
	return path
}

func wideColumns(n int) string {
	cols := make([]string, n)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return strings.Join(cols, ",")
}

// enginePragmas reads the header values the engine reports.
func enginePragmas(t testing.TB, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range []string{"page_size", "page_count", "freelist_count", "user_version", "application_id", "encoding", "auto_vacuum", "schema_version"} {
		out[p] = pragmaString(t, db, p)
	}
	return out
}

// compareInfo checks Info against the engine's pragmas.
func compareInfo(t testing.TB, info sqlitefile.Info, p map[string]string) {
	t.Helper()
	enc := map[sqlitefile.Encoding]string{sqlitefile.EncUTF8: "UTF-8", sqlitefile.EncUTF16LE: "UTF-16le", sqlitefile.EncUTF16BE: "UTF-16be"}[info.Encoding]
	got := map[string]string{
		"page_size":      strconv.Itoa(info.PageSize),
		"page_count":     strconv.Itoa(int(info.PageCount)),
		"freelist_count": strconv.Itoa(int(info.FreelistCount)),
		"user_version":   strconv.Itoa(int(info.UserVersion)),
		"application_id": strconv.Itoa(int(int32(info.ApplicationID))),
		"encoding":       enc,
		"auto_vacuum":    strconv.Itoa(int(info.AutoVacuum)),
		"schema_version": strconv.Itoa(int(info.SchemaCookie)),
	}
	for k, want := range p {
		if got[k] != want {
			t.Errorf("Info %s = %q, engine pragma = %q", k, got[k], want)
		}
	}
}

// TestLiveMatchesEngine: for every scenario of the reduced matrix, Live() rows
// (Resolved) equal the engine's for every table, Info equals the pragmas and a
// clean database raises no warning.
func TestLiveMatchesEngine(t *testing.T) {
	for _, s := range reducedLiveMatrix() {
		t.Run(s.String(), func(t *testing.T) { runLiveScenario(t, s) })
	}
}

func runLiveScenario(t *testing.T, s liveScenario) {
	path := buildLiveDB(t, s)
	e := openEngine(t, path)
	want := engineDumpRows(t, e)
	pragmas := enginePragmas(t, e)
	if got := pragmaString(t, e, "integrity_check"); got != "ok" {
		t.Fatalf("integrity_check = %q", got)
	}
	_ = e.Close()
	data := readFileRetry(t, path)
	d := openLibrary(t, data, nil, nil)
	v := d.Live()
	defer v.Release()
	compareInfo(t, d.Info(), pragmas)
	got := liveDumpRows(t, v)
	diff, omitted := diffDumps(got, want)
	if diff != "" {
		t.Fatalf("%s: %s", s, diff)
	}
	t.Logf("%s: %d tables compared, %d omitted-flagged values skipped", s, len(want), omitted)
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("warnings on a clean database: %v", w)
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("view warnings on a clean database: %v", w)
	}
	// the tables the matrix promises are all there
	for _, n := range []string{"kinds", "al", "dsc", "wr", "over", "overt", "alt", "gen", "rl", "st", "emp", "wide", "sq", "sqlite_sequence", "log"} {
		if _, ok := got[n]; !ok {
			t.Errorf("table %s missing from the dump", n)
		}
	}
	if _, err := v.Table(context.Background(), "kinds"); err != nil {
		t.Fatal(err)
	}
}

// oracleOmittedAllowed documents the deliberate deviation: a value the library
// cannot materialize (a virtual generated column is not read at all, a
// non-literal ADD COLUMN default) is flagged Omitted, never guessed; the matrix
// skips it and counts it.
var _ = oracleOmittedAllowed

const oracleOmittedAllowed = "omitted-flagged"

// TestEngineRefusesNonConstantAddColumnDefault records why the matrix has no
// non-literal ADD COLUMN default: the engine refuses one, so no database holds
// a record shorter than its table whose missing column has an expression
// default. The ADD COLUMN defaults the matrix uses are a literal, a signed
// number, a blob literal and TRUE.
func TestEngineRefusesNonConstantAddColumnDefault(t *testing.T) {
	db := openEngine(t, filepath.Join(t.TempDir(), "d.db"))
	mustExec(t, db, "create table t(a)")
	mustExec(t, db, "insert into t values(1)")
	for _, q := range []string{"alter table t add column b default (1 + 2)", "alter table t add column c default current_timestamp"} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("the engine accepted %q: the matrix must cover a non-literal default", q)
		}
	}
}
