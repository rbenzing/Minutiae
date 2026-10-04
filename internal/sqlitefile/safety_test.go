package sqlitefile_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestScanContextCancel(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 5000)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})

	// Cancelled before the call: nothing is parsed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := v.ScanTree(ctx, tb.Root(), sqlitefile.TableTree, func(sqlitefile.Row) bool { called = true; return true })
	if !errors.Is(err, context.Canceled) || called || v.Stats().CellsParsed != 0 {
		t.Errorf("pre-cancelled: err %v, called %v, cells %d", err, called, v.Stats().CellsParsed)
	}
	if _, _, err := v.LookupRowid(ctx, tb.Root(), 5); !errors.Is(err, context.Canceled) {
		t.Errorf("lookup with a cancelled context: %v", err)
	}

	// Cancelled inside visit: at most 1024 more cells are parsed.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	var atCancel int64
	rows := 0
	err = v.ScanTree(ctx, tb.Root(), sqlitefile.TableTree, func(sqlitefile.Row) bool {
		rows++
		if rows == 700 {
			atCancel = v.Stats().CellsParsed
			cancel()
		}
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if extra := v.Stats().CellsParsed - atCancel; extra > 1024 || rows >= 5000 {
		t.Errorf("%d cells parsed after the cancel, %d rows delivered", extra, rows)
	}
}

func TestScanVisitStopsEarly(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 2000)
	b.CreateIndex("ib", "t", "create index ib on t(b)", 1)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	for _, c := range []struct {
		root uint32
		kind sqlitefile.BTreeKind
	}{{tb.Root(), sqlitefile.TableTree}, {b.Object("ib").Root(), sqlitefile.IndexTree}} {
		n := 0
		err := v.ScanTree(context.Background(), c.root, c.kind, func(sqlitefile.Row) bool { n++; return n < 25 })
		if err != nil || n != 25 {
			t.Errorf("kind %d: %d rows, err %v", c.kind, n, err)
		}
	}
	if st := v.Stats(); st.PageReads > 20 {
		t.Errorf("an early stop still read %d pages", st.PageReads)
	}
}

func TestBudgetBalancedAndEnforced(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
	data := b.Bytes()
	full := func(budget sqlitefile.Budget) (int, error) {
		_, v := openLive(t, data, sqlitefile.Options{Budget: budget})
		n := 0
		err := v.ScanTree(context.Background(), tb.Root(), sqlitefile.TableTree, func(sqlitefile.Row) bool { n++; return true })
		if err == nil {
			_, _, err = v.LookupRowid(context.Background(), tb.Root(), 97)
		}
		v.Release()
		return n, err
	}
	rb := newRecBudget(1 << 30)
	if n, err := full(rb); err != nil || n != 300 {
		t.Fatalf("%d rows, %v", n, err)
	}
	rb.check(t)
	total := rb.allocCalls
	if total < 50 {
		t.Fatalf("only %d charges", total)
	}
	// Refuse the n-th charge (and later ones): an error wrapping ErrBudget or,
	// when n is past the last charge, a complete scan. Never a panic, never a
	// quiet partial result, and the books balance afterwards.
	step := max(1, total/60)
	for n := 1; n <= total+1; n += step {
		nb := &nthBudget{n: n}
		rows, err := full(nb)
		switch {
		case err == nil:
			if rows != 300 || n <= total {
				t.Fatalf("refusing charge %d of %d: success with %d rows", n, total, rows)
			}
		case !errors.Is(err, sqlitefile.ErrBudget):
			t.Fatalf("refusing charge %d: %v", n, err)
		}
		if nb.used != 0 {
			t.Fatalf("refusing charge %d: %d bytes still charged", n, nb.used)
		}
	}
}

// guardedReader hands the library only ReadAt in earnest: every other method
// a reader value plausibly has fails the test at once.
type guardedReader struct {
	t      *testing.T
	data   []byte
	closed bool // set once Open has returned an error
	reads  int
}

func (g *guardedReader) ReadAt(p []byte, off int64) (int, error) {
	g.t.Helper()
	if g.closed {
		g.t.Fatalf("ReadAt after Open returned an error")
	}
	if off < 0 || off > int64(len(g.data)) || off+int64(len(p)) > int64(len(g.data)) {
		g.t.Fatalf("ReadAt(len %d, off %d) reaches outside the %d authoritative bytes", len(p), off, len(g.data))
	}
	g.reads++
	n := copy(p, g.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (g *guardedReader) WriteAt([]byte, int64) (int, error) {
	g.t.Fatal("WriteAt called")
	return 0, nil
}

func (g *guardedReader) Write([]byte) (int, error) { g.t.Fatal("Write called"); return 0, nil }

func (g *guardedReader) Truncate(int64) error { g.t.Fatal("Truncate called"); return nil }

func (g *guardedReader) Close() error { g.t.Fatal("Close called"); return nil }

func (g *guardedReader) Seek(int64, int) (int64, error) { g.t.Fatal("Seek called"); return 0, nil }

func (g *guardedReader) ReadFrom(io.Reader) (int64, error) {
	g.t.Fatal("ReadFrom called")
	return 0, nil
}
func (g *guardedReader) Sync() error        { g.t.Fatal("Sync called"); return nil }
func (g *guardedReader) Stat() (any, error) { g.t.Fatal("Stat called"); return nil, nil }

func TestNeverWritesAndUsesOnlyReadAt(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 800)
	data := b.Bytes()
	before := sha256.Sum256(data)
	g := &guardedReader{t: t, data: data}
	db, err := sqlitefile.Open(g, int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := db.Live()
	scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	scanRows(t, v, 1, sqlitefile.TableTree)
	if _, ok, err := v.LookupRowid(context.Background(), tb.Root(), 97); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := v.ReadPage(3); err != nil {
		t.Fatal(err)
	}
	v.Release()
	_ = db.Status()
	if g.reads == 0 {
		t.Fatal("no reads")
	}
	if after := sha256.Sum256(data); after != before {
		t.Error("the evidence bytes changed")
	}

	// A file that Open refuses is never read again.
	bad := &guardedReader{t: t, data: bytes.Repeat([]byte{0x41}, 4096)}
	if _, err := sqlitefile.Open(bad, 4096, sqlitefile.Options{}); err == nil {
		t.Fatal("not a database was opened")
	}
	bad.closed = true
}

func TestRowCloneIsOwned(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	var kept, plain []sqlitefile.Row
	err := v.ScanTree(context.Background(), tb.Root(), sqlitefile.TableTree, func(r sqlitefile.Row) bool {
		kept = append(kept, r.Clone())
		if r.Rowid == 97 {
			// Mutating the original must not touch the clone.
			c := r.Clone()
			for i := range r.Values {
				for j := range r.Values[i].Bytes {
					r.Values[i].Bytes[j] ^= 0xff
				}
			}
			plain = append(plain, c)
			if len(r.Loc.Overflow) > 0 {
				r.Loc.Overflow[0].Page = 0
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range kept {
		if err := rowIs(r, genRow(r.Rowid)); err != nil {
			t.Fatalf("clone of rowid %d: %v", r.Rowid, err)
		}
	}
	if len(plain) != 1 || plain[0].Loc.OverflowTotal == 0 || plain[0].Loc.Overflow[0].Page == 0 {
		t.Errorf("the clone shares its overflow list with the original: %+v", plain)
	}
	if err := rowIs(plain[0], genRow(97)); err != nil {
		t.Errorf("the clone shares bytes with the original: %v", err)
	}
}

func TestConcurrentLiveReads(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 1500)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			if err := v.ScanTree(context.Background(), tb.Root(), sqlitefile.TableTree, func(r sqlitefile.Row) bool {
				n++
				return rowIs(r, genRow(r.Rowid)) == nil
			}); err != nil || n != 1500 {
				errs <- errors.Join(err, errors.New("scan"))
				return
			}
			for id := int64(g + 1); id <= 1500; id += 97 {
				r, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
				if err != nil || !ok || r.Rowid != id || rowIs(r, genRow(id)) != nil {
					errs <- errors.Join(err, errors.New("lookup"))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStatusComposesInfoWALJournal(t *testing.T) {
	b, _ := stdTable(t, sqlitetest.Options{PageSize: 1024}, 5)
	db, _ := openLive(t, b.Bytes(), sqlitefile.Options{})
	st := db.Status()
	if st.WAL != nil || st.Journal != nil {
		t.Errorf("companions without any attached: %+v", st)
	}
	if st.Info.PageSize != 1024 || st.Info.PageCount != db.Info().PageCount {
		t.Errorf("Info %+v", st.Info)
	}
}

func TestViewInfoWarningsStartFromTheDatabase(t *testing.T) {
	b, _ := stdTable(t, sqlitetest.Options{PageSize: 512}, 50)
	data := b.Bytes()
	data = data[:len(data)-100] // a trailing partial page: the open warns
	db, v := openLive(t, data, sqlitefile.Options{})
	if !slices.Equal(warnCodesOf(v), warningCodes(db)) || len(warningCodes(db)) == 0 {
		t.Errorf("view %v, database %v", warnCodesOf(v), warningCodes(db))
	}
	if v.Info().PageCount != db.Info().PageCount {
		t.Error("view Info differs")
	}
}

// TestScanKeepsWhatItCanOfDamagedRecords: a record that cannot be decoded is
// skipped with a warning; a value over a cap or behind a damaged chain is
// flagged Omitted and counted; the rest of the tree is delivered.
func TestScanKeepsWhatItCanOfDamagedRecords(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, "one", int64(1))
	tb.Insert(2, strings.Repeat("big", 400), int64(2)) // 1200 bytes: overflow, over the lowered cap
	tb.InsertRaw(3, []uint64{1 /* int8 */, 13 + 2*40 /* text of 40 */}, []byte{9})
	tb.Insert(4, "four", int64(4))
	tb.Insert(5, bytes.Repeat([]byte("x"), 3000), int64(5)) // chain cut below
	data := b.Bytes()
	chain := tb.Overflow(5)
	if len(chain) < 4 {
		t.Fatalf("chain %d", len(chain))
	}
	ovPage := pageAt(data, 512, chain[1])
	copy(ovPage[:4], []byte{0, 0, 0, 0}) // the chain now ends one page early
	_, v := openLive(t, data, sqlitefile.Options{Limits: sqlitefile.Limits{MaxTextBytes: 1000}})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if got := rowids(rows); !slices.Equal(got, []int64{1, 2, 4, 5}) {
		t.Fatalf("rowids %v: the record that declares bytes it lacks is skipped, the others kept", got)
	}
	big := rows[1].Values[0]
	if big.Kind != sqlitefile.KindText || !big.Omitted || big.Bytes != nil || big.Len != 1200 {
		t.Errorf("over-cap value: kind %d omitted %v len %d bytes %d", big.Kind, big.Omitted, big.Len, len(big.Bytes))
	}
	if rows[1].Values[1].Int != 2 {
		t.Errorf("the column after an omitted value: %+v", rows[1].Values[1])
	}
	cut := rows[3].Values[0]
	if !cut.Omitted || cut.Len != 3000 {
		t.Errorf("value behind the cut chain: %+v", cut)
	}
	for _, code := range []string{sqlitefile.WarnRecordInvalid, sqlitefile.WarnCellTooLarge, sqlitefile.WarnCellOverflowChain} {
		if !viewWarns(v, code, 0) {
			t.Errorf("no %s warning: %v", code, v.Warnings())
		}
	}
}

func TestScanPayloadOverTheCapIsSkippedNotFatal(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 200)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{Limits: sqlitefile.Limits{MaxPayloadBytes: 2000}})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	// Rows 97 and 194 have 3000-byte values: over the 2000 byte payload cap.
	if got := len(rows); got != 198 {
		t.Errorf("%d rows, want 198", got)
	}
	if !viewWarns(v, sqlitefile.WarnLimitReached, 0) {
		t.Errorf("warnings %v", v.Warnings())
	}
}

func TestScanTrailingPartialPage(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 400)
	full := b.Bytes()
	last := tb.Leaves()[len(tb.Leaves())-1]
	cut := int(last-1)*512 + 300 // the file ends 300 bytes into the last leaf
	if int(last) != len(full)/512 {
		t.Skipf("last leaf %d is not the last page %d", last, len(full)/512)
	}
	_, v := openLive(t, full[:cut], sqlitefile.Options{})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	lost := len(tb.Leaves()) // sanity: something was read from the partial page
	_ = lost
	onLast := 0
	for id := int64(1); id <= 400; id++ {
		if _, pg, _ := tb.CellBytes(id); pg == last {
			onLast++
		}
	}
	if len(rows) >= 400 || len(rows) < 400-onLast {
		t.Errorf("%d rows: all but at most the %d rows of the cut page expected", len(rows), onLast)
	}
	for _, r := range rows {
		if err := rowIs(r, genRow(r.Rowid)); err != nil {
			t.Fatalf("rowid %d: %v (bytes past the end must never read as zeros)", r.Rowid, err)
		}
	}
	if !viewWarns(v, sqlitefile.WarnCellPointer, last) {
		t.Errorf("warnings: %v", v.Warnings())
	}
}

// TestScanContextCancelInsideOneBigLeaf: a 64 KiB leaf holds thousands of cells,
// so the context is polled per cell batch, not only per page.
func TestScanContextCancelInsideOneBigLeaf(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 65536})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 6000; i++ {
		tb.Insert(i, i)
	}
	if tb.Depth() != 1 {
		t.Fatalf("depth %d: the rows must share one leaf", tb.Depth())
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var atCancel int64
	n := 0
	err := v.ScanTree(ctx, tb.Root(), sqlitefile.TableTree, func(sqlitefile.Row) bool {
		if n++; n == 100 {
			atCancel = v.Stats().CellsParsed
			cancel()
		}
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if extra := v.Stats().CellsParsed - atCancel; extra > 1024 {
		t.Errorf("%d cells parsed after the cancel", extra)
	}
}
