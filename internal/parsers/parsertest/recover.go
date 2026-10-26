package parsertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// RecoverExpect is a <variant>.recover.expect.json: what the typed recovery pass must make of a fixture
// (plan 4G, ruling 4G-R14). The generator computes it from the COMMITTED fixture files with an
// independent page walker and the scenario's own record of what it wrote and deleted; it shares no code
// with the Go reader. Records are the versions that must come back as typed records; Written is every
// version the scenario stored in a claimed table that is not live at the end, the only values a typed
// record may carry (nothing invented); Unmapped are the rows that must be reported, not typed.
type RecoverExpect struct {
	Generator Generator           `json:"generator"` // Files: the SHA-256 of the fixture files this oracle was computed from
	Variant   string              `json:"variant"`
	Claimed   []string            `json:"claimed"`
	Columns   map[string][]string `json:"columns"` // table -> its columns in record order; Written.Values follow it
	Records   []RecoverExpectRecord
	Digests   []RecoverDigest
	Unmapped  []RecoverExpectUnmapped
	Written   []RecoverWritten
}

// RecoverExpectRecord is one version that must come back as a typed record: the facts the host stamps
// (method, relation, basis, origin, confidence, notes, the cell) and the typed fields the mapping builds.
type RecoverExpectRecord struct {
	Table      string    `json:"table"` // the claimed table the row is attributed to
	Rowid      *int64    `json:"rowid"`
	Method     string    `json:"method"`
	Relation   string    `json:"relation"`
	Basis      string    `json:"basis"`
	Origin     string    `json:"origin"`
	Confidence int       `json:"confidence"`
	Notes      []string  `json:"notes"` // the row's own notes; the runner adds joins-from-live-view
	File       string    `json:"file"`  // db, wal or journal: the file the cell lies in
	Page       uint32    `json:"page"`
	Offset     int64     `json:"offset"`
	Length     int64     `json:"length"`
	Record     ExpRecord `json:"record"` // Deleted, SourcePath, artifact and range are the host's, not compared
}

// RecoverDigest is the SHA-256 of the canonical typed record (type, times, body, payload) the oracle expects at
// a cell. Every want has one, whether or not it has a full entry in Records, so the mapping's output is checked
// for every recovered version, not only for the listed ones.
type RecoverDigest struct {
	Table  string `json:"table"`
	Rowid  int64  `json:"rowid"`
	File   string `json:"file"`
	Page   uint32 `json:"page"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Digest string `json:"digest"`
}

// recordDigest is the digest of an emitted record, built like the generator's record_digest: canonical JSON
// with sorted keys, no spaces and no HTML escaping.
func recordDigest(r records.Record) string {
	times := map[string]string{}
	for _, nt := range r.Times {
		times[nt.Kind] = nt.Time.T.UTC().Format(time.RFC3339Nano)
	}
	ts := func(t *records.Time) any {
		if t == nil {
			return nil
		}
		return t.T.UTC().Format(time.RFC3339Nano)
	}
	obj := map[string]any{
		"type": r.Type, "time": ts(r.Time), "time_end": ts(r.TimeEnd), "times": times, "body": r.Body,
		"payload": normalize(r.Payload),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return "unencodable: " + err.Error()
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte{10}))
	return hex.EncodeToString(sum[:])
}

// RecoverExpectUnmapped is a row that must be reported unmapped with a reason containing Contains.
type RecoverExpectUnmapped struct {
	Table    string `json:"table"`
	Rowid    *int64 `json:"rowid"`
	Contains string `json:"contains"`
}

// RecoverWritten is one version the scenario stored: Values follow RecoverExpect.Columns[Table]; a value
// is null, a number, a string or {"blob_b64": ...}.
type RecoverWritten struct {
	Table  string `json:"table"`
	Rowid  int64  `json:"rowid"`
	Values []any  `json:"values"`
}

// LoadRecoverExpect reads <dir>/<variant>.recover.expect.json and requires it to have been computed from the
// fixture files that are committed now (the SHA-256 of every file equals the one in <variant>.expect.json).
func LoadRecoverExpect(t testing.TB, dir, variant string) RecoverExpect {
	t.Helper()
	fx := LoadFixture(t, dir, variant) // checks the files against their own oracle first
	raw, err := os.ReadFile(filepath.Join(dir, variant+".recover.expect.json"))
	if err != nil {
		t.Fatal(err)
		return RecoverExpect{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields() // a misspelt key must not silently disable the check it states
	var x RecoverExpect
	if err := dec.Decode(&x); err != nil {
		t.Fatalf("%s recover oracle: %v", variant, err)
		return RecoverExpect{}
	}
	if x.Variant != variant {
		t.Fatalf("%s recover oracle is for variant %q", variant, x.Variant)
	}
	if !reflect.DeepEqual(x.Generator.Files, fx.Expect.Generator.Files) {
		t.Fatalf("%s recover oracle was computed from other fixture files (%v) than the committed ones (%v): regenerate it with --recover-oracle",
			variant, x.Generator.Files, fx.Expect.Generator.Files)
	}
	return x
}

// recoveredCapture is the part of a row the kit reads before the mapping runs: its rowid and the values of
// the columns the oracle lists, so a typed record can be traced back to a version the scenario wrote.
type recoveredCapture struct {
	rowid    int64
	hasRowid bool
	vals     map[string]any // column -> nil | int64 | string | blobValue | unreadable
}

type blobValue string

type unreadable struct{}

func capture(m parse.MapContext, row parse.Row, table string, cols []string) *recoveredCapture {
	c := &recoveredCapture{vals: map[string]any{}}
	c.rowid, c.hasRowid = row.Rowid()
	for _, name := range cols {
		i := m.Column(table, name)
		if i < 0 {
			continue
		}
		switch {
		case row.IsNull(i):
			c.vals[name] = nil
		default:
			if n, ok := row.Int(i); ok {
				c.vals[name] = n
			} else if b, ok := row.Text(i); ok {
				c.vals[name] = string(b)
			} else if b, ok := row.Blob(i); ok {
				c.vals[name] = blobValue(b)
			} else {
				c.vals[name] = unreadable{}
			}
		}
	}
	return c
}

type recoverEmitted struct {
	rec   records.Record
	facts parse.RecoveredFacts
	row   *recoveredCapture
}

type recoverUnmapped struct {
	facts  parse.RecoveredFacts
	reason string
	row    *recoveredCapture
}

// recoverRecorder is the strict recovery emitter of the kit: it records what the runner hands over,
// fails the test on a live Emit, and validates every record as the writer would (records.NewValidator),
// after stamping the facts the host would stamp.
type recoverRecorder struct {
	t        testing.TB
	validate func(records.Record) error
	cur      *recoveredCapture
	recs     []recoverEmitted
	unmapped []recoverUnmapped
	warns    []Warning
	notes    map[string]string
}

func (e *recoverRecorder) Emit(context.Context, records.Record) error {
	e.t.Errorf("parsertest: the typed recovery pass called Emit: a recovered row must never become a live record")
	return errors.New("parsertest: live Emit refused in a recovery run")
}

func (e *recoverRecorder) EmitRecovered(_ context.Context, r records.Record, f parse.RecoveredFacts) error {
	row := e.cur
	e.recs = append(e.recs, recoverEmitted{rec: copyRecord(r), facts: f, row: row})
	if err := f.Validate(); err != nil {
		e.t.Errorf("parsertest: facts of %s are invalid: %v", f.Locator, err)
	}
	if r.Recovery != "" || r.Confidence != nil || r.Deleted {
		e.t.Errorf("parsertest: the mapping set Recovery/Confidence/Deleted on the record of %s: only the host stamps them", f.Locator)
	}
	s := copyRecord(r) // the stamps the host's recovery emitter applies, for the validator only
	s.Deleted, s.Recovery, s.Locator = true, f.Method, f.Locator
	conf := f.Confidence
	s.Confidence = &conf
	if err := e.validate(s); err != nil {
		e.t.Errorf("parsertest: the writer's rules refuse the typed record of %s: %v", f.Locator, err)
	}
	return nil
}

func (e *recoverRecorder) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	e.unmapped = append(e.unmapped, recoverUnmapped{facts: f, reason: reason, row: e.cur})
	e.cur = nil
	return nil
}

func (e *recoverRecorder) Warn(_ context.Context, locator, reason string) error {
	e.warns = append(e.warns, Warning{Locator: locator, Reason: reason})
	return nil
}

func (e *recoverRecorder) Note(k, v string)      { e.notes[k] = v }
func (e *recoverRecorder) Progress(int64, int64) {}

var _ parse.RecoveryEmitter = (*recoverRecorder)(nil)

// capturingMappings wraps each mapping so the row it reads is captured before it maps it.
func capturingMappings(ms []parse.TableMapping, cols map[string][]string, e *recoverRecorder) []parse.TableMapping {
	out := make([]parse.TableMapping, len(ms))
	for i, m := range ms {
		inner, table := m.Map, m.Table
		m.Map = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
			e.cur = capture(mc, row, table, cols[table])
			return inner(mc, row)
		}
		out[i] = m
	}
	return out
}

// AssertClaimedTablesMapRecoveredRows runs the typed recovery runner (recrun.Run) with the parser's Mappings
// and Meta().Claims over the bundle, with a strict recovery emitter, and requires:
//
//	(a) every want.Records entry has a typed record for the same table and cell (file, page, offset, length), and
//	    a cell has exactly as many typed records as the oracle lists for it, each with the oracle's method, relation, basis, origin, confidence and notes and the oracle's type, time(s),
//	    summary, body and payload;
//	(b) every typed record's values equal one version in want.Written of its table (nothing invented);
//	(c) every want.Unmapped entry was reported unmapped with a reason containing Contains;
//	(d) per claimed table Delivered == Mapped + Unmapped, no row was skipped, every record has valid facts;
//	(e) the writer's rules (records.NewValidator) refuse no record, and no mapping set a recovery field.
func AssertClaimedTablesMapRecoveredRows(t testing.TB, h *Harness, p parse.RowMapper, b Bundle, want RecoverExpect) {
	t.Helper()
	in := b.Input(h, p, false)
	ids := []string{in.Primary.ID}
	for _, a := range in.Artifacts {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	validate, err := records.NewValidator(h.Case, ids)
	if err != nil {
		t.Fatalf("parsertest: %v", err)
		return
	}
	e := &recoverRecorder{t: t, validate: validate, notes: map[string]string{}}
	meta := p.Meta()
	rep, err := recrun.Run(context.Background(), in, e, capturingMappings(p.Mappings(), want.Columns, e), meta.Claims)
	if err != nil {
		t.Errorf("parsertest: the typed recovery pass failed: %v", err)
		return
	}
	for _, tc := range rep.Tables { // (d)
		if tc.Delivered != tc.Mapped+tc.Unmapped {
			t.Errorf("table %s: %d rows delivered, %d mapped and %d unmapped: a row was dropped silently", tc.Table, tc.Delivered, tc.Mapped, tc.Unmapped)
		}
		if tc.Skipped != 0 {
			t.Errorf("table %s: %d rows skipped for invalid facts", tc.Table, tc.Skipped)
		}
	}
	// (a) the oracle lists one entry per typed record, in emission order within a cell
	cells := map[string][]RecoverExpectRecord{}
	var order []string
	for _, w := range want.Records {
		k := cellKey(w.Table, w.File, w.Page, w.Offset, w.Length)
		if _, seen := cells[k]; !seen {
			order = append(order, k)
		}
		cells[k] = append(cells[k], w)
	}
	for _, k := range order {
		ws := cells[k]
		var gs []recoverEmitted
		for _, g := range e.recs {
			f := g.facts
			if cellKey(f.Table, string(f.Role), f.Page, f.Range.Offset, f.Range.Length) == k && ridMatches(ws[0].Rowid, g.row) {
				gs = append(gs, g)
			}
		}
		w0 := ws[0]
		id := fmt.Sprintf("%s rowid %s (%s page %d offset %d)", w0.Table, ridString(w0.Rowid), w0.File, w0.Page, w0.Offset)
		switch {
		case len(gs) == 0:
			t.Errorf("%s: no typed record for that cell", id)
		case len(gs) != len(ws):
			t.Errorf("%s: %d typed records for the cell, the oracle expects %d", id, len(gs), len(ws))
		default:
			for i := range ws {
				compareRecovered(t, b, gs[i], ws[i])
			}
		}
	}
	// (a2) the digest of every want covers the records the oracle does not list in full
	dig := map[string][]RecoverDigest{}
	for _, d := range want.Digests {
		k := cellKey(d.Table, d.File, d.Page, d.Offset, d.Length)
		dig[k] = append(dig[k], d)
	}
	for _, g := range e.recs {
		f := g.facts
		ds := dig[cellKey(f.Table, string(f.Role), f.Page, f.Range.Offset, f.Range.Length)]
		var cands []RecoverDigest
		for _, d := range ds {
			if g.row != nil && g.row.hasRowid && g.row.rowid == d.Rowid {
				cands = append(cands, d)
			}
		}
		if len(cands) == 0 {
			continue
		}
		got := recordDigest(g.rec)
		ok := false
		for _, d := range cands {
			ok = ok || d.Digest == got
		}
		if !ok {
			t.Errorf("typed record %s: its digest %s is not the oracle's for that cell (type, time, body or payload differ)", f.Locator, got[:16])
		}
	}
	// (b)
	for _, g := range e.recs {
		if !isWritten(want, g) {
			t.Errorf("typed record %s: its values are no version the scenario wrote (invented or misread): %v", g.facts.Locator, g.row)
		}
	}
	// (c)
	for _, u := range want.Unmapped {
		found := false
		for _, g := range e.unmapped {
			found = found || (strings.EqualFold(g.facts.Table, u.Table) && strings.Contains(g.reason, u.Contains) &&
				(u.Rowid == nil || (g.row != nil && g.row.hasRowid && g.row.rowid == *u.Rowid)))
		}
		if !found {
			t.Errorf("unmapped %s rowid %s: no row was reported unmapped with a reason containing %q", u.Table, ridString(u.Rowid), u.Contains)
		}
	}
}

func ridString(r *int64) string {
	if r == nil {
		return "(lost)"
	}
	return strconv.FormatInt(*r, 10)
}

func cellKey(table, file string, page uint32, offset, length int64) string {
	return strings.ToLower(table) + "|" + file + "|" + strconv.FormatUint(uint64(page), 10) + "|" + strconv.FormatInt(offset, 10) + "|" + strconv.FormatInt(length, 10)
}

func ridMatches(want *int64, row *recoveredCapture) bool {
	return want == nil || (row != nil && row.hasRowid && row.rowid == *want)
}

func compareRecovered(t testing.TB, b Bundle, g recoverEmitted, w RecoverExpectRecord) {
	t.Helper()
	id := fmt.Sprintf("%s rowid %s (%s page %d offset %d)", w.Table, ridString(w.Rowid), w.File, w.Page, w.Offset)
	f := g.facts
	if f.Method != w.Method || f.Relation != w.Relation || f.Basis != w.Basis || f.Origin != w.Origin || f.Confidence != w.Confidence {
		t.Errorf("%s: facts method %q relation %q basis %q origin %q confidence %d, the oracle expects %q %q %q %q %d",
			id, f.Method, f.Relation, f.Basis, f.Origin, f.Confidence, w.Method, w.Relation, w.Basis, w.Origin, w.Confidence)
	}
	wantNotes := append(slices.Clone(w.Notes), recrun.NoteJoinsFromLiveView)
	if !slices.Equal(f.Notes, wantNotes) {
		t.Errorf("%s: notes %v, the oracle expects %v", id, f.Notes, wantNotes)
	}
	art, ok := b.ByRole[w.File]
	r := g.rec
	if !ok || r.ArtifactID != art.ID {
		t.Errorf("%s: the record lies on artifact %q, want the %s artifact", id, r.ArtifactID, w.File)
	}
	if r.Range == nil || r.Range.Offset != w.Offset || r.Range.Length != w.Length {
		t.Errorf("%s: record range %+v, want %d+%d", id, r.Range, w.Offset, w.Length)
	}
	e := w.Record
	if r.Type != e.Type {
		t.Errorf("%s: Type %q, want %q", id, r.Type, e.Type)
	}
	compareTime(t, id+" time", r.Time, e.Time)
	compareTime(t, id+" time_end", r.TimeEnd, e.TimeEnd)
	gotTimes := map[string]time.Time{}
	for _, nt := range r.Times {
		if nt.Time.Basis != "" && nt.Time.Basis != records.BasisUTC {
			t.Errorf("%s: time %s has basis %q", id, nt.Kind, nt.Time.Basis)
		}
		gotTimes[nt.Kind] = nt.Time.T
	}
	if len(r.Times) != len(e.Times) {
		t.Errorf("%s: %d Times %v, want %d %v", id, len(r.Times), gotTimes, len(e.Times), e.Times)
	}
	for k, s := range e.Times {
		wt, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Errorf("%s: oracle time %s: %v", id, k, err)
		} else if gt, ok := gotTimes[k]; !ok || !gt.Equal(wt) {
			t.Errorf("%s: time %s = %v, want %s", id, k, gt, s)
		}
	}
	if e.Summary != nil && r.Summary != *e.Summary {
		t.Errorf("%s: Summary %q, want %q", id, r.Summary, *e.Summary)
	}
	wantBody := ""
	if e.Body != nil {
		wantBody = *e.Body
	}
	if r.Body != wantBody {
		t.Errorf("%s: Body %q, want %q", id, clip(r.Body), clip(wantBody))
	}
	gp, wp := normalize(r.Payload), normalize(subst(e.Payload, b))
	if !reflect.DeepEqual(gp, wp) {
		gj, _ := json.Marshal(gp)
		wj, _ := json.Marshal(wp)
		t.Errorf("%s: payload differs\n got %s\nwant %s", id, clip(string(gj)), clip(string(wj)))
	}
}

// isWritten reports whether the captured values of g equal a version the scenario wrote to the table the
// row is attributed to (or, for a dropped table of the same shape, listed under that table).
func isWritten(want RecoverExpect, g recoverEmitted) bool {
	if g.row == nil || !g.row.hasRowid {
		return false
	}
	if len(g.row.vals) == 0 {
		return false // a mapping that read no column of the row proves nothing
	}
	cols := want.Columns[g.facts.Table]
	for _, w := range want.Written {
		if !strings.EqualFold(w.Table, g.facts.Table) || w.Rowid != g.row.rowid || len(w.Values) != len(cols) {
			continue
		}
		match := true
		for i, name := range cols {
			got, captured := g.row.vals[name]
			if !captured {
				continue // the mapping does not read the column
			}
			if !sameValue(got, w.Values[i]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func sameValue(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case json.Number:
		n, err := w.Int64()
		g, ok := got.(int64)
		return err == nil && ok && g == n
	case string:
		g, ok := got.(string)
		return ok && g == w
	case map[string]any:
		raw, _ := w["blob_b64"].(string)
		data, err := base64.StdEncoding.DecodeString(raw)
		g, ok := got.(blobValue)
		return err == nil && ok && string(g) == string(data)
	}
	return false
}

// RecoveredRowResult is what one recovered row became.
type RecoveredRowResult struct {
	Records  []records.Record
	Unmapped string // the reason, "" when the row was mapped
	Err      error  // a fatal error of the mapping
}

// FirstClaimedRow returns a deep copy of the first recovered row that the typed pass would deliver for the
// parser's claims (sqlitefile.ClaimMatch accepts it) in table and for which pick is true; ok is false when
// there is none. The copy owns its memory, so a test may change it (clear the rowid, clip a value) and hand it
// to MapRecoveredRow.
func FirstClaimedRow(t testing.TB, h *Harness, p parse.RowMapper, b Bundle, table string, pick func(sqlitefile.RecoveredRow) bool) (rr sqlitefile.RecoveredRow, ok bool) {
	t.Helper()
	ctx := context.Background()
	in := b.Input(h, p, false)
	db := openBundle(ctx, t, in)
	if db == nil {
		return rr, false
	}
	defer db.Release()
	errFound := errors.New("found")
	_, err := db.ClaimedHistory(ctx, func(tb string) bool { return strings.EqualFold(tb, table) }, func(r sqlitefile.RecoveredRow) error {
		if pick != nil && !pick(r) {
			return nil
		}
		rr, ok = cloneRow(r), true
		return errFound
	})
	if err != nil && !errors.Is(err, errFound) {
		t.Fatalf("parsertest: %v", err)
	}
	return rr, ok
}

func cloneRow(r sqlitefile.RecoveredRow) sqlitefile.RecoveredRow {
	if r.Rowid != nil {
		v := *r.Rowid
		r.Rowid = &v
	}
	r.Values = slices.Clone(r.Values)
	for i := range r.Values {
		r.Values[i].Bytes = slices.Clone(r.Values[i].Bytes)
	}
	if r.WAL != nil {
		v := *r.WAL
		r.WAL = &v
	}
	if r.Journal != nil {
		v := *r.Journal
		r.Journal = &v
	}
	r.Notes = slices.Clone(r.Notes)
	return r
}

func openBundle(ctx context.Context, t testing.TB, in *parse.Input) *sqlitedb.DB {
	t.Helper()
	files := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		files.WAL, files.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		files.Journal, files.JournalSize = a.R, a.Size
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		t.Fatalf("parsertest: %v", err)
		return nil
	}
	return db
}

// MapRecoveredRow maps one recovered row (for instance a row of the fixture with its rowid cleared, as a
// carved row loses it) through the mapping of rr.Table exactly as the typed pass does: the row is rebuilt
// with sqlitedb.FromRecovered and the error is classified like recrun.Run does (a *rowrun.RowError, an
// undecidable join or no record is Unmapped, anything else is Err).
func MapRecoveredRow(t testing.TB, h *Harness, p parse.RowMapper, b Bundle, rr sqlitefile.RecoveredRow) RecoveredRowResult {
	t.Helper()
	ctx := context.Background()
	in := b.Input(h, p, false)
	db := openBundle(ctx, t, in)
	if db == nil {
		return RecoveredRowResult{}
	}
	defer db.Release()
	var mp *parse.TableMapping
	for _, m := range p.Mappings() {
		if strings.EqualFold(m.Table, rr.Table) {
			m := m
			mp = &m
		}
	}
	if mp == nil {
		t.Fatalf("parsertest: the parser has no mapping for table %q", rr.Table)
		return RecoveredRowResult{}
	}
	var need []string
	for _, c := range mp.Columns {
		if c.Required {
			need = append(need, c.Name)
		}
	}
	tbl, err := db.Table(ctx, mp.Table, need, nil)
	if err != nil {
		t.Fatalf("parsertest: table %s: %v", mp.Table, err)
		return RecoveredRowResult{}
	}
	row, err := sqlitedb.FromRecovered(tbl, rr)
	if err != nil {
		t.Fatalf("parsertest: %v", err)
		return RecoveredRowResult{}
	}
	defer row.Release()
	mc := sqlitedb.NewContext(ctx, db, in, noteSink{})
	recs, merr := mp.Map(mc, row)
	mc.Close()
	if err := mc.Err(); err != nil {
		return RecoveredRowResult{Err: err}
	}
	var re *rowrun.RowError
	switch {
	case errors.As(merr, &re):
		return RecoveredRowResult{Unmapped: re.Reason}
	case errors.Is(merr, sqlitedb.ErrKeyUndecidable):
		return RecoveredRowResult{Unmapped: "a join of the row is undecidable: " + merr.Error()}
	case merr != nil:
		return RecoveredRowResult{Err: merr}
	case len(recs) == 0:
		return RecoveredRowResult{Unmapped: "the mapping produced no record"}
	}
	return RecoveredRowResult{Records: recs}
}

type noteSink struct{}

func (noteSink) Note(string, string) {}
