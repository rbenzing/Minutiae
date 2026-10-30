package iosaddressbook

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
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

func txt(s string) cell { return cell{kind: cText, b: []byte(s)} }
func num(i int64) cell  { return cell{kind: cInt, i: i} }

// fakeRow is a decoded row: the mapping sees only the parse.Row interface, so its behaviour on any storage
// class in any column is fuzzed here without a database.
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

func (fakeRow) Locator() (string, bool) { return "row:rowid=1", true }

// joinCols are the columns of the joined tables, by index; the person table's come from personSpecs.
var joinCols = map[string][]string{
	"ABMultiValue":         {"UID", "record_id", "property", "label", "value"},
	"ABMultiValueLabel":    {"value"},
	"ABMultiValueEntry":    {"parent_id", "key", "value"},
	"ABMultiValueEntryKey": {"value"},
	"ABStore":              {"Name"},
	"ABGroup":              {"Name"},
	"ABGroupMembers":       {"group_id", "member_type", "member_id"},
}

// joinTables is the order the fuzz input is cut into joined rows.
var joinTables = []string{"ABMultiValue", "ABMultiValueLabel", "ABMultiValueEntry", "ABMultiValueEntryKey", "ABStore", "ABGroup", "ABGroupMembers"}

// fakeCtx answers every join with the rows it was given for the table, whatever the key: the mapping must hold
// for any rows the engine could return.
type fakeCtx struct {
	in   *parse.Input
	rows map[string][]fakeRow
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (c fakeCtx) Column(table, col string) int {
	names := joinCols[table]
	if table == tablePerson {
		names = specNames(personSpecs())
	}
	return slices.Index(names, col)
}

func (c fakeCtx) Get(table string, _ int64) (parse.Row, bool, error) {
	if rs := c.rows[table]; len(rs) > 0 {
		return rs[0], true, nil
	}
	return nil, false, nil
}

func (c fakeCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	var out []parse.Row
	for _, r := range c.rows[table] {
		out = append(out, r)
	}
	return out, nil
}

// world is one fuzzed or hand-built situation: the person row and the rows every join returns.
type world struct {
	person []cell
	joins  map[string][]fakeRow
}

func (w world) cells() [][]cell {
	out := [][]cell{w.person}
	for _, t := range joinTables {
		for _, r := range w.joins[t] {
			out = append(out, r.cells)
		}
	}
	return out
}

func (w world) run() ([]records.Record, error) {
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/db"}}}}
	return mapPerson(fakeCtx{in: in, rows: w.joins}, fakeRow{table: tablePerson, cells: w.person})
}

func isNumber(c cell) bool { return c.kind == cInt || c.kind == cFloat }

func personCol(name string) int { return slices.Index(specNames(personSpecs()), name) }

// heldTexts is every text any TEXT cell of the world holds, cleaned the way the mapping cleans it, NUL separated.
func (w world) heldTexts() string {
	var sb strings.Builder
	for _, cs := range w.cells() {
		for _, c := range cs {
			if c.kind == cText {
				sb.WriteString(common.CleanText(c.b, maxText).Text)
				sb.WriteByte(0)
			}
		}
	}
	return sb.String()
}

// check holds the mapping to its contract on any world: no panic, and either a typed row error (a counted
// warning) or exactly one record that validates, carries its place, and invents nothing the cells do not say.
func (w world) check(t testing.TB) {
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
	if r.Type != contact.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := contact.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	for _, k := range []string{"birthday", "ims", "starred", "photo", "deleted", "recovery", "snapshot"} {
		if _, has := r.Payload[k]; has {
			t.Errorf("the mapper states %s, which no cell of an ABPerson row gives it", k)
		}
	}
	held := w.heldTexts()
	inHeld := func(where, v string) {
		t.Helper()
		if v != "" && !strings.Contains(held, v) {
			t.Errorf("%s = %q appears in no input cell", where, v)
		}
	}
	at := func(name string) cell { return w.person[personCol(name)] }
	raw := asMap(r.Payload["raw"])
	unknown := asMap(raw["unknown_columns"])

	// text values
	inHeldKey := func(m map[string]any, parent string, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if s, ok := m[k].(string); ok {
				inHeld(parent+"."+k, s)
			}
		}
	}
	names := asMap(r.Payload["names"])
	inHeldKey(names, "names", "given", "family", "middle", "prefix", "suffix", "nickname")
	if ph, _ := names["phonetic"].(string); ph != "" {
		for _, w := range strings.Fields(ph) {
			inHeld("names.phonetic word", w)
		}
	}
	inHeldKey(asMap(r.Payload["organization"]), "organization", "name", "title", "department")
	switch raw["display_name_source"] {
	case "stored":
		inHeld("display_name", r.Payload["display_name"].(string))
	case "composed":
		want := joinSpace(str(names["prefix"]), str(names["given"]), str(names["middle"]), str(names["family"]), str(names["suffix"]))
		if r.Payload["display_name"] != want {
			t.Errorf("composed display name %q, parts say %q", r.Payload["display_name"], want)
		}
	default:
		if _, has := r.Payload["display_name"]; has {
			t.Errorf("a display name %v without a source", r.Payload["display_name"])
		}
	}
	for _, list := range []string{"phones", "emails", "urls"} {
		for _, e := range asList(r.Payload[list]) {
			em := asMap(e)
			inHeldKey(em, list, "value")
			if l, ok := em["label"].(string); ok && l != "" && !strings.Contains(held, l) {
				t.Errorf("%s label %q appears in no input cell", list, l)
			}
		}
	}
	for _, e := range asList(r.Payload["addresses"]) {
		inHeldKey(asMap(e), "address", "formatted", "street", "city", "region", "postcode", "country")
	}
	for _, e := range asList(r.Payload["accounts"]) {
		inHeldKey(asMap(e), "account", "name")
	}
	for _, g := range asList(raw["groups"]) {
		inHeld("raw.groups", g.(string))
	}
	if n, ok := raw["Note"].(string); ok {
		inHeld("raw.Note", n)
	}
	for _, e := range asList(raw["other_values"]) {
		inHeldKey(asMap(e), "other_values", "value")
	}
	for _, id := range asList(r.Payload["source_ids"]) {
		inHeld("source_ids", id.(string))
	}

	// a text column holding another storage class gives no text and is named
	for _, n := range []string{"First", "Last", "Middle", "FirstPhonetic", "MiddlePhonetic", "LastPhonetic", "Organization", "Department", "Note", "JobTitle", "Nickname", "Prefix", "Suffix", "ExternalIdentifier", "ExternalUUID", "DisplayName", "guid"} {
		if k := at(n).kind; k == cInt || k == cFloat || k == cBlob {
			if unknown[n] != "type-mismatch" {
				t.Errorf("%s of kind %d: unknown_columns[%s] = %v", n, k, n, unknown[n])
			}
		}
	}
	for _, n := range []string{"Kind", "StoreID"} {
		if k := at(n).kind; k == cFloat || k == cText || k == cBlob {
			if unknown[n] != "type-mismatch" {
				t.Errorf("%s of kind %d: unknown_columns[%s] = %v", n, k, n, unknown[n])
			}
		}
	}
	if k := at("Kind"); raw["Kind"] != nil && (k.kind != cInt || raw["Kind"] != k.i) {
		t.Errorf("raw.Kind = %v from cell %+v", raw["Kind"], k)
	}
	b := at("Birthday")
	switch b.kind {
	case cInt:
		if raw["birthday"] != b.i {
			t.Errorf("raw.birthday = %v from cell %+v", raw["birthday"], b)
		}
	case cText:
		inHeld("raw.birthday", str(raw["birthday"]))
	case cFloat:
		if math.IsNaN(b.f) || math.IsInf(b.f, 0) {
			if raw["birthday"] != nil || raw["birthday_status"] != "invalid" || raw["birthday_ieee"] == nil {
				t.Errorf("a non-finite birthday is stated as a value: %v", raw)
			}
		}
	default:
		if raw["birthday"] != nil {
			t.Errorf("raw.birthday = %v from cell %+v", raw["birthday"], b)
		}
	}
	// a time needs a numeric date cell
	if r.Time != nil && !isNumber(at("CreationDate")) && !isNumber(at("ModificationDate")) {
		t.Errorf("a time %v from date cells %+v %+v", r.Time, at("CreationDate"), at("ModificationDate"))
	}
	for _, nt := range r.Times {
		col := "CreationDate"
		if nt.Kind == "modified" {
			col = "ModificationDate"
		}
		if !isNumber(at(col)) {
			t.Errorf("a %s time from a %s cell of kind %d", nt.Kind, col, at(col).kind)
		}
	}
}

func str(v any) string { s, _ := v.(string); return s }

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func asList(v any) []any { l, _ := v.([]any); return l }

// goodWorld is a complete address book entry: every column of the person holds a value that names it, and each
// join returns the rows that make a phone, an e-mail, a URL, an address, a store account and a group.
func goodWorld() world {
	var person []cell
	for _, s := range personSpecs() {
		if s.Affinity == "integer" {
			person = append(person, num(1))
		} else {
			person = append(person, txt("x-"+s.Name))
		}
	}
	w := world{person: person, joins: map[string][]fakeRow{}}
	w.person[personCol("CreationDate")] = cell{kind: cFloat, f: 700000000.5}
	w.person[personCol("ModificationDate")] = num(710000000)
	w.person[personCol("Birthday")] = txt("1815-12-10")
	w.person[personCol("DisplayName")] = cell{kind: cNull}
	row := func(t string, cs ...cell) fakeRow { return fakeRow{table: t, cells: cs} }
	w.joins["ABMultiValue"] = []fakeRow{
		row("ABMultiValue", num(1), num(1), num(3), num(1), txt("+15550001")),
		row("ABMultiValue", num(2), num(1), num(4), num(1), txt("a@b.test")),
		row("ABMultiValue", num(3), num(1), num(22), num(1), txt("http://x.test")),
		row("ABMultiValue", num(4), num(1), num(5), num(1), txt("x-Formatted")),
	}
	w.joins["ABMultiValueLabel"] = []fakeRow{row("ABMultiValueLabel", txt("_$!<Mobile>!$_"))}
	w.joins["ABMultiValueEntry"] = []fakeRow{row("ABMultiValueEntry", num(4), num(1), txt("Main St"))}
	w.joins["ABMultiValueEntryKey"] = []fakeRow{row("ABMultiValueEntryKey", txt("Street"))}
	w.joins["ABStore"] = []fakeRow{row("ABStore", txt("Local"))}
	w.joins["ABGroup"] = []fakeRow{row("ABGroup", txt("Family"))}
	w.joins["ABGroupMembers"] = []fakeRow{row("ABGroupMembers", num(1), num(0), num(1))}
	return w
}

func (w world) clone() world {
	c := world{person: slices.Clone(w.person), joins: map[string][]fakeRow{}}
	for t, rs := range w.joins {
		for _, r := range rs {
			c.joins[t] = append(c.joins[t], fakeRow{table: r.table, cells: slices.Clone(r.cells)})
		}
	}
	return c
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes
// a column for the integer and float, texts is split at NUL bytes. off rotates the input so rows differ.
func cellsFrom(n, off int, kinds, nums, texts []byte) []cell {
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
		j := i + off
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

// fuzzWorld builds a world from fuzz bytes: sel gives the number of rows (0 to 3) each join returns, and its top
// bit keeps the integer cells of joined rows small so the property, member type and key paths are reached.
func fuzzWorld(sel byte, kinds, nums, texts []byte) world {
	w := world{person: cellsFrom(len(personSpecs()), 0, kinds, nums, texts), joins: map[string][]fakeRow{}}
	off := 7
	for ti, t := range joinTables {
		n := int(sel>>(ti%4*2)) & 3
		if ti >= 4 {
			n = int(sel>>(ti-4)) & 1
		}
		for range n {
			cs := cellsFrom(len(joinCols[t]), off, kinds, nums, texts)
			off += 3
			if sel&0x80 != 0 {
				for i := range cs {
					if cs[i].kind == cInt {
						cs[i].i = int64(uint64(cs[i].i) % 25)
					}
				}
			}
			w.joins[t] = append(w.joins[t], fakeRow{table: t, cells: cs})
		}
	}
	return w
}

// The good rows map to exactly what their cells say; then every column of every row, and every row as a whole,
// in every other storage class is contained: a typed row error or one valid record that states nothing the
// cells do not.
func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	w := goodWorld()
	recs, err := w.run()
	if err != nil || len(recs) != 1 {
		t.Fatalf("err %v, records %+v", err, recs)
	}
	p, raw := recs[0].Payload, asMap(recs[0].Payload["raw"])
	names := asMap(p["names"])
	for k, want := range map[string]string{"given": "x-First", "family": "x-Last", "middle": "x-Middle", "prefix": "x-Prefix", "suffix": "x-Suffix", "nickname": "x-Nickname", "phonetic": "x-FirstPhonetic x-MiddlePhonetic x-LastPhonetic"} {
		if names[k] != want {
			t.Errorf("names.%s = %v, want %q", k, names[k], want)
		}
	}
	org := asMap(p["organization"])
	if org["name"] != "x-Organization" || org["department"] != "x-Department" || org["title"] != "x-JobTitle" {
		t.Errorf("organization %v", org)
	}
	if p["display_name"] != "x-Prefix x-First x-Middle x-Last x-Suffix" || raw["display_name_source"] != "composed" {
		t.Errorf("display name %v from %v", p["display_name"], raw["display_name_source"])
	}
	if p["source_id"] != "1" || !slices.Equal(asStrings(p["source_ids"]), []string{"x-guid", "x-ExternalIdentifier", "x-ExternalUUID"}) {
		t.Errorf("source ids %v %v", p["source_id"], p["source_ids"])
	}
	one := func(list, key, want string) {
		t.Helper()
		l := asList(p[list])
		if len(l) != 1 || asMap(l[0])[key] != want {
			t.Errorf("%s = %v, want one with %s %q", list, p[list], key, want)
		}
	}
	one("phones", "value", "+15550001")
	one("emails", "value", "a@b.test")
	one("urls", "value", "http://x.test")
	one("addresses", "formatted", "x-Formatted")
	one("addresses", "street", "Main St")
	one("accounts", "name", "Local")
	if ph := asList(p["phones"]); len(ph) == 0 || asMap(ph[0])["label"] != "Mobile" {
		t.Errorf("phone label in %v", p["phones"])
	}
	if raw["Note"] != "x-Note" || raw["birthday"] != "1815-12-10" || raw["Kind"] != int64(1) || !slices.Equal(asStrings(raw["groups"]), []string{"Family"}) {
		t.Errorf("raw %v", raw)
	}
	if recs[0].Time == nil || len(recs[0].Times) != 2 {
		t.Errorf("times %v %v", recs[0].Time, recs[0].Times)
	}
	w.check(t)

	storage := func(k byte) cell { return cell{kind: k, i: 1 << 60, f: math.NaN(), b: []byte{0xff, 0xfe}} }
	for k := range numKinds {
		for col := range w.person {
			c := w.clone()
			c.person[col] = storage(k)
			c.check(t)
		}
		for _, tbl := range joinTables {
			for ri := range w.joins[tbl] {
				for col := range w.joins[tbl][ri].cells {
					c := w.clone()
					c.joins[tbl][ri].cells[col] = storage(k)
					c.check(t)
				}
			}
		}
		c := w.clone()
		for i := range c.person {
			c.person[i] = storage(k)
		}
		for _, tbl := range joinTables {
			for ri := range c.joins[tbl] {
				for col := range c.joins[tbl][ri].cells {
					c.joins[tbl][ri].cells[col] = storage(k)
				}
			}
		}
		c.check(t)
	}
	for _, v := range []int64{math.MinInt64, -1, 0, 1 << 53, math.MaxInt64} {
		c := w.clone()
		for i, cl := range c.person {
			if cl.kind == cInt {
				c.person[i].i = v
			}
		}
		for _, tbl := range joinTables {
			for ri := range c.joins[tbl] {
				for col, cl := range c.joins[tbl][ri].cells {
					if cl.kind == cInt {
						c.joins[tbl][ri].cells[col].i = v
					}
				}
			}
		}
		c.check(t)
	}
	for _, f := range []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1), -3, 1e300} {
		for _, col := range []string{"CreationDate", "ModificationDate", "Birthday"} {
			c := w.clone()
			c.person[personCol(col)] = cell{kind: cFloat, f: f}
			c.check(t)
		}
	}
}

func asStrings(v any) []string {
	var out []string
	for _, e := range asList(v) {
		out = append(out, str(e))
	}
	return out
}

// FuzzMapRows fuzzes the mapping on decoded cells directly: every column of the person and of every joined row
// takes any storage class and value.
func FuzzMapRows(f *testing.F) {
	f.Add(byte(0xff), []byte{cText, cInt, cInt, cInt}, []byte{3, 0, 0, 0, 0, 0, 0, 0}, []byte("+1555\x00hi\x00Street"))
	f.Add(byte(0x85), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(0), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(byte(0xaa), []byte{cText, cText, cText, cInt, cInt, cText, cText, cText, cFloat, cText, cInt, cInt}, []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x7f}, []byte("5\x005\x00x"))
	f.Add(byte(0x9b), []byte{cText}, []byte{5, 0, 0, 0, 0, 0, 0, 0}, []byte("_$!<Home>!$_\x00City\x00Family"))
	f.Fuzz(func(t *testing.T, sel byte, kinds, nums, texts []byte) {
		fuzzWorld(sel, kinds, nums, texts).check(t)
	})
}
