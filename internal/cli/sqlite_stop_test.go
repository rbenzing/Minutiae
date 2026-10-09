package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// runStdout runs the CLI and returns the exit code with stdout and stderr apart: a reader of stdout alone must
// not take a stopped run for a complete one (J95).
func runStdout(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stderr bytes.Buffer
	code, out := run(t, Deps{Err: &stderr}, args...)
	return code, out, stderr.String()
}

func stoppedResult() dbrecover.Result {
	return dbrecover.Result{
		AnalysisID: "an1", RulesHash: "h",
		Inputs: []dbrecover.InputInfo{{Role: "db", ArtifactID: "a1", SHA256: "ab", Size: 10}},
		Ingest: records.IngestResult{IngestID: "ing1", Outcome: "incomplete"},
	}
}

// J95: a run that stopped after the writer started says so on stdout, with the reason and "incomplete".
func TestSQLiteRecoverStoppedRunStatesReasonAndIncomplete(t *testing.T) {
	c := recordstest.NewCase(t)
	var out bytes.Buffer
	err := finishSQLiteRun(&out, c, stoppedResult(), context.Canceled, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the run error", err)
	}
	s := out.String()
	for _, want := range []string{"stopped: context canceled", "incomplete", "ingest ing1 incomplete"} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "not started") || strings.Contains(s, "ingest none") {
		t.Errorf("a stopped run reads as not started or empty:\n%s", s)
	}
	var js bytes.Buffer
	if err := finishSQLiteRun(&js, c, stoppedResult(), context.Canceled, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("json err %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		t.Fatalf("json: %v\n%s", err, js.String())
	}
	if m["stopped"] != "context canceled" || m["incomplete"] != true {
		t.Errorf("json stopped %v incomplete %v", m["stopped"], m["incomplete"])
	}
}

// J95: a failure before the writer starts prints "not started: <reason>", never "ingest none".
func TestSQLiteRecoverNotStartedSaysSo(t *testing.T) {
	c := recordstest.NewCase(t)
	res := stoppedResult()
	res.Ingest = records.IngestResult{}
	var out bytes.Buffer
	boom := errors.New("summarize failed")
	if err := finishSQLiteRun(&out, c, res, boom, false); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "not started: summarize failed") || strings.Contains(s, "ingest none") || strings.Contains(s, "stopped:") {
		t.Errorf("stdout:\n%s", s)
	}
	var js bytes.Buffer
	_ = finishSQLiteRun(&js, c, res, boom, true)
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["not_started"] != "summarize failed" {
		t.Errorf("json not_started %v", m["not_started"])
	}
	// a complete run still says "ingest none" when nothing was ingested and nothing failed
	var ok bytes.Buffer
	if err := finishSQLiteRun(&ok, c, res, nil, false); err != nil || !strings.Contains(ok.String(), "ingest none") {
		t.Errorf("complete empty run: %v\n%s", err, ok.String())
	}
}

// J95 through the command: a refused second run prints "not started" on stdout.
func TestSQLiteRecoverRefusedRunPrintsNotStartedOnStdout(t *testing.T) {
	e := newSQLEnv(t, "t", "/data/app.db")
	if code, out := e.recover(t, e.db.ID); code != 0 {
		t.Fatalf("first run %d\n%s", code, out)
	}
	code, out, errOut := runStdout(t, "sqlite", "recover", "--case", e.dir, e.db.ID)
	if code != ExitError {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "not started: ") || strings.Contains(out, "ingest none") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

// J96: the print error joins the run error, and an integrity failure is never masked by a cancel.
func TestSQLiteRecoverPrintIntegrityErrorIsNotMaskedByCancel(t *testing.T) {
	c := recordstest.NewCase(t)
	old := sqliteManifest
	sqliteManifest = func(*evidence.Case) ([]evidence.ManifestRecord, error) {
		return nil, fmt.Errorf("manifest: %w", evidence.ErrIntegrity)
	}
	t.Cleanup(func() { sqliteManifest = old })
	var out bytes.Buffer
	err := finishSQLiteRun(&out, c, stoppedResult(), context.Canceled, false)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the cancel kept", err)
	}
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("err %v, want the manifest integrity failure joined", err)
	}
	if got := ExitCode(err); got != ExitIntegrity {
		t.Errorf("exit %d, want 4", got)
	}
	if got := ExitCode(errors.Join(context.Canceled, evidence.ErrIntegrity)); got != ExitIntegrity {
		t.Errorf("joined exit %d, want 4", got)
	}
}

// J97: the refusals that fall through to exit 1.
func TestSQLiteRecoverRefusalsExit1(t *testing.T) {
	c := recordstest.NewCase(t)
	s := newSQLScenario("t")
	shot := sqlCapture(t, c, "acq1", "shot.db", "screenshot", "dev1", "/data/shot.db", s.db)
	// a hot journal whose header claims 2048-byte pages against the 1024-byte database: the live state is unavailable
	b := sqlTable(0, "t")
	j := b.NewJournal(512, 0x1234, 6)
	j.Record(4, b.PageBytes(4))
	j.SetHeader(1, 0x1234, 6, 512, 2048)
	live := sqlCapture(t, c, "acq1", "x.db", "file", "dev1", "/data/x.db", b.Bytes())
	sqlCapture(t, c, "acq1", "x.db-journal", "file", "dev1", "/data/x.db-journal", j.Bytes())
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, id, want string }{
		{"ineligible kind", shot.ID, dbrecover.ReasonIneligibleKind},
		{"live unavailable", live.ID, dbrecover.ReasonLiveUnavailable},
	} {
		code, out, errOut := runStdout(t, "sqlite", "recover", "--case", dir, tc.id)
		if code != ExitError {
			t.Errorf("%s: exit %d, want 1\n%s%s", tc.name, code, out, errOut)
		}
		if !strings.Contains(out+errOut, tc.want) {
			t.Errorf("%s: the reason %q is not named:\n%s%s", tc.name, tc.want, out, errOut)
		}
	}
}
