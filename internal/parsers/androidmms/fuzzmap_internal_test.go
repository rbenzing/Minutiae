package androidmms

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
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
type fakeRow struct {
	table string
	cells []cell
}

func (r fakeRow) Table() string        { return r.table }
func (r fakeRow) Rowid() (int64, bool) { return 1, true }
func (r fakeRow) IsNull(c int) bool    { return r.cells[c].kind == cNull }
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
func (r fakeRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}
func (r fakeRow) Locator() (string, bool) { return "row:rowid=1", true }

// fakeCtx knows the columns of one table by their spec order; every other table is
// present and empty.
type fakeCtx struct {
	table string
	specs []parse.ColumnSpec
	in    *parse.Input
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

// mapCells maps one decoded row of sms or pdu.
func mapCells(pdu bool, cells []cell) ([]records.Record, error) {
	table, specs, fn := "sms", smsSpecs(), mapSMS
	if pdu {
		table, specs, fn = "pdu", pduSpecs(), mapPDU
	}
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/mmssms.db"}}}}
	return fn(fakeCtx{table: table, specs: specs, in: in}, fakeRow{table: table, cells: cells})
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed
// row error (a counted warning) or exactly one record that validates, carries its place,
// and invents nothing the cells do not say.
func checkMapped(t testing.TB, pdu bool, cells []cell) {
	t.Helper()
	recs, err := mapCells(pdu, cells)
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
	if r.Type != message.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/mmssms.db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := message.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	dateCol, typeCol := 1, 2 // sms
	if pdu {
		dateCol, typeCol = 0, 1
	}
	isInt := func(c int) bool { return c < len(cells) && cells[c].kind == cInt }
	if !isInt(dateCol) && r.Time != nil {
		t.Errorf("a time from a date cell of kind %d", cells[dateCol].kind)
	}
	if !isInt(typeCol) && r.Payload["direction"] != message.DirUnknown {
		t.Errorf("direction %v from a type cell of kind %d", r.Payload["direction"], cells[typeCol].kind)
	}
	if !pdu {
		if a := cells[0]; (a.kind != cText || len(a.b) == 0) && len(r.Payload["participants"].([]any)) != 0 {
			t.Errorf("a participant from an address cell of kind %d", a.kind)
		}
	}
	subCol := 12 // sub_id, in sms and pdu alike
	if _, has := r.Payload["subscription"]; has && (!isInt(subCol) || cells[subCol].i < 0) {
		t.Errorf("a subscription from sub_id cell %+v", cells[subCol])
	}
	if rd, has := r.Payload["read"]; has {
		col := 6
		if pdu {
			col = 4
		}
		if !isInt(col) || (cells[col].i != 0 && cells[col].i != 1) {
			t.Errorf("read %v from cell %+v", rd, cells[col])
		}
	}
}

func goodCells(pdu bool) []cell {
	specs := smsSpecs()
	if pdu {
		specs = pduSpecs()
	}
	cs := make([]cell, len(specs))
	for i, s := range specs {
		if s.Affinity == "integer" {
			cs[i] = cell{kind: cInt, i: 1}
		} else {
			cs[i] = cell{kind: cText, b: []byte("+15551234567")}
		}
	}
	if pdu {
		cs[0] = cell{kind: cInt, i: 1682935200}
	} else {
		cs[1] = cell{kind: cInt, i: 1682935200000}
	}
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
	for _, pdu := range []bool{false, true} {
		recs, err := mapCells(pdu, goodCells(pdu))
		if err != nil || len(recs) != 1 || recs[0].Time == nil || recs[0].Payload["direction"] != message.DirIn {
			t.Fatalf("pdu=%v: err %v, records %+v", pdu, err, recs)
		}
		checkMapped(t, pdu, goodCells(pdu))
		for _, k := range []byte{cNull, cFloat, cText, cBlob} {
			cs := goodCells(pdu)
			for i := range cs {
				cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
			}
			checkMapped(t, pdu, cs)
		}
		for _, v := range []int64{math.MinInt64, -1, 0, math.MaxInt64} {
			cs := goodCells(pdu)
			for i := range cs {
				if cs[i].kind == cInt {
					cs[i].i = v
				}
			}
			checkMapped(t, pdu, cs)
		}
	}
}

// FuzzMapRows fuzzes mapSMS and mapPDU on decoded column values directly: every column of
// the row takes any storage class and value.
func FuzzMapRows(f *testing.F) {
	f.Add(false, []byte{cText, cInt, cInt, cText}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("+1555\x00hi"))
	f.Add(true, []byte{cInt, cInt, cNull, cFloat, cBlob}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00text/plain"))
	f.Add(false, []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, pdu bool, kinds, nums, texts []byte) {
		n := len(smsSpecs())
		if pdu {
			n = len(pduSpecs())
		}
		checkMapped(t, pdu, cellsFrom(n, kinds, nums, texts))
	})
}
