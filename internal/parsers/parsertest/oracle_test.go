package parsertest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func sp(s string) *string { return &s }

func utc(s string) *records.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return &records.Time{T: t.UTC()}
}

func oracleBundle() Bundle {
	db := parse.Artifact{ID: "ID-DB", Size: 10000, Source: parse.SourceInfo{RemotePath: "/data/x.db"}}
	wal := parse.Artifact{ID: "ID-WAL", Size: 5000, Source: parse.SourceInfo{RemotePath: "/data/x.db-wal"}}
	img := parse.Artifact{ID: "ID-IMG", SHA256: strings.Repeat("ab", 32), Size: 77}
	return Bundle{
		Primary: db,
		ByRole:  map[string]parse.Artifact{"db": db, "wal": wal},
		ByPath:  map[string]parse.Artifact{"/p/img.jpg": img},
	}
}

func goodRecord() records.Record {
	return records.Record{
		Type: "message", SourcePath: "/data/x.db", ArtifactID: "ID-DB", Locator: "sqlite:table=sms;rowid=1", Range: &records.Range{Offset: 100, Length: 40},
		Time: utc("2023-05-01T10:00:00Z"), Times: []records.NamedTime{{Kind: "received", Time: *utc("2023-05-01T10:00:00Z")}},
		Summary: "sms in +1555: hi", Body: "hi",
		Payload: map[string]any{"channel": "sms", "participants": []any{}, "raw": map[string]any{"date": int64(1682935200000)}},
	}
}

func goodExpect() Expect {
	var exp Expect
	exp.Records = []ExpRecord{{
		Table: "sms", Rowid: 1, File: "db", Type: "message", SourcePath: "/data/x.db", Time: sp("2023-05-01T10:00:00Z"), Times: map[string]string{"received": "2023-05-01T10:00:00Z"},
		Summary: sp("sms in +1555: hi"), Body: sp("hi"),
		Payload: map[string]any{"channel": "sms", "participants": []any{}, "raw": map[string]any{"date": json.Number("1682935200000")}},
	}}
	exp.Notes = map[string]string{"schema.sms": "columns:3"}
	return exp
}

func TestAssertMatchesOracleAcceptsAMatchingRun(t *testing.T) {
	rec := &recorder{TB: t}
	AssertMatchesOracle(rec, Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}, oracleBundle(), goodExpect())
	if rec.Failed() {
		t.Errorf("a matching run was refused: %v", rec.errs)
	}
}

func TestAssertMatchesOracleRefusesEveryKindOfDifference(t *testing.T) {
	mutate := map[string]func(*Result, *Expect){
		"missing record":    func(r *Result, _ *Expect) { r.Records = nil },
		"extra record":      func(r *Result, _ *Expect) { r.Records = append(r.Records, goodRecord()) },
		"wrong rowid":       func(r *Result, _ *Expect) { r.Records[0].Locator = "sqlite:table=sms;rowid=2" },
		"wrong time":        func(r *Result, _ *Expect) { r.Records[0].Time = utc("2023-05-01T10:00:01Z") },
		"time where none":   func(_ *Result, e *Expect) { e.Records[0].Time = nil },
		"wrong times":       func(r *Result, _ *Expect) { r.Records[0].Times = nil },
		"deleted":           func(r *Result, _ *Expect) { r.Records[0].Deleted = true },
		"recovery set":      func(r *Result, _ *Expect) { r.Records[0].Deleted, r.Records[0].Recovery = true, "sqlite-freelist" },
		"recovery only":     func(r *Result, _ *Expect) { r.Records[0].Recovery = "sqlite-freelist" },
		"wrong type":        func(r *Result, _ *Expect) { r.Records[0].Type = "call" },
		"wrong source path": func(r *Result, _ *Expect) { r.Records[0].SourcePath = "/data/other.db" },
		"no source path":    func(r *Result, _ *Expect) { r.Records[0].SourcePath = "" },
		"extra times kind": func(r *Result, _ *Expect) {
			r.Records[0].Times = append(r.Records[0].Times, records.NamedTime{Kind: "sent", Time: *utc("2023-05-01T10:00:00Z")})
		},
		"duplicate times kind": func(r *Result, e *Expect) {
			e.Records[0].Times["sent"] = "2023-05-01T10:00:00Z"
			r.Records[0].Times = []records.NamedTime{r.Records[0].Times[0], r.Records[0].Times[0]}
		},
		"range overflow": func(r *Result, _ *Expect) {
			r.Records[0].Range = &records.Range{Offset: 100, Length: math.MaxInt64}
		},
		"confidence set":     func(r *Result, _ *Expect) { c := 80; r.Records[0].Confidence = &c },
		"wrong summary":      func(r *Result, _ *Expect) { r.Records[0].Summary = "other" },
		"wrong body":         func(r *Result, _ *Expect) { r.Records[0].Body = "ho" },
		"payload value":      func(r *Result, _ *Expect) { r.Records[0].Payload["channel"] = "mms" },
		"payload extra key":  func(r *Result, _ *Expect) { r.Records[0].Payload["zzz"] = "x" },
		"payload raw value":  func(r *Result, _ *Expect) { r.Records[0].Payload["raw"] = map[string]any{"date": int64(1)} },
		"range outside":      func(r *Result, _ *Expect) { r.Records[0].Range = &records.Range{Offset: 9990, Length: 40} },
		"no range":           func(r *Result, _ *Expect) { r.Records[0].Range = nil },
		"wal locator on db":  func(r *Result, _ *Expect) { r.Records[0].Locator += ";wal_frame=3" },
		"wrong artifact":     func(r *Result, _ *Expect) { r.Records[0].ArtifactID = "ID-WAL" },
		"unexpected warning": func(r *Result, _ *Expect) { r.Warnings = []Warning{{Locator: "x", Reason: "y"}} },
		"parser error":       func(r *Result, _ *Expect) { r.Err = os.ErrInvalid },
		"missing note":       func(r *Result, _ *Expect) { r.Notes = nil },
		"wrong note":         func(r *Result, _ *Expect) { r.Notes["schema.sms"] = "columns:4" },
	}
	for name, mut := range mutate {
		res := Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}
		exp := goodExpect()
		mut(&res, &exp)
		rec := &recorder{TB: t}
		AssertMatchesOracle(rec, res, oracleBundle(), exp)
		if !rec.Failed() {
			t.Errorf("%s: the difference was not noticed", name)
		}
	}
}

func TestAssertMatchesOracleAnyFileAndTableFilter(t *testing.T) {
	exp := goodExpect()
	exp.Records[0].File, exp.Records[0].SourcePath = "any", ""
	res := Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}
	res.Records[0].Locator += ";wal_frame=3"
	res.Records[0].ArtifactID, res.Records[0].SourcePath = "ID-WAL", "/data/x.db-wal"
	rec := &recorder{TB: t}
	AssertMatchesOracle(rec, res, oracleBundle(), exp)
	if rec.Failed() {
		t.Errorf("file any with a wal cell was refused: %v", rec.errs)
	}
	// with a table filter the other tables' records are neither required nor allowed to count
	exp2 := goodExpect()
	exp2.Records = append(exp2.Records, ExpRecord{Table: "pdu", Rowid: 1, File: "db", Payload: map[string]any{}})
	rec = &recorder{TB: t}
	AssertMatchesOracle(rec, Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}, oracleBundle(), exp2, "sms")
	if rec.Failed() {
		t.Errorf("filter sms: %v", rec.errs)
	}
}

// Ruling T15: under a table filter a reader warning is compared on both sides, never dropped from one.
func TestAssertMatchesOracleComparesReaderWarningsUnderATableFilter(t *testing.T) {
	reader := Warning{Locator: "sqlite:file=journal;page=0;offset=0", Reason: "journal-hot: a hot journal was applied in memory"}
	base := func() Result {
		return Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}
	}
	exp := goodExpect()
	exp.Warnings = []ExpWarning{{Table: "", Contains: "journal-hot"}}
	res := base()
	res.Warnings = []Warning{reader}
	rec := &recorder{TB: t}
	AssertMatchesOracle(rec, res, oracleBundle(), exp, "sms")
	if rec.Failed() {
		t.Errorf("an expected reader warning was refused under a filter: %v", rec.errs)
	}
	rec = &recorder{TB: t}
	AssertMatchesOracle(rec, base(), oracleBundle(), exp, "sms")
	if !rec.Failed() {
		t.Error("a missing reader warning was accepted under a filter")
	}
	exp.Warnings = nil
	rec = &recorder{TB: t}
	AssertMatchesOracle(rec, res, oracleBundle(), exp, "sms")
	if !rec.Failed() {
		t.Error("an unexpected reader warning was accepted under a filter")
	}
}

func TestAssertMatchesOracleResolvesPlaceholdersAndWarnings(t *testing.T) {
	exp := goodExpect()
	exp.Records[0].Payload["attachments"] = []any{map[string]any{"artifact_id": "@id:/p/img.jpg", "artifact_sha256": "@sha256:/p/img.jpg", "size": "@size:/p/img.jpg"}}
	exp.Warnings = []ExpWarning{{Table: "sms", Rowid: 9, Contains: "no rowid"}}
	r := goodRecord()
	r.Payload["attachments"] = []any{map[string]any{"artifact_id": "ID-IMG", "artifact_sha256": strings.Repeat("ab", 32), "size": int64(77)}}
	res := Result{
		Records: []records.Record{r}, Notes: map[string]string{"schema.sms": "columns:3"},
		Warnings: []Warning{{Locator: "sqlite:table=sms;rowid=9", Reason: "the sms row has no rowid locator"}},
	}
	rec := &recorder{TB: t}
	AssertMatchesOracle(rec, res, oracleBundle(), exp)
	if rec.Failed() {
		t.Errorf("placeholders or warnings refused: %v", rec.errs)
	}
	res.Warnings[0].Reason = "something else"
	rec = &recorder{TB: t}
	AssertMatchesOracle(rec, res, oracleBundle(), exp)
	if !rec.Failed() {
		t.Error("a warning with another reason was accepted")
	}
}

func writeFixture(t *testing.T, dir, name string, db []byte, hash string) {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(db); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".db.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	exp := `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"` + hash + `"}},"family":"f","variant":"v","records":[],"warnings":[],"notes":{}}`
	if err := os.WriteFile(filepath.Join(dir, name+".expect.json"), []byte(exp), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFixtureChecksTheHashBeforeAnything(t *testing.T) {
	db := []byte("SQLite format 3\x00 fake body")
	sum := sha256.Sum256(db)
	dir := t.TempDir()
	writeFixture(t, dir, "good", db, hex.EncodeToString(sum[:]))
	if f := LoadFixture(t, dir, "good"); !bytes.Equal(f.Files["db"], db) || f.Expect.Variant != "v" {
		t.Errorf("fixture = %+v", f)
	}
	writeFixture(t, dir, "stale", db, strings.Repeat("0", 64))
	rec := &recorder{TB: t}
	LoadFixture(rec, dir, "stale")
	if !rec.Failed() || !strings.Contains(strings.Join(rec.errs, " "), "regenerate") {
		t.Errorf("a stale oracle was accepted: %v", rec.errs)
	}
}

func writeRawFixture(t *testing.T, dir, name, expect string, db []byte) {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(db); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".db.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".expect.json"), []byte(expect), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A misspelled key in an expect.json (or a role key the loader does not know) must be an
// error: silently dropped it would disable the check it was meant to state (ruling T40/T41).
func TestLoadFixtureRefusesUnknownKeysAndRoles(t *testing.T) {
	db := []byte("SQLite format 3 fake body for the key checks")
	sum := hex.EncodeToString(func() []byte { s := sha256.Sum256(db); return s[:] }())
	cases := map[string]string{
		"unknown record key": `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"` + sum + `"}},"family":"f","variant":"v","records":[{"table":"sms","rowid":1,"file":"db","delete":true}],"warnings":[],"notes":{}}`,
		"unknown top key":    `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"` + sum + `"}},"family":"f","variant":"v","recordz":[],"warnings":[],"notes":{}}`,
		"unknown role":       `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"` + sum + `","journals":"` + sum + `"}},"family":"f","variant":"v","records":[],"warnings":[],"notes":{}}`,
	}
	for name, exp := range cases {
		dir := t.TempDir()
		writeRawFixture(t, dir, "x", exp, db)
		rec := &recorder{TB: t}
		LoadFixture(rec, dir, "x")
		if !rec.Failed() {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	writeRawFixture(t, dir, "x", `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"`+sum+`"}},"family":"f","variant":"v","records":[{"table":"sms","rowid":1,"file":"db","type":"message","source_path":"/p"}],"warnings":[],"notes":{}}`, db)
	if f := LoadFixture(t, dir, "x"); len(f.Expect.Records) != 1 || f.Expect.Records[0].SourcePath != "/p" {
		t.Errorf("a well-formed expect.json was refused or misread: %+v", f.Expect.Records)
	}
}

// ruling T48: a record's SourcePath is the remote path of the file that holds its ArtifactID's bytes.
func TestSourcePathIsThatOfTheFileHoldingTheCell(t *testing.T) {
	walRecord := func(path string) Result {
		r := goodRecord()
		r.Locator += ";wal_frame=3"
		r.ArtifactID, r.SourcePath = "ID-WAL", path
		return Result{Records: []records.Record{r}, Notes: map[string]string{"schema.sms": "columns:3"}}
	}
	for _, c := range []struct {
		name, file, stated, got string
		ok                      bool
	}{
		{"wal row states and reports the -wal path", "wal", "/data/x.db-wal", "/data/x.db-wal", true},
		{"wal row reports the db path", "wal", "/data/x.db-wal", "/data/x.db", false},
		{"wal row whose oracle states the db path", "wal", "/data/x.db", "/data/x.db", false},
		{"any row derives -wal from the locator", "any", "", "/data/x.db-wal", true},
		{"any row reporting the db path for a wal cell", "any", "", "/data/x.db", false},
		{"any row whose oracle states the db path for a wal cell", "any", "/data/x.db", "/data/x.db-wal", false},
	} {
		exp := goodExpect()
		exp.Records[0].File, exp.Records[0].SourcePath = c.file, c.stated
		rec := &recorder{TB: t}
		AssertMatchesOracle(rec, walRecord(c.got), oracleBundle(), exp)
		if rec.Failed() == c.ok {
			t.Errorf("%s: failed = %v, errors %v", c.name, rec.Failed(), rec.errs)
		}
	}
	// an any row in the db: the derived path is the db artifact's
	exp := goodExpect()
	exp.Records[0].File, exp.Records[0].SourcePath = "any", ""
	rec := &recorder{TB: t}
	AssertMatchesOracle(rec, Result{Records: []records.Record{goodRecord()}, Notes: map[string]string{"schema.sms": "columns:3"}}, oracleBundle(), exp)
	if rec.Failed() {
		t.Errorf("any row in the db refused: %v", rec.errs)
	}
}
