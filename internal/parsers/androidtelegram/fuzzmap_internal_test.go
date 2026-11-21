package androidtelegram

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
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
// column is fuzzed here without a database (restated from androidcalendar/fuzzmap_internal_test.go, F29).
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

// joinOutcome is what the fake context answers for a join: a clean miss, a table the database lacks
// (sqlitedb.ErrNoSuchTable), a damaged table (an error wrapping sqlitedb.ErrCorrupt) or a hit whose joined row holds
// the fuzzed cells (Task 13 review M3: hostile users, chats and enc_chats rows reach the dialog path). The missing and
// damaged tables must make the join undecided, never "no row".
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

// joinedColumns are the columns of the joined tables as the v181 DDL declares them (spec 6.3; enc_chats by its two
// allowlisted columns only, spec 9), so a joined fake row is read at the indexes a database would give.
func joinedColumns(table string) []string {
	switch table {
	case tableUsers:
		return []string{"uid", "name", colStatus, colData}
	case tableChats:
		return []string{"uid", "name", colData}
	case tableEncChats:
		return encChatsColumns()
	case tableContacts:
		return []string{"uid", colMutual}
	case tableUsersData:
		return []string{"uid", colAbout}
	}
	return nil
}

// mapCtx knows the columns of the mapped table by the mapping's order and those of the joined tables
// (joinedColumns); every Get and Match answers with join, a hit with a row of the joined table holding hit's cells
// (cycled).
type mapCtx struct {
	in    *parse.Input
	table string
	cols  []parse.ColumnSpec
	join  joinOutcome
	hit   []cell
}

func (c mapCtx) Input() *parse.Input { return c.in }
func (mapCtx) Note(_, _ string)      {}
func (c mapCtx) Column(table, col string) int {
	if table != c.table {
		for i, n := range joinedColumns(table) {
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

// joined is the fake row of a joined table on a hit: one cell per joinedColumns entry, from hit (cycled; NULL when
// hit is empty).
func (c mapCtx) joined(table string) fakeRow {
	cs := make([]cell, len(joinedColumns(table)))
	for i := range cs {
		if len(c.hit) > 0 {
			cs[i] = c.hit[i%len(c.hit)]
		}
	}
	return fakeRow{table: table, cells: cs}
}

func (c mapCtx) Get(table string, _ int64) (parse.Row, bool, error) {
	if c.join == joinHit && joinedColumns(table) != nil {
		return c.joined(table), true, nil
	}
	return nil, false, c.join.err()
}

func (c mapCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	if c.join == joinHit && joinedColumns(table) != nil {
		return []parse.Row{c.joined(table)}, nil
	}
	return nil, c.join.err()
}

const fakeRemote = "/data/data/org.telegram.messenger/files/cache4.db"

func fakeInput() *parse.Input {
	return &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: "android:" + fakeRemote, Source: parse.SourceInfo{RemotePath: fakeRemote}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: fakeRemote}}},
		Budget:    parse.NewBudget(1 << 30).View(),
	}
}

// mappingUnderTest is one mapping and whether it is the live one (a Parse env: its joins reach the context).
type mappingUnderTest struct {
	m    parse.TableMapping
	live bool
}

// mappingsUnderTest are the typed pass's three mappings (a nil env, 4I4-P6: a recovered row joins nothing) and the
// live ones (an empty guard, one constructor table as Parse builds it).
func mappingsUnderTest() []mappingUnderTest {
	tab := newTable()
	e := &env{Account: "0"}
	out := []mappingUnderTest{}
	for _, m := range (Parser{}).Mappings() {
		out = append(out, mappingUnderTest{m: m})
	}
	return append(out,
		mappingUnderTest{messageMapping(e, tab, tableMessages), true},
		mappingUnderTest{messageMapping(e, tab, tableMessagesV2), true},
		mappingUnderTest{userMapping(e, tab), true},
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

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed row error or exactly one
// record that validates against its type, carries its place, stamps no recovery and invents nothing the cells do not
// say (a guid only from an INTEGER mid, a thread only from an INTEGER uid, a time only from an INTEGER date, a Body
// only from a BLOB data cell); a join the context cannot answer is undecided, never "no row", and on a live message
// row whose INTEGER uid names a dialog that joins (user, secret, negative) it is named in raw.join_undecided (Task 13
// review M3). On a hit the joined row holds the row's own cells.
func checkMapped(t testing.TB, mt mappingUnderTest, join joinOutcome, cells []cell) {
	t.Helper()
	mp := mt.m
	ctx := mapCtx{in: fakeInput(), table: mp.Table, cols: mp.Columns, join: join, hit: cells}
	recs, err := mp.Map(ctx, fakeRow{table: mp.Table, cells: cells})
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("%s: untyped error %v", mp.Table, err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%s: %d records: every row is one record (spec 8.5, 8.7, 8.9)", mp.Table, len(recs))
	}
	r := recs[0]
	if r.ArtifactID != "a1" || r.SourcePath != fakeRemote || r.Range == nil || r.Locator != "sqlite:table="+mp.Table+";rowid=1" {
		t.Fatalf("%s: record lost its place: %+v", mp.Table, r)
	}
	var verr error
	switch r.Type {
	case message.Type:
		verr = message.Validate(r.Payload)
	case call.Type:
		verr = call.Validate(r.Payload)
	case contact.Type:
		verr = contact.Validate(r.Payload)
	default:
		t.Fatalf("%s: a record of type %q", mp.Table, r.Type)
	}
	if verr != nil {
		t.Fatalf("%s: payload refused: %v", mp.Table, verr)
	}
	if r.Recovery != "" || r.Confidence != nil || r.Deleted {
		t.Errorf("%s: the mapping stamped a recovery or a deletion: %+v", mp.Table, r)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	for k, v := range raw {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "no-") && strings.HasSuffix(s, "-row") && join != joinMiss {
			t.Errorf("%s: raw.%s = %q while the join answers %d: an unanswered join is undecided, never no row", mp.Table, k, s, join)
		}
	}
	if (!mt.live || join == joinMiss || join == joinHit) && raw["join_undecided"] != nil {
		t.Errorf("%s: join_undecided %v on a clean miss, a hit or with no join", mp.Table, raw["join_undecided"])
	}
	if mp.Table == tableUsers {
		return
	}
	if uid := cellOf(mp.Columns, cells, "uid"); mt.live && (join == joinNoTable || join == joinDamaged) && uid.kind == cInt {
		switch c, _ := dialogOf(mp.Table, uid.i); c {
		case classUser, classSecret, classNegative:
			if raw["join_undecided"] == nil {
				t.Errorf("%s: a %s dialog (uid %d) whose join answers %d has no raw.join_undecided", mp.Table, c, uid.i, join)
			}
		}
	}
	if r.Type == message.Type && r.Payload["guid"] != nil && cellOf(mp.Columns, cells, "mid").kind != cInt {
		t.Errorf("%s: guid %v from a mid cell of kind %d", mp.Table, r.Payload["guid"], cellOf(mp.Columns, cells, "mid").kind)
	}
	if r.Payload["thread"] != nil && cellOf(mp.Columns, cells, "uid").kind != cInt {
		t.Errorf("%s: a thread from a uid cell of kind %d", mp.Table, cellOf(mp.Columns, cells, "uid").kind)
	}
	if r.Time != nil && cellOf(mp.Columns, cells, "date").kind != cInt {
		t.Errorf("%s: a time from a date cell of kind %d", mp.Table, cellOf(mp.Columns, cells, "date").kind)
	}
	if r.Body != "" && cellOf(mp.Columns, cells, "data").kind != cBlob {
		t.Errorf("%s: a Body from a data cell of kind %d", mp.Table, cellOf(mp.Columns, cells, "data").kind)
	}
}

// goodCells is a row that maps to a timed record: INTEGER columns 0 (mid 7, uid 1000002, date D, read_state 3), the
// data cell a decodable TL_message (a TL_user for users), the other BLOB columns NULL.
func goodCells(mp parse.TableMapping) []cell {
	cs := make([]cell, len(mp.Columns))
	for i, s := range mp.Columns {
		switch s.Affinity {
		case "integer":
			cs[i] = cell{kind: cInt}
		default:
			cs[i] = cell{kind: cNull}
		}
		switch s.Name {
		case "mid":
			cs[i].i = 7
		case "uid":
			cs[i].i = 1000002
		case "date":
			cs[i].i = 1700000000
		case "read_state":
			cs[i].i = 3
		case colData:
			cs[i] = cell{kind: cBlob, b: goodBlob(mp.Table)}
		}
	}
	return cs
}

// goodBlob is a TL_message from user 1000002 to 1000001 (a TL_user 1000002 named "Ann" for users), written here from
// the wire facts of spec 6.5 (constructors.tsv field order).
func goodBlob(table string) []byte {
	le32 := func(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
	le64 := func(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
	if table == tableUsers {
		b := le32(le32(le32(nil, 0xb1b8cc83), 1<<1), 0) // TL_user, flags (first_name), flags2
		b = le64(b, 1000002)
		return append(b, 3, 'A', 'n', 'n')
	}
	b := le32(le32(le32(nil, 0x7600b9d3), 1<<8), 0) // TL_message, flags (from_id), flags2
	b = le32(b, 7)
	b = le64(le32(b, 0x59511722), 1000002) // from_id
	b = le64(le32(b, 0x59511722), 1000001) // peer_id
	b = le32(b, 1700000000)
	return append(b, 2, 'h', 'i', 0)
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column
// for the integer and float, texts is split at NUL bytes for the TEXT cells, and blob is every BLOB cell.
func cellsFrom(n int, kinds, nums, texts, blob []byte) []cell {
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
		if cs[i].kind == cBlob {
			cs[i].b = blob
		}
	}
	return cs
}

// Every mapping maps a good row to one timed record (users: one contact), and every storage class in every column,
// alone or in all columns, a NaN, the extreme integers and every join answer keep the contract (checkMapped).
func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for _, mt := range mappingsUnderTest() {
		mp := mt.m
		recs, err := mp.Map(mapCtx{in: fakeInput(), table: mp.Table, cols: mp.Columns}, fakeRow{table: mp.Table, cells: goodCells(mp)})
		if err != nil || len(recs) != 1 || (mp.Table != tableUsers && (recs[0].Time == nil || recs[0].Body != "hi")) {
			t.Fatalf("%s (live %v): a good row did not map to its record: %v %+v", mp.Table, mt.live, err, recs)
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

// hostileBlobs are the data blobs of telegram-hostile, in rowid order (the fuzz seeds of FuzzMapAndroidTelegramRows).
func hostileBlobs(t testing.TB) [][]byte {
	t.Helper()
	data := parsertest.LoadFixture(t, "testdata", "telegram-hostile").Files["db"]
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), tableMessagesV2, []string{colData}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	i := tb.Col(colData)
	err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
		if b, ok := row.Blob(i); ok {
			out = append(out, bytes.Clone(b))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// FuzzMapAndroidTelegramRows fuzzes the message and users mappings, typed and live, on decoded column values
// directly: every column takes any storage class and value, every BLOB cell the fuzzed blob (seeded with the
// telegram-hostile blobs), and which picks the mapping and the context's join answer.
func FuzzMapAndroidTelegramRows(f *testing.F) {
	// the message columns mid, uid, date, data, out, read_state, send_state, ttl, media, replydata (cycled): the blob
	// in data (and replies_data), every other cell the INTEGER 1000002
	ints := binary.LittleEndian.AppendUint64(nil, 1000002)
	for i, b := range hostileBlobs(f) {
		f.Add(byte(i), []byte{cInt, cInt, cInt, cBlob, cInt, cInt, cInt, cInt, cInt, cNull}, ints, []byte("x"), b)
	}
	f.Add(byte(0), []byte{cNull, cFloat, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"), []byte{})
	f.Add(byte(5), []byte{cBlob}, []byte{}, []byte{}, goodBlob(tableUsers))
	f.Fuzz(func(t *testing.T, which byte, kinds, nums, texts, blob []byte) {
		ms := mappingsUnderTest()
		mt := ms[int(which)%len(ms)]
		join := joinOutcome(which>>4) % numOutcomes
		checkMapped(t, mt, join, cellsFrom(len(mt.m.Columns), kinds, nums, texts, blob))
	})
}
