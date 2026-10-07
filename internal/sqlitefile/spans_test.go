package sqlitefile_test

// Free spans of page images, secure-delete evidence, beyond-end pages, orphans
// and the history caps (plan 3I, Task 10).

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const sps = 512

// leaf512 is a hand-made table-leaf page of 512 bytes: three 12-byte cells at
// 480, 440 and 400 (content start 400, pointer array 8..14), and the free
// regions [412,440), [452,480) and [492,512) between and after them. set may
// write freeblocks, the first-freeblock field and the fragmented count.
func leaf512(set func(p []byte)) []byte {
	p := make([]byte, sps)
	p[0] = 0x0d
	binary.BigEndian.PutUint16(p[3:], 3)
	binary.BigEndian.PutUint16(p[5:], 400)
	for i, off := range []int{480, 440, 400} {
		binary.BigEndian.PutUint16(p[8+2*i:], uint16(off))
		p[off], p[off+1] = 10, byte(i+1) // payload length 10, rowid
		for k := 0; k < 10; k++ {
			p[off+2+k] = byte(0xA0 + i)
		}
	}
	if set != nil {
		set(p)
	}
	return p
}

func fb(p []byte, off, next, size int) {
	binary.BigEndian.PutUint16(p[off:], uint16(next))
	binary.BigEndian.PutUint16(p[off+2:], uint16(size))
}

func setFirst(p []byte, off int) { binary.BigEndian.PutUint16(p[1:], uint16(off)) }

// pagesDB returns a database whose pages 3.. are the given 512-byte pages: they
// lie inside the header count and no structure claims them, so they are orphans.
func pagesDB(t testing.TB, pages ...[]byte) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: sps})
	b.CreateTable("t", "create table t(a)")
	db := b.Bytes()
	if len(db) != 2*sps {
		t.Fatalf("%d bytes", len(db))
	}
	for _, p := range pages {
		db = append(db, p...)
	}
	binary.BigEndian.PutUint32(db[28:], uint32(len(db)/sps))
	copy(db[92:96], db[24:28])
	return db
}

func historyOf(t testing.TB, db []byte, o sqlitefile.Options) (*sqlitefile.DB, *sqlitefile.Hist) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), o)
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	return d, h
}

func imageOf(t testing.TB, h *sqlitefile.Hist, origin sqlitefile.Origin, pg uint32) sqlitefile.PageImage {
	t.Helper()
	for _, p := range collectPages(t, h) {
		if p.Origin == origin && p.Number == pg {
			return p
		}
	}
	t.Fatalf("no image of page %d with origin %d", pg, origin)
	return sqlitefile.PageImage{}
}

func spanList(s []sqlitefile.Span) [][3]int {
	var out [][3]int
	for _, x := range s {
		out = append(out, [3]int{int(x.Kind), x.Offset, x.Length})
	}
	return out
}

// TestPageSpansGapAndFreeblocks: the gap between the pointer array and the
// content area and every freeblock are spans (in this order), the fragmented
// bytes are counted, and the cells are parsed.
func TestPageSpansGapAndFreeblocks(t *testing.T) {
	good := leaf512(func(p []byte) {
		setFirst(p, 412)
		fb(p, 412, 452, 28)
		fb(p, 452, 492, 28)
		fb(p, 492, 0, 20)
		p[7] = 3
	})
	_, h := historyOf(t, pagesDB(t, good), sqlitefile.Options{})
	img := imageOf(t, h, sqlitefile.OriginOrphan, 3)
	pp, err := img.Parse()
	if err != nil {
		t.Fatal(err)
	}
	want := [][3]int{
		{int(sqlitefile.SpanGap), 14, 386},
		{int(sqlitefile.SpanFreeblock), 412, 28},
		{int(sqlitefile.SpanFreeblock), 452, 28},
		{int(sqlitefile.SpanFreeblock), 492, 20},
	}
	if got := spanList(pp.Spans); !slices.Equal(got, want) {
		t.Errorf("spans %v, want %v", got, want)
	}
	if pp.FragmentedBytes != 3 || pp.Rejected != 0 || pp.Header.CellCount != 3 {
		t.Errorf("fragmented %d rejected %d header %+v", pp.FragmentedBytes, pp.Rejected, pp.Header)
	}
	if len(pp.Cells) != 3 || pp.Cells[0].Offset != 480 || pp.Cells[1].Offset != 440 || pp.Cells[2].Offset != 400 || pp.Cells[2].Rowid != 3 || pp.Cells[0].Index != 0 || pp.Cells[2].Index != 2 {
		t.Errorf("cells %+v", pp.Cells)
	}
	for _, s := range pp.Spans {
		if s.FileOffset != img.Loc.Offset+int64(s.Offset) {
			t.Errorf("span %+v: file offset is not the page's %d plus its offset", s, img.Loc.Offset)
		}
	}
	// the image carries the same spans as Parse
	if got := spanList(img.Spans); !slices.Equal(got, want) {
		t.Errorf("image spans %v, want %v", got, want)
	}
	if w := h.Warnings(); hasCode(w, sqlitefile.WarnFreeblockChain) {
		t.Errorf("a sound chain warned: %v", w)
	}
}

func hasCode(w []sqlitefile.Warning, code string) bool {
	return slices.ContainsFunc(w, func(x sqlitefile.Warning) bool { return x.Code == code })
}

// TestParseRejectsBadCellPointers: a pointer into the page header is rejected and
// counted; the other cells are still parsed.
func TestParseRejectsBadCellPointers(t *testing.T) {
	p := leaf512(func(p []byte) { binary.BigEndian.PutUint16(p[10:], 3) })
	_, h := historyOf(t, pagesDB(t, p), sqlitefile.Options{})
	pp, err := imageOf(t, h, sqlitefile.OriginOrphan, 3).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if pp.Rejected != 1 || len(pp.Cells) != 2 {
		t.Errorf("rejected %d cells %d", pp.Rejected, len(pp.Cells))
	}
	// a page that is not a b-tree page is an error, not a parse
	_, h2 := historyOf(t, pagesDB(t, make([]byte, sps)), sqlitefile.Options{})
	if _, err := imageOf(t, h2, sqlitefile.OriginOrphan, 3).Parse(); err == nil {
		t.Error("a zero page parsed as a b-tree page")
	}
}

// TestFreeblockChainHostile: a cycle, descending offsets, an offset outside the
// page or inside the header, sizes 0 and 3, a block past the page end and an
// overlap end the chain at the first violation: bounded, counted in Rejected,
// reported as freeblock-chain, and the spans before the violation are kept.
func TestFreeblockChainHostile(t *testing.T) {
	good := func(p []byte) { fb(p, 412, 452, 28) }
	for _, c := range []struct {
		name string
		set  func(p []byte)
		keep int // freeblock spans kept
	}{
		{"cycle", func(p []byte) { setFirst(p, 412); good(p); fb(p, 452, 412, 28) }, 2},
		{"self loop", func(p []byte) { setFirst(p, 412); fb(p, 412, 412, 28) }, 1},
		{"descending", func(p []byte) { setFirst(p, 452); fb(p, 452, 412, 28); fb(p, 412, 0, 28) }, 1},
		{"first offset outside the page", func(p []byte) { setFirst(p, 600) }, 0},
		{"next offset outside the page", func(p []byte) { setFirst(p, 412); fb(p, 412, 600, 28) }, 1},
		{"first offset inside the header", func(p []byte) { setFirst(p, 4) }, 0},
		{"first offset at the last bytes", func(p []byte) { setFirst(p, 510) }, 0},
		{"size 0", func(p []byte) { setFirst(p, 412); fb(p, 412, 0, 0) }, 0},
		{"size 3", func(p []byte) { setFirst(p, 412); fb(p, 412, 0, 3) }, 0},
		{"size past the page", func(p []byte) { setFirst(p, 412); fb(p, 412, 0, 200) }, 0},
		{"overlap", func(p []byte) { setFirst(p, 412); fb(p, 412, 420, 28); fb(p, 420, 0, 28) }, 1},
		{"inside the gap", func(p []byte) { setFirst(p, 100); fb(p, 100, 0, 28) }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := historyOf(t, pagesDB(t, leaf512(c.set)), sqlitefile.Options{})
			img := imageOf(t, h, sqlitefile.OriginOrphan, 3)
			pp, err := img.Parse()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, s := range pp.Spans {
				if s.Kind == sqlitefile.SpanFreeblock {
					n++
				}
			}
			if n != c.keep || pp.Rejected < 1 {
				t.Errorf("%d freeblock spans kept (want %d), rejected %d: %v", n, c.keep, pp.Rejected, spanList(pp.Spans))
			}
			if !hasCode(h.Warnings(), sqlitefile.WarnFreeblockChain) {
				t.Errorf("no freeblock-chain warning: %v", h.Warnings())
			}
		})
	}
}

// TestFreeblockChainDenseIsAccepted: the longest chain a page can hold (blocks of
// 4 bytes side by side, 124 on a 512-byte page, below the cap of pagesize/4) is
// followed whole and does not warn.
func TestFreeblockChainDenseIsAccepted(t *testing.T) {
	p := make([]byte, sps)
	p[0] = 0x0d
	binary.BigEndian.PutUint16(p[5:], 16)
	setFirst(p, 16)
	n := 0
	for off := 16; off+4 <= sps; off += 4 {
		next := off + 4
		if next+4 > sps {
			next = 0
		}
		fb(p, off, next, 4)
		n++
	}
	_, h := historyOf(t, pagesDB(t, p), sqlitefile.Options{})
	pp, err := imageOf(t, h, sqlitefile.OriginOrphan, 3).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if len(pp.Spans) != n+1 || pp.Rejected != 0 || hasCode(h.Warnings(), sqlitefile.WarnFreeblockChain) {
		t.Errorf("%d spans (want %d), rejected %d, warnings %v", len(pp.Spans), n+1, pp.Rejected, h.Warnings())
	}
}

// TestTrunkTailSpan: a freelist trunk's span is the bytes after its leaf list,
// 8 + 4L to the end of the usable area.
func TestTrunkTailSpan(t *testing.T) {
	for _, reserved := range []int{0, 24} {
		t.Run(fmt.Sprintf("reserved %d", reserved), func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: 1024, Reserved: reserved})
			b.CreateTable("t", "create table t(a)")
			b.SetFreelist([][]uint32{{3, 4, 5}})
			db := b.Bytes()
			_, h := historyOf(t, db, sqlitefile.Options{})
			img := imageOf(t, h, sqlitefile.OriginFreelistTrunk, 3)
			if len(img.Spans) != 1 {
				t.Fatalf("spans %v", img.Spans)
			}
			s := img.Spans[0]
			if s.Kind != sqlitefile.SpanTrunkTail || s.Offset != 16 || s.Length != 1024-reserved-16 || s.FileOffset != 2*1024+16 {
				t.Errorf("span %+v", s)
			}
			// the leaves are listed and have no spans
			for _, pg := range []uint32{4, 5} {
				if li := imageOf(t, h, sqlitefile.OriginFreelistLeaf, pg); len(li.Spans) != 0 {
					t.Errorf("leaf %d has spans %v", pg, li.Spans)
				}
			}
		})
	}
}

// TestSecureDeleteZeroedFreePagesCounted: all-zero freelist leaves are counted
// in FreePagesZeroed (secure_delete evidence), are listed, and are not taken for
// content; the trunk is never counted as zeroed.
func TestSecureDeleteZeroedFreePagesCounted(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(a)")
	b.SetFreelist([][]uint32{{3, 4, 5, 6, 7, 8}})
	db := b.Bytes()
	for _, pg := range []int{6, 7, 8} { // 4 and 5 stay zero
		copy(db[(pg-1)*1024:], patPage(byte(pg)))
	}
	_, h := historyOf(t, db, sqlitefile.Options{})
	sum, err := h.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.FreePages != 6 || sum.FreePagesZeroed != 2 {
		t.Errorf("free %d zeroed %d, want 6 and 2", sum.FreePages, sum.FreePagesZeroed)
	}
	for _, pg := range []uint32{4, 5} {
		img := imageOf(t, h, sqlitefile.OriginFreelistLeaf, pg)
		got, err := img.Bytes()
		if err != nil || !bytes.Equal(got, make([]byte, 1024)) {
			t.Errorf("page %d: %v", pg, err)
		}
		if _, err := img.Parse(); err == nil || len(img.Spans) != 0 {
			t.Errorf("page %d: a zero page was taken for content (err %v, spans %v)", pg, err, img.Spans)
		}
	}
}

// TestBeyondEndPages: a file longer than the header's page count, with a valid
// counter, lists the whole pages past the count as OriginBeyondEnd; with an
// invalid counter the file size is the count and nothing lies beyond it.
func TestBeyondEndPages(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(a)")
	db := b.Bytes()
	for i := 0; i < 3; i++ {
		db = append(db, patPage(byte(0x40+i))...)
	}
	trusted := withCount(db, 2)
	_, h := historyOf(t, trusted, sqlitefile.Options{})
	var got []uint32
	for _, p := range collectPages(t, h) {
		if p.Origin == sqlitefile.OriginBeyondEnd {
			got = append(got, p.Number)
			b, err := p.Bytes()
			if err != nil || !bytes.Equal(b, trusted[(p.Number-1)*1024:p.Number*1024]) {
				t.Errorf("page %d: %v", p.Number, err)
			}
		}
	}
	if !slices.Equal(got, []uint32{3, 4, 5}) {
		t.Errorf("beyond-end pages %v", got)
	}
	untrusted := bytes.Clone(trusted)
	untrusted[92]++ // version-valid-for no longer equals the change counter
	_, h2 := historyOf(t, untrusted, sqlitefile.Options{})
	for _, p := range collectPages(t, h2) {
		if p.Origin == sqlitefile.OriginBeyondEnd {
			t.Errorf("beyond-end page %d with an untrusted counter", p.Number)
		}
	}
}

// TestOrphanPagesListed: the pages Layout calls orphans are listed, in page
// order, with their raw bytes.
func TestOrphanPagesListed(t *testing.T) {
	data, pages := orphanDB(t, 5)
	d, h := historyOf(t, data, sqlitefile.Options{})
	v := d.Live()
	defer v.Release()
	lay, err := v.Layout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, p := range collectPages(t, h) {
		if p.Origin != sqlitefile.OriginOrphan {
			continue
		}
		got = append(got, p.Number)
		b, err := p.Bytes()
		if err != nil || !bytes.Equal(b, data[(p.Number-1)*512:p.Number*512]) {
			t.Errorf("page %d: %v", p.Number, err)
		}
	}
	if !slices.Equal(got, lay.Orphans) || len(got) < 5 {
		t.Errorf("orphans %v, Layout says %v", got, lay.Orphans)
	}
	for _, pg := range pages {
		if !slices.Contains(got, pg) {
			t.Errorf("page %d is missing", pg)
		}
	}
}

// TestHistoryCaps: MaxHistoryPages cuts the sequence (a limit-reached warning,
// no error), and a visitor that returns false stops it at once.
func TestHistoryCaps(t *testing.T) {
	m := newHistMaster(t)
	full := collectPages(t, m.open(t, sqlitefile.Options{}, true).History())
	d := m.open(t, sqlitefile.Options{Limits: sqlitefile.Limits{MaxHistoryPages: 5}}, true)
	h := d.History()
	t.Cleanup(h.Release)
	got := collectPages(t, h)
	if len(got) != 5 {
		t.Fatalf("%d images with MaxHistoryPages 5", len(got))
	}
	for i := range got {
		if describe(got[i]) != describe(full[i]) {
			t.Errorf("image %d differs from the uncapped sequence", i)
		}
	}
	if !hasCode(h.Warnings(), sqlitefile.WarnLimitReached) {
		t.Errorf("no limit-reached warning: %v", h.Warnings())
	}
	sum, err := h.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, c := range sum.PagesByOrigin {
		n += c
	}
	if n != 5 {
		t.Errorf("summary counts %d images under the cap", n)
	}
	// an early stop
	h2 := m.open(t, sqlitefile.Options{}, true).History()
	t.Cleanup(h2.Release)
	seen := 0
	if err := h2.Pages(context.Background(), func(sqlitefile.PageImage) bool { seen++; return seen < 3 }); err != nil || seen != 3 {
		t.Errorf("early stop: %d visits, %v", seen, err)
	}
	// a cancelled context is returned
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h2.Pages(ctx, func(sqlitefile.PageImage) bool { return true }); err == nil {
		t.Error("a cancelled context was not reported")
	}
}

// TestPageSpansAgainstEngine: the engine deletes and updates rows through its
// driver; for every b-tree page, read through dbstat, the span lengths plus the
// fragmented bytes equal dbstat's unused bytes. The oracle is dbstat.
func TestPageSpansAgainstEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.db")
	e := openEngine(t, path)
	for _, q := range []string{
		"pragma page_size=1024",
		"create table t(id integer primary key, a text, b blob)",
		"create index ta on t(a)",
	} {
		if _, err := e.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	for i := 1; i <= 400; i++ {
		if _, err := e.Exec("insert into t values(?,?,?)", i, fmt.Sprintf("name-%d-%s", i, longText(i%40+5, byte(i))), bytes.Repeat([]byte{byte(i)}, i%50)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"delete from t where id % 3 = 0",
		"update t set a = substr(a, 1, 8) where id % 7 = 0",
		"update t set b = zeroblob(80) where id % 11 = 0",
		"delete from t where id between 100 and 130",
	} {
		if _, err := e.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	unused := map[uint32]int{}
	rows, err := e.Query("select pageno, pagetype, unused from dbstat where pagetype in ('leaf','internal')")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var pg uint32
		var typ string
		var u int
		if err := rows.Scan(&pg, &typ, &u); err != nil {
			t.Fatal(err)
		}
		unused[pg] = u
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	dir := t.TempDir()
	copyFiles(t, dir, path)
	data, err := os.ReadFile(filepath.Join(dir, "spans.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, h := historyOf(t, data, sqlitefile.Options{})
	checked, withFree := 0, 0
	for _, p := range collectPages(t, h) {
		if p.Origin != sqlitefile.OriginLiveBTree {
			continue
		}
		want, ok := unused[p.Number]
		if !ok {
			t.Errorf("page %d is a live b-tree page but dbstat does not list it", p.Number)
			continue
		}
		pp, err := p.Parse()
		if err != nil {
			t.Fatalf("page %d: %v", p.Number, err)
		}
		sum := pp.FragmentedBytes
		for _, s := range pp.Spans {
			sum += s.Length
		}
		if sum != want || pp.Rejected != 0 {
			t.Errorf("page %d: spans %d + fragmented %d = %d, dbstat unused %d (rejected %d)", p.Number, sum-pp.FragmentedBytes, pp.FragmentedBytes, sum, want, pp.Rejected)
		}
		if got := spanList(p.Spans); !slices.Equal(got, spanList(pp.Spans)) {
			t.Errorf("page %d: image spans differ from Parse", p.Number)
		}
		checked++
		if len(pp.Spans) > 1 || pp.FragmentedBytes > 0 {
			withFree++
		}
		delete(unused, p.Number)
	}
	if len(unused) != 0 || checked < 10 || withFree < 3 {
		t.Errorf("dbstat pages not listed %v; checked %d, with freeblocks or fragments %d", unused, checked, withFree)
	}
}
