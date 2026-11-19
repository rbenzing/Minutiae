package ioswacontacts

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
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

// fuzzCtx knows the columns of ZWAADDRESSBOOKCONTACT by its spec order. lacking names a column the variant lacks (-1).
type fuzzCtx struct {
	in      *parse.Input
	mp      parse.TableMapping
	lacking string
	notes   map[string]string
}

func (c fuzzCtx) Input() *parse.Input { return c.in }
func (c fuzzCtx) Note(k, v string) {
	if c.notes != nil {
		c.notes[k] = v
	}
}

func (c fuzzCtx) Column(table, col string) int {
	if table != c.mp.Table || col == c.lacking {
		return -1
	}
	for i, n := range specNames(c.mp.Columns) {
		if n == col {
			return i
		}
	}
	return -1
}

func (fuzzCtx) Get(string, int64) (parse.Row, bool, error) { return nil, false, nil }

func (fuzzCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, nil }

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

// The modes a row is mapped in: the typed pass (no Parse context) and Parse's.
const (
	modeRecovered = iota
	modeParse
	numModes
)

// fuzzEnv is the environment Parse would hand the mapping in a mode.
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
	return &env{Place: p, Label: l}
}

func cellOf(mp parse.TableMapping, cells []cell, name string) cell {
	for i, s := range mp.Columns {
		if s.Name == name {
			return cells[i]
		}
	}
	return cell{}
}

// textOf is the text of a cell: its bytes when it is a TEXT cell, ok false otherwise.
func textOf(c cell) (string, bool) { return string(c.b), c.kind == cText }

// rawKeys are the keys raw may hold: nothing else is ever invented.
var rawKeys = map[string]bool{
	"app": true, "app_basis": true, "app_state": true, "row_flags": true, "unknown_columns": true, "text_flags": true,
	"phone_label_stored": true, "ZPHONENUMBERLABEL": true, "whatsapp_id_state": true, "whatsapp_id_text": true, "lid_map": true,
	"lid_state": true, "Z_ENT": true, "ZHIGHLIGHTEDNAME": true, "ZABOUTTIMESTAMP": true, "ZLID": true, "ZWHATSAPPID": true,
}

// checkMapped holds the mapping to its contract on any row: no panic, and either a typed row error (a counted warning) or at
// most one record that validates, carries its place, and invents nothing the cells do not say.
func checkMapped(t testing.TB, place, mode byte, cells []cell, lacking string) {
	t.Helper()
	e := fuzzEnv(place, mode)
	mp := contactMappingWith(e)
	in := fuzzInput(place)
	notes := map[string]string{}
	recs, err := mp.Map(fuzzCtx{in: in, mp: mp, lacking: lacking, notes: notes}, fuzzRow{table: mp.Table, id: 1, cells: cells})
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("%s: untyped error %v", mp.Table, err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%s: %d records and no error", mp.Table, len(recs))
	}
	r := recs[0]
	if r.ArtifactID != "a1" || r.SourcePath != "UDID1/aa/aabbcc" || r.Range == nil || r.Locator != "sqlite:table="+mp.Table+";rowid=1" {
		t.Fatalf("%s: record lost its place: %+v", mp.Table, r)
	}
	if r.Type != contact.Type {
		t.Fatalf("%s: a record of type %q", mp.Table, r.Type)
	}
	if verr := contact.Validate(r.Payload); verr != nil {
		t.Fatalf("%s: payload refused: %v", mp.Table, verr)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	// raw stays inside a bound that follows the text cells only: a blob is shown by its length or by at most 256 bytes of
	// base64, however large it is (the JSON escaping of a text byte is at most 6)
	if rawJSON, err := json.Marshal(r.Payload["raw"]); err != nil || len(rawJSON) > rawBound(cells) {
		t.Errorf("%s: raw is %d bytes (%v), over the bound %d", mp.Table, len(rawJSON), err, rawBound(cells))
	}
	for k := range raw {
		if !rawKeys[k] {
			t.Errorf("%s: raw key %q is not in the vocabulary", mp.Table, k)
		}
	}
	if r.Recovery != "" || r.Confidence != nil || r.Time != nil || r.TimeEnd != nil || len(r.Times) != 0 {
		t.Errorf("%s: the mapping stamped a recovery or a time: %+v", mp.Table, r)
	}
	// a name only from a non-empty TEXT ZFULLNAME, a phone only from a non-empty TEXT ZPHONENUMBER (never from the JID)
	full, fullOK := textOf(cellOf(mp, cells, "ZFULLNAME"))
	num, numOK := textOf(cellOf(mp, cells, "ZPHONENUMBER"))
	if _, has := r.Payload["display_name"]; has != (fullOK && full != "" && lacking != "ZFULLNAME") {
		t.Errorf("%s: display_name %v from ZFULLNAME %q (text %v)", mp.Table, r.Payload["display_name"], full, fullOK)
	}
	if _, has := r.Payload["phones"]; has != (numOK && num != "" && lacking != "ZPHONENUMBER") {
		t.Errorf("%s: phones %v from ZPHONENUMBER %q (text %v)", mp.Table, r.Payload["phones"], num, numOK)
	}
	// an ims entry only for a JID that is TEXT, was not changed by cleaning and is a user or LID address; the LID map only for
	// a user JID with the row's own clean ZLID text
	jid, jidOK := textOf(cellOf(mp, cells, colWhatsAppID))
	ims, hasIMS := r.Payload["ims"].([]any)
	if hasIMS {
		m, _ := ims[0].(map[string]any)
		j, perr := wajid.Parse(jid)
		if !jidOK || m["value"] != jid || m["kind"] != "whatsapp" || perr != nil || (j.Class != wajid.ClassUser && j.Class != wajid.ClassLID) || len(jid) > maxJID {
			t.Errorf("%s: ims %v from JID %q (text %v)", mp.Table, ims, jid, jidOK)
		}
	}
	if lm, has := raw["lid_map"].([]any); has {
		m, _ := lm[0].(map[string]any)
		lid, lidOK := textOf(cellOf(mp, cells, "ZLID"))
		j, perr := wajid.Parse(jid)
		if !hasIMS || !lidOK || perr != nil || j.Class != wajid.ClassUser || m["lid"] != lid || m["jid"] != jid || m["source"] != "ZWAADDRESSBOOKCONTACT.ZLID" || len(lm) != 1 {
			t.Errorf("%s: lid_map %v from JID %q and ZLID %q", mp.Table, lm, jid, lid)
		}
		if lacking == "ZLID" {
			t.Errorf("%s: lid_map without a ZLID column", mp.Table)
		}
	}
	if raw["lid_state"] != nil && raw["lid_map"] != nil {
		t.Errorf("%s: both lid_map and lid_state: %v", mp.Table, raw)
	}
	// no state for an ims entry; a state for every JID that is not one
	if hasIMS == (raw["whatsapp_id_state"] != nil) {
		t.Errorf("%s: ims %v with whatsapp_id_state %v", mp.Table, ims, raw["whatsapp_id_state"])
	}
	if mode%numModes == modeParse && lacking == "ZLID" && notes["ZLID"] != "absent" {
		t.Errorf("%s: a store without ZLID states no note: %v", mp.Table, notes)
	}
	if mode%numModes == modeRecovered && len(notes) != 0 {
		t.Errorf("%s: the typed pass wrote notes: %v", mp.Table, notes)
	}
	// the label of a row: Parse's, or in the typed pass a domain names its app and an app group states none (PF5)
	if mode%numModes == modeRecovered {
		if place%3 == 2 && (raw["app"] != "unknown" || raw["app_state"] != "not-evaluated") {
			t.Errorf("%s: a row of an app group states app %v state %v", mp.Table, raw["app"], raw["app_state"])
		}
		if place%3 != 2 && raw["app"] != "whatsapp" && raw["app"] != "whatsapp_business" {
			t.Errorf("%s: a row of a backup domain states app %v", mp.Table, raw["app"])
		}
	}
}

// goodCells is a plain contact of a user JID with a name and a phone.
func goodCells(mp parse.TableMapping) []cell {
	cs := make([]cell, len(mp.Columns))
	for i, s := range mp.Columns {
		switch s.Name {
		case colWhatsAppID:
			cs[i] = cell{kind: cText, b: []byte("15550000001@s.whatsapp.net")}
		case "ZLID":
			cs[i] = cell{kind: cText, b: []byte("100000000000001@lid")}
		case "ZABOUTTIMESTAMP":
			cs[i] = cell{kind: cFloat, f: 600000001}
		case "Z_PK", "Z_ENT":
			cs[i] = cell{kind: cInt, i: 1}
		default:
			cs[i] = cell{kind: cText, b: []byte("x")}
		}
	}
	return cs
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

// lackingOf picks the column a variant lacks from a fuzz byte: none for 0, else one of the columns after the key.
func lackingOf(mp parse.TableMapping, b byte) string {
	if b == 0 {
		return ""
	}
	cols := mp.Columns[2:] // the key and the JID column are required: Run fails a table that lacks them
	return cols[int(b)%len(cols)].Name
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	mp := contactMappingWith(nil)
	for place := range byte(len(fuzzPlaces)) {
		for mode := range byte(numModes) {
			recs, err := contactMappingWith(fuzzEnv(place, mode)).Map(fuzzCtx{in: fuzzInput(place), mp: mp}, fuzzRow{table: mp.Table, id: 1, cells: goodCells(mp)})
			if err != nil || len(recs) != 1 {
				t.Fatalf("place %d mode %d: a good row did not map: %v %v", place, mode, err, recs)
			}
			for lack := byte(0); lack < byte(len(mp.Columns)); lack++ {
				checkMapped(t, place, mode, goodCells(mp), lackingOf(mp, lack))
			}
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range goodCells(mp) {
					cs := goodCells(mp)
					cs[col] = cell{kind: k, i: -1, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
					checkMapped(t, place, mode, cs, "")
				}
				cs := goodCells(mp)
				for i := range cs {
					cs[i] = cell{kind: k, f: math.NaN(), b: []byte{0xff, 0xfe}}
				}
				checkMapped(t, place, mode, cs, "")
			}
			for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 1 << 53, math.MaxInt64} {
				cs := goodCells(mp)
				for i := range cs {
					if cs[i].kind == cInt {
						cs[i].i = v
					}
				}
				checkMapped(t, place, mode, cs, "")
			}
			for _, jid := range []string{"1@g.us", "x", "@s.whatsapp.net", "1@broadcast", "status@broadcast", "1@lid", "1:2@s.whatsapp.net", "1:99999@s.whatsapp.net", "1@newsletter", "1@other"} {
				cs := goodCells(mp)
				cs[1] = cell{kind: cText, b: []byte(jid)}
				checkMapped(t, place, mode, cs, "")
			}
		}
	}
}

// FuzzMapIOSWhatsAppContactsRows fuzzes the ZWAADDRESSBOOKCONTACT mapping on decoded column values directly: every column of every
// row takes any storage class and value, at each place and in each mode (the typed pass and Parse's), with any one optional
// column lacking.
func FuzzMapIOSWhatsAppContactsRows(f *testing.F) {
	jid := []byte("n\x0015550000001@s.whatsapp.net\x00+15550000001")
	f.Add(byte(0), byte(1), byte(0), []byte{cInt, cText, cInt, cText, cText, cText, cText, cText, cText, cFloat, cText, cText}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, jid)
	f.Add(byte(2), byte(0), byte(5), []byte{cInt, cText, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(byte(1), byte(1), byte(11), []byte{cInt, cText, cInt, cText, cNull, cNull, cText}, []byte{}, []byte("Ann\x00100000000000001@lid\x00_$!<Home>!$_"))
	f.Add(byte(5), byte(1), byte(0), []byte{cInt, cText, cInt, cText}, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}, []byte("a\x001@g.us"))
	f.Add(byte(4), byte(0), byte(2), []byte{cInt}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("x"))
	f.Fuzz(func(t *testing.T, place, mode, lack byte, kinds, nums, texts []byte) {
		mp := contactMappingWith(nil)
		checkMapped(t, place, mode, cellsFrom(len(mp.Columns), kinds, nums, texts), lackingOf(mp, lack))
	})
}

// rawBound is the size raw may reach for a row: a fixed 64 KiB for the keys, labels and 256-byte blobs, plus six bytes per byte of
// text in any cell (the JSON escape of a control byte). A BLOB cell adds nothing: it is shown by its length or a 256-byte base64.
func rawBound(cells []cell) int {
	n := 64 << 10
	for _, c := range cells {
		if c.kind == cText {
			n += 6 * len(c.b)
		}
	}
	return n
}

// A 1 MiB BLOB and a 1 MiB TEXT in every column of the row: raw stays inside its bound (an uncapped base64 of the blobs would be
// about 100 MB) and the text is cut.
func TestMapRowsBigCellsDoNotGrowRaw(t *testing.T) {
	mp := contactMappingWith(nil)
	big := make([]byte, 1<<20)
	for place := range byte(len(fuzzPlaces)) {
		for mode := range byte(numModes) {
			for _, k := range []byte{cBlob, cText} {
				for col := range mp.Columns { // one column at a time: a row with a wrong-class required cell is a typed error and shows nothing
					cs := goodCells(mp)
					cs[col] = cell{kind: k, b: big}
					checkMapped(t, place, mode, cs, "")
				}
			}
		}
	}
}
