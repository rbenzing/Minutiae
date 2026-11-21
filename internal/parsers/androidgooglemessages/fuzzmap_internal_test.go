package androidgooglemessages

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
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

// fakeRow is a decoded row: a mapping sees only the parse.Row interface, so its behaviour on any storage class in any
// column is fuzzed here without a database (restated from androidtelegram/fuzzmap_internal_test.go, F29). noRowid makes
// the rowid unreadable.
type fakeRow struct {
	table   string
	cells   []cell
	noRowid bool
}

func (r fakeRow) Table() string { return r.table }
func (r fakeRow) Rowid() (int64, bool) {
	return 1, !r.noRowid
}
func (r fakeRow) IsNull(c int) bool { return r.cells[c].kind == cNull }
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

// joinOutcome is what the fake context answers for a join: a clean miss, a table the database lacks, a damaged table or
// a hit whose joined row holds the fuzzed cells. The missing and damaged tables must never read as "no row".
type joinOutcome byte

const (
	joinMiss joinOutcome = iota
	joinNoTable
	joinDamaged
	joinHit
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

// mapCtx knows the columns of the mapped table by the mapping's order and those of the joined tables by mappedColumns;
// every Get and Match answers with join, a hit with a row of the joined table holding hit's cells (cycled). failMatch, when
// set, is the error every Match of table failTable answers.
type mapCtx struct {
	in        *parse.Input
	table     string
	cols      []parse.ColumnSpec
	join      joinOutcome
	hit       []cell
	failTable string
	failMatch error
}

func (c mapCtx) Input() *parse.Input { return c.in }
func (mapCtx) Note(_, _ string)      {}
func (c mapCtx) Column(table, col string) int {
	if table != c.table {
		for i, n := range mappedColumns(table) {
			if n == col {
				return i
			}
		}
		return -1
	}
	for i, s := range c.cols {
		if s.Name == col {
			return i
		}
	}
	return -1
}

func (c mapCtx) joined(table string) fakeRow {
	cs := make([]cell, len(mappedColumns(table)))
	for i := range cs {
		if len(c.hit) > 0 {
			cs[i] = c.hit[i%len(c.hit)]
		}
	}
	return fakeRow{table: table, cells: cs}
}

func (c mapCtx) Get(table string, _ int64) (parse.Row, bool, error) {
	if c.join == joinHit {
		return c.joined(table), true, nil
	}
	return nil, false, c.join.err()
}

func (c mapCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	if c.failMatch != nil && table == c.failTable {
		return nil, c.failMatch
	}
	if c.join == joinHit {
		return []parse.Row{c.joined(table)}, nil
	}
	return nil, c.join.err()
}

const fakeRemote = "/data/data/com.google.android.apps.messaging/databases/bugle_db"

func fakeInput() *parse.Input {
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: "android:" + fakeRemote, Source: parse.SourceInfo{RemotePath: fakeRemote}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: fakeRemote}}},
		Budget:    parse.NewBudget(1 << 30).View(),
	}
}

// fakeEnv is a fresh live environment (the guard is empty: the fake context answers the joins).
func fakeEnv(in *parse.Input) *env {
	cnt := &counts{JoinUndecided: map[string]int64{}}
	return &env{IDs: newIDSet(in.Budget), Members: newMemberCache(in.Budget, cnt), Counts: cnt}
}

// mappingUnderTest builds one mapping with fresh per-job state (a member cache would otherwise carry one join answer to the
// next row) and says whether it is the live one.
type mappingUnderTest struct {
	name  string
	build func(in *parse.Input) parse.TableMapping
	live  bool
}

func mappingsUnderTest() []mappingUnderTest {
	typed := (Parser{}).Mappings()
	var out []mappingUnderTest
	for i := range typed {
		out = append(out, mappingUnderTest{name: "typed " + typed[i].Table, build: func(*parse.Input) parse.TableMapping { return (Parser{}).Mappings()[i] }})
	}
	return append(out,
		mappingUnderTest{name: "live messages", live: true, build: func(in *parse.Input) parse.TableMapping { return messagesMapping(fakeEnv(in)) }},
		mappingUnderTest{name: "live parts", live: true, build: func(in *parse.Input) parse.TableMapping { return partsMapping(fakeEnv(in)) }},
	)
}

func cellOf(cols []parse.ColumnSpec, cells []cell, name string) cell {
	for i, s := range cols {
		if s.Name == name {
			return cells[i]
		}
	}
	return cell{kind: cNull}
}

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed row error (or a host-level error
// of a join) or exactly one record that validates, carries its place, stamps no recovery and invents nothing the cells do
// not say (a channel only from an INTEGER protocol, a direction only from an INTEGER status, a time only from an INTEGER
// timestamp, a thread only from an INTEGER conversation id, a Body only from a TEXT part). A join the context cannot
// answer is undecided, never an answer: no join_undecided on a clean miss, a hit or without a join, and one on a live
// messages row whose conversation id is an INTEGER when the join is damaged or the table missing.
func checkMapped(t testing.TB, mt mappingUnderTest, join joinOutcome, cells []cell) {
	t.Helper()
	in := fakeInput()
	mp := mt.build(in)
	ctx := mapCtx{in: in, table: mp.Table, cols: mp.Columns, join: join, hit: cells}
	recs, err := mp.Map(ctx, fakeRow{table: mp.Table, cells: cells})
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) && !parsertest.HostileOK(err) {
			t.Fatalf("%s: untyped error %v", mt.name, err)
		}
		return
	}
	if len(recs) > 1 {
		t.Fatalf("%s: %d records: a row is at most one record", mt.name, len(recs))
	}
	if len(recs) == 0 {
		if !mt.live || mp.Table != tableParts {
			t.Fatalf("%s: no record and no error: every row is a record or a row warning", mt.name)
		}
		return // a joined part
	}
	r := recs[0]
	if r.ArtifactID != "a1" || r.SourcePath != fakeRemote || r.Range == nil || r.Locator != "sqlite:table="+mp.Table+";rowid=1" {
		t.Fatalf("%s: record lost its place: %+v", mt.name, r)
	}
	if r.Type != message.Type {
		t.Fatalf("%s: a record of type %q", mt.name, r.Type)
	}
	if verr := message.Validate(r.Payload); verr != nil {
		t.Fatalf("%s: payload refused: %v", mt.name, verr)
	}
	if r.Recovery != "" || r.Confidence != nil || r.Deleted {
		t.Errorf("%s: the mapping stamped a recovery or a deletion: %+v", mt.name, r)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	if (!mt.live || join == joinMiss || join == joinHit) && raw["join_undecided"] != nil {
		t.Errorf("%s: join_undecided %v on a clean miss, a hit or with no join", mt.name, raw["join_undecided"])
	}
	if mp.Table == tableMessages {
		if cid := cellOf(mp.Columns, cells, "conversation_id"); mt.live && (join == joinNoTable || join == joinDamaged) && cid.kind == cInt && raw["join_undecided"] == nil {
			t.Errorf("%s: a conversation (id %d) whose join answers %d has no raw.join_undecided", mt.name, cid.i, join)
		}
		if ch, _ := r.Payload["channel"].(string); (ch == "sms" || ch == "mms") && cellOf(mp.Columns, cells, "message_protocol").kind != cInt {
			t.Errorf("%s: channel %q from a protocol cell of kind %d", mt.name, ch, cellOf(mp.Columns, cells, "message_protocol").kind)
		}
		if d, _ := r.Payload["direction"].(string); (d == "in" || d == "out") && cellOf(mp.Columns, cells, "message_status").kind != cInt {
			t.Errorf("%s: direction %q from a status cell of kind %d", mt.name, d, cellOf(mp.Columns, cells, "message_status").kind)
		}
		if r.Time != nil && cellOf(mp.Columns, cells, "received_timestamp").kind != cInt && cellOf(mp.Columns, cells, "sent_timestamp").kind != cInt {
			t.Errorf("%s: a time from no INTEGER timestamp cell", mt.name)
		}
		if th, _ := r.Payload["thread"].(map[string]any); th != nil && th["id"] != nil && cellOf(mp.Columns, cells, "conversation_id").kind != cInt {
			t.Errorf("%s: a thread id from a conversation_id cell of kind %d", mt.name, cellOf(mp.Columns, cells, "conversation_id").kind)
		}
	}
	if r.Body != "" {
		fromPart := mp.Table == tableParts && cellOf(mp.Columns, cells, "text").kind == cText
		fromJoin := mp.Table == tableMessages && mt.live && join == joinHit
		if !fromPart && !fromJoin {
			t.Errorf("%s: a Body %q from no TEXT part cell", mt.name, r.Body)
		}
	}
}

// goodCells is a row that maps to a timed record: INTEGER columns 1, the timestamps T, the TEXT columns "x".
func goodCells(mp parse.TableMapping) []cell {
	cs := make([]cell, len(mp.Columns))
	for i, s := range mp.Columns {
		switch s.Affinity {
		case "integer":
			cs[i] = cell{kind: cInt, i: 1}
		case "text":
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
		switch s.Name {
		case "received_timestamp", "sent_timestamp", "timestamp":
			cs[i].i = 1700000000000
		case "message_status", "message_protocol":
			cs[i].i = 0
		}
	}
	return cs
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column
// for the integer and float, texts is split at NUL bytes for the TEXT and BLOB cells.
func cellsFrom(n int, kinds, nums, texts []byte) []cell {
	parts := bytes.Split(texts, []byte{0})
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

// Spec 9, 12: a good row maps to its record (a joined part to none, counted), and every storage class in every column,
// alone or in all columns, a NaN, the extreme integers and every join answer keep the contract (checkMapped).
func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for _, mt := range mappingsUnderTest() {
		in := fakeInput()
		mp := mt.build(in)
		recs, err := mp.Map(mapCtx{in: in, table: mp.Table, cols: mp.Columns}, fakeRow{table: mp.Table, cells: goodCells(mp)})
		var re *rowrun.RowError
		switch {
		case !mt.live && mp.Table == tableParts:
			if !errors.As(err, &re) {
				t.Fatalf("%s: a recovered part must be a row warning in this version: %v", mt.name, err)
			}
		case err != nil || len(recs) != 1:
			t.Fatalf("%s: a good row did not map to its record: %v %+v", mt.name, err, recs)
		}
		check := func(cells []cell) {
			for j := range numOutcomes {
				checkMapped(t, mt, j, cells)
			}
		}
		check(goodCells(mp))
		for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
			for col := range mp.Columns {
				cs := goodCells(mp)
				cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
				check(cs)
			}
			cs := goodCells(mp)
			for j := range cs {
				cs[j] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
			}
			check(cs)
		}
		for _, v := range []int64{math.MinInt64, -1, 0, 1, 1 << 62, 1<<62 | 77, math.MaxInt64} {
			cs := goodCells(mp)
			for j := range cs {
				if cs[j].kind == cInt {
					cs[j].i = v
				}
			}
			check(cs)
		}
	}
}

// hostileCells are the rows of gm-hostile's messages table as storage classes, integers and texts (the seeds of
// FuzzMapAndroidGoogleMessagesRows: the hostile cells, TEXT, REAL, BLOB and NaN, in the columns the mapping reads).
func hostileCells(t testing.TB) (kinds, nums, texts [][]byte) {
	t.Helper()
	data := parsertest.LoadFixture(t, "testdata", "gm-hostile").Files["db"]
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	names := []string{}
	for _, s := range messagesSpecs() {
		names = append(names, s.Name)
	}
	tb, err := db.Table(t.Context(), tableMessages, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
		var k, n, x []byte
		for _, name := range names {
			c := tb.Col(name)
			switch {
			case row.IsNull(c) && row.State(c) == sqlitedb.StateNull && row.Value(c).Serial == 7:
				k, n = append(k, cFloat), append(n, 0, 0, 0, 0, 0, 0, 0xf8, 0x7f)
			case row.IsNull(c):
				k, n = append(k, cNull), append(n, make([]byte, 8)...)
			default:
				if v, ok := row.Int(c); ok {
					k = append(k, cInt)
					n = binary.LittleEndian.AppendUint64(n, uint64(v))
				} else if f, ok := row.Float(c); ok {
					k = append(k, cFloat)
					n = binary.LittleEndian.AppendUint64(n, math.Float64bits(f))
				} else if b, ok := row.Text(c); ok {
					k, n = append(k, cText), append(n, make([]byte, 8)...)
					x = append(append(x, bytes.ReplaceAll(b[:min(len(b), 64)], []byte{0}, []byte{1})...), 0)
				} else {
					k, n = append(k, cBlob), append(n, make([]byte, 8)...)
					x = append(x, 0)
				}
			}
		}
		kinds, nums, texts = append(kinds, k), append(nums, n), append(texts, x)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return kinds, nums, texts
}

// FuzzMapAndroidGoogleMessagesRows fuzzes both mappings, typed and live, on decoded column values directly: every column
// takes any storage class and value, and which picks the mapping and the context's join answer. The seeds are the cells of
// gm-hostile's rows.
func FuzzMapAndroidGoogleMessagesRows(f *testing.F) {
	ks, ns, xs := hostileCells(f)
	for i := range ks {
		f.Add(byte(2+(i%2)+16*(i%4)), ks[i], ns[i], xs[i])
	}
	f.Add(byte(0), []byte{cNull, cFloat, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(3), []byte{cInt, cText, cBlob}, []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts []byte) {
		ms := mappingsUnderTest()
		mt := ms[int(which)%len(ms)]
		join := joinOutcome(which>>4) % numOutcomes
		mp := mt.build(fakeInput())
		checkMapped(t, mt, join, cellsFrom(len(mp.Columns), kinds, nums, texts))
	})
}
