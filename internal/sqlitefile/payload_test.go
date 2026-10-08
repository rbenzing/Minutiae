package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// recBudget is a recording Budget: it counts and orders what is charged and
// refuses past limit bytes in use.
type recBudget struct {
	mu                 sync.Mutex
	limit              int64
	used, peak         int64
	allocs, frees      int64 // bytes charged and given back in total
	allocCalls         int
	refused            int
	negative, overFree bool
}

func newRecBudget(limit int64) *recBudget { return &recBudget{limit: limit} }

func (b *recBudget) Alloc(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 {
		b.negative = true
	}
	if b.used+n > b.limit {
		b.refused++
		return fmt.Errorf("%w: refused %d bytes", sqlitefile.ErrBudget, n)
	}
	b.used += n
	b.allocs += n
	b.allocCalls++
	b.peak = max(b.peak, b.used)
	return nil
}

func (b *recBudget) Free(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.used {
		b.overFree = true
	}
	b.used -= n
	b.frees += n
}

func (b *recBudget) check(t *testing.T) {
	t.Helper()
	if b.used != 0 || b.allocs != b.frees || b.negative || b.overFree {
		t.Errorf("budget not balanced: used %d, charged %d, freed %d, negative %v, over-freed %v", b.used, b.allocs, b.frees, b.negative, b.overFree)
	}
}

const ovPage = 512 // page size (and usable size) of the overflow fixtures

// chainOf writes data as an overflow chain on pages (in order) of src: each
// page holds the next page number and up to ovPage-4 bytes.
func chainOf(src *sqlitefile.FakeSource, data []byte, pages ...uint32) {
	for i, pg := range pages {
		p := make([]byte, ovPage)
		if i+1 < len(pages) {
			copy(p, []byte{byte(pages[i+1] >> 24), byte(pages[i+1] >> 16), byte(pages[i+1] >> 8), byte(pages[i+1])})
		}
		lo := min(i*(ovPage-4), len(data))
		hi := min((i+1)*(ovPage-4), len(data))
		copy(p[4:], data[lo:hi])
		src.Pages[pg] = p
	}
}

func payloadData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + i/251)
	}
	return b
}

func cellFor(data []byte, local int, head uint32) sqlitefile.Cell {
	return sqlitefile.Cell{LocalBytes: data[:local], PayloadLen: int64(len(data)), OverflowHead: head}
}

func newPayloadFor(t *testing.T, env *sqlitefile.TestEnv, src *sqlitefile.FakeSource, vis sqlitefile.Visited, c sqlitefile.Cell) (*sqlitefile.TestPayload, *sqlitefile.TestLedger) {
	t.Helper()
	l := env.Ledger()
	return env.NewTestPayload(src, l, vis, ovPage, c), l
}

func TestOverflowChainFollowed(t *testing.T) {
	const local = 40
	data := payloadData(local + 3*(ovPage-4) - 100) // a 3-page chain whose last page is partly used
	pages := []uint32{7, 3, 12}
	src := sqlitefile.NewFakeSource()
	chainOf(src, data[local:], pages...)
	for i, pg := range pages {
		src.Locs[pg] = sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: int64(1000 + i), Frame: uint32(10 + i)}
	}
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	l := env.Ledger()
	vis, err := env.NewMapVisited(l, 16)
	if err != nil {
		t.Fatal(err)
	}
	p := env.NewTestPayload(src, l, vis, ovPage, cellFor(data, local, pages[0]))
	read := func(off int64, n int) []byte {
		t.Helper()
		buf := make([]byte, n)
		got, err := p.ReadAt(buf, off)
		if err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", off, n, err)
		}
		return buf[:got]
	}
	edge := int64(local + ovPage - 4) // first byte of the second overflow page
	for _, c := range []struct {
		off int64
		n   int
	}{
		{0, 10},               // local only
		{30, 20},              // local into the first page
		{local, ovPage - 4},   // exactly the first overflow page
		{local - 1, 1},        // last local byte
		{local, 1},            // first overflow byte
		{edge - 1, 2},         // straddles pages 1 and 2
		{edge, 1},             // first byte of page 2
		{edge, ovPage - 4},    // exactly page 2
		{edge + 100, 2 * 508}, // a value straddling pages 2 and 3, and running to the end
		{int64(len(data)) - 1, 1},
		{0, len(data)}, // everything at once
		{5, len(data) - 5},
	} {
		want := data[c.off:min(int(c.off)+c.n, len(data))]
		if got := read(c.off, c.n); !bytes.Equal(got, want) {
			t.Errorf("ReadAt(%d, %d): %d bytes, differs from the payload (want %d bytes)", c.off, c.n, len(got), len(want))
		}
	}
	// Going back after going forward re-reads a recorded page.
	if got := read(local, 5); !bytes.Equal(got, data[local:local+5]) {
		t.Error("re-reading the first overflow page after the last gave other bytes")
	}
	// Past the end: short, and no error.
	if got := read(int64(len(data))-3, 10); !bytes.Equal(got, data[len(data)-3:]) {
		t.Error("a read across the end must return the bytes up to the end")
	}
	if got := read(int64(len(data)), 4); len(got) != 0 {
		t.Errorf("a read at the end returned %d bytes", len(got))
	}
	if got := read(-1, 4); len(got) != 0 {
		t.Errorf("a read at a negative offset returned %d bytes", len(got))
	}
	if _, _, damaged := p.Damaged(); damaged {
		t.Error("an intact chain is not damaged")
	}
	// Every page the source was asked for is in the recorded chain.
	for _, r := range src.Reads {
		if !slices.Contains(pages, r) {
			t.Errorf("the source was asked for page %d, which is not in the chain %v", r, pages)
		}
	}
	chain := p.Chain()
	if len(chain) != 3 {
		t.Fatalf("chain = %+v", chain)
	}
	for i, s := range chain {
		if s.Page != pages[i] || s.At != src.Locs[pages[i]] {
			t.Errorf("step %d = %+v, want page %d at %+v", i, s, pages[i], src.Locs[pages[i]])
		}
	}
	p.Release()
}

func TestOverflowChainCycleTerminates(t *testing.T) {
	const local = 20
	data := payloadData(local + 5*(ovPage-4))
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	for _, c := range []struct {
		name  string
		pages func(src *sqlitefile.FakeSource) uint32
		good  int // pages of real data before the cycle closes
	}{
		{"a page that points to itself", func(src *sqlitefile.FakeSource) uint32 {
			chainOf(src, data[local:], 5)
			copy(src.Pages[5], []byte{0, 0, 0, 5})
			return 5
		}, 1},
		{"A -> B -> A", func(src *sqlitefile.FakeSource) uint32 {
			chainOf(src, data[local:], 5, 6)
			copy(src.Pages[6], []byte{0, 0, 0, 5})
			return 5
		}, 2},
		{"a longer loop that skips the head", func(src *sqlitefile.FakeSource) uint32 {
			chainOf(src, data[local:], 5, 6, 7, 8)
			copy(src.Pages[8], []byte{0, 0, 0, 6})
			return 5
		}, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := sqlitefile.NewFakeSource()
			head := c.pages(src)
			l := env.Ledger()
			vis, _ := env.NewMapVisited(l, 64)
			p := env.NewTestPayload(src, l, vis, ovPage, cellFor(data, local, head))
			buf := make([]byte, len(data))
			n, err := p.ReadAt(buf, 0)
			if err != nil {
				t.Fatal(err)
			}
			wantN := local + c.good*(ovPage-4)
			if n != wantN || !bytes.Equal(buf[:n], data[:n]) {
				t.Errorf("read %d bytes, want the %d bytes before the cycle", n, wantN)
			}
			why, _, damaged := p.Damaged()
			if !damaged || why == "" {
				t.Error("a cycle must be reported as damage")
			}
			if len(src.Reads) > c.good {
				t.Errorf("the source was read %d times for a chain of %d distinct pages: %v", len(src.Reads), c.good, src.Reads)
			}
			// Asking again does not walk the cycle again.
			before := len(src.Reads)
			if n2, _ := p.ReadAt(buf, int64(wantN)); n2 != 0 || len(src.Reads) != before {
				t.Errorf("a second read past the cycle: %d bytes, %d new page reads", n2, len(src.Reads)-before)
			}
		})
	}
}

func TestOverflowChainSharedPageDetected(t *testing.T) {
	const local = 10
	a := payloadData(local + 2*(ovPage-4))
	b := payloadData(local + 2*(ovPage-4))
	src := sqlitefile.NewFakeSource()
	chainOf(src, a[local:], 5, 6)
	chainOf(src, b[local:], 8, 6) // the second chain ends on A's second page
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	for _, mk := range []struct {
		name string
		vis  func(l *sqlitefile.TestLedger) (sqlitefile.Visited, error)
	}{
		{"bitset", func(l *sqlitefile.TestLedger) (sqlitefile.Visited, error) { return env.NewPageSetVisited(l, 20) }},
		{"map", func(l *sqlitefile.TestLedger) (sqlitefile.Visited, error) { return env.NewMapVisited(l, 20) }},
	} {
		t.Run(mk.name, func(t *testing.T) {
			l := env.Ledger()
			vis, err := mk.vis(l)
			if err != nil {
				t.Fatal(err)
			}
			pa := env.NewTestPayload(src, l, vis, ovPage, cellFor(a, local, 5))
			pb := env.NewTestPayload(src, l, vis, ovPage, cellFor(b, local, 8))
			bufA, bufB := make([]byte, len(a)), make([]byte, len(b))
			if n, err := pa.ReadAt(bufA, 0); err != nil || n != len(a) || !bytes.Equal(bufA, a) {
				t.Fatalf("first cell: %d bytes, %v", n, err)
			}
			n, err := pb.ReadAt(bufB, 0)
			if err != nil {
				t.Fatal(err)
			}
			if want := local + (ovPage - 4); n != want || !bytes.Equal(bufB[:n], b[:n]) {
				t.Errorf("second cell read %d bytes, want %d (its own page, not the shared one)", n, want)
			}
			if why, pg, damaged := pb.Damaged(); !damaged || pg != 6 {
				t.Errorf("the shared page 6 must be reported (%q, page %d, %v)", why, pg, damaged)
			}
			if _, _, damaged := pa.Damaged(); damaged {
				t.Error("the first cell owns its chain")
			}
		})
	}
}

func TestOverflowChainShortAndLong(t *testing.T) {
	const local = 30
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	t.Run("ends early", func(t *testing.T) {
		data := payloadData(local + 3*(ovPage-4))
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6) // two pages for a three-page payload; page 6 ends the chain
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		n, err := p.ReadAt(buf, 0)
		if err != nil {
			t.Fatal(err)
		}
		if want := local + 2*(ovPage-4); n != want || !bytes.Equal(buf[:n], data[:n]) {
			t.Errorf("read %d bytes, want the %d that exist", n, want)
		}
		if _, _, damaged := p.Damaged(); !damaged {
			t.Error("a chain that ends early is damage")
		}
		// A value wholly inside what exists is still readable.
		one := make([]byte, 10)
		if got, _ := p.ReadAt(one, local+100); got != 10 || !bytes.Equal(one, data[local+100:local+110]) {
			t.Error("a value wholly inside the chain must stay readable")
		}
	})
	t.Run("longer than needed", func(t *testing.T) {
		data := payloadData(local + (ovPage - 4) + 50) // needs two pages
		src := sqlitefile.NewFakeSource()
		chainOf(src, payloadData(10*(ovPage-4)), 5, 6, 7, 8, 9) // five pages are on disk
		chainOf(src, data[local:], 5, 6)
		copy(src.Pages[6], []byte{0, 0, 0, 7}) // page 6 points on to page 7
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 16), cellFor(data, local, 5))
		buf := make([]byte, len(data)+1000)
		n, err := p.ReadAt(buf, 0)
		if err != nil || n != len(data) || !bytes.Equal(buf[:n], data) {
			t.Fatalf("read %d bytes, %v", n, err)
		}
		if _, _, damaged := p.Damaged(); damaged {
			t.Error("the extra pages are ignored, not damage")
		}
		if slices.Contains(src.Reads, 7) {
			t.Error("a page past what the payload needs was read")
		}
	})
	t.Run("no pointer at all", func(t *testing.T) {
		data := payloadData(local + 100)
		p, _ := newPayloadFor(t, env, sqlitefile.NewFakeSource(), mustVisited(env, 4), cellFor(data, local, 0))
		buf := make([]byte, len(data))
		if n, _ := p.ReadAt(buf, 0); n != local {
			t.Errorf("read %d bytes, want the %d local ones", n, local)
		}
		if _, _, damaged := p.Damaged(); !damaged {
			t.Error("a missing overflow pointer is damage")
		}
	})
}

func mustVisited(env *sqlitefile.TestEnv, n int) sqlitefile.Visited {
	v, err := env.NewMapVisited(env.Ledger(), n)
	if err != nil {
		panic(err)
	}
	return v
}

func TestOverflowPageOutOfRange(t *testing.T) {
	const local = 30
	data := payloadData(local + 2*(ovPage-4))
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	t.Run("head not in the file", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 99))
		buf := make([]byte, len(data))
		n, err := p.ReadAt(buf, 0)
		if err != nil || n != local {
			t.Errorf("(%d, %v), want the local bytes only", n, err)
		}
		if _, pg, damaged := p.Damaged(); !damaged || pg != 99 {
			t.Errorf("page 99 must be named, got page %d, %v", pg, damaged)
		}
		if len(src.Reads) != 0 {
			t.Errorf("a page that does not exist was read: %v", src.Reads)
		}
	})
	t.Run("pointer inside the chain leaves the file", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		copy(src.Pages[5], []byte{0xff, 0xff, 0xff, 0xff})
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		if n, _ := p.ReadAt(buf, 0); n != local+ovPage-4 {
			t.Errorf("read %d bytes", n)
		}
		if _, pg, damaged := p.Damaged(); !damaged || pg != 0xffffffff {
			t.Errorf("(%d, %v)", pg, damaged)
		}
	})
	t.Run("page 0", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		copy(src.Pages[5], []byte{0, 0, 0, 0}) // ends the chain early
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		if n, _ := p.ReadAt(buf, 0); n != local+ovPage-4 {
			t.Errorf("read %d bytes", n)
		}
	})
	t.Run("a page the source cannot supply", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		src.Errs[6] = fmt.Errorf("beyond the file: %w", sqlitefile.ErrPageUnavailable)
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		n, err := p.ReadAt(buf, 0)
		if err != nil || n != local+ovPage-4 {
			t.Errorf("(%d, %v)", n, err)
		}
		if _, pg, damaged := p.Damaged(); !damaged || pg != 6 {
			t.Errorf("(%d, %v)", pg, damaged)
		}
	})
	t.Run("a page shorter than its next pointer", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		src.Pages[6] = src.Pages[6][:3]
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		if n, _ := p.ReadAt(buf, 0); n != local+ovPage-4 {
			t.Errorf("read %d bytes", n)
		}
		if _, _, damaged := p.Damaged(); !damaged {
			t.Error("damage expected")
		}
	})
	t.Run("a truncated last page serves only the bytes present", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		src.Pages[6] = src.Pages[6][:4+100] // the file ends 100 bytes into the page
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		n, err := p.ReadAt(buf, 0)
		if err != nil || n != local+ovPage-4+100 || !bytes.Equal(buf[:n], data[:n]) {
			t.Errorf("(%d, %v), want %d bytes equal to the payload and no zero padding", n, err, local+ovPage-4+100)
		}
		if _, _, damaged := p.Damaged(); !damaged {
			t.Error("missing bytes are damage")
		}
	})
	t.Run("an I/O error is returned as it is, never as damage", func(t *testing.T) {
		src := sqlitefile.NewFakeSource()
		chainOf(src, data[local:], 5, 6)
		boom := errors.New("disk on fire")
		src.Errs[5] = boom
		p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 5))
		buf := make([]byte, len(data))
		if _, err := p.ReadAt(buf, 0); !errors.Is(err, boom) || errors.Is(err, sqlitefile.ErrCorrupt) {
			t.Errorf("err = %v", err)
		}
		if _, _, damaged := p.Damaged(); damaged {
			t.Error("an I/O error is not a verdict on the chain")
		}
	})
}

func TestOverflowRecordsChainProvenance(t *testing.T) {
	const local = 10
	data := payloadData(local + 4*(ovPage-4))
	src := sqlitefile.NewFakeSource()
	chainOf(src, data[local:], 4, 9, 2, 6)
	locs := []sqlitefile.PageLoc{
		{File: sqlitefile.FileDB, Offset: 1536},
		{File: sqlitefile.FileWAL, Offset: 32 + 24, Frame: 1},
		{File: sqlitefile.FileJournal, Offset: 4096 + 4, Record: 3},
		{File: sqlitefile.FileWAL, Offset: 32 + 2*(24+ovPage) + 24, Frame: 3},
	}
	for i, pg := range []uint32{4, 9, 2, 6} {
		src.Locs[pg] = locs[i]
	}
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	p, _ := newPayloadFor(t, env, src, mustVisited(env, 8), cellFor(data, local, 4))
	// Reading only the first two pages' worth follows only those.
	buf := make([]byte, local+ovPage-4+10)
	if _, err := p.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if got := p.Chain(); len(got) != 2 || got[0].Page != 4 || got[0].At != locs[0] || got[1].Page != 9 || got[1].At != locs[1] {
		t.Errorf("chain after a partial read = %+v", got)
	}
	all := make([]byte, len(data))
	if _, err := p.ReadAt(all, 0); err != nil {
		t.Fatal(err)
	}
	got := p.Chain()
	if len(got) != 4 {
		t.Fatalf("chain = %+v", got)
	}
	for i, pg := range []uint32{4, 9, 2, 6} {
		if got[i].Page != pg || got[i].At != locs[i] {
			t.Errorf("step %d = %+v, want page %d at %+v", i, got[i], pg, locs[i])
		}
	}
	// The chain is a copy.
	got[0].Page = 777
	if p.Chain()[0].Page != 4 {
		t.Error("Chain() must return a copy")
	}
}

func TestPayloadBudgetCharged(t *testing.T) {
	// A record with a header, two small ints, a long text and a blob that
	// straddle the overflow chain.
	text := bytes.Repeat([]byte("ab"), 600)
	blob := payloadData(900)
	rb := mkRecord([]uint64{1, 1, uint64(13 + 2*len(text)), uint64(12 + 2*len(blob))}, []byte{3}, []byte{4}, text, blob)
	const local = 40
	pages := []uint32{3, 4, 5, 6, 7}
	build := func() *sqlitefile.FakeSource {
		src := sqlitefile.NewFakeSource()
		chainOf(src, rb[local:], pages...)
		return src
	}
	// Measure what a full read needs.
	run := func(budget *recBudget) (sqlitefile.Record, error) {
		var rec sqlitefile.Record
		env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})
		src := build()
		err := env.Call(func(l *sqlitefile.TestLedger) error {
			vis, err := env.NewMapVisited(l, 16)
			if err != nil {
				return err
			}
			p := env.NewTestPayload(src, l, vis, ovPage, cellFor(rb, local, pages[0]))
			var held int64
			rec, held, _, err = env.ReadRecord(l, p, sqlitefile.EncUTF8, nil)
			if err != nil {
				return err
			}
			// Copy what the caller keeps, then give everything back.
			rec.Values = slices.Clone(rec.Values)
			p.Release()
			vis.Release(l)
			l.Free(held)
			return nil
		})
		return rec, err
	}
	ok := newRecBudget(1 << 30)
	rec, err := run(ok)
	if err != nil {
		t.Fatal(err)
	}
	ok.check(t)
	if !bytes.Equal(rec.Values[2].Bytes, text) || !bytes.Equal(rec.Values[3].Bytes, blob) || rec.Values[0].Int != 3 {
		t.Error("values wrong")
	}
	if ok.peak < int64(len(text)+len(blob)) || ok.allocCalls == 0 {
		t.Errorf("the values (%d bytes) must be charged: peak %d, %d charges", len(text)+len(blob), ok.peak, ok.allocCalls)
	}
	// A budget that refuses at any threshold: ErrBudget, never a panic, and
	// nothing left charged after a refusal.
	for limit := int64(0); limit <= ok.peak; limit += max(1, ok.peak/200) {
		b := newRecBudget(limit)
		_, err := run(b)
		if limit < ok.peak && !errors.Is(err, sqlitefile.ErrBudget) {
			t.Fatalf("limit %d (peak needed %d): err = %v, want ErrBudget", limit, ok.peak, err)
		}
		b.check(t)
	}
	// A refusal comes with a *PanicError never.
	var pe *sqlitefile.PanicError
	if _, err := run(newRecBudget(10)); errors.As(err, &pe) {
		t.Errorf("a refused charge must not surface as a panic: %v", err)
	}
}

func TestLazyPayloadNeverAssemblesHugeCell(t *testing.T) {
	// A cell that declares 1<<30 bytes: a small int, a blob declared at half
	// a gibibyte, and another small int after it, with only a 3-page chain.
	huge := uint64(12 + 2*(1<<29))
	local := mkRecord([]uint64{1, huge, 1}, []byte{7}) // the first column is local
	const total = 1 << 30
	src := sqlitefile.NewFakeSource()
	chain := payloadData(3 * (ovPage - 4))
	chainOf(src, chain, 5, 6, 7)
	cell := sqlitefile.Cell{LocalBytes: local, PayloadLen: total, OverflowHead: 5}
	budget := newRecBudget(64 << 20)
	env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})

	t.Run("only the first column", func(t *testing.T) {
		src.Reads = nil
		err := env.Call(func(l *sqlitefile.TestLedger) error {
			vis, _ := env.NewMapVisited(l, 16)
			p := env.NewTestPayload(src, l, vis, ovPage, cell)
			rec, held, _, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, func(c int) bool { return c == 0 })
			if err != nil {
				return err
			}
			if rec.Values[0].Int != 7 || rec.Values[0].Omitted {
				t.Errorf("column 0 = %+v", rec.Values[0])
			}
			if !rec.Values[1].Omitted || rec.Values[1].Len != 1<<29 || !rec.Values[2].Omitted {
				t.Errorf("unrequested columns must be omitted with their true length: %+v %+v", rec.Values[1], rec.Values[2])
			}
			vis.Release(l)
			p.Release()
			l.Free(held)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.Reads) != 0 {
			t.Errorf("the requested column is local, yet pages %v were read", src.Reads)
		}
	})
	t.Run("a column beyond the chain", func(t *testing.T) {
		src.Reads = nil
		err := env.Call(func(l *sqlitefile.TestLedger) error {
			vis, _ := env.NewMapVisited(l, 16)
			p := env.NewTestPayload(src, l, vis, ovPage, cell)
			rec, held, warns, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, func(c int) bool { return c == 0 || c == 2 })
			if err != nil {
				return err
			}
			// The 512 MiB blob is over the blob cap of nobody asked for; the last int lies at 1<<29+.. beyond the chain.
			if !rec.Truncated || !rec.Values[2].Omitted {
				t.Errorf("record = %+v", rec)
			}
			found := false
			for _, w := range warns {
				found = found || w.Code == sqlitefile.WarnCellOverflowChain
			}
			if !found {
				t.Errorf("a chain that ends early must raise %s: %v", sqlitefile.WarnCellOverflowChain, warns)
			}
			vis.Release(l)
			p.Release()
			l.Free(held)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.Reads) > 3 {
			t.Errorf("at most the 3 chain pages may be read, got %v", src.Reads)
		}
	})
	if budget.peak > 1<<20 {
		t.Errorf("budget peak %d: nothing proportional to the declared size may be allocated", budget.peak)
	}
	budget.check(t)

	t.Run("over MaxPayloadBytes", func(t *testing.T) {
		big := sqlitefile.Cell{LocalBytes: local, PayloadLen: total + 1, OverflowHead: 5}
		err := env.Call(func(l *sqlitefile.TestLedger) error {
			vis, _ := env.NewMapVisited(l, 16)
			_, _, _, err := env.ReadRecord(l, env.NewTestPayload(src, l, vis, ovPage, big), sqlitefile.EncUTF8, nil)
			return err
		})
		if !errors.Is(err, sqlitefile.ErrLimit) {
			t.Errorf("err = %v, want ErrLimit", err)
		}
		budget.check(t)
	})
}

// TestLazyRecordEqualsDecodeRecord: reading a record through an overflow
// chain gives exactly what DecodeRecord gives on the assembled payload, for
// every split between the local part and the chain.
func TestLazyRecordEqualsDecodeRecord(t *testing.T) {
	text := []byte("héllo wörld, this text is long enough to straddle a boundary ")
	text = bytes.Repeat(text, 20)
	rb := mkRecord([]uint64{0, 1, 6, 7, 8, 9, uint64(13 + 2*len(text)), 12, uint64(12 + 2*300)},
		[]byte{0xfe}, be(1<<40+5, 8), be(math.Float64bits(-6.25), 8), text, payloadData(300))
	want, err := sqlitefile.DecodeRecord(rb, sqlitefile.EncUTF8, sqlitefile.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	for _, local := range []int{len(rb), 1, 2, 12, 13, 14, 25, 40, 100, 507, 508, 509, 1000} {
		if local > len(rb) {
			continue
		}
		src := sqlitefile.NewFakeSource()
		var pages []uint32
		for i := 0; i < (len(rb)-local+ovPage-5)/(ovPage-4); i++ {
			pages = append(pages, uint32(10+3*i))
		}
		head := uint32(0)
		if len(pages) > 0 {
			head = pages[0]
			chainOf(src, rb[local:], pages...)
		}
		err := env.Call(func(l *sqlitefile.TestLedger) error {
			vis, _ := env.NewMapVisited(l, 64)
			p := env.NewTestPayload(src, l, vis, ovPage, cellFor(rb, local, head))
			got, held, warns, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, nil)
			if err != nil {
				return fmt.Errorf("local %d: %w", local, err)
			}
			if len(warns) != 0 || got.Truncated || got.HeaderLen != want.HeaderLen || got.BodyLen != want.BodyLen {
				t.Errorf("local %d: record %+v, warnings %v", local, got, warns)
			}
			for i := range want.Values {
				if !valuesEqual(got.Values[i], want.Values[i]) {
					t.Errorf("local %d column %d: %+v, want %+v", local, i, got.Values[i], want.Values[i])
				}
			}
			p.Release()
			l.Free(held)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func valuesEqual(a, b sqlitefile.Value) bool {
	return a.Kind == b.Kind && a.Int == b.Int && a.Float == b.Float && bytes.Equal(a.Bytes, b.Bytes) &&
		a.Len == b.Len && a.Omitted == b.Omitted && a.Clipped == b.Clipped && a.Enc == b.Enc && a.Serial == b.Serial
}

func TestLazyReadReportsCappedValues(t *testing.T) {
	text := bytes.Repeat([]byte("t"), 100)
	rb := mkRecord([]uint64{uint64(13 + 2*len(text)), uint64(13 + 2*len(text)), 1}, text, text, []byte{9})
	env := sqlitefile.NewTestEnv(sqlitefile.Options{Limits: sqlitefile.Limits{MaxTextBytes: 50}})
	err := env.Call(func(l *sqlitefile.TestLedger) error {
		vis, _ := env.NewMapVisited(l, 4)
		p := env.NewTestPayload(sqlitefile.NewFakeSource(), l, vis, ovPage, cellFor(rb, len(rb), 0))
		rec, held, warns, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, nil)
		if err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			if v := rec.Values[i]; !v.Omitted || v.Len != 100 || v.Bytes != nil {
				t.Errorf("column %d = %+v, want omitted with its true length", i, v)
			}
		}
		if rec.Values[2].Int != 9 {
			t.Errorf("a later integer is still read: %+v", rec.Values[2])
		}
		if len(warns) != 1 || warns[0].Code != sqlitefile.WarnCellTooLarge || warns[0].Page != 7 || warns[0].Offset != 99 {
			t.Errorf("warnings = %+v, want one %s for the cell", warns, sqlitefile.WarnCellTooLarge)
		}
		l.Free(held)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPayloadContextCancelStopsChainWalk(t *testing.T) {
	const local = 10
	data := payloadData(local + 3*(ovPage-4))
	src := sqlitefile.NewFakeSource()
	chainOf(src, data[local:], 5, 6, 7)
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	l := env.Ledger()
	vis, _ := env.NewMapVisited(l, 8)
	p := env.NewTestPayload(src, l, vis, ovPage, cellFor(data, local, 5))
	ctx, cancel := context.WithCancel(context.Background())
	p.SetContext(ctx)
	cancel()
	buf := make([]byte, len(data))
	if _, err := p.ReadAt(buf, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(src.Reads) != 0 {
		t.Errorf("pages read after cancellation: %v", src.Reads)
	}
}

// TestOverflowChainFromBuilder: payloads of cells written by the independent
// builder, read through the reader's page cache over a real file, equal what
// was inserted; the pages followed are exactly the ones the builder chained.
func TestOverflowChainFromBuilder(t *testing.T) {
	for _, ps := range []int{512, 1024, 4096} {
		t.Run(fmt.Sprint(ps), func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
			tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
			vals := map[int64][2]any{}
			for i := int64(1); i <= 12; i++ {
				v := [2]any{longText(int(i)*ps/3, byte(i)), longBlob(int(i)*ps/2, byte(i))}
				vals[i] = v
				tb.Insert(i, v[0], v[1])
			}
			file := b.Bytes()
			db, err := sqlitefile.Open(bytes.NewReader(file), int64(len(file)), sqlitefile.Options{})
			if err != nil {
				t.Fatal(err)
			}
			env := sqlitefile.NewTestEnv(sqlitefile.Options{})
			for rowid, v := range vals {
				cellBytes, pg, off := tb.CellBytes(rowid)
				page, err := db.RawPage(pg)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(page[off:off+len(cellBytes)], cellBytes) {
					t.Fatalf("row %d: the builder's cell is not where it says", rowid)
				}
				h, err := sqlitefile.ParsePageHeader(page, pg)
				if err != nil {
					t.Fatal(err)
				}
				ptrs, err := sqlitefile.CellPointers(page, h, db.Info().UsableSize)
				if err != nil || !slices.ContainsFunc(ptrs.Good, func(p sqlitefile.CellPointer) bool { return p.Offset == off }) {
					t.Fatalf("row %d: pointers %v do not include %d (%v)", rowid, ptrs.Good, off, err)
				}
				cell, err := sqlitefile.ParseCell(page, db.Info().UsableSize, h, off)
				if err != nil {
					t.Fatal(err)
				}
				if cell.Rowid != rowid || cell.Length != len(cellBytes) {
					t.Fatalf("row %d: cell %+v, builder cell is %d bytes", rowid, cell, len(cellBytes))
				}
				err = env.Call(func(l *sqlitefile.TestLedger) error {
					p, err := db.PayloadOf(l, cell)
					if err != nil {
						return err
					}
					rec, held, _, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, nil)
					if err != nil {
						return err
					}
					if s, ok := rec.Values[0].Text(); !ok || s != v[0].(string) || !bytes.Equal(rec.Values[1].Bytes, v[1].([]byte)) {
						t.Errorf("row %d: values differ from what the builder was given", rowid)
					}
					var followed []uint32
					for _, s := range p.Chain() {
						followed = append(followed, s.Page)
						if s.At.File != sqlitefile.FileDB || s.At.Offset != sqlitefile.PageOffset(ps, s.Page) {
							t.Errorf("row %d: step %+v has the wrong location", rowid, s)
						}
					}
					if want := tb.Overflow(rowid); !slices.Equal(followed, want) {
						t.Errorf("row %d: followed %v, the builder chained %v", rowid, followed, want)
					}
					p.Release()
					l.Free(held)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// sqlitetestNew builds a three-table database of the given page size.
func sqlitetestNew(pageSize int) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: pageSize})
	for _, n := range []string{"a", "b"} {
		b.CreateTable(n, "CREATE TABLE "+n+"(x)").Insert(1, n)
	}
	return b
}

func TestPayloadWithAbsurdUsableSizeDoesNotPanic(t *testing.T) {
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	for _, u := range []int{-5, 0, 4, 5} {
		src := sqlitefile.NewFakeSource()
		chainOf(src, payloadData(100), 3)
		l := env.Ledger()
		vis, _ := env.NewMapVisited(l, 4)
		data := payloadData(50)
		p := env.NewTestPayload(src, l, vis, u, cellFor(data, 10, 3))
		buf := make([]byte, 50)
		if n, err := p.ReadAt(buf, 0); err != nil || n < 10 || n > 50 {
			t.Errorf("usable %d: (%d, %v)", u, n, err)
		}
	}
}
