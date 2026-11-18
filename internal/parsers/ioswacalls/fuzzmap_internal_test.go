package ioswacalls

import (
	"encoding/binary"
	"encoding/json"
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

// fuzzRow is a decoded row: a mapping sees only the parse.Row interface, so its behaviour on any storage class in any column is
// fuzzed here without a database.
type fuzzRow struct {
	table string
	id    int64
	cells []cell
}

func (r fuzzRow) Table() string        { return r.table }
func (r fuzzRow) Rowid() (int64, bool) { return r.id, true }
func (r fuzzRow) IsNull(c int) bool    { return r.cells[c].kind == cNull }
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

// aggColumns are the columns of the one join table, in the order a fuzzRow's cells follow.
var aggColumns = []string{"Z_PK", "ZINCOMING", "ZVIDEO", "ZMISSED", "ZMISSEDREASON"}

// fuzzCtx knows the columns of ZWACDCALLEVENT by its spec order and the join table by aggColumns, and answers Get on the join
// table with the one row the fuzz bytes gave: Get(1) finds it, any other key finds nothing.
type fuzzCtx struct {
	in  *parse.Input
	mp  parse.TableMapping
	agg *fuzzRow
}

func (c fuzzCtx) Input() *parse.Input { return c.in }
func (fuzzCtx) Note(_, _ string)      {}
func (c fuzzCtx) Column(table, col string) int {
	names := []string(nil)
	switch table {
	case c.mp.Table:
		names = specNames(c.mp.Columns)
	case tableAgg:
		names = aggColumns
	}
	for i, n := range names {
		if n == col {
			return i
		}
	}
	return -1
}

func (c fuzzCtx) Get(table string, id int64) (parse.Row, bool, error) {
	if table == tableAgg && c.agg != nil && id == 1 {
		return *c.agg, true, nil
	}
	return nil, false, nil
}

func (c fuzzCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

// fuzzPlaces are the logical paths a row is mapped at: the consumer and Business backup domains and an app group.
var fuzzPlaces = []string{
	"ios:" + domainConsumer + "/" + dbName,
	"ios:" + domainBusiness + "/" + dbName,
	appGroupRoot + "5A0B4C3D-0000-4000-8000-00000000A001/" + dbName,
}

func fuzzInput(place byte) *parse.Input {
	logical := fuzzPlaces[int(place)%len(fuzzPlaces)]
	const remote = "UDID1/aa/aabbcc"
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: logical, Source: parse.SourceInfo{RemotePath: remote}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: remote}}},
	}
}

// The modes a row is mapped in: the typed pass (no Parse context), Parse with a complete set, with a set read from a damaged
// table, with a set that hit its cap, and with no aggregate column.
const (
	modeRecovered = iota
	modeComplete
	modeLossy
	modeCapped
	modeNoColumn
	numModes
)

// fuzzEnv is the environment Parse would hand the mapping in a mode: the aggregate 1 is linked by this row (the mapped row has
// rowid 1) alone, 2 by two calls, 3 by more than the cap.
func fuzzEnv(place, mode byte) *env {
	if mode%numModes == modeRecovered {
		return nil
	}
	in := fuzzInput(place)
	p := classifyPlace(in.Primary.Logical)
	l := appLabel{App: p.App, Basis: "backup_domain"}
	if p.Place == placeAppGroup {
		l = appLabel{App: "unknown", State: "no-container-metadata"}
	}
	as := &aggSet{ByAgg: map[int64]aggEntry{1: {Count: 1, Only: 1}, 2: {Count: 2, Only: 7}, 3: {Count: maxAggCount, Only: 9}}, Complete: true}
	switch mode % numModes {
	case modeLossy:
		as.Complete, as.Lossy = false, true
	case modeCapped:
		as.Complete = false
	case modeNoColumn:
		as.NoColumn = true
	}
	return &env{Place: p, Label: l, Aggs: as}
}

func intCell(mp parse.TableMapping, cells []cell, name string) (cell, bool) {
	for i, s := range mp.Columns {
		if s.Name == name {
			return cells[i], true
		}
	}
	return cell{}, false
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed row error (a counted warning) or at most
// one record that validates, carries its place, and invents nothing the cells do not say. agg is the aggregate row.
func checkMapped(t testing.TB, place, mode byte, cells []cell, agg *fuzzRow) {
	t.Helper()
	e := fuzzEnv(place, mode)
	mp := callMappingWith(e)
	in := fuzzInput(place)
	recs, err := mp.Map(fuzzCtx{in: in, mp: mp, agg: agg}, fuzzRow{table: mp.Table, id: 1, cells: cells})
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
		if r.ArtifactID != "a1" || r.SourcePath != "UDID1/aa/aabbcc" || r.Range == nil || r.Locator != "sqlite:table="+mp.Table+";rowid=1" {
			t.Fatalf("%s: record lost its place: %+v", mp.Table, r)
		}
		if r.Type != call.Type {
			t.Fatalf("%s: a record of type %q", mp.Table, r.Type)
		}
		if verr := call.Validate(r.Payload); verr != nil {
			t.Fatalf("%s: payload refused: %v", mp.Table, verr)
		}
		// raw stays inside a bound that follows the text cells only: a blob is shown by its length or by at most 256 bytes of
		// base64, however large it is (the JSON escaping of a text byte is at most 6)
		if rawJSON, err := json.Marshal(r.Payload["raw"]); err != nil || len(rawJSON) > rawBound(cells, agg) {
			t.Errorf("%s: raw is %d bytes (%v), over the bound %d", mp.Table, len(rawJSON), err, rawBound(cells, agg))
		}
		if r.Recovery != "" || r.Confidence != nil {
			t.Errorf("%s: the mapping stamped a recovery: %+v", mp.Table, r)
		}
		// never a counterparty, a service other than the app's, or a kind (ZVIDEO is not mapped, PF6)
		if _, has := r.Payload["address"]; has || r.Payload["service"] != "whatsapp" || r.Payload["kind"] != "unknown" {
			t.Errorf("%s: address %v service %v kind %v", mp.Table, r.Payload["address"], r.Payload["service"], r.Payload["kind"])
		}
		raw, _ := r.Payload["raw"].(map[string]any)
		// a time only from a numeric ZDATE cell; an end only with a time and a positive duration
		if c, ok := intCell(mp, cells, "ZDATE"); ok && r.Time != nil && c.kind != cInt && c.kind != cFloat {
			t.Errorf("%s: a time from a ZDATE cell of kind %d", mp.Table, c.kind)
		}
		if r.TimeEnd != nil {
			if d, _ := r.Payload["duration_s"].(float64); r.Time == nil || !(d > 0) || raw["time_end_basis"] != "start+duration" {
				t.Errorf("%s: an end %v with time %v duration %v basis %v", mp.Table, r.TimeEnd, r.Time, d, raw["time_end_basis"])
			}
		}
		if d, ok := r.Payload["duration_s"].(float64); ok && (math.IsNaN(d) || math.IsInf(d, 0) || d < 0) {
			t.Errorf("%s: duration_s %v", mp.Table, d)
		}
		// an outcome only from a ZOUTCOME INTEGER 0 or 1
		if c, ok := intCell(mp, cells, "ZOUTCOME"); ok && r.Payload["outcome"] != "unknown" && (c.kind != cInt || (c.i != 0 && c.i != 1)) {
			t.Errorf("%s: outcome %v from a cell %+v", mp.Table, r.Payload["outcome"], c)
		}
		// a direction only from a complete set that says this row is the one call of an aggregate whose ZINCOMING is 0 or 1
		if r.Payload["direction"] != "unknown" {
			z1, _ := intCell(mp, cells, "Z1CALLEVENTS")
			ok := e != nil && !e.Aggs.Lossy && !e.Aggs.NoColumn && z1.kind == cInt && z1.i == 1 && agg != nil && agg.cells[1].kind == cInt &&
				(agg.cells[1].i == 0 || agg.cells[1].i == 1)
			if !ok {
				t.Errorf("%s: direction %v in mode %d from cells %+v / %+v", mp.Table, r.Payload["direction"], mode, z1, agg)
			}
		}
		// a recovered row borrows nothing from the live aggregates (Review Focus 4)
		if mode%numModes == modeRecovered && (raw["direction_state"] != "recovered-row" || raw["aggregate"] != nil || r.Payload["direction"] != "unknown") {
			t.Errorf("%s: a recovered row states direction %v state %v aggregate %v", mp.Table, r.Payload["direction"], raw["direction_state"], raw["aggregate"])
		}
		// the label of a recovered row: a domain names its app, an app group states none (PF5)
		if mode%numModes == modeRecovered {
			if place%3 == 2 && (raw["app"] != "unknown" || raw["app_state"] != "not-evaluated") {
				t.Errorf("%s: a row of an app group states app %v state %v", mp.Table, raw["app"], raw["app_state"])
			}
			if place%3 != 2 && (raw["app"] != "whatsapp" && raw["app"] != "whatsapp_business") {
				t.Errorf("%s: a row of a backup domain states app %v", mp.Table, raw["app"])
			}
		}
	}
}

// goodCells is a plain answered call of aggregate 1.
func goodCells(mp parse.TableMapping) []cell {
	cs := make([]cell, len(mp.Columns))
	for i, s := range mp.Columns {
		switch s.Affinity {
		case "integer":
			cs[i] = cell{kind: cInt, i: 1}
		case "real":
			cs[i] = cell{kind: cFloat, f: 600000001}
		default:
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
	}
	for i, s := range mp.Columns {
		if s.Name == "ZOUTCOME" {
			cs[i].i = 0
		}
	}
	return cs
}

// goodAgg is one valid aggregate row (ZINCOMING 1).
func goodAgg() *fuzzRow {
	one := cell{kind: cInt, i: 1}
	return &fuzzRow{table: tableAgg, id: 1, cells: []cell{one, one, one, {kind: cInt, i: 0}, {kind: cInt, i: 3}}}
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column for the
// integer and float, texts is split at NUL bytes.
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
	mp := callMappingWith(nil)
	for place := range byte(len(fuzzPlaces)) {
		for mode := range byte(numModes) {
			recs, err := callMappingWith(fuzzEnv(place, mode)).Map(fuzzCtx{in: fuzzInput(place), mp: mp, agg: goodAgg()}, fuzzRow{table: mp.Table, id: 1, cells: goodCells(mp)})
			if err != nil || len(recs) != 1 {
				t.Fatalf("place %d mode %d: a good row did not map: %v %v", place, mode, err, recs)
			}
			checkMapped(t, place, mode, goodCells(mp), goodAgg())
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range goodCells(mp) {
					cs := goodCells(mp)
					cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
					checkMapped(t, place, mode, cs, goodAgg())
				}
				cs := goodCells(mp)
				for i := range cs {
					cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
				}
				checkMapped(t, place, mode, cs, goodAgg())
				// every column of the aggregate row takes the class k
				ag := goodAgg()
				for i := range ag.cells {
					ag.cells[i] = cell{kind: k, i: 1, f: math.Inf(1), b: []byte{0xff}}
				}
				checkMapped(t, place, mode, goodCells(mp), ag)
			}
			checkMapped(t, place, mode, goodCells(mp), nil) // the aggregate row does not exist
			for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 3, 5, 1 << 53, 1<<53 + 1, math.MaxInt64} {
				cs := goodCells(mp)
				for i := range cs {
					if cs[i].kind == cInt {
						cs[i].i = v
					}
				}
				checkMapped(t, place, mode, cs, goodAgg())
				ag := goodAgg()
				for i := range ag.cells {
					ag.cells[i].i = v
				}
				checkMapped(t, place, mode, goodCells(mp), ag)
			}
			for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 1e300, 600000001.5} {
				cs := goodCells(mp)
				for i := range cs {
					if cs[i].kind == cFloat {
						cs[i].f = f
					}
				}
				checkMapped(t, place, mode, cs, goodAgg())
			}
		}
	}
}

// FuzzMapIOSWhatsAppCallsRows fuzzes the ZWACDCALLEVENT mapping on decoded column values directly, with the aggregate row
// answering from the same bytes: every column of every row takes any storage class and value, at each place and in each mode
// (the typed pass, a complete, a damaged and a capped aggregate set, and no aggregate column).
func FuzzMapIOSWhatsAppCallsRows(f *testing.F) {
	f.Add(byte(0), byte(1), []byte{cInt, cInt, cInt, cFloat, cInt, cInt, cInt, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("15550000001@s.whatsapp.net\x00hello"))
	f.Add(byte(2), byte(0), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(1), byte(2), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(5), byte(3), []byte{cInt, cInt, cInt, cInt, cInt, cInt, cInt, cText, cText}, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}, []byte("../a\x00*\x005"))
	f.Add(byte(4), byte(4), []byte{cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("x"))
	f.Fuzz(func(t *testing.T, place, mode byte, kinds, nums, texts []byte) {
		mp := callMappingWith(nil)
		agg := &fuzzRow{table: tableAgg, id: 1, cells: cellsFrom(len(aggColumns), kinds, nums, texts)}
		if len(kinds) > 0 && kinds[0]%7 == 6 {
			agg = nil
		}
		checkMapped(t, place, mode, cellsFrom(len(mp.Columns), kinds, nums, texts), agg)
	})
}

// rawBound is the size raw may reach for a row: a fixed 64 KiB for the keys, labels and 256-byte blobs, plus six bytes per byte of
// text in any cell (the JSON escape of a control byte). A BLOB cell adds nothing: it is shown by its length or a 256-byte base64.
func rawBound(cells []cell, agg *fuzzRow) int {
	n := 64 << 10
	count := func(cs []cell) {
		for _, c := range cs {
			if c.kind == cText {
				n += 6 * len(c.b)
			}
		}
	}
	count(cells)
	if agg != nil {
		count(agg.cells)
	}
	return n
}

// A 1 MiB BLOB in every column of the row and of the aggregate row: raw stays inside its bound (an uncapped base64 of the blobs
// would be about 100 MB).
func TestMapRowsBigBlobsDoNotGrowRaw(t *testing.T) {
	mp := callMappingWith(nil)
	big := make([]byte, 1<<20)
	fill := func(n int) []cell {
		cs := make([]cell, n)
		for i := range cs {
			cs[i] = cell{kind: cBlob, b: big}
		}
		return cs
	}
	agg := &fuzzRow{table: tableAgg, id: 1, cells: fill(len(aggColumns))}
	for place := range byte(len(fuzzPlaces)) {
		for mode := range byte(numModes) {
			for col := range mp.Columns { // one column at a time: a row with a wrong-class required cell is a typed error and shows nothing
				cs := goodCells(mp)
				cs[col] = cell{kind: cBlob, b: big}
				checkMapped(t, place, mode, cs, goodAgg())
			}
			checkMapped(t, place, mode, goodCells(mp), agg)
		}
	}
}
