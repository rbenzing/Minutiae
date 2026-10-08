package sqlitefile_test

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// engineQuery returns every row of query as []any (nil, int64, float64,
// string, []byte).
func engineQuery(t testing.TB, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("engine: %s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// expectRows checks that the engine's rows equal want (value by value, with
// the type the builder was given).
func expectRows(t testing.TB, what string, got, want [][]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: engine returned %d rows, the builder was given %d", what, len(got), len(want))
		return
	}
	for i := range want {
		for j := range want[i] {
			g, w := got[i][j], want[i][j]
			if b, ok := w.([]byte); ok && len(b) == 0 {
				if gb, ok := g.([]byte); ok && len(gb) == 0 {
					continue
				}
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: row %d column %d: engine %T %.60v, builder %T %.60v", what, i, j, g, g, w, w)
				return
			}
		}
	}
}

func checkIntegrity(t testing.TB, db *sql.DB, what string) {
	t.Helper()
	if got := pragmaString(t, db, "integrity_check"); got != "ok" {
		t.Errorf("%s: integrity_check = %q", what, got)
	}
}

// builderRows is what a rowid table was given, as the engine should return
// it for a select of the rowid and every column.
type builderRows [][]any

func (r *builderRows) add(rowid int64, vals ...any) { *r = append(*r, append([]any{rowid}, vals...)) }

func longText(n int, seed byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte((i*7+int(seed))%26)
	}
	return string(b)
}

func longBlob(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) + seed
	}
	return b
}

func sortStringRows(rows [][]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		for c := range rows[i] {
			switch x := rows[i][c].(type) {
			case string:
				if y := rows[j][c].(string); x != y {
					return x < y
				}
			case int64:
				if y := rows[j][c].(int64); x != y {
					return x < y
				}
			}
		}
		return false
	})
}

type tableScenario struct {
	name  string
	opts  sqlitetest.Options
	build func(t *testing.T, b *sqlitetest.Builder) map[string][][]any // query -> expected rows
}

func fillTable(b *sqlitetest.Builder, name string, n int, pad func(i int) any) (map[string][][]any, *sqlitetest.Table) {
	tb := b.CreateTable(name, "CREATE TABLE "+name+"(a, b, c)")
	var want builderRows
	for i := 1; i <= n; i++ {
		vals := []any{int64(i * 3), fmt.Sprintf("row %d", i), pad(i)}
		tb.Insert(int64(i), vals...)
		want.add(int64(i), vals...)
	}
	return map[string][][]any{"select rowid, * from " + name + " order by rowid": want}, tb
}

func tableScenarios() []tableScenario {
	return []tableScenario{
		{"values of every kind", sqlitetest.Options{}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTable("t", "CREATE TABLE t(a, b, c, d, e)")
			var want builderRows
			rows := [][]any{
				{nil, int64(0), int64(1), int64(-1), 3.5},
				{int64(127), int64(-128), int64(32767), int64(-32768), -0.25},
				{int64(8388607), int64(-8388608), int64(2147483647), int64(-2147483648), 1e300},
				{int64(140737488355327), int64(-140737488355328), int64(9223372036854775807), int64(-9223372036854775808), 5e-324},
				{"", []byte{}, "héllo wörld ☃ 𝄞", []byte{0, 1, 2, 255}, "x"},
			}
			for i, r := range rows {
				tb.Insert(int64(i+1), r...)
				want.add(int64(i+1), r...)
			}
			return map[string][][]any{"select rowid, * from t order by rowid": want}
		}},
		{"multi-level tree page 512", sqlitetest.Options{PageSize: 512}, func(t *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, tb := fillTable(b, "t", 5000, func(i int) any { return float64(i) / 4 })
			if tb.Depth() < 3 {
				t.Errorf("depth %d, want >= 3", tb.Depth())
			}
			return m
		}},
		{"overflow 4096", sqlitetest.Options{}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "t", 40, func(i int) any {
				if i%3 == 0 {
					return longText(3000+i*400, byte(i))
				}
				return longBlob(i*700, byte(i))
			})
			return m
		}},
		{"overflow 512 reserved 32", sqlitetest.Options{PageSize: 512, Reserved: 32}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "t", 60, func(i int) any { return longText(i*53, byte(i)) })
			return m
		}},
		{"overflow 65536", sqlitetest.Options{PageSize: 65536}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "t", 6, func(i int) any { return longBlob(70000*i, byte(i)) })
			return m
		}},
		{"utf-16le", sqlitetest.Options{Encoding: 2, PageSize: 1024}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "t", 100, func(i int) any { return longText(i*30, byte(i)) + "𝄞é" })
			return m
		}},
		{"utf-16be", sqlitetest.Options{Encoding: 3}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "t", 100, func(i int) any { return longText(i*30, byte(i)) + "𝄞é" })
			return m
		}},
		{"index", sqlitetest.Options{PageSize: 512}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTable("t", "CREATE TABLE t(a, b, c)")
			var want builderRows
			for i := 1; i <= 800; i++ {
				vals := []any{fmt.Sprintf("k%03d", (i*37)%211), int64(i % 17), longText(i%90, byte(i))}
				tb.Insert(int64(i), vals...)
				want.add(int64(i), vals...)
			}
			tb.Insert(900, nil, nil, "null key")
			want.add(900, nil, nil, "null key")
			b.CreateIndex("t_a", "t", "CREATE INDEX t_a ON t(a)", 0)
			b.CreateIndex("t_bc", "t", "CREATE INDEX t_bc ON t(b, c)", 1, 2)
			return map[string][][]any{"select rowid, * from t order by rowid": want}
		}},
		{"index with overflow entries", sqlitetest.Options{PageSize: 1024}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
			var want builderRows
			for i := 1; i <= 60; i++ {
				vals := []any{longText(2500+i, byte(i%5)), int64(i)}
				tb.Insert(int64(i), vals...)
				want.add(int64(i), vals...)
			}
			b.CreateIndex("t_a", "t", "CREATE INDEX t_a ON t(a)", 0)
			return map[string][][]any{"select rowid, * from t order by rowid": want}
		}},
		{"without rowid", sqlitetest.Options{PageSize: 512}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTableWithoutRowid("w", "CREATE TABLE w(k TEXT, n, v, PRIMARY KEY(k, n)) WITHOUT ROWID", 2)
			var want [][]any
			for i := 0; i < 300; i++ {
				k, n := fmt.Sprintf("key%03d", (i*91)%300), int64(i%4)
				v := longText(i%70, byte(i))
				tb.Insert(int64(i), k, n, v)
				want = append(want, []any{k, n, v})
			}
			tb.Insert(1000, "zzz", int64(1), longText(2000, 3))
			want = append(want, []any{"zzz", int64(1), longText(2000, 3)})
			sortStringRows(want)
			return map[string][][]any{"select k, n, v from w order by k, n": want}
		}},
		{"updates and deletes", sqlitetest.Options{PageSize: 1024}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
			rows := map[int64][]any{}
			for i := int64(1); i <= 200; i++ {
				rows[i] = []any{fmt.Sprintf("value %d", i), longText(int(i)%40, byte(i))}
				tb.Insert(i, rows[i]...)
			}
			for i := int64(1); i <= 200; i += 3 {
				tb.Delete(i)
				delete(rows, i)
			}
			for i := int64(2); i <= 200; i += 7 {
				if _, ok := rows[i]; !ok {
					continue
				}
				rows[i] = []any{"updated", longText(int(i)%55+30, byte(i))}
				tb.Update(i, rows[i]...)
			}
			for i := int64(5); i <= 200; i += 11 { // same-length update, in place
				if v, ok := rows[i]; ok {
					s := v[0].(string)
					rows[i] = []any{s[:len(s)-1] + "X", v[1]}
					tb.Update(i, rows[i]...)
				}
			}
			tb.Insert(150000, "a late row", nil)
			rows[150000] = []any{"a late row", nil}
			var want builderRows
			for i := int64(1); i <= 200; i++ {
				if v, ok := rows[i]; ok {
					want.add(i, v...)
				}
			}
			want.add(150000, rows[150000]...)
			return map[string][][]any{"select rowid, * from t order by rowid": want}
		}},
		{"deletes with overflow", sqlitetest.Options{PageSize: 512}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			tb := b.CreateTable("t", "CREATE TABLE t(a)")
			var want builderRows
			for i := int64(1); i <= 30; i++ {
				v := longText(int(i)*60, byte(i))
				tb.Insert(i, v)
				if i%2 == 0 {
					tb.Delete(i)
					continue
				}
				want.add(i, v)
			}
			return map[string][][]any{"select rowid, * from t order by rowid": want}
		}},
		{"dropped table", sqlitetest.Options{PageSize: 1024}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			m, _ := fillTable(b, "keep", 30, func(i int) any { return longText(i*40, 1) })
			fillTable(b, "gone", 400, func(i int) any { return longText(i%30*30, 2) })
			b.CreateIndex("gone_a", "gone", "CREATE INDEX gone_a ON gone(a)", 0)
			b.DropTable("gone")
			m["select name from sqlite_schema order by name"] = [][]any{{"keep"}}
			return m
		}},
		{"schema larger than page 1", sqlitetest.Options{PageSize: 512}, func(_ *testing.T, b *sqlitetest.Builder) map[string][][]any {
			var want [][]any
			for i := 0; i < 120; i++ {
				name := fmt.Sprintf("table_%03d", i)
				b.CreateTable(name, "CREATE TABLE "+name+"(a, b)").Insert(1, int64(i), "x")
				want = append(want, []any{name})
			}
			sortStringRows(want)
			return map[string][][]any{"select name from sqlite_schema where type = 'table' order by name": want}
		}},
	}
}

// runTableScenarios builds each scenario and has the independent engine read
// it: integrity_check is ok and the rows it reads equal what the builder was
// given.
func runTableScenarios(t *testing.T) {
	for _, s := range tableScenarios() {
		t.Run(s.name, func(t *testing.T) {
			b := sqlitetest.New(s.opts)
			queries := s.build(t, b)
			db := openEngine(t, writeTemp(t, b.Bytes()))
			checkIntegrity(t, db, s.name)
			for q, want := range queries {
				expectRows(t, q, engineQuery(t, db, q), want)
			}
		})
	}
}
