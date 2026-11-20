package androidcalendar

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
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

// fakeCtx knows the Events columns by their spec order. Every other table is missing, so every join of the live
// mapping is undecided (a table the database lacks).
type fakeCtx struct{ in *parse.Input }

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (fakeCtx) Column(table, col string) int {
	if table != tableEvents {
		return -1
	}
	for i, s := range eventSpecs() {
		if s.Name == col {
			return i
		}
	}
	return -1
}
func (fakeCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

const fakeRemote = "/data/data/com.android.providers.calendar/databases/calendar.db"

func fakeInput() *parse.Input {
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: "android:" + fakeRemote, Source: parse.SourceInfo{RemotePath: fakeRemote}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: fakeRemote}}},
	}
}

// mappingsUnderTest are the typed pass's mapping (no env: a recovered row joins nothing) and the live one (an empty
// guard: every join reaches the fake context, whose tables are missing, or no row).
func mappingsUnderTest() []parse.TableMapping {
	return []parse.TableMapping{Parser{}.Mappings()[0], eventMappingWith(&env{})}
}

func cellOf(cells []cell, name string) (cell, bool) {
	for i, s := range eventSpecs() {
		if s.Name == name {
			return cells[i], true
		}
	}
	return cell{}, false
}

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed row error or exactly one record
// that validates, carries its place, stamps no recovery, and invents nothing the cells do not say.
func checkMapped(t testing.TB, mp parse.TableMapping, cells []cell) {
	t.Helper()
	recs, err := mp.Map(fakeCtx{in: fakeInput()}, fakeRow{table: tableEvents, cells: cells})
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("untyped error %v", err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%d records: every Events row is one record (spec 10)", len(recs))
	}
	r := recs[0]
	if r.ArtifactID != "a1" || r.SourcePath != fakeRemote || r.Range == nil || r.Locator != "sqlite:table=Events;rowid=1" {
		t.Fatalf("record lost its place: %+v", r)
	}
	if r.Type != calendar.Type {
		t.Fatalf("a record of type %q", r.Type)
	}
	if err := calendar.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	if r.Recovery != "" || r.Confidence != nil {
		t.Errorf("the mapping stamped a recovery: %+v", r)
	}
	// a start only from an INTEGER dtstart cell
	if c, _ := cellOf(cells, "dtstart"); r.Time != nil && c.kind != cInt {
		t.Errorf("a start from a dtstart cell of kind %d", c.kind)
	}
	// Deleted only from an INTEGER 1 of Events.deleted or Events.lastSynced (the fake context joins no calendar)
	del, _ := cellOf(cells, "deleted")
	ls, _ := cellOf(cells, "lastSynced")
	if r.Deleted && (del.kind != cInt || del.i != 1) && (ls.kind != cInt || ls.i != 1) {
		t.Errorf("Deleted from deleted %+v, lastSynced %+v", del, ls)
	}
	// a decided status only from an INTEGER eventStatus cell
	if c, _ := cellOf(cells, "eventStatus"); r.Payload["status"] != nil && r.Payload["status"] != "unknown" && c.kind != cInt {
		t.Errorf("status %v from an eventStatus cell of kind %d", r.Payload["status"], c.kind)
	}
}

func goodCells() []cell {
	specs := eventSpecs()
	cs := make([]cell, len(specs))
	for i, s := range specs {
		switch s.Affinity {
		case "integer":
			cs[i] = cell{kind: cInt, i: 0}
		default:
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
		switch s.Name {
		case "_id":
			cs[i] = cell{kind: cNull}
		case "calendar_id":
			cs[i].i = 1
		case "dtstart":
			cs[i].i = 1700000000000
		case "dtend":
			cs[i].i = 1700003600000
		case "eventTimezone":
			cs[i].b = []byte("Europe/Berlin")
		case "duration":
			cs[i] = cell{kind: cNull}
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
	for i, mp := range mappingsUnderTest() {
		recs, err := mp.Map(fakeCtx{in: fakeInput()}, fakeRow{table: tableEvents, cells: goodCells()})
		if err != nil || len(recs) != 1 || recs[0].Time == nil {
			t.Fatalf("mapping %d: a good row did not map to a timed record: %v %v", i, err, recs)
		}
		checkMapped(t, mp, goodCells())
		for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
			for col := range goodCells() {
				cs := goodCells()
				cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
				checkMapped(t, mp, cs)
			}
			cs := goodCells()
			for j := range cs {
				cs[j] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
			}
			checkMapped(t, mp, cs)
		}
		for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 4242, 1 << 53, math.MaxInt64} {
			cs := goodCells()
			for j := range cs {
				if cs[j].kind == cInt {
					cs[j].i = v
				}
			}
			checkMapped(t, mp, cs)
		}
	}
}

// FuzzMapAndroidCalendarRows fuzzes both Events mappings on decoded column values directly: every column of the row
// takes any storage class and value.
func FuzzMapAndroidCalendarRows(f *testing.F) {
	f.Add(byte(0), []byte{cNull, cInt, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("T\x00P1D"))
	f.Add(byte(1), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(0), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(1), []byte{cInt, cInt, cInt, cInt, cInt, cInt, cText, cText}, []byte{0, 0x10, 0xa5, 0xd4, 0xe8, 0, 0, 0}, []byte("PT1H\x00Europe/Berlin\x00FREQ=DAILY"))
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts []byte) {
		ms := mappingsUnderTest()
		mp := ms[int(which)%len(ms)]
		checkMapped(t, mp, cellsFrom(len(mp.Columns), kinds, nums, texts))
	})
}
