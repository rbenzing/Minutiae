//go:build realdata

package parsers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the independent engine that counts rows (tests only)

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// The real-data parser tests (4H-R10). They run only with -tags realdata and $MINUTIAE_TEST_DATA (the layout is
// read by realDataSets in realdata_inputs_test.go); without data every test SKIPs with the reason. Running them
// is an OPERATOR step, never a gate (docs/testing.md "Real data"). Nothing here commits, copies out or prints
// personal data: the logs name tables, columns, counts and notes only.

func rowMapperNamed(t *testing.T, name string) parse.RowMapper {
	t.Helper()
	for _, p := range All() {
		if p.Meta().Name == name {
			rm, ok := p.(parse.RowMapper)
			if !ok {
				t.Fatalf("%s is not a RowMapper", name)
			}
			return rm
		}
	}
	t.Fatalf("%s is not a registered parser", name)
	return nil
}

// parserSets is the sets of the parsers (the ios-backup sets belong to the command line tests); it skips t when
// there is none.
func parserSets(t *testing.T) []realDataSet {
	t.Helper()
	var out []realDataSet
	for _, s := range realDataSets(t) {
		if s.Parser != realDataBackupKind {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		t.Skip("the layout holds no parser set")
	}
	return out
}

// realBundle captures the files of a set into h's case through Case.Capture (hashed, in the manifest) as file
// artifacts of an Android or iOS device and returns the bundle, with the size of each artifact by id. The files
// are read into memory: a real database of some hundred MiB is fine for an operator run.
func realBundle(t *testing.T, h *parsertest.Harness, meta parse.Meta, s realDataSet) (parsertest.Bundle, map[string]int64) {
	t.Helper()
	in := realDataInputs[s.Parser]
	roles := map[string]bool{}
	for _, r := range meta.Inputs {
		roles[r.Role] = true
	}
	b := parsertest.Bundle{Others: map[string]parse.Artifact{}, ByRole: map[string]parse.Artifact{}, ByPath: map[string]parse.Artifact{}}
	sizes := map[string]int64{}
	logicalOf := map[string]string{"db": s.Logical, "wal": s.Logical + "-wal", "journal": s.Logical + "-journal"}
	for role, file := range s.Files {
		if !roles[role] {
			t.Fatalf("%s: the set holds a %s file (%s) but the parser has no such input role", s.Dir, role, file)
		}
		if _, ok := logicalOf[role]; !ok {
			logicalOf[role] = path.Dir(s.Logical) + "/" + filepath.Base(file)
		}
	}
	for _, role := range slices.Sorted(maps.Keys(s.Files)) {
		data, err := os.ReadFile(s.Files[role])
		if err != nil {
			t.Fatal(err)
		}
		var a parse.Artifact
		if in.Platform == parse.PlatformAndroid {
			m := parsertest.AddAndroidFile(t, h.Case, "acq1", strings.TrimPrefix(logicalOf[role], "android:"), data)
			a = h.Artifact(m, data)
		} else {
			remote := "/" + strings.TrimPrefix(logicalOf[role], "ios:")
			src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: remote}
			m, err := h.Case.Capture("IOS1", "acq1", strings.TrimPrefix(remote, "/"), src, func(w io.Writer) error {
				_, err := w.Write(data)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			a = h.IOSArtifact(m, data, logicalOf[role])
		}
		sizes[a.ID] = a.Size
		b.ByRole[role] = a
		if role == "db" {
			b.Primary = a
		} else {
			b.Others[role] = a
		}
	}
	return b, sizes
}

func openRealDB(t *testing.T, in *parse.Input) *sqlitedb.DB {
	t.Helper()
	files := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		files.WAL, files.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		files.Journal, files.JournalSize = a.R, a.Size
	}
	db, err := sqlitedb.Open(context.Background(), files, in.Budget)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	return db
}

// locatorParts reads "sqlite:table=<t>;rowid=<n>" with an optional wal_frame or journal_rec; ok is false for any other form.
func locatorParts(loc string) (table string, rowid int64, ok bool) {
	rest, found := strings.CutPrefix(loc, "sqlite:")
	if !found {
		return "", 0, false
	}
	var haveRowid bool
	for _, f := range strings.Split(rest, ";") {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "table":
			u, err := url.PathUnescape(v)
			if err != nil {
				return "", 0, false
			}
			table = u
		case "rowid":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return "", 0, false
			}
			rowid, haveRowid = n, true
		case "wal_frame", "journal_rec": // where the cell sits; the row is still found by its rowid
		default:
			return "", 0, false
		}
	}
	return table, rowid, table != "" && haveRowid
}

// locatorTable is the table named by a locator, whatever else it says.
func locatorTable(loc string) string {
	rest, found := strings.CutPrefix(loc, "sqlite:")
	if !found {
		return ""
	}
	for _, f := range strings.Split(rest, ";") {
		if v, ok := strings.CutPrefix(f, "table="); ok {
			if u, err := url.PathUnescape(v); err == nil {
				return u
			}
			return v
		}
	}
	return ""
}

// engineCounts copies the set's files into dir and counts the rows of each table with the independent engine.
func engineCounts(t *testing.T, s realDataSet, dir string, tables []string) map[string]int64 {
	t.Helper()
	copyFile := func(src, dst string) {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(dir, "copy.db")
	copyFile(s.Files["db"], dst)
	if p, ok := s.Files["wal"]; ok {
		copyFile(p, dst+"-wal")
	}
	if p, ok := s.Files["journal"]; ok {
		copyFile(p, dst+"-journal")
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	out := map[string]int64{}
	for _, tb := range tables {
		var n int64
		q := `SELECT count(*) FROM "` + strings.ReplaceAll(tb, `"`, `""`) + `"`
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Errorf("the independent engine cannot count %s: %v", tb, err)
			continue
		}
		out[tb] = n
	}
	return out
}

func TestRealDataParsers(t *testing.T) {
	for _, s := range parserSets(t) {
		t.Run(s.Parser+"/"+s.Name, func(t *testing.T) {
			p := rowMapperNamed(t, s.Parser)
			meta := p.Meta()
			h := parsertest.NewHarness(t)
			b, sizes := realBundle(t, h, meta, s)
			newInput := func() *parse.Input { return b.Input(h, p, false) }

			// Probe: the from-memory schema check of the iOS parsers and Safari (4F-R12, 4E-R13) against a real database.
			app, err := p.Probe(context.Background(), b.Input(h, p, true))
			if err != nil {
				t.Fatalf("Probe failed: %v", err)
			}
			if app.Status != parse.Applicable {
				t.Fatalf("Probe says %q (%s): the parser does not recognise a real %s database; this is a finding", app.Status, app.Reason, s.Parser)
			}

			// A lenient run (it collects the row warnings that the counts below need): no rejection, no error, no broken
			// rule. Strict runs are the unit tests job.
			res := h.RunLenient(p, newInput())
			if res.Err != nil || res.Refused != 0 || len(res.Violations) != 0 {
				t.Errorf("the run: err %v, %d refused, violations %v", res.Err, res.Refused, res.Violations)
			}
			res.AssertNoLateCalls(t)
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, newInput()) })

			// Every Range lies inside its artifact; every locator re-resolves to the same row.
			db := openRealDB(t, newInput())
			defer db.Release()
			tables := map[string]*sqlitedb.Table{}
			resolved, unresolvable := 0, 0
			perTableRecords := map[string]int64{}
			for _, r := range res.Records {
				size, known := sizes[r.ArtifactID]
				if !known {
					t.Errorf("a %s record names artifact %q, which is not in the bundle", r.Type, r.ArtifactID)
					continue
				}
				if rg := r.Range; rg != nil && (rg.Offset < 0 || rg.Length < 0 || rg.Offset > size || rg.Length > size-rg.Offset) {
					t.Errorf("a %s record at %s has range %+v outside its %d-byte artifact", r.Type, r.Locator, *rg, size)
				}
				perTableRecords[strings.ToLower(locatorTable(r.Locator))]++
				tb, rowid, ok := locatorParts(r.Locator)
				if !ok {
					unresolvable++
					continue
				}
				key := strings.ToLower(tb)
				tbl := tables[key]
				if tbl == nil {
					if tbl, err = db.Table(context.Background(), tb, nil, nil); err != nil {
						t.Errorf("locator %s: table %s: %v", r.Locator, tb, err)
						continue
					}
					tables[key] = tbl
				}
				row, found, err := tbl.Get(context.Background(), rowid)
				if err != nil || !found {
					t.Errorf("locator %s does not re-resolve to a row (found %v, err %v)", r.Locator, found, err)
					continue
				}
				if got, has := row.Rowid(); !has || got != rowid {
					t.Errorf("locator %s resolves to rowid %d (has %v)", r.Locator, got, has)
				}
				row.Release()
				resolved++
			}
			t.Logf("%d records, %d warnings; %d locators re-resolved, %d of another form not re-resolved", len(res.Records), len(res.Warnings), resolved, unresolvable)

			// For each claimed table, records plus counted row warnings equal the independent engine's count.
			perTableWarnings := map[string]int64{}
			for _, w := range res.Warnings {
				perTableWarnings[strings.ToLower(locatorTable(w.Locator))]++
			}
			var claimed []string
			for _, c := range meta.Claims {
				if _, err := db.Table(context.Background(), c.Table, nil, nil); err != nil {
					t.Logf("claimed table %s is absent from this database (%v): not counted", c.Table, err) // ios.calls claims both layouts
					continue
				}
				claimed = append(claimed, c.Table)
			}
			counts := engineCounts(t, s, t.TempDir(), claimed)
			for _, tb := range claimed {
				k := strings.ToLower(tb)
				got := perTableRecords[k] + perTableWarnings[k]
				if want, ok := counts[tb]; ok && got != want {
					t.Errorf("table %s: %d records + %d row warnings != %d rows counted by the independent engine", tb, perTableRecords[k], perTableWarnings[k], want)
				} else if ok {
					t.Logf("table %s: %d rows = %d records + %d row warnings", tb, want, perTableRecords[k], perTableWarnings[k])
				}
			}

			// The schema report: what the real database has that the mapping does not read, and the reverse.
			for _, m := range p.Mappings() {
				tbl, err := db.Table(context.Background(), m.Table, nil, nil)
				if err != nil {
					t.Logf("schema %s: the real database has no such table (%v)", m.Table, err)
					continue
				}
				var have, unmapped, missing []string
				for _, c := range tbl.Cols() {
					have = append(have, strings.ToLower(c.Name))
				}
				specs := map[string]bool{}
				for _, c := range m.Columns {
					specs[strings.ToLower(c.Name)] = true
					if !slices.Contains(have, strings.ToLower(c.Name)) {
						missing = append(missing, c.Name)
					}
				}
				for _, c := range have {
					if !specs[c] {
						unmapped = append(unmapped, c)
					}
				}
				t.Logf("schema %s: %d columns; real columns not in the mapping %v; mapped columns absent from the database %v", m.Table, len(have), unmapped, missing)
			}
			var keys []string
			for k := range res.Notes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				t.Logf("note %s = %s", k, res.Notes[k])
			}
			if v, ok := res.Notes["attributed_body_differs"]; ok {
				t.Logf("ios.sms attributed_body_differs = %s (4E-R14)", v)
			}
		})
	}
}

// recoveryRecorder is the RecoveryEmitter of the typed pass over a real database: it counts and checks, it keeps
// no row.
type recoveryRecorder struct {
	t               *testing.T
	typed, unmapped int64
	notes           map[string]string
}

func (e *recoveryRecorder) Emit(context.Context, records.Record) error {
	e.t.Errorf("the typed recovery pass called Emit: a recovered row must never become a live record")
	return errors.New("live Emit refused in a recovery run")
}

func (e *recoveryRecorder) EmitRecovered(_ context.Context, r records.Record, f parse.RecoveredFacts) error {
	e.typed++
	if err := f.Validate(); err != nil {
		e.t.Errorf("the facts of a recovered row are invalid: %v", err)
	}
	if r.Recovery != "" || r.Confidence != nil {
		e.t.Errorf("the mapping of %s set Recovery/Confidence: only the host stamps them", f.Locator)
	}
	return nil
}

func (e *recoveryRecorder) Unmapped(context.Context, parse.RecoveredFacts, string) error {
	e.unmapped++
	return nil
}

func (e *recoveryRecorder) Warn(context.Context, string, string) error { return nil }
func (e *recoveryRecorder) Note(k, v string)                           { e.notes[k] = v }
func (e *recoveryRecorder) Progress(int64, int64)                      {}

// The typed pass over a real database: no panic, no error, every recovered row either mapped or reported
// unmapped, and typed plus unmapped plus skipped equals the claimed history rows sqlitefile.ClaimMatch accepts.
// (The host stamps Deleted, the method and the capped confidence; TestRealDataEndToEnd in internal/cli reads
// them back from the case.)
func TestRealDataTypedRecovery(t *testing.T) {
	for _, s := range parserSets(t) {
		t.Run(s.Parser+"/"+s.Name, func(t *testing.T) {
			p := rowMapperNamed(t, s.Parser)
			meta := p.Meta()
			h := parsertest.NewHarness(t)
			b, _ := realBundle(t, h, meta, s)
			e := &recoveryRecorder{t: t, notes: map[string]string{}}
			rep, err := recrun.Run(context.Background(), b.Input(h, p, false), e, p.Mappings(), meta.Claims)
			if err != nil {
				t.Fatalf("the typed recovery pass failed: %v", err)
			}
			var mapped, unmapped, skipped, recs int64
			for _, tc := range rep.Tables {
				if tc.Delivered != tc.Mapped+tc.Unmapped {
					t.Errorf("table %s: %d rows delivered, %d mapped and %d unmapped: a row was dropped silently", tc.Table, tc.Delivered, tc.Mapped, tc.Unmapped)
				}
				mapped, unmapped, skipped, recs = mapped+tc.Mapped, unmapped+tc.Unmapped, skipped+tc.Skipped, recs+tc.Records
				t.Logf("table %s: delivered %d, mapped %d (%d records), unmapped %d, skipped %d", tc.Table, tc.Delivered, tc.Mapped, tc.Records, tc.Unmapped, tc.Skipped)
			}
			if mapped+unmapped+skipped != rep.History.Delivered {
				t.Errorf("typed %d + unmapped %d + skipped %d != %d claimed history rows", mapped, unmapped, skipped, rep.History.Delivered)
			}
			if e.typed != recs || e.unmapped != unmapped {
				t.Errorf("the emitter saw %d typed records and %d unmapped rows, the report counts %d and %d", e.typed, e.unmapped, recs, unmapped)
			}
			var verdicts []string
			for v, n := range rep.History.Skipped {
				verdicts = append(verdicts, fmt.Sprintf("%s=%d", v, n))
			}
			sort.Strings(verdicts)
			t.Logf("history: %d rows delivered to the claims, not delivered by verdict %s; %d warnings", rep.History.Delivered, strings.Join(verdicts, " "), len(rep.History.Warnings))
		})
	}
}
