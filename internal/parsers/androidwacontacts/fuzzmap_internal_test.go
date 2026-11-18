package androidwacontacts

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

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

// fakeRow is a decoded row: the mapping sees only the parse.Row interface, so its behaviour on any storage class in
// any column is fuzzed here without a database.
type fakeRow struct{ cells []cell }

func (fakeRow) Table() string        { return "wa_contacts" }
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

// fakeCtx knows the columns of the wa_contacts table by their spec order.
type fakeCtx struct{ in *parse.Input }

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (fakeCtx) Column(table, col string) int {
	if table != "wa_contacts" {
		return -1
	}
	for i, s := range contactSpecs() {
		if s.Name == col {
			return i
		}
	}
	return -1
}
func (fakeCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

// Column indexes, in contactSpecs order.
const (
	colJID = iota
	colIsUser
	colStatus
	colStatusTS
	colNumber
	colRawContact
	colDisplay
	colPhoneType
	colPhoneLabel
	colUnseen
	colThumbTS
	colPhotoTS
	colGiven
	colFamily
	colWAName
	colSortName
	colNickname
	colCompany
	colTitle
)

func mapCells(cells []cell) ([]records.Record, error) {
	in := &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: 100, Logical: "android:/data/data/com.whatsapp/databases/wa.db", Source: parse.SourceInfo{RemotePath: "/data/data/com.whatsapp/databases/wa.db"}},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/data/data/com.whatsapp/databases/wa.db"}}},
	}
	return mapContact(fakeCtx{in: in}, fakeRow{cells: cells})
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed row error (a counted warning)
// or exactly one record that validates, carries its place, and invents nothing the cells do not say.
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
	if r.Type != contact.Type || r.ArtifactID != "a1" || r.SourcePath != "/data/data/com.whatsapp/databases/wa.db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if r.Time != nil || r.TimeEnd != nil || len(r.Times) != 0 || r.Deleted || r.Recovery != "" {
		t.Errorf("a time, deleted marker or recovery stamp on a contact: %+v", r)
	}
	if err := contact.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	isText := func(c int) bool { return cells[c].kind == cText && len(cells[c].b) > 0 }
	if cells[colJID].kind != cText {
		t.Errorf("a record from a JID cell of kind %d", cells[colJID].kind)
	}
	if !isText(colDisplay) && !isText(colWAName) && !isText(colNumber) {
		t.Errorf("a record from a row with no display name, push name or number: %+v", cells)
	}
	if _, has := r.Payload["display_name"]; has && !isText(colDisplay) && !isText(colWAName) {
		t.Errorf("a display name from cells of kinds %d and %d", cells[colDisplay].kind, cells[colWAName].kind)
	}
	if _, has := r.Payload["phones"]; has != isText(colNumber) {
		t.Errorf("phones %v from a number cell of kind %d", has, cells[colNumber].kind)
	}
	rawMap, _ := r.Payload["raw"].(map[string]any)
	var wantSrc any
	switch {
	case isText(colDisplay):
		wantSrc = "address_book"
	case isText(colWAName):
		wantSrc = "wa_name_push_name"
	}
	if rawMap["display_name_source"] != wantSrc {
		t.Errorf("display_name_source %v, want %v", rawMap["display_name_source"], wantSrc)
	}
	if _, has := rawMap["label_is_whatsapp_user"]; has && (cells[colIsUser].kind != cInt || cells[colIsUser].i != 1) {
		t.Errorf("a label for an is_whatsapp_user cell %+v", cells[colIsUser])
	}
	if cells[colIsUser].kind == cInt && cells[colIsUser].i == 1 && rawMap["label_is_whatsapp_user"] != "whatsapp_user" {
		t.Errorf("is_whatsapp_user 1 without its label: %v", rawMap)
	}
	// a name cell that is not text is named, never silent
	unknown, _ := rawMap["unknown_columns"].(map[string]any)
	for col, key := range map[int]string{colDisplay: "display_name", colWAName: "wa_name", colNumber: "number", colGiven: "given_name", colFamily: "family_name"} {
		if k := cells[col].kind; (k == cInt || k == cFloat || k == cBlob) && unknown[key] != "type-mismatch" {
			t.Errorf("%s of kind %d is not flagged: %v", key, k, unknown)
		}
	}
	if k := cells[colIsUser].kind; (k == cFloat || k == cText || k == cBlob) && unknown["is_whatsapp_user"] != "type-mismatch" {
		t.Errorf("is_whatsapp_user of kind %d is not flagged: %v", k, unknown)
	}
	// the Body states only what the cells say
	if !strings.Contains(r.Body, string(cells[colJID].b)) {
		t.Errorf("the body %q does not hold the JID", r.Body)
	}
}

func goodCells() []cell {
	cs := make([]cell, len(contactSpecs()))
	for i, s := range contactSpecs() {
		if s.Affinity == "integer" {
			cs[i] = cell{kind: cInt, i: 1}
		} else {
			cs[i] = cell{kind: cText, b: []byte("Ann")}
		}
	}
	cs[colJID] = cell{kind: cText, b: []byte("15550000001@s.whatsapp.net")}
	cs[colNumber] = cell{kind: cText, b: []byte("+15550000001")}
	return cs
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a column
// for the integer and float, texts is split at NUL bytes.
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
	if err != nil || len(recs) != 1 || recs[0].Payload["display_name"] != "Ann" {
		t.Fatalf("err %v, records %+v", err, recs)
	}
	checkMapped(t, goodCells())
	for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
		for col := range goodCells() {
			cs := goodCells()
			cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe}}
			checkMapped(t, cs)
		}
		cs := goodCells()
		for i := range cs {
			cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
		}
		checkMapped(t, cs)
	}
	for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, math.MaxInt64} {
		cs := goodCells()
		for i := range cs {
			if cs[i].kind == cInt {
				cs[i].i = v
			}
		}
		checkMapped(t, cs)
	}
	for _, jid := range []string{"", "@", "a@", "@s.whatsapp.net", "1:x@s.whatsapp.net", "1.999:1@s.whatsapp.net", "x@g.us", "status@broadcast", "1@lid", "1@s.whatsapp.net\x00", "1@s.whatsapp.net\xff"} {
		cs := goodCells()
		cs[colJID] = cell{kind: cText, b: []byte(jid)}
		checkMapped(t, cs)
	}
}

// FuzzMapWAContactsRows fuzzes mapContact on decoded column values directly: every column of the row takes any
// storage class and value.
func FuzzMapWAContactsRows(f *testing.F) {
	f.Add([]byte{cText, cInt, cText, cInt, cText}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("15550000001@s.whatsapp.net\x00hi"))
	f.Add([]byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add([]byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add([]byte{cText}, []byte{0, 0, 0, 0, 0, 0, 0x20, 0}, []byte("1@lid\x00Ann\x00+1555"))
	f.Fuzz(func(t *testing.T, kinds, nums, texts []byte) {
		checkMapped(t, cellsFrom(len(contactSpecs()), kinds, nums, texts))
	})
}
