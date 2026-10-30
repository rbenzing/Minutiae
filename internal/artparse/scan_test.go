package artparse_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// sniff is a parser whose Probe says applicable exactly when the primary starts with magic (identity from
// structure: where the file sits does not matter), whose glob matches only <name>.dat under .../parsertest/, and
// whose Parse calls are counted.
type sniff struct {
	parsertest.WellBehaved
	magic  string
	parses *atomic.Int32
}

func sniffer(name, magic string) sniff {
	return sniff{WellBehaved: parsertest.WellBehaved{Name: name, Version: "1.0.0"}, magic: magic, parses: new(atomic.Int32)}
}

func (p sniff) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	b := make([]byte, len(p.magic))
	n, err := in.Primary.R.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return parse.Applicability{}, err
	}
	if string(b[:n]) != p.magic {
		return parse.Applicability{Status: parse.NotApplicable, Reason: "no magic"}, nil
	}
	return parse.Applicability{Status: parse.Applicable}, nil
}

func (p sniff) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	p.parses.Add(1)
	return p.WellBehaved.Parse(ctx, in, out)
}

const magic = "MAGIC\n"

var scanSel = artparse.Selection{Scan: true}

func scanPlan(t testing.TB, h *artparse.Host, sel artparse.Selection) artparse.Plan {
	t.Helper()
	sel.Scan = true
	return planOf(t, h, sel)
}

func scanRows(p artparse.Plan) []artparse.PlanRow {
	var out []artparse.PlanRow
	for _, r := range p.Rows {
		if r.Scan {
			out = append(out, r)
		}
	}
	return out
}

func TestScanFindsARelocatedDatabase(t *testing.T) {
	f := newRx(t)
	matched := putFile(t, f.c, "D1", "A1", fakePath("sniff"), magic+"matched\n")
	moved := putFile(t, f.c, "D1", "A1", "/sdcard/old/sms-copy.db", magic+"moved\n")
	h := f.host(nil, sniffer("sniff", magic))

	if p := planOf(t, h, artparse.Selection{}); len(p.Rows) != 1 || p.Rows[0].Job.Primary.Artifact.ID != matched.ID || p.Scan != nil {
		t.Fatalf("without Scan the plan is the glob-matched job alone: %+v scan %+v", p.Rows, p.Scan)
	}
	p := scanPlan(t, h, artparse.Selection{})
	rows := scanRows(p)
	if len(p.Rows) != 2 || len(rows) != 1 {
		t.Fatalf("rows %d, scan rows %d, want 2 and 1", len(p.Rows), len(rows))
	}
	r := rows[0]
	if r.Job.Primary.Artifact.ID != moved.ID || r.Status != artparse.StatusCandidate || r.Job.Parser.Meta().Name != "sniff" || !r.Job.Explicit || !r.Scan {
		t.Errorf("scan row %+v", r)
	}
	if sc(t, p).Candidates != 1 || sc(t, p).Considered != 1 || sc(t, p).Probed != 1 || sc(t, p).Stopped != "" {
		t.Errorf("scan counts %+v", p.Scan)
	}
	if p.ByStatus[artparse.StatusCandidate] != 1 {
		t.Errorf("by status %v", p.ByStatus)
	}

	sum := f.run(h, artparse.Selection{})
	if len(sum.Jobs) != 1 || sum.Jobs[0].ArtifactID != matched.ID {
		t.Fatalf("the default run must not touch the candidate: %+v", sum.Jobs)
	}
	sum = f.run(h, artparse.Selection{Artifacts: []string{moved.ID}})
	if len(sum.Jobs) != 1 || sum.Jobs[0].ArtifactID != moved.ID || sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("--artifact runs the candidate: %+v", sum.Jobs)
	}
}

func TestScanSkipsGlobMatchedAndBundledArtifacts(t *testing.T) {
	f := newRx(t)
	putFile(t, f.c, "D1", "A1", fakePath("w"), "db")
	putFile(t, f.c, "D1", "A1", fakePath("w")+"-wal", "wal")
	putFile(t, f.c, "D1", "A1", "/sdcard/stray.db-wal", "stray")
	other := putFile(t, f.c, "D1", "A1", "/sdcard/other.bin", "other")
	h := f.host(nil, walParser{name: "w"})
	p := scanPlan(t, h, artparse.Selection{})
	if sc(t, p).Considered != 2 || sc(t, p).Companions != 1 || sc(t, p).Probed != 1 || sc(t, p).Candidates != 1 {
		t.Fatalf("scan counts %+v", p.Scan)
	}
	rows := scanRows(p)
	if len(rows) != 1 || rows[0].Job.Primary.Artifact.ID != other.ID || len(p.Rows) != 2 {
		t.Errorf("rows %+v", p.Rows)
	}
}

func TestScanHashesEachCandidateOnce(t *testing.T) {
	f := newRx(t)
	cand := putFile(t, f.c, "D1", "A1", "/sdcard/a.db", magic)
	opens := map[string]int{}
	h := f.host(func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(id string) { opens[id]++ })
	}, sniffer("a", magic), sniffer("b", magic), sniffer("c", magic))
	p := scanPlan(t, h, artparse.Selection{})
	if len(scanRows(p)) != 3 {
		t.Fatalf("every parser probes the candidate: %+v", p.Rows)
	}
	if opens[cand.ID] != 1 {
		t.Errorf("the candidate was opened and hashed %d times, want once for three parsers", opens[cand.ID])
	}
}

func TestScanCountsOversizeArtifacts(t *testing.T) {
	f := newRx(t)
	big := putFile(t, f.c, "D1", "A1", "/sdcard/big.bin", magic+strings.Repeat("x", 2000))
	small := putFile(t, f.c, "D1", "A1", "/sdcard/small.bin", magic)
	opens := map[string]int{}
	h := f.host(func(o *artparse.Options) {
		artparse.WithScanMaxSize(o, 1024)
		artparse.WithOnOpen(o, func(id string) { opens[id]++ })
	}, sniffer("a", magic))
	p := scanPlan(t, h, artparse.Selection{})
	if sc(t, p).TooLarge != 1 || sc(t, p).Considered != 2 || sc(t, p).Probed != 1 {
		t.Fatalf("scan counts %+v", p.Scan)
	}
	if opens[big.ID] != 0 {
		t.Errorf("an oversize artifact was opened %d times", opens[big.ID])
	}
	if rows := scanRows(p); len(rows) != 1 || rows[0].Job.Primary.Artifact.ID != small.ID {
		t.Errorf("rows %+v", p.Rows)
	}
}

func TestScanListsProbeFailuresAsFindings(t *testing.T) {
	build := func(mod func(*artparse.Options)) artparse.Plan {
		f := newRx(t)
		putFile(t, f.c, "D1", "A1", "/sdcard/odd.db", "x")
		h := hostWith(t, f.c, func(o *artparse.Options) {
			fastLimits(o)
			if mod != nil {
				mod(o)
			}
		},
			regFake(t, parsertest.ProbeOnly{Name: "errs", Version: "1.0.0", Err: errors.New("disk on fire")}, "errs"),
			regFake(t, parsertest.Panicking{Name: "panics", Version: "1.0.0", Where: parsertest.PanicInProbe, Value: "kaboom"}, "panics"),
			regFake(t, parsertest.ProbeOnly{Name: "uns", Version: "1.0.0", Status: parse.UnsupportedSchema, Reason: "no table sms"}, "uns"),
		)
		return scanPlan(t, h, artparse.Selection{})
	}
	p := build(nil)
	if len(p.Rows) != 0 || sc(t, p).Probed != 1 || sc(t, p).NotApplicable != 0 {
		t.Fatalf("a probe failure is never a row: rows %+v counts %+v", p.Rows, p.Scan)
	}
	want := []struct{ parser, status, reason string }{
		{"errs", "probe-failed", "disk on fire"}, {"panics", "probe-failed", "kaboom"}, {"uns", parse.UnsupportedSchema, "no table sms"},
	}
	if len(p.ScanFindings) != len(want) {
		t.Fatalf("findings %+v", p.ScanFindings)
	}
	for i, w := range want {
		g := p.ScanFindings[i]
		if g.Parser != w.parser || g.Status != w.status || !strings.Contains(g.Reason, w.reason) || g.ArtifactID == "" || !strings.HasSuffix(g.Path, "odd.db") {
			t.Errorf("finding %d = %+v, want %+v", i, g, w)
		}
	}
	p = build(func(o *artparse.Options) { artparse.WithMaxScanFindings(o, 2) })
	if len(p.ScanFindings) != 2 || sc(t, p).FindingsDropped != 1 {
		t.Errorf("past the bound: %d findings kept, %d dropped, want 2 and 1", len(p.ScanFindings), sc(t, p).FindingsDropped)
	}
}

func TestScanCountsNotApplicable(t *testing.T) {
	f := newRx(t)
	putFile(t, f.c, "D1", "A1", "/sdcard/photo.jpg", "\xff\xd8\xff\xe0 not a database")
	h := f.host(nil, sniffer("a", magic), sniffer("b", magic), sniffer("c", magic))
	p := scanPlan(t, h, artparse.Selection{})
	if len(p.Rows) != 0 || len(p.ScanFindings) != 0 || sc(t, p).NotApplicable != 3 || sc(t, p).Probed != 1 || sc(t, p).Candidates != 0 {
		t.Errorf("rows %+v findings %+v counts %+v", p.Rows, p.ScanFindings, p.Scan)
	}
}

func TestScanStopsAfterAnAbandonedProbe(t *testing.T) {
	f := newRx(t)
	for _, n := range []string{"/sdcard/1.db", "/sdcard/2.db", "/sdcard/3.db"} {
		putFile(t, f.c, "D1", "A1", n, magic)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var after atomic.Int32
	h := hostWith(t, f.c, fastLimits,
		regFake(t, &hangProbe{name: "hangs", release: release}, "hangs"),
		regFake(t, &countingProbe{name: "after", n: &after}, "after"))
	p := scanPlan(t, h, artparse.Selection{})
	if sc(t, p).Stopped != artparse.StoppedAbandoned || sc(t, p).NotProbed != 2 || sc(t, p).Probed != 1 {
		t.Errorf("counts %+v", p.Scan)
	}
	if after.Load() != 0 {
		t.Errorf("a parser probed after an abandoned Probe (%d)", after.Load())
	}
	if len(p.Rows) != 0 || len(p.ScanFindings) != 1 || p.ScanFindings[0].Status != "probe-failed" {
		t.Errorf("rows %+v findings %+v", p.Rows, p.ScanFindings)
	}
}

func TestScanStopsOnContextCancel(t *testing.T) {
	f := newRx(t)
	for _, n := range []string{"/sdcard/1.db", "/sdcard/2.db", "/sdcard/3.db", "/sdcard/4.db"} {
		putFile(t, f.c, "D1", "A1", n, magic)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opens := 0
	h := f.host(func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(string) {
			opens++
			if opens == 2 {
				cancel()
			}
		})
	}, sniffer("a", magic))
	p, err := h.Plan(ctx, artparse.Selection{Scan: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan = %v, want the context error", err)
	}
	// candidate 1 was probed and keeps its row; candidate 2 was interrupted while it was read, 3 and 4 never started
	if len(scanRows(p)) != 1 || sc(t, p).Probed != 1 || sc(t, p).Stopped != artparse.StoppedCancelled || sc(t, p).NotProbed != 3 || sc(t, p).Considered != 4 {
		t.Errorf("rows %d counts %+v", len(p.Rows), p.Scan)
	}
}

func TestScanCandidateIntegrityFailure(t *testing.T) {
	f := newRx(t)
	bad := putFile(t, f.c, "D1", "A1", "/sdcard/1.db", magic+"original")
	good := putFile(t, f.c, "D1", "A1", "/sdcard/2.db", magic)
	file := filepath.Join(f.c.Dir, filepath.FromSlash(bad.Path))
	if err := os.WriteFile(file, []byte(magic+"tampered"), 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Fatal(err)
	}
	h := f.host(nil, sniffer("a", magic))
	p := scanPlan(t, h, artparse.Selection{})
	rows := scanRows(p)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", p.Rows)
	}
	byID := map[string]artparse.PlanRow{}
	for _, r := range rows {
		byID[r.Job.Primary.Artifact.ID] = r
	}
	if r := byID[bad.ID]; r.Status != artparse.StatusRefused || !r.Integrity || !strings.HasPrefix(r.Reason, "integrity:") {
		t.Errorf("tampered candidate %+v", r)
	}
	if r := byID[good.ID]; r.Status != artparse.StatusCandidate || r.Integrity {
		t.Errorf("the scan must go on after an integrity failure: %+v", r)
	}
}

func TestScanIsAPlanOption(t *testing.T) {
	f := newRx(t, "a")
	h := f.host(nil, sniffer("a", magic))
	if _, err := h.Run(context.Background(), artparse.RunOptions{Selection: scanSel}); !errors.Is(err, artparse.ErrSelection) {
		t.Errorf("Run with Scan = %v, want ErrSelection", err)
	}
	if _, err := h.RecoverTyped(context.Background(), artparse.RecoverOptions{Selection: scanSel}); !errors.Is(err, artparse.ErrSelection) {
		t.Errorf("RecoverTyped with Scan = %v, want ErrSelection", err)
	}
	if _, err := h.Plan(context.Background(), artparse.Selection{Scan: true, Artifacts: []string{f.recs["a"].ID}}); !errors.Is(err, artparse.ErrSelection) {
		t.Errorf("Plan with Scan and Artifacts = %v, want ErrSelection", err)
	}
}

func TestScanNeverParses(t *testing.T) {
	f := newRx(t)
	putFile(t, f.c, "D1", "A1", "/sdcard/a.db", magic)
	p := sniffer("a", magic)
	h := f.host(nil, p)
	if plan := scanPlan(t, h, artparse.Selection{}); len(scanRows(plan)) != 1 {
		t.Fatalf("rows %+v", plan.Rows)
	}
	if n := p.parses.Load(); n != 0 {
		t.Errorf("Parse was called %d times by a scan", n)
	}
	aud, err := f.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range aud {
		if strings.HasPrefix(e.Action, "parse.") || strings.HasPrefix(e.Action, "records.") {
			t.Errorf("a scan wrote the audit entry %s", e.Action)
		}
	}
}

func TestScanFindsChromiumHistoryOnIOS(t *testing.T) {
	dir := filepath.Join("..", "parsers", "chromium", "testdata")
	fx := parsertest.LoadFixture(t, dir, "current")
	c := newCase(t)
	acquireStart(t, c, "IOS1", "A1", "ios.backup")
	src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: "/private/var/mobile/Containers/Data/Application/X/Library/Chrome/History"}
	rec, err := c.Capture("IOS1", "A1", "files/chrome-history", src, func(w io.Writer) error {
		_, err := w.Write(fx.Files["db"])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h := hostWith(t, c, nil, regFake(t, chromium.Parser{}, chromium.Name))
	if p := planOf(t, h, artparse.Selection{}); len(p.Rows) != 0 {
		t.Fatalf("no glob matches the iOS path: %+v", p.Rows)
	}
	p := scanPlan(t, h, artparse.Selection{})
	rows := scanRows(p)
	if len(rows) != 1 || rows[0].Job.Primary.Artifact.ID != rec.ID || rows[0].Job.Parser.Meta().Name != chromium.Name || rows[0].Status != artparse.StatusCandidate {
		t.Fatalf("rows %+v findings %+v", p.Rows, p.ScanFindings)
	}
	if rows[0].Job.Primary.Platform != parse.PlatformIOS {
		t.Errorf("platform %q", rows[0].Job.Primary.Platform)
	}
}

// sc is the scan counts of p; a plan without them fails the test.
func sc(t testing.TB, p artparse.Plan) artparse.ScanCounts {
	t.Helper()
	if p.Scan == nil {
		t.Fatalf("the plan holds no scan counts: %+v", p.Rows)
	}
	return *p.Scan
}
