package dbrecover

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func slackVarint(v uint64) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	var out []byte
	for v >= 0x80 {
		out = append([]byte{byte(v & 0x7f)}, out...)
		v >>= 7
	}
	out = append([]byte{byte(v)}, out...)
	for i := 0; i < len(out)-1; i++ {
		out[i] |= 0x80
	}
	return out
}

// slackRecord encodes a record of NULL (nil), small non-negative integers
// (int, one byte) and text (string) values.
func slackRecord(vals ...any) []byte {
	var hdr, body []byte
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			hdr = append(hdr, 0)
		case int:
			hdr = append(hdr, 1)
			body = append(body, byte(x))
		case string:
			hdr = append(hdr, slackVarint(uint64(13+2*len(x)))...)
			body = append(body, x...)
		default:
			panic("slackRecord: unsupported value")
		}
	}
	h := append(slackVarint(uint64(len(hdr)+1)), hdr...)
	return append(h, body...)
}

// slackCell encodes a table-leaf cell.
func slackCell(rowid int64, vals ...any) []byte {
	rec := slackRecord(vals...)
	c := slackVarint(uint64(len(rec)))
	c = append(c, slackVarint(uint64(rowid))...)
	return append(c, rec...)
}

// slackScene is a database built with the sqlitetest builder; planted bytes
// stand for what a defragmentation or a delete left behind.
type slackScene struct {
	b *sqlitetest.Builder
}

func newSlackScene(build func(b *sqlitetest.Builder)) *slackScene {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	build(b)
	return &slackScene{b: b}
}

// plainScene has t(a, b) on page 2 with the live rows 1 and 2.
func plainScene() *slackScene {
	return newSlackScene(func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		tb.Insert(1, 10, "live-row-one-"+filler(30, 1))
		tb.Insert(2, 20, "live-row-two-"+filler(30, 2))
	})
}

// gap returns the offset in page pg and the length of its gap span.
func (s *slackScene) gap(t *testing.T, pg uint32) (off, n int) {
	t.Helper()
	db := s.open(t)
	h := db.History()
	defer h.Release()
	err := h.Pages(context.Background(), func(img sqlitefile.PageImage) bool {
		if img.Number == pg {
			for _, sp := range img.Spans {
				if sp.Kind == sqlitefile.SpanGap {
					off, n = sp.Offset, sp.Length
				}
			}
		}
		return true
	})
	if err != nil || n == 0 {
		t.Fatalf("no gap on page %d (%v)", pg, err)
	}
	return off, n
}

// plant writes p at offset off of page pg.
func (s *slackScene) plant(pg uint32, off int, p []byte) {
	s.b.Patch(int(pg-1)*1024+off, p...)
}

func (s *slackScene) open(t *testing.T) *sqlitefile.DB {
	t.Helper()
	raw := s.b.Bytes()
	db, err := sqlitefile.Open(bytesAt{raw}, int64(len(raw)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func (s *slackScene) carve(t *testing.T) ([]sqlitefile.RecoveredRow, slackStats) {
	t.Helper()
	db := s.open(t)
	h := db.History()
	defer h.Release()
	var rows []sqlitefile.RecoveredRow
	st, err := carveSpans(context.Background(), db, h, func(r sqlitefile.RecoveredRow) bool {
		rows = append(rows, r)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, st
}

func rowText(r sqlitefile.RecoveredRow, col int) string {
	if col >= len(r.Values) {
		return ""
	}
	s, _ := r.Values[col].Text()
	return s
}

func hasNote(r sqlitefile.RecoveredRow, n string) bool { return slices.Contains(r.Notes, n) }

func TestCarveGapCopyRecoversDefragmentedCell(t *testing.T) {
	s := plainScene()
	off, _ := s.gap(t, 2)
	cell := slackCell(7, 70, "deleted-row-seven-"+filler(30, 7))
	s.plant(2, off+40, cell) // a stale copy a defragmentation left in the gap
	rows, st := s.carve(t)
	if len(rows) != 1 || st.Carved != 1 {
		t.Fatalf("%d rows, Carved %d, want 1 and 1", len(rows), st.Carved)
	}
	r := rows[0]
	if r.Method != sqlitefile.MethodPageSlack || r.Loc.Cell != -1 || r.Origin != sqlitefile.OriginLiveBTree {
		t.Errorf("method %q cell %d origin %v", r.Method, r.Loc.Cell, r.Origin)
	}
	if r.Rowid == nil || *r.Rowid != 7 || len(r.Values) != 2 || r.Values[0].Int != 70 || !strings.HasPrefix(rowText(r, 1), "deleted-row-seven-") {
		t.Errorf("rowid %v values %+v", r.Rowid, r.Values)
	}
	if r.Loc.Page != 2 || r.Loc.PageOffset != 1024 || r.Loc.Offset != int64(1024+off+40) || r.Loc.Length != int64(len(cell)) {
		t.Errorf("loc %+v, want page 2 offset %d length %d", r.Loc, 1024+off+40, len(cell))
	}
	if r.Relation != sqlitefile.RelUnknown {
		t.Errorf("relation %q, want unknown", r.Relation)
	}
	if r.Table != "t" || r.TableBasis != sqlitefile.BasisFit {
		t.Errorf("table %q basis %q, want t by fit", r.Table, r.TableBasis)
	}
	for _, n := range []string{sqlitefile.NoteIdentityByFitOnly, noteBoundariesInferred} {
		if !hasNote(r, n) {
			t.Errorf("notes %v lack %q", r.Notes, n)
		}
	}
	if hasNote(r, sqlitefile.NoteEqualsLiveRow) {
		t.Errorf("notes %v: the row does not equal a live row", r.Notes)
	}
}

func TestSlackCopyOfLiveCellIsDropped(t *testing.T) {
	s := plainScene()
	off, _ := s.gap(t, 2)
	// byte-equal to the live row 2 of the same page
	s.plant(2, off+40, slackCell(2, 20, "live-row-two-"+filler(30, 2)))
	rows, st := s.carve(t)
	if len(rows) != 0 || st.Carved != 0 || st.DroppedLiveCopy != 1 {
		t.Fatalf("%d rows, Carved %d, DroppedLiveCopy %d, want 0, 0, 1", len(rows), st.Carved, st.DroppedLiveCopy)
	}
	if !slices.ContainsFunc(st.warnings, func(w string) bool { return strings.Contains(w, "page 2") }) {
		t.Errorf("warnings %q: none names page 2", st.warnings)
	}
}

func TestSlackCrossPageLiveEqualIsAnnotatedNotDropped(t *testing.T) {
	s := newSlackScene(func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		tb.Insert(1, 10, "live-row-one-"+filler(30, 1))
		u := b.CreateTable("u", "create table u(p integer)")
		u.Insert(1, 5)
	})
	// page 2 is t, page 3 is u: a copy of t's live row 1 in the gap of u's page
	off, n := s.gap(t, 3)
	if n < 200 {
		t.Fatalf("gap of page 3: %d bytes", n)
	}
	s.plant(3, off+20, slackCell(1, 10, "live-row-one-"+filler(30, 1)))
	rows, st := s.carve(t)
	if len(rows) != 1 || st.Carved != 1 || st.CrossPageEqual != 1 || st.DroppedLiveCopy != 0 {
		t.Fatalf("%d rows %+v, stats %+v, want one delivered, CrossPageEqual 1", len(rows), rows, st)
	}
	r := rows[0]
	if r.Table != "t" || !hasNote(r, sqlitefile.NoteEqualsLiveRow) || r.Relation != sqlitefile.RelUnknown {
		t.Errorf("table %q notes %v relation %q", r.Table, r.Notes, r.Relation)
	}
}

func TestSlackNeverOverlapsLiveCells(t *testing.T) {
	s := plainScene()
	// a freeblock that starts at the lowest live cell and covers both live
	// cells: the span is not free space, no carved cell may lie over a live cell
	var live [][2]int
	h := s.open(t).History()
	_ = h.Pages(context.Background(), func(img sqlitefile.PageImage) bool {
		if img.Number == 2 {
			data, _ := img.Bytes()
			hd, _ := sqlitefile.ParsePageHeader(data, 2)
			cp, _ := sqlitefile.CellPointers(data, hd, 1024)
			for _, g := range cp.Good {
				c, _ := sqlitefile.ParseCell(data, 1024, hd, g.Offset)
				live = append(live, [2]int{c.Offset, c.Offset + c.Length})
			}
		}
		return true
	})
	h.Release()
	if len(live) != 2 {
		t.Fatalf("%d live cells", len(live))
	}
	lo := min(live[0][0], live[1][0])
	hi := max(live[0][1], live[1][1])
	s.plant(2, 1, []byte{byte(lo >> 8), byte(lo)})
	s.plant(2, lo, []byte{0, 0, byte((hi - lo) >> 8), byte(hi - lo)})
	rows, st := s.carve(t)
	for _, r := range rows {
		a, b := int(r.Loc.Offset-r.Loc.PageOffset), int(r.Loc.Offset-r.Loc.PageOffset+r.Loc.Length)
		for _, l := range live {
			if a < l[1] && l[0] < b {
				t.Errorf("carved cell [%d,%d) overlaps live cell %v", a, b, l)
			}
		}
	}
	if st.DroppedOverlap == 0 {
		t.Errorf("stats %+v: the live cells inside the span were neither dropped as overlap nor as copies", st)
	}
}

func TestSlackRequiresCompleteCellInsideOneSpan(t *testing.T) {
	good := slackCell(9, 90, "complete-cell-"+filler(24, 9))
	cases := map[string]func(off, _ int) (int, []byte){
		"positive control": func(off, _ int) (int, []byte) { return off + 10, good },
		"crosses the span end": func(off, n int) (int, []byte) {
			return off + n - len(good) + 6, good // the last 6 bytes would lie past the span
		},
		"header plus body differ from the payload length": func(off, _ int) (int, []byte) {
			c := slices.Clone(good)
			c[0]++ // declares one more payload byte than the record holds
			return off + 10, append(c, 0x41)
		},
		"reserved serial type 10": func(off, _ int) (int, []byte) {
			rec := []byte{3, 10, 1, 5}
			return off + 10, append(append(slackVarint(uint64(len(rec))), 9), rec...)
		},
		"all-NULL record": func(off, _ int) (int, []byte) {
			rec := []byte{4, 0, 0, 0}
			return off + 10, append(append(slackVarint(uint64(len(rec))), 9), rec...)
		},
		"non-canonical rowid varint": func(off, _ int) (int, []byte) {
			rec := slackRecord(90, "complete-cell-"+filler(24, 9))
			c := append(slackVarint(uint64(len(rec))), 0x80, 9) // the rowid 9 in two bytes
			return off + 10, append(c, rec...)
		},
		"reserved serial type 11": func(off, _ int) (int, []byte) {
			rec := []byte{3, 11, 1, 5}
			return off + 10, append(append(slackVarint(uint64(len(rec))), 9), rec...)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			s := plainScene()
			off, n := s.gap(t, 2)
			at, cell := mk(off, n)
			// the bytes after the planted cell are made non-zero so that an
			// overrun is real data, not zero fill
			s.plant(2, at, cell)
			rows, st := s.carve(t)
			if name == "positive control" {
				if len(rows) != 1 {
					t.Fatalf("control: %d rows, want 1", len(rows))
				}
				return
			}
			if len(rows) != 0 {
				t.Errorf("%d rows carved from an incomplete or invalid cell", len(rows))
			}
			if st.DroppedOverlap != 0 || st.DroppedLiveCopy != 0 {
				t.Errorf("stats %+v: the cell must be refused as invalid, not as a live cell", st)
			}
		})
	}
}

func TestSlackGreedyNonOverlapping(t *testing.T) {
	s := plainScene()
	off, _ := s.gap(t, 2)
	inner := slackCell(4, 40, "inner-cell-"+filler(20, 4))
	outer := slackCell(5, 50, string(inner)) // a text value that is itself a valid cell
	next := slackCell(6, 60, "following-cell-"+filler(20, 6))
	s.plant(2, off+10, append(slices.Clone(outer), next...))
	rows, _ := s.carve(t)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want the outer cell and the following one", len(rows))
	}
	if *rows[0].Rowid != 5 || *rows[1].Rowid != 6 {
		t.Errorf("rowids %d, %d, want 5 then 6 (leftmost wins, nothing inside a taken cell)", *rows[0].Rowid, *rows[1].Rowid)
	}
	if rows[0].Loc.Offset+rows[0].Loc.Length > rows[1].Loc.Offset {
		t.Errorf("carved cells overlap: %+v %+v", rows[0].Loc, rows[1].Loc)
	}
}

func TestSlackFreeblockWithOverwrittenStartIsCountedNotInvented(t *testing.T) {
	s := newSlackScene(func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		for id := int64(1); id <= 6; id++ {
			tb.Insert(id, int(id)*10, "row-"+filler(40, byte(id)))
		}
		tb.Delete(3) // a residue cell between live cells: a freeblock whose first 4 bytes are overwritten
	})
	rows, st := s.carve(t)
	for _, r := range rows {
		if r.Rowid != nil && *r.Rowid == 3 {
			t.Errorf("row 3 was invented from a freeblock whose start is overwritten: %+v", r)
		}
	}
	if st.UnparsedBytes < 30 {
		t.Errorf("UnparsedBytes %d: the unreadable freeblock bytes are not counted", st.UnparsedBytes)
	}
}

func TestSlackAmbiguousFitNamesNoTable(t *testing.T) {
	s := newSlackScene(func(b *sqlitetest.Builder) {
		t1 := b.CreateTable("t", "create table t(a integer, b text)")
		t1.Insert(1, 10, "live-row-one-"+filler(30, 1))
		v := b.CreateTable("v", "create table v(a integer, b text)")
		v.Insert(1, 11, "another-"+filler(30, 3))
	})
	off, _ := s.gap(t, 2)
	s.plant(2, off+10, slackCell(8, 80, "ambiguous-"+filler(24, 8)))
	rows, _ := s.carve(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if r := rows[0]; r.Table != "" || r.TableBasis != sqlitefile.BasisNone {
		t.Errorf("table %q basis %q: two tables fit, so none may be named", r.Table, r.TableBasis)
	}
}

func TestSlackAllZeroSpansAreSilent(t *testing.T) {
	s := plainScene()
	off, n := s.gap(t, 2)
	s.plant(2, off, make([]byte, n))
	rows, st := s.carve(t)
	if len(rows) != 0 || st.Carved != 0 || st.UnparsedBytes != 0 || st.DroppedLiveCopy != 0 || len(st.warnings) != 0 {
		t.Errorf("%d rows, stats %+v: a zeroed span must be silent", len(rows), st)
	}
}

func TestSlackIsBoundedOnHostilePages(t *testing.T) {
	s := plainScene()
	off, n := s.gap(t, 2)
	// every offset looks like a long record header: 0x7f is a payload length
	// of 127 and a header length of 127 followed by 126 serial types
	s.plant(2, off, bytes.Repeat([]byte{0x7f}, n/2)) // the second half of the gap stays zero
	old := slackPageBudget
	slackPageBudget = 500
	defer func() { slackPageBudget = old }()
	rows, st := s.carve(t)
	if st.WorkCapped != 1 {
		t.Errorf("%d pages capped, want 1 (page work cap %d)", st.WorkCapped, slackPageBudget)
	}
	if !slices.ContainsFunc(st.warnings, func(w string) bool { return strings.Contains(w, "page 2") && strings.Contains(w, "work cap") }) {
		t.Errorf("warnings %q: the work cap on page 2 is not reported", st.warnings)
	}
	if st.UnparsedBytes == 0 {
		t.Errorf("rows %d, UnparsedBytes 0: the bytes left unexamined are not counted", len(rows))
	}
	if st.UnparsedBytes > int64(n/2) {
		t.Errorf("UnparsedBytes %d: zero bytes must not be counted (only %d bytes were planted)", st.UnparsedBytes, n/2)
	}
}

func FuzzCarveSpans(f *testing.F) {
	f.Add([]byte{}, 0)
	f.Add(slackCell(7, 70, "seed-cell-"+filler(30, 7)), 10)
	f.Add(bytes.Repeat([]byte{0x7f}, 300), 0)
	f.Add(bytes.Repeat([]byte{0x00}, 300), 0)
	f.Add(bytes.Repeat([]byte{0xff}, 300), 3)
	f.Fuzz(func(t *testing.T, plant []byte, at int) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		tb.Insert(1, 10, "live-row-one-"+filler(30, 1))
		raw := b.Bytes()
		page := raw[1024 : 2*1024]
		if at < 0 {
			at = -at
		}
		at %= 1024
		copy(page[at:], plant)
		db, err := sqlitefile.Open(bytesAt{raw}, int64(len(raw)), sqlitefile.Options{})
		if err != nil {
			return
		}
		h := db.History()
		defer h.Release()
		var last int64 = -1
		_, err = carveSpans(context.Background(), db, h, func(r sqlitefile.RecoveredRow) bool {
			if r.Method != sqlitefile.MethodPageSlack || r.Loc.Cell != -1 || r.Relation != sqlitefile.RelUnknown {
				t.Fatalf("bad row %+v", r)
			}
			if r.Loc.Offset < r.Loc.PageOffset || r.Loc.Offset+r.Loc.Length > r.Loc.PageOffset+1024 {
				t.Fatalf("row outside its page: %+v", r.Loc)
			}
			if r.Loc.Page == 2 && r.Loc.Offset < last {
				t.Fatalf("rows out of order or overlapping: %d after %d", r.Loc.Offset, last)
			}
			if r.Loc.Page == 2 {
				last = r.Loc.Offset + r.Loc.Length
			}
			return true
		})
		_ = err
	})
}

func TestSlackCellOverwrittenByLaterCellIsNotDelivered(t *testing.T) {
	s := plainScene()
	off, _ := s.gap(t, 2)
	older := slackCell(12, 120, "older-cell-"+filler(40, 12))
	newer := slackCell(13, 130, "newer-cell-"+filler(40, 13))
	at := off + 10
	s.plant(2, at, older)
	s.plant(2, at+len(older)-3, newer) // written over the last 3 bytes of the older cell
	rows, st := s.carve(t)
	if len(rows) != 1 || *rows[0].Rowid != 13 {
		t.Fatalf("%d rows %v, want only the newer cell (rowid 13): the older one's tail is not its own", len(rows), rows)
	}
	if st.DroppedCrossed != 1 {
		t.Errorf("DroppedCrossed %d, want 1", st.DroppedCrossed)
	}
	if !slices.ContainsFunc(st.warnings, func(w string) bool { return strings.Contains(w, "page 2") && strings.Contains(w, "overwritten") }) {
		t.Errorf("warnings %q: the damaged cell is not reported", st.warnings)
	}
	if st.UnparsedBytes == 0 {
		t.Errorf("UnparsedBytes 0: the damaged cell's bytes are not counted")
	}
}
