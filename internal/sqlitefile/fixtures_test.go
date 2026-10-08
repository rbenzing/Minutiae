package sqlitefile_test

// Committed fixtures with independent oracles (plan 3I, Task 14). The fixtures
// under testdata/ are real files (written by the engine, by the test builder
// or by the container family); every expect.json was produced by code that
// shares nothing with the library: the engine's own answers, a minimal cell,
// WAL and journal walker in the generator (fixturegen_test.go) or the python
// oracle (tools/fixtures/sqlite_oracle.py). These tests need no engine and no
// Docker.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// ---- the expectation file ----

type fxVal struct {
	T string `json:"t"` // null, int, float (bit pattern in hex), text, blob (base64)
	V string `json:"v,omitempty"`
}

type fxLoc struct {
	File    string `json:"file"` // db, wal, journal
	Page    uint32 `json:"page"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	CellHex string `json:"cell_hex"`
}

type fxCellRow struct {
	Rowid int64 `json:"rowid"`
	fxLoc
}

type fxGenerator struct {
	Tool       string            `json:"tool"`
	Version    string            `json:"version"`
	Class      string            `json:"class"`
	Statements []string          `json:"statements"`
	FileSHA256 map[string]string `json:"file_sha256"`
}

type fxHeader struct {
	PageSize         int    `json:"page_size"`
	Reserved         int    `json:"reserved"`
	Encoding         string `json:"encoding"`
	HeaderPages      uint32 `json:"header_pages"`
	HeaderPagesValid bool   `json:"header_pages_valid"`
	FilePages        uint32 `json:"file_pages"`
	ChangeCounter    uint32 `json:"change_counter"`
	VersionValidFor  uint32 `json:"version_valid_for"`
	SQLiteVersion    uint32 `json:"sqlite_version"`
	SchemaCookie     uint32 `json:"schema_cookie"`
	SchemaFormat     uint32 `json:"schema_format"`
	FreelistTrunk    uint32 `json:"freelist_trunk"`
	FreelistCount    uint32 `json:"freelist_count"`
	AutoVacuum       int    `json:"auto_vacuum"`
	LargestRoot      uint32 `json:"largest_root"`
	UserVersion      int32  `json:"user_version"`
	ApplicationID    uint32 `json:"application_id"`
	WriteVersion     int    `json:"write_version"`
	ReadVersion      int    `json:"read_version"`
}

type fxSchemaRow struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	TblName  string `json:"tbl_name"`
	Rootpage uint32 `json:"rootpage"`
	SQL      string `json:"sql"`
}

type fxLiveTable struct {
	WithoutRowid bool      `json:"without_rowid"`
	Rows         [][]fxVal `json:"rows"` // rowid tables: [rowid, columns...]
}

type fxInfo struct {
	IntegrityCheck string `json:"integrity_check"`
	FreelistCount  int64  `json:"freelist_count"`
	PageCount      int64  `json:"page_count"`
}

type fxFreelist struct {
	Trunks []uint32 `json:"trunks"`
	Leaves []uint32 `json:"leaves"`
}

type fxFrame struct {
	Slot       uint32 `json:"slot"`
	Page       uint32 `json:"page"`
	DBSize     uint32 `json:"db_size"`
	Salt1      uint32 `json:"salt1"`
	Salt2      uint32 `json:"salt2"`
	Check1     uint32 `json:"check1"`
	Check2     uint32 `json:"check2"`
	State      string `json:"state"`
	Linked     bool   `json:"linked"`
	Generation int    `json:"generation"`
	Offset     int64  `json:"offset"`
}

type fxGeneration struct {
	Salt1     uint32 `json:"salt1"`
	Salt2     uint32 `json:"salt2"`
	FirstSlot uint32 `json:"first_slot"`
	Slots     uint32 `json:"slots"`
	Anchored  bool   `json:"anchored"`
	Age       uint32 `json:"age"`
	Commits   uint32 `json:"commits"`
}

type fxWAL struct {
	HeaderValid        bool           `json:"header_valid"`
	BigEndian          bool           `json:"big_endian"`
	PageSize           uint32         `json:"page_size"`
	CheckpointSeq      uint32         `json:"checkpoint_seq"`
	Salt1              uint32         `json:"salt1"`
	Salt2              uint32         `json:"salt2"`
	FrameSlots         uint32         `json:"frame_slots"`
	TrailingBytes      int64          `json:"trailing_bytes"`
	FramesValid        uint32         `json:"frames_valid"`
	FramesCommitted    uint32         `json:"frames_committed"`
	LastCommit         uint32         `json:"last_commit"`
	Commits            uint32         `json:"commits"`
	FramesUncommitted  uint32         `json:"frames_uncommitted"`
	FramesBroken       uint32         `json:"frames_broken"`
	FramesDetached     uint32         `json:"frames_detached"`
	FramesStale        uint32         `json:"frames_stale"`
	DBPagesAfterCommit uint32         `json:"db_pages_after_commit"`
	MaxPageNumber      uint32         `json:"max_page_number"`
	Frames             []fxFrame      `json:"frames"`
	Generations        []fxGeneration `json:"generations"`
}

type fxJournalSeg struct {
	Offset          int64  `json:"offset"`
	DeclaredRecords uint32 `json:"declared_records"`
	Records         uint32 `json:"records"`
}

type fxJournalRec struct {
	Index      int    `json:"index"`
	Segment    int    `json:"segment"`
	Page       uint32 `json:"page"`
	Offset     int64  `json:"offset"`
	ChecksumOK bool   `json:"checksum_ok"`
	Applied    bool   `json:"applied"`
}

type fxJournal struct {
	Hot              bool           `json:"hot"`
	HeaderValid      bool           `json:"header_valid"`
	ZeroedHeader     bool           `json:"zeroed_header"`
	PageSize         uint32         `json:"page_size"`
	SectorSize       uint32         `json:"sector_size"`
	InitialPages     uint32         `json:"initial_pages"`
	Nonce            uint32         `json:"nonce"`
	Applied          bool           `json:"applied"`
	NotAppliedReason string         `json:"not_applied_reason"`
	RecordsTotal     uint32         `json:"records_total"`
	RecordsValid     uint32         `json:"records_valid"`
	AppliedRecords   uint32         `json:"applied_records"`
	Segments         []fxJournalSeg `json:"segments"`
	Records          []fxJournalRec `json:"records"` // empty when the header is zeroed
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

type fxUnreachable struct {
	Table  string  `json:"table"`
	Rowid  *int64  `json:"rowid"`
	Values []fxVal `json:"values"`
	Marker string  `json:"marker"`
}

type fxWarnings struct {
	Live []string `json:"live"`
}

type fxExpect struct {
	Generator   fxGenerator                `json:"generator"`
	NotSQLite   bool                       `json:"not_sqlite,omitempty"`
	Header      *fxHeader                  `json:"header,omitempty"`
	Schema      []fxSchemaRow              `json:"schema,omitempty"`
	Live        map[string]fxLiveTable     `json:"live,omitempty"`
	Cells       map[string][]fxCellRow     `json:"cells,omitempty"`
	Info        *fxInfo                    `json:"info,omitempty"`
	Freelist    *fxFreelist                `json:"freelist,omitempty"`
	WAL         *fxWAL                     `json:"wal,omitempty"`
	Journal     *fxJournal                 `json:"journal,omitempty"`
	AsFound     map[string][][]fxVal       `json:"as_found,omitempty"` // raw rows: [rowid, record values...]
	History     []fxHistory                `json:"history,omitempty"`
	Unreachable []fxUnreachable            `json:"unreachable,omitempty"`
	Warnings    fxWarnings                 `json:"warnings"`
	Extra       map[string]json.RawMessage `json:"extra,omitempty"`
}

// ---- typed values ----

func fxEnc(v any) fxVal {
	switch x := v.(type) {
	case nil:
		return fxVal{T: "null"}
	case int64:
		return fxVal{T: "int", V: strconv.FormatInt(x, 10)}
	case float64:
		return fxVal{T: "float", V: strconv.FormatUint(math.Float64bits(x), 16)}
	case string:
		return fxVal{T: "text", V: x}
	case []byte:
		return fxVal{T: "blob", V: base64.StdEncoding.EncodeToString(x)}
	}
	panic(fmt.Sprintf("fxEnc: %T", v))
}

func fxDec(t testing.TB, v fxVal) any {
	t.Helper()
	switch v.T {
	case "null":
		return nil
	case "int":
		n, err := strconv.ParseInt(v.V, 10, 64)
		if err != nil {
			t.Fatalf("bad int %q", v.V)
		}
		return n
	case "float":
		n, err := strconv.ParseUint(v.V, 16, 64)
		if err != nil {
			t.Fatalf("bad float %q", v.V)
		}
		return math.Float64frombits(n)
	case "text":
		return v.V
	case "blob":
		b, err := base64.StdEncoding.DecodeString(v.V)
		if err != nil {
			t.Fatalf("bad blob %q", v.V)
		}
		return b
	}
	t.Fatalf("bad value type %q", v.T)
	return nil
}

func fxDecAll(t testing.TB, vs []fxVal) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = fxDec(t, v)
	}
	return out
}

func sameAll(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameValue(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ---- loading ----

type fxLoaded struct {
	spec                   fixtureSpec
	exp                    fxExpect
	db, wal, journal       []byte
	dbName, walName, jName string
}

func gunzipFile(t testing.TB, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("fixture file missing: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return b
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// loadFx reads a fixture and checks every file's sha256 against the oracle
// FIRST, so a stale oracle (or a stale image) fails before anything else.
func loadFx(t testing.TB, spec fixtureSpec) *fxLoaded {
	t.Helper()
	dir := "testdata"
	f := &fxLoaded{spec: spec, dbName: spec.Name + ".db", walName: spec.Name + ".db-wal", jName: spec.Name + ".db-journal"}
	raw, err := os.ReadFile(filepath.Join(dir, spec.Name+".expect.json"))
	if err != nil {
		t.Fatalf("fixture %s: no oracle committed: %v", spec.Name, err)
	}
	if err := json.Unmarshal(raw, &f.exp); err != nil {
		t.Fatalf("fixture %s: oracle: %v", spec.Name, err)
	}
	want := f.exp.Generator.FileSHA256
	n := 1
	f.db = gunzipFile(t, filepath.Join(dir, f.dbName+".gz"))
	check := func(name string, b []byte) {
		if want[name] != sha256hex(b) {
			t.Fatalf("fixture %s: %s has sha256 %s, the oracle describes %s (regenerate image and oracle together)", spec.Name, name, sha256hex(b), want[name])
		}
	}
	check(f.dbName, f.db)
	if spec.WAL {
		f.wal = gunzipFile(t, filepath.Join(dir, f.walName+".gz"))
		check(f.walName, f.wal)
		n++
	}
	if spec.Journal {
		f.journal = gunzipFile(t, filepath.Join(dir, f.jName+".gz"))
		check(f.jName, f.journal)
		n++
	}
	if len(want) != n {
		t.Fatalf("fixture %s: the oracle lists %d files, the fixture has %d", spec.Name, len(want), n)
	}
	return f
}

func (f *fxLoaded) lib(t testing.TB) *sqlitefile.DB {
	t.Helper()
	return openLibrary(t, f.db, f.wal, f.journal)
}

func (f *fxLoaded) fileBytes(name string) []byte {
	switch name {
	case "db":
		return f.db
	case "wal":
		return f.wal
	case "journal":
		return f.journal
	}
	return nil
}

func eachFixture(t *testing.T, fn func(t *testing.T, f *fxLoaded)) {
	t.Helper()
	for _, spec := range fixtureSpecs {
		if spec.NotSQLite {
			continue
		}
		t.Run(spec.Name, func(t *testing.T) { fn(t, loadFx(t, spec)) })
	}
}

// ---- immutability and generator ----

func TestFixturesAreImmutable(t *testing.T) {
	for _, spec := range fixtureSpecs {
		f := loadFx(t, spec)
		if f.exp.Generator.Class != spec.Class {
			t.Errorf("%s: the oracle says class %q, the table says %q", spec.Name, f.exp.Generator.Class, spec.Class)
		}
		if len(f.exp.Generator.Statements) == 0 {
			t.Errorf("%s: the oracle records no generating statements", spec.Name)
		}
	}
}

// TestFixtureGeneratorIsDeterministic runs the Go generator twice in this
// process for the class A fixtures and compares every byte. Class B is
// excluded by name (the engine draws salts and nonces from its PRNG) and
// class C is not written by Go at all. It does not compare with the committed
// files, so an engine upgrade breaks nothing here; it only makes a
// regeneration due.
//
// Unlike the other fixture tests, which need no engine, this one runs the
// modernc.org/sqlite test dependency (the class A and B engine files are made
// by it).
func TestFixtureGeneratorIsDeterministic(t *testing.T) {
	var b []string
	for _, s := range fixtureSpecs {
		if s.Class == "B" {
			b = append(b, s.Name)
		}
	}
	sort.Strings(b)
	want := append([]string(nil), classBNames...)
	sort.Strings(want)
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("the class B fixtures are %v, the exclusion list is %v", b, want)
	}
	first := fxGenerateClassA(t)
	second := fxGenerateClassA(t)
	if len(first) == 0 {
		t.Fatal("no class A fixture was generated")
	}
	var names []string
	for _, s := range fixtureSpecs {
		if s.Class == "A" {
			names = append(names, s.Name)
		}
	}
	for _, n := range names {
		a, ok := first[n]
		if !ok {
			t.Errorf("class A fixture %s was not generated", n)
			continue
		}
		b := second[n]
		for file, data := range a.files {
			if !bytes.Equal(data, b.files[file]) {
				t.Errorf("%s: generating twice gave different bytes (%s)", file, n)
			}
		}
		if len(a.files) != len(b.files) {
			t.Errorf("%s: file sets differ between runs", n)
		}
	}
}

// ---- live ----

func normRowsFromExpect(t testing.TB, f *fxLoaded) map[string][]engineRow {
	out := map[string][]engineRow{}
	for n, lt := range f.exp.Live {
		rows := make([]engineRow, 0, len(lt.Rows))
		for _, r := range lt.Rows {
			vals := fxDecAll(t, r)
			if lt.WithoutRowid {
				rows = append(rows, engineRow{Vals: vals})
				continue
			}
			rows = append(rows, engineRow{Rowid: vals[0].(int64), HasRowid: true, Vals: vals[1:]})
		}
		out[n] = rows
	}
	return out
}

func headerDiff(info sqlitefile.Info, h *fxHeader) string {
	got := fxHeader{
		PageSize: info.PageSize, Reserved: info.Reserved, Encoding: info.Encoding.String(),
		HeaderPages: info.HeaderPages, HeaderPagesValid: info.HeaderPagesValid, FilePages: info.FilePages,
		ChangeCounter: info.ChangeCounter, VersionValidFor: info.VersionValidFor, SQLiteVersion: info.SQLiteVersion,
		SchemaCookie: info.SchemaCookie, SchemaFormat: info.SchemaFormat,
		FreelistTrunk: info.FreelistTrunk, FreelistCount: info.FreelistCount, AutoVacuum: int(info.AutoVacuum),
		LargestRoot: info.LargestRoot, UserVersion: info.UserVersion, ApplicationID: info.ApplicationID,
		WriteVersion: int(info.WriteVersion), ReadVersion: int(info.ReadVersion),
	}
	if got != *h {
		return fmt.Sprintf("library %+v, oracle %+v", got, *h)
	}
	return ""
}

func codesOf(ws ...[]sqlitefile.Warning) []string {
	seen := map[string]bool{}
	for _, l := range ws {
		for _, w := range l {
			seen[w.Code] = true
		}
	}
	out := []string{}
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func compareLive(t *testing.T, f *fxLoaded) {
	t.Helper()
	ctx := context.Background()
	d := f.lib(t)
	v := d.Live()
	defer v.Release()
	if diff := headerDiff(d.Info(), f.exp.Header); diff != "" {
		t.Errorf("Info: %s", diff)
	}
	sch, err := v.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var got []fxSchemaRow
	for _, o := range sch.Objects {
		got = append(got, fxSchemaRow{Type: o.Type, Name: o.Name, TblName: o.TblName, Rootpage: o.RootPage, SQL: o.SQL})
	}
	if !reflect.DeepEqual(got, f.exp.Schema) {
		t.Errorf("Schema differs:\n library %+v\n oracle  %+v", got, f.exp.Schema)
	}
	if diff, _ := diffDumps(liveDumpRows(t, v), normRowsFromExpect(t, f)); diff != "" {
		t.Errorf("live rows: %s", diff)
	}
	fl, err := v.Freelist(ctx)
	if err != nil {
		t.Fatalf("Freelist: %v", err)
	}
	if f.exp.Freelist != nil {
		if !reflect.DeepEqual(nz(fl.Trunks), nz(f.exp.Freelist.Trunks)) || !reflect.DeepEqual(nz(fl.Leaves), nz(f.exp.Freelist.Leaves)) {
			t.Errorf("Freelist: library trunks %v leaves %v, oracle trunks %v leaves %v", fl.Trunks, fl.Leaves, f.exp.Freelist.Trunks, f.exp.Freelist.Leaves)
		}
		if int64(fl.Walked) != f.exp.Info.FreelistCount {
			t.Errorf("Freelist walked %d pages, the engine says freelist_count %d", fl.Walked, f.exp.Info.FreelistCount)
		}
	}
	if _, err := v.Layout(ctx); err != nil {
		t.Fatalf("Layout: %v", err)
	}
	if codes := codesOf(d.Warnings(), v.Warnings()); !reflect.DeepEqual(codes, nz(f.exp.Warnings.Live)) {
		t.Errorf("warnings: library %v, oracle %v", codes, f.exp.Warnings.Live)
	}
}

func nz[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func TestFixturesLiveMatchOracle(t *testing.T) {
	eachFixture(t, func(t *testing.T, f *fxLoaded) { compareLive(t, f) })
}

// ---- as found ----

func TestFixturesAsFoundMatchOracle(t *testing.T) {
	eachFixture(t, func(t *testing.T, f *fxLoaded) {
		ctx := context.Background()
		d := f.lib(t)
		v := d.AsFound()
		defer v.Release()
		sch, err := v.Schema(ctx)
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		seen := 0
		for _, o := range sch.Objects {
			if o.Type != "table" || o.Virtual || o.RootPage == 0 {
				continue
			}
			seen++
			want, ok := f.exp.AsFound[strings.ToLower(o.Name)]
			if !ok {
				t.Errorf("as found: the oracle has no table %s", o.Name)
				continue
			}
			tb, err := v.Table(ctx, o.Name)
			if err != nil {
				t.Fatalf("Table %s: %v", o.Name, err)
			}
			var got [][]any
			err = tb.Rows(ctx, func(r sqlitefile.Row) bool {
				row := []any{}
				if r.HasRowid {
					row = append(row, r.Rowid)
				}
				for _, val := range r.Values {
					row = append(row, normValue(val))
				}
				got = append(got, row)
				return true
			})
			if err != nil {
				t.Fatalf("Rows %s: %v", o.Name, err)
			}
			if len(got) != len(want) {
				t.Errorf("as found %s: library %d rows, oracle %d", o.Name, len(got), len(want))
				continue
			}
			for i := range want {
				if !sameAll(got[i], fxDecAll(t, want[i])) {
					t.Errorf("as found %s row %d: library %v, oracle %v", o.Name, i, got[i], fxDecAll(t, want[i]))
					break
				}
			}
		}
		if seen != len(f.exp.AsFound) {
			t.Errorf("as found: library reads %d tables, oracle %d", seen, len(f.exp.AsFound))
		}
	})
}

// ---- WAL ----

func TestFixturesWALMatchOracle(t *testing.T) {
	for _, spec := range fixtureSpecs {
		if !spec.WAL {
			continue
		}
		t.Run(spec.Name, func(t *testing.T) {
			f := loadFx(t, spec)
			d := f.lib(t)
			sc := d.WAL()
			if sc == nil || f.exp.WAL == nil {
				t.Fatalf("no WAL scan (library %v, oracle %v)", sc != nil, f.exp.WAL != nil)
			}
			w, e := sc.Info, f.exp.WAL
			got := fxWAL{
				HeaderValid: w.HeaderValid, BigEndian: w.BigEndianChecksums, PageSize: w.PageSize, CheckpointSeq: w.CheckpointSeq,
				Salt1: w.Salt1, Salt2: w.Salt2, FrameSlots: w.FrameSlots, TrailingBytes: w.TrailingBytes,
				FramesValid: w.FramesValid, FramesCommitted: w.FramesCommitted, LastCommit: w.LastCommit, Commits: w.Commits,
				FramesUncommitted: w.FramesUncommitted, FramesBroken: w.FramesBroken, FramesDetached: w.FramesDetached,
				FramesStale: w.FramesStale, DBPagesAfterCommit: w.DBPagesAfterCommit, MaxPageNumber: w.MaxPageNumber,
			}
			want := *e
			want.Frames, want.Generations = nil, nil
			if !reflect.DeepEqual(got, want) {
				t.Errorf("WALInfo: library %+v, oracle %+v", got, want)
			}
			if len(sc.Frames) != len(e.Frames) {
				t.Fatalf("frames: library %d, oracle %d", len(sc.Frames), len(e.Frames))
			}
			for i, fr := range sc.Frames {
				g := fxFrame{
					Slot: fr.Slot, Page: fr.Page, DBSize: fr.DBSize, Salt1: fr.Salt1, Salt2: fr.Salt2, Check1: fr.Check1, Check2: fr.Check2,
					State: fr.State.String(), Linked: fr.Linked, Generation: fr.Generation, Offset: fr.Offset,
				}
				if g != e.Frames[i] {
					t.Errorf("frame %d: library %+v, oracle %+v", i+1, g, e.Frames[i])
				}
			}
			if len(w.Generations) != len(e.Generations) {
				t.Fatalf("generations: library %d, oracle %d", len(w.Generations), len(e.Generations))
			}
			for i, g := range w.Generations {
				got := fxGeneration{Salt1: g.Salt1, Salt2: g.Salt2, FirstSlot: g.FirstSlot, Slots: g.Slots, Anchored: g.Anchored, Age: g.Age, Commits: g.Commits}
				if got != e.Generations[i] {
					t.Errorf("generation %d: library %+v, oracle %+v", i, got, e.Generations[i])
				}
			}
			if !w.UsedByLive {
				t.Errorf("the committed WAL is not used by Live")
			}
			compareLive(t, f) // Live() with the WAL applied equals the oracle's rows
		})
	}
}

// ---- journal ----

func TestFixturesJournalMatchOracle(t *testing.T) {
	for _, spec := range fixtureSpecs {
		if !spec.Journal {
			continue
		}
		t.Run(spec.Name, func(t *testing.T) {
			f := loadFx(t, spec)
			d := f.lib(t)
			sc := d.Journal()
			if sc == nil || f.exp.Journal == nil {
				t.Fatalf("no journal scan (library %v, oracle %v)", sc != nil, f.exp.Journal != nil)
			}
			j, e := sc.Info, f.exp.Journal
			got := fxJournal{
				Hot: j.Hot, HeaderValid: j.HeaderValid, ZeroedHeader: j.ZeroedHeader, PageSize: j.PageSize, SectorSize: j.SectorSize,
				InitialPages: j.InitialPages, Nonce: j.Nonce, Applied: j.Applied, NotAppliedReason: j.NotAppliedReason,
				RecordsTotal: j.RecordsTotal, RecordsValid: j.RecordsValid, AppliedRecords: j.AppliedRecords,
			}
			want := *e
			want.Segments, want.Records = nil, nil
			if e.ZeroedHeader { // the sector, nonce and counts of a zeroed header are the library's to derive
				got.PageSize, got.SectorSize, got.Nonce, got.RecordsTotal, got.RecordsValid, got.InitialPages = 0, 0, 0, 0, 0, 0
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("JournalInfo: library %+v, oracle %+v", got, want)
			}
			if e.ZeroedHeader {
				return
			}
			if len(j.Segments) != len(e.Segments) {
				t.Fatalf("segments: library %d, oracle %d", len(j.Segments), len(e.Segments))
			}
			for i, s := range j.Segments {
				if g := (fxJournalSeg{Offset: s.Offset, DeclaredRecords: s.DeclaredRecords, Records: s.Records}); g != e.Segments[i] {
					t.Errorf("segment %d: library %+v, oracle %+v", i, g, e.Segments[i])
				}
			}
			if len(sc.Records) != len(e.Records) {
				t.Fatalf("records: library %d, oracle %d", len(sc.Records), len(e.Records))
			}
			for i, r := range sc.Records {
				g := fxJournalRec{Index: r.Index, Segment: r.Segment, Page: r.Page, Offset: r.Offset, ChecksumOK: r.ChecksumOK, Applied: r.Applied}
				if g != e.Records[i] {
					t.Errorf("record %d: library %+v, oracle %+v", i, g, e.Records[i])
				}
			}
			compareLive(t, f) // Live() is the post-rollback state the engine returns
		})
	}
}

// ---- history ----

type fxDelivered struct {
	r      sqlitefile.RecoveredRow
	values []any
}

var (
	histMu    sync.Mutex
	histCache = map[string][]fxDelivered{}
)

func fixtureHistory(t testing.TB, f *fxLoaded) []fxDelivered {
	t.Helper()
	histMu.Lock()
	defer histMu.Unlock()
	if h, ok := histCache[f.spec.Name]; ok {
		return h
	}
	d := f.lib(t)
	h := d.History()
	defer h.Release()
	var out []fxDelivered
	_, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		vals := make([]any, len(r.Values))
		for i, v := range r.Values {
			vals[i] = normValue(v)
		}
		r.Values = nil
		out = append(out, fxDelivered{r: r, values: vals})
		return true
	})
	if err != nil {
		t.Fatalf("history rows: %v", err)
	}
	histCache[f.spec.Name] = out
	return out
}

func recFileName(k sqlitefile.FileKind) string { return k.String() }

func ridEq(a *int64, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func hasNoteS(notes []string, n string) bool {
	for _, x := range notes {
		if x == n {
			return true
		}
	}
	return false
}

// matchHistory checks one oracle entry against what the library delivered and
// returns the delivered row.
func matchHistory(t *testing.T, f *fxLoaded, e fxHistory, got []fxDelivered) *fxDelivered {
	t.Helper()
	vals := fxDecAll(t, e.Values)
	var near []string
	for i := range got {
		g := &got[i]
		if g.r.Table != e.Table || !ridEq(g.r.Rowid, e.Rowid) || !sameAll(g.values, vals) || g.r.Method != e.Method {
			continue
		}
		l := g.r.Loc
		if recFileName(l.File) != e.Cell.File || l.Page != e.Cell.Page || l.Offset != e.Cell.Offset {
			near = append(near, fmt.Sprintf("%s page %d offset %d", recFileName(l.File), l.Page, l.Offset))
			continue
		}
		if string(g.r.Relation) != e.Relation {
			t.Errorf("%s %s rowid %v: relation %q, oracle %q", f.spec.Name, e.Method, ptr(e.Rowid), g.r.Relation, e.Relation)
		}
		if g.r.Origin.String() != e.Origin {
			t.Errorf("%s %s rowid %v: origin %q, oracle %q", f.spec.Name, e.Method, ptr(e.Rowid), g.r.Origin, e.Origin)
		}
		if string(g.r.TableBasis) != e.Basis {
			t.Errorf("%s %s rowid %v: table basis %q, oracle %q", f.spec.Name, e.Method, ptr(e.Rowid), g.r.TableBasis, e.Basis)
		}
		if c := sqlitefile.Confidence(g.r); c != e.Confidence {
			t.Errorf("%s %s rowid %v: confidence %d, oracle %d", f.spec.Name, e.Method, ptr(e.Rowid), c, e.Confidence)
		}
		for _, n := range e.Notes {
			if !hasNoteS(g.r.Notes, n) {
				t.Errorf("%s %s rowid %v: notes %v lack %q", f.spec.Name, e.Method, ptr(e.Rowid), g.r.Notes, n)
			}
		}
		if g.r.Loc.Length != e.Cell.Length {
			t.Errorf("%s %s rowid %v: cell length %d, oracle %d", f.spec.Name, e.Method, ptr(e.Rowid), g.r.Loc.Length, e.Cell.Length)
		}
		return g
	}
	t.Errorf("%s: oracle history entry not delivered: %s rowid %v table %q values %v at %s page %d offset %d (same values elsewhere: %v)",
		f.spec.Name, e.Method, ptr(e.Rowid), e.Table, vals, e.Cell.File, e.Cell.Page, e.Cell.Offset, near)
	return nil
}

func ptr(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

func TestFixturesHistoryMatchesOracle(t *testing.T) {
	eachFixture(t, func(t *testing.T, f *fxLoaded) {
		got := fixtureHistory(t, f)
		claimed := map[*fxDelivered]bool{}
		for _, e := range f.exp.History {
			if g := matchHistory(t, f, e, got); g != nil {
				if claimed[g] {
					t.Errorf("%s: two oracle entries match one delivered row (%s rowid %v)", f.spec.Name, e.Method, ptr(e.Rowid))
				}
				claimed[g] = true
			}
		}
		// The other direction (review I-4). The oracle lists the versions the
		// generator wrote; the library also delivers every other cell of the
		// history images (the freelist pages and old page versions hold many).
		// Each such extra row is held to account against the BYTES, by the
		// walker's own decoder: the cell it points at decodes to exactly the
		// delivered values and rowid, and
		// a label that is not structural claims no relation to the live data.
		fs := fxNewState(fxFiles{db: f.db, wal: f.wal, journal: f.journal}, true, true)
		extra := 0
		for i := range got {
			g := &got[i]
			if claimed[g] {
				continue
			}
			extra++
			f.checkExtraRow(t, fs, g)
		}
		t.Logf("%s: %d oracle entries, %d further delivered rows verified against the bytes", f.spec.Name, len(f.exp.History), extra)
		for _, u := range f.exp.Unreachable {
			vals := fxDecAll(t, u.Values)
			for _, g := range got {
				if u.Marker != "" {
					for _, v := range g.values {
						if s, ok := v.(string); ok && strings.Contains(s, u.Marker) {
							t.Errorf("%s: a delivered row holds the deleted marker %q", f.spec.Name, u.Marker)
						}
						if b, ok := v.([]byte); ok && bytes.Contains(b, []byte(u.Marker)) {
							t.Errorf("%s: a delivered row holds the deleted marker %q", f.spec.Name, u.Marker)
						}
					}
				}
				if u.Table != "" && g.r.Table == u.Table && ridEq(g.r.Rowid, u.Rowid) && sameAll(g.values, vals) {
					t.Errorf("%s: the library delivers %s rowid %v, which the oracle lists as unreachable", f.spec.Name, u.Table, ptr(u.Rowid))
				}
			}
		}
		if f.spec.Name == "secure-delete" && len(got) != 0 {
			t.Errorf("secure-delete: %d rows delivered, want none", len(got))
		}
	})
}

// TestFixturesLocationsReproduceBytes: the cell of every live row and every
// recovered row lies where the oracle (its own walker, not the library) found
// it, and the bytes at that place equal the oracle's cell.
func TestFixturesLocationsReproduceBytes(t *testing.T) {
	eachFixture(t, func(t *testing.T, f *fxLoaded) {
		ctx := context.Background()
		d := f.lib(t)
		v := d.Live()
		defer v.Release()
		check := func(what string, l sqlitefile.Loc, want fxLoc) {
			t.Helper()
			if recFileName(l.File) != want.File || l.Page != want.Page || l.Offset != want.Offset || l.Length != want.Length {
				t.Errorf("%s: library says %s page %d offset %d length %d, oracle %s page %d offset %d length %d",
					what, recFileName(l.File), l.Page, l.Offset, l.Length, want.File, want.Page, want.Offset, want.Length)
				return
			}
			b := f.fileBytes(want.File)
			if l.Offset < 0 || l.Offset+l.Length > int64(len(b)) {
				t.Errorf("%s: location is outside the file", what)
				return
			}
			if hex.EncodeToString(b[l.Offset:l.Offset+l.Length]) != want.CellHex {
				t.Errorf("%s: the bytes at the location are not the oracle's cell", what)
			}
		}
		total := 0
		for name, cells := range f.exp.Cells {
			tb, err := v.Table(ctx, name)
			if err != nil {
				t.Fatalf("Table %s: %v", name, err)
			}
			i := 0
			err = tb.Rows(ctx, func(r sqlitefile.Row) bool {
				if i >= len(cells) {
					t.Errorf("%s: more rows than oracle cells", name)
					return false
				}
				if r.HasRowid && r.Rowid != cells[i].Rowid {
					t.Errorf("%s row %d: rowid %d, oracle %d", name, i, r.Rowid, cells[i].Rowid)
				}
				check(fmt.Sprintf("%s row %d", name, i), r.Loc, cells[i].fxLoc)
				i++
				total++
				return true
			})
			if err != nil {
				t.Fatalf("Rows %s: %v", name, err)
			}
			if i != len(cells) {
				t.Errorf("%s: library %d rows, oracle %d cells", name, i, len(cells))
			}
		}
		if len(f.exp.Cells) == 0 || total == 0 {
			t.Errorf("the oracle holds no live cell to check")
		}
		got := fixtureHistory(t, f)
		for _, e := range f.exp.History {
			g := matchHistory(t, f, e, got)
			if g != nil {
				check(fmt.Sprintf("recovered %s rowid %v", e.Method, ptr(e.Rowid)), g.r.Loc, e.Cell)
			}
		}
	})
}

// TestFixturesProveConfidenceTiers: across the fixtures the oracle labels a
// row for every table basis and for the two methods the rules single out, and
// the library agrees with each label and with its confidence number.
func TestFixturesProveConfidenceTiers(t *testing.T) {
	bases := map[string]bool{}
	methods := map[string]bool{}
	var priorUnderFrame, fitNote bool
	for _, spec := range fixtureSpecs {
		if spec.NotSQLite || spec.Class == "C" {
			continue
		}
		f := loadFx(t, spec)
		got := fixtureHistory(t, f)
		for _, e := range f.exp.History {
			if g := matchHistory(t, f, e, got); g == nil {
				continue
			}
			bases[e.Basis] = true
			methods[e.Method] = true
			if e.Method == sqlitefile.MethodWALPrior && e.Origin == "db-under-wal" && e.Cell.File == "db" {
				priorUnderFrame = true
			}
			if (e.Basis == "fit" || e.Basis == "guess") && hasNoteS(e.Notes, sqlitefile.NoteIdentityByFitOnly) && e.Relation == "unknown" {
				fitNote = true
			}
		}
	}
	for _, b := range []string{"schema", "fit", "guess", "none"} {
		if !bases[b] {
			t.Errorf("no fixture row is labelled with table basis %q", b)
		}
	}
	for _, m := range []string{sqlitefile.MethodJournalRolledBack, sqlitefile.MethodWALPrior} {
		if !methods[m] {
			t.Errorf("no fixture row is recovered by %s", m)
		}
	}
	if !priorUnderFrame {
		t.Errorf("no sqlite-wal-prior row comes from a database page under a committed frame")
	}
	if !fitNote {
		t.Errorf("no fit-labelled row carries the identity-by-fit-only note with relation unknown")
	}
}

// ---- encrypted-like, determinism, names ----

func TestFixtureEncryptedLikeIsRejected(t *testing.T) {
	var spec fixtureSpec
	for _, s := range fixtureSpecs {
		if s.NotSQLite {
			spec = s
		}
	}
	f := loadFx(t, spec)
	if !f.exp.NotSQLite {
		t.Fatal("the oracle does not say the file is not SQLite")
	}
	_, err := sqlitefile.Open(bytes.NewReader(f.db), int64(len(f.db)), sqlitefile.Options{})
	if !errors.Is(err, sqlitefile.ErrNotSQLite) || !errors.Is(err, sqlitefile.ErrLooksEncrypted) {
		t.Fatalf("Open: %v, want ErrNotSQLite and ErrLooksEncrypted", err)
	}
}

func fxDump(t testing.TB, f *fxLoaded) string {
	t.Helper()
	d := f.lib(t)
	v := d.Live()
	defer v.Release()
	var sb strings.Builder
	rows := liveDumpRows(t, v)
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, r := range rows[n] {
			fmt.Fprintf(&sb, "%s %d %v\n", n, r.Rowid, r.Vals)
		}
	}
	for _, g := range fixtureHistory2(t, d) {
		fmt.Fprintf(&sb, "H %s %s %v %v %s %v %v\n", g.r.Method, g.r.Table, g.r.Rowid != nil, g.values, g.r.Relation, g.r.Loc.Offset, g.r.Notes)
	}
	return sb.String()
}

func fixtureHistory2(t testing.TB, d *sqlitefile.DB) []fxDelivered {
	h := d.History()
	defer h.Release()
	var out []fxDelivered
	if _, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		vals := make([]any, len(r.Values))
		for i, v := range r.Values {
			vals[i] = normValue(v)
		}
		r.Values = nil
		out = append(out, fxDelivered{r: r, values: vals})
		return true
	}); err != nil {
		t.Fatalf("history: %v", err)
	}
	return out
}

func TestFixturesReadDeterministically(t *testing.T) {
	eachFixture(t, func(t *testing.T, f *fxLoaded) {
		if a, b := fxDump(t, f), fxDump(t, f); a != b {
			t.Errorf("two passes over the same bytes differ")
		}
	})
}

// denyHashes holds the SHA-256 of lower-case words (and of two-word phrases)
// no fixture text may contain; the words themselves are not in this file.
var denyHashes = map[string]bool{
	"83d152812bf09572a346dd563dd0d4611ed3256980e72cd7a2a3d64ef436733e": true,
	"b05c7c0319acc0a0b9feb3945e2022c41e844e94343a34a3bff85b78422ee9c7": true,
	"0acbf84757d057b4eaf593501359e26e0504de13993d6d08f8a383e97bdec5cd": true,
	"6ca9cec140ea6dd1af27c2391035c394c4b3b5679c5022e1dd2652a16f40931a": true,
	"3e44c5465980684f9f36de7095f2a271b35047769664dc7b4f75be958c990f09": true,
	"bc5da1ceccf97a8cf870cdb72514028ca67972b3af489d76acaa2765f6d58c3b": true,
	"e142a2a882272c402365930c7956f634ca1afcf1b3c92dbb94fcfff86e8e0d32": true,
	"83344e3de38ac7bca84715e165d6e1851e0dc67a687c6e40e8ed06bd69b1f126": true,
	"36bd68025c88cec311238b1658f5f8091fb499587f8b50c4bedef5338f4abd1b": true,
	"85072c19628b06af61dd8a6ab662365afb9f25359d63073fc9dc0ed805388d9f": true,
	"3b8e886d1720d0abaea4943f970acc62855a57f07701e79f2950f56e540bc92a": true,
	"7916eb71b5f9f2ae8b762141586485146687697b1a03bc04e537d58fdccdc1a5": true,
	"91af28fd4106b38f198f18fe26ec2a45e56eb06279ea1d0c9e76ef15e6ae9bd8": true,
	"c128771337c2121f070539be9409060fc6b863235bd29544f23c624149c347f3": true,
	"cbfee83e77fa73a57736752a42d33154a8b3a8b782e7b58c1e011ee7cc35747e": true,
	"2ff8e008a43ccc94ff9947027be66ff7d4f207087176c4d25398d6e049dad5a8": true,
	"885e37788239ae954a458d23bb2dc0da35a33f71fccc05812af015e72230ac8c": true,
	"d3fcc484c69fdda49a4d288389d7dd7ce35dc780891d82f78bacb165d7cbbd38": true,
	"67c919f4ce6ebc00f52d58b668c794cf351d39c8166fd62780acb4ae922f2a95": true,
	"2e7cb40cf9b5efcf73edb997711ddf7488809e6b07dd87d0d8604c01a2fb929c": true,
	"990517ac300720528f52ff4de6ddff994829e194a2af88b4f2374c0b8932d2ac": true,
}

func denyHit(s string) string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for i, w := range fields {
		if denyHashes[sha256hex([]byte(w))] {
			return w
		}
		if i+1 < len(fields) && denyHashes[sha256hex([]byte(w+" "+fields[i+1]))] {
			return w + " " + fields[i+1]
		}
	}
	return ""
}

func TestFixturesCarryNoVendorNames(t *testing.T) {
	if len(denyHashes) < 10 {
		t.Fatal("the deny list is not loaded")
	}
	for _, spec := range fixtureSpecs {
		raw, err := os.ReadFile(filepath.Join("testdata", spec.Name+".expect.json"))
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		if w := denyHit(string(raw)); w != "" {
			t.Errorf("%s: the oracle contains a denied word", spec.Name)
		}
		if spec.NotSQLite {
			continue
		}
		f := loadFx(t, spec)
		for _, s := range f.exp.Generator.Statements {
			if denyHit(s) != "" {
				t.Errorf("%s: a statement contains a denied word", spec.Name)
			}
		}
		if denyHit(string(f.db)) != "" {
			t.Errorf("%s: the database bytes contain a denied word", spec.Name)
		}
	}
}

// checkExtraRow verifies a delivered history row the oracle does not list.
func (f *fxLoaded) checkExtraRow(t *testing.T, fs *fxState, g *fxDelivered) {
	t.Helper()
	l := g.r.Loc
	b := f.fileBytes(recFileName(l.File))
	where := fmt.Sprintf("%s: extra row %s %s page %d offset %d", f.spec.Name, g.r.Method, recFileName(l.File), l.Page, l.Offset)
	if l.PageOffset < 0 || l.PageOffset+int64(fs.ps) > int64(len(b)) || l.Offset < l.PageOffset {
		t.Errorf("%s: the location is outside its file", where)
		return
	}
	page := b[l.PageOffset : l.PageOffset+int64(fs.ps)]
	typ := page[fxHeaderBase(l.Page)]
	c, err := fs.cellAt(page, l.Page, recFileName(l.File), l.PageOffset, typ, int(l.Offset-l.PageOffset))
	if err != nil {
		t.Errorf("%s: the cell does not decode: %v", where, err)
		return
	}
	if len(l.Overflow) == 0 && (!sameAll(c.vals, g.values) || c.length != int(l.Length)) {
		t.Errorf("%s: the bytes decode to %v (length %d), the library delivered %v (length %d)", where, c.vals, c.length, g.values, l.Length)
	}
	if c.hasRowid != (g.r.Rowid != nil) || (c.hasRowid && c.rowid != *g.r.Rowid) {
		t.Errorf("%s: rowid %v, the cell holds %d", where, ptr(g.r.Rowid), c.rowid)
	}
	switch g.r.TableBasis {
	case sqlitefile.BasisFit, sqlitefile.BasisGuess, sqlitefile.BasisNone:
		if g.r.Relation != sqlitefile.RelUnknown {
			t.Errorf("%s: basis %s with relation %s, want unknown", where, g.r.TableBasis, g.r.Relation)
		}
	}
}
