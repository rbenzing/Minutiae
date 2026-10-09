package sqlitedb_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// recTable builds a database holding the one table t (sql) and returns the
// resolved table, its DB and budget.
func recTable(t testing.TB, sql string, fill func(tt *sqlitetest.Table)) (*sqlitedb.Table, *sqlitedb.DB, *testBudget) {
	t.Helper()
	return joinTable(t, sqlitetest.Options{PageSize: 1024}, sql, fill)
}

func ip(v int64) *int64 { return &v }

// rawLive scans the live view of data with the library and returns its rows of
// table name (the library rows a recovered row is rebuilt from).
func rawLive(t testing.TB, data []byte, name string) []sqlitefile.Row {
	t.Helper()
	lib, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tb, err := lib.Live().Table(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []sqlitefile.Row
	if err := tb.Rows(t.Context(), func(r sqlitefile.Row) bool {
		r.Values = append([]sqlitefile.Value(nil), r.Values...)
		out = append(out, r)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameRow(t testing.TB, label string, want, got sqlitedb.Row) {
	t.Helper()
	if want.NumCols() != got.NumCols() {
		t.Fatalf("%s: NumCols %d vs %d", label, want.NumCols(), got.NumCols())
	}
	wid, wok := want.Rowid()
	gid, gok := got.Rowid()
	if wid != gid || wok != gok {
		t.Errorf("%s: Rowid %d,%v vs %d,%v", label, wid, wok, gid, gok)
	}
	for c := range want.NumCols() {
		if want.State(c) != got.State(c) {
			t.Errorf("%s col %d: State %v vs %v", label, c, want.State(c), got.State(c))
		}
		if !reflect.DeepEqual(want.Value(c), got.Value(c)) {
			t.Errorf("%s col %d: Value %+v vs %+v", label, c, want.Value(c), got.Value(c))
		}
		if want.IsNull(c) != got.IsNull(c) || want.Unknown(c) != got.Unknown(c) || want.Kind(c) != got.Kind(c) {
			t.Errorf("%s col %d: IsNull/Unknown/Kind differ", label, c)
		}
		wi, wiok := want.Int(c)
		gi, giok := got.Int(c)
		wf, wfok := want.Float(c)
		gf, gfok := got.Float(c)
		wt, wtok := want.Text(c)
		gt, gtok := got.Text(c)
		wb, wbok := want.Blob(c)
		gb, gbok := got.Blob(c)
		wr, wrawok := want.RawText(c)
		gr, grawok := got.RawText(c)
		if wi != gi || wiok != giok || math.Float64bits(wf) != math.Float64bits(gf) || wfok != gfok || !bytes.Equal(wt, gt) || wtok != gtok ||
			!bytes.Equal(wb, gb) || wbok != gbok || !bytes.Equal(wr, gr) || wrawok != grawok {
			t.Errorf("%s col %d: typed accessors differ", label, c)
		}
	}
	if want.Flags() != got.Flags() || want.ExtraValues() != got.ExtraValues() {
		t.Errorf("%s: Flags %v/%d vs %v/%d", label, want.Flags(), want.ExtraValues(), got.Flags(), got.ExtraValues())
	}
	wr, wrole := want.Range()
	gr, grole := got.Range()
	if wr != gr || wrole != grole {
		t.Errorf("%s: Range %+v %q vs %+v %q", label, wr, wrole, gr, grole)
	}
	if want.Table() != got.Table() {
		t.Errorf("%s: Table %q vs %q", label, want.Table(), got.Table())
	}
}

func TestFromRecoveredSameAccessorsAsLive(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, a, b text, c real default 7, d blob, e text default 'x', f default (1+1))")
		tt.Insert(1, 1, 42, "hello", 2.5, []byte{1, 2, 3}, "e", nil)
		tt.InsertRaw(2, []uint64{0, 1}, []byte{9}) // short: b..f take their defaults
		tt.Insert(3, 3, nil, "ünï", 3, nil, "", 5)
		tt.InsertRaw(4, []uint64{0, 1, 0, 0, 0, 0, 0, 1}, []byte{1, 2}) // an extra value
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var live []sqlitedb.Row
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		c, err := r.Clone()
		live = append(live, c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raws := rawLive(t, data, "t")
	if len(live) != 4 || len(raws) != 4 {
		t.Fatalf("rows: %d live, %d raw", len(live), len(raws))
	}
	for i, raw := range raws {
		id := raw.Rowid
		rr := sqlitefile.RecoveredRow{Method: "m", Table: "T", Rowid: &id, Values: raw.Values, Loc: raw.Loc}
		got, err := sqlitedb.FromRecovered(tb, rr)
		if err != nil {
			t.Fatalf("row %d: %v", id, err)
		}
		sameRow(t, fmt.Sprintf("row %d", id), live[i], got)
		if got.Recovered() == nil || got.Recovered().Method != "m" {
			t.Errorf("row %d: Recovered() = %+v", id, got.Recovered())
		}
		got.Release()
	}
}

func TestFromRecoveredLostRowidAliasIsLostNotNull(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(id integer primary key, a)", func(*sqlitetest.Table) {})
	rr := sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}, {Kind: sqlitefile.KindInt, Int: 5}}}
	r, err := sqlitedb.FromRecovered(tb, rr)
	if err != nil {
		t.Fatal(err)
	}
	if r.State(0) != sqlitedb.StateLost || r.IsNull(0) || r.Unknown(0) == false {
		t.Errorf("alias column: state %v, IsNull %v; a lost rowid is not a NULL", r.State(0), r.IsNull(0))
	}
	if _, ok := r.Int(0); ok {
		t.Error("Int of a lost alias answered")
	}
	if id, ok := r.Rowid(); ok || id != 0 {
		t.Errorf("Rowid = %d, %v", id, ok)
	}
	if r.State(1) != sqlitedb.StatePresent || r.Flags()&sqlitedb.FlagUnknownValues == 0 {
		t.Errorf("col 1 state %v, flags %v", r.State(1), r.Flags())
	}
}

func TestFromRecoveredNilRowidDoesNotPanic(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a, b text)", func(*sqlitetest.Table) {})
	rr := sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindInt, Int: 3}, {Kind: sqlitefile.KindText, Bytes: []byte("x"), Len: 1, Enc: sqlitefile.EncUTF8},
	}}
	r, err := sqlitedb.FromRecovered(tb, rr)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := r.Rowid(); ok || id != 0 {
		t.Errorf("Rowid = %d, %v; want 0, false", id, ok)
	}
	if v, ok := r.Int(0); !ok || v != 3 {
		t.Errorf("a = %d, %v", v, ok)
	}
	if s, ok := r.Text(1); !ok || string(s) != "x" {
		t.Errorf("b = %q, %v", s, ok)
	}
	if r.Flags() != 0 {
		t.Errorf("Flags = %v", r.Flags())
	}
}

func TestFromRecoveredTruncatedTailIsUnread(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a, b default 7, c)", func(*sqlitetest.Table) {})
	one := []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}}
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: one, Truncated: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.State(0) != sqlitedb.StatePresent || r.State(1) != sqlitedb.StateUnread || r.State(2) != sqlitedb.StateUnread {
		t.Errorf("states %v %v %v; the cut-off tail is unread, not its default or NULL", r.State(0), r.State(1), r.State(2))
	}
	if _, ok := r.Int(1); ok {
		t.Error("the default of a truncated record was presented as a value")
	}
	if rec := r.Recovered(); rec == nil || !rec.Truncated {
		t.Errorf("Recovered = %+v", rec)
	}
	if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
		t.Error("FlagUnknownValues not set")
	}
}

func TestFromRecoveredShortRecordAppliesLiteralDefaults(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a, b default 7, c text default 'x', d default (1+1), e)", func(*sqlitetest.Table) {})
	one := []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}}
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: one})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := r.Int(1); !ok || v != 7 || r.State(1) != sqlitedb.StateDefaulted {
		t.Errorf("b = %d, %v, %v; want the literal default 7", v, ok, r.State(1))
	}
	if s, ok := r.Text(2); !ok || string(s) != "x" || r.State(2) != sqlitedb.StateDefaulted {
		t.Errorf("c = %q, %v, %v", s, ok, r.State(2))
	}
	if r.State(3) != sqlitedb.StateOmitted {
		t.Errorf("d (non-literal default) = %v, want omitted", r.State(3))
	}
	if r.State(4) != sqlitedb.StateDefaulted || !r.IsNull(4) {
		t.Errorf("e (no default) = %v null %v", r.State(4), r.IsNull(4))
	}
	if r.Flags()&sqlitedb.FlagShortRecord == 0 {
		t.Error("FlagShortRecord not set")
	}
}

func TestFromRecoveredClippedIsNotComplete(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a text, b, c blob, d)", func(*sqlitetest.Table) {})
	rr := sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindText, Bytes: []byte("abc"), Len: 5000, Clipped: true, Enc: sqlitefile.EncUTF8},
		{Kind: sqlitefile.KindInt, Unread: true},
		{Kind: sqlitefile.KindBlob, Len: 9000, Omitted: true},
		{Kind: sqlitefile.KindFloat, Unread: true},
	}}
	r, err := sqlitedb.FromRecovered(tb, rr)
	if err != nil {
		t.Fatal(err)
	}
	if r.State(0) != sqlitedb.StateClipped {
		t.Errorf("clipped text state %v", r.State(0))
	}
	if _, ok := r.Text(0); ok {
		t.Error("Text answered for a clipped value")
	}
	if raw, ok := r.RawText(0); !ok || string(raw) != "abc" {
		t.Errorf("RawText = %q, %v", raw, ok)
	}
	if r.State(1) != sqlitedb.StateUnread || r.State(3) != sqlitedb.StateUnread {
		t.Errorf("unread scalars: %v, %v", r.State(1), r.State(3))
	}
	if _, ok := r.Int(1); ok {
		t.Error("an unread integer was presented as a value")
	}
	if _, ok := r.Float(3); ok {
		t.Error("an unread real was presented as a value")
	}
	if r.State(2) != sqlitedb.StateOmitted {
		t.Errorf("omitted blob state %v", r.State(2))
	}
	if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
		t.Error("FlagUnknownValues not set")
	}
}

func TestFromRecoveredTableMismatchIsAnError(t *testing.T) {
	tb, _, b := recTable(t, "create table t(a)", func(*sqlitetest.Table) {})
	vals := []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}}
	for name, rr := range map[string]sqlitefile.RecoveredRow{
		"index entry":   {Method: "m", Table: "t", Index: "i", Values: vals},
		"unknown table": {Method: "m", Values: vals},
		"other table":   {Method: "m", Table: "u", Values: vals},
		"prefix":        {Method: "m", Table: "tt", Values: vals},
	} {
		used := b.used
		r, err := sqlitedb.FromRecovered(tb, rr)
		if !errors.Is(err, sqlitedb.ErrRowMismatch) || r.NumCols() != 0 {
			t.Errorf("%s: %v, %d cols; want ErrRowMismatch and the zero Row", name, err, r.NumCols())
		}
		if b.used != used {
			t.Errorf("%s: the refusal charged %d bytes", name, b.used-used)
		}
	}
	if r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "T", Values: vals}); err != nil {
		t.Errorf("ASCII case-insensitive table name refused: %v", err)
	} else {
		r.Release()
	}
}

func TestFromRecoveredCarriesLabelUnchanged(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(id integer primary key, a)", func(*sqlitetest.Table) {})
	wal := &sqlitefile.WALProv{Frame: 3, Salt1: 7, Salt2: 8, Committed: true, Generation: 1, Note: "n"}
	jr := &sqlitefile.JournalProv{Record: 2, Segment: 1, Note: "j", ChecksumOK: true, Hot: true, Applied: true, Nonce: 9}
	rr := sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodWALStale, Origin: sqlitefile.OriginWALStale, Table: "t", TableBasis: sqlitefile.BasisFit,
		Rowid: ip(12), Relation: sqlitefile.RelUnknown, Truncated: false, WAL: wal, Journal: jr, Notes: []string{"identity-by-fit-only", "x"},
		Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}, {Kind: sqlitefile.KindInt, Int: 1}},
		Loc:    sqlitefile.Loc{File: sqlitefile.FileWAL, Frame: 3, Offset: 100, Length: 20},
	}
	r, err := sqlitedb.FromRecovered(tb, rr)
	if err != nil {
		t.Fatal(err)
	}
	rec := r.Recovered()
	if rec == nil || rec.Method != rr.Method || rec.Origin != rr.Origin || rec.Basis != rr.TableBasis || rec.Relation != rr.Relation ||
		rec.Truncated != rr.Truncated || !reflect.DeepEqual(rec.WAL, wal) || !reflect.DeepEqual(rec.Journal, jr) ||
		!reflect.DeepEqual(rec.Notes, rr.Notes) {
		t.Errorf("Recovered = %+v", rec)
	}
	if rec.WAL == wal || rec.Journal == jr {
		t.Error("the provenance pointers are the caller's")
	}
	if loc, ok := r.Locator(); ok || loc != "" {
		t.Errorf("Locator = %q, %v; a recovered row has none", loc, ok)
	}
	if id, ok := r.Rowid(); !ok || id != 12 {
		t.Errorf("Rowid = %d, %v", id, ok)
	}
	if v, ok := r.Int(0); !ok || v != 12 || r.State(0) != sqlitedb.StatePresent {
		t.Errorf("alias column = %d, %v, %v", v, ok, r.State(0))
	}
}

func TestFromRecoveredRangeRoles(t *testing.T) {
	for _, fx := range []struct {
		name string
		file sqlitefile.FileKind
		role parse.FileRole
	}{
		{"persist-journal", sqlitefile.FileJournal, parse.RoleJournal},
		{"builder-journal-multiseg", sqlitefile.FileJournal, parse.RoleJournal},
		{"hot-journal", sqlitefile.FileDB, parse.RoleDB}, // its history is the rolled-back database pages
		{"wal-uncheckpointed", sqlitefile.FileWAL, parse.RoleWAL},
		{"freelist-dropped", sqlitefile.FileDB, parse.RoleDB},
	} {
		t.Run(fx.name, func(t *testing.T) {
			db, wal, journal := loadFixture(t, fx.name)
			d := openBytes(t, db, wal, journal, bigBudget())
			lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if wal != nil {
				if _, err := lib.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
					t.Fatal(err)
				}
			}
			if journal != nil {
				if _, err := lib.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
					t.Fatal(err)
				}
			}
			matched, noTable := 0, 0
			_, err = lib.History().Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
				if rr.Index != "" || rr.Table == "" {
					r, err := sqlitedb.FromRecovered(mustTable(t, d, firstTableName(t, d)), rr)
					if !errors.Is(err, sqlitedb.ErrRowMismatch) {
						t.Errorf("row without a table or with an index: %v, %v", r.NumCols(), err)
					}
					return true
				}
				tb, err := d.Table(t.Context(), rr.Table, nil, nil)
				if err != nil {
					noTable++
					return true
				}
				r, err := sqlitedb.FromRecovered(tb, rr)
				if err != nil {
					t.Errorf("FromRecovered(%s): %v", rr.Table, err)
					return true
				}
				defer r.Release()
				rg, role := r.Range()
				if rr.Loc.File != fx.file {
					return true
				}
				matched++
				if role != fx.role || rg.Offset != rr.Loc.Offset || rg.Length != rr.Loc.Length {
					t.Errorf("Range = %+v %q, want %d+%d %q", rg, role, rr.Loc.Offset, rr.Loc.Length, fx.role)
				}
				if r.Loc().File != fx.file {
					t.Errorf("Loc().File = %v", r.Loc().File)
				}
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			if matched == 0 {
				t.Fatalf("no recovered row of %s lay in the %s file (%d had no live table)", fx.name, fx.role, noTable)
			}
		})
	}
}

func firstTableName(t testing.TB, d *sqlitedb.DB) string {
	t.Helper()
	for _, n := range []string{"a", "kv", "people", "t"} {
		if _, err := d.Table(t.Context(), n, nil, nil); err == nil {
			return n
		}
	}
	t.Fatal("no known table in the fixture")
	return ""
}

func mustTable(t testing.TB, d *sqlitedb.DB, name string) *sqlitedb.Table {
	t.Helper()
	tb, err := d.Table(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestFromRecoveredOwnsItsMemory(t *testing.T) {
	tb, _, b := recTable(t, "create table t(id integer primary key, a text, b blob)", func(*sqlitetest.Table) {})
	before := b.used
	text := []byte("hello")
	blob := []byte{1, 2, 3}
	wal := &sqlitefile.WALProv{Frame: 1, Note: "n"}
	notes := []string{"n1"}
	rr := sqlitefile.RecoveredRow{
		Method: "m", Table: "t", Rowid: ip(4), WAL: wal, Notes: notes,
		Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}, {Kind: sqlitefile.KindText, Bytes: text, Len: 5, Enc: sqlitefile.EncUTF8}, {Kind: sqlitefile.KindBlob, Bytes: blob, Len: 3}},
		Loc:    sqlitefile.Loc{File: sqlitefile.FileDB, Overflow: []sqlitefile.PagePart{{}}},
	}
	r, err := sqlitedb.FromRecovered(tb, rr)
	if err != nil {
		t.Fatal(err)
	}
	if b.used <= before {
		t.Fatalf("nothing charged: %d -> %d", before, b.used)
	}
	// Overwrite everything the caller owns.
	for i := range text {
		text[i] = '#'
	}
	for i := range blob {
		blob[i] = 0xFF
	}
	wal.Note, wal.Frame = "scribbled", 99
	notes[0] = "scribbled"
	rr.Values[1].Bytes = nil
	rr.Loc.Overflow[0].Page = 77
	if s, ok := r.Text(1); !ok || string(s) != "hello" {
		t.Errorf("text = %q", s)
	}
	if bl, ok := r.Blob(2); !ok || !bytes.Equal(bl, []byte{1, 2, 3}) {
		t.Errorf("blob = %v", bl)
	}
	if rec := r.Recovered(); rec.WAL.Note != "n" || rec.WAL.Frame != 1 || rec.Notes[0] != "n1" {
		t.Errorf("Recovered = %+v %+v", rec, rec.WAL)
	}
	if r.Loc().Overflow[0].Page == 77 {
		t.Error("Loc.Overflow aliases the caller's slice")
	}
	r.Release()
	r.Release() // idempotent
	if b.used != before {
		t.Errorf("used %d after Release, want %d", b.used, before)
	}
}

func TestFromRecoveredUtf16TextIsDecodedNow(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a text)").Insert(1, 1, "x")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb := mustTable(t, d, "t")
	raw := []byte{'h', 0, 'i', 0}
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindText, Bytes: raw, Len: 4, Enc: sqlitefile.EncUTF16LE},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'X'
	if s, ok := r.Text(0); !ok || string(s) != "hi" {
		t.Errorf("Text = %q, %v", s, ok)
	}
	bad, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindText, Bytes: []byte{'h', 0, 0x00, 0xD8}, Len: 4, Enc: sqlitefile.EncUTF16LE},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if bad.State(0) != sqlitedb.StateUndecodable {
		t.Errorf("lone surrogate state %v, want undecodable", bad.State(0))
	}
}

func TestFromRecoveredBudgetRefusal(t *testing.T) {
	tb, _, b := recTable(t, "create table t(id integer primary key, a text)", func(*sqlitetest.Table) {})
	b.limit = b.used // nothing more may be charged
	used := b.used
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Rowid: ip(1), Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindNull}, {Kind: sqlitefile.KindText, Bytes: []byte("abc"), Len: 3, Enc: sqlitefile.EncUTF8},
	}})
	if !errors.Is(err, parse.ErrBudget) || r.NumCols() != 0 {
		t.Fatalf("FromRecovered = %d cols, %v; want parse.ErrBudget and the zero Row", r.NumCols(), err)
	}
	if b.used != used {
		t.Errorf("used %d after the refusal, want %d", b.used, used)
	}
}

// countingReader fails the test on any read once armed.
type countingReader struct {
	r     *bytes.Reader
	armed atomic.Bool
	reads atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	if c.armed.Load() {
		c.reads.Add(1)
		return 0, errors.New("read after the table was resolved")
	}
	return c.r.ReadAt(p, off)
}

func TestFromRecoveredIsPure(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(id integer primary key, a text default 'd', b)").Insert(1, 1, "x", 2)
	})
	cr := &countingReader{r: bytes.NewReader(data)}
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: cr, DBSize: int64(len(data))}, bigBudget())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	tb := mustTable(t, d, "t")
	cr.armed.Store(true)
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Rowid: ip(5), Values: []sqlitefile.Value{
		{Kind: sqlitefile.KindNull}, {Kind: sqlitefile.KindText, Bytes: []byte("v"), Len: 1, Enc: sqlitefile.EncUTF8},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n := cr.reads.Load(); n != 0 {
		t.Errorf("FromRecovered read the file %d times", n)
	}
	if s, ok := r.Text(1); !ok || string(s) != "v" {
		t.Errorf("a = %q, %v", s, ok)
	}
	if r.State(2) != sqlitedb.StateDefaulted {
		t.Errorf("b state %v", r.State(2))
	}
}

// A table from one DB takes a row built from another database's recovery.
func TestFromRecoveredTableMayComeFromAnotherDB(t *testing.T) {
	tb, _, b := recTable(t, "create table t(a)", func(*sqlitetest.Table) {})
	before := b.used
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{Method: "m", Table: "t", Values: []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if b.used <= before {
		t.Errorf("charges: %d -> %d", before, b.used)
	}
	r.Release()
}

func TestFromRecoveredLengthMismatchNoteDoesNotDefaultTheTail(t *testing.T) {
	tb, _, _ := recTable(t, "create table t(a, b default 7)", func(*sqlitetest.Table) {})
	r, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{
		Method: "m", Table: "t", Notes: []string{sqlitefile.NoteRecordLengthMismatch},
		Values: []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.State(1) != sqlitedb.StateUnread || r.Flags()&sqlitedb.FlagLengthMismatch == 0 {
		t.Errorf("b state %v, flags %v; a damaged record's missing tail is unread, not its default", r.State(1), r.Flags())
	}
}
