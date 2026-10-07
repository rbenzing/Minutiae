package evidence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func validRecovery(free int64) *evidence.Recovery {
	return &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(55), Basis: []string{"dirent:1:1"},
		Alloc: evidence.AllocSummary{Free: free}, Algorithm: evidence.AlgorithmRecover,
	}
}

func TestVerifyRecoveredRunsRules(t *testing.T) {
	add := func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte, spec evidencetest.RecoveredSpec) {
		evidencetest.AddRecovered(t, c, p, d, spec)
	}
	rows := []struct {
		name string
		do   func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte)
		want string
	}{
		{"hole", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			add(t, c, p, d, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: -1, Length: 4}}})
		}, "run 0 is a hole"},
		{"negative", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			add(t, c, p, d, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: -2, Length: 4}}})
		}, "is not valid (negative or overflowing)"},
		{"zero length", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			add(t, c, p, d, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 0}}})
		}, "is empty (zero length)"},
		{"sum mismatch", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			add(t, c, p, d, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 8}}, Data: []byte("abcd")})
		}, "runs cover 8 bytes but the artifact holds 4"},
		{"no runs", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			add(t, c, p, d, evidencetest.RecoveredSpec{Data: []byte("abcd"), Alloc: &evidence.AllocSummary{}})
		}, "records no runs"},
		{"incomplete without error", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			dd := derivedOf(p, evidence.Run{Offset: 0, Length: 4})
			dd.Recovery = validRecovery(4)
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, d[:4], errors.New(""))
		}, "is flagged incomplete but records no error"},
		{"sidecar missing", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			dd := derivedOf(p)
			dd.RunsArtifact = "art-missing"
			dd.Recovery = validRecovery(4)
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, d[:4], nil)
		}, "runs sidecar"},
		{"sidecar with an unknown field", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, d []byte) {
			sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(p)}, []byte(`{"offset":0,"length":4,"x":1}`+"\n"), nil)
			dd := derivedOf(p)
			dd.RunsArtifact = sc.ID
			dd.Recovery = validRecovery(4)
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, d[:4], nil)
		}, "runs sidecar"},
		{"too many inline runs", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord, _ []byte) {
			var runs []evidence.Run
			for i := range evidence.MaxInlineRuns + 1 {
				runs = append(runs, evidence.Run{Offset: int64(i * 2), Length: 1})
			}
			dd := derivedOf(p, runs...)
			dd.Recovery = validRecovery(int64(len(runs)))
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, make([]byte, len(runs)), nil)
		}, "inline runs"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			row.do(t, c, parent, data)
			evidencetest.RequireProblem(t, verifyOf(t, c), row.want)
		})
	}
}

func TestCheckRecoveredRunsTooManyRuns(t *testing.T) {
	runs := make([]evidence.Run, evidence.MaxRecoveredRuns+1)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(i * 2), Length: 1}
	}
	got := evidence.CheckRecoveredRuns(evidence.ManifestRecord{ID: "a", Path: "p", Size: int64(len(runs))}, runs)
	if len(got) != 1 || !strings.Contains(got[0], "too many runs") {
		t.Fatalf("problems = %q, want one containing %q", got, "too many runs")
	}
}

// C3: a recovered artifact records the runs of the bytes it holds, so the sum
// equals the size for a cut prefix and for a failed read alike.
func TestVerifyRecoveredIncompleteDirections(t *testing.T) {
	cases := []struct {
		name string
		spec evidencetest.RecoveredSpec
		want string // "" = clean
	}{
		{"cut prefix, sum equals size", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 100}}, Incomplete: true}, ""},
		{"sum above size is rejected", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 200}}, Data: bytes.Repeat([]byte("a"), 100), Incomplete: true}, "runs cover 200 bytes but the artifact holds 100"},
		{"sum below size is rejected", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 100}}, Data: bytes.Repeat([]byte("a"), 200), Incomplete: true}, "runs cover 100 bytes but the artifact holds 200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, tc.spec)
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

func TestVerifyRecoveredAllocRules(t *testing.T) {
	carved := func(scope string, a evidence.AllocSummary) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{
			Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-jpeg", Scope: scope, Path: "carved/c1.bin",
			Runs: []evidence.Run{{Offset: 0, Length: 64}}, Alloc: &a,
		}
	}
	rows := []struct {
		name string
		spec evidencetest.RecoveredSpec
		want string
	}{
		{"deleted-file with allocated bytes", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}, Alloc: &evidence.AllocSummary{Free: 32, Allocated: 32}}, "allocated bytes in a deleted-file artifact"},
		{"deleted-file with unknown bytes", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}, Alloc: &evidence.AllocSummary{Free: 32, Unknown: 32}}, "unknown bytes in a deleted-file artifact"},
		{"total mismatch", evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}, Alloc: &evidence.AllocSummary{Free: 10}}, "alloc: free+allocated+unknown"},
		{"carved in scope unallocated with allocated bytes", carved("unallocated", evidence.AllocSummary{Free: 32, Allocated: 32}), "scope unallocated but"},
		{"carved in scope raw with allocated bytes", carved("raw", evidence.AllocSummary{Free: 32, Allocated: 32}), ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, row.spec)
			rep := verifyOf(t, c)
			if row.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, row.want)
		})
	}
}

func TestVerifyRecoveredNoticesForUnreproducedKinds(t *testing.T) {
	c, parent, data := recoverParent(t)
	run := []evidence.Run{{Offset: 0, Length: 64}}
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindSlack, Class: evidence.ClassSlack, Method: "slack-file", Path: "slack/s1.bin", Runs: run, Confidence: ip(10),
	})
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: "ext4-journal-inode", Path: "journal/j1.bin", Runs: run,
		Mutate: func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"} },
	})
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "ext4-journal-inode", Path: "reports/r1.json", Runs: run,
	})
	rep := verifyOf(t, c)
	evidencetest.RequireClean(t, rep)
	for _, k := range []string{"slack", "journal", "report"} {
		evidencetest.RequireNotice(t, rep, fmt.Sprintf("kind %q: 1 artifact(s) are not reproduced byte for byte by this build", k))
	}
	if rep.RecoveredArtifacts != 3 {
		t.Fatalf("RecoveredArtifacts = %d, want 3", rep.RecoveredArtifacts)
	}
}

func lastVerifyRun(t *testing.T, c *evidence.Case) evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	last := es[len(es)-1]
	if last.Action != "verify.run" {
		t.Fatalf("last audit action = %s, want verify.run", last.Action)
	}
	return last
}

func noop(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) {}

func TestVerifyRunAuditCoversComposedChecks(t *testing.T) {
	t.Run("a problem and the totals reach the audit entry, in order, after the built-in checks", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}})
		var order []string
		first := func(_ context.Context, _ *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
			order = append(order, "first")
			if rep.RecoveredArtifacts != 1 || len(recs) != 2 || rep.ArtifactsChecked != 2 {
				t.Errorf("built-in checks had not run: recovered=%d recs=%d checked=%d", rep.RecoveredArtifacts, len(recs), rep.ArtifactsChecked)
			}
			rep.AddProblem("reproduce: x")
		}
		second := func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, rep *evidence.VerifyReport) {
			order = append(order, "second")
			rep.RecoveredReproduced = 1
			rep.AddNotice("reproduce: ok")
		}
		rep := verifyOf(t, c, first, second)
		if strings.Join(order, ",") != "first,second" {
			t.Fatalf("checks ran %v", order)
		}
		evidencetest.RequireProblem(t, rep, "reproduce: x")
		evidencetest.RequireNotice(t, rep, "reproduce: ok")
		if rep.OK() {
			t.Fatal("the report is OK despite a problem")
		}
		d := lastVerifyRun(t, c).Details
		if d["ok"] != false || fmt.Sprint(d["problems"]) != "1" ||
			fmt.Sprint(d["recovered_artifacts"]) != "1" || fmt.Sprint(d["recovered_reproduced"]) != "1" {
			t.Fatalf("verify.run details = %v", d)
		}
		for _, k := range []string{"fts_docs_checked", "artifacts_checked", "audit_entries", "records_checked", "record_batches_checked", "record_runs_checked"} {
			if _, ok := d[k]; !ok {
				t.Fatalf("verify.run lost %s: %v", k, d)
			}
		}
	})
	t.Run("a panicking check is a problem and later checks still run", func(t *testing.T) {
		c, _, _ := recoverParent(t)
		ran := false
		rep := verifyOf(t, c,
			func(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) {
				panic("boom")
			},
			func(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) { ran = true },
		)
		evidencetest.RequireProblem(t, rep, "verify check panicked: boom")
		if !ran {
			t.Fatal("the check after the panic did not run")
		}
	})
	t.Run("a cancelled context is never OK", func(t *testing.T) {
		c, _, _ := recoverParent(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := false
		rep, err := c.VerifyWith(ctx, func(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) { ran = true })
		if err != nil {
			t.Fatal(err)
		}
		evidencetest.RequireProblem(t, rep, "reproduce: verification cancelled")
		if ran {
			t.Fatal("a check ran under a cancelled context")
		}
		if d := lastVerifyRun(t, c).Details; d["ok"] != false {
			t.Fatalf("verify.run ok = %v, want false", d["ok"])
		}
	})
	t.Run("cancelled during a check is reported once", func(t *testing.T) {
		c, _, _ := recoverParent(t)
		ctx, cancel := context.WithCancel(context.Background())
		rep, err := c.VerifyWith(ctx, func(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) { cancel() }, noop)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, p := range rep.Problems {
			if strings.Contains(p, "reproduce: verification cancelled") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("cancel reported %d times, want once: %q", n, rep.Problems)
		}
	})
	t.Run("Verify equals VerifyWith without checks", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}})
		a, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		b := verifyOf(t, c)
		a.AuditEntries, b.AuditEntries = 0, 0 // the first run appended its own entry
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			t.Fatalf("Verify = %s\nVerifyWith() = %s", ja, jb)
		}
	})
}

func TestVerifyReportJSONShape(t *testing.T) {
	c, _, _ := recoverParent(t)
	b, err := json.Marshal(verifyOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"audit_entries", "artifacts_checked", "records_checked", "record_batches_checked", "record_runs_checked",
		"fts_docs_checked", "problems", "notices", "recovered_artifacts", "recovered_reproduced",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("report JSON has no %q: %s", k, b)
		}
	}
	if len(m) != 10 {
		t.Errorf("report JSON has %d keys, want 10: %s", len(m), b)
	}
}

func TestVerifyRecoveredStreamsProblemsCapped(t *testing.T) {
	c, parent, data := recoverParent(t)
	for i := range 60 {
		evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
			Path: fmt.Sprintf("recovered/p1-mtfs/%06d-a.bin", i+1), Runs: []evidence.Run{{Offset: int64(i), Length: 1}},
			Mutate: func(r *evidence.Recovery) { r.Confidence = ip(81) },
		})
	}
	rep := verifyOf(t, c)
	listed, further := 0, 0
	for _, p := range rep.Problems {
		switch {
		case strings.Contains(p, "above the ceiling 80"):
			listed++
		case strings.Contains(p, "10 further") && strings.Contains(p, "problems are not listed"):
			further++
		}
	}
	if listed != 50 || further != 1 {
		t.Fatalf("listed %d, further lines %d; want 50 and 1:\n%s", listed, further, strings.Join(rep.Problems, "\n"))
	}
}
