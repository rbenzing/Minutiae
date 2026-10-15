package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const sqlPageSize = 1024

func sqlFiller(n int, seed byte) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte('a' + byte((i*7+int(seed))%26))
	}
	return b.String()
}

func sqlTable(rev byte, table string) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: sqlPageSize})
	tb := b.CreateTable(table, "create table \""+table+"\"(a, b)")
	for id := int64(1); id <= 12; id++ {
		tb.Insert(id, id*3, sqlFiller(100, byte(id)+rev))
	}
	return b
}

// sqlScenario is a database whose freelist leaf keeps a stale leaf of the table, with a WAL of two
// committed frames and a hot journal.
type sqlScenario struct{ db, wal, journal []byte }

func newSQLScenario(table string) sqlScenario {
	b1, b2 := sqlTable(0, table), sqlTable(40, table)
	b1.SetFreelist([][]uint32{{5, 6}})
	db := b1.Bytes()
	old3 := slices.Clone(db[2*sqlPageSize : 3*sqlPageSize])
	old4 := slices.Clone(db[3*sqlPageSize : 4*sqlPageSize])
	copy(db[5*sqlPageSize:], old3)
	n2 := b2.Bytes()
	new3, new4 := n2[2*sqlPageSize:3*sqlPageSize], n2[3*sqlPageSize:4*sqlPageSize]
	copy(db[3*sqlPageSize:], new4)
	w := b1.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(3, old3, 6)
	w.Frame(3, new3, 6)
	j := b1.NewJournal(512, 0xdeadbeef, 6)
	j.Record(4, old4)
	return sqlScenario{db: db, wal: w.Bytes(), journal: j.Bytes()}
}

func sqlCapture(t *testing.T, c *evidence.Case, acq, name, kind, device, remote string, data []byte) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: kind, DeviceID: device, RemotePath: remote}
	rec, err := c.Capture(device, acq, name, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

type sqlEnv struct {
	dir              string
	db, wal, journal evidence.ManifestRecord
}

// newSQLEnv builds a closed case with the database /data/app.db and, by name, its WAL and journal.
func newSQLEnv(t *testing.T, table, remote string) *sqlEnv {
	t.Helper()
	c := recordstest.NewCase(t)
	s := newSQLScenario(table)
	e := &sqlEnv{dir: c.Dir}
	e.db = sqlCapture(t, c, "acq1", "app.db", "file", "dev1", remote, s.db)
	e.wal = sqlCapture(t, c, "acq1", "app.db-wal", "file", "dev1", remote+"-wal", s.wal)
	e.journal = sqlCapture(t, c, "acq1", "app.db-journal", "file", "dev1", remote+"-journal", s.journal)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *sqlEnv) recover(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return run(t, Deps{}, append([]string{"sqlite", "recover", "--case", e.dir}, args...)...)
}

// section returns the lines of out from the line that starts with from up to the one that starts with to.
func section(out, from, to string) []string {
	var got []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, from):
			in = true
		case strings.HasPrefix(l, to):
			in = false
		}
		if in {
			got = append(got, l)
		}
	}
	return got
}

func TestSQLiteRecoverRunsAndPrintsSummary(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	code, out := e.recover(t, e.db.ID)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"inputs:", "  db: artifact " + e.db.ID, "  wal: artifact " + e.wal.ID, "  journal: artifact " + e.journal.ID,
		"siblings:", "wal: artifact " + e.wal.ID + " (found by ", "recovered rows by method:", "  " + sqlitefile.MethodFreelist + ": rows ",
		", band ", "relation unknown: ", "secure_delete evidence: ", " freelist pages are all zero", "ingest ", " complete", "analysis ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "assessment only") {
		t.Errorf("a run says it is an assessment:\n%s", out)
	}
}

func fileSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) == "audit.jsonl" {
			return err
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // a test reading the files of its own temp case
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSQLiteRecoverListIsReadOnly(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	before := fileSnapshot(t, e.dir)
	nAudit := len(auditActionsOf(t, e.dir))
	for _, args := range [][]string{{"--list"}, {"--list", "--json"}, {"--list", "--no-siblings"}} {
		code, out := e.recover(t, append([]string{e.db.ID}, args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d:\n%s", args, code, out)
		}
		if !strings.Contains(out, "assessment only: nothing was written") {
			t.Errorf("%v: output lacks the assessment note:\n%s", args, out)
		}
	}
	if after := fileSnapshot(t, e.dir); !mapsEqual2(before, after) {
		t.Error("--list changed a file of the case")
	}
	acts := auditActionsOf(t, e.dir)[nAudit:]
	for _, a := range acts {
		if a != "case.open" {
			t.Errorf("--list appended the audit entry %q", a)
		}
	}
	if code, out := run(t, Deps{}, "records", "list", "--case", e.dir, "--type", "db_row"); code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("records exist after --list: %d %q", code, out)
	}
}

func mapsEqual2(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func auditActionsOf(t *testing.T, dir string) []string {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var a []string
	for _, e := range es {
		a = append(a, e.Action)
	}
	return a
}

func TestSQLiteRecoverListMatchesRun(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	code, listOut := e.recover(t, e.db.ID, "--list")
	if code != 0 {
		t.Fatalf("list: %d\n%s", code, listOut)
	}
	code, runOut := e.recover(t, e.db.ID)
	if code != 0 {
		t.Fatalf("run: %d\n%s", code, runOut)
	}
	a, b := section(listOut, "recovered rows by method:", "rows written"), section(runOut, "recovered rows by method:", "rows written")
	if len(a) < 2 || !slices.Equal(a, b) {
		t.Errorf("per-method lines differ:\nlist %q\nrun  %q", a, b)
	}
	if !slices.Equal(section(listOut, "siblings:", "recovered rows"), section(runOut, "siblings:", "recovered rows")) {
		t.Error("sibling lines differ between --list and the run")
	}
}

func TestSQLiteRecoverJSONShape(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	code, out := e.recover(t, e.db.ID, "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var got struct {
		Assessment bool   `json:"assessment"`
		AnalysisID string `json:"analysis_id"`
		RulesHash  string `json:"rules_hash"`
		Inputs     []struct {
			Role       string `json:"role"`
			ArtifactID string `json:"artifact_id"`
			Recovered  bool   `json:"recovered"`
		} `json:"inputs"`
		Siblings map[string]struct {
			Status string `json:"status"`
			Text   string `json:"text"`
		} `json:"siblings"`
		Counts struct {
			Written int64 `json:"written"`
			Methods []struct {
				Method string `json:"method"`
				Rows   int64  `json:"rows"`
			} `json:"methods"`
			Narrowed []string `json:"narrowed"`
		} `json:"counts"`
		Ingest *struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
			Records int64  `json:"records"`
		} `json:"ingest"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Assessment || got.AnalysisID == "" || got.RulesHash == "" || got.Ingest == nil || got.Ingest.Outcome != "complete" || got.Ingest.ID == "" {
		t.Errorf("shape: %+v", got)
	}
	if len(got.Inputs) != 3 || got.Inputs[0].Role != "db" || got.Inputs[0].ArtifactID != e.db.ID {
		t.Errorf("inputs %+v", got.Inputs)
	}
	if got.Siblings["wal"].Status != "found" || got.Siblings["journal"].Status != "found" || got.Siblings["wal"].Text == "" {
		t.Errorf("siblings %+v", got.Siblings)
	}
	var rows int64
	for _, m := range got.Counts.Methods {
		rows += m.Rows
	}
	if len(got.Counts.Methods) != 8 || rows == 0 || rows != got.Counts.Written || got.Ingest.Records != got.Counts.Written || got.Counts.Narrowed == nil {
		t.Errorf("counts %+v ingest %+v", got.Counts, got.Ingest)
	}
	code, out = e.recover(t, e.db.ID, "--list", "--json", "--allow-reingest")
	if code != 0 || !strings.Contains(out, `"assessment": true`) || strings.Contains(out, `"ingest"`) {
		t.Errorf("list json: %d\n%s", code, out)
	}
}

func TestSQLiteRecoverExitCodes(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	garbage := func(c *evidence.Case) evidence.ManifestRecord {
		return sqlCapture(t, c, "acq1", "junk.bin", "file", "dev1", "/data/junk.bin", []byte(strings.Repeat("not a database ", 100)))
	}
	c, err := evidence.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	junk := garbage(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"unknown artifact", []string{"nope"}, ExitError},
		{"not sqlite", []string{junk.ID}, ExitError},
		{"unusable explicit sibling", []string{e.db.ID, "--wal", junk.ID}, ExitError},
		{"min-confidence above 100", []string{e.db.ID, "--min-confidence", "101"}, ExitUsage},
		{"min-confidence negative", []string{e.db.ID, "--min-confidence", "-1"}, ExitUsage},
		{"unknown method", []string{e.db.ID, "--methods", "bogus"}, ExitUsage},
		{"empty method", []string{e.db.ID, "--methods", "sqlite-freelist,"}, ExitUsage},
		{"wal with no-siblings", []string{e.db.ID, "--wal", e.wal.ID, "--no-siblings"}, ExitUsage},
		{"journal with no-siblings", []string{e.db.ID, "--journal", e.journal.ID, "--no-siblings"}, ExitUsage},
		{"negative max-records", []string{e.db.ID, "--max-records", "-1"}, ExitUsage},
		{"negative max-bytes", []string{e.db.ID, "--max-bytes", "-1"}, ExitUsage},
		{"negative max-row-bytes", []string{e.db.ID, "--max-row-bytes", "-1"}, ExitUsage},
		{"negative max-body-bytes", []string{e.db.ID, "--max-body-bytes", "-1"}, ExitUsage},
		{"negative max-fit-equality-rows", []string{e.db.ID, "--max-fit-equality-rows", "-1"}, ExitUsage},
		{"negative timeout", []string{e.db.ID, "--timeout", "-1s"}, ExitUsage},
		{"bad timeout", []string{e.db.ID, "--timeout", "soon"}, ExitUsage},
		{"no artifact", nil, ExitUsage},
		{"cancelled by the deadline", []string{e.db.ID, "--timeout", "1ns"}, ExitError},
		{"list: bad flags", []string{e.db.ID, "--list", "--min-confidence", "101"}, ExitUsage},
	} {
		code, out := e.recover(t, tc.args...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d:\n%s", tc.name, code, tc.want, out)
		}
	}
	// a first run is complete; a second run of the same inputs is refused
	if code, out := e.recover(t, e.db.ID); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	if code, out := e.recover(t, e.db.ID); code != ExitError || !strings.Contains(out, "already") {
		t.Errorf("already ingested: exit %d, want 1 naming the earlier ingest:\n%s", code, out)
	}
	if code, out := e.recover(t, e.db.ID, "--allow-reingest"); code != 0 {
		t.Errorf("--allow-reingest: exit %d\n%s", code, out)
	}
}

func TestSQLiteRecoverNarrowedRunExits0AndPrintsNarrowing(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	code, out := e.recover(t, e.db.ID, "--methods", sqlitefile.MethodFreelist, "--exclude-table", "t", "--min-confidence", "10")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "narrowed by: methods, min-confidence, exclude-tables") {
		t.Errorf("the narrowing is not printed:\n%s", out)
	}
	if !strings.Contains(out, "ingest ") || !strings.Contains(out, " incomplete") {
		t.Errorf("a narrowed run must be recorded as incomplete:\n%s", out)
	}
}

func TestSQLiteRecoverAmbiguousSiblingExits2(t *testing.T) {
	c := recordstest.NewCase(t)
	s := newSQLScenario("t")
	db := sqlCapture(t, c, "acq1", "app.db", "file", "dev1", "/data/app.db", s.db)
	sqlCapture(t, c, "acq1", "app.db-wal", "file", "dev1", "/data/app.db-wal", s.wal)
	sqlCapture(t, c, "acq1", "app.db-wal-copy", "file", "dev1", "/data/app.db-wal", s.wal)
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--list"}} {
		code, out := run(t, Deps{}, append([]string{"sqlite", "recover", "--case", dir, db.ID}, extra...)...)
		if code != ExitUsage || !strings.Contains(out, "more than one candidate") {
			t.Errorf("%v: exit %d, want 2 naming the candidates:\n%s", extra, code, out)
		}
	}
	// naming one, or turning the search off, resolves it
	if code, out := run(t, Deps{}, "sqlite", "recover", "--case", dir, db.ID, "--no-siblings"); code != 0 {
		t.Errorf("--no-siblings: %d\n%s", code, out)
	}
}

func TestSQLiteRecoverInputChangedExits4(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	p := filepath.Join(e.dir, filepath.FromSlash(e.db.Path))
	b, err := os.ReadFile(p) //nolint:gosec // inside the test case
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // inside the test case
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--list"}} {
		code, out := e.recover(t, append([]string{e.db.ID}, extra...)...)
		if code != ExitIntegrity {
			t.Errorf("%v: exit %d, want 4:\n%s", extra, code, out)
		}
	}
}

func TestSQLiteRecoverNeedsUpgradeExits2(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	code, out := run(t, Deps{}, "sqlite", "recover", "--case", dir, "A1")
	if code != ExitUsage || !strings.Contains(out, "case upgrade") {
		t.Errorf("exit %d, want 2 naming case upgrade:\n%s", code, out)
	}
}

func TestSQLiteRecoverIndexNotCurrentExits2(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	recordstest.SetFTSNormVersion(t, e.dir, "0")
	code, out := e.recover(t, e.db.ID)
	if code != ExitUsage || !strings.Contains(out, "reindex") {
		t.Errorf("exit %d, want 2 naming records reindex:\n%s", code, out)
	}
}

func TestSQLiteRecoverOnTamperedCaseExits4(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	overwriteLine(t, filepath.Join(e.dir, "audit.jsonl"), 2)
	for _, extra := range [][]string{nil, {"--list"}} {
		code, out := e.recover(t, append([]string{e.db.ID}, extra...)...)
		if code != ExitIntegrity {
			t.Errorf("%v: exit %d, want 4:\n%s", extra, code, out)
		}
	}
}

func TestSQLiteRecoverRegistersDBRowType(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	if code, out := e.recover(t, e.db.ID); code != 0 {
		t.Fatalf("run: %d\n%s", code, out)
	}
	code, out := run(t, Deps{}, "records", "list", "--case", e.dir, "--type", "db_row")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("records list --type db_row: %d %q", code, out)
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(l, "[recovered:") {
			t.Errorf("a recovered row is shown without its marker: %s", l)
		}
	}
}

func TestSQLiteRecoverEscapesOutput(t *testing.T) {
	evil := "a\x1b[31mRED\x1b]0;title\x07\u202eevil\u2066x\u2069\u200b\ufeff\u0085\nforged: line"
	e := newSQLEnv(t, evil, "/data/"+evil+".db")
	for _, args := range [][]string{
		{e.db.ID},
		{e.db.ID, "--list"},
		{e.db.ID, "--exclude-table", evil},
		{e.db.ID, "--no-siblings"},
		{evil},
		{e.db.ID, "--wal", evil},
		{e.db.ID, "--journal", evil, "--list"},
		{e.db.ID, "--methods", evil},
		{e.db.ID, "--allow-reingest"},
	} {
		code, out := e.recover(t, args...)
		noRawNonPrintable(t, fmt.Sprint(args, " exit ", code), out)
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "forged:") {
				t.Errorf("%v: a case-supplied newline forged the line %q", args, l)
			}
		}
	}
}

func TestSQLiteRecoverSiblingMessages(t *testing.T) {
	// a database with no companions: both searches say what they searched by and never say the file does not exist
	c := recordstest.NewCase(t)
	s := newSQLScenario("t")
	plain := sqlCapture(t, c, "acq1", "plain.db", "file", "dev1", "/data/plain.db", s.db)
	backup := sqlCapture(t, c, "acq1", "backup.db", "backup", "dev1", "/data/backup.db", s.db)
	// a WAL of another acquisition is named, never paired
	other := sqlCapture(t, c, "acq2", "other.db-wal", "file", "dev1", "/data/plain.db-wal", s.wal)
	unused := sqlCapture(t, c, "acq1", "unused.db", "file", "dev1", "/data/unused.db", s.db)
	bad := slices.Clone(s.wal)
	bad[24] ^= 0xff // the header checksum
	sqlCapture(t, c, "acq1", "unused.db-wal", "file", "dev1", "/data/unused.db-wal", bad)
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	exp := func(name, artifact string, args []string, wants ...string) {
		t.Helper()
		code, out := run(t, Deps{}, append([]string{"sqlite", "recover", "--case", dir, artifact, "--list"}, args...)...)
		if code != 0 {
			t.Errorf("%s: exit %d\n%s", name, code, out)
			return
		}
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output lacks %q:\n%s", name, w, out)
			}
		}
		if strings.Contains(out, "does not exist") || strings.Contains(out, "no wal") {
			t.Errorf("%s: a search that found nothing must not claim the file is absent:\n%s", name, out)
		}
	}
	exp("none found", plain.ID, nil, "wal: none found", "(searched by device", "journal: none found")
	exp("backup kind none found", backup.ID, nil, "wal: none found (searched by device", "/data/backup.db-wal", "journal: none found")
	exp("other acquisition", plain.ID, nil, "none found in this acquisition", other.ID, "not paired")
	exp("explicit", plain.ID, []string{"--wal", other.ID}, "wal: artifact "+other.ID+" (named by the examiner)")
	exp("disabled", plain.ID, []string{"--no-siblings"}, "wal: not searched (the search is turned off)", "journal: not searched (the search is turned off)")
	exp("found, unused", unused.ID, nil, "wal: artifact ", "found, unused (the WAL header is not valid", "journal: none found")
}

// The text a case can supply (ids, a recovery method, a companion's id and unused reason, skip reasons,
// narrowing) reaches the terminal only escaped, whatever the library puts in it.
func TestSQLiteRecoverEscapesEveryCaseString(t *testing.T) {
	evil := string([]rune{'a', 0x1b}) + "[31mRED" + string([]rune{0x1b, ']'}) + "0;t" + string([]rune{7, 0x202e}) + "evil" + string([]rune{0x2066, 'x', 0x2069, 0x200b, 0xfeff, 0x85, '\n'}) + "forged: line"
	inputs := []dbrecover.InputInfo{{Role: evil, ArtifactID: evil, SHA256: evil, Size: 1, Recovered: true, Incomplete: true}}
	trails := map[string]string{evil: evil}
	sib := dbrecover.SiblingReport{
		WAL:     dbrecover.SiblingResult{Status: dbrecover.SiblingFound, ArtifactID: evil, Unused: evil, SearchedBy: evil},
		Journal: dbrecover.SiblingResult{Status: dbrecover.SiblingAmbiguous, Candidates: []string{evil}, SearchedBy: evil},
	}
	counts := dbrecover.Counts{
		ByMethod: map[string]int64{evil: 1}, ConfidenceRange: map[string]dbrecover.ConfRange{evil: {Min: 1, Max: 2}},
		SkippedByReason: map[string]int64{evil: 1}, Narrowed: []string{evil}, LimitsHit: []string{evil},
	}
	var b strings.Builder
	printSQLiteBody(&b, inputs, trails, sib, counts)
	noRawNonPrintable(t, "summary body", b.String())
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(l, "forged:") {
			t.Errorf("a case-supplied newline forged the line %q", l)
		}
	}
}

// TestSQLiteRecoverJSONIncompleteFollowsTheOutcome (J125): the top-level
// incomplete is true whenever the ingest concluded incomplete, a narrowed or a
// limit-stopped run included, and false for a complete run.
func TestSQLiteRecoverJSONIncompleteFollowsTheOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want bool
	}{
		"complete":     {nil, false},
		"narrowed":     {[]string{"--methods", sqlitefile.MethodFreelist}, true},
		"limit-stoped": {[]string{"--max-records", "1"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSQLEnv(t, "t", "/data/app.db")
			code, out := e.recover(t, append([]string{e.db.ID, "--json"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			var got struct {
				Incomplete bool `json:"incomplete"`
				Ingest     struct {
					Outcome string `json:"outcome"`
				} `json:"ingest"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got.Incomplete != tc.want || (got.Ingest.Outcome == "incomplete") != tc.want {
				t.Errorf("incomplete %v outcome %q, want incomplete %v", got.Incomplete, got.Ingest.Outcome, tc.want)
			}
		})
	}
}

// TestSQLiteRecoverPrintsIncompleteReasons (J112): a run that concluded
// incomplete without an error says so, with the reasons, before the ingest line.
func TestSQLiteRecoverPrintsIncompleteReasons(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	code, out := e.recover(t, e.db.ID, "--max-records", "1", "--methods", sqlitefile.MethodFreelist)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "incomplete: methods, max-records\n") {
		t.Errorf("no incomplete line naming the reasons:\n%s", out)
	}
	e2 := newSQLEnv(t, "t", "/data/app.db")
	_, out2 := e2.recover(t, e2.db.ID)
	if strings.Contains(out2, "incomplete:") {
		t.Errorf("a complete run prints an incomplete line:\n%s", out2)
	}
}
