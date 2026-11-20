package ioscalendar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
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
// column is fuzzed here without a database. (A stored NaN, which only a sqlitedb.Row can show, is reached by
// FuzzParseIOSCalendar and the Go-built tests.)
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

// joinOutcome is what the fake context answers for a join (Task 6 review M3, restated): a clean miss (Get: no row, the
// state "no-<Table>-row"; Match: a decided empty join), a table the database lacks (sqlitedb.ErrNoSuchTable) or a
// damaged table (an error wrapping sqlitedb.ErrCorrupt). The last two must make the join undecided, never "no row".
type joinOutcome byte

const (
	joinMiss joinOutcome = iota
	joinNoTable
	joinDamaged
	numOutcomes
)

func (o joinOutcome) err() error {
	switch o {
	case joinNoTable:
		return fmt.Errorf("%w: fake", sqlitedb.ErrNoSuchTable)
	case joinDamaged:
		return fmt.Errorf("%w: fake page 3", sqlitedb.ErrCorrupt)
	}
	return nil
}

// fakeCtx knows the CalendarItem columns by their spec order; every Get (Location, Calendar, the organizer's
// Participant) answers get and every Match (Participant by item_id, Recurrence, ExceptionDate, Alarm) answers match.
type fakeCtx struct {
	in         *parse.Input
	get, match joinOutcome
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (fakeCtx) Column(table, col string) int {
	if table != tableItem {
		return -1
	}
	for i, s := range itemSpecs() {
		if s.Name == col {
			return i
		}
	}
	return -1
}

func (c fakeCtx) Get(string, int64) (parse.Row, bool, error) { return nil, false, c.get.err() }

func (c fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, c.match.err() }

// outcomesOf picks the Get and Match answers from a fuzz byte (the mapping takes its low bit).
func outcomesOf(which byte) (get, match joinOutcome) {
	return joinOutcome(which>>1) % numOutcomes, joinOutcome(which>>3) % numOutcomes
}

const fakeLogical = "ios:HomeDomain/Library/Calendar/Calendar.sqlitedb"

func fakeInput() *parse.Input {
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: fakeLogical},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100}},
	}
}

// mappingsUnderTest are the typed pass's mapping (no env: a recovered row joins nothing) and the live one (an empty
// guard: every join reaches the fake context and gets its joinOutcome).
func mappingsUnderTest() []parse.TableMapping {
	return []parse.TableMapping{Parser{}.Mappings()[0], eventMappingWith(&env{})}
}

func cellOf(cells []cell, name string) (cell, int) {
	for i, s := range itemSpecs() {
		if s.Name == name {
			return cells[i], i
		}
	}
	return cell{}, -1
}

// getJoins are the Get joins by a CalendarItem key: the key column, the table and the raw state key.
func getJoins() [][3]string {
	return [][3]string{
		{"location_id", tableLocation, "location_state"},
		{"calendar_id", tableCalendar, "calendar_state"},
		{"organizer_id", tableParticipant, "organizer_state"},
	}
}

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed row error or exactly one record
// that validates, carries its place, stamps no recovery, and invents nothing the cells do not say. Each join's outcome
// is stated per answer of the context (live is the mapping with a Parse context): the typed pass joins nothing and
// states "not-joined-recovered" for each key it holds; live, a clean miss is "no-<Table>-row" and a decided empty child
// join, a missing or damaged table makes the join undecided (raw.join_undecided), never "no row".
func checkMapped(t testing.TB, mp parse.TableMapping, live bool, ctx fakeCtx, cells []cell) {
	t.Helper()
	ctx.in = fakeInput()
	row := fakeRow{table: tableItem, cells: cells}
	recs, err := mp.Map(ctx, row)
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("untyped error %v", err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%d records: every CalendarItem event row is one record (spec 10)", len(recs))
	}
	r := recs[0]
	if r.ArtifactID != "a1" || r.Range == nil || r.Locator != "sqlite:table=CalendarItem;rowid=1" {
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
	raw, _ := r.Payload["raw"].(map[string]any)
	if raw["deletion_state"] != stateNotEvaluated || r.Deleted {
		t.Errorf("deletion_state %v, Deleted %v: no iOS record is Deleted (spec 6.4)", raw["deletion_state"], r.Deleted)
	}
	undecided, _ := raw["join_undecided"].([]any)
	// the fake context never returns a row: no join is ever made
	for _, k := range []string{"location", "calendar", "organizer", "attendees", "recurrence"} {
		if r.Payload[k] != nil {
			t.Errorf("payload.%s = %v from a context that returns no row", k, r.Payload[k])
		}
	}
	for _, j := range getJoins() {
		_, i := cellOf(cells, j[0])
		_, k := intCellOf(row, i)
		holds := k == rowrun.CellValue || k == rowrun.CellOther || k == rowrun.CellUnknown
		switch {
		case !live && holds:
			if raw[j[2]] != notJoinedRecovered {
				t.Errorf("typed pass, %s of kind %d: raw.%s = %v, want %q", j[0], k, j[2], raw[j[2]], notJoinedRecovered)
			}
		case !live || !holds:
			if raw[j[2]] != nil {
				t.Errorf("%s of kind %d: raw.%s = %v; a NULL key joins nothing and states nothing", j[0], k, j[2], raw[j[2]])
			}
		case k == rowrun.CellValue && ctx.get == joinMiss:
			if raw[j[2]] != noRow(j[1]) {
				t.Errorf("Get answers a clean miss: raw.%s = %v, want %q", j[2], raw[j[2]], noRow(j[1]))
			}
		default: // an undecidable key, or a missing or damaged table
			if raw[j[2]] != nil || !slices.Contains(undecided, any(j[1])) {
				t.Errorf("%s of kind %d, Get answers %d: raw.%s = %v, join_undecided %v; want the %s join undecided", j[0], k, ctx.get, j[2], raw[j[2]], undecided, j[1])
			}
		}
	}
	children := []string{tableRecurrence, tableExceptions, tableAlarm}
	switch {
	case !live:
		if raw["join_undecided"] != nil {
			t.Errorf("typed pass: join_undecided %v; a recovered row joins nothing", undecided)
		}
		for _, k := range matchStateKeys() {
			if raw[k] != notJoinedRecovered {
				t.Errorf("typed pass: raw.%s = %v, want %q", k, raw[k], notJoinedRecovered)
			}
		}
	case ctx.match != joinMiss:
		for _, c := range append(children, tableParticipant) {
			if !slices.Contains(undecided, any(c)) {
				t.Errorf("Match answers %d: join_undecided %v; want %s undecided", ctx.match, undecided, c)
			}
		}
	default:
		for _, c := range children {
			if slices.Contains(undecided, any(c)) {
				t.Errorf("a decided empty child join is listed undecided: %v", undecided)
			}
		}
		for _, k := range matchStateKeys() {
			if raw[k] != nil {
				t.Errorf("live: raw.%s = %v; an empty match is no children, never a state (F15)", k, raw[k])
			}
		}
	}
	if live {
		for _, k := range []string{"recurrence", "exception_dates", "alarms", "attendees"} {
			if raw[k] != nil {
				t.Errorf("raw.%s = %v from a context that returns no row", k, raw[k])
			}
		}
	}
	// a start only from a numeric start_date cell
	if c, _ := cellOf(cells, "start_date"); r.Time != nil && c.kind != cInt && c.kind != cFloat {
		t.Errorf("a start from a start_date cell of kind %d", c.kind)
	}
	// no code is labelled (Q1): a stated status is "unknown", and only from a cell that holds a value
	if c, _ := cellOf(cells, "status"); r.Payload["status"] != nil && (r.Payload["status"] != labelUnknown || c.kind == cNull) {
		t.Errorf("status %v from a status cell of kind %d", r.Payload["status"], c.kind)
	}
}

func goodCells() []cell {
	specs := itemSpecs()
	cs := make([]cell, len(specs))
	for i, s := range specs {
		switch s.Affinity {
		case "integer":
			cs[i] = cell{kind: cInt, i: 0}
		case "real":
			cs[i] = cell{kind: cFloat, f: 721692800}
		case "blob":
			cs[i] = cell{kind: cBlob, b: []byte{1, 2}}
		default:
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
		switch s.Name {
		case "ROWID":
			cs[i] = cell{kind: cNull}
		case "calendar_id", "location_id", "organizer_id":
			cs[i].i = 1
		case "entity_type":
			cs[i].i = kindEvent
		case "end_date":
			cs[i].f = 721692800 + 3600
		case "start_tz", "end_tz":
			cs[i].b = []byte("Europe/Berlin")
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

// Every join answer of the context reaches the live mapping (Task 6 review M3, restated): a clean miss is
// "no-<Table>-row" and a decided empty child join, a missing or damaged table makes the join undecided, never "no
// row"; the typed pass joins nothing whatever the context would answer.
func TestMapRowsJoinOutcomesAreStated(t *testing.T) {
	for i, mp := range mappingsUnderTest() {
		for get := range numOutcomes {
			for match := range numOutcomes {
				checkMapped(t, mp, i == 1, fakeCtx{get: get, match: match}, goodCells())
			}
		}
	}
	live := mappingsUnderTest()[1]
	recs, err := live.Map(fakeCtx{in: fakeInput()}, fakeRow{table: tableItem, cells: goodCells()})
	if err != nil || len(recs) != 1 {
		t.Fatalf("%v %v", err, recs)
	}
	raw, _ := recs[0].Payload["raw"].(map[string]any)
	if raw["location_state"] != "no-Location-row" || raw["calendar_state"] != "no-Calendar-row" || raw["organizer_state"] != "no-Participant-row" || raw["join_undecided"] != nil {
		t.Errorf("a clean miss: location_state %v, calendar_state %v, organizer_state %v, join_undecided %v", raw["location_state"], raw["calendar_state"], raw["organizer_state"], raw["join_undecided"])
	}
	recs, err = live.Map(fakeCtx{in: fakeInput(), get: joinDamaged, match: joinNoTable}, fakeRow{table: tableItem, cells: goodCells()})
	if err != nil || len(recs) != 1 {
		t.Fatalf("%v %v", err, recs)
	}
	raw, _ = recs[0].Payload["raw"].(map[string]any)
	want := []any{"Alarm", "Calendar", "ExceptionDate", "Location", "Participant", "Recurrence"}
	if got, _ := raw["join_undecided"].([]any); !slices.Equal(got, want) || raw["location_state"] != nil {
		t.Errorf("a damaged and a missing table: join_undecided %v, location_state %v; want %v and no state", raw["join_undecided"], raw["location_state"], want)
	}
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for i, mp := range mappingsUnderTest() {
		recs, err := mp.Map(fakeCtx{in: fakeInput()}, fakeRow{table: tableItem, cells: goodCells()})
		if err != nil || len(recs) != 1 || recs[0].Time == nil {
			t.Fatalf("mapping %d: a good row did not map to a timed record: %v %v", i, err, recs)
		}
		check := func(t testing.TB, mp parse.TableMapping, cells []cell) {
			t.Helper()
			for _, c := range []fakeCtx{{}, {get: joinDamaged, match: joinNoTable}} {
				checkMapped(t, mp, i == 1, c, cells)
			}
		}
		check(t, mp, goodCells())
		for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
			for col := range goodCells() {
				cs := goodCells()
				cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
				check(t, mp, cs)
			}
			cs := goodCells()
			for j := range cs {
				cs[j] = cell{kind: k, f: math.Inf(1), b: []byte{0xff, 0xfe}}
			}
			check(t, mp, cs)
		}
		for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 3, 4242, 1 << 53, math.MaxInt64} {
			cs := goodCells()
			for j := range cs {
				if cs[j].kind == cInt {
					cs[j].i = v
				}
			}
			check(t, mp, cs)
		}
	}
}

// FuzzMapIOSCalendarRows fuzzes both CalendarItem mappings on decoded column values directly: every column of the row
// takes any storage class and value, and which picks the mapping (low bit) and the Get and Match answers of the
// context (outcomesOf).
func FuzzMapIOSCalendarRows(f *testing.F) {
	f.Add(byte(0), []byte{cNull, cText, cFloat, cText, cInt, cInt}, []byte{0, 0, 0, 0x50, 0x9c, 0x81, 0xc5, 0x41}, []byte("T\x00Europe/Berlin"))
	f.Add(byte(1), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00_float"))
	f.Add(byte(3), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(11), []byte{cInt, cInt, cInt, cInt, cInt, cInt, cText, cText}, []byte{2, 0, 0, 0, 0, 0, 0, 0}, []byte("_float\x00Europe/Berlin\x00https://x"))
	f.Add(byte(21), []byte{cFloat}, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}, []byte("x"))
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts []byte) {
		ms := mappingsUnderTest()
		i := int(which) % len(ms)
		get, match := outcomesOf(which)
		checkMapped(t, ms[i], i == 1, fakeCtx{get: get, match: match}, cellsFrom(len(ms[i].Columns), kinds, nums, texts))
	})
}
