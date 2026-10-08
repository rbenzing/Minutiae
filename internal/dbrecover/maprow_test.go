package dbrecover

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func ptr[T any](v T) *T { return &v }

func testMapper(sizes map[sqlitefile.FileKind]int64) *mapper {
	m := &mapper{byFile: map[sqlitefile.FileKind]inputRef{}}
	ids := map[sqlitefile.FileKind][2]string{
		sqlitefile.FileDB: {"db", "A1"}, sqlitefile.FileWAL: {"wal", "A2"}, sqlitefile.FileJournal: {"journal", "A3"},
	}
	for k, n := range sizes {
		m.byFile[k] = inputRef{Role: ids[k][0], ArtifactID: ids[k][1], SHA256: strings.Repeat("0", 64), Size: n}
	}
	return m
}

func dbOnly() *mapper { return testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1 << 20}) }

func baseRow() sqlitefile.RecoveredRow {
	return sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodFreelist, Origin: sqlitefile.OriginFreelistLeaf,
		Table: "t", TableBasis: sqlitefile.BasisSchema, Rowid: ptr(int64(7)),
		Values:   []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 5, Serial: 1}, textV([]byte("hello"), sqlitefile.EncUTF8)},
		Loc:      sqlitefile.Loc{File: sqlitefile.FileDB, Page: 3, Cell: 0, Offset: 2100, Length: 50, PageOffset: 2048},
		Relation: sqlitefile.RelUnknown,
	}
}

var allMethods = []string{
	sqlitefile.MethodWALPrior, sqlitefile.MethodJournalBefore, sqlitefile.MethodWALStale, sqlitefile.MethodFreelist,
	sqlitefile.MethodWALUncommitted, sqlitefile.MethodJournalRolledBack, sqlitefile.MethodJournalPersist, sqlitefile.MethodPageSlack,
}

func mustRecord(t testing.TB, m *mapper, r sqlitefile.RecoveredRow) records.Record {
	t.Helper()
	rec, err := m.record(r)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestMapEveryMethodAndBasisIsRecoveredNeverLive(t *testing.T) {
	for _, method := range allMethods {
		for _, basis := range []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit, sqlitefile.BasisGuess, sqlitefile.BasisNone} {
			for _, cp := range []*int{nil, ptr(30)} {
				r := baseRow()
				r.Method, r.TableBasis = method, basis
				if basis == sqlitefile.BasisNone {
					r.Table = ""
				}
				m := dbOnly()
				m.cap = cp
				rec := mustRecord(t, m, r)
				if err := assertRecovered(rec); err != nil {
					t.Fatalf("%s/%s: %v", method, basis, err)
				}
				if rec.Confidence == nil {
					t.Fatalf("%s/%s: no confidence", method, basis)
				}
				base := sqlitefile.Confidence(r)
				if rec.Recovery != method || !rec.Deleted || rec.Type != TypeName || rec.ArtifactID != "A1" {
					t.Errorf("%s/%s: %+v", method, basis, rec)
				}
				if *rec.Confidence > base || cp != nil && *rec.Confidence > *cp || *rec.Confidence != sqlitefile.CapConfidence(base, cp) {
					t.Errorf("%s/%s: confidence %d, base %d, cap %v", method, basis, *rec.Confidence, base, cp)
				}
				if rec.SourcePath != "" || rec.Time != nil {
					t.Errorf("%s/%s: source path or time set", method, basis)
				}
				if err := validateDBRow(rec.Payload); err != nil {
					t.Errorf("%s/%s: %v", method, basis, err)
				}
			}
		}
	}
}

func TestAssertRecoveredRefuses(t *testing.T) {
	good := records.Record{Recovery: "sqlite-freelist", Deleted: true, Confidence: ptr(40)}
	if err := assertRecovered(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*records.Record){
		"no recovery":   func(r *records.Record) { r.Recovery = "" },
		"not deleted":   func(r *records.Record) { r.Deleted = false },
		"no confidence": func(r *records.Record) { r.Confidence = nil },
	} {
		r := good
		mut(&r)
		if err := assertRecovered(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the mapper itself refuses a row with no method (it would be live)
	r := baseRow()
	r.Method = ""
	if _, err := dbOnly().record(r); err == nil {
		t.Error("a row with no recovery method was mapped")
	}
}

func TestMapSummaryAndBody(t *testing.T) {
	r := baseRow()
	r.Table = "messages"
	r.Values = []sqlitefile.Value{
		{Kind: sqlitefile.KindInt, Int: 5},
		textV([]byte("hello"), sqlitefile.EncUTF8),
		textV([]byte("w\u00f6rld"), sqlitefile.EncUTF8),
		{Kind: sqlitefile.KindBlob, Bytes: []byte{1, 2, 3}, Len: 3},
		{Kind: sqlitefile.KindNull},
		{Kind: sqlitefile.KindFloat, Float: 1.5},
	}
	rec := mustRecord(t, dbOnly(), r)
	if want := "messages #7: 5 | hello | w\u00f6rld | <blob 3> | NULL | 1.5"; rec.Summary != want {
		t.Errorf("summary %q, want %q", rec.Summary, want)
	}
	if rec.Body != "hello\nw\u00f6rld" {
		t.Errorf("body %q", rec.Body)
	}
	fit := baseRow()
	fit.TableBasis, fit.Rowid = sqlitefile.BasisFit, nil
	if got := mustRecord(t, dbOnly(), fit).Summary; got != "? #?: 5 | hello" {
		t.Errorf("fit-only summary %q", got)
	}
	idx := baseRow()
	idx.Table, idx.Index = "", "ix"
	if got := mustRecord(t, dbOnly(), idx).Summary; got != "index ix: 5 | hello" {
		t.Errorf("index summary %q", got)
	}
	// the body is cut at maxBody and says so
	long := baseRow()
	long.Values = []sqlitefile.Value{textV([]byte(strings.Repeat("a", 100)), sqlitefile.EncUTF8), textV([]byte("tail"), sqlitefile.EncUTF8)}
	m := dbOnly()
	m.maxBody = 50
	rec = mustRecord(t, m, long)
	if len(rec.Body) != 50 || rec.Payload["body_clipped"] != true {
		t.Errorf("body %d bytes, body_clipped %v", len(rec.Body), rec.Payload["body_clipped"])
	}
}

func TestHostileTextNeverReachesSummaryOrBody(t *testing.T) {
	r := baseRow()
	r.Table = "t\x00\xff\n"
	r.Values = []sqlitefile.Value{
		textV([]byte("a\x00b"), sqlitefile.EncUTF8),
		textV([]byte{0xff, 0xfe}, sqlitefile.EncUTF8),
		textV([]byte("ok1"), sqlitefile.EncUTF8),
		textV([]byte("line1\nline2\r\x1b[31m"), sqlitefile.EncUTF8),
		textV(u16("ok2\u202e", false), sqlitefile.EncUTF16LE),
		textV([]byte(strings.Repeat("\u4e16", 3000)), sqlitefile.EncUTF8),
	}
	for i := 0; i < 400; i++ {
		r.Values = append(r.Values, textV([]byte("x\xffy"), sqlitefile.EncUTF8))
	}
	rec := mustRecord(t, dbOnly(), r)
	for name, s := range map[string]string{"summary": rec.Summary, "body": rec.Body} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			t.Errorf("%s holds NUL or invalid UTF-8: %q", name, s)
		}
	}
	if len(rec.Summary) > 512 || strings.ContainsAny(rec.Summary, "\n\r") {
		t.Errorf("summary %d bytes, newline %v", len(rec.Summary), strings.ContainsAny(rec.Summary, "\n\r"))
	}
	if strings.Contains(rec.Body, "a\x00b") || !strings.HasPrefix(rec.Body, "ok1\nline1\nline2") || !strings.Contains(rec.Body, "\nok2\u202e\n") {
		t.Errorf("body %q", rec.Body[:min(len(rec.Body), 80)])
	}
	if err := validateDBRow(rec.Payload); err != nil {
		t.Error(err)
	}
}

func TestWideHostileRowDegradesLoudly(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	blob := bytes.Repeat([]byte{0xab}, 1<<20)
	r := baseRow()
	r.Values = nil
	for i := 0; i < 2000; i++ {
		r.Values = append(r.Values, sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: blob, Len: int64(len(blob))})
	}
	m := &mapper{byFile: map[sqlitefile.FileKind]inputRef{sqlitefile.FileDB: {Role: "db", ArtifactID: art.ID, Size: recordstest.ArtifactSize}}}
	rec := mustRecord(t, m, r)
	res := recordstest.Ingest(t, c, records.Parser{Name: "t", Version: "1"}, []string{art.ID}, []records.Record{rec})
	if res.Records != 1 {
		t.Fatalf("stored %d records, want 1", res.Records)
	}
	notes, _ := rec.Payload["notes"].([]any)
	if !slices.Contains(notes, any("payload-budget")) {
		t.Errorf("notes %v lack payload-budget", notes)
	}
	cols := rec.Payload["columns"].([]any)
	var full, omitted int
	for _, c := range cols {
		if c.(map[string]any)["omitted"] == "payload-budget" {
			omitted++
		} else {
			full++
		}
	}
	if len(cols) != 2000 || full == 0 || omitted == 0 || full+omitted != 2000 {
		t.Errorf("%d columns, %d full, %d omitted", len(cols), full, omitted)
	}
	if rec.Payload["values_omitted"] != int64(omitted) {
		t.Errorf("values_omitted = %v, want %d", rec.Payload["values_omitted"], omitted)
	}
	enc, err := json.Marshal(rec.Payload)
	if err != nil || len(enc) >= 16<<20 || int64(len(enc)) > defaultMaxPayload+(1<<20) {
		t.Errorf("canonical payload %d bytes (err %v)", len(enc), err)
	}
}

func TestHostileNamesAreEncodedExactly(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	m := &mapper{byFile: map[sqlitefile.FileKind]inputRef{sqlitefile.FileDB: {Role: "db", ArtifactID: art.ID, Size: recordstest.ArtifactSize}}}
	tbl := baseRow()
	tbl.Table = "a\x00b\xff"
	idx := baseRow()
	idx.Table, idx.Index = "", "i\x00x"
	var recs []records.Record
	for _, r := range []sqlitefile.RecoveredRow{tbl, idx} {
		recs = append(recs, mustRecord(t, m, r))
	}
	if p := recs[0].Payload; p["table_b64"] != "YQBi/w==" || p["table_valid"] != false || p["table"] != nil {
		t.Errorf("table encoding %v", p)
	}
	if p := recs[1].Payload; p["index_b64"] != "aQB4" || p["index_valid"] != false || p["entry_kind"] != "index" {
		t.Errorf("index encoding %v", p)
	}
	if !strings.Contains(recs[0].Locator, "table=a%00b%FF;") {
		t.Errorf("locator %s", recs[0].Locator)
	}
	res := recordstest.Ingest(t, c, records.Parser{Name: "t", Version: "1"}, []string{art.ID}, recs)
	if res.Records != 2 {
		t.Fatalf("stored %d records, want 2", res.Records)
	}
}

func TestRowLevelUnreadFlagsOnFitRow(t *testing.T) {
	r := baseRow()
	r.TableBasis, r.Relation = sqlitefile.BasisFit, sqlitefile.RelUnknown
	r.Notes = []string{"identity-by-fit-only", "record-length-mismatch"}
	clipped := textV([]byte("ab"), sqlitefile.EncUTF8)
	clipped.Len, clipped.Clipped = 10, true
	r.Values = []sqlitefile.Value{
		{Kind: sqlitefile.KindInt, Unread: true},
		clipped,
		{Kind: sqlitefile.KindBlob, Omitted: true, Len: 1 << 30},
	}
	rec := mustRecord(t, dbOnly(), r)
	p := rec.Payload
	if p["values_unread"] != int64(1) || p["values_omitted"] != int64(1) || p["values_clipped"] != int64(1) {
		t.Errorf("counts %v %v %v", p["values_unread"], p["values_omitted"], p["values_clipped"])
	}
	notes, _ := p["notes"].([]any)
	want := []any{"identity-by-fit-only", "record-length-mismatch", "value-unread", "value-omitted", "value-clipped"}
	if !reflect.DeepEqual(notes, want) {
		t.Errorf("notes %v, want %v", notes, want)
	}
	if !strings.HasPrefix(rec.Summary, "? #7:") || !strings.Contains(rec.Summary, "[unread]") || !strings.Contains(rec.Summary, "[len-mismatch]") {
		t.Errorf("summary %q", rec.Summary)
	}
	if err := validateDBRow(p); err != nil {
		t.Error(err)
	}
	// a clean row carries none of it
	clean := mustRecord(t, dbOnly(), baseRow())
	for _, k := range []string{"values_unread", "values_omitted", "values_clipped", "notes"} {
		if _, has := clean.Payload[k]; has {
			t.Errorf("clean row carries %q", k)
		}
	}
	if strings.Contains(clean.Summary, "[") {
		t.Errorf("clean summary %q", clean.Summary)
	}
}

func goldenJSON(t *testing.T, rec records.Record) string {
	t.Helper()
	b, err := json.Marshal(rec.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPayloadGolden(t *testing.T) {
	both := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1 << 20, sqlitefile.FileWAL: 1 << 20})
	both.pairing = "by-name"
	wal := sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodWALPrior, Origin: sqlitefile.OriginWALSuperseded, Table: "t", TableBasis: sqlitefile.BasisSchema,
		Rowid:    ptr(int64(7)),
		Values:   []sqlitefile.Value{{Kind: sqlitefile.KindInt, Int: 5, Serial: 1}, textV([]byte("hi"), sqlitefile.EncUTF8)},
		Loc:      sqlitefile.Loc{File: sqlitefile.FileWAL, Page: 3, Cell: 2, Offset: 500, Length: 40, PageOffset: 56, Frame: 1},
		WAL:      &sqlitefile.WALProv{Frame: 1, Salt1: 1, Salt2: 2, State: sqlitefile.FrameCommitted, Committed: true, Linked: true},
		Relation: sqlitefile.RelSupersededVersion, Notes: []string{"x"},
	}
	freelist := sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodFreelist, Origin: sqlitefile.OriginFreelistLeaf, Table: "t", TableBasis: sqlitefile.BasisFit,
		Values:   []sqlitefile.Value{{Kind: sqlitefile.KindNull}},
		Loc:      sqlitefile.Loc{File: sqlitefile.FileDB, Page: 6, Cell: 0, Offset: 5200, Length: 10, PageOffset: 5120},
		Relation: sqlitefile.RelUnknown, Notes: []string{"identity-by-fit-only"},
	}
	capped := dbOnly()
	capped.cap = ptr(30)
	jm := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1 << 20, sqlitefile.FileJournal: 1 << 20})
	journal := sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodJournalBefore, Origin: sqlitefile.OriginJournalBefore, Table: "t", TableBasis: sqlitefile.BasisSchema,
		Rowid: ptr(int64(9)), Values: []sqlitefile.Value{},
		Loc: sqlitefile.Loc{
			File: sqlitefile.FileJournal, Page: 4, Cell: 1, Offset: 1000, Length: 20, PageOffset: 516, Record: 2,
			OverflowTotal: 2, OverflowMixed: true, Overflow: []sqlitefile.PagePart{{Page: 9, At: sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: 8192}}},
		},
		Journal:  &sqlitefile.JournalProv{Record: 2, ChecksumOK: true, Nonce: 7},
		Relation: sqlitefile.RelSupersededVersion,
	}
	slack := sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodPageSlack, Origin: sqlitefile.OriginOrphan, TableBasis: sqlitefile.BasisNone, Values: []sqlitefile.Value{},
		Loc:      sqlitefile.Loc{File: sqlitefile.FileDB, Page: 8, Cell: -1, Offset: 4216, Length: 33, PageOffset: 4096},
		Relation: sqlitefile.RelUnknown,
	}
	for name, c := range map[string]struct {
		m    *mapper
		r    sqlitefile.RecoveredRow
		want string
	}{
		"wal prior": {both, wal, `{"cell":2,"columns":[{"i":0,"serial":1,"type":"integer","value":5},{"enc":"UTF-8","i":1,"len":2,"serial":17,"type":"text","value":"hi"}],"entry_kind":"table","file":"wal","method_confidence":75,"notes":["x"],"origin":"wal-superseded","page":3,"relation":"superseded-version","rowid":7,"sibling_artifacts":["A1"],"sibling_pairing":"by-name","table":"t","table_basis":"schema","wal":{"committed":true,"frame":1,"generation":0,"linked":true,"salt1":1,"salt2":2,"state":"committed"}}`},
		"freelist":  {capped, freelist, `{"cell":0,"columns":[{"i":0,"type":"null"}],"confidence_cap":30,"entry_kind":"table","file":"db","method_confidence":60,"notes":["identity-by-fit-only"],"origin":"freelist-leaf","page":6,"relation":"unknown","rowid":null,"table":"t","table_basis":"fit"}`},
		"journal":   {jm, journal, `{"cell":1,"columns":[],"entry_kind":"table","file":"journal","journal":{"applied":false,"checksum_ok":true,"hot":false,"nonce":7,"record":2,"segment":0},"method_confidence":70,"origin":"journal-before","overflow":{"mixed":true,"pages":[{"file":"db","offset":8192,"page":9}],"total":2},"page":4,"relation":"superseded-version","rowid":9,"sibling_artifacts":["A1"],"table":"t","table_basis":"schema"}`},
		"slack":     {dbOnly(), slack, `{"carved":{"span_kind":"orphan","span_length":33,"span_offset":120},"columns":[],"entry_kind":"table","file":"db","method_confidence":25,"origin":"orphan","page":8,"relation":"unknown","rowid":null,"table_basis":"none"}`},
	} {
		if got := goldenJSON(t, mustRecord(t, c.m, c.r)); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}
	// siblings that were found but not applied are named on every row (D9)
	un := dbOnly()
	un.byFile[sqlitefile.FileWAL] = inputRef{Role: "wal", ArtifactID: "A2", Unused: "wal header invalid"}
	got := goldenJSON(t, mustRecord(t, un, freelist))
	if !strings.Contains(got, `"sibling_artifacts":["A2"]`) || !strings.Contains(got, `"sibling_unused":[{"id":"A2","reason":"wal header invalid","role":"wal"}]`) {
		t.Errorf("unused sibling: %s", got)
	}
	flags := dbOnly()
	flags.mixed, flags.incomplete = true, true
	got = goldenJSON(t, mustRecord(t, flags, freelist))
	if !strings.Contains(got, `"mixed_inputs":true`) || !strings.Contains(got, `"input_incomplete":true`) {
		t.Errorf("mixed/incomplete: %s", got)
	}
}

func TestRangeOutsideArtifactKeepsRowWithoutRange(t *testing.T) {
	const size = 1000
	for name, c := range map[string]struct {
		off, length int64
		ok          bool
	}{
		"inside":        {100, 50, true},
		"ends at end":   {950, 50, true},
		"one past":      {951, 50, false},
		"starts past":   {1000, 1, false},
		"negative":      {-1, 5, false},
		"zero length":   {10, 0, false},
		"overflow":      {1<<63 - 10, 100, false},
		"huge length":   {5, 1 << 62, false},
		"negative size": {5, -3, false},
	} {
		r := baseRow()
		r.Loc.Offset, r.Loc.Length = c.off, c.length
		m := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: size})
		rec := mustRecord(t, m, r)
		notes, _ := rec.Payload["notes"].([]any)
		has := slices.Contains(notes, any("range-outside-artifact"))
		if c.ok {
			if rec.Range == nil || *rec.Range != (records.Range{Offset: c.off, Length: c.length}) || has || m.outsideRange != 0 {
				t.Errorf("%s: range %v, note %v, counted %d", name, rec.Range, has, m.outsideRange)
			}
		} else if rec.Range != nil || !has || m.outsideRange != 1 || rec.Recovery == "" {
			t.Errorf("%s: range %v, note %v, counted %d", name, rec.Range, has, m.outsideRange)
		}
	}
}

func TestMapperRefusesAFileWithoutAnArtifact(t *testing.T) {
	r := baseRow()
	r.Loc.File = sqlitefile.FileWAL
	if _, err := dbOnly().record(r); err == nil || !errors.Is(err, errNoArtifactForFile) {
		t.Errorf("err = %v", err)
	}
}

func TestRecoveredRowRangeReproducesCell(t *testing.T) {
	f := newBuilderDB(t)
	seen := map[sqlitefile.FileKind]int{}
	for _, set := range []struct {
		name    string
		journal []byte
	}{{"hot", f.journal}, {"persist", f.persist}} {
		m := testMapper(map[sqlitefile.FileKind]int64{
			sqlitefile.FileDB: int64(len(f.db)), sqlitefile.FileWAL: int64(len(f.wal)), sqlitefile.FileJournal: int64(len(set.journal)),
		})
		data := map[sqlitefile.FileKind][]byte{sqlitefile.FileDB: f.db, sqlitefile.FileWAL: f.wal, sqlitefile.FileJournal: set.journal}
		for _, r := range f.historyWith(t, set.journal) {
			if r.Loc.Cell < 0 {
				continue
			}
			seen[r.Loc.File]++
			rec := mustRecord(t, m, r)
			file := data[r.Loc.File]
			if r.Loc.File == sqlitefile.FileWAL && (r.Loc.PageOffset < 56 || (r.Loc.PageOffset-56)%(24+testPageSize) != 0) {
				t.Errorf("%s: WAL page offset %d is not frame offset + 24", set.name, r.Loc.PageOffset)
			}
			page := file[r.Loc.PageOffset : r.Loc.PageOffset+testPageSize]
			h, err := sqlitefile.ParsePageHeader(page, r.Loc.Page)
			if err != nil {
				t.Fatal(err)
			}
			cell, err := sqlitefile.ParseCell(page, testPageSize, h, int(r.Loc.Offset-r.Loc.PageOffset))
			if err != nil {
				t.Fatal(err)
			}
			dec, err := sqlitefile.DecodeRecord(cell.LocalBytes, sqlitefile.EncUTF8, sqlitefile.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if len(dec.Values) != len(r.Values) || !cell.HasRowid || r.Rowid == nil || cell.Rowid != *r.Rowid {
				t.Fatalf("%s %+v: cell rowid %d, %d values vs %d", set.name, r.Loc, cell.Rowid, len(dec.Values), len(r.Values))
			}
			for i, v := range dec.Values {
				if v.Kind != r.Values[i].Kind || v.Int != r.Values[i].Int || !bytes.Equal(v.Bytes, r.Values[i].Bytes) {
					t.Errorf("%s: value %d differs from the cell", set.name, i)
				}
			}
			if rec.Range == nil || rec.Range.Length != int64(cell.Length) {
				t.Fatalf("%s %+v: range %v, cell length %d", set.name, r.Loc, rec.Range, cell.Length)
			}
			got := file[rec.Range.Offset : rec.Range.Offset+rec.Range.Length]
			if !bytes.Equal(got, page[cell.Offset:cell.Offset+cell.Length]) {
				t.Errorf("%s %+v: range bytes are not the cell", set.name, r.Loc)
			}
		}
	}
	for _, k := range []sqlitefile.FileKind{sqlitefile.FileDB, sqlitefile.FileWAL, sqlitefile.FileJournal} {
		if seen[k] == 0 {
			t.Errorf("the scenario produced no row from file %d", k)
		}
	}
}

// mutate returns a copy of v changed in a way a mapper must notice.
func mutate(v reflect.Value) reflect.Value {
	n := reflect.New(v.Type()).Elem()
	n.Set(v)
	switch v.Kind() {
	case reflect.String:
		n.SetString(v.String() + "z")
	case reflect.Bool:
		n.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n.SetUint(v.Uint() + 1)
	case reflect.Pointer:
		if v.IsNil() {
			n.Set(reflect.New(v.Type().Elem()))
		} else {
			e := mutate(v.Elem())
			p := reflect.New(v.Type().Elem())
			p.Elem().Set(e)
			n.Set(p)
		}
	case reflect.Struct:
		n.Field(0).Set(mutate(v.Field(0)))
	case reflect.Slice:
		n.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
	}
	return n
}

// TestMapperCopiesEveryRecoveredRowField changes each field of a RecoveredRow
// (and of its Loc) in turn and requires the record to change; a field the
// mapper deliberately drops must be listed here with its reason.
func TestMapperCopiesEveryRecoveredRowField(t *testing.T) {
	dropped := map[string]string{}
	// the page start is not a payload key; the cell range and page number place the cell
	droppedLoc := map[string]string{"PageOffset": "derivable from the page number and the page size; only the span of a carved row records it"}
	m := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1 << 20, sqlitefile.FileWAL: 1 << 20, sqlitefile.FileJournal: 1 << 20})
	base := baseRow()
	base.WAL = &sqlitefile.WALProv{Frame: 1}
	base.Journal = &sqlitefile.JournalProv{Record: 1}
	base.Loc.OverflowTotal = 1
	base.Loc.Overflow = []sqlitefile.PagePart{{Page: 9, At: sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: 8192}}}
	baseRec := mustRecord(t, m, base)
	rt := reflect.TypeOf(base)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if dropped[f.Name] != "" || f.Name == "Loc" {
			continue
		}
		r := base
		reflect.ValueOf(&r).Elem().Field(i).Set(mutate(reflect.ValueOf(base).Field(i)))
		if f.Name == "Method" {
			r.Method = sqlitefile.MethodWALStale
		}
		got, err := m.record(r)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if reflect.DeepEqual(got, baseRec) {
			t.Errorf("RecoveredRow.%s is not copied into the record", f.Name)
		}
	}
	// Loc, per file kind: Frame only matters for a WAL row, Record for a journal row
	for _, file := range []sqlitefile.FileKind{sqlitefile.FileDB, sqlitefile.FileWAL, sqlitefile.FileJournal} {
		b := base
		b.Loc.File = file
		bRec := mustRecord(t, m, b)
		lt := reflect.TypeOf(b.Loc)
		for i := 0; i < lt.NumField(); i++ {
			name := lt.Field(i).Name
			if droppedLoc[name] != "" || name == "File" || name == "Frame" && file != sqlitefile.FileWAL || name == "Record" && file != sqlitefile.FileJournal {
				continue
			}
			r := b
			reflect.ValueOf(&r.Loc).Elem().Field(i).Set(mutate(reflect.ValueOf(b.Loc).Field(i)))
			got, err := m.record(r)
			if err != nil {
				t.Fatalf("Loc.%s: %v", name, err)
			}
			if reflect.DeepEqual(got, bRec) {
				t.Errorf("file %d: Loc.%s is not copied into the record", file, name)
			}
		}
	}
}

// TestMapperRecordsAreAcceptedForFileAndDerivedArtifacts ingests mapped rows of
// a captured database and of a copy derived from it.
func TestMapperRecordsAreAcceptedForFileAndDerivedArtifacts(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	parent := addFile(t, c, "/data/app.db", f.db)
	child := addDerivedFile(t, c, parent, "copy.db", f.db)
	for _, art := range []string{parent.ID, child.ID} {
		m := &mapper{byFile: map[sqlitefile.FileKind]inputRef{sqlitefile.FileDB: {Role: "db", ArtifactID: art, Size: int64(len(f.db))}}}
		var recs []records.Record
		for _, r := range f.historyWith(t, f.persist) {
			if r.Loc.File == sqlitefile.FileDB {
				recs = append(recs, mustRecord(t, m, r))
			}
		}
		if len(recs) == 0 {
			t.Fatal("no database rows")
		}
		if res := recordstest.Ingest(t, c, records.Parser{Name: "t" + art, Version: "1"}, []string{art}, recs); res.Records != int64(len(recs)) {
			t.Errorf("artifact %s: stored %d of %d records", art, res.Records, len(recs))
		}
	}
}
