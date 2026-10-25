package recrun_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// The committed fixtures live with the library; the structs below are a copy of the part of its
// oracle these tests read, so the runner is compared with an oracle that shares no code with it.
const fixtureDir = "../../../sqlitefile/testdata"

type fxVal struct {
	T string `json:"t"`
	V string `json:"v,omitempty"`
}

type fxLoc struct {
	File   string `json:"file"`
	Page   uint32 `json:"page"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

type fxHistory struct {
	Table      string   `json:"table"`
	Rowid      *int64   `json:"rowid"`
	Values     []fxVal  `json:"values"`
	Method     string   `json:"method"`
	Relation   string   `json:"relation"`
	Origin     string   `json:"origin"`
	Basis      string   `json:"basis"`
	Confidence int      `json:"confidence"`
	Notes      []string `json:"notes"`
	Cell       fxLoc    `json:"cell"`
}

type fxFrame struct {
	Slot   uint32 `json:"slot"`
	Salt1  uint32 `json:"salt1"`
	Salt2  uint32 `json:"salt2"`
	State  string `json:"state"`
	Offset int64  `json:"offset"`
}

type fxJournalRec struct {
	Index   int   `json:"index"`
	Segment int   `json:"segment"`
	Offset  int64 `json:"offset"`
	Applied bool  `json:"applied"`
}

type fxExpect struct {
	Generator struct {
		FileSHA256 map[string]string `json:"file_sha256"`
	} `json:"generator"`
	Header struct {
		PageSize int `json:"page_size"`
	} `json:"header"`
	Live map[string]struct {
		Rows [][]fxVal `json:"rows"`
	} `json:"live"`
	WAL *struct {
		Frames []fxFrame `json:"frames"`
	} `json:"wal"`
	Journal *struct {
		Hot     bool           `json:"hot"`
		Records []fxJournalRec `json:"records"`
	} `json:"journal"`
	History []fxHistory `json:"history"`
}

type fixture struct {
	name             string
	db, wal, journal []byte
	exp              fxExpect
}

func gunzipIfPresent(t testing.TB, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// loadFixture reads a committed fixture and its oracle and checks every file against the oracle's hash.
func loadFixture(t testing.TB, name string) fixture {
	t.Helper()
	f := fixture{name: name}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f.exp); err != nil {
		t.Fatal(err)
	}
	files := map[string]*[]byte{".db": &f.db, ".db-wal": &f.wal, ".db-journal": &f.journal}
	n := 0
	for suffix, dst := range files {
		if b := gunzipIfPresent(t, filepath.Join(fixtureDir, name+suffix+".gz")); b != nil {
			*dst = b
			n++
			sum := sha256.Sum256(b)
			if want := f.exp.Generator.FileSHA256[name+suffix]; want != hex.EncodeToString(sum[:]) {
				t.Fatalf("fixture %s%s is stale against its oracle", name, suffix)
			}
		}
	}
	if f.db == nil || n != len(f.exp.Generator.FileSHA256) {
		t.Fatalf("fixture %s: %d files, the oracle lists %d", name, n, len(f.exp.Generator.FileSHA256))
	}
	return f
}

// input is the Input of the bundle of bytes: role db, and wal and journal when given.
func input(db, wal, journal []byte) *parse.Input {
	art := func(id string, b []byte) parse.Artifact {
		return parse.Artifact{ID: id, Size: int64(len(b)), R: bytes.NewReader(b), Platform: parse.PlatformAndroid, Logical: "android:/data/x/t.db"}
	}
	in := &parse.Input{
		Primary: art("A-DB", db), Artifacts: map[string]parse.Artifact{"db": art("A-DB", db)},
		Limits: parse.DefaultLimits(), Budget: parse.NewBudget(1 << 30).View(),
	}
	if wal != nil {
		in.Artifacts["wal"] = art("A-WAL", wal)
	}
	if journal != nil {
		in.Artifacts["journal"] = art("A-JOURNAL", journal)
	}
	return in
}

func (f fixture) input() *parse.Input { return input(f.db, f.wal, f.journal) }

type emitted struct {
	Rec   records.Record
	Facts parse.RecoveredFacts
}

type unmappedCall struct {
	Facts  parse.RecoveredFacts
	Reason string
}

type warn struct{ Locator, Reason string }

// errLiveEmit is what the recording emitter answers to a plain Emit.
var errLiveEmit = errors.New("live Emit refused")

// recEmitter records everything the runner hands to the host. A plain Emit is a test failure: the
// runner must never emit a live record.
type recEmitter struct {
	t        testing.TB
	recs     []emitted
	unmapped []unmappedCall
	warns    []warn
	notes    map[string]string
	liveEmit int
}

func newEmitter(t testing.TB) *recEmitter { return &recEmitter{t: t, notes: map[string]string{}} }

func (e *recEmitter) Emit(context.Context, records.Record) error {
	e.liveEmit++
	e.t.Errorf("the runner called Emit: a recovered row must never become a live record")
	return errLiveEmit
}

func (e *recEmitter) EmitRecovered(_ context.Context, r records.Record, f parse.RecoveredFacts) error {
	e.recs = append(e.recs, emitted{r, f})
	return nil
}

func (e *recEmitter) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	e.unmapped = append(e.unmapped, unmappedCall{f, reason})
	return nil
}

func (e *recEmitter) Warn(_ context.Context, locator, reason string) error {
	e.warns = append(e.warns, warn{locator, reason})
	return nil
}

func (e *recEmitter) Note(k, v string)      { e.notes[k] = v }
func (e *recEmitter) Progress(int64, int64) {}

var _ parse.RecoveryEmitter = (*recEmitter)(nil)

// cell renders column c of a row for the fake mapping: NULL, an integer ("i5"), text ("tabc") or "?".
func cell(row parse.Row, c int) string {
	if row.IsNull(c) {
		return "NULL"
	}
	if n, ok := row.Int(c); ok {
		return "i" + strconv.FormatInt(n, 10)
	}
	if b, ok := row.Text(c); ok {
		return "t" + string(b)
	}
	return "?"
}

// fakeMapping maps every one of cols columns to Payload["c<i>"] and the rowid to Payload["rowid"].
func fakeMapping(table string, cols int) parse.TableMapping {
	return parse.TableMapping{Table: table, Map: func(_ parse.MapContext, row parse.Row) ([]records.Record, error) {
		p := map[string]any{}
		if id, ok := row.Rowid(); ok {
			p["rowid"] = id
		}
		for c := range cols {
			p["c"+strconv.Itoa(c)] = cell(row, c)
		}
		rng, _ := row.Range()
		return []records.Record{{Type: "test", Payload: p, Range: &rng}}, nil
	}}
}

func claim(tables ...string) []parse.TableClaim {
	var cs []parse.TableClaim
	for _, t := range tables {
		cs = append(cs, parse.TableClaim{Role: "db", Table: t})
	}
	return cs
}

func foldClaimed(names ...string) func(string) bool {
	return func(t string) bool {
		for _, n := range names {
			if strings.EqualFold(n, t) {
				return true
			}
		}
		return false
	}
}

// refRows walks the history with the library alone and returns the locators of the rows ClaimMatch
// accepts, in delivery order, FactsOf of each, and the verdict counts of the others.
func refRows(t testing.TB, db, wal, journal []byte, tables ...string) (order []string, facts map[string]parse.RecoveredFacts, skipped map[string]int64) {
	t.Helper()
	lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		if _, err := lib.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatal(err)
		}
	}
	if journal != nil {
		if _, err := lib.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
			t.Fatal(err)
		}
	}
	sch, err := lib.Live().Schema(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := lib.History()
	defer h.Release()
	facts, skipped = map[string]parse.RecoveredFacts{}, map[string]int64{}
	_, err = h.Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
		if v := sch.ClaimMatch(rr, foldClaimed(tables...)); v != sqlitefile.ClaimYes {
			skipped[string(v)]++
			return true
		}
		loc := sqlitefile.RecoveredLocator(rr)
		order = append(order, loc)
		facts[loc] = recrun.FactsOf(rr)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return order, facts, skipped
}

// oracleText renders an oracle value like cell does.
func oracleText(v fxVal) string {
	switch v.T {
	case "null":
		return "NULL"
	case "int":
		return "i" + v.V
	case "text":
		return "t" + v.V
	}
	return "?"
}

// byRange indexes the emitted records by the file role and byte range of their cell.
func byRange(t testing.TB, got []emitted) map[string]emitted {
	t.Helper()
	m := map[string]emitted{}
	for _, e := range got {
		k := string(e.Facts.Role) + ":" + strconv.FormatInt(e.Facts.Range.Offset, 10) + "+" + strconv.FormatInt(e.Facts.Range.Length, 10)
		if _, dup := m[k]; dup {
			t.Fatalf("two records for the cell %s", k)
		}
		m[k] = e
	}
	return m
}

func rangeKey(h fxHistory) string {
	return h.Cell.File + ":" + strconv.FormatInt(h.Cell.Offset, 10) + "+" + strconv.FormatInt(h.Cell.Length, 10)
}

// tc0 is the counts of the first claimed table, or the zero value when the report holds none.
func tc0(rep recrun.Report) recrun.TableCounts {
	if len(rep.Tables) == 0 {
		return recrun.TableCounts{}
	}
	return rep.Tables[0]
}

// firstReason is the reason of the first Unmapped call, "" when there was none.
func firstReason(e *recEmitter) string {
	if len(e.unmapped) == 0 {
		return ""
	}
	return e.unmapped[0].Reason
}
