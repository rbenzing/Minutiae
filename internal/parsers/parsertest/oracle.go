package parsertest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// Generator is the provenance block of an expect.json. Files holds the SHA-256 of
// every decompressed file by role ("db", "wal", "journal").
type Generator struct {
	Tool   string            `json:"tool"`
	Python string            `json:"python"`
	SQLite string            `json:"sqlite"`
	Files  map[string]string `json:"files"`
}

// ExtraFile is a file the scenario puts next to the database in the case (an
// attachment the parser can find through Lookup).
type ExtraFile struct {
	RemotePath   string `json:"remote_path"`
	ContentB64   string `json:"content_b64"`
	Device       string `json:"device"`        // the device it was acquired from; "" and "SER1" are the database's own device
	Domain       string `json:"domain"`        // iOS backup extra: the backup domain (BundleIOSBackup)
	RelativePath string `json:"relative_path"` // iOS backup extra: the path within the domain
	Role         string `json:"role"`          // "": a plain extra (Others["extraN"]); else the bundle role it fills, for example "images"
}

// ExpRecord is one expected record, stated literally by the generator. File is
// "db", "wal", "journal" or "any" (the cell may lie in any of the files).
type ExpRecord struct {
	Table      string            `json:"table"`
	Rowid      int64             `json:"rowid"`
	File       string            `json:"file"`
	Type       string            `json:"type"`        // the record type, compared literally
	SourcePath string            `json:"source_path"` // the record's SourcePath; "" on a file "any" row: derived from the role its locator reports (T48)
	Time       *string           `json:"time"`
	TimeEnd    *string           `json:"time_end"`
	Times      map[string]string `json:"times"`
	Deleted    bool              `json:"deleted"`
	Summary    *string           `json:"summary"` // nil: not compared
	Body       *string           `json:"body"`    // nil: no body
	Payload    map[string]any    `json:"payload"`
}

// ExpWarning is a warning the parser must raise for a row: the locator names the
// row and the reason contains the text. An empty Table is a warning of the SQLite
// reader (locator sqlite:file=...) instead of a row's.
type ExpWarning struct {
	Table    string `json:"table"`
	Rowid    int64  `json:"rowid"`
	Contains string `json:"contains"`
}

// Expect is an <variant>.expect.json.
type Expect struct {
	Generator  Generator         `json:"generator"`
	Family     string            `json:"family"`
	Variant    string            `json:"variant"`
	DDL        string            `json:"ddl"`
	Notes      map[string]string `json:"notes"`
	Records    []ExpRecord       `json:"records"`
	Warnings   []ExpWarning      `json:"warnings"`
	ExtraFiles []ExtraFile       `json:"extra_files"`
}

// Fixture is one committed SQLite fixture: its decompressed files and its oracle.
type Fixture struct {
	Name   string
	Files  map[string][]byte
	Expect Expect
}

const maxFixtureFile = 64 << 20

var fixtureRoles = []struct{ role, suffix string }{{"db", ".db.gz"}, {"wal", ".db-wal.gz"}, {"journal", ".db-journal.gz"}, {"images", ".images.db.gz"}, {"images_wal", ".images.db-wal.gz"}}

// LoadFixture reads <dir>/<name>.expect.json and the compressed files it lists.
// The SHA-256 of every decompressed file must equal generator.files[role] before
// anything else is looked at: a stale oracle or a regenerated image alone fails.
func LoadFixture(t testing.TB, dir, name string) Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
		return Fixture{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	f := Fixture{Name: name, Files: map[string][]byte{}}
	dec.DisallowUnknownFields() // a misspelt key must not silently disable the check it states
	if err := dec.Decode(&f.Expect); err != nil {
		t.Fatalf("%s: %v", name, err)
		return Fixture{}
	}
	for role := range f.Expect.Generator.Files {
		known := false
		for _, fr := range fixtureRoles {
			known = known || fr.role == role
		}
		if !known {
			t.Fatalf("%s: generator.files lists the unknown role %q (known: db, wal, journal, images, images_wal): its file would never be hashed", name, role)
			return Fixture{}
		}
	}
	for _, fr := range fixtureRoles {
		want, listed := f.Expect.Generator.Files[fr.role]
		if !listed {
			continue
		}
		gz, err := os.ReadFile(filepath.Join(dir, name+fr.suffix))
		if err != nil {
			t.Fatal(err)
			return Fixture{}
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatalf("%s %s: %v", name, fr.role, err)
			return Fixture{}
		}
		data, err := io.ReadAll(io.LimitReader(zr, maxFixtureFile+1))
		if err != nil || len(data) > maxFixtureFile {
			t.Fatalf("%s %s: unreadable or over %d bytes: %v", name, fr.role, maxFixtureFile, err)
			return Fixture{}
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s %s: sha256 %s does not match the oracle's %s: regenerate the image, the oracle and the pin together", name, fr.role, got, want)
			return Fixture{}
		}
		f.Files[fr.role] = data
	}
	if _, ok := f.Files["db"]; !ok {
		t.Fatalf("%s: the oracle lists no db file", name)
	}
	return f
}

// addAndroidFileFor is AddAndroidFile for a file acquired from the device named device.
func addAndroidFileFor(t testing.TB, c *evidence.Case, device, acq, remotePath string, data []byte) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: "file", DeviceID: device, RemotePath: remotePath}
	rec, err := c.Capture(device, acq, strings.TrimPrefix(remotePath, "/"), src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// Bundle is a fixture added to a harness case: the artifacts by role and the extra
// files by remote path.
type Bundle struct {
	Primary parse.Artifact
	Others  map[string]parse.Artifact // for Harness.Input: wal, journal and extraN
	ByRole  map[string]parse.Artifact // db, wal, journal
	ByPath  map[string]parse.Artifact // remote path (iOS backup: logical path ios:<domain>/<relative_path>) -> artifact, for the extra files
}

// ErrEmptyRemotePath is returned by BuildBundle when the database path or an extra file's path is empty: an artifact
// without a remote path would let a SourcePath check pass vacuously (ruling T52).
var ErrEmptyRemotePath = errors.New("parsertest: an artifact has an empty remote path")

// Bundle stores the fixture's files in h's case as an Android file acquisition: the database at remotePath,
// the WAL and journal beside it with their suffixes, the extra files at their own paths. It fails the test on an error.
func (f Fixture) Bundle(h *Harness, remotePath string) Bundle {
	h.T.Helper()
	b, err := f.BuildBundle(h, remotePath)
	if err != nil {
		h.T.Fatal(err)
	}
	return b
}

// BuildBundle is Bundle returning its error; an empty remote path is ErrEmptyRemotePath.
func (f Fixture) BuildBundle(h *Harness, remotePath string) (Bundle, error) {
	h.T.Helper()
	if remotePath == "" {
		return Bundle{}, fmt.Errorf("the database: %w", ErrEmptyRemotePath)
	}
	for i, x := range f.Expect.ExtraFiles {
		if x.RemotePath == "" {
			return Bundle{}, fmt.Errorf("extra file %d: %w", i, ErrEmptyRemotePath)
		}
	}
	b := Bundle{Others: map[string]parse.Artifact{}, ByRole: map[string]parse.Artifact{}, ByPath: map[string]parse.Artifact{}}
	add := func(path string, data []byte) parse.Artifact {
		m := AddAndroidFile(h.T, h.Case, "acq1", path, data)
		return h.Artifact(m, data)
	}
	b.Primary = add(remotePath, f.Files["db"])
	b.ByRole["db"] = b.Primary
	for _, c := range []struct{ role, suffix string }{{"wal", "-wal"}, {"journal", "-journal"}} {
		if data, ok := f.Files[c.role]; ok {
			a := add(remotePath+c.suffix, data)
			b.Others[c.role], b.ByRole[c.role] = a, a
		}
	}
	for i, x := range f.Expect.ExtraFiles {
		data, err := base64.StdEncoding.DecodeString(x.ContentB64)
		if err != nil {
			return b, fmt.Errorf("extra file %s: %w", x.RemotePath, err)
		}
		var a parse.Artifact
		if x.Device == "" || x.Device == "SER1" {
			a = add(x.RemotePath, data)
		} else {
			a = h.Artifact(addAndroidFileFor(h.T, h.Case, x.Device, "acq-"+x.Device, x.RemotePath, data), data)
		}
		if x.Role != "" {
			if _, taken := b.ByRole[x.Role]; taken {
				return b, fmt.Errorf("extra file %s: the role %q is already filled", x.RemotePath, x.Role)
			}
			b.Others[x.Role], b.ByRole[x.Role] = a, a
		} else {
			b.Others["extra"+strconv.Itoa(i)] = a
		}
		b.ByPath[x.RemotePath] = a
	}
	return b, nil
}

// Input is h.Input over the bundle.
func (b Bundle) Input(h *Harness, p parse.Parser, forProbe bool) *parse.Input {
	return h.Input(p, b.Primary, b.Others, forProbe)
}

// splitLocator parses sqlite:table=<t>;rowid=<n>[;...] and returns the part after the rowid.
func splitLocator(loc string) (table string, rowid int64, rest string, ok bool) {
	s, found := strings.CutPrefix(loc, "sqlite:table=")
	if !found {
		return "", 0, "", false
	}
	table, s, found = strings.Cut(s, ";rowid=")
	if !found {
		return "", 0, "", false
	}
	num, rest, _ := strings.Cut(s, ";")
	id, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	if rest != "" {
		rest = ";" + rest
	}
	return table, id, rest, true
}

func roleOf(rest string) string {
	switch {
	case strings.Contains(rest, ";wal_frame="):
		return "wal"
	case strings.Contains(rest, ";journal_rec="):
		return "journal"
	}
	return "db"
}

func wanted(tables []string, table string) bool {
	if len(tables) == 0 {
		return true
	}
	for _, t := range tables {
		if t == table {
			return true
		}
	}
	return false
}

// AssertMatchesOracle requires the run to equal the oracle: no record missing or
// extra, in the oracle's order. tables, when given, restricts the comparison to
// records and warnings of those tables (a parser built in steps).
func AssertMatchesOracle(t testing.TB, res Result, b Bundle, exp Expect, tables ...string) {
	t.Helper()
	if res.Err != nil {
		t.Errorf("the parser returned %v", res.Err)
	}
	for _, v := range res.Violations {
		t.Errorf("contract violation: %v", v)
	}
	var want []ExpRecord
	for _, w := range exp.Records {
		if wanted(tables, w.Table) {
			want = append(want, w)
		}
	}
	var got []records.Record
	for _, r := range res.Records {
		if tb, _, _, ok := splitLocator(r.Locator); !ok || wanted(tables, tb) {
			got = append(got, r)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d records, the oracle expects %d", len(got), len(want))
	}
	for i := 0; i < min(len(got), len(want)); i++ {
		compareRecord(t, b, got[i], want[i])
	}
	for i := len(got); i < len(want) && i < len(got)+5; i++ {
		t.Errorf("missing record: %s rowid %d", want[i].Table, want[i].Rowid)
	}
	for i := len(want); i < len(got) && i < len(want)+5; i++ {
		t.Errorf("extra record: %s", got[i].Locator)
	}
	compareWarnings(t, res, exp, tables)
	for k, v := range exp.Notes {
		if !wanted(tables, noteTable(k)) {
			continue
		}
		if res.Notes[k] != v {
			t.Errorf("note %s = %q, the oracle expects %q", k, res.Notes[k], v)
		}
	}
}

// noteTable returns the table a note key names ("schema.sms" -> "sms"), "" otherwise.
func noteTable(key string) string {
	_, tb, ok := strings.Cut(key, ".")
	if !ok {
		return ""
	}
	return tb
}

func compareWarnings(t testing.TB, res Result, exp Expect, tables []string) {
	t.Helper()
	var want []ExpWarning
	for _, w := range exp.Warnings {
		if w.Table == "" || wanted(tables, w.Table) { // a reader warning belongs to no table: it is always compared, on both sides
			want = append(want, w)
		}
	}
	var got []Warning
	for _, w := range res.Warnings {
		if tb, _, _, ok := splitLocator(w.Locator); !ok || wanted(tables, tb) {
			got = append(got, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d warnings %v, the oracle expects %d", len(got), got, len(want))
		return
	}
	for i, w := range want {
		if w.Table == "" { // a warning of the SQLite reader, not of a row
			if !strings.HasPrefix(got[i].Locator, "sqlite:file=") || !strings.Contains(got[i].Reason, w.Contains) {
				t.Errorf("warning %d = %+v, the oracle expects a reader warning containing %q", i, got[i], w.Contains)
			}
			continue
		}
		tb, id, _, ok := splitLocator(got[i].Locator)
		if !ok || tb != w.Table || id != w.Rowid || !strings.Contains(got[i].Reason, w.Contains) {
			t.Errorf("warning %d = %+v, the oracle expects %s rowid %d containing %q", i, got[i], w.Table, w.Rowid, w.Contains)
		}
	}
}

func compareRecord(t testing.TB, b Bundle, got records.Record, w ExpRecord) {
	t.Helper()
	id := fmt.Sprintf("%s rowid %d", w.Table, w.Rowid)
	tb, rowid, rest, ok := splitLocator(got.Locator)
	if !ok || tb != w.Table || rowid != w.Rowid {
		t.Errorf("%s: locator %q", id, got.Locator)
		return
	}
	role := roleOf(rest)
	if w.File != "any" && w.File != role {
		t.Errorf("%s: the cell lies in the %s file, the oracle expects %s", id, role, w.File)
	}
	art, found := b.ByRole[role]
	if !found || got.ArtifactID != art.ID {
		t.Errorf("%s: artifact %q, want the %s artifact %q", id, got.ArtifactID, role, art.ID)
	}
	if got.Range == nil || got.Range.Offset < 0 || got.Range.Length <= 0 || got.Range.Length > art.Size-got.Range.Offset {
		t.Errorf("%s: range %+v is not inside the %s artifact (%d bytes)", id, got.Range, role, art.Size)
	}
	if got.Type != w.Type {
		t.Errorf("%s: Type %q, want %q", id, got.Type, w.Type)
	}
	// ruling T48: the path of the file that holds ArtifactID's bytes (db, db-wal or db-journal), whatever the oracle says
	if found && got.SourcePath != art.Source.RemotePath {
		t.Errorf("%s: SourcePath %q is not the remote path %q of the %s artifact that holds the cell", id, got.SourcePath, art.Source.RemotePath, role)
	}
	wantPath := w.SourcePath
	if w.File == "any" && wantPath == "" { // the cell may lie in any file: the path follows the role the locator reports
		wantPath = art.Source.RemotePath
	}
	if got.SourcePath != wantPath {
		t.Errorf("%s: SourcePath %q, want %q", id, got.SourcePath, wantPath)
	}
	if got.Deleted != w.Deleted {
		t.Errorf("%s: Deleted = %v, want %v", id, got.Deleted, w.Deleted)
	}
	if got.Recovery != "" || got.Confidence != nil {
		t.Errorf("%s: a live parse set Recovery %q / Confidence %v", id, got.Recovery, got.Confidence)
	}
	compareTime(t, id+" time", got.Time, w.Time)
	compareTime(t, id+" time_end", got.TimeEnd, w.TimeEnd)
	gotTimes := map[string]time.Time{}
	for _, nt := range got.Times {
		if nt.Time.Basis != "" && nt.Time.Basis != records.BasisUTC {
			t.Errorf("%s: time %s has basis %q", id, nt.Kind, nt.Time.Basis)
		}
		gotTimes[nt.Kind] = nt.Time.T
	}
	if len(got.Times) != len(w.Times) {
		t.Errorf("%s: %d Times %v, want %d %v", id, len(got.Times), gotTimes, len(w.Times), w.Times)
	}
	for k, s := range w.Times {
		wt, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Errorf("%s: oracle time %s: %v", id, k, err)
		} else if g, ok := gotTimes[k]; !ok || !g.Equal(wt) {
			t.Errorf("%s: time %s = %v, want %s", id, k, g, s)
		}
	}
	if w.Summary != nil && got.Summary != *w.Summary {
		t.Errorf("%s: Summary %q, want %q", id, got.Summary, *w.Summary)
	}
	wantBody := ""
	if w.Body != nil {
		wantBody = *w.Body
	}
	if got.Body != wantBody {
		t.Errorf("%s: Body %q, want %q", id, clip(got.Body), clip(wantBody))
	}
	gp, wp := normalize(got.Payload), normalize(subst(w.Payload, b))
	if !reflect.DeepEqual(gp, wp) {
		gj, _ := json.Marshal(gp)
		wj, _ := json.Marshal(wp)
		t.Errorf("%s: payload differs\n got %s\nwant %s", id, clip(string(gj)), clip(string(wj)))
	}
}

func clip(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

func compareTime(t testing.TB, label string, got *records.Time, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s: got %v, want %v", label, got, want)
	default:
		wt, err := time.Parse(time.RFC3339Nano, *want)
		if err != nil || !got.T.Equal(wt) || (got.Basis != "" && got.Basis != records.BasisUTC) {
			t.Errorf("%s: got %v (%q), want %s", label, got.T, got.Basis, *want)
		}
	}
}

// normalize round-trips v through JSON so integers, floats and nested types
// compare the way the stored payload would.
func normalize(v any) any {
	bs, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	dec := json.NewDecoder(bytes.NewReader(bs))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return err.Error()
	}
	return out
}

// subst resolves the placeholders of an expected payload from the bundle's extra files.
func subst(v any, b Bundle) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = subst(e, b)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = subst(e, b)
		}
		return out
	case string:
		for _, p := range []struct {
			prefix string
			f      func(parse.Artifact) any
		}{
			{"@id:", func(a parse.Artifact) any { return a.ID }},
			{"@sha256:", func(a parse.Artifact) any { return a.SHA256 }},
			{"@size:", func(a parse.Artifact) any { return json.Number(strconv.FormatInt(a.Size, 10)) }},
		} {
			if rest, ok := strings.CutPrefix(x, p.prefix); ok {
				if a, found := b.ByPath[rest]; found {
					return p.f(a)
				}
			}
		}
	}
	return v
}
