package sqlitefile_test

// Hostile declared sizes (plan 3I, Task 15): a number read from a file never
// drives an allocation or a loop. Every case runs the whole public surface on a
// file that is small and claims something enormous; the budget refuses
// anything past 64 MiB (and the test asserts it was never even asked), a
// counting reader and the Stats counters show the work stays proportional to
// the real bytes, and the time budget is generous. No MemStats measurement is
// used: the recording budget is the measure, and every library allocation of
// size is charged to it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

type countReader struct {
	r     *bytes.Reader
	bytes atomic.Int64
	calls atomic.Int64
}

func newCountReader(b []byte) *countReader { return &countReader{r: bytes.NewReader(b)} }

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.bytes.Add(int64(n))
	c.calls.Add(1)
	return n, err
}

type hostileFiles struct{ db, wal, journal []byte }

func (f hostileFiles) total() int { return len(f.db) + len(f.wal) + len(f.journal) }

// hostileResult is what exercise saw.
type hostileResult struct {
	warnings  int
	errs      []error
	stats     sqlitefile.Stats
	bytesRead int64
	reads     int64
	rows      int
	pages     int
	elapsed   time.Duration
	opened    bool
}

func noPanic(t *testing.T, what string, err error) {
	t.Helper()
	var pe *sqlitefile.PanicError
	if errors.As(err, &pe) {
		t.Fatalf("%s: panic: %v\n%s", what, pe.Value, pe.Stack)
	}
}

// exercise runs the public surface over the files, in the order a caller would.
func exercise(t *testing.T, f hostileFiles, rb *recBudget) hostileResult {
	t.Helper()
	var res hostileResult
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := sqlitefile.Options{Budget: rb}
	note := func(what string, err error) {
		t.Helper()
		noPanic(t, what, err)
		if err != nil {
			res.errs = append(res.errs, err)
		}
	}
	dbr := newCountReader(f.db)
	d, err := sqlitefile.Open(dbr, int64(len(f.db)), opts)
	note("Open", err)
	if err != nil {
		res.elapsed = time.Since(start)
		return res
	}
	res.opened = true
	var extra []*countReader
	if f.wal != nil {
		r := newCountReader(f.wal)
		extra = append(extra, r)
		_, err := d.AttachWAL(r, int64(len(f.wal)))
		note("AttachWAL", err)
	}
	if f.journal != nil {
		r := newCountReader(f.journal)
		extra = append(extra, r)
		_, err := d.AttachJournal(r, int64(len(f.journal)))
		note("AttachJournal", err)
	}
	views := map[string]*sqlitefile.View{"live": d.Live(), "as-found": d.AsFound()}
	for name, v := range views {
		s, err := v.Schema(ctx)
		note(name+" Schema", err)
		_, err = v.Layout(ctx)
		note(name+" Layout", err)
		_, err = v.Freelist(ctx)
		note(name+" Freelist", err)
		if s != nil {
			for _, o := range s.Objects {
				if o.Type != "table" || o.Virtual {
					continue
				}
				tb, err := v.Table(ctx, o.Name)
				note(name+" Table", err)
				if err != nil {
					continue
				}
				n := 0
				note(name+" Rows", tb.Rows(ctx, func(sqlitefile.Row) bool { n++; return n < 5000 }))
				res.rows += n
				for id := int64(1); id <= 3; id++ {
					_, _, err := tb.Get(ctx, id)
					note(name+" Get", err)
				}
			}
		}
	}
	h := d.History()
	n := 0
	note("Pages", h.Pages(ctx, func(sqlitefile.PageImage) bool { n++; return n < 5000 }))
	res.pages = n
	_, err = h.Summary(ctx)
	note("Summary", err)
	nr := 0
	_, err = h.Rows(ctx, func(sqlitefile.RecoveredRow) bool { nr++; return nr < 5000 })
	note("History Rows", err)
	res.rows += nr
	res.warnings = len(d.Warnings()) + len(h.Warnings())
	for _, v := range views {
		res.warnings += len(v.Warnings())
		st := v.Stats()
		res.stats.PageReads += st.PageReads
		res.stats.CacheHits += st.CacheHits
		res.stats.CellsParsed += st.CellsParsed
		res.stats.PagesSkipped += st.PagesSkipped
	}
	if s := d.WAL(); s != nil {
		res.warnings += len(s.Warnings)
	}
	if s := d.Journal(); s != nil {
		res.warnings += len(s.Warnings)
	}
	h.Release()
	for _, v := range views {
		v.Release()
	}
	res.bytesRead = dbr.bytes.Load()
	res.reads = dbr.calls.Load()
	for _, r := range extra {
		res.bytesRead += r.bytes.Load()
		res.reads += r.calls.Load()
	}
	res.elapsed = time.Since(start)
	return res
}

// ---- hand-built hostile pages ----

// vint is the SQLite varint encoding of v.
func vint(v uint64) []byte {
	if v > 0x00ffffffffffffff {
		out := []byte{byte(v)}
		v >>= 8
		for i := 0; i < 8; i++ {
			out = append([]byte{byte(v&0x7f) | 0x80}, out...)
			v >>= 7
		}
		return out
	}
	out := []byte{byte(v & 0x7f)}
	v >>= 7
	for v > 0 {
		out = append([]byte{byte(v&0x7f) | 0x80}, out...)
		v >>= 7
	}
	return out
}

// tableLeafLocal is the local payload size of a table leaf cell.
func tableLeafLocal(ps int, p uint64) int {
	x := ps - 35
	m := (ps-12)*32/255 - 23
	if p <= uint64(x) {
		return int(p)
	}
	k := m + int((p-uint64(m))%uint64(ps-4))
	if k <= x {
		return k
	}
	return m
}

// craftedLeaf returns a table leaf page (not page 1) holding one cell: the
// payload size claims p, record is the start of the payload (zero padded to the
// local size), ovf the first overflow page (0: none).
func craftedLeaf(ps int, p uint64, record []byte, ovf uint32) []byte {
	local := tableLeafLocal(ps, p)
	cell := append(vint(p), 1) // rowid 1
	body := make([]byte, local)
	copy(body, record)
	cell = append(cell, body...)
	if uint64(local) < p {
		var o [4]byte
		binary.BigEndian.PutUint32(o[:], ovf)
		cell = append(cell, o[:]...)
	}
	pg := make([]byte, ps)
	pg[0] = 0x0d
	binary.BigEndian.PutUint16(pg[3:], 1)
	start := ps - len(cell)
	binary.BigEndian.PutUint16(pg[5:], uint16(start))
	binary.BigEndian.PutUint16(pg[8:], uint16(start))
	copy(pg[start:], cell)
	return pg
}

// hostileBase builds a database with the table t on page 2 and four rows.
func hostileBase(ps int) (*sqlitetest.Builder, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 4; id++ {
		tb.Insert(id, id, "row")
	}
	return b, tb
}

// withCrafted keeps pages 1 and 2 of base with page 2 replaced by leaf, appends
// pages (they are pages 3, 4, ...), and sets the header page count.
func withCrafted(base []byte, ps int, leaf []byte, more ...[]byte) []byte {
	data := bytes.Clone(base[:2*ps])
	copy(data[ps:], leaf)
	for _, m := range more {
		data = append(data, m...)
	}
	return withCount(data, uint32(len(data)/ps))
}

// chainPages returns n overflow pages numbered first, first+1, ...: each holds
// the next page number (0 at the end) and zero bytes.
func chainPages(ps, n int, first uint32) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		pg := make([]byte, ps)
		if i+1 < n {
			binary.BigEndian.PutUint32(pg, first+uint32(i)+1)
		}
		out = append(out, pg)
	}
	return out
}

func TestHostileDeclaredSizesNeverAllocate(t *testing.T) {
	const ps = 512
	type tcase struct {
		name  string
		files func(t *testing.T) hostileFiles
	}
	cases := []tcase{
		{"the header claims 2^32-1 pages on a 4 KiB file", func(t *testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			b.SetHeaderPages(0xffffffff, true)
			db := b.Bytes()
			if len(db) > 4096 {
				t.Fatalf("file of %d bytes", len(db))
			}
			return hostileFiles{db: db}
		}},
		{"a WAL commit frame claims 2^32-1 pages", func(*testing.T) hostileFiles {
			b, tb := hostileBase(ps)
			db := walMode(withCount(b.Bytes(), b.Snapshot().Pages()))
			tb.Update(2, int64(22), "new")
			w := b.NewWAL(false, 0x1000, 0x1001, 0)
			w.Frame(2, b.Snapshot().Page(2), 0xffffffff)
			return hostileFiles{db: db, wal: w.Bytes()}
		}},
		{"a journal's initial size is 2^32-1", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			img := b.Snapshot()
			db := withCount(b.Bytes(), img.Pages())
			j := b.NewJournal(512, 5, 0xffffffff)
			j.Record(2, img.Page(2))
			return hostileFiles{db: db, journal: j.Bytes()}
		}},
		{"a cell claims a payload of 2^31 with a short record", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			leaf := craftedLeaf(ps, 1<<31, []byte{2, 1, 7}, 0)
			return hostileFiles{db: withCrafted(b.Bytes(), ps, leaf)}
		}},
		{"a record header claims 30,000 columns", func(t *testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			const hdr = 30003 // 3-byte header-size varint + 30,000 NULL serial types
			if len(vint(hdr)) != 3 {
				t.Fatalf("header size varint is %d bytes", len(vint(hdr)))
			}
			rec := append(vint(hdr), make([]byte, 30000)...)
			local := tableLeafLocal(ps, uint64(len(rec)))
			n := (len(rec) - local + ps - 5) / (ps - 4)
			leaf := craftedLeaf(ps, uint64(len(rec)), rec, 3)
			return hostileFiles{db: withCrafted(b.Bytes(), ps, leaf, chainPages(ps, n, 3)...)}
		}},
		{"an overflow chain is declared for 4 GiB", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			leaf := craftedLeaf(ps, 1<<32, []byte{2, 1, 7}, 3)
			return hostileFiles{db: withCrafted(b.Bytes(), ps, leaf, chainPages(ps, 3, 3)...)}
		}},
		{"a WAL of 40 KiB whose header says page size 65536", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			big := sqlitetest.New(sqlitetest.Options{PageSize: 65536})
			wal := big.NewWAL(false, 1, 2, 0).Bytes()
			wal = append(wal, bytes.Repeat([]byte{0xaa}, 40*1024-len(wal))...)
			return hostileFiles{db: walMode(withCount(b.Bytes(), b.Snapshot().Pages())), wal: wal}
		}},
		{"a freelist trunk claims 2^32 leaves", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			b.SetFreelist([][]uint32{{3}})
			db := b.Bytes()
			binary.BigEndian.PutUint32(pageAt(db, ps, 3)[4:], 0xffffffff)
			return hostileFiles{db: db}
		}},
		{"a journal claims 2^31 records", func(*testing.T) hostileFiles {
			b, _ := hostileBase(ps)
			img := b.Snapshot()
			db := withCount(b.Bytes(), img.Pages())
			j := b.NewJournal(512, 5, img.Pages())
			j.Record(2, img.Page(2))
			j.Record(1, img.Page(1))
			j.SetHeader(1<<31, 5, img.Pages(), 512, ps)
			return hostileFiles{db: db, journal: j.Bytes()}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.files(t)
			rb := newRecBudget(64 << 20)
			res := exercise(t, f, rb)
			t.Logf("opened %v, %d warnings, %d errors %v, %d rows, %d pages, %v; budget peak %d, refused %d; read %d bytes in %d reads of %d file bytes; stats %+v",
				res.opened, res.warnings, len(res.errs), res.errs, res.rows, res.pages, res.elapsed, rb.peak, rb.refused, res.bytesRead, res.reads, f.total(), res.stats)
			if res.elapsed > 10*time.Second {
				t.Errorf("took %v", res.elapsed)
			}
			if rb.peak >= 64<<20 || rb.refused != 0 {
				t.Errorf("budget peak %d, refused %d times: a claimed size drove an allocation", rb.peak, rb.refused)
			}
			if rb.negative || rb.overFree {
				t.Errorf("budget misuse: negative %v over-freed %v", rb.negative, rb.overFree)
			}
			if res.warnings == 0 && len(res.errs) == 0 {
				t.Errorf("no warning and no error for a hostile file")
			}
			total := int64(f.total())
			pages := total/ps + 2
			if res.stats.PageReads > 4*pages || res.stats.CellsParsed > total || res.stats.PagesSkipped > 4*pages {
				t.Errorf("stats %+v are not bounded by the %d real bytes (%d pages)", res.stats, total, pages)
			}
			if res.bytesRead > 64*total+1<<20 || res.reads > 64*pages+1024 {
				t.Errorf("read %d bytes in %d calls from %d file bytes", res.bytesRead, res.reads, total)
			}
		})
	}
}
