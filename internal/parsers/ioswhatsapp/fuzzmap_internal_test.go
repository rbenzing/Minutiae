package ioswhatsapp

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

// joinColumns are the columns of the four join tables, in the order a fuzzRow's cells follow.
var joinColumns = map[string][]string{
	tableChat:      {"Z_PK", colSessionType, "ZREMOVED", "ZHIDDEN", "ZFLAGS", "ZPARTNERNAME", "ZCONTACTJID"},
	tableMember:    {"Z_PK", "ZMEMBERJID", "ZCHATSESSION"},
	tableMediaItem: {"Z_PK", "ZMESSAGE", "ZFILESIZE", "ZMEDIAURLDATE", "ZLATITUDE", "ZLONGITUDE", "ZMEDIALOCALPATH", "ZTITLE", "ZVCARDNAME", "ZVCARDSTRING", "ZMEDIAKEY", "ZMETADATA", "ZMEDIAURL", "ZAUTHORNAME"},
	tableInfo:      {"Z_PK", "ZMESSAGE", "ZRECEIPTINFO"},
}

// fuzzCtx knows the columns of ZWAMESSAGE by its spec order and the four join tables by joinColumns, and answers every join
// with the one row of its table that the fuzz bytes gave: Get(1) and every Match find it, any other key finds nothing.
type fuzzCtx struct {
	in    *parse.Input
	mp    parse.TableMapping
	joins map[string]fuzzRow
}

func (c fuzzCtx) Input() *parse.Input { return c.in }
func (fuzzCtx) Note(_, _ string)      {}
func (c fuzzCtx) Column(table, col string) int {
	names := joinColumns[table]
	if table == c.mp.Table {
		names = specNames(c.mp.Columns)
	}
	for i, n := range names {
		if n == col {
			return i
		}
	}
	return -1
}

func (c fuzzCtx) Get(table string, id int64) (parse.Row, bool, error) {
	if r, ok := c.joins[table]; ok && id == 1 {
		return r, true, nil
	}
	return nil, false, nil
}

func (c fuzzCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	if r, ok := c.joins[table]; ok {
		return []parse.Row{r}, nil
	}
	return nil, nil
}

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

func intCell(mp parse.TableMapping, cells []cell, name string) (cell, bool) {
	for i, s := range mp.Columns {
		if s.Name == name {
			return cells[i], true
		}
	}
	return cell{}, false
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed row error (a counted warning) or at most
// one record that validates, carries its place, and invents nothing the cells do not say. joins are the rows of the join tables.
func checkMapped(t testing.TB, place byte, cells []cell, joins map[string]fuzzRow) {
	t.Helper()
	mp := messageMappingWith(nil)
	in := fuzzInput(place)
	recs, err := mp.Map(fuzzCtx{in: in, mp: mp, joins: joins}, fuzzRow{table: mp.Table, id: 1, cells: cells})
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
		if r.Type != message.Type {
			t.Fatalf("%s: a record of type %q", mp.Table, r.Type)
		}
		if verr := message.Validate(r.Payload); verr != nil {
			t.Fatalf("%s: payload refused: %v", mp.Table, verr)
		}
		if r.Recovery != "" || r.Confidence != nil {
			t.Errorf("%s: the mapping stamped a recovery: %+v", mp.Table, r)
		}
		// a time only from a numeric ZMESSAGEDATE cell
		if c, ok := intCell(mp, cells, "ZMESSAGEDATE"); ok && r.Time != nil && c.kind != cInt && c.kind != cFloat {
			t.Errorf("%s: a time from a ZMESSAGEDATE cell of kind %d", mp.Table, c.kind)
		}
		// a direction only from an INTEGER 0 or 1 of ZISFROMME
		if c, ok := intCell(mp, cells, "ZISFROMME"); ok && r.Payload["direction"] != nil && r.Payload["direction"] != "unknown" && (c.kind != cInt || (c.i != 0 && c.i != 1)) {
			t.Errorf("%s: direction %v from a cell %+v", mp.Table, r.Payload["direction"], c)
		}
		// a recovered row's label: a domain names its app, an app group states none (PF5)
		raw, _ := r.Payload["raw"].(map[string]any)
		if place%3 == 2 && (raw["app"] != "unknown" || raw["app_state"] != "not-evaluated") {
			t.Errorf("%s: a row of an app group states app %v state %v", mp.Table, raw["app"], raw["app_state"])
		}
		if place%3 != 2 && (raw["app"] != "whatsapp" && raw["app"] != "whatsapp_business") {
			t.Errorf("%s: a row of a backup domain states app %v", mp.Table, raw["app"])
		}
	}
}

// goodCells is a plain incoming text message of chat session 1.
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
	return cs
}

// goodJoins are one valid row of every join table.
func goodJoins() map[string]fuzzRow {
	text := func(s string) cell { return cell{kind: cText, b: []byte(s)} }
	one := cell{kind: cInt, i: 1}
	return map[string]fuzzRow{
		tableChat:      {table: tableChat, id: 1, cells: []cell{one, {kind: cInt, i: 0}, {kind: cInt, i: 0}, {kind: cInt, i: 0}, {kind: cInt, i: 0}, text("T"), text("15550000001@s.whatsapp.net")}},
		tableMember:    {table: tableMember, id: 1, cells: []cell{one, text("15550000002@s.whatsapp.net"), one}},
		tableMediaItem: {table: tableMediaItem, id: 1, cells: []cell{one, one, one, {kind: cFloat, f: 600000001}, {kind: cFloat, f: 1}, {kind: cFloat, f: 2}, text("Media/a/b.jpg"), text("t"), text("n"), text("image/jpeg"), {kind: cBlob, b: []byte{1}}, {kind: cBlob, b: []byte{2}}, text("u"), text("a")}},
		tableInfo:      {table: tableInfo, id: 1, cells: []cell{one, one, {kind: cBlob, b: []byte{3}}}},
	}
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

// joinsFrom gives each join table a row made from the same fuzz bytes, each shifted by its index so the tables differ.
func joinsFrom(kinds, nums, texts []byte) map[string]fuzzRow {
	out := map[string]fuzzRow{}
	shift := 0
	for _, name := range []string{tableChat, tableMember, tableMediaItem, tableInfo} {
		k := make([]byte, len(kinds))
		for i := range kinds {
			k[i] = kinds[(i+shift)%len(kinds)]
		}
		out[name] = fuzzRow{table: name, id: 1, cells: cellsFrom(len(joinColumns[name]), k, nums, texts)}
		shift++
	}
	return out
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	mp := messageMappingWith(nil)
	for place := range byte(len(fuzzPlaces)) {
		recs, err := mp.Map(fuzzCtx{in: fuzzInput(place), mp: mp, joins: goodJoins()}, fuzzRow{table: mp.Table, id: 1, cells: goodCells(mp)})
		if err != nil || len(recs) != 1 {
			t.Fatalf("place %d: a good row did not map: %v %v", place, err, recs)
		}
		checkMapped(t, place, goodCells(mp), goodJoins())
		for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
			for col := range goodCells(mp) {
				cs := goodCells(mp)
				cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
				checkMapped(t, place, cs, goodJoins())
			}
			cs := goodCells(mp)
			for i := range cs {
				cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
			}
			checkMapped(t, place, cs, goodJoins())
			// every column of every join row takes the class k
			js := goodJoins()
			for name, r := range js {
				for i := range r.cells {
					r.cells[i] = cell{kind: k, i: 1, f: math.Inf(1), b: []byte{0xff}}
				}
				js[name] = r
			}
			checkMapped(t, place, goodCells(mp), js)
		}
		for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 5, 6, 14, 1 << 53, 1<<53 + 1, math.MaxInt64} {
			cs := goodCells(mp)
			for i := range cs {
				if cs[i].kind == cInt {
					cs[i].i = v
				}
			}
			checkMapped(t, place, cs, goodJoins())
		}
		for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 1e300, 600000001.5} {
			cs := goodCells(mp)
			for i := range cs {
				if cs[i].kind == cFloat {
					cs[i].f = f
				}
			}
			checkMapped(t, place, cs, goodJoins())
		}
	}
}

// FuzzMapIOSWhatsAppChatsRows fuzzes the ZWAMESSAGE mapping on decoded column values directly, with the join tables answering
// from the same bytes: every column of every row takes any storage class and value, at each place.
func FuzzMapIOSWhatsAppChatsRows(f *testing.F) {
	f.Add(byte(0), []byte{cInt, cInt, cInt, cFloat, cInt, cInt, cInt, cInt, cInt, cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("15550000001@s.whatsapp.net\x00hello"))
	f.Add(byte(2), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(1), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(5), []byte{cInt, cInt, cInt, cInt, cInt, cInt, cInt, cText, cText}, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}, []byte("../a\x00*\x005"))
	f.Fuzz(func(t *testing.T, place byte, kinds, nums, texts []byte) {
		mp := messageMappingWith(nil)
		checkMapped(t, place, cellsFrom(len(mp.Columns), kinds, nums, texts), joinsFrom(kinds, nums, texts))
	})
}
