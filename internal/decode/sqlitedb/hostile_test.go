package sqlitedb_test

// Hostile input (spec 10.3 / 14): no schema, record, page or companion file may
// make the decoder panic, hang or exceed its budget, and the principle holds
// everywhere: no hostile input is silently lost. A scan that returns fewer rows
// than a clean scan always comes with an error or a warning.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// typedErrors are the errors the decoder promises; anything else from a
// hostile file is an untyped failure.
var typedErrors = []error{
	sqlitedb.ErrNoBudget, sqlitedb.ErrNotSQLite, sqlitedb.ErrEncrypted, sqlitedb.ErrCorrupt,
	sqlitedb.ErrLiveUnavailable, sqlitedb.ErrEngineRefuses, sqlitedb.ErrNoSuchTable, sqlitedb.ErrWithoutRowid,
	sqlitedb.ErrIndexLimit, sqlitedb.ErrBadFiles, sqlitedb.ErrRowMismatch, sqlitedb.ErrInternal,
	sqlitedb.ErrUnsupportedSchema, sqlitedb.ErrUnsupportedCollation, sqlitedb.ErrReleased,
	parse.ErrBudget, sqlitefile.ErrBudget, sqlitefile.ErrLimit,
}

func requireTyped(t testing.TB, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, e := range typedErrors {
		if errors.Is(err, e) {
			return
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%s: ran out of time (a hang): %v", what, err)
		return
	}
	t.Errorf("%s: untyped error %T: %v", what, err, err)
}

// outcome is what exercise saw.
type outcome struct {
	openErr   error
	names     []string
	rows      map[string]int   // rows a bounded scan delivered, per table
	scanErr   map[string]error // the scan's error, per table
	found     map[string][]int64
	getErr    map[string]error
	getMiss   map[string][]int64 // rowids whose Get answered "not found" with no error
	warnings  int64              // Stats().NewWarningsAtLeast after all the work (warnings raised after Open)
	scanWarn  int64              // the same, right after the scans (B65)
	scanCodes map[string]bool    // warning codes right after the scans
	allWarn   int                // every warning the library holds, those of Open included
	codes     map[string]bool
	tableErr  map[string]error // Table() refusals, per listed name
}

const scanBound = 5000

// exercise runs the whole read surface over the files under a budget: Open,
// Tables, Table, a bounded Scan (invariants checked on every row), Get of
// rowids 1..20 and an Index of every column. Every error must be typed, the
// budget must hold and balance.
func exercise(t testing.TB, f sqlitedb.Files, limit int64) *outcome {
	t.Helper()
	b := &testBudget{limit: limit}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	o := &outcome{
		rows: map[string]int{}, scanErr: map[string]error{}, found: map[string][]int64{},
		getErr: map[string]error{}, getMiss: map[string][]int64{}, codes: map[string]bool{}, tableErr: map[string]error{},
	}
	defer func() {
		if b.peak > limit {
			t.Errorf("budget peak %d exceeds the limit %d", b.peak, limit)
		}
		if b.used != 0 {
			t.Errorf("budget used %d after Release", b.used)
		}
	}()
	d, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		requireTyped(t, "Open", err)
		o.openErr = err
		return o
	}
	defer d.Release()
	names, err := d.Tables(ctx)
	requireTyped(t, "Tables", err)
	o.names = names
	var tables []*sqlitedb.Table
	var tnames []string
	for _, n := range names {
		tb, err := d.Table(ctx, n, nil, nil)
		requireTyped(t, "Table "+n, err)
		if err != nil {
			o.tableErr[n] = err
		}
		if err != nil {
			continue
		}
		cnt := 0
		serr := tb.Scan(ctx, func(r sqlitedb.Row) error {
			checkRowInvariants(t, r)
			cnt++
			if cnt >= scanBound {
				return sqlitedb.ErrStop
			}
			return nil
		})
		requireTyped(t, "Scan "+n, serr)
		o.rows[n], o.scanErr[n] = cnt, serr
		tables = append(tables, tb)
		tnames = append(tnames, n)
	}
	// B65: the warnings of the scans alone, before any Get or Index can add to them.
	o.scanWarn = d.Stats().NewWarningsAtLeast
	o.scanCodes = map[string]bool{}
	for _, w := range d.Warnings() {
		o.scanCodes[w.Code] = true
	}
	for i, tb := range tables {
		n := tnames[i]
		if !tb.WithoutRowid() {
			for id := int64(1); id <= 20; id++ {
				r, ok, gerr := tb.Get(ctx, id)
				requireTyped(t, fmt.Sprintf("Get %s %d", n, id), gerr)
				if gerr != nil {
					o.getErr[n] = gerr
				}
				if !ok && gerr == nil {
					o.getMiss[n] = append(o.getMiss[n], id)
				}
				if ok {
					checkRowInvariants(t, r)
					o.found[n] = append(o.found[n], id)
					r.Release()
				}
			}
		}
		for _, c := range tb.Cols() {
			if c.Virtual {
				continue
			}
			ix, ierr := tb.Index(ctx, c.Name, 20000)
			requireTyped(t, fmt.Sprintf("Index %s.%s", n, c.Name), ierr)
			if ierr == nil {
				ix.Release()
			}
		}
	}
	o.warnings = d.Stats().NewWarningsAtLeast
	o.allWarn = len(d.Warnings())
	for _, w := range d.Warnings() {
		o.codes[w.Code] = true
	}
	return o
}

func TestSQLiteDBHostileSchema(t *testing.T) {
	ints := func(n int) []any {
		v := make([]any, n)
		for i := range v {
			v[i] = int64(i + 1)
		}
		return v
	}
	manyCols := make([]string, 1999)
	for i := range manyCols {
		manyCols[i] = fmt.Sprintf("c%d", i)
	}
	lying := func(def string) func(b *sqlitetest.Builder) {
		return func(b *sqlitetest.Builder) {
			tt := b.CreateTable("t", def)
			for i, n := range []int{0, 1, 2, 50} {
				tt.Insert(int64(i+1), ints(n)...)
			}
		}
	}
	cases := []struct {
		name string
		code string // the warning code the damage must raise ("" when the shape loses nothing)
		fill func(b *sqlitetest.Builder)
	}{
		{"lying-one-column", "record-length-mismatch", lying("create table t(a)")},
		{"lying-three-columns", "record-length-mismatch", lying("create table t(a, b, c)")},
		{"lying-alias-and-two", "record-length-mismatch", lying("create table t(id integer primary key, a, b)")},
		{"unparsed-definition", "schema-sql-unparsed", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "this is not a create statement (").Insert(1, int64(1), "x")
		}},
		{"1999-columns", "", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t("+strings.Join(manyCols, ",")+")").Insert(1, int64(1), int64(2), int64(3))
		}},
		{"duplicate-column-names", "schema-sql-unparsed", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a, A, a)").Insert(1, int64(1), int64(2), int64(3))
		}},
		{"rootpage-0", "schema-row-invalid", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
			b.AddSchemaRow("table", "x", "x", int64(0), "create table x(a)")
		}},
		{"rootpage-1", "schema-row-invalid", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
			b.AddSchemaRow("table", "x", "x", int64(1), "create table x(a)")
		}},
		{"rootpage-2-pow-31", "schema-row-invalid", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
			b.AddSchemaRow("table", "x", "x", int64(1)<<31, "create table x(a)")
		}},
		{"rootpage-negative", "schema-row-invalid", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
			b.AddSchemaRow("table", "x", "x", int64(-5), "create table x(a)")
		}},
		{"root-shared-by-two-tables", "", func(b *sqlitetest.Builder) {
			tt := b.CreateTable("t", "create table t(a)")
			tt.Insert(1, int64(1))
			tt.Insert(2, int64(2))
			b.AddSchemaRow("table", "u", "u", int64(tt.Pages()[0]), "create table u(a)")
		}},
		{"table-named-sqlite-schema", "", func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
			b.AddSchemaRow("table", "sqlite_schema", "sqlite_schema", int64(2), "create table sqlite_schema(a)")
		}},
		{"without-rowid-key-column-count-mismatch", "schema-sql-unparsed", func(b *sqlitetest.Builder) {
			b.CreateTableWithoutRowid("w", "create table w(a, b) without rowid", 1).Insert(1, int64(1), "x")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, c.fill)
			o := exercise(t, filesOf(data, nil, nil), 64<<20)
			for _, n := range o.names {
				if _, scanned := o.rows[n]; !scanned && o.tableErr[n] == nil {
					t.Errorf("table %q is listed but neither scanned nor refused", n)
				}
				if o.tableErr[n] != nil && o.allWarn == 0 {
					t.Errorf("table %q refused (%v) with no warning", n, o.tableErr[n])
				}
			}
			if c.code != "" && !o.codes[c.code] {
				t.Errorf("no %q warning among %v", c.code, o.codes)
			}
			t.Logf("open=%v tables=%v rows=%v scanErr=%v warnings=%d all=%d codes=%v", o.openErr, o.names, o.rows, o.scanErr, o.warnings, o.allWarn, o.codes)
		})
	}
}

func TestSQLiteDBHostileRecords(t *testing.T) {
	type tcase struct {
		name string
		opts sqlitetest.Options
		fill func(tt *sqlitetest.Table, b *sqlitetest.Builder)
		want int    // rows inserted: every one must be delivered, or an error or warning raised
		code string // the warning code the damage must raise ("" when nothing is lost)
	}
	cases := []tcase{
		{name: "reserved-serial-types", want: 2, code: "record-reserved-serial", fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			tt.InsertRaw(1, []uint64{0, 10, 11, 1}, []byte{5})
			tt.InsertRaw(2, []uint64{0, 1, 1, 1}, []byte{1, 2, 3})
		}},
		{name: "text-claims-2-pow-40", want: 2, code: "record-invalid", fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			tt.InsertRaw(1, []uint64{0, 1, 2*(1<<40) + 13}, []byte{1})
			tt.InsertRaw(2, []uint64{0, 1}, []byte{1})
		}},
		{name: "blob-claims-2-pow-40", want: 1, code: "record-invalid", fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			tt.InsertRaw(1, []uint64{0, 1, 2*(1<<40) + 12}, []byte{1})
		}},
		{name: "nine-byte-varint-serials", want: 2, code: "record-invalid", fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			tt.InsertRaw(1, []uint64{0, 1 << 62, math.MaxUint64}, []byte{1})
			tt.InsertRaw(2, []uint64{0, 1}, []byte{3})
		}},
		{name: "extreme-rowids", want: 5, fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			for _, id := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
				tt.Insert(id, nil, id, "x")
			}
		}},
		{name: "blob-64MiB-plus-1", opts: sqlitetest.Options{PageSize: 4096}, want: 1, code: "cell-too-large", fill: func(tt *sqlitetest.Table, _ *sqlitetest.Builder) {
			tt.Insert(1, nil, int64(7), make([]byte, 64<<20+1))
		}},
		{name: "header-past-the-cell", want: 3, code: "record-invalid", fill: func(tt *sqlitetest.Table, b *sqlitetest.Builder) {
			for i := int64(1); i <= 3; i++ {
				tt.Insert(i, nil, i, "xy")
			}
			// The payload's first byte is the header size; row 2's is patched to
			// a value past the cell. Cell: payload-size varint, rowid varint, payload.
			_, page, off := tt.CellBytes(2)
			b.Patch(int(page-1)*1024+off+2, 0x7f)
		}},
		{name: "duplicate-rowids-across-leaves", want: 200, code: "btree-order", opts: sqlitetest.Options{PageSize: 512}, fill: func(tt *sqlitetest.Table, b *sqlitetest.Builder) {
			for i := int64(1); i <= 200; i++ {
				tt.Insert(i, nil, i, strings.Repeat("v", 20))
			}
			// Row 190 (a late leaf) takes the rowid 130 (also a two-byte varint).
			_, page, off := tt.CellBytes(190)
			b.Patch(int(page-1)*512+off+1, 0x81, 0x02)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			if opts.PageSize == 0 {
				opts.PageSize = 1024
			}
			data := newBuilderDB(t, opts, func(b *sqlitetest.Builder) {
				c.fill(b.CreateTable("t", "create table t(id integer primary key, a, b)"), b)
			})
			o := exercise(t, filesOf(data, nil, nil), 512<<20)
			if o.openErr != nil {
				return // a typed refusal is loud
			}
			got := o.rows["t"]
			if o.scanErr["t"] == nil && got < c.want && o.scanWarn == 0 {
				t.Errorf("scan delivered %d of %d rows with no error and no warning (silent loss)", got, c.want)
			}
			if c.code != "" && !o.scanCodes[c.code] {
				t.Errorf("no %q warning from the scan among %v", c.code, o.scanCodes)
			}
			t.Logf("rows %d/%d scanErr=%v warnings=%d codes=%v", got, c.want, o.scanErr["t"], o.warnings, o.codes)
		})
	}
	t.Run("blob-over-cap-is-omitted", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096}, func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(id integer primary key, a)").Insert(1, nil, make([]byte, 64<<20+1))
		})
		d := openBytes(t, data, nil, nil, &testBudget{limit: 512 << 20})
		tb := mustTable(t, d, "t")
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if st := r.State(1); st != sqlitedb.StateOmitted {
				t.Errorf("a 64 MiB + 1 blob is %v, want omitted", st)
			}
			checkRowInvariants(t, r)
		})
	})
}
