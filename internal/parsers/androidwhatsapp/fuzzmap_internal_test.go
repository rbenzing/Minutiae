package androidwhatsapp

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
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

// fakeRow is a decoded row: a mapping sees only the parse.Row interface, so its behaviour on any storage class in any
// column is fuzzed here without a database.
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

func (r fakeRow) Locator() (string, bool) { return "sqlite:table=" + r.table + ";rowid=1", true }

// fakeCtx knows the columns of one table by their spec order. Every other table is missing, so every join is "no row".
type fakeCtx struct {
	in *parse.Input
	mp parse.TableMapping
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (c fakeCtx) Column(table, col string) int {
	if table != c.mp.Table {
		return -1
	}
	for i, s := range c.mp.Columns {
		if s.Name == col {
			return i
		}
	}
	return -1
}
func (fakeCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

func fakeInput() *parse.Input {
	const remote = "/data/user/10/com.whatsapp/databases/msgstore.db"
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: "android:" + remote, Source: parse.SourceInfo{RemotePath: remote}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: remote}}},
	}
}

// mappingsUnderTest are the four mappings of the parser.
func mappingsUnderTest() []parse.TableMapping {
	return Parser{}.Mappings()
}

func intCell(mp parse.TableMapping, cells []cell, name string) (cell, bool) {
	for i, s := range mp.Columns {
		if s.Name == name {
			return cells[i], true
		}
	}
	return cell{}, false
}

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed row error (a counted warning) or
// at most one record that validates, carries its place, and invents nothing the cells do not say.
func checkMapped(t testing.TB, mp parse.TableMapping, cells []cell) {
	t.Helper()
	recs, err := mp.Map(fakeCtx{in: fakeInput(), mp: mp}, fakeRow{table: mp.Table, cells: cells})
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("%s: untyped error %v", mp.Table, err)
		}
		return
	}
	if len(recs) > 1 {
		t.Fatalf("%s: %d records", mp.Table, len(recs))
	}
	for _, r := range recs {
		if r.ArtifactID != "a1" || r.SourcePath != "/data/user/10/com.whatsapp/databases/msgstore.db" || r.Range == nil || r.Locator != "sqlite:table="+mp.Table+";rowid=1" {
			t.Fatalf("%s: record lost its place: %+v", mp.Table, r)
		}
		var verr error
		switch r.Type {
		case message.Type:
			verr = message.Validate(r.Payload)
		case call.Type:
			verr = call.Validate(r.Payload)
		default:
			t.Fatalf("%s: a record of type %q", mp.Table, r.Type)
		}
		if verr != nil {
			t.Fatalf("%s: payload refused: %v", mp.Table, verr)
		}
		if r.Recovery != "" || r.Confidence != nil {
			t.Errorf("%s: the mapping stamped a recovery: %+v", mp.Table, r)
		}
		// a time only from an INTEGER timestamp cell
		if c, ok := intCell(mp, cells, "timestamp"); ok && r.Time != nil && c.kind != cInt {
			t.Errorf("%s: a time from a timestamp cell of kind %d", mp.Table, c.kind)
		}
		// direction only from an INTEGER 0 or 1 of the direction column (a reaction's is unknown whatever it holds)
		dirCol := map[string]string{tblMessage: "from_me", tblMessages: "key_from_me", tblCallLog: "from_me"}[mp.Table]
		if c, ok := intCell(mp, cells, dirCol); ok && r.Payload["direction"] != nil && r.Payload["direction"] != "unknown" && (c.kind != cInt || (c.i != 0 && c.i != 1)) {
			t.Errorf("%s: direction %v from a cell %+v", mp.Table, r.Payload["direction"], c)
		}
		if mp.Table == tblAddOn && r.Payload["direction"] != "unknown" {
			t.Errorf("a reaction with direction %v (4I-R20)", r.Payload["direction"])
		}
	}
}

func goodCells(mp parse.TableMapping) []cell {
	cs := make([]cell, len(mp.Columns))
	for i, s := range mp.Columns {
		if s.Affinity == "integer" {
			cs[i] = cell{kind: cInt, i: 1}
		} else {
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
	}
	if c, ok := intCell(mp, cs, "timestamp"); ok {
		_ = c
		for i, s := range mp.Columns {
			if s.Name == "timestamp" {
				cs[i] = cell{kind: cInt, i: 1700000000000}
			}
		}
	}
	return cs
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column for
// the integer and float, texts is split at NUL bytes.
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
	for _, mp := range mappingsUnderTest() {
		t.Run(mp.Table, func(t *testing.T) {
			if recs, err := mp.Map(fakeCtx{in: fakeInput(), mp: mp}, fakeRow{table: mp.Table, cells: goodCells(mp)}); err != nil && mp.Table != tblMessages {
				t.Fatalf("a good row did not map: %v %v", err, recs)
			}
			checkMapped(t, mp, goodCells(mp))
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range goodCells(mp) {
					cs := goodCells(mp)
					cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
					checkMapped(t, mp, cs)
				}
				cs := goodCells(mp)
				for i := range cs {
					cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
				}
				checkMapped(t, mp, cs)
			}
			for _, v := range []int64{math.MinInt64, -1, 0, 1, 5, 56, 1 << 53, 1<<53 + 1, math.MaxInt64} {
				cs := goodCells(mp)
				for i := range cs {
					if cs[i].kind == cInt {
						cs[i].i = v
					}
				}
				checkMapped(t, mp, cs)
			}
		})
	}
}

// FuzzMapAndroidWhatsAppRows fuzzes the four mappings on decoded column values directly: every column of the row takes
// any storage class and value.
func FuzzMapAndroidWhatsAppRows(f *testing.F) {
	f.Add(byte(0), []byte{cText, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("K1\x00hello"))
	f.Add(byte(1), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(2), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(3), []byte{cInt, cInt, cInt, cInt, cInt, cInt, cText, cText}, []byte{0, 0, 0, 0, 0, 0, 0x20, 0}, []byte("5\x005\x00x"))
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts []byte) {
		ms := mappingsUnderTest()
		mp := ms[int(which)%len(ms)]
		checkMapped(t, mp, cellsFrom(len(mp.Columns), kinds, nums, texts))
	})
}
