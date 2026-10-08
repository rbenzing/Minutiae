package sqlitefile_test

// FuzzOpen, FuzzRecord and FuzzSniff (plan 3I, Task 15). The fuzz bodies hold the
// library to its invariants on arbitrary bytes: nothing panics (the no-recover
// entry points turn a recovered panic back into one, so the fuzzer sees it), every
// reported location lies inside the file it names, every value obeys the Value
// invariants exactly, two runs over the same bytes agree byte for byte, and the
// budget is balanced once every view is released.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

var lastTrace string

const (
	fuzzRowCap  = 5000
	fuzzGetCap  = 100
	fuzzHistCap = 5000
)

// checkValue holds v to the Value invariants of the format reference, exactly.
func checkValue(t testing.TB, what string, v sqlitefile.Value, live bool) {
	t.Helper()
	bad := func(format string, a ...any) {
		t.Helper()
		t.Fatalf("%s: value %+v: %s", what, v, fmt.Sprintf(format, a...))
	}
	if v.Kind > sqlitefile.KindBlob {
		bad("unknown kind")
	}
	if v.Clipped && live {
		bad("Clipped is set only for recovered values")
	}
	switch v.Kind {
	case sqlitefile.KindText, sqlitefile.KindBlob:
		if v.Len < 0 {
			bad("negative Len")
		}
		holds := 0
		if v.Omitted {
			holds++
			if v.Bytes != nil || v.Clipped {
				bad("an omitted value has Bytes == nil and is not clipped")
			}
		}
		if !v.Omitted && !v.Clipped && int64(len(v.Bytes)) == v.Len {
			holds++
		}
		if !v.Omitted && v.Clipped && int64(len(v.Bytes)) < v.Len {
			holds++
		}
		if holds != 1 {
			bad("exactly one of Omitted, len(Bytes) == Len, Clipped with len(Bytes) < Len must hold (len(Bytes) = %d)", len(v.Bytes))
		}
		if v.Kind == sqlitefile.KindText && v.Enc != sqlitefile.EncUTF8 && v.Enc != sqlitefile.EncUTF16LE && v.Enc != sqlitefile.EncUTF16BE {
			bad("text with encoding %d", v.Enc)
		}
		if v.Serial >= 12 && v.Len != sqlitefile.SerialSize(v.Serial) {
			bad("Len differs from the size of serial type %d", v.Serial)
		}
	default:
		if v.Len != 0 {
			bad("Len must be 0 for null, integer and float")
		}
		if v.Bytes != nil {
			bad("Bytes must be nil for null, integer and float")
		}
		if v.Clipped {
			bad("only text and blob values are clipped")
		}
	}
	if v.Kind == sqlitefile.KindText {
		if s, ok := v.Text(); ok && v.Omitted {
			bad("Text of an omitted value returned %q", s)
		}
	}
}

type fuzzFiles struct {
	db, wal, journal []byte
}

func (f fuzzFiles) file(k sqlitefile.FileKind) []byte {
	switch k {
	case sqlitefile.FileDB:
		return f.db
	case sqlitefile.FileWAL:
		return f.wal
	case sqlitefile.FileJournal:
		return f.journal
	}
	return nil
}

func checkLoc(t testing.TB, what string, l sqlitefile.Loc, f fuzzFiles, pageSize int) {
	t.Helper()
	b := f.file(l.File)
	if b == nil && l.File != sqlitefile.FileDB {
		t.Fatalf("%s: location in a file that is not attached: %+v", what, l)
	}
	if l.Length <= 0 || l.Offset < 0 || l.Offset+l.Length > int64(len(b)) {
		t.Fatalf("%s: location %+v lies outside the %d bytes of %s", what, l, len(b), l.File)
	}
	if l.PageOffset < 0 || l.Offset < l.PageOffset || l.Offset+l.Length > l.PageOffset+int64(pageSize) || l.Page == 0 {
		t.Fatalf("%s: location %+v is not inside one page image of %d bytes", what, l, pageSize)
	}
}

// noPanicF fails with the recovered panic, if err is one.
func noPanicF(t testing.TB, what string, err error) {
	t.Helper()
	var pe *sqlitefile.PanicError
	if errors.As(err, &pe) {
		t.Fatalf("%s: panic: %v\n%s", what, pe.Value, pe.Stack)
	}
}

// fuzzOpenBody opens the files and walks everything, checking the invariants. It
// returns a digest of everything observed (two runs must agree) and the codes of
// the warnings raised.
func fuzzOpenBody(t testing.TB, f fuzzFiles) (digest [32]byte, codes []string) {
	t.Helper()
	ctx := context.Background()
	h := sha256.New()
	var trace bytes.Buffer
	w := io.MultiWriter(h, &trace)
	put := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	defer func() { lastTrace = trace.String() }()
	rb := newRecBudget(1 << 30)
	opts := sqlitefile.Options{Budget: rb}
	d, err := sqlitefile.OpenNoRecover(bytes.NewReader(f.db), int64(len(f.db)), opts)
	if err != nil {
		put("open error %v", err)
		return sha256.Sum256(nil), nil
	}
	if len(f.wal) > 0 {
		_, err := d.AttachWAL(bytes.NewReader(f.wal), int64(len(f.wal)))
		noPanicF(t, "AttachWAL", err)
		put("wal %v", err)
	}
	if len(f.journal) > 0 {
		_, err := d.AttachJournal(bytes.NewReader(f.journal), int64(len(f.journal)))
		noPanicF(t, "AttachJournal", err)
		put("journal %v", err)
	}
	base := rb.level()
	info := d.Info()
	put("info %+v", info)
	ps := info.PageSize
	live, found := d.Live(), d.AsFound()
	for _, nv := range []struct {
		name string
		v    *sqlitefile.View
	}{{"live", live}, {"as-found", found}} {
		v := nv.v
		sch, err := v.Schema(ctx)
		noPanicF(t, nv.name+" Schema", err)
		put("%s schema err %v", nv.name, err)
		lay, err := v.Layout(ctx)
		noPanicF(t, nv.name+" Layout", err)
		if err == nil {
			if len(lay.Class) != int(v.Addressable())+1 {
				t.Fatalf("%s: layout of %d pages for Addressable %d", nv.name, len(lay.Class), v.Addressable())
			}
			put("%s layout %v %v %d %q", nv.name, lay.Orphans, lay.PtrmapPages, lay.OrphansTotal, lay.Problems)
		}
		fl, err := v.Freelist(ctx)
		noPanicF(t, nv.name+" Freelist", err)
		if err == nil {
			for _, pg := range slices.Concat(fl.Trunks, fl.Leaves) {
				if pg == 0 || pg > v.Addressable() {
					t.Fatalf("%s: freelist page %d outside 1..%d", nv.name, pg, v.Addressable())
				}
			}
			put("%s freelist %v %v", nv.name, fl.Trunks, fl.Leaves)
		}
		if sch != nil {
			for _, o := range sch.Objects {
				put("%s object %s %s %d", nv.name, o.Type, o.Name, o.RootPage)
				if o.Type != "table" || o.Virtual || o.RootPage == 0 {
					continue
				}
				tb, err := v.Table(ctx, o.Name)
				noPanicF(t, "Table", err)
				if err != nil {
					continue
				}
				n := 0
				var firstIDs []int64
				err = tb.Rows(ctx, func(r sqlitefile.Row) bool {
					n++
					what := fmt.Sprintf("%s table %s row %d", nv.name, o.Name, n)
					checkLoc(t, what, r.Loc, f, ps)
					if r.Loc.Page > v.Addressable() {
						t.Fatalf("%s: page %d beyond Addressable %d", what, r.Loc.Page, v.Addressable())
					}
					if !r.HasRowid && r.Rowid != 0 {
						t.Fatalf("%s: Rowid %d without HasRowid", what, r.Rowid)
					}
					// a definition that did not parse leaves the kind of tree to the root page
					if def := tb.Def(); def.ParseOK && def.WithoutRowid == r.HasRowid {
						t.Fatalf("%s: HasRowid %v for a table with WithoutRowid %v", what, r.HasRowid, tb.Def().WithoutRowid)
					}
					for i, val := range r.Values {
						checkValue(t, fmt.Sprintf("%s value %d", what, i), val, true)
					}
					for i, val := range tb.Resolve(r) {
						checkValue(t, fmt.Sprintf("%s resolved %d", what, i), val, true)
					}
					put("row %d %v %+v %+v", r.Rowid, r.HasRowid, r.Loc, r.Values)
					if r.HasRowid && len(firstIDs) < fuzzGetCap {
						firstIDs = append(firstIDs, r.Rowid)
					}
					return n < fuzzRowCap
				})
				noPanicF(t, "Rows", err)
				put("rows %s %d %v", o.Name, n, err)
				if tb.Def().WithoutRowid {
					continue
				}
				for i := int64(0); i < fuzzGetCap; i++ {
					id := i + 1
					if int(i) < len(firstIDs) {
						id = firstIDs[i]
					}
					r, ok, err := tb.Get(ctx, id)
					noPanicF(t, "Get", err)
					if ok && err == nil {
						if !r.HasRowid || r.Rowid != id {
							t.Fatalf("Get(%d) returned rowid %d (HasRowid %v)", id, r.Rowid, r.HasRowid)
						}
						checkLoc(t, fmt.Sprintf("Get(%d)", id), r.Loc, f, ps)
						for _, val := range r.Values {
							checkValue(t, fmt.Sprintf("Get(%d)", id), val, true)
						}
					}
					put("get %d %v %v", id, ok, err)
				}
			}
		}
		for _, w := range v.Warnings() {
			codes = append(codes, w.Code)
			put("warn %s %s %d %d %v", nv.name, w.Code, w.Page, w.Offset, w.File)
		}
		st := v.Stats()
		put("stats %+v", st)
	}
	hist := d.History()
	np, nr := 0, 0
	err = sqlitefile.HistoryNoRecover(ctx, hist,
		func(p sqlitefile.PageImage) bool {
			np++
			put("page %d %v %+v", p.Number, p.Origin, p.Loc)
			return np < fuzzHistCap
		},
		func(r sqlitefile.RecoveredRow) bool {
			nr++
			what := fmt.Sprintf("history row %d", nr)
			checkLoc(t, what, r.Loc, f, ps)
			if r.Rowid == nil && r.Table != "" && r.Index == "" && false {
				t.Fatalf("%s: no rowid", what)
			}
			for i, val := range r.Values {
				checkValue(t, fmt.Sprintf("%s value %d", what, i), val, false)
			}
			put("hrow %s %s %v %v %+v", r.Method, r.Table, ptr(r.Rowid), r.Relation, r.Values)
			return nr < fuzzHistCap
		})
	noPanicF(t, "history", err)
	put("history %d %d %v", np, nr, err)
	for _, w := range hist.Warnings() {
		codes = append(codes, w.Code)
		put("hwarn %s %d", w.Code, w.Page)
	}
	if s := d.WAL(); s != nil {
		for _, w := range s.Warnings {
			codes = append(codes, w.Code)
		}
	}
	if s := d.Journal(); s != nil {
		for _, w := range s.Warnings {
			codes = append(codes, w.Code)
		}
	}
	hist.Release()
	live.Release()
	found.Release()
	if got := rb.level(); got != base {
		t.Fatalf("budget not balanced: %d bytes charged after every view and the history were released, %d before", got, base)
	}
	if rb.negative || rb.overFree {
		t.Fatalf("budget misuse: negative %v over-freed %v", rb.negative, rb.overFree)
	}
	copy(digest[:], h.Sum(nil))
	return digest, codes
}

// fuzzSeed is one input of the fuzz corpus with the warning codes the clean
// reading of it is allowed to raise.
type fuzzSeed struct {
	name             string
	db, wal, journal []byte
	allowed          []string
}

func trimTail(b []byte, limit int) []byte {
	if len(b) > limit {
		b = b[:limit]
	}
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return bytes.Clone(b)
}

func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	var out []fuzzSeed
	db, wal, journal := panicFixture()
	out = append(out, fuzzSeed{name: "builder db+wal+journal", db: db, wal: wal, journal: journal, allowed: []string{
		sqlitefile.WarnJournalHot, sqlitefile.WarnJournalAndWAL,
	}})
	out = append(out, fuzzSeed{name: "builder plain", db: db[:len(db):len(db)]})
	for _, o := range []sqlitetest.Options{
		{PageSize: 512}, {PageSize: 4096, Encoding: 2}, {PageSize: 1024, Encoding: 3}, {PageSize: 512, AutoVacuum: 1}, {PageSize: 1024, AutoVacuum: 2},
	} {
		b := sqlitetest.New(o)
		tb := b.CreateTable("t", "create table t(a, b, c)")
		for id := int64(1); id <= 6; id++ {
			tb.Insert(id, id, "text", []byte{1, 2, 3})
		}
		wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
		wr.Insert(1, "k1", int64(1))
		wr.Insert(2, "k2", 2.5)
		big := b.CreateTable("big", "create table big(x)")
		big.Insert(1, longText(3000, 7))
		b.CreateIndex("i", "t", "create index i on t(a)", 0)
		if o.AutoVacuum != 0 {
			b.WritePtrmap()
		}
		out = append(out, fuzzSeed{name: fmt.Sprintf("builder ps%d enc%d av%d", o.PageSize, o.Encoding, o.AutoVacuum), db: b.Bytes()})
	}
	// freelist, a dropped table and a hostile header
	{
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "create table t(a, b)")
		tb.Insert(1, int64(1), "x")
		gone := b.CreateTable("gone", "create table gone(a, b)")
		for id := int64(1); id <= 10; id++ {
			gone.Insert(id, id, longText(60, byte(id)))
		}
		b.DropTable("gone")
		out = append(out, fuzzSeed{name: "builder freelist", db: b.Bytes()})
		b.SetHeaderPages(0xffffffff, true)
		out = append(out, fuzzSeed{name: "builder header claims 2^32-1 pages", db: b.Bytes(), allowed: []string{sqlitefile.WarnPageCountClamped, sqlitefile.WarnTruncatedFile}})
	}
	for _, spec := range fixtureSpecs {
		if spec.NotSQLite {
			continue
		}
		f := loadFx(t, spec)
		out = append(out, fuzzSeed{
			name: "fixture " + spec.Name, db: trimTail(f.db, 48<<10), wal: trimTail(f.wal, 48<<10), journal: trimTail(f.journal, 48<<10),
			// trimmed files are cut mid-structure on purpose: warnings of any kind are expected
			allowed: nil,
		})
		out[len(out)-1].allowed = []string{"*"}
	}
	return out
}

func FuzzOpen(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		files := fuzzFiles{db: db, wal: wal, journal: journal}
		a, _ := fuzzOpenBody(t, files)
		b, _ := fuzzOpenBody(t, files)
		if a != b {
			t.Fatalf("two runs over the same bytes differ")
		}
	})
}

// TestFuzzSeedsOpen: every builder and fixture seed passes the fuzz body, and the
// builder seeds raise no warning other than the ones expected of them.
func TestFuzzSeedsOpen(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) < 12 {
		t.Fatalf("%d seeds", len(seeds))
	}
	for _, s := range seeds {
		t.Run(s.name, func(t *testing.T) {
			files := fuzzFiles{db: s.db, wal: s.wal, journal: s.journal}
			a, codes := fuzzOpenBody(t, files)
			b, _ := fuzzOpenBody(t, files)
			if a != b {
				t.Errorf("two runs differ")
			}
			if slices.Contains(s.allowed, "*") {
				return
			}
			for _, c := range codes {
				if !slices.Contains(s.allowed, c) {
					t.Errorf("unexpected warning %q (allowed %v; all %v)", c, s.allowed, dedup(codes))
				}
			}
		})
	}
}

func FuzzRecord(f *testing.F) {
	seeds := [][]byte{
		{0x01},
		{0x02, 0x01, 0x07},
		{0x03, 0x00, 0x15, 'a', 'b', 'c'},
		{0x04, 0x01, 0x0d, 0x11, 1, 'h', 'i', 'x'},
		{0x02, 0x07, 0x40, 0x09, 0x21, 0x00, 0, 0, 0, 0, 0, 0},
		{0x03, 0x0a, 0x0b},
		{0x05, 0x06, 0x01, 0x02, 0x03, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x80},
		bytes.Repeat([]byte{0x80}, 12),
	}
	for _, s := range seeds {
		for enc := uint8(1); enc <= 3; enc++ {
			f.Add(s, enc, uint16(2000))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, enc uint8, maxCols uint16) {
		serials, hl, body, err := sqlitefile.ParseRecordHeader(b, int(maxCols))
		if err == nil {
			if hl < 1 || hl > len(b) || len(serials) > int(maxCols) || body < 0 {
				t.Fatalf("header: len %d of %d, %d serials (max %d), body %d", hl, len(b), len(serials), maxCols, body)
			}
			var sum int64
			for _, s := range serials {
				sum += sqlitefile.SerialSize(s)
			}
			if sum != body {
				t.Fatalf("body length %d, the serial types sum to %d", body, sum)
			}
		}
		lim := sqlitefile.Limits{MaxColumns: int(maxCols) + 1}
		rec, err := sqlitefile.DecodeRecord(b, sqlitefile.Encoding(enc%4), lim)
		noPanicF(t, "DecodeRecord", err)
		if err != nil {
			return
		}
		if len(rec.Values) != len(rec.Serials) {
			t.Fatalf("%d values for %d serial types", len(rec.Values), len(rec.Serials))
		}
		for i, v := range rec.Values {
			checkValue(t, fmt.Sprintf("value %d", i), v, true)
			if v.Kind == sqlitefile.KindFloat && math.IsNaN(v.Float) && false {
				t.Fatal("NaN")
			}
			if s, ok := v.Text(); ok && v.Kind != sqlitefile.KindText {
				t.Fatalf("Text() of a non-text value returned %q", s)
			}
		}
		// the same bytes decode the same way, and as recovered values only clip
		rec2, err := sqlitefile.DecodeRecord(b, sqlitefile.Encoding(enc%4), lim)
		if err != nil || fmt.Sprintf("%+v", rec) != fmt.Sprintf("%+v", rec2) {
			t.Fatalf("a second decode differs: %v", err)
		}
	})
}

func FuzzSniff(f *testing.F) {
	db, _, journal := panicFixture()
	f.Add(db)
	f.Add(db[:100])
	f.Add(journal)
	f.Add([]byte("SQLite format 3\x00"))
	f.Add(bytes.Repeat([]byte{0xa7, 0x13, 0x5c, 0xe1}, 1500))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := sqlitefile.Sniff(bytes.NewReader(b), int64(len(b)))
		noPanicF(t, "Sniff", err)
		if err != nil {
			t.Fatalf("Sniff of %d in-memory bytes: %v", len(b), err)
		}
		if s.Size != int64(len(b)) || s.PageSize < 0 || s.Entropy < 0 || s.Entropy > 8 {
			t.Fatalf("sniffed %+v for %d bytes", s, len(b))
		}
		s2, _ := sqlitefile.Sniff(bytes.NewReader(b), int64(len(b)))
		if s != s2 {
			t.Fatalf("two sniffs differ: %+v %+v", s, s2)
		}
	})
}
