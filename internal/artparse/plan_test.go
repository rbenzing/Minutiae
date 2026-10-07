package artparse_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// regFake registers a parsertest-style fake (its Meta is parsertest.FakeMeta).
func regFake(t testing.TB, p parse.Parser, name string) artparse.Registered {
	t.Helper()
	r, err := artparse.Register(p, hashFor(name))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func fakePath(name string) string { return "/data/x/parsertest/" + name + ".dat" }

func planOf(t testing.TB, h *artparse.Host, sel artparse.Selection) artparse.Plan {
	t.Helper()
	p, err := h.Plan(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rowOf(t testing.TB, p artparse.Plan, parser string) artparse.PlanRow {
	t.Helper()
	for _, r := range p.Rows {
		if r.Job.Parser.Meta().Name == parser {
			return r
		}
	}
	t.Fatalf("no row for parser %s in %+v", parser, p.Rows)
	return artparse.PlanRow{}
}

func hostWith(t testing.TB, c *evidence.Case, mod func(*artparse.Options), ps ...artparse.Registered) *artparse.Host {
	t.Helper()
	opt := artparse.Options{Limits: parse.DefaultLimits()}
	if mod != nil {
		mod(&opt)
	}
	h, err := artparse.New(c, ps, opt)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// countingProbe counts its Probe calls.
type countingProbe struct {
	name string
	n    *atomic.Int32
}

func (p *countingProbe) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *countingProbe) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	p.n.Add(1)
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p *countingProbe) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func TestPlanStatuses(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	names := []string{"fresh", "done", "stale", "enc", "uns", "skip", "bad"}
	recs := map[string]evidence.ManifestRecord{}
	for _, n := range names {
		recs[n] = putFile(t, c, "D1", "A1", fakePath(n), "content of "+n)
	}
	// an encrypted extract is its own artifact
	d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: fakePath("encx"), Encrypted: true}
	put(t, c, "D1", "A1", "encx.dat", evidence.Source{Kind: "extract", RemotePath: fakePath("encx"), Derived: d}, "ciphertext")

	var probed atomic.Int32
	ps := map[string]parse.Parser{
		"fresh": parsertest.ProbeOnly{Name: "fresh", Version: "1.0.0", Status: parse.Applicable},
		"done":  parsertest.ProbeOnly{Name: "done", Version: "1.0.0", Status: parse.Applicable},
		"stale": walParser{name: "stale"},
		"enc":   parsertest.ProbeOnly{Name: "enc", Version: "1.0.0", Status: parse.Encrypted, Reason: "database is encrypted"},
		"uns":   parsertest.ProbeOnly{Name: "uns", Version: "1.0.0", Status: parse.UnsupportedSchema, Reason: "no table sms"},
		"skip":  parsertest.ProbeOnly{Name: "skip", Version: "1.0.0", Status: parse.NotApplicable, Reason: "not a message store"},
		"bad":   parsertest.ProbeOnly{Name: "bad", Version: "1.0.0", Status: parse.Applicable},
		"encx":  &countingProbe{name: "encx", n: &probed},
	}
	order := append(append([]string(nil), names...), "encx")
	var regs []artparse.Registered
	byName := map[string]artparse.Registered{}
	for _, n := range order {
		r := regFake(t, ps[n], n)
		regs = append(regs, r)
		byName[n] = r
	}
	ingest := func(n string, ids ...string) {
		rec := []records.Record{{Type: message.Type, ArtifactID: recs[n].ID, Summary: "s", Payload: recordstest.ValidPayload(message.Type)}}
		recordstest.Ingest(t, c, byName[n].Identity(), ids, rec)
	}
	ingest("done", recs["done"].ID)
	ingest("stale", recs["stale"].ID)
	putFile(t, c, "D1", "A1", fakePath("stale")+"-wal", "wal added later")
	// flip one byte of "bad" on disk, same size
	file := filepath.Join(c.Dir, filepath.FromSlash(recs["bad"].Path))
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil { //nolint:gosec // writes inside a temporary test case
		t.Fatal(err)
	}

	p := planOf(t, hostWith(t, c, nil, regs...), artparse.Selection{})
	want := map[string]struct{ status, reasonHas string }{
		"fresh": {artparse.StatusNew, ""},
		"done":  {artparse.StatusAlreadyParsed, ""},
		"stale": {artparse.StatusStaleBundle, ""},
		"enc":   {artparse.StatusUnparsed, "encrypted: database is encrypted"},
		"uns":   {artparse.StatusUnparsed, "unsupported-schema: no table sms"},
		"skip":  {artparse.StatusSkipped, "not a message store"},
		"bad":   {artparse.StatusRefused, "integrity"},
		"encx":  {artparse.StatusUnparsed, "encrypted content (sub-project 10)"},
	}
	for n, w := range want {
		row := rowOf(t, p, n)
		if row.Status != w.status || !strings.Contains(row.Reason, w.reasonHas) {
			t.Errorf("%s: status %q reason %q, want %q containing %q", n, row.Status, row.Reason, w.status, w.reasonHas)
		}
		if row.Job.Primary.Logical == "" || row.Job.Primary.Platform != parse.PlatformAndroid || row.Job.Primary.Namer == "" {
			t.Errorf("%s: row lacks logical path, platform or namer: %+v", n, row.Job.Primary)
		}
	}
	if !rowOf(t, p, "bad").Integrity || rowOf(t, p, "fresh").Integrity {
		t.Error("only the byte-flipped artifact is an integrity refusal")
	}
	if got := rowOf(t, p, "fresh").Job.Primary.Logical; got != "android:"+fakePath("fresh") {
		t.Errorf("logical path %q", got)
	}
	// every row is counted under its status; the sum is the number of rows
	total := 0
	for _, n := range p.ByStatus {
		total += n
	}
	if total != len(p.Rows) || len(p.Rows) != len(order) {
		t.Errorf("ByStatus %v does not account for %d rows", p.ByStatus, len(p.Rows))
	}
	if p.ByStatus[artparse.StatusUnparsed] != 3 || p.ByStatus[artparse.StatusNew] != 1 {
		t.Errorf("ByStatus = %v", p.ByStatus)
	}
	if probed.Load() != 0 {
		t.Errorf("Probe ran %d times for an encrypted extract", probed.Load())
	}
}

func TestPlanEncryptedExtractSkipsProbe(t *testing.T) {
	c := newCase(t)
	d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: fakePath("e"), Encrypted: true}
	put(t, c, "D1", "A1", "e.dat", evidence.Source{Kind: "extract", RemotePath: fakePath("e"), Derived: d}, "ciphertext")
	var n atomic.Int32
	p := planOf(t, hostWith(t, c, nil, regFake(t, &countingProbe{name: "e", n: &n}, "e")), artparse.Selection{})
	if len(p.Rows) != 1 || p.Rows[0].Status != artparse.StatusUnparsed || p.Rows[0].Reason != "encrypted content (sub-project 10)" {
		t.Fatalf("rows %+v", p.Rows)
	}
	if n.Load() != 0 {
		t.Fatalf("Probe was called %d times", n.Load())
	}
}

// caseFingerprint hashes every file of the case directory, by relative path.
func caseFingerprint(t testing.TB, c *evidence.Case) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "case.lock" { // the OS lock file holds no evidence and cannot be read while held
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // reads files of a temporary test case
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.Dir, p)
		out[filepath.ToSlash(rel)] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlanAppendsNothingToAudit(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	a := putFile(t, c, "D1", "A1", fakePath("a"), "aaaa")
	putFile(t, c, "D1", "A1", fakePath("b"), "bbbb")
	reg := regFake(t, parsertest.ProbeOnly{Name: "a", Version: "1.0.0", Status: parse.Applicable}, "a")
	regB := regFake(t, parsertest.ProbeOnly{Name: "b", Version: "1.0.0", Status: parse.NotApplicable, Reason: "r"}, "b")
	recordstest.Ingest(t, c, reg.Identity(), []string{a.ID},
		[]records.Record{{Type: message.Type, ArtifactID: a.ID, Summary: "s", Payload: recordstest.ValidPayload(message.Type)}})
	before := caseFingerprint(t, c)
	auditBefore, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	p := planOf(t, hostWith(t, c, nil, reg, regB), artparse.Selection{})
	if len(p.Rows) != 2 {
		t.Fatalf("rows %+v", p.Rows)
	}
	after := caseFingerprint(t, c)
	if len(before) != len(after) {
		t.Errorf("files before %d, after %d", len(before), len(after))
	}
	for f, h := range before {
		if after[f] != h {
			t.Errorf("%s changed by Plan", f)
		}
	}
	auditAfter, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore) {
		t.Errorf("audit entries before %d, after %d", len(auditBefore), len(auditAfter))
	}
}

// Plan and Run share one front half: every call of openBundle and of a parser's Probe sits in
// prepareJob, and Plan (and Run, once it exists) reaches the artifacts through it.
func TestPlanAndRunShareOneFrontHalf(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	calls := map[string]map[string]bool{} // function -> names it calls
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			set := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					switch fn := ce.Fun.(type) {
					case *ast.Ident:
						set[fn.Name] = true
					case *ast.SelectorExpr:
						set[fn.Sel.Name] = true
					}
				}
				return true
			})
			calls[fd.Name.Name] = set
		}
	}
	for _, name := range []string{"openBundle", "Probe"} {
		for fn, set := range calls {
			if set[name] && fn != "prepareJob" {
				t.Errorf("%s calls %s: only prepareJob may", fn, name)
			}
		}
		if !calls["prepareJob"][name] {
			t.Errorf("prepareJob does not call %s", name)
		}
	}
	if _, ok := calls["Plan"]; !ok {
		t.Fatal("Plan not found")
	}
	for _, fn := range []string{"Plan", "Run"} {
		set, ok := calls[fn]
		if !ok {
			t.Logf("%s does not exist yet (Task 17 must call prepareJob and nothing else of the front half)", fn)
			continue
		}
		if !set["prepareJob"] {
			t.Errorf("%s does not call prepareJob", fn)
		}
	}
}

// hangProbe's Probe ignores its context and blocks until released.
type hangProbe struct {
	name    string
	release chan struct{}
}

func (p *hangProbe) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *hangProbe) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	<-p.release
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p *hangProbe) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func fastLimits(o *artparse.Options) {
	artparse.SkipLimitMinimums(o)
	o.Limits.ProbeTimeout = 150 * time.Millisecond
	o.Limits.GracePeriod = 20 * time.Millisecond
}

func TestPlanProbeRunsInSandbox(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	for _, n := range []string{"panics", "hangs", "errs", "ok"} {
		putFile(t, c, "D1", "A1", fakePath(n), "x")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	regs := []artparse.Registered{
		regFake(t, parsertest.Panicking{Name: "panics", Version: "1.0.0", Where: parsertest.PanicInProbe, Value: "kaboom"}, "panics"),
		regFake(t, &hangProbe{name: "hangs", release: release}, "hangs"),
		regFake(t, parsertest.ProbeOnly{Name: "errs", Version: "1.0.0", Err: errors.New("disk on fire")}, "errs"),
		regFake(t, parsertest.ProbeOnly{Name: "ok", Version: "1.0.0", Status: parse.Applicable}, "ok"),
	}
	p := planOf(t, hostWith(t, c, fastLimits, regs...), artparse.Selection{})
	for n, frag := range map[string]string{"panics": "kaboom", "hangs": "", "errs": "disk on fire"} {
		r := rowOf(t, p, n)
		if r.Status != artparse.StatusUnparsed || !strings.HasPrefix(r.Reason, "probe failed") || !strings.Contains(r.Reason, frag) {
			t.Errorf("%s: %q %q", n, r.Status, r.Reason)
		}
	}
	if r := rowOf(t, p, "ok"); r.Status != artparse.StatusNew {
		t.Errorf("a failing neighbour changed the healthy job: %q %q", r.Status, r.Reason)
	}
}

func TestPlanProbeReasonIsCleaned(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("r"), "x")
	reason := "bad\x00text\xff" + strings.Repeat("y", 5000)
	p := planOf(t, hostWith(t, c, nil, regFake(t, parsertest.ProbeOnly{Name: "r", Version: "1.0.0", Status: parse.NotApplicable, Reason: reason}, "r")), artparse.Selection{})
	if len(p.Rows) != 1 {
		t.Fatalf("rows %+v", p.Rows)
	}
	got := p.Rows[0].Reason
	if strings.ContainsRune(got, 0) || strings.Contains(got, "\xff") || len(got) > 1100 {
		t.Errorf("reason not cleaned: %d bytes %q", len(got), got[:20])
	}
}

// bigReader reads the whole primary in 4 KiB steps and reports how much it got.
type bigReader struct {
	got *atomic.Int64
	err *atomic.Value
}

func (p bigReader) Meta() parse.Meta { return parsertest.FakeMeta("big", "1.0.0") }
func (p bigReader) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	buf := make([]byte, 4096)
	for off := int64(0); ; off += int64(len(buf)) {
		n, err := in.Primary.R.ReadAt(buf, off)
		p.got.Add(int64(n))
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.err.Store(err)
			}
			break
		}
	}
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p bigReader) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func TestPlanProbeSeesOnlyProbeBytes(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("big"), strings.Repeat("z", 40000))
	var got atomic.Int64
	var rerr atomic.Value
	h := hostWith(t, c, func(o *artparse.Options) { o.Limits.ProbeBytes = 8192 }, regFake(t, bigReader{&got, &rerr}, "big"))
	p := planOf(t, h, artparse.Selection{})
	if len(p.Rows) != 1 || p.Rows[0].Status != artparse.StatusNew {
		t.Fatalf("rows %+v", p.Rows)
	}
	if got.Load() > 8192 || got.Load() == 0 {
		t.Errorf("Probe read %d bytes, limit 8192", got.Load())
	}
	if e, _ := rerr.Load().(error); !errors.Is(e, parse.ErrProbeLimit) {
		t.Errorf("read past the limit gave %v, want ErrProbeLimit", e)
	}
}

func TestPlanRefusesPayloadMismatchAndMissingContract(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("pv"), "x")
	putFile(t, c, "D1", "A1", fakePath("nc"), "x")
	good := hostWith(t, c, nil)
	pv := artparse.WithEmits(regFake(t, parsertest.ProbeOnly{Name: "pv", Version: "1.0.0", Status: parse.Applicable}, "pv"),
		[]parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion + 1}})
	nc := artparse.WithEmits(regFake(t, parsertest.ProbeOnly{Name: "nc", Version: "1.0.0", Status: parse.Applicable}, "nc"),
		[]parse.Emit{{Type: "no_such_type", PayloadVersion: 1}})
	artparse.SetRegistry(good, pv, nc)
	p := planOf(t, good, artparse.Selection{})
	for n, want := range map[string]string{"pv": "payload-version", "nc": "no-payload-contract"} {
		r := rowOf(t, p, n)
		if r.Status != artparse.StatusRefused || !strings.Contains(r.Reason, want) {
			t.Errorf("%s: %q %q, want refused %s", n, r.Status, r.Reason, want)
		}
		if r.Integrity {
			t.Errorf("%s: a payload refusal is not an integrity failure", n)
		}
	}
}

func TestPlanCancelledReturnsTheError(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("a"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := hostWith(t, c, nil, regFake(t, parsertest.ProbeOnly{Name: "a", Version: "1.0.0", Status: parse.Applicable}, "a"))
	if _, err := h.Plan(ctx, artparse.Selection{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan with a cancelled context = %v", err)
	}
}

// Discovery counts reach the plan: nothing the host skipped is silent.
func TestPlanCarriesDiscoveryCounts(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("a"), "x")
	put(t, c, "D1", "A1", "info.txt", evidence.Source{Kind: "info", RemotePath: "exec:getprop"}, "i")
	p := planOf(t, hostWith(t, c, nil, regFake(t, parsertest.ProbeOnly{Name: "a", Version: "1.0.0", Status: parse.Applicable}, "a")), artparse.Selection{})
	if p.Counts.Manifest != 2 || p.Counts.NotParserInput != 1 {
		t.Errorf("counts %+v", p.Counts)
	}
}

// walParser is a parser with a primary and a required-free "wal" role (a companion file).
type walParser struct{ name string }

func (p walParser) Meta() parse.Meta {
	m := parsertest.FakeMeta(p.name, "1.0.0")
	m.Inputs = []parse.InputSpec{
		{Role: "primary", Globs: []string{"android:**/parsertest/" + p.name + ".dat"}, Required: true},
		{Role: "wal", Companions: []string{"-wal"}},
	}
	return m
}

func (p walParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p walParser) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }
