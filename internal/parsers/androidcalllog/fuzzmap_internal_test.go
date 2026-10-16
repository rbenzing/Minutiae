package androidcalllog

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

// fakeRow is a decoded row: the mapping sees only the parse.Row interface, so its
// behaviour on any storage class in any column is fuzzed here without a database.
type fakeRow struct{ cells []cell }

func (fakeRow) Table() string        { return "calls" }
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

// fakeCtx knows the columns of the calls table by their spec order.
type fakeCtx struct{ in *parse.Input }

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (fakeCtx) Column(table, col string) int {
	if table != "calls" {
		return -1
	}
	for i, s := range callSpecs() {
		if s.Name == col {
			return i
		}
	}
	return -1
}
func (fakeCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

// Column indexes, in callSpecs order.
const (
	colNumber = iota
	colDate
	colDuration
	colType
	colPresentation
	colName
	colCountry
	colGeo
	colSubID
	colFeatures
	colIsRead
	colNew
)

func mapCells(cells []cell) ([]records.Record, error) {
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/calllog.db"}}}}
	return mapCall(fakeCtx{in: in}, fakeRow{cells: cells})
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed
// row error (a counted warning) or exactly one record that validates, carries its place,
// and invents nothing the cells do not say.
func checkMapped(t testing.TB, cells []cell) {
	t.Helper()
	recs, err := mapCells(cells)
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
	if r.Type != call.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/calllog.db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := call.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	isInt := func(c int) bool { return cells[c].kind == cInt }
	rawMap, _ := r.Payload["raw"].(map[string]any)
	unknown, _ := rawMap["unknown_columns"].(map[string]any)
	if !isInt(colDate) && r.Time != nil {
		t.Errorf("a time from a date cell of kind %d", cells[colDate].kind)
	}
	if r.TimeEnd != nil && (r.Time == nil || !isInt(colDuration) || cells[colDuration].i <= 0 || cells[colDuration].i > maxDuration) {
		t.Errorf("an end time from duration cell %+v", cells[colDuration])
	}
	if !isInt(colType) && (r.Payload["direction"] != call.DirUnknown || r.Payload["outcome"] != call.OutcomeUnknown) {
		t.Errorf("direction %v outcome %v from a type cell of kind %d", r.Payload["direction"], r.Payload["outcome"], cells[colType].kind)
	}
	if _, has := r.Payload["address"]; has && (cells[colNumber].kind != cText || len(cells[colNumber].b) == 0) {
		t.Errorf("an address from a number cell of kind %d", cells[colNumber].kind)
	}
	if _, has := r.Payload["address_presentation"]; has && !isInt(colPresentation) {
		t.Errorf("a presentation from a cell of kind %d", cells[colPresentation].kind)
	}
	if _, has := r.Payload["kind"]; has && !isInt(colFeatures) {
		t.Errorf("a kind from a features cell of kind %d", cells[colFeatures].kind)
	}
	if d, has := r.Payload["duration_s"]; has {
		c := cells[colDuration]
		if !isInt(colDuration) || c.i < 0 || c.i > maxExact || d != float64(c.i) {
			t.Errorf("duration_s %v from duration cell %+v", d, c)
		}
	}
	if d := cells[colDuration].i; isInt(colDuration) && (d > maxExact || d < -maxExact) && unknown["duration"] != "precision" {
		t.Errorf("duration %d beyond 2^53 is not flagged: %v", d, unknown)
	}
	sid := cells[colSubID]
	if s, has := r.Payload["subscription"]; has {
		if (sid.kind != cInt && sid.kind != cText) || (sid.kind == cInt && (sid.i < 0 || s != sid.i)) {
			t.Errorf("subscription %v from sub id cell %+v", s, sid)
		}
	}
	if (sid.kind == cFloat || sid.kind == cBlob) && unknown["subscription_id"] != "type-mismatch" {
		t.Errorf("a subscription_id of kind %d is not flagged: %v", sid.kind, unknown)
	}
	for col, key := range map[int]string{colIsRead: "read", colNew: "new"} {
		if v, has := r.Payload[key]; has && (!isInt(col) || (cells[col].i != 0 && cells[col].i != 1)) {
			t.Errorf("%s %v from cell %+v", key, v, cells[col])
		}
	}
	// a cell of a storage class the mapping does not read is named, never silent
	for col, key := range map[int]string{colDate: "date", colDuration: "duration", colType: "type", colPresentation: "presentation", colFeatures: "features"} {
		if k := cells[col].kind; (k == cFloat || k == cText || k == cBlob) && unknown[key] != "type-mismatch" {
			t.Errorf("%s of kind %d is not flagged: %v", key, k, unknown)
		}
	}
}

func goodCells() []cell {
	cs := make([]cell, len(callSpecs()))
	for i, s := range callSpecs() {
		if s.Affinity == "integer" {
			cs[i] = cell{kind: cInt, i: 1}
		} else {
			cs[i] = cell{kind: cText, b: []byte("+15551234567")}
		}
	}
	cs[colDate] = cell{kind: cInt, i: 1682935200000}
	cs[colDuration] = cell{kind: cInt, i: 125}
	return cs
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class,
// nums supplies 8 bytes a column for the integer and float, texts is split at NUL bytes.
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
	recs, err := mapCells(goodCells())
	if err != nil || len(recs) != 1 || recs[0].Time == nil || recs[0].TimeEnd == nil || recs[0].Payload["direction"] != call.DirIn {
		t.Fatalf("err %v, records %+v", err, recs)
	}
	checkMapped(t, goodCells())
	for _, k := range []byte{cNull, cFloat, cText, cBlob} {
		for col := range goodCells() {
			cs := goodCells()
			cs[col] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
			checkMapped(t, cs)
		}
		cs := goodCells()
		for i := range cs {
			cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
		}
		checkMapped(t, cs)
	}
	for _, v := range []int64{math.MinInt64, -(1 << 53) - 1, -1, 0, 1 << 53, 1<<53 + 1, math.MaxInt64} {
		cs := goodCells()
		for i := range cs {
			if cs[i].kind == cInt {
				cs[i].i = v
			}
		}
		checkMapped(t, cs)
	}
}

// FuzzMapRows fuzzes mapCall on decoded column values directly: every column of the row
// takes any storage class and value.
func FuzzMapRows(f *testing.F) {
	f.Add([]byte{cText, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("+1555\x00hi"))
	f.Add([]byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add([]byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add([]byte{cText, cInt, cInt, cInt, cInt, cText, cText, cText, cFloat, cText, cInt, cInt}, []byte{0, 0, 0, 0, 0, 0, 0x20, 0}, []byte("5\x005\x00x"))
	f.Fuzz(func(t *testing.T, kinds, nums, texts []byte) {
		checkMapped(t, cellsFrom(len(callSpecs()), kinds, nums, texts))
	})
}
