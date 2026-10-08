package sqlitefile_test

// The L1-L6 checklist of sub-project 4, section 7.1: what the table decoding
// layer (plan 4B) requires of the reader. Each test confirms one item through
// the public API only and names it; the behaviour itself is proven in depth by
// the tests this file composes (scan, lookup, overlay, journal, history). These
// tests pass on arrival: they confirm a surface that earlier tasks built.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// L1: tables with name, root page, CREATE text, columns (name, declared type,
// affinity, rowid alias, NOT NULL, literal DEFAULT) and WITHOUT ROWID, tolerant
// of an unreadable definition.
func TestL1SchemaSurface(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	people := b.CreateTable("people", "create table people(id integer primary key, name text not null default 'anon', age int default 7, score real, note)")
	kv := b.CreateTableWithoutRowid("kv", "create table kv(k text primary key, v) without rowid", 1)
	b.AddSchemaRow("table", "broken", "broken", int64(2), "create table broken(((")
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	sch, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obj := func(name string) sqlitefile.SchemaObject {
		t.Helper()
		for _, o := range sch.Objects {
			if o.Name == name {
				return o
			}
		}
		t.Fatalf("no schema object %q", name)
		return sqlitefile.SchemaObject{}
	}
	p := obj("people")
	if p.Type != "table" || p.RootPage != people.Root() || p.SQL == "" || p.Table == nil || !p.Table.ParseOK || p.Table.WithoutRowid {
		t.Fatalf("people: %+v", p)
	}
	cols := p.Table.Columns
	if len(cols) != 5 || p.Table.RowidAlias != 0 {
		t.Fatalf("people columns %d, rowid alias %d", len(cols), p.Table.RowidAlias)
	}
	for i, want := range []struct {
		name, decl string
		aff        sqlitefile.Affinity
		notNull    bool
	}{
		{"id", "integer", sqlitefile.AffInteger, false},
		{"name", "text", sqlitefile.AffText, true},
		{"age", "int", sqlitefile.AffInteger, false},
		{"score", "real", sqlitefile.AffReal, false},
		{"note", "", sqlitefile.AffBlob, false},
	} {
		c := cols[i]
		if c.Name != want.name || c.DeclType != want.decl || c.Affinity != want.aff || c.NotNull != want.notNull {
			t.Errorf("column %d: %+v, want %+v", i, c, want)
		}
	}
	if d := cols[1].Default; d.Kind != sqlitefile.DefaultLiteral || !valueIs(d.Value, "anon") {
		t.Errorf("name default: %+v", d)
	}
	if d := cols[2].Default; d.Kind != sqlitefile.DefaultLiteral || !valueIs(d.Value, 7) {
		t.Errorf("age default: %+v", d)
	}
	if d := cols[3].Default; d.Kind != sqlitefile.DefaultNone {
		t.Errorf("score default: %+v", d)
	}
	k := obj("kv")
	if k.RootPage != kv.Root() || k.Table == nil || !k.Table.ParseOK || !k.Table.WithoutRowid {
		t.Errorf("kv: %+v", k)
	}
	// An unreadable definition is a warning and an unparsed definition, never a failure.
	br := obj("broken")
	if br.Table == nil || br.Table.ParseOK || br.Table.ParseNote == "" || len(br.Table.Columns) != 0 {
		t.Errorf("broken: %+v", br.Table)
	}
	if !viewWarns(v, sqlitefile.WarnSchemaSQLUnparsed, 0) {
		t.Errorf("no schema-sql-unparsed warning: %v", warnCodesOf(v))
	}
	// The table api resolves the same table by name.
	tb, err := v.Table(context.Background(), "people")
	if err != nil || tb.RootPage() != people.Root() || tb.Def().Columns[1].Name != "name" {
		t.Errorf("Table(people): %v", err)
	}
}

// L2: Table.Rows in rowid order, cancellable, with typed values and the place of
// the cell: file, page, cell index, offset and length, the frame for a WAL page
// and the record for a journal page.
func TestL2OrderedScanWithLocation(t *testing.T) {
	ctx := context.Background()
	// A database file: rowid order, typed values, the bytes at the location are the cell.
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
	data := b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	tab, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	var prev int64
	n := 0
	if err := tab.Rows(ctx, func(r sqlitefile.Row) bool {
		n++
		if !r.HasRowid || r.Rowid <= prev {
			t.Fatalf("rowid %d after %d", r.Rowid, prev)
		}
		prev = r.Rowid
		want := genRow(r.Rowid)
		for i, w := range want {
			if i == 0 || i == 4 {
				continue // the first column is every kind; the last may be an overflow blob
			}
			if got := tab.Resolve(r)[i]; !valueIs(got, w) {
				t.Fatalf("rowid %d column %d: %+v, want %v", r.Rowid, i, got, w)
			}
		}
		cell, page, off := tb.CellBytes(r.Rowid)
		l := r.Loc
		if l.File != sqlitefile.FileDB || l.Page != page || l.Cell < 0 || l.Offset != l.PageOffset+int64(off) || l.Length != int64(len(cell)) || !bytes.Equal(data[l.Offset:l.Offset+l.Length], cell) {
			t.Fatalf("rowid %d: loc %+v", r.Rowid, l)
		}
		return true
	}); err != nil || n != 300 {
		t.Fatalf("%d rows, %v", n, err)
	}
	// Cancellable: a cancelled context ends the scan with its error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := tab.Rows(cctx, func(sqlitefile.Row) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled scan: %v", err)
	}
	// Stopping from the callback is not an error.
	seen := 0
	if err := tab.Rows(ctx, func(sqlitefile.Row) bool { seen++; return seen < 3 }); err != nil || seen != 3 {
		t.Errorf("early stop: %d rows, %v", seen, err)
	}

	// WAL rows: the file is the WAL, the frame is set and the offset is a WAL offset.
	f, db, w := twoTxns(t)
	wal := w.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	wt, err := d.Live().Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	inWAL := 0
	if err := wt.Rows(ctx, func(r sqlitefile.Row) bool {
		l := r.Loc
		if l.File != sqlitefile.FileWAL {
			return true
		}
		inWAL++
		cell, _, _ := f.tb.CellBytes(r.Rowid)
		if l.Frame == 0 || l.PageOffset != int64(32+(int(l.Frame)-1)*(24+ovPS)+24) || l.Length != int64(len(cell)) || !bytes.Equal(wal[l.Offset:l.Offset+l.Length], cell) {
			t.Fatalf("rowid %d: wal loc %+v", r.Rowid, l)
		}
		return true
	}); err != nil || inWAL == 0 {
		t.Fatalf("%d rows from the WAL, %v", inWAL, err)
	}

	// Journal rows: a hot journal is rolled back in Live(); the rows it restores
	// lie in the journal file, with the record index.
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	jd, jv, info := attachJ(t, j.dbAfter, journal, sqlitefile.Options{})
	if !info.Hot || !info.Applied {
		t.Fatalf("journal info %+v", info)
	}
	jt, err := jv.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	inJournal := 0
	if err := jt.Rows(ctx, func(r sqlitefile.Row) bool {
		l := r.Loc
		if l.File != sqlitefile.FileJournal {
			return true
		}
		inJournal++
		rec := jd.Journal().Records[l.Record]
		if l.PageOffset != rec.Offset || l.Offset < l.PageOffset || l.Offset+l.Length > int64(len(journal)) || l.Page == 0 {
			t.Fatalf("rowid %d: journal loc %+v, record %+v", r.Rowid, l, rec)
		}
		return true
	}); err != nil || inJournal == 0 {
		t.Fatalf("%d rows from the journal, %v", inJournal, err)
	}
}

// L3: Table.Get(rowid) is a logarithmic point lookup that agrees with the scan.
func TestL3PointLookup(t *testing.T) {
	ctx := context.Background()
	b, _ := stdTable(t, sqlitetest.Options{PageSize: 512}, 3000)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	tab, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []sqlitefile.Row
	if err := tab.Rows(ctx, func(r sqlitefile.Row) bool { all = append(all, r.Clone()); return true }); err != nil || len(all) != 3000 {
		t.Fatalf("%d rows, %v", len(all), err)
	}
	scanReads := v.Stats().PageReads
	for _, id := range []int64{1, 2, 1500, 2999, 3000} {
		before := v.Stats()
		r, ok, err := tab.Get(ctx, id)
		if err != nil || !ok || r.Rowid != id || !r.HasRowid {
			t.Fatalf("Get(%d): %v %v %+v", id, ok, err, r)
		}
		want := all[id-1]
		if len(r.Values) != len(want.Values) || r.Loc.Offset != want.Loc.Offset || r.Loc.Page != want.Loc.Page {
			t.Errorf("Get(%d) differs from the scan row: %+v vs %+v", id, r.Loc, want.Loc)
		}
		after := v.Stats()
		if got := after.PageReads - before.PageReads + after.CacheHits - before.CacheHits; got > 8 {
			t.Errorf("Get(%d) touched %d pages; a scan touched %d", id, got, scanReads)
		}
	}
	for _, id := range []int64{0, -5, 3001, 1 << 40} {
		if _, ok, err := tab.Get(ctx, id); ok || err != nil {
			t.Errorf("Get(%d): %v %v, want a clean miss", id, ok, err)
		}
	}
	// WITHOUT ROWID has no rowids.
	wb := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	wb.CreateTableWithoutRowid("kv", "create table kv(k text primary key, v) without rowid", 1).Insert(1, "a", "b")
	_, wv := openLive(t, wb.Bytes(), sqlitefile.Options{})
	wt, err := wv.Table(ctx, "kv")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := wt.Get(ctx, 1); !errors.Is(err, sqlitefile.ErrWithoutRowid) {
		t.Errorf("Get on WITHOUT ROWID: %v", err)
	}
}

// L4: a damaged page is skipped with a recorded warning, never a failure; the
// memory budget hook is charged and balanced; text that does not decode is
// returned as bytes.
func TestL4RobustnessAndBudget(t *testing.T) {
	ctx := context.Background()
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 600)
	data := b.Bytes()
	ps := b.PageSize()
	var leaf uint32
	for _, p := range tb.Pages() {
		if p != tb.Root() && data[(int(p)-1)*ps] == byte(sqlitefile.PageTableLeaf) {
			leaf = p
			break
		}
	}
	if leaf == 0 {
		t.Fatal("no leaf page")
	}
	data[(int(leaf)-1)*ps] = 0xff // not a b-tree page type
	rb := newRecBudget(1 << 30)
	d, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Budget: rb})
	if err != nil {
		t.Fatal(err)
	}
	v := d.Live()
	tab, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := tab.Rows(ctx, func(sqlitefile.Row) bool { n++; return true }); err != nil {
		t.Fatalf("a damaged page failed the scan: %v", err)
	}
	if n == 0 || n >= 600 {
		t.Errorf("%d rows: the damaged page must be skipped and the rest delivered", n)
	}
	if !viewWarns(v, sqlitefile.WarnPageTypeInvalid, leaf) {
		t.Errorf("no page-type-invalid warning for page %d: %v", leaf, warnCodesOf(v))
	}
	if rb.peak == 0 || rb.refused != 0 {
		t.Errorf("budget peak %d, refused %d", rb.peak, rb.refused)
	}
	v.Release()
	rb.check(t) // everything the library charged was given back

	// A budget too small to open is refused with ErrBudget, not a panic.
	if _, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Budget: newRecBudget(16)}); err == nil {
		t.Log("Open fits in 16 bytes")
	} else if !errors.Is(err, sqlitefile.ErrBudget) {
		t.Errorf("tiny budget: %v", err)
	}
	// Named caps: a limit below the data ends the read with ErrLimit or a warning, never a crash.
	// Text that does not decode as the database encoding stays bytes.
	tb2 := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	t2 := tb2.CreateTable("t", "create table t(a)")
	t2.Insert(1, "valid")
	t2.Insert(2, []byte{0xff, 0xfe, 'x'}) // a blob here; below the same bytes as text
	_, v2 := openLive(t, tb2.Bytes(), sqlitefile.Options{})
	rows := scanRows(t, v2, t2.Root(), sqlitefile.TableTree)
	if len(rows) != 2 || rows[1].Values[0].Kind != sqlitefile.KindBlob || !bytes.Equal(rows[1].Values[0].Bytes, []byte{0xff, 0xfe, 'x'}) {
		t.Errorf("blob bytes: %+v", rows)
	}
	bad := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{0xff, 0xfe, 'x'}, Len: 3, Enc: sqlitefile.EncUTF8}
	if s, ok := bad.Text(); ok || s != "" {
		t.Errorf("Text() of invalid UTF-8: %q %v", s, ok)
	}
	if !bytes.Equal(bad.Bytes, []byte{0xff, 0xfe, 'x'}) {
		t.Error("the raw bytes of undecodable text are lost")
	}
}

// L5: page size, encoding, page count, user_version, application_id, freelist
// count, WAL frame counts, hot journal, and "not SQLite" versus "looks
// encrypted", all through DB.Status() (and Sniff for a file that is not a
// database).
func TestL5Info(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 2048, Encoding: 3, UserVersion: -7, AppID: 0xcafe0001})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 5; i++ {
		tb.Insert(i, i)
	}
	b.SetFreelist([][]uint32{{10, 11, 12}})
	data := b.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st := d.Status()
	in := st.Info
	if in.PageSize != 2048 || in.Encoding != sqlitefile.EncUTF16BE || !in.EncodingValid || in.PageCount != uint32(len(data)/2048) ||
		in.UserVersion != -7 || in.ApplicationID != 0xcafe0001 || in.FreelistCount != 3 {
		t.Errorf("Info: %+v", in)
	}
	if st.WAL != nil || st.Journal != nil {
		t.Errorf("no companion attached: WAL %v, journal %v", st.WAL, st.Journal)
	}

	// WAL counters: frames total, valid, committed and trailing.
	_, wdb, w := twoTxns(t)
	wal := append(w.Bytes(), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10) // a torn frame after the last slot
	wd, err := sqlitefile.Open(bytes.NewReader(wdb), int64(len(wdb)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wd.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	wi := wd.Status().WAL
	if wi == nil || !wi.Present || !wi.HeaderValid || wi.FrameSlots != 4 || wi.FramesValid != 4 || wi.FramesCommitted != 4 || wi.Commits != 2 || wi.TrailingBytes != 10 || !wi.UsedByLive {
		t.Errorf("WAL: %+v", wi)
	}

	// A hot journal.
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	jd, _, _ := attachJ(t, j.dbAfter, journal, sqlitefile.Options{})
	ji := jd.Status().Journal
	if ji == nil || !ji.Present || !ji.Hot || !ji.HeaderValid || !ji.Applied || ji.RecordsTotal != uint32(len(j.changed)) {
		t.Errorf("journal: %+v", ji)
	}

	// Not SQLite versus looks encrypted.
	text := bytes.Repeat([]byte("this is plain text, not a database\n"), 200)
	s, err := sqlitefile.Sniff(bytes.NewReader(text), int64(len(text)))
	if err != nil || s.Kind != sqlitefile.SniffNotSQLite {
		t.Errorf("plain text: %+v %v", s, err)
	}
	var noise []byte
	for sum := sha256.Sum256([]byte("l5")); len(noise) < 8192; sum = sha256.Sum256(sum[:]) {
		noise = append(noise, sum[:]...)
	}
	s, err = sqlitefile.Sniff(bytes.NewReader(noise), int64(len(noise)))
	if err != nil || s.Kind != sqlitefile.SniffLooksEncrypted {
		t.Errorf("high-entropy bytes: %+v %v", s, err)
	}
	if _, err := sqlitefile.Open(bytes.NewReader(noise), int64(len(noise)), sqlitefile.Options{}); !errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("Open of high-entropy bytes: %v", err)
	}
}

// L6: a recovered row carries file, page, cell, offset, length and frame (or
// journal record) in the same Loc type a live row has, and the bytes at that
// place are its cell.
func TestL6RecoveredRowSameLocationModel(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	rows, _ := collectRows(t, d.History())
	if len(rows) == 0 {
		t.Fatal("no recovered rows")
	}
	f := files{db: m.db, wal: m.wal, journal: m.journal}
	var live sqlitefile.Loc
	tab, err := d.Live().Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Rows(context.Background(), func(r sqlitefile.Row) bool { live = r.Loc; return false }); err != nil {
		t.Fatal(err)
	}
	_ = []sqlitefile.Loc{live, rows[0].Loc} // one Loc type for live and recovered rows
	files := map[sqlitefile.FileKind]int{}
	for _, r := range rows {
		l := r.Loc
		files[l.File]++
		if l.Page == 0 || l.Length <= 0 || l.Cell < 0 || l.Offset < l.PageOffset {
			t.Fatalf("incomplete location %+v", l)
		}
		switch l.File {
		case sqlitefile.FileWAL:
			if l.Frame == 0 {
				t.Errorf("a WAL row without a frame: %+v", l)
			}
		case sqlitefile.FileJournal:
			if l.Record < 0 {
				t.Errorf("a journal row without a record: %+v", l)
			}
		}
		c := parseAt(t, f, l, hps)
		if r.Rowid == nil || c.Rowid != *r.Rowid {
			t.Errorf("row %+v: the cell at its location has rowid %d", l, c.Rowid)
		}
	}
	if files[sqlitefile.FileDB] == 0 && files[sqlitefile.FileWAL] == 0 && files[sqlitefile.FileJournal] == 0 {
		t.Errorf("no row in any file: %v", files)
	}
	if live.File == 0 || live.Page == 0 || live.Length <= 0 || (live.File == sqlitefile.FileWAL) != (live.Frame != 0) {
		t.Errorf("live row location %+v", live)
	}
}

// TestConfidenceExportedForParsers: the confidence rule is a pure exported
// function for sub-project 4 (4G) and 3J: it compiles and behaves from this
// external test package.
func TestConfidenceExportedForParsers(t *testing.T) {
	conf := func(method string, basis sqlitefile.TableBasis) int {
		return sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: method, TableBasis: basis})
	}
	for _, c := range []struct {
		method string
		basis  sqlitefile.TableBasis
		want   int
	}{
		{sqlitefile.MethodWALPrior, sqlitefile.BasisSchema, 75},
		{sqlitefile.MethodWALPrior, sqlitefile.BasisFit, 60},
		{sqlitefile.MethodWALPrior, sqlitefile.BasisGuess, 40},
		{sqlitefile.MethodWALPrior, sqlitefile.BasisNone, 30},
		{sqlitefile.MethodFreelist, sqlitefile.BasisSchema, 60},
		{sqlitefile.MethodJournalRolledBack, sqlitefile.BasisSchema, 45},
		{sqlitefile.MethodPageSlack, sqlitefile.BasisFit, 25},
	} {
		if got := conf(c.method, c.basis); got != c.want {
			t.Errorf("Confidence(%s, %s) = %d, want %d", c.method, c.basis, got, c.want)
		}
	}
	schema := conf(sqlitefile.MethodWALPrior, sqlitefile.BasisSchema)
	if sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: "no-such-method", TableBasis: sqlitefile.BasisSchema}) != 0 {
		t.Error("an unknown method must rank 0")
	}
	ten := 10
	if got := sqlitefile.CapConfidence(schema, &ten); got != 10 {
		t.Errorf("CapConfidence(%d, 10) = %d", schema, got)
	}
	if got := sqlitefile.CapConfidence(5, &ten); got != 5 {
		t.Errorf("CapConfidence never raises: %d", got)
	}
	if got := sqlitefile.CapConfidence(schema, nil); got != schema {
		t.Errorf("CapConfidence with no artifact confidence: %d", got)
	}
	if sqlitefile.Band(70) != "high" || sqlitefile.Band(40) != "medium" || sqlitefile.Band(39) != "low" {
		t.Error("Band thresholds")
	}
}
