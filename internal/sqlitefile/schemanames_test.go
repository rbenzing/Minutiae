package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestSchemaRowNamesMustAgreeWithTheirSQL: the engine refuses a schema whose
// row name or tbl_name disagrees with the statement (or whose index or trigger
// names no table). The engine decides each case; the library warns exactly
// then (schema-row-invalid), counts it in Schema.Skipped and lists the
// refusal in Info.EngineRefuses once the schema was read (final review A, F4
// and F9). A row the engine accepts is not flagged.
func TestSchemaRowNamesMustAgreeWithTheirSQL(t *testing.T) {
	patch := func(old, nu string) func([]byte) []byte {
		if len(old) != len(nu) {
			panic("patch must keep the length")
		}
		return func(d []byte) []byte { return bytes.Replace(d, []byte(old), []byte(nu), 1) }
	}
	trig := func(name, tbl, on string) func(b *sqlitetest.Builder) {
		return func(b *sqlitetest.Builder) {
			b.AddSchemaRow("trigger", name, tbl, int64(0), "create trigger "+name+" after insert on "+on+" begin select 1; end")
		}
	}
	ix := func(b *sqlitetest.Builder) { b.CreateIndex("ix", "t", "create index ix on t(a)", 0) }
	for _, c := range []struct {
		name  string
		build func(b *sqlitetest.Builder)
		post  func([]byte) []byte
	}{
		{"clean index", ix, nil},
		{"name differs from the sql", func(b *sqlitetest.Builder) { b.CreateTable("kipds", "create table kinds(a)") }, nil},
		{"name differs by case only", func(b *sqlitetest.Builder) { b.CreateTable("K", "create table k(a)") }, nil},
		{"table tbl_name differs", func(*sqlitetest.Builder) {}, patch("tableuu", "tableux")},
		{"index tbl_name names no table", ix, patch("indexixt", "indexixz")},
		{"index tbl_name names another table", ix, patch("indexixt", "indexixu")},
		{"index name differs from the sql", func(b *sqlitetest.Builder) { b.CreateIndex("iy", "t", "create index ix on t(a)", 0) }, nil},
		{"view name differs from the sql", func(b *sqlitetest.Builder) { b.AddSchemaRow("view", "v", "v", int64(0), "create view w as select 1") }, nil},
		{"view tbl_name differs", func(b *sqlitetest.Builder) { b.AddSchemaRow("view", "v", "w", int64(0), "create view v as select 1") }, nil},
		{"clean view", func(b *sqlitetest.Builder) { b.AddSchemaRow("view", "v", "v", int64(0), "create view v as select 1") }, nil},
		{"clean trigger", trig("g1", "t", "t"), nil},
		{"trigger name differs from the sql", func(b *sqlitetest.Builder) {
			b.AddSchemaRow("trigger", "g1", "t", int64(0), "create trigger g2 after insert on t begin select 1; end")
		}, nil},
		{"trigger tbl_name differs from ON", trig("g1", "u", "t"), nil},
		{"trigger tbl_name names no table", trig("g1", "zz", "zz"), nil},
		{"quoted names", func(b *sqlitetest.Builder) {
			b.AddSchemaRow("view", "v w", "v w", int64(0), `create view "v w" as select 1`)
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{})
			b.CreateTable("t", "create table t(a, b)").Insert(1, "x", int64(1))
			b.CreateTable("u", "create table u(a)")
			c.build(b)
			data := b.Bytes()
			if c.post != nil {
				n := len(data)
				if data = c.post(data); len(data) != n {
					t.Fatal("patch changed the length")
				}
			}
			var n int
			engineErr := openEngine(t, writeTemp(t, data)).QueryRow("select count(*) from sqlite_master").Scan(&n)
			engineRefuses := engineErr != nil

			_, v := openLive(t, data, sqlitefile.Options{})
			sch, err := v.Schema(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			warned := 0
			for _, w := range v.Warnings() {
				if w.Code == sqlitefile.WarnSchemaRowInvalid {
					warned++
				}
			}
			if (warned > 0) != engineRefuses || (sch.Skipped > 0) != engineRefuses {
				t.Errorf("engine refuses=%v (%v): %d schema-row-invalid warnings, Skipped %d: %v", engineRefuses, engineErr, warned, sch.Skipped, v.Warnings())
			}
			if got := len(v.Info().EngineRefuses) > 0; got != engineRefuses {
				t.Errorf("engine refuses=%v but Info.EngineRefuses = %q", engineRefuses, v.Info().EngineRefuses)
			}
		})
	}
}

// TestTableAndIndexAreNotAbsentAfterASkippedSchemaRow: a name that is not in
// a schema read with damage is not provably absent. The answer is ErrCorrupt,
// never ErrNotFound; with a clean schema it stays ErrNotFound (final review
// A, F4a).
func TestTableAndIndexAreNotAbsentAfterASkippedSchemaRow(t *testing.T) {
	build := func(bogus bool) *sqlitefile.View {
		b := sqlitetest.New(sqlitetest.Options{})
		b.CreateTable("t", "create table t(a)")
		if bogus {
			b.AddSchemaRow("bogus", "foo", "foo", int64(3), "create table foo(a)")
		}
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
		return v
	}
	ctx := context.Background()
	v := build(true)
	if _, err := v.Table(ctx, "foo"); !errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotFound) {
		t.Errorf("Table(foo) after a skipped row = %v, want ErrCorrupt and not ErrNotFound", err)
	}
	if _, err := v.Index(ctx, "ifoo"); !errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotFound) {
		t.Errorf("Index(ifoo) after a skipped row = %v, want ErrCorrupt and not ErrNotFound", err)
	}
	if tb, err := v.Table(ctx, "t"); err != nil || tb == nil {
		t.Errorf("Table(t) = %v, %v: a table that was found is still found", tb, err)
	}
	v = build(false)
	if _, err := v.Table(ctx, "foo"); !errors.Is(err, sqlitefile.ErrNotFound) {
		t.Errorf("Table(foo) on a clean schema = %v, want ErrNotFound", err)
	}
	if _, err := v.Index(ctx, "ifoo"); !errors.Is(err, sqlitefile.ErrNotFound) {
		t.Errorf("Index(ifoo) on a clean schema = %v, want ErrNotFound", err)
	}
}
