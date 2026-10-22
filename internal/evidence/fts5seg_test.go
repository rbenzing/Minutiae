package evidence

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// ftsDecoded is one decoded position, as fts5vocab 'instance' lists it.
type ftsDecoded struct {
	term  string
	rowid int64
	col   int
	off   int64
}

type ftsPage struct {
	pgno  int64
	block []byte
}

// ftsStructureOf decodes the structure record of an FTS table.
func ftsStructureOf(t testing.TB, db *sql.DB, table string) []ftsSegment {
	t.Helper()
	var structure []byte
	if err := db.QueryRow("SELECT block FROM "+table+"_data WHERE id = ?", ftsStructureRowID).Scan(&structure); err != nil { //nolint:gosec // a constant table name in a test
		t.Fatal(err)
	}
	segs, err := decodeFTSStructureSegments(structure)
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return segs
}

// ftsLeafPages reads the leaf rows of a segment in page order (a local row id below 1<<31).
func ftsLeafPages(t testing.TB, db *sql.DB, table string, seg int64) []ftsPage {
	t.Helper()
	lo := seg << ftsSegidShift
	rows, err := db.Query("SELECT id, block FROM "+table+"_data WHERE id >= ? AND id < ? ORDER BY id", lo, lo+1<<31) //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []ftsPage
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, ftsPage{pgno: id - lo, block: b})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// decodeFTSTable runs the decoder over every segment of an FTS table.
func decodeFTSTable(t testing.TB, db *sql.DB, table string, sink ftsSink) error {
	t.Helper()
	for _, seg := range ftsStructureOf(t, db, table) {
		r := newFTSSegReader(seg)
		if seg.Empty() {
			// lingering rows of an emptied segment are not decoded (D143)
			if err := r.Finish(); err != nil {
				return err
			}
			continue
		}
		for _, p := range ftsLeafPages(t, db, table, seg.ID) {
			if err := r.Page(p.pgno, p.block, sink); err != nil {
				return err
			}
		}
		if err := r.Finish(); err != nil {
			return err
		}
	}
	return nil
}

func ensureVocab(t testing.TB, db *sql.DB, table string) {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = ?", ftsVocabName(table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		ddl, _ := FTSVocabDDL(table)
		probeExec(t, db, ddl)
	}
}

var ftsVocabCols = map[string]int{"summary": 0, "body": 1}

// vocabPostings is the multiset fts5vocab shows for a table.
func vocabPostings(t testing.TB, db *sql.DB, table string) map[ftsDecoded]int {
	t.Helper()
	ensureVocab(t, db, table)
	rows, err := db.Query("SELECT term, doc, col, offset FROM " + ftsVocabName(table)) //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[ftsDecoded]int{}
	for rows.Next() {
		var p ftsDecoded
		var col string
		if err := rows.Scan(&p.term, &p.rowid, &col, &p.off); err != nil {
			t.Fatal(err)
		}
		c, ok := ftsVocabCols[col]
		if !ok {
			t.Fatalf("vocab column %q", col)
		}
		p.col = c
		out[p]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requireDecodeEqualsVocab decodes every segment of the table and compares the multiset of positions
// with fts5vocab. It also requires no delete marker.
func requireDecodeEqualsVocab(t testing.TB, db *sql.DB, table string) {
	t.Helper()
	want := vocabPostings(t, db, table)
	got := map[ftsDecoded]int{}
	deletes := 0
	err := decodeFTSTable(t, db, table, ftsSink{
		Posting: func(term []byte, rowid int64, col int, off int64) { got[ftsDecoded{string(term), rowid, col, off}]++ },
		Delete:  func([]byte, int64) { deletes++ },
	})
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	if deletes != 0 {
		t.Fatalf("%s: %d delete markers in a genuine index", table, deletes)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: decoded %d distinct positions, fts5vocab shows %d", table, len(got), len(want))
	}
	for p, n := range want {
		if got[p] != n {
			t.Fatalf("%s: %+v decoded %d times, fts5vocab shows %d", table, p, got[p], n)
		}
	}
}

// longDoclistWorkload puts one term into 30000 documents, so its doclist spans far more than
// FTS5_MIN_DLIDX_SIZE (4) pages and gets a doclist index.
func longDoclistWorkload(t testing.TB, db *sql.DB) {
	t.Helper()
	for part := 0; part < 3; part++ {
		probeExec(t, db, "BEGIN")
		for i := part*10000 + 1; i <= (part+1)*10000; i++ {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, "common", fmt.Sprintf("common u%d", i%251))
			}
		}
		probeExec(t, db, "COMMIT")
	}
}

func hasDlidxRows(t testing.TB, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	// the doclist index bit is bit 36 of the row id (FTS5_DATA_DLI_B at FTS5_DATA_PAGE_B+FTS5_DATA_HEIGHT_B)
	if err := db.QueryRow("SELECT count(*) FROM "+table+"_data WHERE id >= ? AND (id >> 36) & 1 = 1", 1<<ftsSegidShift).Scan(&n); err != nil { //nolint:gosec // a constant table name in a test
		t.Fatal(err)
	}
	return n > 0
}

func TestFTS5DecodeEqualsVocabOnGenuineIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (> 60 s): runs in the merge gate")
	}
	check := func(t *testing.T, db *sql.DB) {
		for _, table := range FTSTables() {
			requireDecodeEqualsVocab(t, db, table)
		}
	}
	for name, load := range genuineFTSWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			check(t, db)
		})
	}
	t.Run("long doclist", func(t *testing.T) {
		db := genuineFTSWorkload(t, "long doclist", longDoclistWorkload)
		for _, table := range FTSTables() {
			if !hasDlidxRows(t, db, table) {
				t.Fatalf("%s: the workload built no doclist index", table)
			}
		}
		check(t, db)
	})
	t.Run("ReindexText over 6000 records", func(t *testing.T) {
		c := indexedTextCase(t, 6000)
		check(t, c.store.db)
	})
}

func TestFTS5DecodeIsBounded(t *testing.T) {
	for name, load := range genuineFTSWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			for _, table := range FTSTables() {
				for _, seg := range ftsStructureOf(t, db, table) {
					r := newFTSSegReader(seg)
					for _, p := range ftsLeafPages(t, db, table, seg.ID) {
						before := r.consumed
						leaf := 0
						err := r.Page(p.pgno, p.block, ftsSink{Leaf: func(l ftsLeafInfo) {
							leaf++
							if l.Pgno != p.pgno || l.SzLeaf > len(p.block) || l.HasRowid != (l.RowidOffset != 0) || l.HasTerm != (len(l.TermOffsets) > 0) {
								t.Errorf("%s seg %d: inconsistent leaf info %+v", table, seg.ID, l)
							}
							if l.HasTerm && (l.TermOffsets[0] != ftsLeafHeader) == !l.DoclistContinues {
								t.Errorf("%s seg %d page %d: DoclistContinues %v with first term at %d", table, seg.ID, p.pgno, l.DoclistContinues, l.TermOffsets[0])
							}
							if l.HasTerm && (len(l.FirstTerm) == 0 || l.FirstTerm[0] != ftsMainPrefix) {
								t.Errorf("%s seg %d page %d: first term %q", table, seg.ID, p.pgno, l.FirstTerm)
							}
						}})
						if err != nil {
							t.Fatalf("%s: %v", table, err)
						}
						if got := r.consumed - before; got != len(p.block) || leaf != 1 {
							t.Fatalf("%s seg %d page %d: consumed %d bytes of %d, %d leaf callbacks", table, seg.ID, p.pgno, got, len(p.block), leaf)
						}
					}
					if err := r.Finish(); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestFTS5DecodeReportsDeleteMarkers(t *testing.T) {
	db := genuineFTS(t)
	fill(t, db, 1, 20, 10)
	probeExec(t, db, "INSERT INTO records_fts(records_fts, rank) VALUES('automerge', 0)")
	// the distinct terms of document 5, from the index before the delete
	terms := map[string]bool{}
	for p := range vocabPostings(t, db, FTSWordTable) {
		if p.rowid == 5 {
			terms[p.term] = true
		}
	}
	if len(terms) < 5 {
		t.Fatalf("document 5 has only %d terms", len(terms))
	}
	summary, body := fillText(5, 10)
	probeExec(t, db, "INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES('delete', 5, ?, ?)", summary, body)

	deletes := map[string]int{}
	var markerSeg int64
	var postingSegs []int64
	for _, seg := range ftsStructureOf(t, db, FTSWordTable) {
		r := newFTSSegReader(seg)
		sink := ftsSink{
			Delete: func(term []byte, rowid int64) {
				if rowid != 5 {
					t.Errorf("delete marker for record %d", rowid)
				}
				deletes[string(term)]++
				markerSeg = seg.ID
			},
			Posting: func(_ []byte, rowid int64, _ int, _ int64) {
				if rowid == 5 && !slices.Contains(postingSegs, seg.ID) {
					postingSegs = append(postingSegs, seg.ID)
				}
			},
		}
		for _, p := range ftsLeafPages(t, db, FTSWordTable, seg.ID) {
			if err := r.Page(p.pgno, p.block, sink); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(); err != nil {
			t.Fatal(err)
		}
	}
	if len(deletes) != len(terms) {
		t.Fatalf("%d terms carry a delete marker, document 5 has %d", len(deletes), len(terms))
	}
	for term := range terms {
		if deletes[term] != 1 {
			t.Fatalf("term %q: %d delete markers, want 1", term, deletes[term])
		}
	}
	// the marker segment is newer than every segment that still holds a posting of the record
	age := ftsSegmentAge(ftsStructureOf(t, db, FTSWordTable))
	if len(postingSegs) == 0 {
		t.Fatal("no segment holds a posting of record 5")
	}
	for _, s := range postingSegs {
		if age[markerSeg] <= age[s] {
			t.Fatalf("marker segment %d (age %d) is not newer than posting segment %d (age %d)", markerSeg, age[markerSeg], s, age[s])
		}
	}
}

func TestFTSStructureSegmentsKeepLevelsAndPages(t *testing.T) {
	for name, load := range genuineFTSWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			for _, table := range FTSTables() {
				segs := ftsStructureOf(t, db, table)
				var structure []byte
				if err := db.QueryRow("SELECT block FROM "+table+"_data WHERE id = ?", ftsStructureRowID).Scan(&structure); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
				ids, err := decodeFTSStructure(structure)
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) != len(segs) {
					t.Fatalf("%s: %d ids, %d segments", table, len(ids), len(segs))
				}
				pos := map[int]int{}
				for _, s := range segs {
					if _, ok := ids[s.ID]; !ok {
						t.Fatalf("%s: segment %d is not in the id set", table, s.ID)
					}
					if s.Pos != pos[s.Level] {
						t.Fatalf("%s: segment %d has Pos %d in level %d, want %d", table, s.ID, s.Pos, s.Level, pos[s.Level])
					}
					pos[s.Level]++
					pages := ftsLeafPages(t, db, table, s.ID)
					if int64(len(pages)) != s.Last-s.First+1 {
						t.Fatalf("%s: segment %d has %d leaf rows for pages %d..%d", table, s.ID, len(pages), s.First, s.Last)
					}
					for i, p := range pages {
						if p.pgno != s.First+int64(i) {
							t.Fatalf("%s: segment %d: leaf row %d is page %d, want %d", table, s.ID, i, p.pgno, s.First+int64(i))
						}
					}
				}
				age := ftsSegmentAge(segs)
				seen := map[int]bool{}
				for _, s := range segs {
					a := age[s.ID]
					if a < 1 || a > len(segs) || seen[a] {
						t.Fatalf("%s: age %d of segment %d is out of range or repeated", table, a, s.ID)
					}
					seen[a] = true
				}
			}
		})
	}
}

func TestFTSSegmentAgeOrder(t *testing.T) {
	segs := []ftsSegment{
		{ID: 9, Level: 0, Pos: 0}, {ID: 8, Level: 0, Pos: 1}, {ID: 7, Level: 1, Pos: 0}, {ID: 3, Level: 1, Pos: 1}, {ID: 2, Level: 2, Pos: 0},
	}
	age := ftsSegmentAge(segs)
	// newest first: level 0 from its last segment, then level 1 from its last, then level 2
	want := []int64{8, 9, 3, 7, 2}
	for i := 1; i < len(want); i++ {
		if age[want[i-1]] <= age[want[i]] {
			t.Fatalf("segment %d (age %d) must be newer than %d (age %d)", want[i-1], age[want[i-1]], want[i], age[want[i]])
		}
	}
}

func TestFTS5SegmentRequiresEveryPage(t *testing.T) {
	db := genuineFTS(t)
	probeExec(t, db, "INSERT INTO records_fts(rowid, summary, body) VALUES (1, 's', ?)", strings.Repeat("alpha beta gamma delta ", 9000))
	var seg ftsSegment
	for _, s := range ftsStructureOf(t, db, FTSWordTable) {
		if s.Last-s.First >= 2 {
			seg = s
		}
	}
	if seg.ID == 0 {
		t.Fatal("no segment of 3 pages or more")
	}
	pages := ftsLeafPages(t, db, FTSWordTable, seg.ID)
	reason := func(err error) string {
		var le *ftsLeafError
		if !errors.As(err, &le) || !errors.Is(err, errFTSLeaf) {
			t.Fatalf("not an ftsLeafError: %v", err)
		}
		return le.Reason
	}
	t.Run("a skipped page", func(t *testing.T) {
		r := newFTSSegReader(seg)
		if err := r.Page(pages[0].pgno, pages[0].block, ftsSink{}); err != nil {
			t.Fatal(err)
		}
		if got := reason(r.Page(pages[2].pgno, pages[2].block, ftsSink{})); got != "missing page" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("the last page missing", func(t *testing.T) {
		r := newFTSSegReader(seg)
		for _, p := range pages[:len(pages)-1] {
			if err := r.Page(p.pgno, p.block, ftsSink{}); err != nil {
				t.Fatal(err)
			}
		}
		if got := reason(r.Finish()); got != "missing page" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a repeated page and a page past the segment", func(t *testing.T) {
		r := newFTSSegReader(seg)
		if err := r.Page(pages[0].pgno, pages[0].block, ftsSink{}); err != nil {
			t.Fatal(err)
		}
		if got := reason(r.Page(pages[0].pgno, pages[0].block, ftsSink{})); got != "page out of order" {
			t.Fatalf("reason %q", got)
		}
		r = newFTSSegReader(seg)
		if got := reason(r.Page(seg.Last+1, pages[0].block, ftsSink{})); got != "page outside the segment" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a reader that failed stays failed", func(t *testing.T) {
		r := newFTSSegReader(seg)
		err := r.Page(pages[1].pgno, pages[1].block, ftsSink{})
		if err == nil || r.Page(pages[0].pgno, pages[0].block, ftsSink{}) == nil || r.Finish() == nil {
			t.Fatal("a failed reader accepted more")
		}
	})
	t.Run("everything", func(t *testing.T) {
		r := newFTSSegReader(seg)
		for _, p := range pages {
			if err := r.Page(p.pgno, p.block, ftsSink{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(); err != nil {
			t.Fatal(err)
		}
	})
}

// testVarint is SQLite's encoder for the pages the tests build.
func testVarint(v uint64) []byte {
	if v>>56 != 0 {
		out := make([]byte, 9)
		out[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return out
	}
	rev := []byte{byte(v & 0x7f)}
	for v >>= 7; v != 0; v >>= 7 {
		rev = append(rev, byte(v&0x7f)|0x80)
	}
	slices.Reverse(rev)
	return rev
}

// leafBuilder assembles a leaf page: the body bytes, the term starts and the header.
type leafBuilder struct {
	body []byte
	offs []int
}

func (b *leafBuilder) raw(x ...byte) *leafBuilder { b.body = append(b.body, x...); return b }

func (b *leafBuilder) term() *leafBuilder {
	b.offs = append(b.offs, ftsLeafHeader+len(b.body))
	return b
}

func (b *leafBuilder) page(rowidOff int) []byte {
	szLeaf := ftsLeafHeader + len(b.body)
	out := []byte{byte(rowidOff >> 8), byte(rowidOff), byte(szLeaf >> 8), byte(szLeaf)}
	out = append(out, b.body...)
	prev := 0
	for _, o := range b.offs {
		out = append(out, testVarint(uint64(o-prev))...)
		prev = o
	}
	return out
}

// goodPage holds two terms, "abc" (records 10 and 15) and "ac" (record 7). Its positions are
// (abc,10,0,3) (abc,10,0,4) (abc,15,1,0) (ac,7,0,0).
func goodPage() []byte {
	b := &leafBuilder{}
	b.term().raw(4).raw([]byte("0abc")...)
	b.raw(10, 4, 5, 3)      // record 10: size 2 bytes, offsets 3 and 4
	b.raw(5, 6, 1, 1, 2)    // record 15: size 3 bytes, a column marker for column 1, offset 0
	b.term().raw(2, 1, 'c') // "0ac"
	b.raw(7, 2, 2)          // record 7: one position at offset 0
	return b.page(0)
}

func decodePages(pages ...[]byte) (got []ftsDecoded, deletes int, err error) {
	r := newFTSSegReader(ftsSegment{ID: 1, First: 1, Last: int64(len(pages))})
	sink := ftsSink{
		Posting: func(term []byte, rowid int64, col int, off int64) {
			got = append(got, ftsDecoded{string(term), rowid, col, off})
		},
		Delete: func([]byte, int64) { deletes++ },
	}
	for i, p := range pages {
		if err = r.Page(int64(i+1), p, sink); err != nil {
			return got, deletes, err
		}
	}
	return got, deletes, r.Finish()
}

func TestFTS5LeafAcceptsTheGoodPage(t *testing.T) {
	got, deletes, err := decodePages(goodPage())
	if err != nil || deletes != 0 {
		t.Fatalf("%v %d", err, deletes)
	}
	want := []ftsDecoded{{"abc", 10, 0, 3}, {"abc", 10, 0, 4}, {"abc", 15, 1, 0}, {"ac", 7, 0, 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestFTS5LeafRejectsHostilePages(t *testing.T) {
	tail := func(b *leafBuilder) *leafBuilder { return b.term().raw(2, 1, 'c').raw(7, 2, 2) }
	head := func() *leafBuilder { return (&leafBuilder{}).term().raw(4).raw([]byte("0abc")...) }
	long := strings.Repeat("a", fts5MaxTokenSize+1) // "0" + this is one byte over the limit
	good := goodPage()
	cases := []struct {
		name   string
		page   []byte
		reason string
	}{
		{"a varint padded with 0x80", tail(head().raw(0x80, 10, 4, 5, 3)).page(0), "non-canonical varint"},
		{"a prefix one shorter than maximal", head().raw(10, 4, 5, 3).term().raw(1, 2, 'a', 'c').raw(7, 2, 2).page(0), "prefix not maximal"},
		{"a rowid delta of 0", tail(head().raw(10, 4, 5, 3).raw(0, 6, 1, 1, 2)).page(0), "rowid not ascending"},
		{"a position delta of 0 after a position", tail(head().raw(10, 4, 5, 2)).page(0), "position not ascending"},
		{"column 2", tail(head().raw(10, 6, 1, 2, 2)).page(0), "column out of order or range"},
		{"a column marker for column 0", tail(head().raw(10, 6, 1, 0, 2)).page(0), "column out of order or range"},
		{"an offset below 2 after the column", tail(head().raw(10, 6, 1, 1, 1)).page(0), "invalid first position after a column"},
		{"a poslist that ends in a column marker", tail(head().raw(10, 4, 5, 1)).page(0), "poslist ends inside a column marker"},
		{"a term that runs into the next term start", func() []byte {
			b := (&leafBuilder{}).raw(10).raw([]byte("0abcdefghi")...).raw(10, 2, 2)
			b.offs = []int{4, 8}
			return b.page(0)
		}(), "footer mismatch"},
		{"a first term offset inside the header", func() []byte { p := slices.Clone(good); p[len(p)-2] = 3; return p }(), "term offset out of range"},
		{"a first term offset past the body", func() []byte { p := slices.Clone(good); p[len(p)-2] = 100; return p }(), "term offset out of range"},
		{"a poslist size past 32 bits", head().raw(10).raw(testVarint(1 << 33)...).page(0), "poslist size out of range"},
		{"a position past 2^63", head().raw(10, 18).raw(testVarint(1<<63 + 100)...).page(0), "position out of range"},
		{"position 0", tail(head().raw(10, 2, 0)).page(0), "invalid position 0"},
		{"an empty poslist", tail(head().raw(10, 0)).page(0), "empty poslist"},
		{"a poslist cut by the next term", tail(head().raw(10, 8, 5)).page(0), "footer mismatch"},
		{"a term that does not ascend", head().raw(10, 4, 5, 3).term().raw(2, 1, 'a').raw(7, 2, 2).page(0), "terms not ascending"},
		{"a term with an unknown index prefix", (&leafBuilder{}).term().raw(4).raw([]byte("1abc")...).raw(10, 2, 2).page(0), "unknown index prefix"},
		{"a term with no doclist", head().term().raw(2, 1, 'c').raw(7, 2, 2).page(0), "term without a doclist"},
		{"an empty term", (&leafBuilder{}).term().raw(0).raw(10, 2, 2).page(0), "empty term"},
		{"a term past the page", (&leafBuilder{}).term().raw(100).raw([]byte("0abc")...).page(0), "term overruns the page"},
		{"a term grown past the token limit", (&leafBuilder{}).term().raw(testVarint(uint64(len(long)+1))...).raw([]byte("0"+long)...).raw(10, 2, 2).page(0), "term too long"},
		{"a doclist before any term", (&leafBuilder{}).raw(10, 2, 2).page(4), "first page of a segment does not start with a term"},
		{"szLeaf past the end", func() []byte { p := slices.Clone(good); p[2], p[3] = 0, byte(len(p)+1); return p }(), "szLeaf out of range"},
		{"szLeaf of 4", []byte{0, 0, 0, 4}, "empty leaf"},
		{"a footer byte changed", func() []byte { p := slices.Clone(good); p[len(p)-1]++; return p }(), "footer mismatch"},
		{"a footer offset past the body", func() []byte { p := slices.Clone(good); p[len(p)-1] = 0x7f; return p }(), "term offset out of range"},
		{"a footer delta of 0", func() []byte { p := slices.Clone(good); p[len(p)-1] = 0; return p }(), "term offset out of range"},
		{"a padded footer varint", append(slices.Clone(good[:len(good)-2]), 0x80, 4, 14), "non-canonical varint"},
		{"firstRowidOff set on a page that starts with a term", func() []byte { p := slices.Clone(good); p[1] = 9; return p }(), "first rowid offset"},
		{"a header cut short", good[:3], "short page"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := decodePages(c.page)
			var le *ftsLeafError
			if !errors.Is(err, errFTSLeaf) || !errors.As(err, &le) {
				t.Fatalf("err = %v", err)
			}
			if le.Reason != c.reason {
				t.Fatalf("reason %q, want %q (%v)", le.Reason, c.reason, err)
			}
		})
	}

	t.Run("the page cut at each byte", func(t *testing.T) {
		for n := 0; n < len(good); n++ {
			if _, _, err := decodePages(good[:n]); !errors.Is(err, errFTSLeaf) {
				t.Fatalf("a page cut to %d bytes decodes (err %v)", n, err)
			}
		}
	})
	t.Run("an open poslist at the end of the segment", func(t *testing.T) {
		_, _, err := decodePages(head().raw(10, 8, 5).page(0))
		var le *ftsLeafError
		if !errors.As(err, &le) || le.Reason != "poslist open at the end of the segment" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a continuation page with a wrong rowid offset", func(t *testing.T) {
		first := head().raw(10, 4, 5, 3).page(0)
		for _, off := range []int{0, 5, 3} {
			cont := (&leafBuilder{}).raw(20, 2, 2).page(off)
			_, _, err := decodePages(first, cont)
			var le *ftsLeafError
			if !errors.As(err, &le) || le.Reason != "first rowid offset" {
				t.Fatalf("offset %d: err = %v", off, err)
			}
		}
		got, _, err := decodePages(first, (&leafBuilder{}).raw(20, 2, 2).page(4))
		if err != nil || len(got) != 3 || got[2].rowid != 20 {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("a continuation rowid that does not ascend", func(t *testing.T) {
		first := head().raw(10, 4, 5, 3).page(0)
		_, _, err := decodePages(first, (&leafBuilder{}).raw(10, 2, 2).page(4))
		var le *ftsLeafError
		if !errors.As(err, &le) || le.Reason != "rowid not ascending" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a poslist split between pages", func(t *testing.T) {
		first := head().raw(10, 6, 5).page(0) // record 10: three bytes, one here
		cont := (&leafBuilder{}).raw(3, 3).page(0)
		got, _, err := decodePages(first, cont)
		want := []ftsDecoded{{"abc", 10, 0, 3}, {"abc", 10, 0, 4}, {"abc", 10, 0, 5}}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("a size varint at the start of the next page", func(t *testing.T) {
		first := head().raw(10).page(0)               // the page ends right after the rowid
		cont := (&leafBuilder{}).raw(4, 5, 3).page(0) // size, then the poslist
		got, _, err := decodePages(first, cont)
		if err != nil || len(got) != 2 || got[1].off != 4 {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("a delete marker is reported and is not an error", func(t *testing.T) {
		got, deletes, err := decodePages(tail(head().raw(10, 4, 5, 3).raw(5, 1)).page(0))
		if err != nil || deletes != 1 || len(got) != 3 {
			t.Fatalf("%v %d %v", got, deletes, err)
		}
	})
	t.Run("a delete marker that carries a poslist", func(t *testing.T) {
		got, deletes, err := decodePages(head().raw(10, 5, 5, 3).page(0)) // size 5 = 2 bytes, deleted
		if err != nil || deletes != 1 || len(got) != 0 {
			t.Fatalf("%v %d %v", got, deletes, err)
		}
	})
	t.Run("a position past the 32-bit range", func(t *testing.T) {
		big := testVarint(1 << 33)
		size := testVarint(uint64(len(big)) << 1)
		_, _, err := decodePages(head().raw(10).raw(size...).raw(big...).page(0))
		var le *ftsLeafError
		if !errors.As(err, &le) || le.Reason != "position out of range" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a rowid delta that overflows", func(t *testing.T) {
		abs := testVarint(1<<63 - 1) // MaxInt64 as the first rowid
		_, _, err := decodePages(head().raw(abs...).raw(2, 2).raw(1, 2, 2).page(0))
		var le *ftsLeafError
		if !errors.As(err, &le) || le.Reason != "rowid not ascending" {
			t.Fatalf("err = %v", err)
		}
	})
}

// A page that declares an enormous term (a 9-byte varint of 2^62) costs one reader and one error.
func TestFTS5LeafDoesNotAllocateFromDeclaredSizes(t *testing.T) {
	page := (&leafBuilder{}).term().raw(testVarint(1 << 62)...).page(0)
	allocs := testing.AllocsPerRun(50, func() {
		r := newFTSSegReader(ftsSegment{ID: 1, First: 1, Last: 1})
		if err := r.Page(1, page, ftsSink{}); err == nil {
			t.Error("accepted a hostile term")
		}
	})
	if allocs > 2 {
		t.Fatalf("%v allocations for a hostile page", allocs)
	}
	// a poslist size of 2^31 is a counter, not an allocation: pages keep decrementing it
	huge := testVarint(1<<32 - 2)
	big := (&leafBuilder{}).term().raw(4).raw([]byte("0abc")...).raw(10).raw(huge...).raw(5, 3).page(0)
	allocs = testing.AllocsPerRun(50, func() {
		r := newFTSSegReader(ftsSegment{ID: 1, First: 1, Last: 1})
		_ = r.Page(1, big, ftsSink{})
		_ = r.Finish()
	})
	if allocs > 6 {
		t.Fatalf("%v allocations for a page that declares a huge poslist", allocs)
	}
}

// A genuine index in the middle of an incremental merge holds segments that _fts5TrimSegments emptied: the
// structure still lists them, with pages 0 to 0 and no leaf rows (5D-14h2 D143). The decoder reads nothing
// for such a segment and never calls it a missing page; the postings of the table still equal fts5vocab at
// every step of the merge.
func TestFTS5DecodeAcceptsEmptiedSegmentsOfAnIncrementalMerge(t *testing.T) {
	db := genuineFTS(t)
	probeExec(t, db, "INSERT INTO records_fts(records_fts, rank) VALUES('automerge', 0)")
	fill(t, db, 1, 120, 20)
	emptied := 0
	for step := 0; step < 400; step++ {
		for _, table := range FTSTables() {
			probeExec(t, db, "INSERT INTO "+table+"("+table+", rank) VALUES('merge', 1)")
		}
		for _, seg := range ftsStructureOf(t, db, FTSWordTable) {
			if seg.First == 0 && seg.Last == 0 {
				emptied++
				if !seg.Empty() {
					t.Fatalf("segment %d has pages 0 to 0 and is not Empty()", seg.ID)
				}
			} else if seg.Empty() {
				t.Fatalf("segment %d (pages %d to %d) is Empty()", seg.ID, seg.First, seg.Last)
			}
		}
		for _, table := range FTSTables() {
			requireDecodeEqualsVocab(t, db, table)
		}
	}
	if emptied == 0 {
		t.Fatal("the merge workload never produced an emptied segment, so it proves nothing")
	}
	r := newFTSSegReader(ftsSegment{ID: 7})
	if err := r.Page(0, []byte{0, 0, 0, 5, 1}, ftsSink{}); err == nil {
		t.Fatal("an emptied segment accepted a page")
	}
}
