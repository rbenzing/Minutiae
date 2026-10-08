package sqlitefile_test

// The fixture generator (plan 3I, Task 14). It writes the class A and class B
// fixtures into testdata/ when MINUTIAE_REGEN_SQLITE_FIXTURES=1:
//
//	MINUTIAE_REGEN_SQLITE_FIXTURES=1 go test ./internal/sqlitefile -run TestGenerateFixtures
//
// Engine fixtures are written by the modernc engine, builder fixtures by
// internal/sqlitefile/sqlitetest. Every oracle is made from the engine's own
// answers (schema, rows, pragmas of a COPY of the files) and from the walker
// of fixturewalk_test.go, which shares no code with the library. Nothing here
// calls internal/sqlitefile. The class C fixtures are written by
// tools/fixtures/sqlite.sh in the fixtures container.

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

type fxGen struct {
	name, class string
	files       map[string][]byte // "<name>.db", "<name>.db-wal", "<name>.db-journal"
	expect      fxExpect
}

// fxWant is one version of a row the statements prove existed. The oracle
// reports it for every place a non-live page image holds it.
type fxWant struct {
	table    string
	rowid    *int64
	vals     []any // the raw record values (the rowid alias column is NULL)
	basis    string
	label    string // table label of a fit or guess
	required bool
}

type fxScenario struct {
	name, class string
	stmts       []string
	files       fxFiles
	wants       []fxWant
	unreach     []fxUnreachable
	markers     []string // bytes that must not be in the files at all (secure_delete)
	// requiredFound: the oracle must find at least this many history entries.
	minHistory int
}

// ---- engine plumbing ----

type fxEgen struct {
	t     *testing.T
	path  string
	db    *sql.DB
	stmts []string
}

func fxNewEgen(t *testing.T) *fxEgen {
	t.Helper()
	dir := t.TempDir()
	g := &fxEgen{t: t, path: filepath.Join(dir, "g.db")}
	g.db = openEngine(t, g.path)
	return g
}

func (g *fxEgen) x(q string) {
	g.t.Helper()
	g.stmts = append(g.stmts, q)
	mustExec(g.t, g.db, q)
}

// tx runs the statements as one transaction.
func (g *fxEgen) tx(qs ...string) {
	g.t.Helper()
	g.stmts = append(g.stmts, "BEGIN")
	g.stmts = append(g.stmts, qs...)
	g.stmts = append(g.stmts, "COMMIT")
	tx, err := g.db.Begin()
	if err != nil {
		g.t.Fatal(err)
	}
	for _, q := range qs {
		if _, err := tx.Exec(q); err != nil {
			_ = tx.Rollback()
			g.t.Fatalf("engine exec %q: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		g.t.Fatal(err)
	}
}

func (g *fxEgen) bytesOf(suffix string) []byte {
	g.t.Helper()
	b, err := os.ReadFile(g.path + suffix)
	if err != nil {
		g.t.Fatalf("read %s: %v", g.path+suffix, err)
	}
	return b
}

// dbOnly closes the connection (checkpointing) and returns the database file.
func (g *fxEgen) dbOnly() fxFiles {
	g.t.Helper()
	if err := g.db.Close(); err != nil {
		g.t.Fatal(err)
	}
	return fxFiles{db: g.bytesOf("")}
}

// snapshot copies the files while the connection is open.
func (g *fxEgen) snapshot(suffixes ...string) fxFiles {
	g.t.Helper()
	dir := g.t.TempDir()
	srcs := []string{g.path}
	for _, s := range suffixes {
		srcs = append(srcs, g.path+s)
	}
	copyFiles(g.t, dir, srcs...)
	var f fxFiles
	rd := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, filepath.Base(g.path)+n))
		if err != nil {
			g.t.Fatal(err)
		}
		return b
	}
	f.db = rd("")
	for _, s := range suffixes {
		switch s {
		case "-wal":
			f.wal = rd(s)
		case "-journal":
			f.journal = rd(s)
		}
	}
	return f
}

// fxLorem is an SQL expression for n deterministic characters that depend on i.
func fxLorem(i string, n int) string {
	return fmt.Sprintf("substr(printf('%%08d', %s) || replace(hex(zeroblob(%d)), '00', 'abcdefghijklmnop'), 1, %d)", i, n/16+1, n)
}

func fxCTE(n int) string {
	return fmt.Sprintf("with recursive c(i) as (select 1 union all select i+1 from c where i < %d) ", n)
}

func fxI64(n int64) *int64 { return &n }

// ---- the scenarios ----

func fxScenBasic(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("pragma user_version = 7")
	g.x("pragma application_id = 1330860884")
	g.x("create table kinds(id integer primary key, i integer, f real, t text, b blob, n)")
	g.x("insert into kinds values " +
		"(1, 0, 0.0, '', x'', null)," +
		"(2, 1, 1.5, 'a', x'00', 1)," +
		"(3, -1, -2.5, 'héllo wörld ✓', x'ff00ff', 2.5)," +
		"(4, 127, 3.14159265358979, 'line1' || char(10) || 'line2', x'0102030405', x'ab')," +
		"(5, 128, 1e300, 'mixed 東京', zeroblob(10), 'text')," +
		"(6, 32767, -1e-300, 'x', x'ff', 0)," +
		"(7, 32768, 0.1, 'y', x'fe', 1)," +
		"(8, 8388607, 123456789.125, 'z', x'fd', 2)," +
		"(9, 8388608, 2.0, 'w', x'fc', 3)," +
		"(10, 2147483647, 5e-324, 'v', x'fb', 4)," +
		"(11, 2147483648, 1.7976931348623157e308, 'u', x'fa', 5)," +
		"(12, 140737488355327, -3.0, 't', x'f9', 6)," +
		"(13, 140737488355328, 7.0, 's', x'f8', 7)," +
		"(14, 9223372036854775807, 8.0, 'r', x'f7', 8)," +
		"(15, -9223372036854775808, 9.0, 'q', x'f6', 9)")
	g.x("create table people(id integer primary key, name text, age integer, email text not null)")
	g.x(fxCTE(600) + "insert into people select i, 'person-' || i, i % 90, 'p' || i || '@example.test' from c")
	g.x("create index idx_people_name on people(name)")
	g.x("create view adults as select id, name from people where age >= 18")
	return &fxScenario{name: "basic-utf8-4k", class: "A", stmts: g.stmts, files: g.dbOnly()}
}

func fxScenUTF16(name string, enc string, ps, n int) func(*testing.T) *fxScenario {
	return func(t *testing.T) *fxScenario {
		g := fxNewEgen(t)
		g.x(fmt.Sprintf("pragma page_size = %d", ps))
		g.x(fmt.Sprintf("pragma encoding = '%s'", enc))
		g.x("create table docs(id integer primary key, title text, body text, n integer)")
		g.x(fxCTE(n) + "insert into docs select i, case i % 6 when 0 then 'Zürich' when 1 then '東京' when 2 then 'Привет' when 3 then 'café' when 4 then '\U0001F600 smile' else 'plain' end || ' ' || i, " +
			"'Ünïcödé body ' || i || ' ' || replace(hex(zeroblob(i % 12)), '00', '日本語 '), i * 3 from c")
		g.x("create index idx_docs_title on docs(title)")
		g.x("create table tags(doc integer, tag text)")
		g.x(fxCTE(40) + "insert into tags select i, 'tag-' || i || '-é' from c")
		return &fxScenario{name: name, class: "A", stmts: g.stmts, files: g.dbOnly()}
	}
}

func fxScenOverflow(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("create table blobs(id integer primary key, b blob, note text)")
	// 4096-byte pages: a payload of about 4500 bytes spills to 1 overflow page,
	// about 12600 to 3 and about 163900 to 40.
	for _, r := range []struct{ id, n int }{{1, 4500}, {2, 12665}, {3, 163900}, {4, 100}, {5, 4061}, {6, 4062}} {
		g.x(fmt.Sprintf("insert into blobs values (%d, cast(%s as blob), 'row %d of %d bytes')", r.id, fxLorem(fmt.Sprint(r.id), r.n), r.id, r.n))
	}
	cols := make([]string, 0, 120)
	for i := 1; i <= 120; i++ {
		cols = append(cols, fmt.Sprintf("c%d %s", i, []string{"integer", "text", "real", "blob"}[i%4]))
	}
	g.x("create table wide(id integer primary key, " + strings.Join(cols, ", ") + ")")
	vals := make([]string, 0, 120)
	for i := 1; i <= 120; i++ {
		switch i % 4 {
		case 0:
			vals = append(vals, fmt.Sprintf("%d", i*1000))
		case 1:
			vals = append(vals, fmt.Sprintf("'value-%d'", i))
		case 2:
			vals = append(vals, fmt.Sprintf("%d.5", i))
		default:
			vals = append(vals, fmt.Sprintf("x'%02x%02x'", i, 255-i))
		}
	}
	for id := 1; id <= 3; id++ {
		g.x(fmt.Sprintf("insert into wide values (%d, %s)", id, strings.Join(vals, ", ")))
	}
	return &fxScenario{name: "overflow-wide", class: "A", stmts: g.stmts, files: g.dbOnly()}
}

func fxScenWithoutRowid(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("create table kv(region text, id integer, v text, n real, primary key(region, id)) without rowid")
	g.x(fxCTE(300) + "insert into kv select case i % 3 when 0 then 'north' when 1 then 'south' else 'east' end, (i * 7919) % 1000, 'value-' || i || '-' || replace(hex(zeroblob(i % 9)), '00', 'z'), i / 4.0 from c")
	g.x("create index idx_kv_v on kv(v)")
	g.x("create table strict_t(id integer primary key, a integer, b text, c blob, d real, e any) strict")
	g.x("insert into strict_t values (1, 10, 'ten', x'0a', 10.5, 'anything'), (2, -20, 'minus', x'', -0.25, 42), (3, 0, '', x'ff', 1e10, x'01'), (4, 9223372036854775807, 'max', zeroblob(3), 3.0, null)")
	g.x("create table plain(id integer primary key, v text)")
	g.x(fxCTE(30) + "insert into plain select i, 'plain-' || i from c")
	return &fxScenario{name: "without-rowid", class: "A", stmts: g.stmts, files: g.dbOnly()}
}

func fxScenAddColumn(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("create table acct(id integer primary key, name text)")
	g.x(fxCTE(50) + "insert into acct select i, 'name-' || i from c")
	g.x("alter table acct add column status text default 'new'")
	g.x("alter table acct add column score integer not null default 10")
	g.x("alter table acct add column ratio real default 0.5")
	g.x("alter table acct add column memo text")
	g.x(fxCTE(10) + "insert into acct select 100 + i, 'late-' || i, 'active', i * 2, i / 3.0, 'memo ' || i from c")
	g.x("update acct set status = 'closed' where id between 5 and 9")
	return &fxScenario{name: "addcolumn-short", class: "A", stmts: g.stmts, files: g.dbOnly()}
}

func fxScenAutovacuum(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("pragma page_size = 1024")
	g.x("pragma auto_vacuum = 2")
	g.x("create table t1(id integer primary key, v text)")
	g.x("create table t2(id integer primary key, v text)")
	g.x("create table t3(id integer primary key, v text)")
	g.x(fxCTE(1500) + "insert into t1 select i, " + fxLorem("i", 110) + " from c")
	g.x(fxCTE(900) + "insert into t2 select i, " + fxLorem("i", 120) + " from c")
	g.x(fxCTE(300) + "insert into t3 select i, " + fxLorem("i", 100) + " from c")
	g.x("delete from t1 where id > 700")
	g.x("delete from t2 where id % 5 = 0")
	g.x("drop table t3")
	return &fxScenario{name: "autovacuum-incr", class: "A", stmts: g.stmts, files: g.dbOnly()}
}

// fxFreelistTables builds the dropped-table fxScenario; secure selects
// secure_delete = ON (the negative control).
func fxFreelistTables(t *testing.T, name string, secure bool) *fxScenario {
	g := fxNewEgen(t)
	if secure {
		g.x("pragma secure_delete = ON")
	}
	g.x("create table people(id integer primary key, name text, age integer, email text not null)")
	g.x(fxCTE(40) + "insert into people select i, 'resident-' || i, 20 + i % 50, 'r' || i || '@example.test' from c")
	g.x("create table events(ts integer, kind text)")
	g.x(fxCTE(20) + "insert into events select 1700000000 + i * 60, 'kind-' || (i % 4) from c")
	g.x("create table decoy(a text)")
	g.x(fxCTE(5) + "insert into decoy select 'decoy-' || i from c")
	g.x("create table d_fit(id integer primary key, name text, age integer, email text not null)")
	g.x(fxCTE(12) + "insert into d_fit select 100 + i, 'fitted-" + fxMarkerFor(secure) + "-' || i, 30 + i, 'f' || i || '@old.test' from c")
	g.x("create table d_guess(id integer primary key, name text, age integer)")
	g.x(fxCTE(12) + "insert into d_guess select 200 + i, 'guessed-" + fxMarkerFor(secure) + "-' || i, 40 + i from c")
	g.x("create table d_none(a integer, b integer, c text, d text, e blob)")
	g.x(fxCTE(12) + "insert into d_none select 300 + i, i * i, 'none-" + fxMarkerFor(secure) + "-' || i, 'second-' || i, x'cafe' from c")
	g.x("delete from people where id between 10 and 15")
	g.x("drop table decoy")
	g.x("drop table d_fit")
	g.x("drop table d_guess")
	g.x("drop table d_none")
	sc := &fxScenario{name: name, class: "A", stmts: g.stmts, files: g.dbOnly()}
	for i := int64(10); i <= 15; i++ {
		sc.unreach = append(sc.unreach, fxUnreachable{Table: "people", Rowid: fxI64(i), Values: []fxVal{
			fxEnc(nil), fxEnc(fmt.Sprintf("resident-%d", i)), fxEnc(20 + i%50), fxEnc(fmt.Sprintf("r%d@example.test", i)),
		}})
	}
	if secure {
		for _, m := range []string{"fitted-" + fxMarkerFor(true), "guessed-" + fxMarkerFor(true), "none-" + fxMarkerFor(true)} {
			sc.markers = append(sc.markers, m)
			sc.unreach = append(sc.unreach, fxUnreachable{Marker: m})
		}
		return sc
	}
	for i := int64(1); i <= 12; i++ {
		sc.wants = append(sc.wants,
			fxWant{table: "d_fit", rowid: fxI64(100 + i), vals: []any{nil, fmt.Sprintf("fitted-%s-%d", fxMarkerFor(false), i), 30 + i, fmt.Sprintf("f%d@old.test", i)}, basis: "fit", label: "people", required: true},
			fxWant{table: "d_guess", rowid: fxI64(200 + i), vals: []any{nil, fmt.Sprintf("guessed-%s-%d", fxMarkerFor(false), i), 40 + i}, basis: "guess", label: "people", required: true},
			fxWant{table: "d_none", rowid: nil, vals: []any{300 + i, i * i, fmt.Sprintf("none-%s-%d", fxMarkerFor(false), i), fmt.Sprintf("second-%d", i), []byte{0xca, 0xfe}}, basis: "none", required: true},
		)
	}
	sc.minHistory = 36
	return sc
}

func fxMarkerFor(secure bool) string {
	if secure {
		return "SDMARK"
	}
	return "old"
}

// ---- class B: the engine writes a WAL or journal ----

func fxScenWALUncheckpointed(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("create table kv(k integer primary key, v text, n integer)")
	g.x(fxCTE(200) + "insert into kv select i, 'init-' || i, i from c")
	g.x("pragma journal_mode = wal")
	g.x("pragma wal_autocheckpoint = 0")
	ver := map[int64][]string{}
	for i := int64(1); i <= 200; i++ {
		ver[i] = []string{fmt.Sprintf("init-%d", i)}
	}
	n := map[int64]int64{}
	for i := int64(1); i <= 200; i++ {
		n[i] = i
	}
	type vr struct {
		k int64
		v string
		n int64
	}
	var hist []vr
	hist = append(hist, func() []vr {
		var o []vr
		for i := int64(1); i <= 200; i++ {
			o = append(o, vr{i, fmt.Sprintf("init-%d", i), i})
		}
		return o
	}()...)
	g.tx("update kv set v = 't1-' || k where k between 1 and 10")
	for i := int64(1); i <= 10; i++ {
		hist = append(hist, vr{i, fmt.Sprintf("t1-%d", i), i})
	}
	g.tx("update kv set v = 't2-' || k where k between 1 and 5")
	for i := int64(1); i <= 5; i++ {
		hist = append(hist, vr{i, fmt.Sprintf("t2-%d", i), i})
	}
	g.tx("delete from kv where k between 20 and 25", "insert into kv values (1001, 't3-new-1001', 1), (1002, 't3-new-1002', 2), (1003, 't3-new-1003', 3)")
	for i := int64(1001); i <= 1003; i++ {
		hist = append(hist, vr{i, fmt.Sprintf("t3-new-%d", i), i - 1000})
	}
	g.tx("update kv set n = n + 1000 where k between 100 and 105")
	for i := int64(100); i <= 105; i++ {
		hist = append(hist, vr{i, fmt.Sprintf("init-%d", i), i + 1000})
	}
	sc := &fxScenario{name: "wal-uncheckpointed", class: "B", stmts: g.stmts, files: g.final("-wal")}
	for _, h := range hist {
		sc.wants = append(sc.wants, fxWant{table: "kv", rowid: fxI64(h.k), vals: []any{nil, h.v, h.n}})
	}
	sc.minHistory = 20
	sc.unreach = append(sc.unreach, fxUnreachable{Table: "kv", Rowid: fxI64(7777), Values: []fxVal{fxEnc(nil), fxEnc("never-existed"), fxEnc(int64(0))}})
	return sc
}

func fxScenWALStale(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("create table kv(k integer primary key, v text, n integer)")
	g.x(fxCTE(400) + "insert into kv select i, 'init-' || i || '-' || " + fxLorem("i", 30) + ", i from c")
	g.x("pragma journal_mode = wal")
	g.x("pragma wal_autocheckpoint = 0")
	type vr struct {
		k int64
		v string
		n int64
	}
	var hist []vr
	ver := func(tag string, from, to int64) {
		for i := from; i <= to; i++ {
			hist = append(hist, vr{i, fmt.Sprintf("%s-%d-%s", tag, i, fxLoremGo(i, 30)), i})
		}
	}
	ver("init", 1, 400)
	upd := func(tag string, from, to int64) string {
		return fmt.Sprintf("update kv set v = '%s-' || k || '-' || %s where k between %d and %d", tag, fxLorem("k", 30), from, to)
	}
	g.tx(upd("a1", 1, 400))
	ver("a1", 1, 400)
	g.tx(upd("a2", 100, 300))
	ver("a2", 100, 300)
	g.x("pragma wal_checkpoint(RESTART)")
	g.tx(upd("b3", 1, 1))
	ver("b3", 1, 1)
	sc := &fxScenario{name: "wal-stale-generation", class: "B", stmts: g.stmts, files: g.final("-wal")}
	for _, h := range hist {
		sc.wants = append(sc.wants, fxWant{table: "kv", rowid: fxI64(h.k), vals: []any{nil, h.v, h.n}})
	}
	sc.minHistory = 10
	return sc
}

func fxScenHotJournal(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("pragma journal_mode = delete")
	g.x("create table a(id integer primary key, v text)")
	g.x("create table b(id integer primary key, v text)")
	g.x(fxCTE(1500) + "insert into a select i, " + fxLorem("i", 120) + " from c")
	g.x(fxCTE(100) + "insert into b select i, " + fxLorem("i", 300) + " from c")
	g.x("pragma cache_size = 10")
	g.x("pragma cache_spill = 10")
	stmts := []string{
		"update a set v = 'interrupted-' || id || substr(v, 18) where id % 7 = 0",
		"delete from b where id % 2 = 0",
		"insert into b select 1000 + i, 'inserted-' || i from (select 1 as i union all select 2 union all select 3)",
	}
	tx, err := g.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	g.stmts = append(g.stmts, "BEGIN")
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("engine exec %q: %v", q, err)
		}
		g.stmts = append(g.stmts, q)
	}
	g.stmts = append(g.stmts, "-- the files are copied here, in the middle of the transaction, which is then rolled back")
	f := g.snapshot("-journal")
	_ = tx.Rollback()
	_ = g.db.Close()
	sc := &fxScenario{name: "hot-journal", class: "B", stmts: g.stmts, files: f}
	// The committed rows are live after the rollback; the uncommitted ones
	// are the history.
	for i := int64(7); i <= 1500; i += 7 {
		sc.wants = append(sc.wants, fxWant{table: "a", rowid: fxI64(i), vals: []any{nil, fmt.Sprintf("interrupted-%d", i) + fxLoremGo(i, 120)[17:]}})
	}
	for i := int64(1); i <= 3; i++ {
		sc.wants = append(sc.wants, fxWant{table: "b", rowid: fxI64(1000 + i), vals: []any{nil, fmt.Sprintf("inserted-%d", i)}})
	}
	sc.minHistory = 5
	return sc
}

// fxLoremGo mirrors the SQL fxLorem(i, n) expression.
func fxLoremGo(i int64, n int) string {
	s := fmt.Sprintf("%08d", i) + strings.Repeat("abcdefghijklmnop", n/16+1)
	return s[:n]
}

func fxScenPersist(t *testing.T) *fxScenario {
	g := fxNewEgen(t)
	g.x("pragma journal_mode = persist")
	g.x("create table kv(k integer primary key, v text, n integer)")
	g.x(fxCTE(300) + "insert into kv select i, 'first-' || i, i from c")
	g.tx("update kv set v = 'second-' || k where k between 1 and 60")
	sc := &fxScenario{name: "persist-journal", class: "B", stmts: g.stmts, files: g.final("-journal")}
	for i := int64(1); i <= 60; i++ {
		sc.wants = append(sc.wants, fxWant{table: "kv", rowid: fxI64(i), vals: []any{nil, fmt.Sprintf("first-%d", i), i}})
	}
	sc.minHistory = 5
	return sc
}

// ---- class A, builder ----

func fxScenBuilderWAL(_ *testing.T) *fxScenario {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	notes := b.CreateTable("notes", "CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT, n INTEGER)")
	type vr struct {
		id   int64
		body string
		n    int64
	}
	var hist []vr
	for i := int64(1); i <= 60; i++ {
		body := fmt.Sprintf("v0-row-%02d", i)
		notes.Insert(i, nil, body, i)
		hist = append(hist, vr{i, body, i})
	}
	b.Patch(18, 2, 2) // the header says WAL mode
	db := b.Bytes()
	prev := b.Snapshot()
	w := b.NewWAL(false, 0x11110001, 0x22220001, 0)
	upd := func(from, to int64, tag string) {
		for i := from; i <= to; i++ {
			body := fmt.Sprintf("%s-row-%02d", tag, i)
			notes.Update(i, nil, body, i)
			hist = append(hist, vr{i, body, i})
		}
	}
	upd(1, 5, "v1")
	b.CommitTo(w, prev)
	prev = b.Snapshot()
	upd(3, 8, "v2")
	notes.Delete(10)
	notes.Insert(61, nil, "v2-new-61", 61)
	hist = append(hist, vr{61, "v2-new-61", 61})
	b.CommitTo(w, prev)
	prev = b.Snapshot()
	upd(40, 45, "v3")
	b.CommitTo(w, prev)
	prev = b.Snapshot()
	// A new generation: salts change, the log restarts at slot 1; the old
	// generation's later frames stay in the file beyond the new end (stale).
	w.Reset(0x11110002, 0x22220002)
	upd(1, 1, "v4")
	b.CommitTo(w, prev)
	// An uncommitted tail: frames that follow the last commit.
	notes.Update(30, nil, "uncommitted-30", 30)
	hist = append(hist, vr{30, "uncommitted-30", 30})
	_, leaf, _ := notes.CellBytes(30)
	w.Frame(leaf, b.PageBytes(leaf), 0)
	sc := &fxScenario{name: "builder-wal-generations", class: "A", files: fxFiles{db: db, wal: w.Bytes()}}
	sc.stmts = []string{
		"sqlitetest: 60 rows v0, WAL mode header",
		"commit 1: update ids 1..5 (v1)",
		"commit 2: update ids 3..8 (v2), delete 10, insert 61",
		"commit 3: update ids 40..45 (v3)",
		"reset to generation 2 (new salts)",
		"commit 4: update id 1 (v4)",
		"uncommitted tail: update id 30",
	}
	for _, h := range hist {
		sc.wants = append(sc.wants, fxWant{table: "notes", rowid: fxI64(h.id), vals: []any{nil, h.body, h.n}})
	}
	sc.minHistory = 6
	return sc
}

func fxBuilderTwoTables(rowsA, rowsB int) (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	a := b.CreateTable("a", "CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)")
	bb := b.CreateTable("b", "CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)")
	for i := int64(1); i <= int64(rowsA); i++ {
		a.Insert(i, nil, fmt.Sprintf("a-row-%03d-%s", i, strings.Repeat("x", 60)))
	}
	for i := int64(1); i <= int64(rowsB); i++ {
		bb.Insert(i, nil, fmt.Sprintf("b-row-%03d-%s", i, strings.Repeat("y", 40)))
	}
	return b, a, bb
}

func fxChangedPages(base *sqlitetest.Image, b *sqlitetest.Builder) []uint32 {
	var out []uint32
	for n := uint32(1); n <= base.Pages() && int(n) <= int(b.Snapshot().Pages()); n++ {
		if !bytes.Equal(base.Page(n), b.PageBytes(n)) {
			out = append(out, n)
		}
	}
	return out
}

func fxScenBuilderHotJournal(_ *testing.T) *fxScenario {
	b, a, bb := fxBuilderTwoTables(120, 30)
	base := b.Snapshot()
	var wants []fxWant
	for i := int64(1); i <= 100; i += 3 {
		v := fxSameLen(fmt.Sprintf("chg-%03d-", i), "c")
		a.Update(i, nil, v)
		wants = append(wants, fxWant{table: "a", rowid: fxI64(i), vals: []any{nil, v}})
	}
	for i := int64(5); i <= 10; i++ {
		bb.Delete(i)
	}
	for i := int64(200); i <= 205; i++ {
		v := fmt.Sprintf("inserted-%d-%s", i, strings.Repeat("n", 50))
		a.Insert(i, nil, v)
		wants = append(wants, fxWant{table: "a", rowid: fxI64(i), vals: []any{nil, v}})
	}
	db := b.Bytes()
	j := b.NewJournal(512, 0x5eed0001, base.Pages())
	for _, n := range fxChangedPages(base, b) {
		j.Record(n, base.Page(n))
	}
	sc := &fxScenario{name: "builder-hot-journal", class: "A", files: fxFiles{db: db, journal: j.Bytes()}, wants: wants}
	sc.stmts = []string{"sqlitetest: tables a (120 rows) and b (30 rows)", "interrupted transaction: update every third a row, delete b 5..10, insert a 200..205; spilled pages are in the database file, the before-images in the journal"}
	sc.minHistory = 10
	return sc
}

func fxScenBuilderMultiseg(_ *testing.T) *fxScenario {
	b, a, _ := fxBuilderTwoTables(150, 20)
	s0 := b.Snapshot()
	var wants []fxWant
	upd := func(from, to int64, tag string) {
		for i := from; i <= to; i += 2 {
			v := fxSameLen(fmt.Sprintf("%s-%03d-", tag, i), "m")
			a.Update(i, nil, v)
			wants = append(wants, fxWant{table: "a", rowid: fxI64(i), vals: []any{nil, v}})
		}
	}
	upd(1, 60, "edt01")
	s1 := b.Snapshot()
	upd(1, 150, "edt02")
	db := b.Bytes()
	j := b.NewJournal(512, 0x5eed0002, s0.Pages())
	pages := fxChangedPages(s0, b)
	half := len(pages) / 2
	for _, n := range pages[:half] {
		j.Record(n, s0.Page(n))
	}
	j.NewSegment()
	for _, n := range pages[half:] {
		j.Record(n, s0.Page(n))
	}
	// One page is journaled a second time with a later image: the last record
	// of a page wins, so the first one is not applied.
	dup := pages[0]
	j.NewSegment()
	j.Record(dup, s1.Page(dup))
	sc := &fxScenario{name: "builder-journal-multiseg", class: "A", files: fxFiles{db: db, journal: j.Bytes()}, wants: wants}
	sc.stmts = []string{"sqlitetest: table a (150 rows)", "interrupted transaction in three journal segments; one page is journaled twice"}
	sc.minHistory = 10
	return sc
}

func fxScenEncryptedLike(_ *testing.T) *fxScenario {
	var buf []byte
	for i := 0; len(buf) < 8192; i++ {
		s := fxSha256sum(fmt.Sprintf("encrypted-like:%d", i))
		buf = append(buf, s[:]...)
	}
	return &fxScenario{
		name: "encrypted-like", class: "A", files: fxFiles{db: buf[:8192]},
		stmts: []string{"8192 bytes of sha256 counter-mode output (seed 'encrypted-like'); not a database"},
	}
}

func fxSha256sum(s string) [32]byte { return fxSha256Of([]byte(s)) }

// ---- the registry ----

var fxScenarioBuilders = map[string]func(*testing.T) *fxScenario{
	"basic-utf8-4k":            fxScenBasic,
	"utf16le-1k":               fxScenUTF16("utf16le-1k", "UTF-16le", 1024, 400),
	"utf16be-512":              fxScenUTF16("utf16be-512", "UTF-16be", 512, 700),
	"overflow-wide":            fxScenOverflow,
	"without-rowid":            fxScenWithoutRowid,
	"addcolumn-short":          fxScenAddColumn,
	"autovacuum-incr":          fxScenAutovacuum,
	"freelist-dropped":         func(t *testing.T) *fxScenario { return fxFreelistTables(t, "freelist-dropped", false) },
	"secure-delete":            func(t *testing.T) *fxScenario { return fxFreelistTables(t, "secure-delete", true) },
	"builder-wal-generations":  fxScenBuilderWAL,
	"builder-hot-journal":      fxScenBuilderHotJournal,
	"builder-journal-multiseg": fxScenBuilderMultiseg,
	"wal-uncheckpointed":       fxScenWALUncheckpointed,
	"wal-stale-generation":     fxScenWALStale,
	"hot-journal":              fxScenHotJournal,
	"persist-journal":          fxScenPersist,
	"encrypted-like":           fxScenEncryptedLike,
}

func fxGenerateFixtures(t *testing.T, classes ...string) map[string]*fxGen {
	t.Helper()
	want := map[string]bool{}
	for _, c := range classes {
		want[c] = true
	}
	out := map[string]*fxGen{}
	for _, spec := range fixtureSpecs {
		if !want[spec.Class] || spec.Class == "C" {
			continue
		}
		build, ok := fxScenarioBuilders[spec.Name]
		if !ok {
			t.Fatalf("no fxScenario for fixture %s", spec.Name)
		}
		sc := build(t)
		out[spec.Name] = fxFinish(t, spec, sc)
	}
	return out
}

func fxGenerateClassA(t *testing.T) map[string]*fxGen { return fxGenerateFixtures(t, "A") }

func TestGenerateFixtures(t *testing.T) {
	if os.Getenv("MINUTIAE_REGEN_SQLITE_FIXTURES") != "1" {
		t.Skip("set MINUTIAE_REGEN_SQLITE_FIXTURES=1 to regenerate the class A and B fixtures into testdata/")
	}
	gens := fxGenerateFixtures(t, "A", "B")
	if err := os.MkdirAll("testdata", 0o750); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(gens))
	for n := range gens {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := gens[n]
		for fname, data := range g.files {
			fxWriteGz(t, filepath.Join("testdata", fname+".gz"), data)
		}
		raw, err := json.MarshalIndent(g.expect, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", n+".expect.json"), append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d files)", n, len(g.files))
	}
}

func fxWriteGz(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

var _ = math.Pi

// final copies the files and then closes the connection, so a later scenario
// of the same process starts from a clean engine (an open connection of an
// earlier one changes how the page cache spills).
func (g *fxEgen) final(suffixes ...string) fxFiles {
	g.t.Helper()
	f := g.snapshot(suffixes...)
	if err := g.db.Close(); err != nil {
		g.t.Fatal(err)
	}
	return f
}

// fxSameLen pads prefix with fill to the 70 bytes of a builder row of tables
// a, so an update leaves the layout of the tree unchanged (only leaf pages
// change, as in a real in-place update).
func fxSameLen(prefix, fill string) string { return prefix + strings.Repeat(fill, 70-len(prefix)) }
