package dbrecover

// Oracle tests on the committed real databases of plan 3I (Task 13). The
// oracle is each fixture's expect.json (written by the engine or by a minimal
// walker that shares no code with the library) and the engine itself, never a
// value computed by dbrecover or sqlitefile. Every fixture file is pinned by
// the SHA-256 of its uncompressed bytes. No hardware or Docker is needed.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// pinnedFixtureSHA256 is the SHA-256 of every uncompressed fixture file. A
// regenerated fixture must be re-pinned here on purpose.
var pinnedFixtureSHA256 = map[string]string{
	"addcolumn-short.db":                    "0356cadc4473907602eb40dfa56d10c167f31c69732b9d916a859214f6756c83",
	"autovacuum-incr.db":                    "e4abc59d9657a74ebd845ac8ae207ce997c44c21e3d616e55b94e80ea16a84dc",
	"basic-utf8-4k.db":                      "ec759d590b6516ff594ba8a0e4e1c31497d1a87f2471d37a23a6a007f2823635",
	"builder-hot-journal.db":                "68ab954d3da9e9b9e7041789874c99beeac7613e813da907f1c6d29c6dce72b5",
	"builder-journal-multiseg.db":           "999e9bf715a833df08586ae5da1a1cedd66c705d121b7383fe36dd178d17bd0e",
	"builder-wal-generations.db":            "d24972d2b715721f33b78f67f6e95173a22fde403d538318dd44d2cd2ecd1106",
	"encrypted-like.db":                     "4a0a24f20018e2c2baa36fde67f787f2cd0c00efeb2ca0d70d1354bd190e54c9",
	"freelist-dropped.db":                   "e697cb4b33c788bb363d34bcdaae21646c51573e4fff60536c0310beb0caf4ea",
	"hot-journal.db":                        "1eb63ba75a2c50bb37505775f5fe96f77f8c2a81fdb5f8643a674873aba8648f",
	"overflow-wide.db":                      "ff4caf5a3e510240e0a6791bc9c5aa25ec4951d4179f75137927b4456ba7c652",
	"persist-journal.db":                    "1ee5077cca3d25f5052ca6779b8daa43ac89b3568fd09480528abab4027626d8",
	"secure-delete.db":                      "62b5d75ac1f51484fbca535ddba1deda7bbc449b53655dae8f42f041a0639b99",
	"sqlite3-freelist.db":                   "f86dc194f8ced3f46e099b4c4a025bb8dd5246028b0e4e5abf7c2e287e4f3e78",
	"sqlite3-hot-journal-killed.db":         "951aff6cf1e361b84f7fce5462c40dd991fe2f7b2fa7fb9ecaacc760fcb12240",
	"sqlite3-utf16.db":                      "0f5d3af95897c67f9d79a02281077f89e698ce6fdecaa0ea73e1ab5c286d4de1",
	"sqlite3-wal-killed.db":                 "dda3b2bd0ae46bb134879dbc5dba4fbfbe17b703364cc4fcf74f337257d85e79",
	"utf16be-512.db":                        "4cedc54018ed05d80d98b98efb38eed9a3c1492f6ece125335c442a1e632af2e",
	"utf16le-1k.db":                         "2094caba248de1c5874f0cabc5ced91fb0d2f96677087e8b28daa65f941c34a5",
	"wal-stale-generation.db":               "f80323299e63b88ac1eb029cd14156c61251a069c1bc351783ed06aa153d947a",
	"wal-uncheckpointed.db":                 "110195cfd4c770d0ee7941f837a2b167848abc060d00823f9b8ea91730f4ddf0",
	"without-rowid.db":                      "cb4229389df18fc72bf04a60b43fcfce648d83b9d4c6a978bfcfdb2aa9db6bf0",
	"builder-wal-generations.db-wal":        "5b0092a1f4b64b1dc3936e34d244accfa68eec924c83018b307aa355c9be90a5",
	"sqlite3-wal-killed.db-wal":             "97ac7d2c914f5cfc8e297219bd2d7628f04b8e58d25bb12caf8e49bf59cd90e6",
	"wal-stale-generation.db-wal":           "0af7fcff304e873197236e2262d554d4ed70f53b2e4bbe6126019076ab5660d0",
	"wal-uncheckpointed.db-wal":             "6c28e3623a399df2e04a8ff9983a46f70f8c8bd3bf1750f40f99c3ee3b37ad38",
	"builder-hot-journal.db-journal":        "62dfcda7f4134dbfbd5e87859feb6aa9a5c3ae6af8e655110d90ef8829962d83",
	"builder-journal-multiseg.db-journal":   "2cd8a82163edd9ef0967c991a3b33b7c6ee5ecdb864a7636bd3a16b63e5c24db",
	"hot-journal.db-journal":                "e4231d17a00ca23f25a48c4ebf4c32bd9e01b6379b92f22efd546defc1141869",
	"persist-journal.db-journal":            "4d062952de09d53791f7d39bdf9c56f14db48903d4e9c186f24e98b14ac13002",
	"sqlite3-hot-journal-killed.db-journal": "0faf52f16e2f49aa46e2392a8f825d03f0922f54754a8c663f1ae77267727eb0",
}

// the minimal part of expect.json this package reads
type oVal struct {
	T string `json:"t"`
	V string `json:"v"`
}

type oCell struct {
	File    string `json:"file"`
	Page    uint32 `json:"page"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	CellHex string `json:"cell_hex"`
}

type oHist struct {
	Table      string `json:"table"`
	Rowid      *int64 `json:"rowid"`
	Values     []oVal `json:"values"`
	Method     string `json:"method"`
	Relation   string `json:"relation"`
	Basis      string `json:"basis"`
	Confidence int    `json:"confidence"`
	Cell       oCell  `json:"cell"`
}

type oExpect struct {
	Generator struct {
		FileSHA256 map[string]string `json:"file_sha256"`
	} `json:"generator"`
	NotSQLite bool `json:"not_sqlite"`
	WAL       *struct {
		PageSize uint32 `json:"page_size"`
		Frames   []struct {
			Slot   uint32 `json:"slot"`
			Offset int64  `json:"offset"`
		} `json:"frames"`
	} `json:"wal"`
	Journal *struct {
		Records []struct {
			Index  int   `json:"index"`
			Offset int64 `json:"offset"`
		} `json:"records"`
	} `json:"journal"`
	History     []oHist `json:"history"`
	Unreachable []struct {
		Table  string `json:"table"`
		Marker string `json:"marker"`
	} `json:"unreachable"`
}

type oFixture struct {
	name             string
	exp              oExpect
	db, wal, journal []byte
}

func (f *oFixture) file(name string) []byte {
	switch name {
	case "wal":
		return f.wal
	case "journal":
		return f.journal
	}
	return f.db
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// loadOracleFixture reads a fixture and its oracle and checks every file
// against the pin in this file AND against the hash the oracle describes.
func loadOracleFixture(t *testing.T, name string) *oFixture {
	t.Helper()
	dir := filepath.Join("..", "sqlitefile", "testdata")
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatalf("fixture %s: no oracle: %v", name, err)
	}
	f := &oFixture{name: name}
	if err := json.Unmarshal(raw, &f.exp); err != nil {
		t.Fatalf("fixture %s: oracle: %v", name, err)
	}
	read := func(file string) []byte {
		b, ok := gunzipFile(t, filepath.Join(dir, file+".gz"))
		if !ok {
			return nil
		}
		want, pinned := pinnedFixtureSHA256[file]
		if !pinned {
			t.Fatalf("fixture file %s is not pinned in this test", file)
		}
		if got := shaHex(b); got != want {
			t.Fatalf("fixture file %s has sha256 %s, pinned %s", file, got, want)
		}
		if o := f.exp.Generator.FileSHA256[file]; o != want {
			t.Fatalf("fixture file %s: the oracle describes %s, pinned %s", file, o, want)
		}
		return b
	}
	f.db = read(name + ".db")
	f.wal = read(name + ".db-wal")
	f.journal = read(name + ".db-journal")
	n := 1
	if f.wal != nil {
		n++
	}
	if f.journal != nil {
		n++
	}
	if len(f.exp.Generator.FileSHA256) != n {
		t.Fatalf("fixture %s: the oracle lists %d files, the fixture has %d", name, len(f.exp.Generator.FileSHA256), n)
	}
	return f
}

// oracleRun is one Run over a fixture.
type oracleRun struct {
	c    *evidence.Case
	res  Result
	ids  map[string]string // file role -> artifact id
	f    *oFixture
	recs []records.Full
}

func runOracleFixture(t *testing.T, f *oFixture) *oracleRun {
	t.Helper()
	c := recordstest.NewCase(t)
	ids := map[string]string{}
	db := addFile(t, c, "/data/"+f.name+".db", f.db)
	ids["db"] = db.ID
	if f.wal != nil {
		ids["wal"] = addFile(t, c, "/data/"+f.name+".db-wal", f.wal).ID
	}
	if f.journal != nil {
		ids["journal"] = addFile(t, c, "/data/"+f.name+".db-journal", f.journal).ID
	}
	res, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
	if err != nil {
		t.Fatalf("%s: Run: %v", f.name, err)
	}
	return &oracleRun{c: c, res: res, ids: ids, f: f, recs: storedRecords(t, c)}
}

// pinnedFixtureNames is the committed fixture set; it must be exactly the
// pinned set, so a new fixture cannot slip in unpinned.
func pinnedFixtureNames(t *testing.T) []string {
	t.Helper()
	var pinned []string
	for k := range pinnedFixtureSHA256 {
		if base, ok := strings.CutSuffix(k, ".db"); ok {
			pinned = append(pinned, base)
		}
	}
	sort.Strings(pinned)
	names := fixtureNames(t)
	sort.Strings(names)
	if !slices.Equal(names, pinned) {
		t.Fatalf("the committed fixtures %v are not the pinned ones %v", names, pinned)
	}
	return names
}

// ---- value comparison against the oracle ----

func decodeJSONNumbers(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

// colMatches compares one payload column with one oracle value exactly.
func colMatches(col map[string]any, v oVal) error {
	if _, ok := col["omitted"]; ok {
		return fmt.Errorf("column %v is omitted", col["i"])
	}
	if col["clipped"] == true {
		return fmt.Errorf("column %v is clipped", col["i"])
	}
	typ, _ := col["type"].(string)
	want := map[string]string{"null": "null", "int": "integer", "float": "real", "text": "text", "blob": "blob"}[v.T]
	if typ != want {
		return fmt.Errorf("type %q, oracle %q", typ, want)
	}
	switch v.T {
	case "int":
		if n, _ := col["value"].(json.Number); n.String() != v.V {
			return fmt.Errorf("value %v, oracle %s", col["value"], v.V)
		}
	case "float":
		bits, err := strconv.ParseUint(v.V, 16, 64)
		if err != nil {
			return err
		}
		if got, _ := col["float_bits"].(string); got != fmt.Sprintf("%016x", bits) {
			return fmt.Errorf("float bits %v, oracle %016x (%v)", col["float_bits"], bits, math.Float64frombits(bits))
		}
	case "text":
		if s, ok := col["value"].(string); ok {
			if s != v.V {
				return fmt.Errorf("text %q, oracle %q", s, v.V)
			}
			return nil
		}
		b, _ := base64.StdEncoding.DecodeString(fmt.Sprint(col["value_b64"]))
		if string(b) != v.V {
			return fmt.Errorf("text bytes %q, oracle %q", b, v.V)
		}
	case "blob":
		want, _ := base64.StdEncoding.DecodeString(v.V)
		got, _ := base64.StdEncoding.DecodeString(fmt.Sprint(col["value_b64"]))
		if !bytes.Equal(got, want) {
			return fmt.Errorf("blob %x, oracle %x", got, want)
		}
	}
	return nil
}

func columnsOf(p map[string]any) []map[string]any {
	var out []map[string]any
	cols, _ := p["columns"].([]any)
	for _, c := range cols {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

// payloadName is the table name of a payload ("" when it has none).
func payloadName(p map[string]any) string {
	if s, ok := p["table"].(string); ok {
		return s
	}
	if s, ok := p["table_b64"].(string); ok {
		b, _ := base64.StdEncoding.DecodeString(s)
		return string(b)
	}
	return ""
}

// locatorField returns the value of key=... in a locator.
func locatorField(loc, key string) (string, bool) {
	for _, part := range strings.Split(strings.TrimPrefix(loc, "sqlite:"), ";") {
		if v, ok := strings.CutPrefix(part, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// frameOf derives the frame of a WAL cell from the oracle's own frame list.
func (f *oFixture) frameOf(off int64) (uint32, bool) {
	if f.exp.WAL == nil {
		return 0, false
	}
	for _, fr := range f.exp.WAL.Frames {
		if off >= fr.Offset+24 && off < fr.Offset+24+int64(f.exp.WAL.PageSize) {
			return fr.Slot, true
		}
	}
	return 0, false
}

// journalRecordOf derives the journal record of a cell from the oracle's own
// record list (a record is a 4-byte page number, the page, a 4-byte checksum).
func (f *oFixture) journalRecordOf(off, pageSize int64) (int, bool) {
	if f.exp.Journal == nil {
		return 0, false
	}
	for _, r := range f.exp.Journal.Records {
		if off >= r.Offset+4 && off < r.Offset+4+pageSize {
			return r.Index, true
		}
	}
	if len(f.exp.Journal.Records) > 0 || off < journalSector {
		return 0, false
	}
	// a zeroed header leaves the oracle no record list: the records tile the
	// file after the first sector, each a page number, the page and a checksum
	return int((off - journalSector - 4) / (pageSize + 8)), true
}

// journalSector is where the first record of a zeroed-header journal starts.
const journalSector = 512

// pageSize is read from the database header bytes (offset 16, big endian).
func (f *oFixture) pageSize() int64 {
	n := int64(f.db[16])<<8 | int64(f.db[17])
	if n == 1 {
		return 65536
	}
	return n
}

func ptrOf(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// find returns the one stored record of the oracle entry.
func (r *oracleRun) find(t *testing.T, e oHist) (records.Full, bool) {
	t.Helper()
	var hits []records.Full
	for _, s := range r.recs {
		if s.Method == e.Method && s.ArtifactID == r.ids[e.Cell.File] && s.Range != nil && s.Range.Offset == e.Cell.Offset {
			hits = append(hits, s)
		}
	}
	if len(hits) != 1 {
		t.Errorf("%s: oracle %s rowid %v table %q at %s offset %d: %d stored records, want exactly 1", r.f.name, e.Method, ptrOf(e.Rowid), e.Table, e.Cell.File, e.Cell.Offset, len(hits))
		return records.Full{}, false
	}
	return hits[0], true
}

// checkEntry compares one stored record with its oracle entry.
func (r *oracleRun) checkEntry(t *testing.T, e oHist, s records.Full) {
	t.Helper()
	tag := fmt.Sprintf("%s %s rowid %v at %s offset %d", r.f.name, e.Method, ptrOf(e.Rowid), e.Cell.File, e.Cell.Offset)
	p, err := decodeJSONNumbers(s.Payload)
	if err != nil {
		t.Errorf("%s: payload: %v", tag, err)
		return
	}
	if got := fmt.Sprint(p["relation"]); got != e.Relation {
		t.Errorf("%s: relation %q, oracle %q", tag, got, e.Relation)
	}
	if got := fmt.Sprint(p["table_basis"]); got != e.Basis {
		t.Errorf("%s: table_basis %q, oracle %q", tag, got, e.Basis)
	}
	if got := payloadName(p); got != e.Table {
		t.Errorf("%s: table %q, oracle %q", tag, got, e.Table)
	}
	// the artifacts are live, uncapped files: the confidence is min(oracle, no cap)
	if s.Confidence == nil || *s.Confidence != e.Confidence {
		t.Errorf("%s: confidence %v, oracle %d", tag, s.Confidence, e.Confidence)
	}
	if !s.Deleted || !s.Recovered {
		t.Errorf("%s: deleted %v recovered %v, want both", tag, s.Deleted, s.Recovered)
	}
	if e.Rowid == nil {
		if p["rowid"] != nil {
			t.Errorf("%s: rowid %v, oracle none", tag, p["rowid"])
		}
	} else if n, _ := p["rowid"].(json.Number); n.String() != strconv.FormatInt(*e.Rowid, 10) {
		t.Errorf("%s: rowid %v, oracle %d", tag, p["rowid"], *e.Rowid)
	}
	cols := columnsOf(p)
	if len(cols) != len(e.Values) {
		t.Errorf("%s: %d columns, oracle %d", tag, len(cols), len(e.Values))
	} else {
		for i, v := range e.Values {
			if err := colMatches(cols[i], v); err != nil {
				t.Errorf("%s: column %d: %v", tag, i, err)
			}
		}
	}
	// the range is the cell: same length and the same bytes as the oracle's cell_hex
	want, err := hex.DecodeString(e.Cell.CellHex)
	if err != nil {
		t.Fatalf("%s: cell_hex: %v", tag, err)
	}
	file := r.f.file(e.Cell.File)
	switch {
	case s.Range == nil || s.Range.Length != e.Cell.Length || int64(len(want)) != e.Cell.Length:
		t.Errorf("%s: range %+v, oracle length %d (cell_hex %d bytes)", tag, s.Range, e.Cell.Length, len(want))
	case s.Range.Offset+s.Range.Length > int64(len(file)) || !bytes.Equal(file[s.Range.Offset:s.Range.Offset+s.Range.Length], want):
		t.Errorf("%s: the bytes under the range are not the oracle's cell_hex", tag)
	}
	// the locator
	if v, _ := locatorField(s.Locator, "page"); v != strconv.FormatUint(uint64(e.Cell.Page), 10) {
		t.Errorf("%s: locator %q, oracle page %d", tag, s.Locator, e.Cell.Page)
	}
	if v, _ := locatorField(s.Locator, "file"); v != e.Cell.File {
		t.Errorf("%s: locator %q, oracle file %s", tag, s.Locator, e.Cell.File)
	}
	switch e.Cell.File {
	case "wal":
		slot, ok := r.f.frameOf(e.Cell.Offset)
		if v, _ := locatorField(s.Locator, "wal_frame"); !ok || v != strconv.FormatUint(uint64(slot), 10) {
			t.Errorf("%s: locator %q, oracle frame slot %d (found %v)", tag, s.Locator, slot, ok)
		}
	case "journal":
		idx, ok := r.f.journalRecordOf(e.Cell.Offset, r.f.pageSize())
		if v, _ := locatorField(s.Locator, "journal_rec"); !ok || v != strconv.Itoa(idx) {
			t.Errorf("%s: locator %q, oracle journal record %d (found %v)", tag, s.Locator, idx, ok)
		}
	}
}

// libraryDelivered is the set of cells (method, file, page, offset) the library and the slack carver
// delivers for a fixture; every stored record must be one of them.
func libraryDelivered(t *testing.T, f *oFixture) map[string]int {
	t.Helper()
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.wal != nil {
		if _, err := db.AttachWAL(bytesAt{f.wal}, int64(len(f.wal))); err != nil {
			t.Fatal(err)
		}
	}
	if f.journal != nil {
		if _, err := db.AttachJournal(bytesAt{f.journal}, int64(len(f.journal))); err != nil {
			t.Fatal(err)
		}
	}
	h := db.History()
	defer h.Release()
	out := map[string]int{}
	if _, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		out[fmt.Sprintf("%s|%s|%d|%d", r.Method, fileName(r.Loc.File), r.Loc.Page, r.Loc.Offset)]++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	// the slack carver is the second pass of the same history
	if _, err := carveSpans(context.Background(), db, h, func(r sqlitefile.RecoveredRow) bool {
		out[fmt.Sprintf("%s|%s|%d|%d", r.Method, fileName(r.Loc.File), r.Loc.Page, r.Loc.Offset)]++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSQLiteRecoverMatchesOracle(t *testing.T) {
	withHistory := 0
	for _, name := range pinnedFixtureNames(t) {
		f := loadOracleFixture(t, name)
		if f.exp.NotSQLite || len(f.exp.History) == 0 {
			continue
		}
		withHistory++
		t.Run(name, func(t *testing.T) {
			r := runOracleFixture(t, f)
			for role, id := range r.ids {
				if role == "db" {
					continue
				}
				found := false
				for _, in := range r.res.Inputs {
					found = found || in.ArtifactID == id
				}
				if !found {
					t.Errorf("sibling %s (%s) is not an input of the run", role, id)
				}
			}
			for _, e := range f.exp.History {
				if s, ok := r.find(t, e); ok {
					r.checkEntry(t, e, s)
				}
			}
			// precision: every stored record is a row the library delivered
			lib := libraryDelivered(t, f)
			for _, s := range r.recs {
				p, _ := decodeJSONNumbers(s.Payload)
				key := fmt.Sprintf("%s|%v|%v|%d", s.Method, p["file"], p["page"], s.Range.Offset)
				if lib[key] == 0 {
					t.Errorf("stored record %s is not a delivered row", key)
				}
			}
			if int64(len(r.recs)) != r.res.Counts.Written || r.res.Ingest.Records != r.res.Counts.Written {
				t.Errorf("stored %d, Written %d, ingest %d", len(r.recs), r.res.Counts.Written, r.res.Ingest.Records)
			}
		})
	}
	if withHistory < 8 {
		t.Errorf("only %d fixtures carry a history oracle", withHistory)
	}
}

func TestEveryFixtureVerifiesClean(t *testing.T) {
	for _, name := range pinnedFixtureNames(t) {
		f := loadOracleFixture(t, name)
		if f.exp.NotSQLite {
			continue
		}
		t.Run(name, func(t *testing.T) {
			r := runOracleFixture(t, f)
			rep, err := r.c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			evidencetest.RequireClean(t, rep)
		})
	}
}

func TestEncryptedLikeFixtureIsRefused(t *testing.T) {
	f := loadOracleFixture(t, "encrypted-like")
	if !f.exp.NotSQLite {
		t.Fatal("the oracle does not call the fixture not-sqlite")
	}
	c := recordstest.NewCase(t)
	a := addFile(t, c, "/data/enc.db", f.db)
	_, err := Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
	if refusalReason(err) != ReasonNotSQLite {
		t.Fatalf("Run = %v, want a %s refusal", err, ReasonNotSQLite)
	}
	noIngest(t, c)
}

// checkSecureDeleteFixture (a half of TestRecoverSecureDeleteInventsNothing): the engine's secure
// delete leaves nothing of the deleted rows; no record may carry one. The
// engine half is in oracle_engine_test.go.
func checkSecureDeleteFixture(t *testing.T) {
	f := loadOracleFixture(t, "secure-delete")
	if len(f.exp.Unreachable) == 0 {
		t.Fatal("the oracle lists no unreachable row")
	}
	r := runOracleFixture(t, f)
	if len(f.exp.History) != 0 {
		t.Fatal("this test assumes the oracle recovers nothing from the fixture")
	}
	if len(r.recs) != 0 {
		t.Errorf("%d records stored where the oracle recovers nothing", len(r.recs))
	}
	for _, s := range r.recs {
		all := s.Summary + s.Body + string(s.Payload)
		for _, u := range f.exp.Unreachable {
			if u.Marker != "" && strings.Contains(all, u.Marker) {
				t.Errorf("record %d carries the marker %q of a securely deleted row", s.ID, u.Marker)
			}
		}
	}
}

// testJournalSector derives the sector size of a zeroed-header journal from the
// journal bytes alone, with the test's own checksum consensus (never the
// library's): for each power-of-two candidate, records cut from that start vote
// for the nonce (stored checksum minus the sampled bytes); the candidate with
// the most agreeing records wins.
func testJournalSector(t *testing.T, j []byte, pageSize int) int {
	t.Helper()
	rsz := pageSize + 8
	best, bestVotes := 0, 0
	for sec := 512; sec <= 65536 && sec+rsz <= len(j); sec *= 2 {
		votes := map[uint32]int{}
		for off := sec; off+rsz <= len(j); off += rsz {
			if binary.BigEndian.Uint32(j[off:]) == 0 {
				continue
			}
			page := j[off+4 : off+4+pageSize]
			var sum uint32
			for i := pageSize - 200; i > 0; i -= 200 {
				sum += uint32(page[i])
			}
			votes[binary.BigEndian.Uint32(j[off+4+pageSize:])-sum]++
		}
		for _, c := range votes {
			if c > bestVotes {
				best, bestVotes = sec, c
			}
		}
	}
	if bestVotes < 2 {
		t.Fatalf("no checksum consensus in the journal (best %d votes)", bestVotes)
	}
	return best
}

// TestZeroedHeaderJournalSectorIsDerivedIndependently pins the sector size of
// the persist-mode journal (J84): the test derives it from the bytes, and the
// library must report the same value by nonce consensus.
func TestZeroedHeaderJournalSectorIsDerivedIndependently(t *testing.T) {
	f := loadOracleFixture(t, "persist-journal")
	if !bytes.Equal(f.journal[:8], make([]byte, 8)) {
		t.Fatal("the fixture journal header is not zeroed")
	}
	got := testJournalSector(t, f.journal, int(f.pageSize()))
	if got != journalSector {
		t.Fatalf("independently derived sector %d, want %d", got, journalSector)
	}
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := db.AttachJournal(bytesAt{f.journal}, int64(len(f.journal)))
	if err != nil {
		t.Fatal(err)
	}
	if int(info.SectorSize) != got || !info.NonceDerived {
		t.Errorf("library sector %d nonce-derived %v, want %d and true", info.SectorSize, info.NonceDerived, got)
	}
}
