package ioscalls

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
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

// fakeRow is a decoded row: the mappings see only the parse.Row interface, so their behaviour on any
// storage class in any column is fuzzed here without a database.
type fakeRow struct {
	table string
	cells []cell
}

func (r fakeRow) Table() string      { return r.table }
func (fakeRow) Rowid() (int64, bool) { return 1, true }
func (r fakeRow) IsNull(c int) bool  { return r.cells[c].kind == cNull }
func (r fakeRow) Int(c int) (int64, bool) {
	return r.cells[c].i, r.cells[c].kind == cInt
}

func (r fakeRow) Float(c int) (float64, bool) {
	return r.cells[c].f, r.cells[c].kind == cFloat
}

func (r fakeRow) Text(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cText
}

func (r fakeRow) Blob(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cBlob
}

func (fakeRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}

func (fakeRow) Locator() (string, bool) { return "row:rowid=1", true }

// fakeCtx knows the columns of one table by their spec order.
type fakeCtx struct {
	in    *parse.Input
	table string
	specs []parse.ColumnSpec
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (c fakeCtx) Column(table, col string) int {
	if table != c.table {
		return -1
	}
	for i, s := range c.specs {
		if s.Name == col {
			return i
		}
	}
	return -1
}
func (fakeCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

// layout is one mapping under test.
type layout struct {
	table string
	specs func() []parse.ColumnSpec
	mp    func(parse.MapContext, parse.Row) ([]records.Record, error)
	date  string // the column read as the call time
	read  string
	ints  []string // columns read as integers and stated in raw
	texts map[string]string
	dir   bool // the table decides direction and outcome
}

var (
	coreLayout = layout{
		table: tableCore, specs: coreSpecs, mp: mapCore, date: "ZDATE", read: "ZREAD",
		ints:  []string{"ZORIGINATED", "ZANSWERED", "ZCALLTYPE", "ZDISCONNECTED_CAUSE", "ZNUMBER_AVAILABILITY", "ZFACE_TIME_DATA", "ZJUNKCONFIDENCE"},
		texts: map[string]string{"ZISO_COUNTRY_CODE": "country_iso", "ZLOCATION": "geo_description", "ZNAME": "name_cached", "ZSERVICE_PROVIDER": "service"},
		dir:   true,
	}
	legacyLayout = layout{
		table: tableLegacy, specs: legacySpecs, mp: mapLegacy, date: "date", read: "read",
		ints:  []string{"flags", "id", "assisted"},
		texts: map[string]string{"country_code": "country_iso", "name": "name_cached"},
	}
)

func (l layout) colIndex(name string) int {
	for i, s := range l.specs() {
		if s.Name == name {
			return i
		}
	}
	panic("no column " + name)
}

func (l layout) mapCells(cells []cell) ([]records.Record, error) {
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/db"}}}}
	return l.mp(fakeCtx{in: in, table: l.table, specs: l.specs()}, fakeRow{table: l.table, cells: cells})
}

func isNumber(c cell) bool { return c.kind == cInt || c.kind == cFloat }

func numValue(c cell) float64 {
	if c.kind == cInt {
		return float64(c.i)
	}
	return c.f
}

// check holds a mapping to its contract on any row: no panic, and either a typed row error (a counted
// warning) or exactly one record that validates, carries its place, and invents nothing the cells do not say.
func (l layout) check(t testing.TB, cells []cell) {
	t.Helper()
	recs, err := l.mapCells(cells)
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("untyped error %v", err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	if r.Type != call.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := call.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	at := func(name string) cell { return cells[l.colIndex(name)] }
	rawMap, _ := r.Payload["raw"].(map[string]any)
	unknown, _ := rawMap["unknown_columns"].(map[string]any)
	flagged := func(name string, want string) {
		if unknown[name] != want {
			t.Errorf("%s of kind %d: unknown_columns[%s] = %v, want %q", name, at(name).kind, name, unknown[name], want)
		}
	}
	date := at(l.date)
	if !isNumber(date) && r.Time != nil {
		t.Errorf("a time from a date cell of kind %d", date.kind)
	}
	if k := date.kind; k == cText || k == cBlob {
		flagged(l.date, "type-mismatch")
	}
	durName := "duration"
	if l.table == tableCore {
		durName = "ZDURATION"
	}
	dur := at(durName)
	if k := dur.kind; k == cText || k == cBlob {
		flagged(durName, "type-mismatch")
	}
	if d, has := r.Payload["duration_s"]; has {
		if !isNumber(dur) || math.IsInf(numValue(dur), 0) || math.IsNaN(numValue(dur)) || numValue(dur) < 0 || d != numValue(dur) {
			t.Errorf("duration_s %v from duration cell %+v", d, dur)
		}
	}
	if dur.kind == cInt && (dur.i > 1<<53 || dur.i < -(1<<53)) && unknown[durName] != "precision" && rawMap[durName+"_status"] != "negative" {
		t.Errorf("duration %d beyond 2^53 is not flagged: %v", dur.i, rawMap)
	}
	if r.TimeEnd != nil {
		if r.Time == nil || !isNumber(dur) || !(numValue(dur) > 0 && numValue(dur) <= 1e9) {
			t.Errorf("an end time from duration cell %+v and time %v", dur, r.Time)
		}
	}
	if rawMap[durName+"_ieee"] != nil && (rawMap[durName] != nil || rawMap[durName+"_status"] != "invalid") {
		t.Errorf("a non-finite duration is stated as a value: %v", rawMap)
	}
	if rawMap[l.date+"_ieee"] != nil && (rawMap[l.date] != nil || rawMap[l.date+"_status"] != "invalid") {
		t.Errorf("a non-finite date is stated as a value: %v", rawMap)
	}
	if l.dir {
		orig, ans := at("ZORIGINATED"), at("ZANSWERED")
		if (orig.kind != cInt || (orig.i != 0 && orig.i != 1)) && r.Payload["direction"] != call.DirUnknown {
			t.Errorf("direction %v from an originated cell %+v", r.Payload["direction"], orig)
		}
		if (orig.kind != cInt || ans.kind != cInt) && r.Payload["outcome"] != call.OutcomeUnknown {
			t.Errorf("outcome %v from cells %+v %+v", r.Payload["outcome"], orig, ans)
		}
		if r.Payload["outcome"] == call.OutcomeCancelled {
			t.Errorf("an outcome 'cancelled' is never stated for a Core Data call (4E-R35)")
		}
		if ct := at("ZCALLTYPE"); ct.kind != cInt && r.Payload["kind"] != nil {
			t.Errorf("a kind from a call type cell %+v", ct)
		}
		if ad := at("ZADDRESS"); r.Payload["address"] != nil && ((ad.kind != cText && ad.kind != cBlob) || len(ad.b) == 0) {
			t.Errorf("an address from a cell %+v", ad)
		}
		if sp := at("ZUNIQUE_ID"); rawMap["ZUNIQUE_ID"] != nil && sp.kind != cText {
			t.Errorf("a raw ZUNIQUE_ID from a cell %+v", sp)
		}
	} else {
		if r.Payload["direction"] != call.DirUnknown || r.Payload["outcome"] != call.OutcomeUnknown || r.Payload["kind"] != nil || r.Payload["service"] != nil {
			t.Errorf("the legacy table decides nothing: %v", r.Payload)
		}
		if ad := at("address"); r.Payload["address"] != nil && ((ad.kind != cText && ad.kind != cBlob) || len(ad.b) == 0) {
			t.Errorf("an address from a cell %+v", ad)
		}
	}
	if v, has := r.Payload["read"]; has {
		if c := at(l.read); c.kind != cInt || (c.i != 0 && c.i != 1) || v != (c.i == 1) {
			t.Errorf("read %v from cell %+v", v, c)
		}
	}
	for _, n := range l.ints {
		c := at(n)
		if k := c.kind; k == cFloat || k == cText || k == cBlob {
			flagged(n, "type-mismatch")
		}
		if v, has := rawMap[n]; has && (c.kind != cInt || v != c.i) {
			t.Errorf("raw[%s] = %v from cell %+v", n, v, c)
		}
	}
	for col, key := range l.texts {
		c := at(col)
		if k := c.kind; k == cInt || k == cFloat || k == cBlob {
			flagged(col, "type-mismatch")
		}
		if _, has := r.Payload[key]; has && (c.kind != cText || len(c.b) == 0) {
			t.Errorf("%s from cell %+v", key, c)
		}
	}
}

func goodCells(l layout) []cell {
	specs := l.specs()
	cs := make([]cell, len(specs))
	for i, s := range specs {
		if s.Affinity == "integer" {
			cs[i] = cell{kind: cInt, i: 1}
		} else {
			cs[i] = cell{kind: cText, b: []byte("+15551234567")}
		}
	}
	if l.table == tableCore {
		cs[l.colIndex("ZDATE")] = cell{kind: cFloat, f: 700000000.5}
		cs[l.colIndex("ZDURATION")] = cell{kind: cFloat, f: 125}
		cs[l.colIndex("ZORIGINATED")] = cell{kind: cInt, i: 0}
	} else {
		cs[l.colIndex("date")] = cell{kind: cInt, i: 1682935200}
		cs[l.colIndex("duration")] = cell{kind: cInt, i: 125}
		cs[l.colIndex("face_time_data")] = cell{kind: cBlob, b: []byte{1}}
	}
	return cs
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes
// a column for the integer and float, texts is split at NUL bytes.
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

// The good rows map; every column of the row, and then the whole row, in every other storage class is
// contained: a typed row error or one valid record that states nothing the cells do not.
func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for _, l := range []layout{coreLayout, legacyLayout} {
		t.Run(l.table, func(t *testing.T) {
			recs, err := l.mapCells(goodCells(l))
			if err != nil || len(recs) != 1 || recs[0].Time == nil || recs[0].TimeEnd == nil {
				t.Fatalf("err %v, records %+v", err, recs)
			}
			if l.dir && (recs[0].Payload["direction"] != call.DirIn || recs[0].Payload["outcome"] != call.OutcomeAnswered) {
				t.Fatalf("good Core Data row: %v", recs[0].Payload)
			}
			l.check(t, goodCells(l))
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range goodCells(l) {
					cs := goodCells(l)
					cs[col] = cell{kind: k, i: 1 << 60, f: math.NaN(), b: []byte{0xff, 0xfe}}
					l.check(t, cs)
				}
				cs := goodCells(l)
				for i := range cs {
					cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
				}
				l.check(t, cs)
			}
			for _, v := range []int64{math.MinInt64, -(1 << 53) - 1, -1, 0, 1 << 53, 1<<53 + 1, math.MaxInt64} {
				cs := goodCells(l)
				for i := range cs {
					if cs[i].kind == cInt {
						cs[i].i = v
					}
				}
				l.check(t, cs)
			}
			for _, f := range []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1), -3, 1e300, 1e9, 1e9 + 1} {
				cs := goodCells(l)
				for i := range cs {
					if cs[i].kind == cFloat {
						cs[i].f = f
					}
				}
				for i, s := range l.specs() {
					if s.Name == l.date || s.Name == "ZDURATION" || s.Name == "duration" {
						cs[i] = cell{kind: cFloat, f: f}
					}
				}
				l.check(t, cs)
			}
		})
	}
}

// FuzzMapRows fuzzes both mappings on decoded column values directly: every column of the row takes any
// storage class and value.
func FuzzMapRows(f *testing.F) {
	f.Add(byte(0), []byte{cText, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("+1555\x00hi"))
	f.Add(byte(1), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(0), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(1), []byte{cText, cInt, cInt, cInt, cInt, cText, cText, cText, cFloat, cText, cInt, cInt}, []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x7f}, []byte("5\x005\x00x"))
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts []byte) {
		l := coreLayout
		if which%2 == 1 {
			l = legacyLayout
		}
		l.check(t, cellsFrom(len(l.specs()), kinds, nums, texts))
	})
}
