//go:build scale

package artparse_test

// TestScaleParse measures the parse pipeline on inputs far above the unit tests (risk R8, plan 4H
// ruling R11). It is manual: `go test -tags scale` is vetted by tools/check and never run by it. Each
// phase runs in its own process (MINUTIAE_SCALE_PHASE), because the peak resident set of a process is a
// high-water mark that a second phase would inherit. Run it through bash .superpowers/sdd/heavy.sh
// (docs/testing.md, docs/performance.md).
//
// Phases, in order: gen, decode-200m, decode-1g, ingest-200m, jobs-500. Environment:
// MINUTIAE_SCALE_DIR (the work directory; about 4 GiB), MINUTIAE_SCALE_LABEL, MINUTIAE_SCALE_MACHINE,
// MINUTIAE_SCALE_PHASE, and for gen only MINUTIAE_SCALE_GEN (a comma list of 200m, 1g; both by default).
//
// Peak resident memory is the high-water mark of the whole process, so it includes the setup of the phase
// (capturing the input into a case, generating the small bundles of jobs-500): it is an upper bound of the
// parse. The scale directory holds generated inputs and cases of several GiB and must be deleted after a
// run.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // generates the inputs (tests only; the parsers never link an engine)

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/records"
)

const (
	scaleMiB      = int64(1) << 20
	size200m      = 200 * scaleMiB  // below MemInputMax (256 MiB): served from memory
	size1g        = 1280 * scaleMiB // 1.25 GiB: above it, streamed
	scaleBudget   = 256 * scaleMiB  // MemBudget of the streamed and many-jobs phases (4h-R33)
	scaleRSSCap   = 2 * scaleBudget // the peak resident set those phases must stay within: 512 MiB
	manyJobs      = 500
	mmsRemotePath = "/data/data/com.android.providers.telephony/databases/mmssms.db"
	mmsDDL        = "../../tools/fixtures/schemas/android-telephony/mmssms-aosp-baseline.sql"
	callDDL       = "../../tools/fixtures/schemas/android-telephony/calllog-features-subid.sql"
)

// scaleEnv is the environment of one run.
type scaleEnv struct {
	dir, label, machine, phase string
}

func scaleEnvOf(t *testing.T) scaleEnv {
	t.Helper()
	e := scaleEnv{phase: os.Getenv("MINUTIAE_SCALE_PHASE")}
	if e.phase == "" {
		t.Skip("set MINUTIAE_SCALE_PHASE (gen, decode-200m, decode-1g, ingest-200m, jobs-500) to run the parse scale test; it is manual (see docs/testing.md)")
	}
	if e.label = os.Getenv("MINUTIAE_SCALE_LABEL"); e.label == "" {
		t.Fatal("set MINUTIAE_SCALE_LABEL (a name for this run, such as 4h)")
	}
	if e.machine = os.Getenv("MINUTIAE_SCALE_MACHINE"); e.machine == "" {
		t.Fatal("set MINUTIAE_SCALE_MACHINE to the CPU, RAM and disk of this machine: a number without its machine is not a record")
	}
	if e.dir = os.Getenv("MINUTIAE_SCALE_DIR"); e.dir == "" {
		t.Fatal("set MINUTIAE_SCALE_DIR to a directory under the session scratchpad: a scale input is large and never lives in the repository")
	}
	return e
}

// phaseResult is the record one phase writes (parse-results/<label>/<phase>.json).
type phaseResult struct {
	Phase           string   `json:"phase"`
	Label           string   `json:"label"`
	Machine         string   `json:"machine"`
	Commit          string   `json:"commit"`
	GoVersion       string   `json:"go_version"`
	MemBudget       int64    `json:"mem_budget"`
	Jobs            int      `json:"jobs"`
	InputBytes      int64    `json:"input_bytes"`
	Rows            int64    `json:"rows"`
	Records         int64    `json:"records"`
	Rejected        int      `json:"rejected"`
	Warnings        int      `json:"warnings"`
	Outcome         string   `json:"outcome"`
	Reason          string   `json:"reason"`
	Streamed        []string `json:"streamed"`
	WallSeconds     float64  `json:"wall_seconds"`
	RecordsPerSec   float64  `json:"records_per_sec"`
	MiBPerSec       float64  `json:"mib_per_sec"`
	JobMeanMS       float64  `json:"job_mean_ms"`
	JobP95MS        float64  `json:"job_p95_ms"`
	PeakRSSMeasured bool     `json:"peak_rss_measured"`
	PeakRSSBytes    int64    `json:"peak_rss_bytes"`
	PeakRSSLimit    int64    `json:"peak_rss_limit"`
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func writeResult(t *testing.T, e scaleEnv, r phaseResult) {
	t.Helper()
	r.Label, r.Machine, r.Commit, r.GoVersion = e.label, e.machine, gitCommit(), goVersion()
	dir := filepath.Join(e.dir, "parse-results", e.label)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, r.Phase+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", r.Phase, b)
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.GoVersion
	}
	return "unknown"
}

// ---- inputs ----

// genInfo is what gen records about a database it wrote.
type genInfo struct {
	Bytes int64 `json:"bytes"`
	Rows  int64 `json:"rows"`
}

// pool is the text the bodies are cut from (a fixed pseudo-random text, so a run is repeatable).
func bodyPool() []byte {
	r := rand.New(rand.NewPCG(7, 11))
	p := make([]byte, 32<<10)
	for i := range p {
		if r.IntN(6) == 0 {
			p[i] = ' '
		} else {
			p[i] = byte('a' + r.IntN(26))
		}
	}
	return p
}

func execScript(db *sql.DB, ddlPath string) error {
	b, err := os.ReadFile(ddlPath) //nolint:gosec // a committed schema snapshot
	if err != nil {
		return err
	}
	_, err = db.Exec(string(b))
	return err
}

func openGen(path string) (*sql.DB, error) {
	_ = os.Remove(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"journal_mode=OFF", "synchronous=OFF", "cache_size=-65536"} {
		if _, err := db.Exec("PRAGMA " + p); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func dbBytes(db *sql.DB) (int64, error) {
	var pages, size int64
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

// genMMS writes an mmssms.db of at least target bytes through the engine from the committed DDL snapshot:
// sms bodies of 40 to 160 bytes, every 50th of 2 to 4 KiB so overflow pages occur. It returns the sms rows.
func genMMS(path string, target int64) (genInfo, error) {
	db, err := openGen(path)
	if err != nil {
		return genInfo{}, err
	}
	if err := execScript(db, mmsDDL); err != nil {
		_ = db.Close()
		return genInfo{}, err
	}
	pool, r := bodyPool(), rand.New(rand.NewPCG(1, 2))
	const batch = 5000
	var rows int64
	for {
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			return genInfo{}, err
		}
		st, err := tx.Prepare("INSERT INTO sms (thread_id, address, date, date_sent, read, status, type, body, seen) VALUES (?,?,?,?,?,?,?,?,?)")
		if err != nil {
			_ = db.Close()
			return genInfo{}, err
		}
		for range batch {
			rows++
			n := 40 + r.IntN(121)
			if rows%50 == 0 {
				n = 2048 + r.IntN(2049)
			}
			off := r.IntN(len(pool) - n)
			date := int64(1_577_836_800_000) + rows*1000
			if _, err := st.Exec(rows/20+1, fmt.Sprintf("+1555%07d", rows%10_000_000), date, date, 1, -1, 1+rows%2, string(pool[off:off+n]), 1); err != nil {
				_ = db.Close()
				return genInfo{}, err
			}
		}
		_ = st.Close()
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			return genInfo{}, err
		}
		size, err := dbBytes(db)
		if err != nil {
			_ = db.Close()
			return genInfo{}, err
		}
		if size >= target {
			break
		}
	}
	if err := db.Close(); err != nil {
		return genInfo{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return genInfo{}, err
	}
	return genInfo{Bytes: st.Size(), Rows: rows}, nil
}

// genCalls writes a small calllog.db of n call rows and returns its bytes.
func genCalls(path string, n int) ([]byte, error) {
	db, err := openGen(path)
	if err != nil {
		return nil, err
	}
	if err := execScript(db, callDDL); err != nil {
		_ = db.Close()
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for i := range n {
		if _, err := tx.Exec("INSERT INTO calls (number, date, duration, type) VALUES (?,?,?,?)", fmt.Sprintf("+1555%07d", i), int64(1_577_836_800_000)+int64(i)*60_000, 10+i, 1+i%3); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // a file this run just wrote
	_ = os.Remove(path)
	return b, err
}

func genPath(e scaleEnv, name string) string { return filepath.Join(e.dir, "input", name) }

func readGenInfo(t *testing.T, e scaleEnv) map[string]genInfo {
	t.Helper()
	b, err := os.ReadFile(genPath(e, "gen.json")) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatalf("run the gen phase first: %v", err)
	}
	m := map[string]genInfo{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func runGen(t *testing.T, e scaleEnv) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	info := map[string]genInfo{}
	if b, err := os.ReadFile(genPath(e, "gen.json")); err == nil { //nolint:gosec // the operator's work directory
		_ = json.Unmarshal(b, &info)
	}
	want := map[string]int64{"200m": size200m, "1g": size1g}
	sizes := os.Getenv("MINUTIAE_SCALE_GEN")
	if sizes == "" {
		sizes = "200m,1g"
	}
	for _, s := range strings.Split(sizes, ",") {
		target, ok := want[strings.TrimSpace(s)]
		if !ok {
			t.Fatalf("MINUTIAE_SCALE_GEN: unknown size %q (200m, 1g)", s)
		}
		name := "mmssms-" + strings.TrimSpace(s) + ".db"
		start := time.Now()
		gi, err := genMMS(genPath(e, name), target)
		if err != nil {
			t.Fatalf("gen %s: %v", name, err)
		}
		info[name] = gi
		t.Logf("gen %s: %d bytes, %d rows in %s", name, gi.Bytes, gi.Rows, time.Since(start).Round(time.Second))
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(genPath(e, "gen.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(t, e, phaseResult{Phase: "gen", Outcome: "generated"})
}

// ---- cases and hosts ----

func newScaleCase(t *testing.T, parent, id string) *evidence.Case {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(parent, id)); err != nil { // an earlier run of this phase
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	c, err := evidence.Create(parent, evidence.CreateOptions{ID: id, Examiner: "scale"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "logical"}); err != nil {
		t.Fatal(err)
	}
	return c
}

func captureReader(t *testing.T, c *evidence.Case, remote string, r io.Reader) {
	t.Helper()
	_, err := c.Capture("D1", "A1", "files"+remote, evidence.Source{Kind: "file", RemotePath: remote, DeviceID: "D1"}, func(w io.Writer) error {
		_, err := io.CopyBuffer(w, r, make([]byte, 1<<20))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func captureFile(t *testing.T, c *evidence.Case, remote, src string) {
	t.Helper()
	f, err := os.Open(src) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	captureReader(t, c, remote, f)
}

func scaleRegister(t *testing.T, p parse.Parser) artparse.Registered {
	t.Helper()
	hash, ok := parsers.HashOf(p)
	if !ok {
		t.Fatal("the parser has no generated source hash")
	}
	r, err := artparse.Register(p, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Meta().Claims) > 0 {
		rh, ok := parsers.RecoveryHashOf(p)
		if !ok {
			t.Fatal("the parser claims tables but has no recovery hash")
		}
		if r, err = r.WithRecoveryHash(rh); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// countWriter is the writer of the decode phases: it counts what the parser emits and stores nothing, so
// the phase costs the parse and not the records writer.
type countWriter struct{ records, warnings, rejected int64 }

func (w *countWriter) Start(context.Context, records.StartOptions) error { return nil }
func (w *countWriter) Add(context.Context, records.Record) error {
	w.records++
	return nil
}

func (w *countWriter) Warn(context.Context, string, string) error {
	w.warnings++
	return nil
}

func (w *countWriter) Reject(context.Context, string, string) error {
	w.rejected++
	return nil
}
func (w *countWriter) Flush(context.Context) error { return nil }
func (w *countWriter) End(context.Context) (records.IngestResult, error) {
	return records.IngestResult{IngestID: "COUNT", Outcome: "complete", Records: w.records, Warnings: int(w.warnings), Rejected: int(w.rejected)}, nil
}

func (w *countWriter) Abort(context.Context, error) (records.IngestResult, error) {
	return records.IngestResult{IngestID: "COUNT", Outcome: "incomplete", Records: w.records}, nil
}
func (w *countWriter) IngestID() string { return "COUNT" }

// runHost runs every job of the case and returns the summary, the wall time and the peak resident set
// when the run ended. Like the CLI it lowers the garbage collector's memory limit to twice the budget.
func runHost(t *testing.T, c *evidence.Case, budget int64, counting bool, ps ...parse.Parser) (artparse.RunSummary, time.Duration, int64, bool) {
	t.Helper()
	lim := parse.DefaultLimits()
	if budget > 0 {
		lim.MemBudget = budget
	}
	regs := make([]artparse.Registered, len(ps))
	for i, p := range ps {
		regs[i] = scaleRegister(t, p)
	}
	opt := artparse.Options{Limits: lim}
	if counting {
		artparse.WithNewWriter(&opt, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
			return &countWriter{}, nil
		})
	}
	h, err := artparse.New(c, regs, opt)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory() // hand back the garbage of the setup (generation, capture) before the measured run
	prev := debug.SetMemoryLimit(min(debug.SetMemoryLimit(-1), 2*lim.MemBudget))
	defer debug.SetMemoryLimit(prev)
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rss, ok := peakRSS()
	return sum, wall, rss, ok
}

// streamedOf lists the artifacts the parse.job.end entries say were served from the file.
func streamedOf(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	all, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range all {
		if e.Action != artparse.ActionParseJobEnd {
			continue
		}
		je, err := evidence.DecodeDetails[artparse.JobEnd](e.Details)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, je.Streamed...)
	}
	return out
}

// fillResult computes the derived figures of a result from the run.
func fillResult(r *phaseResult, sum artparse.RunSummary, wall time.Duration, rss int64, rssOK bool) {
	r.Jobs = len(sum.Jobs)
	for _, j := range sum.Jobs {
		r.Records += j.Records
		r.Rejected += j.Rejected
		r.Warnings += j.Warnings
	}
	if len(sum.Jobs) > 0 {
		r.Outcome, r.Reason = sum.Jobs[0].Outcome, sum.Jobs[0].Reason
	}
	r.WallSeconds = wall.Seconds()
	if r.WallSeconds > 0 {
		r.RecordsPerSec = float64(r.Records) / r.WallSeconds
		r.MiBPerSec = float64(r.InputBytes) / float64(scaleMiB) / r.WallSeconds
	}
	ms := make([]float64, 0, len(sum.Jobs))
	var total float64
	for _, j := range sum.Jobs {
		d := float64(j.Duration) / float64(time.Millisecond)
		ms = append(ms, d)
		total += d
	}
	if len(ms) > 0 {
		slices.Sort(ms)
		r.JobMeanMS = total / float64(len(ms))
		r.JobP95MS = ms[min(len(ms)-1, (len(ms)*95+99)/100-1)]
	}
	r.PeakRSSMeasured, r.PeakRSSBytes = rssOK, rss
}

// checkRSS asserts the peak resident set against limit (0 = recorded only). Where the platform cannot
// read it the phase is skipped with the reason, never passed silently.
type rssOutcome int

const (
	rssPass rssOutcome = iota
	rssOver
	rssNotMeasured
)

// rssVerdict judges the peak resident set of a phase against its limit (0 = recorded only).
func rssVerdict(r phaseResult) rssOutcome {
	switch {
	case r.PeakRSSLimit == 0:
		return rssPass
	case !r.PeakRSSMeasured:
		return rssNotMeasured
	case r.PeakRSSBytes > r.PeakRSSLimit:
		return rssOver
	}
	return rssPass
}

func checkRSS(t *testing.T, r phaseResult) {
	t.Helper()
	switch rssVerdict(r) {
	case rssNotMeasured:
		t.Skip("peak RSS not measured on this platform")
	case rssOver:
		t.Errorf("peak RSS %d MiB is above the limit of %d MiB", r.PeakRSSBytes/scaleMiB, r.PeakRSSLimit/scaleMiB)
	}
}

// parsePhase captures one mmssms.db into a fresh case and parses it: counting (decode) or through the
// real records writer (ingest). streamed says whether the input must be streamed (above MemInputMax).
func parsePhase(t *testing.T, e scaleEnv, phase, input string, gi genInfo, budget int64, counting, streamed bool, rssLimit int64) {
	t.Helper()
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), strings.ToUpper(strings.ReplaceAll(phase, "-", "")))
	captureFile(t, c, mmsRemotePath, input)
	sum, wall, rss, rssOK := runHost(t, c, budget, counting, androidmms.Parser{})
	res := phaseResult{Phase: phase, MemBudget: cmp.Or(budget, parse.DefaultLimits().MemBudget), InputBytes: gi.Bytes, Rows: gi.Rows, PeakRSSLimit: rssLimit}
	fillResult(&res, sum, wall, rss, rssOK)
	res.Streamed = streamedOf(t, c)
	writeResult(t, e, res)
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(sum.Jobs))
	}
	if got := len(res.Streamed) > 0; got != streamed {
		t.Errorf("streamed inputs = %v, want streamed=%v", res.Streamed, streamed)
	}
	switch {
	case res.Outcome == artparse.OutcomeComplete:
		if res.Records != gi.Rows {
			t.Errorf("records = %d, want the %d rows written", res.Records, gi.Rows)
		}
	case res.Outcome == artparse.OutcomeIncomplete && strings.Contains(res.Reason, "memory budget exceeded") && phase == "decode-1g":
		t.Logf("decode-1g ended incomplete with a budget error (recorded, allowed): %s", res.Reason)
	default:
		t.Errorf("outcome %s (%s), want complete", res.Outcome, res.Reason)
	}
	checkRSS(t, res)
}

// manyJobsPhase captures manyJobs small calllog databases under /data/user/<n>/ and parses them all.
func manyJobsPhase(t *testing.T, e scaleEnv, phase string, n int, budget, rssLimit int64) {
	t.Helper()
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), strings.ToUpper(strings.ReplaceAll(phase, "-", "")))
	tmp := filepath.Join(e.dir, "jobs-tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	var rows, inBytes int64
	for i := range n {
		rowsHere := 20 + i%17
		b, err := genCalls(filepath.Join(tmp, "calllog.db"), rowsHere)
		if err != nil {
			t.Fatal(err)
		}
		rows += int64(rowsHere)
		inBytes += int64(len(b))
		captureReader(t, c, fmt.Sprintf("/data/user/%d/com.android.providers.contacts/databases/calllog.db", i), strings.NewReader(string(b)))
	}
	sum, wall, rss, rssOK := runHost(t, c, budget, false, androidcalllog.Parser{})
	res := phaseResult{Phase: phase, MemBudget: budget, InputBytes: inBytes, Rows: rows, PeakRSSLimit: rssLimit}
	fillResult(&res, sum, wall, rss, rssOK)
	res.Streamed = streamedOf(t, c)
	res.Outcome, res.Reason = artparse.OutcomeComplete, ""
	for _, j := range sum.Jobs {
		if j.Outcome != artparse.OutcomeComplete {
			res.Outcome, res.Reason = j.Outcome, j.Reason
			break
		}
	}
	writeResult(t, e, res)
	if res.Jobs != n {
		t.Errorf("%d jobs, want %d", res.Jobs, n)
	}
	if res.Outcome != artparse.OutcomeComplete {
		t.Errorf("a job ended %s (%s), want every job complete", res.Outcome, res.Reason)
	}
	if res.Records != rows {
		t.Errorf("records = %d, want the %d rows written", res.Records, rows)
	}
	checkRSS(t, res)
}

func TestScaleParse(t *testing.T) {
	e := scaleEnvOf(t)
	if err := os.MkdirAll(e.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	inputRSS := func(gi genInfo) int64 { return gi.Bytes + 2*parse.DefaultLimits().MemBudget } // in memory: input + 2 x MemBudget
	switch e.phase {
	case "gen":
		runGen(t, e)
	case "decode-200m", "ingest-200m":
		gi := readGenInfo(t, e)["mmssms-200m.db"]
		parsePhase(t, e, e.phase, genPath(e, "mmssms-200m.db"), gi, 0, e.phase == "decode-200m", false, inputRSS(gi))
	case "decode-1g":
		gi := readGenInfo(t, e)["mmssms-1g.db"]
		parsePhase(t, e, e.phase, genPath(e, "mmssms-1g.db"), gi, scaleBudget, true, true, scaleRSSCap)
	case "jobs-500":
		manyJobsPhase(t, e, e.phase, manyJobs, scaleBudget, scaleRSSCap)
	default:
		t.Fatalf("unknown MINUTIAE_SCALE_PHASE %q (gen, decode-200m, decode-1g, ingest-200m, jobs-500)", e.phase)
	}
}

// TestScaleParseSmoke proves the harness works end to end on a 2 MiB database through decode and ingest,
// and on a few small calllog bundles, without the environment of the real phases.
func TestScaleParseSmoke(t *testing.T) {
	e := scaleEnv{dir: t.TempDir(), label: "smoke", machine: "smoke", phase: "smoke"}
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	gi, err := genMMS(genPath(e, "smoke.db"), 2*scaleMiB)
	if err != nil {
		t.Fatal(err)
	}
	if gi.Bytes < 2*scaleMiB || gi.Rows == 0 {
		t.Fatalf("gen wrote %+v, want at least 2 MiB and some rows", gi)
	}
	for _, counting := range []bool{true, false} {
		name := map[bool]string{true: "smoke-decode", false: "smoke-ingest"}[counting]
		parsePhase(t, e, name, genPath(e, "smoke.db"), gi, 0, counting, false, 0)
	}
	manyJobsPhase(t, e, "smoke-jobs", 5, 0, 0)
	var res phaseResult
	b, err := os.ReadFile(filepath.Join(e.dir, "parse-results", "smoke", "smoke-ingest.json")) //nolint:gosec // this test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Records != gi.Rows || res.RecordsPerSec <= 0 {
		t.Errorf("result record = %+v (%v), want %d records and a rate", res, err, gi.Rows)
	}
}
