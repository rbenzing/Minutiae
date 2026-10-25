package safari

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// Cell storage classes of the fuzzed rows.
const (
	cNull byte = iota
	cInt
	cFloat
	cText
	cBlob
	numKinds
)

type cell struct {
	kind byte
	i    int64
	f    float64
	b    []byte
}

// fuzzRow is a decoded row of any table: the mappings see only the parse.Row interface, so their behaviour
// on any storage class in any column is fuzzed here without a database.
type fuzzRow struct {
	table string
	cells []cell
}

func (r fuzzRow) Table() string      { return r.table }
func (fuzzRow) Rowid() (int64, bool) { return 1, true }
func (r fuzzRow) IsNull(c int) bool  { return r.cells[c].kind == cNull }
func (r fuzzRow) Int(c int) (int64, bool) {
	return r.cells[c].i, r.cells[c].kind == cInt
}

func (r fuzzRow) Float(c int) (float64, bool) {
	return r.cells[c].f, r.cells[c].kind == cFloat
}

func (r fuzzRow) Text(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cText
}

func (r fuzzRow) Blob(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cBlob
}

func (fuzzRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}

func (r fuzzRow) Locator() (string, bool) { return "sqlite:table=" + r.table + ";rowid=1", true }

// Join answers of the fake context.
const (
	joinFound   = iota // Get finds the item; Match finds no visit
	joinHit            // Get finds the item; Match finds a visit
	joinMissing        // Get: a clean miss; Match: an undecidable key
	joinError          // any other error
	joinBudget         // the budget error
	numJoinModes
)

// fakeCtx knows the columns of the three tables by the order of their specs. Its joins answer by mode; the
// item a visit names is item.
type fakeCtx struct {
	in   *parse.Input
	mode int
	item fuzzRow
}

func tombstoneColumns() []string { return []string{"start_time", "end_time", "url", "generation"} }

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}

func (fakeCtx) Column(table, col string) int {
	var names []string
	switch table {
	case "history_items":
		names = itemColumns()
	case "history_visits":
		names = visitNames()
	case "history_tombstones":
		names = tombstoneColumns()
	}
	for i, n := range names {
		if n == col {
			return i
		}
	}
	return -1
}

func (c fakeCtx) Get(string, int64) (parse.Row, bool, error) {
	switch c.mode {
	case joinMissing:
		return nil, false, nil
	case joinError:
		return nil, false, errors.New("corrupt page")
	case joinBudget:
		return nil, false, parse.ErrBudget
	}
	return c.item, true, nil
}

func (c fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) {
	switch c.mode {
	case joinHit:
		return []parse.Row{fuzzRow{table: "history_visits", cells: make([]cell, len(visitNames()))}}, nil
	case joinMissing:
		return nil, sqlitedb.ErrKeyUndecidable
	case joinError:
		return nil, errors.New("index failed")
	case joinBudget:
		return nil, parse.ErrBudget
	}
	return nil, nil
}

// Column positions, in the order of the specs.
const (
	colItemURL = 1
	colItemVC  = 2
	colItemSC  = 3

	colVisItem  = 0
	colVisTime  = 1
	colVisTitle = 2
	colVisBool  = 3

	colTombStart = 0
	colTombEnd   = 1
	colTombURL   = 2
)

func goodItem() fuzzRow {
	return fuzzRow{table: "history_items", cells: []cell{{kind: cInt, i: 1}, {kind: cText, b: []byte("https://a.example/")}, {kind: cInt, i: 3}, {kind: cInt, i: 200}}}
}

func goodVisit() fuzzRow {
	cs := make([]cell, len(visitNames()))
	for i := range cs {
		cs[i] = cell{kind: cInt}
	}
	cs[colVisItem] = cell{kind: cInt, i: 1}
	cs[colVisTime] = cell{kind: cFloat, f: 704628000}
	cs[colVisTitle] = cell{kind: cText, b: []byte("Title")}
	cs[colVisBool] = cell{kind: cInt, i: 1}
	return fuzzRow{table: "history_visits", cells: cs}
}

func goodTombstone() fuzzRow {
	return fuzzRow{table: "history_tombstones", cells: []cell{{kind: cFloat, f: 704627000}, {kind: cFloat, f: 704628000}, {kind: cText, b: []byte("https://gone.example/")}, {kind: cInt, i: 2}}}
}

func inputOf() *parse.Input {
	return &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/History.db"}}}}
}

func mapRow(row, item fuzzRow, mode int) ([]records.Record, error) {
	m := fakeCtx{in: inputOf(), mode: mode, item: item}
	switch row.table {
	case "history_items":
		return mapItem(m, row)
	case "history_visits":
		return mapVisit(m, row)
	}
	return mapTombstone(m, row, nil)
}

// joined reports whether the mapping of row reaches its join: a visit needs an integer history_item, an item
// an integer id (without one the visits join is undecided before it is asked), a tombstone has no join.
func joined(row fuzzRow) bool {
	switch row.table {
	case "history_items":
		return row.cells[0].kind == cInt
	case "history_visits":
		return row.cells[colVisItem].kind == cInt
	}
	return false
}

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed error (a RowError, or
// the budget error returned to the runner), no record by design, or records that validate, carry their
// place, are never recovered, and state no URL, title, time, count or status that the cells do not say.
func checkMapped(t testing.TB, row, item fuzzRow, mode int) {
	t.Helper()
	recs, err := mapRow(row, item, mode)
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) && !errors.Is(err, parse.ErrBudget) {
			t.Fatalf("untyped error %v", err)
		}
		if mode == joinBudget && joined(row) && !errors.Is(err, parse.ErrBudget) {
			t.Fatalf("a budget error of a join was not returned: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("error %v with %d records", err, len(recs))
		}
		return
	}
	if len(recs) > 1 {
		t.Fatalf("%d records", len(recs))
	}
	urlOf := func(c cell) string { return common.CleanText(c.b, maxURL).Text }
	usable := func(c cell) bool { return c.kind == cText && len(c.b) > 0 }
	for _, r := range recs {
		if r.Type != web.VisitType || r.ArtifactID != "a1" || r.SourcePath != "/p/History.db" || r.Locator != "sqlite:table="+row.table+";rowid=1" || r.Range == nil {
			t.Fatalf("record lost its place: %+v", r)
		}
		if err := web.ValidateVisit(r.Payload); err != nil {
			t.Fatalf("payload refused: %v", err)
		}
		if _, err := json.Marshal(r.Payload); err != nil {
			t.Fatalf("payload does not marshal: %v", err)
		}
		if r.Recovery != "" || r.Confidence != nil || r.TimeEnd != nil {
			t.Fatalf("a recovery fact on a live row: %+v", r)
		}
		rawMap, _ := r.Payload["raw"].(map[string]any)
		switch row.table {
		case "history_items":
			c := row.cells[colItemURL]
			if r.Deleted || r.Time != nil || r.Times != nil || !usable(c) || r.Payload["url"] != urlOf(c) || r.Payload["visit_id"] != nil {
				t.Errorf("item record %+v from url cell %+v", r, c)
			}
			decided := mode == joinFound && joined(row) // the visits join ran and found nothing
			if _, visitless := rawMap["visitless"]; visitless != decided || (rawMap["join_undecided"] != nil) == decided {
				t.Errorf("mode %d: raw %v", mode, rawMap)
			}
			checkItemFacts(t, r.Payload, row.cells[colItemVC], row.cells[colItemSC])
		case "history_visits":
			c := item.cells[colItemURL]
			if r.Deleted || !usable(c) || r.Payload["url"] != urlOf(c) || r.Payload["visit_id"] != "1" {
				t.Errorf("visit record %+v from item url cell %+v", r, c)
			}
			if tc := row.cells[colVisTitle]; tc.kind != cText {
				if _, has := r.Payload["title"]; has {
					t.Errorf("a title from a cell of kind %d", tc.kind)
				}
			} else if title, has := r.Payload["title"]; has && title != common.CleanText(tc.b, maxText).Text {
				t.Errorf("title %v from %q", title, tc.b)
			}
			if tm := row.cells[colVisTime]; r.Time != nil && (tm.kind != cFloat || math.IsNaN(tm.f) || math.IsInf(tm.f, 0)) {
				t.Errorf("a time from a visit_time cell of kind %d (%v)", tm.kind, tm.f)
			}
			lc := row.cells[colVisBool]
			if b, has := r.Payload["load_successful"]; has && (lc.kind != cInt || (lc.i != 0 && lc.i != 1) || b != (lc.i == 1)) {
				t.Errorf("load_successful %v from %+v", b, lc)
			}
			checkItemFacts(t, r.Payload, item.cells[colItemVC], item.cells[colItemSC])
		default:
			c := row.cells[colTombURL]
			if !r.Deleted || r.Time != nil || !usable(c) || r.Payload["url"] != urlOf(c) {
				t.Errorf("tombstone record %+v from url cell %+v", r, c)
			}
			if d, _ := r.Payload["deleted"].(map[string]any); d["source"] != "history_tombstones" {
				t.Errorf("tombstone deleted fact %v", r.Payload["deleted"])
			}
			for _, nt := range r.Times {
				col := colTombStart
				switch nt.Kind {
				case "deleted_range_start":
				case "deleted_range_end":
					col = colTombEnd
				default:
					t.Errorf("Times kind %q", nt.Kind)
				}
				if c := row.cells[col]; c.kind != cFloat || math.IsNaN(c.f) || math.IsInf(c.f, 0) {
					t.Errorf("a %s from a cell of kind %d (%v)", nt.Kind, c.kind, c.f)
				}
			}
		}
	}
	if len(recs) == 0 {
		switch {
		case row.table == "history_visits":
			t.Fatalf("a visit mapped to no record and no error")
		case row.table == "history_items" && (mode != joinHit || !joined(row)) && usable(row.cells[colItemURL]):
			t.Fatalf("an item with a usable url and no visit that is known to exist mapped to nothing (mode %d)", mode)
		case row.table == "history_tombstones" && usable(row.cells[colTombURL]):
			t.Fatalf("a tombstone with a usable url mapped to nothing")
		}
	}
}

// checkItemFacts holds url_visit_count and status_code to the item's cells.
func checkItemFacts(t testing.TB, payload map[string]any, vc, sc cell) {
	t.Helper()
	if n, has := payload["url_visit_count"]; has && (vc.kind != cInt || vc.i < 0 || n != vc.i) {
		t.Errorf("url_visit_count %v from %+v", n, vc)
	}
	if s, has := payload["status_code"]; has && (sc.kind != cInt || sc.i == 0 || s != sc.i) {
		t.Errorf("status_code %v from %+v", s, sc)
	}
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes
// a column for the integer and float (any bit pattern, so NaN and the infinities occur), texts is split at
// NUL bytes.
func cellsFrom(n int, kinds, nums, texts []byte) []cell {
	var parts [][]byte
	start := 0
	for i, b := range texts {
		if b == 0 {
			parts = append(parts, texts[start:i])
			start = i + 1
		}
	}
	parts = append(parts, texts[start:])
	cs := make([]cell, n)
	for i := range cs {
		if len(kinds) > 0 {
			cs[i].kind = kinds[i%len(kinds)] % numKinds
		}
		var w [8]byte
		if len(nums) > 0 {
			for j := range w {
				w[j] = nums[(i*8+j)%len(nums)]
			}
		}
		u := binary.LittleEndian.Uint64(w[:])
		cs[i].i, cs[i].f = int64(u), math.Float64frombits(u)
		cs[i].b = parts[i%len(parts)]
	}
	return cs
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for _, row := range []fuzzRow{goodItem(), goodVisit(), goodTombstone()} {
		if recs, err := mapRow(row, goodItem(), joinFound); err != nil || len(recs) != 1 {
			t.Fatalf("%s: err %v, records %+v", row.table, err, recs)
		}
	}
	tomb, _ := mapRow(goodTombstone(), fuzzRow{}, joinFound)
	if len(tomb[0].Times) != 2 || !tomb[0].Deleted || tomb[0].Time != nil {
		t.Errorf("good tombstone: %+v", tomb[0])
	}
	visit, _ := mapRow(goodVisit(), goodItem(), joinFound)
	if visit[0].Time == nil || visit[0].Payload["url_visit_count"] != int64(3) || visit[0].Payload["status_code"] != int64(200) || visit[0].Payload["title"] != "Title" {
		t.Errorf("good visit: %+v", visit[0])
	}
	for mode := range numJoinModes {
		for _, row := range []fuzzRow{goodItem(), goodVisit(), goodTombstone()} {
			checkMapped(t, row, goodItem(), mode)
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range row.cells {
					r := fuzzRow{table: row.table, cells: append([]cell(nil), row.cells...)}
					r.cells[col] = cell{kind: k, i: math.MinInt64, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
					checkMapped(t, r, goodItem(), mode)
					it := goodItem()
					it.cells[col%len(it.cells)] = cell{kind: k, i: -1, f: math.Inf(1), b: []byte{0xff}}
					checkMapped(t, row, it, mode)
				}
			}
		}
	}
}

// FuzzMapSafariRows fuzzes the three mappings on decoded column values directly: every column of the row
// (and of the item a visit names) takes any storage class and value, REAL columns any bit pattern, and the
// joins answer by mode.
func FuzzMapSafariRows(f *testing.F) {
	nan := []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}
	inf := []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x7f}
	f.Add(byte(0), byte(0), []byte{cInt, cText, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("https://a.example/\x00t"))
	f.Add(byte(1), byte(0), []byte{cInt, cFloat, cText, cInt, cNull}, nan, []byte("\xff\xfe\x00 12"))
	f.Add(byte(2), byte(0), []byte{cFloat, cFloat, cText, cInt}, inf, []byte("https://gone.example/"))
	f.Add(byte(1), byte(2), []byte{cInt, cFloat, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("u"))
	f.Add(byte(0), byte(1), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, which, mode byte, kinds, nums, texts []byte) {
		table := [3]string{"history_items", "history_visits", "history_tombstones"}[which%3]
		n := len(tombstoneColumns())
		switch table {
		case "history_items":
			n = len(itemColumns())
		case "history_visits":
			n = len(visitNames())
		}
		rev := make([]byte, len(kinds))
		for i, k := range kinds {
			rev[len(kinds)-1-i] = k
		}
		item := fuzzRow{table: "history_items", cells: cellsFrom(len(itemColumns()), rev, nums, texts)}
		if mode&0x80 != 0 {
			item.cells[0] = cell{kind: cInt, i: 1} // the item's id is the key the visit names
		}
		row := fuzzRow{table: table, cells: cellsFrom(n, kinds, nums, texts)}
		if table == "history_visits" && mode&0x40 != 0 {
			row.cells[colVisItem] = cell{kind: cInt, i: 1}
		}
		checkMapped(t, row, item, int(mode&0x3f)%numJoinModes)
	})
}
