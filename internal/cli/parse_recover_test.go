package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const mmsLogical = "/data/data/com.android.providers.telephony/databases/mmssms.db"

// mmsCase is a case holding the av-freelist-512 telephony database at remotePath (a logical path that
// the android.mmssms glob matches), returning the case directory and the artifact.
func mmsCase(t *testing.T, remotePath string) (string, evidence.ManifestRecord) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "parsers", "androidmms", "testdata", "av-freelist-512.db.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	db, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "logical"}); err != nil {
		t.Fatal(err)
	}
	rec := pcapture(t, c, "files/mmssms.db", evidence.Source{Kind: "file", RemotePath: remotePath}, string(db))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return c.Dir, rec
}

func recoveredRows(t *testing.T, dir string, extra ...string) []rowJSON {
	t.Helper()
	args := append([]string{"records", "list", "--case", dir, "--json", "--limit", "1000"}, extra...)
	code, out, _ := runP(t, nil, args...)
	if code != 0 {
		t.Fatalf("records list: %d\n%s", code, out)
	}
	var l struct {
		Records []rowJSON `json:"records"`
	}
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatalf("records list json: %v\n%s", err, out)
	}
	return l.Records
}

func TestParseRecoverRunsClaimingParsersOnly(t *testing.T) {
	dir, _ := mmsCase(t, mmsLogical)
	code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Count(errOut, "android.mmssms.recover"); got != 1 {
		t.Errorf("%d status lines name android.mmssms.recover, want exactly 1 job\n%s", got, errOut)
	}
	if strings.Contains(errOut, "android.calllog") || strings.Contains(errOut, "android.contacts") {
		t.Errorf("a parser whose glob does not match ran:\n%s", errOut)
	}
	if !strings.Contains(out, "recovered records are not live evidence") {
		t.Errorf("the summary does not say the records are not live:\n%s", out)
	}
	rows := recoveredRows(t, dir, "--recovered", "only")
	if len(rows) <= 100 {
		t.Fatalf("%d recovered records, want more than 100", len(rows))
	}
	for _, r := range rows {
		if !r.Deleted || !r.Recovered || r.RecoveryMethod != "sqlite-freelist" {
			t.Fatalf("record %+v is not a deleted sqlite-freelist recovery", r)
		}
	}
	if live := recoveredRows(t, dir, "--recovered", "none"); len(live) != 0 {
		t.Errorf("%d live records after a recovery pass", len(live))
	}
	_, text, _ := runP(t, nil, "records", "list", "--case", dir, "--limit", "1000")
	if n := strings.Count(text, "[recovered:sqlite-freelist]"); n != len(rows) {
		t.Errorf("records list shows %d [recovered:sqlite-freelist] markers for %d records", n, len(rows))
	}
}

func TestParseRecoverRefusesNonClaimingParser(t *testing.T) {
	p := newPfx(t, "a")
	reg := registry(t, "", good("a"))
	code, _, errOut := runP(t, reg, "parse", "recover", "--case", p.dir, "--parser", "a")
	if code != ExitUsage {
		t.Errorf("exit %d, want 2\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "claims no tables") {
		t.Errorf("stderr does not say the parser claims no tables:\n%s", errOut)
	}
	if code, _, _ := runP(t, reg, "parse", "recover", "--case", p.dir, "--parser", "nope"); code != ExitUsage {
		t.Errorf("unknown parser: exit %d, want 2", code)
	}
}

func TestParseRecoverUsageErrors(t *testing.T) {
	dir, _ := mmsCase(t, mmsLogical)
	for _, args := range [][]string{
		{"--min-confidence", "101"},
		{"--min-confidence", "-1"},
		{"--timeout", "0s"},
		{"--mem-budget", "1KiB"},
	} {
		code, _, errOut := runP(t, nil, append([]string{"parse", "recover", "--case", dir}, args...)...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, errOut)
		}
	}
	if recs := recoveredRows(t, dir); len(recs) != 0 {
		t.Errorf("a usage error wrote %d records", len(recs))
	}
}

func TestParseRecoverMinConfidenceNarrowsExit1(t *testing.T) {
	dir, _ := mmsCase(t, mmsLogical)
	code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir, "--min-confidence", "70")
	if code != ExitError {
		t.Fatalf("exit %d, want 1\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "narrowed") || !strings.Contains(out, "below the minimum confidence") {
		t.Errorf("the summary does not say the run was narrowed:\n%s", out)
	}
	if rows := recoveredRows(t, dir, "--recovered", "only"); len(rows) != 0 {
		t.Errorf("%d records at or over 70 from a freelist whose cap is 60", len(rows))
	}
}

func TestParseRecoverJSONIsTerminalSafe(t *testing.T) {
	hostile := "/data/\x1b[31m\nevil/com.android.providers.telephony/databases/mmssms.db"
	dir, _ := mmsCase(t, hostile)
	code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	events := map[string]int{}
	var end, sum map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		if r, bad := rawControl(line); bad {
			t.Errorf("raw control character %U in %q", r, line)
		}
		ev, _ := m["event"].(string)
		events[ev]++
		switch ev {
		case "job.end":
			end = m
		case "summary":
			sum = m
		}
	}
	for _, ev := range []string{"run.start", "job.start", "job.end", "summary"} {
		if events[ev] == 0 {
			t.Errorf("no %s event: %v", ev, events)
		}
	}
	for _, k := range []string{"claims", "unmapped_recovered", "below_min_confidence", "narrowed", "skipped_invalid", "unmapped_overflow", "records", "outcome", "parser"} {
		if _, ok := end[k]; !ok {
			t.Errorf("job.end has no %q key: %v", k, end)
		}
	}
	if end["parser"] != "android.mmssms.recover" {
		t.Errorf("job.end parser = %v", end["parser"])
	}
	if cl, _ := end["claims"].([]any); len(cl) != 2 {
		t.Errorf("claims = %v, want sms and pdu", end["claims"])
	}
	if nr, ok := end["narrowed"].([]any); !ok || len(nr) != 0 {
		t.Errorf("narrowed = %#v, want an empty list", end["narrowed"])
	}
	if sum["recovered_not_live"] != true {
		t.Errorf("summary does not say recovered records are not live: %v", sum)
	}
	if sum["class"] != "ok" {
		t.Errorf("summary class = %v", sum["class"])
	}
	if jobs, _ := sum["jobs"].([]any); len(jobs) != 1 {
		t.Errorf("summary jobs = %v", sum["jobs"])
	} else if j, _ := jobs[0].(map[string]any); j["claims"] == nil {
		t.Errorf("summary job lacks claims: %v", j)
	}
}

func TestParseRecoverCLIEscapesDeviceText(t *testing.T) {
	hostile := "/data/\x1b[31m\nevil/com.android.providers.telephony/databases/mmssms.db"
	dir, _ := mmsCase(t, hostile)
	var shown string
	for _, args := range [][]string{
		{"parse", "plan", "--case", dir},
		{"parse", "recover", "--case", dir, "--min-confidence", "70", "-v"},
		{"parse", "recover", "--case", dir, "-v", "--reparse"},
	} {
		_, out, errOut := runP(t, nil, args...)
		shown += out + errOut
		for name, s := range map[string]string{"stdout": out, "stderr": errOut} {
			if r, bad := rawControl(s); bad {
				t.Errorf("%v: raw control %U in %s:\n%q", args, r, name, s)
			}
			if strings.Contains(s, "\x1b") {
				t.Errorf("%v: raw escape reached the terminal in %s", args, name)
			}
		}
	}
	if !strings.Contains(shown, `\x1b`) && !strings.Contains(shown, `\u001b`) {
		t.Errorf("the hostile path is not shown escaped:\n%q", shown)
	}
}

// A tampered artifact is an integrity failure (exit 4) that no other error masks; nothing is written.
func TestParseRecoverTamperedArtifactExits4(t *testing.T) {
	dir, rec := mmsCase(t, mmsLogical)
	flipByte(t, dir, rec.Path, 100)
	code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir)
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
	}
	if rows := recoveredRows(t, dir); len(rows) != 0 {
		t.Errorf("%d records written from a tampered artifact", len(rows))
	}
	// a narrowed run over the same tamper is still exit 4, never 1
	code, _, _ = runP(t, nil, "parse", "recover", "--case", dir, "--min-confidence", "70")
	if code != ExitIntegrity {
		t.Errorf("narrowed run: exit %d, want 4", code)
	}
}

// The summary names every way a job fell short of covering its claims, and escapes what came from a device.
func TestParseRecoverSummaryNamesShortfalls(t *testing.T) {
	evil := "bad \x1b[31m\nrow"
	sum := artparse.RunSummary{ParseID: "p1", Records: 3, Jobs: []artparse.JobResult{
		{
			Job: 1, Parser: "x.recover", Version: "1.0.0", ArtifactID: "a1", Logical: evil, Outcome: artparse.OutcomeIncomplete,
			Reason: "3 claimed recovered rows skipped: their facts were invalid" + evil, SkippedInvalid: 3, Unmapped: 2,
		},
		{Job: 2, Parser: "y.recover", Version: "1.0.0", ArtifactID: "a2", Outcome: artparse.OutcomeComplete, UnmappedOverflow: true, Unmapped: 70000},
		{Job: 3, Parser: "z.recover", Version: "1.0.0", ArtifactID: "a3", Outcome: artparse.OutcomeComplete},
	}}
	if sum.Class() != artparse.ClassPartial {
		t.Fatalf("class %v, want partial (an incomplete job)", sum.Class())
	}
	var b bytes.Buffer
	writeRecoverSummaryText(&b, sum, false)
	out := b.String()
	for _, want := range []string{
		"recovered records are not live evidence",
		"3 claimed recovered rows skipped: their facts were invalid",
		"70,000 claimed recovered row(s) could not be mapped",
		"claims are not honored by sqlite recover",
		"70,002 unmapped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "z.recover") {
		t.Errorf("a clean job is listed:\n%s", out)
	}
	if r, bad := rawControl(out); bad {
		t.Errorf("raw control %U in the summary:\n%q", r, out)
	}
	if line := recoverStatusLine(sum.Jobs[0], 3); !strings.Contains(line, "2 unmapped") || strings.Contains(line, "\x1b") {
		t.Errorf("status line %q", line)
	}
}
