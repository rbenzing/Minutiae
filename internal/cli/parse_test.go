package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const pData = "alpha\nbeta\ngamma\n"

type regFn = func() ([]artparse.Registered, error)

// cliParser is a well-behaved fake whose Probe and Parse can be replaced.
type cliParser struct {
	parsertest.WellBehaved
	probe func(context.Context, *parse.Input) (parse.Applicability, error)
	parse func(context.Context, *parse.Input, parse.Emitter) error
	wal   bool
	glob  string
}

func (p cliParser) Meta() parse.Meta {
	m := parsertest.FakeMeta(p.Name, p.Version)
	if p.glob != "" {
		m.Inputs[0].Globs = []string{p.glob}
	}
	if p.wal {
		m.Inputs = append(m.Inputs, parse.InputSpec{Role: "wal", Companions: []string{"-wal"}})
	}
	return m
}

func (p cliParser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if p.probe != nil {
		return p.probe(ctx, in)
	}
	return p.WellBehaved.Probe(ctx, in)
}

func (p cliParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if p.parse != nil {
		return p.parse(ctx, in, out)
	}
	return p.WellBehaved.Parse(ctx, in, out)
}

func good(name string) cliParser {
	return cliParser{WellBehaved: parsertest.WellBehaved{Name: name, Version: "1.0.0"}}
}

func registry(t *testing.T, salt string, ps ...parse.Parser) regFn {
	t.Helper()
	return func() ([]artparse.Registered, error) {
		var out []artparse.Registered
		for _, p := range ps {
			m := p.Meta()
			r, err := artparse.Register(p, parsertest.FakeHash(m.Name+"@"+m.Version+salt))
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
}

type pfx struct {
	t    *testing.T
	dir  string
	recs map[string]evidence.ManifestRecord
}

func ppath(name string) string { return "/data/x/parsertest/" + name + ".dat" }

func newPfx(t *testing.T, names ...string) *pfx {
	t.Helper()
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "logical"}); err != nil {
		t.Fatal(err)
	}
	p := &pfx{t: t, dir: c.Dir, recs: map[string]evidence.ManifestRecord{}}
	for _, n := range names {
		p.recs[n] = pcapture(t, c, "files/"+n+".dat", evidence.Source{Kind: "file", RemotePath: ppath(n)}, pData)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func pcapture(t *testing.T, c *evidence.Case, rel string, src evidence.Source, data string) evidence.ManifestRecord {
	t.Helper()
	src.DeviceID = "D1"
	rec, err := c.Capture("D1", "A1", rel, src, func(w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// edit opens the case, applies fn and closes it.
func (p *pfx) edit(fn func(c *evidence.Case)) {
	p.t.Helper()
	c, err := evidence.Open(p.dir)
	if err != nil {
		p.t.Fatal(err)
	}
	fn(c)
	if err := c.Close(); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pfx) file(name string) string {
	return filepath.Join(p.dir, filepath.FromSlash(p.recs[name].Path))
}

func (p *pfx) flip(name string) {
	p.t.Helper()
	b, err := os.ReadFile(p.file(name))
	if err != nil {
		p.t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(p.file(name), b, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		p.t.Fatal(err)
	}
}

func fastGrace(t *testing.T) {
	t.Helper()
	parseLimitsHook = func(l *parse.Limits) { l.GracePeriod = 50 * time.Millisecond }
	t.Cleanup(func() { parseLimitsHook = nil })
}

func runP(t *testing.T, reg regFn, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	d := Deps{In: strings.NewReader(""), Out: &o, Err: &e, Registry: device.NewRegistry(), ParserRegistry: reg}
	code = Run(args, d)
	return code, o.String(), e.String()
}

func rawControl(s string) (rune, bool) {
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return r, true
		}
	}
	return 0, false
}

// dirDigest hashes every file of the case (the audit log optionally excluded).
func dirDigest(t *testing.T, dir string, skipAudit bool) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if skipAudit && d.Name() == "audit.jsonl" {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // inside a temporary test case
		if err != nil {
			return nil // the lock file may be unreadable while held
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func auditLineCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// listing

func TestParseListTextAndJSON(t *testing.T) {
	reg := registry(t, "", good("alpha"), good("beta"))
	code, out, _ := runP(t, reg, "parse", "list")
	if code != 0 || !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") || !strings.Contains(out, "1.0.0") ||
		!strings.Contains(out, "message@v1") || !strings.Contains(out, "android") || !strings.Contains(out, "src1:sha256:") || !strings.Contains(out, "parsertest/alpha.dat") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var arr []struct {
		Name      string   `json:"name"`
		Version   string   `json:"version"`
		Hash      string   `json:"hash"`
		Platforms []string `json:"platforms"`
		Emits     []struct {
			Type           string `json:"type"`
			PayloadVersion int    `json:"payload_version"`
		} `json:"emits"`
		Inputs []struct {
			Role  string   `json:"role"`
			Globs []string `json:"globs"`
		} `json:"inputs"`
	}
	code, out, _ = runP(t, reg, "parse", "list", "--json")
	if err := json.Unmarshal([]byte(out), &arr); code != 0 || err != nil || len(arr) != 2 {
		t.Fatalf("exit %d err %v:\n%s", code, err, out)
	}
	a := arr[0]
	if a.Name != "alpha" || a.Version != "1.0.0" || !strings.HasPrefix(a.Hash, "src1:sha256:") || a.Platforms[0] != "android" ||
		a.Emits[0].Type != "message" || a.Emits[0].PayloadVersion != 1 || a.Inputs[0].Role != "primary" || len(a.Inputs[0].Globs) != 1 {
		t.Errorf("entry %+v", a)
	}
	_, out, _ = runP(t, reg, "parse", "list", "--platform", "ios")
	if strings.Contains(out, "alpha") || !strings.Contains(out, "no parsers") {
		t.Errorf("--platform ios listed android parsers:\n%s", out)
	}
	_, out, _ = runP(t, reg, "parse", "list", "--platform", "android", "--json")
	if err := json.Unmarshal([]byte(out), &arr); err != nil || len(arr) != 2 {
		t.Errorf("--platform android: %v\n%s", err, out)
	}
	if code, _, _ = runP(t, reg, "parse", "list", "--platform", "windows"); code != ExitUsage {
		t.Errorf("--platform windows: exit %d", code)
	}
	code, out, _ = runP(t, nil, "parse", "list")
	if code != 0 || !strings.Contains(out, "no parsers are compiled in") || !strings.Contains(out, "NAME") {
		t.Errorf("empty registry: exit %d\n%s", code, out)
	}
	_, out, _ = runP(t, nil, "parse", "list", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty registry JSON: %q", out)
	}
}

func TestParseListAppendsNothing(t *testing.T) {
	p := newPfx(t, "a")
	before := dirDigest(t, p.dir, false)
	if code, _, _ := runP(t, registry(t, "", good("a")), "parse", "list"); code != 0 {
		t.Fatal(code)
	}
	if code, _, _ := runP(t, registry(t, "", good("a")), "parse", "list", "--case", p.dir); code != ExitUsage {
		t.Errorf("--case on list: exit %d, want 2 (unknown flag: list opens no case)", code)
	}
	if after := dirDigest(t, p.dir, false); after != before {
		t.Error("parse list changed the case")
	}
}

// plan

func TestParsePlanAppendsNothing(t *testing.T) {
	p := newPfx(t, "a", "b")
	reg := registry(t, "", good("a"), good("b"))
	before, lines := dirDigest(t, p.dir, true), auditLineCount(t, p.dir)
	code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if got := auditLineCount(t, p.dir); got != lines+1 {
		t.Errorf("audit grew by %d lines, want exactly 1 (case.open)", got-lines)
	}
	if after := dirDigest(t, p.dir, true); after != before {
		t.Error("parse plan changed an artifact, the manifest or artifacts.db")
	}
	b, _ := os.ReadFile(filepath.Join(p.dir, "audit.jsonl"))
	if !bytes.Contains(b[bytes.LastIndex(b[:len(b)-1], []byte("\n")):], []byte("case.open")) {
		t.Error("the one new audit line is not case.open")
	}
}

func TestParsePlanTextAndJSON(t *testing.T) {
	p := newPfx(t, "a", "w")
	p.edit(func(c *evidence.Case) {
		pcapture(t, c, "files/w.dat-wal", evidence.Source{Kind: "file", RemotePath: ppath("w") + "-wal"}, "wal")
	})
	wp := good("w")
	wp.wal = true
	reg := registry(t, "", good("a"), wp)
	code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	for _, want := range []string{"new", "a@1.0.0", "w@1.0.0", p.recs["a"].ID, ppath("a"), "android", "wal="} {
		if !strings.Contains(out, want) {
			t.Errorf("text plan lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "note: discovery trusts the manifest's source fields; run `case verify` first to check them against the audit log") {
		t.Errorf("stderr %q", errOut)
	}
	var pl struct {
		Rows []struct {
			Status     string `json:"status"`
			Parser     string `json:"parser"`
			Version    string `json:"version"`
			ArtifactID string `json:"artifact_id"`
			Logical    string `json:"logical_path"`
			Platform   string `json:"platform"`
			Namer      string `json:"namer"`
			Bundle     []struct {
				Role       string `json:"role"`
				ArtifactID string `json:"artifact_id"`
			} `json:"bundle"`
		} `json:"rows"`
		Counts struct {
			Manifest int            `json:"manifest"`
			ByStatus map[string]int `json:"by_status"`
		} `json:"counts"`
	}
	code, out, _ = runP(t, reg, "parse", "plan", "--case", p.dir, "--json")
	if err := json.Unmarshal([]byte(out), &pl); code != 0 || err != nil || len(pl.Rows) != 2 {
		t.Fatalf("exit %d err %v:\n%s", code, err, out)
	}
	r := pl.Rows[0]
	if r.Status != "new" || r.Parser != "a" || r.Version != "1.0.0" || r.ArtifactID != p.recs["a"].ID || r.Logical != "android:"+ppath("a") || r.Platform != "android" || r.Namer == "" {
		t.Errorf("row %+v", r)
	}
	if len(pl.Rows[1].Bundle) != 2 || pl.Counts.Manifest != 3 || pl.Counts.ByStatus["new"] != 2 {
		t.Errorf("w row %+v counts %+v", pl.Rows[1], pl.Counts)
	}
}

// run

var runStatusRE = regexp.MustCompile(`^\[1/1\] a 1\.0\.0 [0-9a-f]+ \.\.\. 3 records, 2 warnings, [0-9.]+ s$`)

func TestParseRunTextAndJSON(t *testing.T) {
	p := newPfx(t)
	p.edit(func(c *evidence.Case) {
		p.recs["a"] = pcapture(t, c, "files/a.dat", evidence.Source{Kind: "file", RemotePath: ppath("a")}, "x\n\n\ny\nz\n")
	})
	reg := registry(t, "", good("a"))
	code, out, errOut := runP(t, reg, "parse", "run", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var found bool
	for _, l := range strings.Split(errOut, "\n") {
		if runStatusRE.MatchString(l) {
			found = true
		}
	}
	if !found {
		t.Errorf("no status line on stderr:\n%s", errOut)
	}
	if !strings.Contains(out, "complete") || !strings.Contains(out, "3 records") {
		t.Errorf("summary:\n%s", out)
	}

	p2 := newPfx(t, "a")
	code, out, errOut = runP(t, reg, "parse", "run", "--case", p2.dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var core []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("stdout line is not a JSON object: %q", sc.Text())
		}
		if k := ev["event"].(string); k != "job.progress" {
			core = append(core, k)
		}
	}
	if got := strings.Join(core, ","); got != "run.start,job.start,job.end,summary" {
		t.Errorf("events %s", got)
	}
	if !strings.Contains(errOut, "[1/1] a 1.0.0") {
		t.Errorf("stderr lacks the status line in --json mode:\n%s", errOut)
	}
}

func TestParseRunProgressThrottled(t *testing.T) {
	p := newPfx(t, "a")
	pr := good("a")
	pr.parse = func(_ context.Context, _ *parse.Input, out parse.Emitter) error {
		for i := int64(1); i <= 20000; i++ {
			out.Progress(i, 20000)
		}
		return nil
	}
	_, out, _ := runP(t, registry(t, "", pr), "parse", "run", "--case", p.dir, "--json")
	if n := strings.Count(out, `"event":"job.progress"`); n > 4 {
		t.Errorf("%d job.progress events for 20000 Progress calls", n)
	}
}

func TestParseRunReparseFlag(t *testing.T) {
	p := newPfx(t, "a")
	reg := registry(t, "", good("a"))
	if code, _, e := runP(t, reg, "parse", "run", "--case", p.dir); code != 0 {
		t.Fatal(code, e)
	}
	_, out, _ := runP(t, reg, "parse", "run", "--case", p.dir, "--json")
	if !strings.Contains(out, `"outcome":"skipped"`) {
		t.Errorf("second run without --reparse:\n%s", out)
	}
	code, out, _ := runP(t, reg, "parse", "run", "--case", p.dir, "--json", "--reparse")
	if code != 0 || !strings.Contains(out, `"outcome":"complete"`) {
		t.Errorf("--reparse: exit %d\n%s", code, out)
	}
}

func TestParseRunNoRecoveredFlag(t *testing.T) {
	p := newPfx(t, "a")
	if code, _, _ := runP(t, registry(t, "", good("a")), "parse", "run", "--case", p.dir, "--include-recovered"); code != ExitUsage {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestParseRunSetsAndRestoresMemoryLimit(t *testing.T) {
	p := newPfx(t, "a")
	var seen atomic.Int64
	pr := good("a")
	pr.parse = func(_ context.Context, _ *parse.Input, _ parse.Emitter) error {
		seen.Store(debug.SetMemoryLimit(-1))
		return nil
	}
	prev := debug.SetMemoryLimit(-1)
	if code, _, e := runP(t, registry(t, "", pr), "parse", "run", "--case", p.dir, "--mem-budget", "64MiB"); code != 0 {
		t.Fatal(code, e)
	}
	if seen.Load() != 128<<20 {
		t.Errorf("limit during the run %d, want %d", seen.Load(), int64(128<<20))
	}
	if now := debug.SetMemoryLimit(-1); now != prev {
		t.Errorf("limit after the run %d, want the previous %d", now, prev)
	}
}

func TestParseExitLineIsOneEscapedLine(t *testing.T) {
	p := newPfx(t, "a", "b")
	p.flip("a")
	code, _, errOut := runP(t, registry(t, "", good("a"), good("b")), "parse", "run", "--case", p.dir)
	if code != ExitIntegrity {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var errLines []string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.HasPrefix(l, "error:") {
			errLines = append(errLines, l)
		}
	}
	if len(errLines) != 1 || !strings.Contains(errLines[0], "1 integrity failure(s)") {
		t.Errorf("error lines %q", errLines)
	}

	p2 := newPfx(t, "c")
	pr := good("c")
	pr.parse = func(context.Context, *parse.Input, parse.Emitter) error {
		return errors.New("bad\nerror: forged\x1b[31m line")
	}
	code, out, errOut := runP(t, registry(t, "", pr), "parse", "run", "--case", p2.dir)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	var n int
	for _, l := range strings.Split(out+errOut, "\n") {
		if strings.HasPrefix(l, "error:") {
			n++
			if !strings.Contains(l, "job(s) incomplete, unparsed or refused") {
				t.Errorf("error line %q", l)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d error lines, want 1:\n%s%s", n, out, errOut)
	}
}

func TestParseSnapshotFlag(t *testing.T) {
	p := newPfx(t)
	var live, snap evidence.ManifestRecord
	p.edit(func(c *evidence.Case) {
		mk := func(rel string, ref *evidence.SnapshotRef) evidence.ManifestRecord {
			d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: ppath("a"), Snapshot: ref}
			return pcapture(t, c, "files/"+rel, evidence.Source{Kind: "extract", RemotePath: ppath("a"), Derived: d}, "x")
		}
		live = mk("live", nil)
		snap = mk("snap", &evidence.SnapshotRef{Name: "s\x1b[31m1", Xid: 7})
	})
	reg := registry(t, "", good("a"))
	code, out, _ := runP(t, reg, "parse", "plan", "--case", p.dir)
	if code != 0 || !strings.Contains(out, live.ID) || strings.Contains(out, snap.ID) {
		t.Fatalf("default plan: exit %d\n%s", code, out)
	}
	if code, _, _ = runP(t, reg, "parse", "plan", "--case", p.dir, "--artifact", snap.ID); code != ExitUsage {
		t.Errorf("--artifact naming a snapshot artifact: exit %d, want 2", code)
	}
	code, out, _ = runP(t, reg, "parse", "plan", "--case", p.dir, "--include-snapshots")
	if code != 0 || !strings.Contains(out, snap.ID) || !strings.Contains(out, `[snapshot "s\x1b[31m1" xid 7]`) {
		t.Errorf("--include-snapshots: exit %d\n%s", code, out)
	}
	if r, bad := rawControl(out); bad {
		t.Errorf("raw %q in the plan", r)
	}
}

func TestParseAbandonmentStopsRunAndExits1(t *testing.T) {
	fastGrace(t)
	p := newPfx(t, "slow", "after")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := parsertest.Slow{Name: "slow", Version: "1.0.0", Release: release, Done: make(chan struct{})}
	reg := registry(t, "", slow, good("after"))
	code, out, errOut := runP(t, reg, "parse", "run", "--case", p.dir, "--timeout", "1s")
	if code != 1 || !strings.Contains(errOut, "restart recommended: a parser did not stop; the run was ended") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if !strings.Contains(out, "not-run") {
		t.Errorf("later job not reported not-run:\n%s", out)
	}
}

func TestParseUnknownSubcommandIsUsage(t *testing.T) {
	if code, _, _ := runP(t, nil, "parse", "bogus"); code != ExitUsage {
		t.Errorf("exit %d", code)
	}
}

func TestParseCLIEscapesDeviceText(t *testing.T) {
	evil := "/data/x/\x1b[31mred\u202eevil\u0085.dat"
	p := newPfx(t)
	p.edit(func(c *evidence.Case) {
		pcapture(t, c, "files/e.dat", evidence.Source{Kind: "file", RemotePath: evil}, pData)
	})
	pr := good("e")
	pr.glob = "android:**/*.dat"
	pr.parse = func(ctx context.Context, _ *parse.Input, out parse.Emitter) error {
		out.Note("k\x1b[2J", "v\u202e\x07")
		_ = out.Warn(ctx, "p\x1b]0;x\x07", "r\u202e\x85")
		panic("boom\x1b[31m\u202e\xff")
	}
	reg := registry(t, "", pr)
	for _, args := range [][]string{{"parse", "plan"}, {"parse", "run", "-v"}} {
		_, out, errOut := runP(t, reg, append(args, "--case", p.dir)...)
		for _, s := range []string{out, errOut} {
			if r, bad := rawControl(s); bad {
				t.Errorf("%v: raw %U reached the terminal:\n%q", args, r, s)
			}
		}
	}
	code, out, _ := runP(t, reg, "parse", "plan", "--case", p.dir, "--json")
	var pl struct {
		Rows []struct {
			Logical string `json:"logical_path"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &pl); code != 0 || err != nil || len(pl.Rows) != 1 || pl.Rows[0].Logical != "android:"+evil {
		t.Errorf("--json must keep the path as given: exit %d err %v %+q want %q", code, err, pl, "android:"+evil)
	}
}

func TestParseExitCodes(t *testing.T) {
	fastGrace(t)
	type setup = func(t *testing.T) (*pfx, regFn, []string)
	type tc struct {
		name  string
		setup setup
		code  int
		err   string // substring of stdout+stderr
	}
	one := func(mut func(p *pfx, pr *cliParser), args ...string) setup {
		return func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "a", "b")
			pa := good("a")
			if mut != nil {
				mut(p, &pa)
			}
			return p, registry(t, "", pa, good("b")), args
		}
	}
	panics := func(_ *pfx, pr *cliParser) {
		pr.parse = func(context.Context, *parse.Input, parse.Emitter) error { panic("boom") }
	}
	tests := []tc{
		{"all complete", one(nil), 0, ""},
		{"zero jobs", func(t *testing.T) (*pfx, regFn, []string) {
			return newPfx(t), registry(t, "", good("a")), nil
		}, 0, ""},
		{"unparsed", one(func(_ *pfx, pr *cliParser) {
			pr.probe = func(context.Context, *parse.Input) (parse.Applicability, error) {
				return parse.Applicability{}, errors.New("cannot probe")
			}
		}), 1, "unparsed"},
		{"incomplete panic", one(panics), 1, "incomplete"},
		{"abandoned", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "slow")
			rel := make(chan struct{})
			t.Cleanup(func() { close(rel) })
			return p, registry(t, "", parsertest.Slow{Name: "slow", Version: "1.0.0", Release: rel, Done: make(chan struct{})}), []string{"--timeout", "1s"}
		}, 1, "restart recommended"},
		{"identity conflict", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "a")
			if code, _, e := runP(t, registry(t, "x", good("a")), "parse", "run", "--case", p.dir); code != 0 {
				t.Fatal(code, e)
			}
			return p, registry(t, "y", good("a")), []string{"--reparse"}
		}, 1, "refused"},
		{"stale bundle", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "w")
			wp := good("w")
			wp.wal = true
			reg := registry(t, "", wp)
			if code, _, e := runP(t, reg, "parse", "run", "--case", p.dir); code != 0 {
				t.Fatal(code, e)
			}
			p.edit(func(c *evidence.Case) {
				pcapture(t, c, "files/w.dat-wal", evidence.Source{Kind: "file", RemotePath: ppath("w") + "-wal"}, "wal")
			})
			return p, reg, nil
		}, 1, "unparsed"},
		{"hash mismatch", one(func(p *pfx, _ *cliParser) { p.flip("a") }), 4, "integrity"},
		{"size mismatch", one(func(p *pfx, _ *cliParser) {
			f, err := os.OpenFile(p.file("a"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("more")
			_ = f.Close()
		}), 4, "integrity"},
		{"missing artifact file", one(func(p *pfx, _ *cliParser) {
			if err := os.Remove(p.file("a")); err != nil {
				t.Fatal(err)
			}
		}), 4, "integrity"},
		{"duplicated manifest id", one(func(p *pfx, _ *cliParser) { recordstest.AppendManifestLine(t, p.dir, p.recs["a"]) }), 4, "integrity"},
		{"incomplete and integrity", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "a", "b")
			p.flip("a")
			pb := good("b")
			panics(nil, &pb)
			return p, registry(t, "", good("a"), pb), nil
		}, 4, "integrity"},
		{"unknown flag", one(nil, "--nope"), 2, ""},
		{"unknown parser", one(nil, "--parser", "zzz"), 2, ""},
		{"parser version not held", one(nil, "--parser", "a@9.9.9"), 2, ""},
		{"malformed parser", one(nil, "--parser", "a@"), 2, ""},
		{"recover artifact", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "a")
			var id string
			p.edit(func(c *evidence.Case) {
				d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: ppath("a")}
				id = pcapture(t, c, "files/r", evidence.Source{Kind: "recover", RemotePath: ppath("a"), Derived: d}, "x").ID
			})
			return p, registry(t, "", good("a")), []string{"--artifact", id}
		}, 2, ""},
		{"info artifact", func(t *testing.T) (*pfx, regFn, []string) {
			p := newPfx(t, "a")
			var id string
			p.edit(func(c *evidence.Case) {
				id = pcapture(t, c, "files/i", evidence.Source{Kind: "info"}, "x").ID
			})
			return p, registry(t, "", good("a")), []string{"--artifact", id}
		}, 2, ""},
		{"unknown artifact id", one(nil, "--artifact", "nosuchid"), 1, ""},
		{"timeout 0", one(nil, "--timeout", "0"), 2, ""},
		{"timeout bad", one(nil, "--timeout", "soon"), 2, ""},
		{"timeout too long", one(nil, "--timeout", "25h"), 2, ""},
		{"mem-budget small", one(nil, "--mem-budget", "1KiB"), 2, ""},
		{"mem-budget junk", one(nil, "--mem-budget", "lots"), 2, ""},
		{"max-records 0", one(nil, "--max-records", "0"), 2, ""},
		{"max-records huge", one(nil, "--max-records", "2000000000"), 2, ""},
		{"v1 case", func(t *testing.T) (*pfx, regFn, []string) {
			return &pfx{t: t, dir: recordstest.NewV1Case(t)}, registry(t, "", good("a")), nil
		}, 2, "case upgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, reg, extra := tt.setup(t)
			args := append([]string{"parse", "run", "--case", p.dir}, extra...)
			code, out, errOut := runP(t, reg, args...)
			if code != tt.code || !strings.Contains(errOut+out, tt.err) {
				t.Fatalf("exit %d (want %d), want %q in:\nstdout %s\nstderr %s", code, tt.code, tt.err, out, errOut)
			}
			if tt.name == "hash mismatch" && !strings.Contains(out, "complete") {
				t.Errorf("the other job did not run:\n%s", out)
			}
		})
	}
	t.Run("context cancelled", func(t *testing.T) {
		p := newPfx(t, "a")
		var o, e bytes.Buffer
		root := newRootCmd(Deps{In: strings.NewReader(""), Out: &o, Err: &e, Registry: device.NewRegistry(), ParserRegistry: registry(t, "", good("a"))})
		root.SetArgs([]string{"parse", "run", "--case", p.dir})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := root.ExecuteContext(ctx); ExitCode(err) != 1 {
			t.Errorf("exit %d (%v), want 1", ExitCode(err), err)
		}
	})
}

func TestFinishRunIntegrityIsNeverMaskedByARunError(t *testing.T) {
	var o bytes.Buffer
	sum := artparse.RunSummary{ParseID: "p", Jobs: []artparse.JobResult{{Job: 1, Outcome: artparse.OutcomeRefused, Integrity: true, Reason: "integrity: x"}}}
	err := finishRun(Deps{Out: &o, Err: &o}, &rootOptions{}, sum, errors.New("audit append failed"))
	if ExitCode(err) != ExitIntegrity || !strings.Contains(err.Error(), "audit append failed") {
		t.Errorf("exit %d: %v", ExitCode(err), err)
	}
}
