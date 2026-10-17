package androidcontacts

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
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

// fakeRow is a decoded row: the mapping sees only the parse.Row interface, so its behaviour on any storage class
// in any column is fuzzed here without a database.
type fakeRow struct {
	table string
	id    int64
	cells []cell
}

func (r fakeRow) Table() string        { return r.table }
func (r fakeRow) Rowid() (int64, bool) { return r.id, true }
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

func (fakeRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}

func (r fakeRow) Locator() (string, bool) { return "row:rowid=" + strconv.FormatInt(r.id, 10), true }

// fakeCtx answers the joins of a mapped raw contact from rows the test supplies. The count of accounts and
// contacts rows is the test's: none is a dangling key, one a decided join, two a duplicate key. inline selects an
// old schema whose raw_contacts keeps account_name and account_type itself.
type fakeCtx struct {
	in     *parse.Input
	inline bool
	acc    []parse.Row
	agg    []parse.Row
	data   []parse.Row
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}

func indexOf(names []string, col string) int {
	for i, n := range names {
		if n == col {
			return i
		}
	}
	return -1
}

func (c fakeCtx) Column(table, col string) int {
	switch table {
	case "raw_contacts":
		if !c.inline && (col == "account_name" || col == "account_type") {
			return -1
		}
		return indexOf(specNames(rawContactSpecs()), col)
	case "accounts":
		return indexOf([]string{"account_name", "account_type"}, col)
	case "contacts":
		return indexOf([]string{"contact_last_updated_timestamp"}, col)
	case "data":
		return indexOf(dataNames(), col)
	case "mimetypes":
		return indexOf([]string{"mimetype"}, col)
	}
	return -1
}

// the three mimetypes the fake database knows, by id
var fakeMimes = map[int64]string{1: mimePrefix + "phone_v2", 2: mimePrefix + "email_v2", 3: mimePrefix + "name"}

func (fakeCtx) Get(table string, id int64) (parse.Row, bool, error) {
	if m, ok := fakeMimes[id]; ok && table == "mimetypes" {
		return fakeRow{table: table, id: id, cells: []cell{{kind: cText, b: []byte(m)}}}, true, nil
	}
	return nil, false, nil
}

func (c fakeCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	switch table {
	case "accounts":
		return c.acc, nil
	case "contacts":
		return c.agg, nil
	case "data":
		return c.data, nil
	}
	return nil, nil
}

// Column indexes of raw_contacts, in rawContactSpecs order.
const (
	colDisplayName = iota
	colAccountID
	colAccountName
	colAccountType
	colContactID
	colStarred
	colDeleted
	colTimesContacted
	colSourceID
)

// Column indexes of data.
const (
	dMime    = 0
	dPrimary = 1
	dData1   = 2
)

type world struct {
	rc     []cell
	inline bool
	acc    [][]cell
	agg    [][]cell
	data   [][]cell
}

func (w world) run() ([]records.Record, error) {
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/contacts2.db"}}}}
	ctx := fakeCtx{in: in, inline: w.inline}
	for i, cs := range w.acc {
		ctx.acc = append(ctx.acc, fakeRow{table: "accounts", id: int64(i + 1), cells: cs})
	}
	for i, cs := range w.agg {
		ctx.agg = append(ctx.agg, fakeRow{table: "contacts", id: int64(i + 1), cells: cs})
	}
	for i, cs := range w.data {
		ctx.data = append(ctx.data, fakeRow{table: "data", id: int64(i + 1), cells: cs})
	}
	return mapRawContact(ctx, fakeRow{table: "raw_contacts", id: 1, cells: w.rc})
}

// checkMapped holds the mapping to its contract on any world: no panic, and either a typed row error (a counted
// warning) or exactly one record that validates, carries its place, and invents nothing the cells do not say.
func checkMapped(t testing.TB, w world) {
	t.Helper()
	recs, err := w.run()
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
	if r.Type != contact.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/contacts2.db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := contact.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	rc := w.rc
	isInt := func(c int) bool { return rc[c].kind == cInt }
	rawMap, _ := r.Payload["raw"].(map[string]any)
	unknown, _ := rawMap["unknown_columns"].(map[string]any)
	joinedUnknown, _ := rawMap["joined_unknown_columns"].(map[string]any)
	undecidedHas := func(table string) bool {
		l, _ := rawMap["join_undecided"].([]any)
		for _, v := range l {
			if v == table {
				return true
			}
		}
		return false
	}

	// T69: the provider holds no time of the raw contact itself; Record.Time is never set
	if r.Time != nil || r.TimeEnd != nil {
		t.Errorf("a raw contact has a Record.Time: %v %v", r.Time, r.TimeEnd)
	}
	if name, has := r.Payload["display_name"]; has && (rc[colDisplayName].kind != cText || name == "") {
		t.Errorf("a display_name %q from a cell of kind %d", name, rc[colDisplayName].kind)
	}
	if s, has := r.Payload["starred"]; has && (!isInt(colStarred) || (rc[colStarred].i != 0 && rc[colStarred].i != 1) || s != (rc[colStarred].i == 1)) {
		t.Errorf("starred %v from cell %+v", s, rc[colStarred])
	}
	if r.Deleted != (isInt(colDeleted) && rc[colDeleted].i != 0) {
		t.Errorf("Deleted %v from cell %+v", r.Deleted, rc[colDeleted])
	}
	if r.Recovery != "" || r.Confidence != nil {
		t.Errorf("a live row has Recovery %q Confidence %v", r.Recovery, r.Confidence)
	}
	if r.Payload["source_id"] != "1" {
		t.Errorf("source_id %v", r.Payload["source_id"])
	}
	if ids, has := r.Payload["source_ids"]; has && (rc[colSourceID].kind != cText || len(ids.([]any)) != 1) {
		t.Errorf("source_ids %v from a sourceid cell of kind %d", ids, rc[colSourceID].kind)
	}

	// the account: from the accounts row or the inline columns, never invented, and an unknown is never absence
	accounts, _ := r.Payload["accounts"].([]any)
	_, hasState := rawMap["account_state"]
	switch {
	case w.inline:
		if len(accounts) > 0 && rc[colAccountName].kind != cText && rc[colAccountType].kind != cText {
			t.Errorf("an inline account from cells of kind %d and %d", rc[colAccountName].kind, rc[colAccountType].kind)
		}
		if hasState || undecidedHas("accounts") {
			t.Errorf("an inline schema states a join: %v", rawMap)
		}
	case !isInt(colAccountID):
		if len(accounts) > 0 || hasState || undecidedHas("accounts") || rawMap["account_id"] != nil {
			t.Errorf("an account from an account_id cell of kind %d: %v", rc[colAccountID].kind, rawMap)
		}
		if k := rc[colAccountID].kind; (k == cFloat || k == cText || k == cBlob) && unknown["account_id"] != "type-mismatch" {
			t.Errorf("an account_id of kind %d is not flagged: %v", k, unknown)
		}
	default:
		switch len(w.acc) {
		case 0:
			if len(accounts) > 0 || rawMap["account_state"] != "no-accounts-row" || undecidedHas("accounts") {
				t.Errorf("a dangling account_id: %v", rawMap)
			}
		case 1:
			if dup, _ := rawMap["join_duplicate"].(map[string]any); hasState || dup["accounts"] != nil {
				t.Errorf("a decided account join states %v", rawMap)
			}
			if len(accounts) > 0 && w.acc[0][0].kind != cText && w.acc[0][1].kind != cText {
				t.Errorf("an account from cells of kind %d and %d", w.acc[0][0].kind, w.acc[0][1].kind)
			}
			for col, name := range []string{"accounts.account_name", "accounts.account_type"} {
				if k := w.acc[0][col].kind; (k == cFloat || k == cInt || k == cBlob) && joinedUnknown[name] != "type-mismatch" {
					t.Errorf("%s of kind %d is not flagged: %v", name, k, joinedUnknown)
				}
			}
		default:
			dup, _ := rawMap["join_duplicate"].(map[string]any)
			if len(accounts) > 0 || hasState || !undecidedHas("accounts") || dup["accounts"] != int64(len(w.acc)) {
				t.Errorf("a duplicate account key: %v", rawMap)
			}
		}
	}

	// the aggregate time: from the contacts row only, and its absence is stated
	at := func() (time.Time, bool) {
		for _, nt := range r.Times {
			if nt.Kind == contact.AggregateLastUpdated {
				return nt.Time.T, true
			}
		}
		return time.Time{}, false
	}
	got, hasTime := at()
	if len(r.Times) > 1 || (len(r.Times) == 1 && !hasTime) {
		t.Errorf("times %v", r.Times)
	}
	_, hasAgg := rawMap["aggregate_state"]
	switch {
	case !isInt(colContactID):
		if hasTime || undecidedHas("contacts") || rawMap["contact_id"] != nil || rawMap["contact_last_updated_timestamp"] != nil {
			t.Errorf("an aggregate from a contact_id cell of kind %d: %v", rc[colContactID].kind, rawMap)
		}
		if k := rc[colContactID].kind; k == cNull && rawMap["aggregate_state"] != "no-contact-id" {
			t.Errorf("a NULL contact_id is not stated: %v", rawMap)
		} else if k != cNull && hasAgg {
			t.Errorf("aggregate_state %v for a contact_id of kind %d (a wrong class lives in unknown_columns)", rawMap["aggregate_state"], k)
		}
		if k := rc[colContactID].kind; (k == cFloat || k == cText || k == cBlob) && unknown["contact_id"] != "type-mismatch" {
			t.Errorf("a contact_id of kind %d is not flagged: %v", k, unknown)
		}
	case len(w.agg) == 0:
		if hasTime || rawMap["aggregate_state"] != "no-contacts-row" || undecidedHas("contacts") {
			t.Errorf("a dangling contact_id: %v", rawMap)
		}
	case len(w.agg) == 1:
		ts := w.agg[0][0]
		if dup, _ := rawMap["join_duplicate"].(map[string]any); hasAgg || dup["contacts"] != nil {
			t.Errorf("a decided contacts join states %v", rawMap)
		}
		if hasTime && (ts.kind != cInt || !got.Equal(time.UnixMilli(ts.i))) {
			t.Errorf("aggregate time %v from cell %+v", got, ts)
		}
		if (ts.kind == cFloat || ts.kind == cText || ts.kind == cBlob) && (hasTime || joinedUnknown["contacts.contact_last_updated_timestamp"] != "type-mismatch") {
			t.Errorf("a contact_last_updated_timestamp of kind %d: time %v, joined_unknown %v", ts.kind, hasTime, joinedUnknown)
		}
	default:
		dup, _ := rawMap["join_duplicate"].(map[string]any)
		if hasTime || hasAgg || !undecidedHas("contacts") || dup["contacts"] != int64(len(w.agg)) {
			t.Errorf("a duplicate contact key: %v", rawMap)
		}
	}

	// the typed values: only what the data rows say
	for _, key := range []string{"phones", "emails"} {
		if l, _ := r.Payload[key].([]any); len(l) > len(w.data) {
			t.Errorf("%d %s from %d data rows", len(l), key, len(w.data))
		}
	}
	if len(w.data) == 0 {
		for _, key := range []string{"phones", "emails", "urls", "ims", "addresses", "names", "organization", "photo"} {
			if _, has := r.Payload[key]; has {
				t.Errorf("%s from no data rows", key)
			}
		}
	}
	if n, _ := rawMap["data_rows"].(int64); n != int64(len(w.data)) {
		t.Errorf("data_rows %v for %d rows", rawMap["data_rows"], len(w.data))
	}

	// a cell of a storage class the mapping does not read is named, never silent
	for col, key := range map[int]string{colStarred: "starred", colDeleted: "deleted", colTimesContacted: "x_times_contacted"} {
		if k := rc[col].kind; (k == cFloat || k == cText || k == cBlob) && unknown[key] != "type-mismatch" {
			t.Errorf("%s of kind %d is not flagged: %v", key, k, unknown)
		}
	}
	for col, key := range map[int]string{colDisplayName: "display_name", colSourceID: "sourceid"} {
		if k := rc[col].kind; (k == cInt || k == cFloat || k == cBlob) && unknown[key] != "type-mismatch" {
			t.Errorf("%s of kind %d is not flagged: %v", key, k, unknown)
		}
	}
}

func goodWorld() world {
	n := len(rawContactSpecs())
	rc := make([]cell, n)
	rc[colDisplayName] = cell{kind: cText, b: []byte("Alice Example")}
	rc[colAccountID] = cell{kind: cInt, i: 1}
	rc[colAccountName] = cell{kind: cText, b: []byte("a@example.test")}
	rc[colAccountType] = cell{kind: cText, b: []byte("com.google")}
	rc[colContactID] = cell{kind: cInt, i: 1}
	rc[colStarred] = cell{kind: cInt, i: 1}
	rc[colDeleted] = cell{kind: cInt, i: 0}
	rc[colTimesContacted] = cell{kind: cInt, i: 3}
	rc[colSourceID] = cell{kind: cText, b: []byte("g-1")}
	phone := make([]cell, len(dataNames()))
	phone[dMime] = cell{kind: cInt, i: 1}
	phone[dPrimary] = cell{kind: cInt, i: 1}
	phone[dData1] = cell{kind: cText, b: []byte("+15551234567")}
	for i := dData1 + 1; i < len(phone); i++ {
		phone[i] = cell{kind: cNull}
	}
	return world{
		rc:   rc,
		acc:  [][]cell{{{kind: cText, b: []byte("a@example.test")}, {kind: cText, b: []byte("com.google")}}},
		agg:  [][]cell{{{kind: cInt, i: 1682935201000}}},
		data: [][]cell{phone},
	}
}

// cellsFrom builds n cells from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column for the
// integer and float, texts is split at NUL bytes. salt moves the starting point, so the rows of one world differ.
func cellsFrom(n, salt int, kinds, nums, texts []byte) []cell {
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
		j := i + salt
		if len(kinds) > 0 {
			cs[i].kind = kinds[j%len(kinds)] % numKinds
		}
		var w [8]byte
		if len(nums) > 0 {
			for k := range w {
				w[k] = nums[(j*8+k)%len(nums)]
			}
		}
		u := binary.LittleEndian.Uint64(w[:])
		cs[i].i, cs[i].f = int64(u), math.Float64frombits(u)
		cs[i].b = parts[j%len(parts)]
	}
	return cs
}

// worldFrom builds a world from fuzz bytes: shape picks the schema (inline or joined), how many accounts, contacts
// and data rows the fake database holds, and the cells of every row come from kinds, nums and texts.
func worldFrom(shape, kinds, nums, texts []byte) world {
	sh := func(i int) int {
		if i < len(shape) {
			return int(shape[i])
		}
		return 1
	}
	w := world{inline: sh(0)%2 == 1}
	w.rc = cellsFrom(len(rawContactSpecs()), 0, kinds, nums, texts)
	for i := 0; i < sh(1)%3; i++ {
		w.acc = append(w.acc, cellsFrom(2, 10+3*i, kinds, nums, texts))
	}
	for i := 0; i < sh(2)%3; i++ {
		w.agg = append(w.agg, cellsFrom(1, 20+3*i, kinds, nums, texts))
	}
	for i := 0; i < sh(3)%4; i++ {
		w.data = append(w.data, cellsFrom(len(dataNames()), 30+17*i, kinds, nums, texts))
	}
	return w
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	w := goodWorld()
	recs, err := w.run()
	if err != nil || len(recs) != 1 || recs[0].Time != nil || len(recs[0].Times) != 1 || recs[0].Times[0].Kind != contact.AggregateLastUpdated {
		t.Fatalf("err %v, records %+v", err, recs)
	}
	if acc, _ := recs[0].Payload["accounts"].([]any); len(acc) != 1 {
		t.Fatalf("accounts %v", recs[0].Payload["accounts"])
	}
	checkMapped(t, w)
	hostile := func(k byte) cell { return cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}} }
	for _, k := range []byte{cNull, cFloat, cText, cBlob, cInt} {
		for _, inline := range []bool{false, true} {
			// one cell of every table at a time, then every cell at once
			for col := range goodWorld().rc {
				w := goodWorld()
				w.inline = inline
				w.rc[col] = hostile(k)
				checkMapped(t, w)
			}
			for col := 0; col < 2; col++ {
				w := goodWorld()
				w.inline = inline
				w.acc[0][col] = hostile(k)
				checkMapped(t, w)
			}
			w := goodWorld()
			w.inline = inline
			w.agg[0][0] = hostile(k)
			checkMapped(t, w)
			for col := range w.data[0] {
				w := goodWorld()
				w.inline = inline
				w.data[0][col] = hostile(k)
				checkMapped(t, w)
			}
			all := goodWorld()
			all.inline = inline
			for _, cs := range append(append([][]cell{all.rc}, all.acc...), append(all.agg, all.data...)...) {
				for i := range cs {
					cs[i] = hostile(k)
				}
			}
			checkMapped(t, all)
		}
	}
	// the number of rows a key matches: none (dangling), one, two (a duplicate)
	for na := 0; na < 3; na++ {
		for ng := 0; ng < 3; ng++ {
			w := goodWorld()
			w.acc, w.agg = nil, nil
			for i := 0; i < na; i++ {
				w.acc = append(w.acc, goodWorld().acc[0])
			}
			for i := 0; i < ng; i++ {
				w.agg = append(w.agg, goodWorld().agg[0])
			}
			checkMapped(t, w)
			for _, k := range []byte{cNull, cText} {
				w.rc[colAccountID], w.rc[colContactID] = cell{kind: k, b: []byte("1")}, cell{kind: k, b: []byte("1")}
				checkMapped(t, w)
			}
		}
	}
	for _, v := range []int64{math.MinInt64, -(1 << 53) - 1, -1, 0, 1 << 53, 1<<53 + 1, math.MaxInt64} {
		w := goodWorld()
		w.agg[0][0] = cell{kind: cInt, i: v}
		w.rc[colStarred] = cell{kind: cInt, i: v}
		w.rc[colDeleted] = cell{kind: cInt, i: v}
		checkMapped(t, w)
	}
}

// FuzzMapRows fuzzes mapRawContact on decoded column values directly: every column of every table of the fake
// database takes any storage class and value, and the number of accounts, contacts and data rows varies.
func FuzzMapRows(f *testing.F) {
	f.Add([]byte{0, 1, 1, 1}, []byte{cText, cInt, cText, cText, cInt, cInt, cInt, cInt, cText}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("Alice\x00a@x\x00g-1"))
	f.Add([]byte{1, 1, 1, 2}, []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add([]byte{0, 0, 0, 0}, []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add([]byte{0, 2, 2, 3}, []byte{cText, cInt, cInt, cText, cInt, cInt, cInt, cText}, []byte{1, 0, 0, 0, 0, 0, 0x20, 0}, []byte("5\x005\x00x\x00y"))
	f.Add([]byte{0, 1, 1, 1}, []byte{cText, cText, cText, cText, cFloat, cFloat, cBlob, cBlob, cText}, []byte{7, 0, 0, 0, 0, 0, 0, 0}, []byte("x"))
	f.Fuzz(func(t *testing.T, shape, kinds, nums, texts []byte) {
		checkMapped(t, worldFrom(shape, kinds, nums, texts))
	})
}
