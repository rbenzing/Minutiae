package sqlitefile_test

// The key-order check against engine-written files (LX-4 Task 8, L105): the check
// must flag nothing on a file the engine wrote (no false positive), and it must
// count, never skip silently, the entries it cannot compare.

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

const (
	uncNone = ""
	uncExpr = "expr" // text of an expression column: counted where either side is text
	uncAll  = "all"  // an automatic index: every entry is counted
)

type orderCase struct {
	name   string
	ddl    []string
	insert string
	row    func(i int, r *rand.Rand) []any
	// unc: object name -> what is counted unchecked.
	unc map[string]string
	// count: object name -> the engine query that counts its entries (default: all rows of t).
	count map[string]string
}

func randCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' && r.IntN(2) == 0 {
			b[i] = c - 32
		}
	}
	return string(b)
}

func spaces(n int) string { return strings.Repeat(" ", n) }

func orderCases() []orderCase {
	word := func(r *rand.Rand) string { return randCase(r, fmt.Sprintf("v%02d", r.IntN(60))) }
	perm := func(n int) func(int) int {
		p := rand.New(rand.NewPCG(7, 7)).Perm(n) //nolint:gosec // a fixed seed makes the fixtures reproducible; no secret is involved
		return func(i int) int { return p[i] }
	}
	p3000 := perm(3000)
	const t3 = "insert into t values(?,?,?)"
	return []orderCase{
		{
			name: "wr_int_desc", ddl: []string{"create table t(k integer primary key desc, v text) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, _ *rand.Rand) []any { return []any{int64(p3000(i) * 3), fmt.Sprintf("v%d", i)} },
		},
		{
			name: "wr_nocase", ddl: []string{"create table t(k text collate nocase primary key, v) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{randCase(r, fmt.Sprintf("key%05d", p3000(i))), i} },
		},
		{
			name: "wr_rtrim", ddl: []string{"create table t(k text collate rtrim primary key, v) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, _ *rand.Rand) []any { return []any{fmt.Sprintf("k%05d", p3000(i)) + spaces(i%3), i} },
		},
		{
			name: "wr_composite", ddl: []string{"create table t(a text collate nocase, b integer, v, primary key(a, b desc)) without rowid"}, insert: t3,
			row: func(i int, r *rand.Rand) []any {
				return []any{randCase(r, "grp"+string(rune('a'+i%40))), int64(p3000(i)), i}
			},
		},
		{
			name: "wr_real_ints", ddl: []string{"create table t(k real primary key, v) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, _ *rand.Rand) []any {
				if i%2 == 0 {
					return []any{int64(p3000(i)) * 2, i}
				}
				return []any{float64(p3000(i))*2 + 1.5, i}
			},
		},
		{
			name: "wr_triple", ddl: []string{"create table t(a integer, b text, c text collate rtrim, v, primary key(a, b desc, c)) without rowid"}, insert: "insert into t values(?,?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				return []any{int64(i % 7), randCase(r, string(rune('a'+i%5))), fmt.Sprintf("c%05d", p3000(i)) + spaces(i%2), i}
			},
		},
		{
			name: "wr_tablepk_desc", ddl: []string{"create table t(k text, v, primary key(k collate nocase desc)) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{randCase(r, fmt.Sprintf("tk%05d", p3000(i))), i} },
		},
		{
			name: "wr_blob", ddl: []string{"create table t(k blob primary key, v) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, r *rand.Rand) []any {
				k := []byte{byte(r.IntN(4)), 0, byte(r.IntN(256))}
				return []any{append(k, byte(i>>8), byte(i)), i}
			},
		},
		{
			name: "wr_unicode", ddl: []string{"create table t(k text collate nocase primary key, v) without rowid"}, insert: "insert into t values(?,?)",
			row: func(i int, _ *rand.Rand) []any {
				pre := []string{"é", "É", "中", "\U0001f600", "a", "A"}[i%6]
				return []any{fmt.Sprintf("%s%05d", pre, p3000(i)), i}
			},
		},
		{
			name: "ix_plain", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, i, word(r)} },
		},
		{
			name: "ix_partial", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b) where a%2=0"}, insert: "insert into t values(?,?,?)",
			row:   func(i int, r *rand.Rand) []any { return []any{i + 1, i, word(r)} },
			count: map[string]string{"i": "select count(*) from t where a%2=0"},
		},
		{
			name: "ix_expr_text", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(lower(b))"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				if i%11 == 0 {
					return []any{i + 1, i, nil}
				}
				return []any{i + 1, i, word(r)}
			},
			unc: map[string]string{"i": uncExpr},
		},
		{
			name: "ix_desc", ddl: []string{"create table t(id integer primary key, a, b, c)", "create index i on t(b desc, c)"}, insert: "insert into t values(?,?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, i, word(r), r.IntN(10)} },
		},
		{
			name: "ix_unique_nulls", ddl: []string{"create table t(id integer primary key, a, b)", "create unique index i on t(b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, _ *rand.Rand) []any {
				if i%3 == 0 {
					return []any{i + 1, i, nil}
				}
				return []any{i + 1, i, fmt.Sprintf("u%05d", p3000(i))}
			},
		},
		{
			name: "ix_nocase", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b collate nocase)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, i, word(r)} },
		},
		{
			name: "ix_mixed_types", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				var b any
				switch i % 6 {
				case 0:
					b = int64(r.IntN(50))
				case 1:
					b = float64(r.IntN(50)) + 0.5
				case 2:
					b = word(r)
				case 3:
					b = []byte{byte(r.IntN(5)), 0, byte(r.IntN(5))}
				case 4:
					b = float64(r.IntN(50))
				}
				return []any{i + 1, i, b}
			},
		},
		{
			name: "ix_expr_int", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t((a+id))"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, r.IntN(100), nil} },
		},
		{
			name: "auto_index", ddl: []string{"create table t(id integer primary key, a unique, b)"}, insert: "insert into t values(?,?,?)",
			row:   func(i int, _ *rand.Rand) []any { return []any{i + 1, fmt.Sprintf("a%05d", p3000(i)), i} },
			unc:   map[string]string{"sqlite_autoindex_t_1": uncAll},
			count: map[string]string{},
		},
		{
			name: "ix_rtrim_desc", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b collate rtrim desc, a)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, r.IntN(5), word(r) + spaces(r.IntN(3))} },
		},
		{
			name: "ix_real_ints", ddl: []string{"create table t(id integer primary key, a, b real)", "create index i on t(b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				if i%2 == 0 {
					return []any{i + 1, i, int64(r.IntN(100))}
				}
				return []any{i + 1, i, float64(r.IntN(100)) / 4}
			},
		},
		{
			name: "ix_blob", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				return []any{i + 1, i, []byte{byte(r.IntN(3)), 0, byte(r.IntN(3)), 0}}
			},
		},
		{
			name: "ix_unicode_nocase", ddl: []string{"create table t(id integer primary key, a, b)", "create index i on t(b collate nocase)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any {
				pre := []string{"é", "É", "中", "a", "A", "\U0001f600"}[r.IntN(6)]
				return []any{i + 1, i, pre + word(r)}
			},
		},
		{
			name: "ix_on_wr", ddl: []string{"create table t(k integer primary key, a, b) without rowid", "create index i on t(a, b)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{int64(p3000(i)), r.IntN(20), word(r)} },
		},
		{
			name: "ix_unique_nocase", ddl: []string{"create table t(id integer primary key, a, b)", "create unique index i on t(b collate nocase)"}, insert: "insert into t values(?,?,?)",
			row: func(i int, r *rand.Rand) []any { return []any{i + 1, i, randCase(r, fmt.Sprintf("n%05d", p3000(i)))} },
		},
	}
}

type engineOrderFile struct {
	name   string
	data   []byte
	counts map[string]int // object -> the engine's count of its entries
}

// buildOrderFile writes the case through the engine, in the given text encoding
// (pragma value, "" for UTF-8), with 512-byte pages so the trees have three levels.
func buildOrderFile(t *testing.T, c orderCase, enc string) engineOrderFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "o.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, a ...any) {
		if _, err := db.Exec(q, a...); err != nil {
			_ = db.Close()
			t.Fatalf("%s: %s: %v", c.name, q, err)
		}
	}
	exec("pragma page_size=512")
	if enc != "" {
		exec("pragma encoding='" + enc + "'")
	}
	for _, q := range c.ddl {
		exec(q)
	}
	exec("begin")
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a fixed seed makes the fixtures reproducible; no secret is involved
	for i := range 3000 {
		exec(c.insert, c.row(i, r)...)
	}
	exec("commit")
	counts := map[string]int{}
	rows, err := db.Query("select name from sqlite_master where type in ('table','index') and rootpage > 0")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	_ = rows.Close()
	for _, n := range names {
		q := "select count(*) from t"
		if cq, ok := c.count[n]; ok {
			q = cq
		}
		var k int
		if err := db.QueryRow(q).Scan(&k); err != nil {
			t.Fatalf("%s: %s: %v", c.name, q, err)
		}
		counts[n] = k
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data := readFileRetry(t, path)
	return engineOrderFile{name: c.name + "/" + enc, data: data, counts: counts}
}

var (
	orderFilesOnce sync.Once
	orderFiles     []engineOrderFile
	orderCaseOf    = map[string]orderCase{}
)

// engineOrderFiles builds the 24 cases in UTF-8 and UTF-16le once.
func engineOrderFiles(t *testing.T) []engineOrderFile {
	t.Helper()
	orderFilesOnce.Do(func() {
		for _, c := range orderCases() {
			for _, enc := range []string{"", "UTF-16le"} {
				f := buildOrderFile(t, c, enc)
				orderFiles = append(orderFiles, f)
				orderCaseOf[f.name] = c
			}
		}
	})
	if len(orderFiles) == 0 {
		t.Fatal("the engine files were not built")
	}
	return orderFiles
}

// treeResult is what one scan of one object delivered.
type treeResult struct {
	object        string
	rows          []sqlitefile.Row
	flagged       int
	orderWarnings int
	unchecked     int
	hasUnchecked  bool
}

// scanOrdered scans every table and index of data through Table.Rows and Index.Entries,
// each on a fresh view so its warnings are its own.
func scanOrdered(t *testing.T, data []byte, only func(string, bool) bool) []treeResult {
	t.Helper()
	_, v0 := openLive(t, data, sqlitefile.Options{})
	s, err := v0.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type obj struct {
		name    string
		isIndex bool
	}
	var objs []obj
	for _, o := range s.Objects {
		if (o.Type == "table" || o.Type == "index") && o.RootPage > 0 && !o.Virtual {
			objs = append(objs, obj{o.Name, o.Type == "index"})
		}
	}
	v0.Release()
	var out []treeResult
	for _, o := range objs {
		if only != nil && !only(o.name, o.isIndex) {
			continue
		}
		_, v := openLive(t, data, sqlitefile.Options{})
		if _, err := v.Schema(context.Background()); err != nil {
			t.Fatal(err)
		}
		before := len(warningsOf(v, sqlitefile.WarnBTreeOrder))
		var rows []sqlitefile.Row
		if o.isIndex {
			rows = scanIndexEntries(t, v, o.name)
		} else {
			rows = scanTableRows(t, v, o.name).rows
		}
		res := treeResult{object: o.name, rows: rows, flagged: len(flaggedIdx(rows)), orderWarnings: len(warningsOf(v, sqlitefile.WarnBTreeOrder)) - before}
		res.unchecked, res.hasUnchecked = uncheckedCount(t, v)
		out = append(out, res)
		v.Release()
	}
	return out
}

// mainTreeDepth is the number of levels of the tree under test: the index i when the
// case has one, else the table t, found by descending the left-most children.
func mainTreeDepth(t *testing.T, data []byte) int {
	t.Helper()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := uint32(0)
	for _, o := range s.Objects {
		if o.Name == "t" && root == 0 {
			root = o.RootPage
		}
		if o.Name == "i" || strings.HasPrefix(o.Name, "sqlite_autoindex") {
			root = o.RootPage
		}
	}
	if root == 0 {
		t.Fatal("no tree under test")
	}
	ps := int(binaryBE16(data[16:]))
	if ps == 1 {
		ps = 65536
	}
	depth := 1
	for pg := root; ; depth++ {
		p := pageAt(data, ps, pg)
		if p[0] != 0x02 && p[0] != 0x05 {
			return depth
		}
		pg = vtU32(p[cellOffsets(p)[0]:]) // the left child of the first cell
	}
}

func binaryBE16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// PASSES ON ARRIVAL against the stub, then keeps passing: the anti-false-positive guard.
// Mutation: invert the comparison sign in the walker and it fails.
func TestOrderCheckHasNoFalsePositivesOnEngineFiles(t *testing.T) {
	files := engineOrderFiles(t)
	if len(files) != 48 {
		t.Fatalf("%d engine files, want 24 cases in two encodings", len(files))
	}
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			if d := mainTreeDepth(t, f.data); d < 3 {
				t.Errorf("the main tree has %d levels, the test needs at least 3", d)
			}
			for _, res := range scanOrdered(t, f.data, nil) {
				if res.flagged != 0 || res.orderWarnings != 0 {
					t.Errorf("%s: %d entries flagged, %d btree-order warnings on an engine-written file", res.object, res.flagged, res.orderWarnings)
				}
				if want, ok := f.counts[res.object]; ok && len(res.rows) != want {
					t.Errorf("%s: %d entries delivered, the engine counts %d", res.object, len(res.rows), want)
				}
			}
		})
	}
}

// The committed fixtures: every index-shaped tree is scanned, nothing is flagged.
func TestOrderCheckHasNoFalsePositivesOnCommittedFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.db.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	scanned := 0
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			zr, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{}); err != nil {
				t.Skipf("not a readable database: %v", err)
			}
			// only the index-shaped trees: a rowid table keeps its own check
			ctx := context.Background()
			_, v := openLive(t, data, sqlitefile.Options{})
			s, err := v.Schema(ctx)
			if err != nil {
				t.Skipf("no schema: %v", err)
			}
			v.Release()
			isWR := map[string]bool{}
			for _, o := range s.Objects {
				if o.Type == "table" && o.Table != nil && o.Table.ParseOK && o.Table.WithoutRowid {
					isWR[o.Name] = true
				}
			}
			for _, res := range scanOrdered(t, data, func(n string, idx bool) bool { return idx || isWR[n] }) {
				scanned++
				if res.flagged != 0 || res.orderWarnings != 0 {
					t.Errorf("%s: %d flagged, %d btree-order warnings", res.object, res.flagged, res.orderWarnings)
				}
			}
		})
	}
	if scanned == 0 {
		t.Error("no index-shaped tree in any committed fixture was scanned")
	}
}

// exprTextPairs counts the entries whose comparison with the previous one involves text in
// the first (expression) column: the entries the check cannot decide.
func exprTextPairs(rows []sqlitefile.Row) int {
	n := 0
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Values[0].Kind == sqlitefile.KindText || rows[i].Values[0].Kind == sqlitefile.KindText {
			n++
		}
	}
	return n
}

// RED against the stub, which counts nothing.
func TestOrderCheckCountsUncheckedEntriesOnEngineFiles(t *testing.T) {
	files := engineOrderFiles(t)
	sawExpr, sawAll := 0, 0
	for _, f := range files {
		c := orderCaseOf[f.name]
		t.Run(f.name, func(t *testing.T) {
			for _, res := range scanOrdered(t, f.data, nil) {
				mode := c.unc[res.object]
				switch mode {
				case uncNone:
					if res.hasUnchecked {
						t.Errorf("%s: %d entries counted unchecked, want none: the order is decidable", res.object, res.unchecked)
					}
				case uncExpr:
					sawExpr++
					want := exprTextPairs(res.rows)
					if want == 0 || !res.hasUnchecked || res.unchecked < want || res.unchecked > len(res.rows) {
						t.Errorf("%s: unchecked %d (found %v), want %d to %d (the pairs with text, plus the entries whose bounds are text)", res.object, res.unchecked, res.hasUnchecked, want, len(res.rows))
					}
				case uncAll:
					sawAll++
					if !res.hasUnchecked || res.unchecked != len(res.rows) || len(res.rows) == 0 {
						t.Errorf("%s: unchecked %d (found %v), want all %d entries", res.object, res.unchecked, res.hasUnchecked, len(res.rows))
					}
				}
			}
		})
	}
	if sawExpr != 2 || sawAll != 2 {
		t.Errorf("the expression case ran %d times and the automatic-index case %d times, want 2 each (two encodings)", sawExpr, sawAll)
	}
}
