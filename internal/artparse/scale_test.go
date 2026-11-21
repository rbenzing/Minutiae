//go:build scale

package artparse_test

// TestScaleParse measures the parse pipeline on inputs far above the unit tests (risk R8, plan 4H
// ruling R11). It is manual: `go test -tags scale` is vetted by tools/check and never run by it. Each
// phase runs in its own process (MINUTIAE_SCALE_PHASE), because the peak resident set of a process is a
// high-water mark that a second phase would inherit. Run it through bash .superpowers/sdd/heavy.sh
// (docs/testing.md, docs/performance.md).
//
// Phases, in order: gen, decode-200m, decode-1g, ingest-200m, jobs-500, and for plan 4I whatsapp-gen and whatsapp-decode-1m
// (a 1,000,000-message msgstore.db written by the engine, then parsed with MemBudget 1 GiB), and for plan 4I2 waios-gen and
// waios-decode-1m (the same for a 1,000,000-message iOS ChatStorage.sqlite; the database file plus the peak resident set
// must stay within 2 GiB; both skip under -short), and for plan 4I3 calendar-gen and calendar-decode (a calendar.db of
// 500,000 events, 900,000 attendees and 500,000 reminders written by the engine, then parsed with MemBudget 1 GiB; the
// same 2 GiB bound; both skip under -short), and for plan 4I4 telegram-gen and telegram-decode (a cache4.db of 1,000,000
// messages_v2 rows over 1,000 dialogs, 10,000 users and 1,000 chats written by the engine, then parsed with MemBudget
// 1 GiB; no undecided join and no budget state; the same 2 GiB bound; both skip under -short). Environment:
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
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // generates the inputs (tests only; the parsers never link an engine)

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
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
		t.Skip("set MINUTIAE_SCALE_PHASE (gen, decode-200m, decode-1g, ingest-200m, jobs-500, whatsapp-gen, whatsapp-decode-1m, waios-gen, waios-decode-1m, calendar-gen, calendar-decode, telegram-gen, telegram-decode) to run the parse scale test; it is manual (see docs/testing.md)")
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
	// TableRows are the rows of each table of a multi-table input (calendar-gen and calendar-decode).
	TableRows map[string]int64 `json:"table_rows,omitempty"`
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
	case "whatsapp-gen":
		runGenWhatsApp(t, e)
	case "whatsapp-decode-1m":
		whatsAppDecode(t, e)
	case "waios-gen":
		runGenWhatsAppIOS(t, e)
	case "waios-decode-1m":
		whatsAppIOSDecode(t, e)
	case "calendar-gen":
		runGenCalendar(t, e)
	case "calendar-decode":
		calendarDecode(t, e)
	case "telegram-gen":
		runGenTelegram(t, e)
	case "telegram-decode":
		telegramDecode(t, e)
	default:
		t.Fatalf("unknown MINUTIAE_SCALE_PHASE %q (gen, decode-200m, decode-1g, ingest-200m, jobs-500, whatsapp-gen, whatsapp-decode-1m, waios-gen, waios-decode-1m, calendar-gen, calendar-decode, telegram-gen, telegram-decode)", e.phase)
	}
}

// ---- WhatsApp (plan 4I task 15) ----

const (
	waRemotePath = "/data/data/com.whatsapp/databases/msgstore.db"
	waDDL        = "../../tools/fixtures/schemas/whatsapp-android/modern-2026.sql"
	waMessages   = 1_000_000
	waChats      = 1000
	waJobs       = 200
	waBudget     = 1024 * scaleMiB // MemBudget of whatsapp-decode-1m (plan 4I task 15)
)

// genWhatsApp writes a modern msgstore.db through the engine from the committed DDL snapshot: n messages over waChats chats,
// 10% of them image messages with a message_media row, and waJobs deleted_chat_job rows. It returns the message rows.
func genWhatsApp(path string, n int64) (genInfo, error) {
	db, err := openGen(path)
	if err != nil {
		return genInfo{}, err
	}
	fail := func(err error) (genInfo, error) { _ = db.Close(); return genInfo{}, err }
	if err := execScript(db, waDDL); err != nil {
		return fail(err)
	}
	r, pool := rand.New(rand.NewPCG(3, 4)), bodyPool()
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	for c := int64(1); c <= waChats; c++ {
		if _, err := tx.Exec("INSERT INTO jid (_id, user, server, raw_string) VALUES (?,?,?,?)", c, fmt.Sprintf("1555%07d", c), "s.whatsapp.net", fmt.Sprintf("1555%07d@s.whatsapp.net", c)); err != nil {
			return fail(err)
		}
		if _, err := tx.Exec("INSERT INTO chat (_id, jid_row_id) VALUES (?,?)", c, c); err != nil {
			return fail(err)
		}
	}
	for j := int64(1); j <= waJobs; j++ {
		if _, err := tx.Exec("INSERT INTO deleted_chat_job (_id, chat_row_id, deleted_message_row_id) VALUES (?,?,?)", j, j, int64(r.IntN(int(n)))); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	const batch = 20000
	for start := int64(1); start <= n; start += batch {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		msg, err := tx.Prepare("INSERT INTO message (_id, chat_row_id, from_me, key_id, sender_jid_row_id, status, timestamp, received_timestamp, message_type, text_data, sort_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)")
		if err != nil {
			return fail(err)
		}
		media, err := tx.Prepare("INSERT INTO message_media (message_row_id, file_path, file_size, mime_type, media_name) VALUES (?,?,?,?,?)")
		if err != nil {
			return fail(err)
		}
		for id := start; id < start+batch && id <= n; id++ {
			typ := int64(0)
			if id%10 == 0 {
				typ = 1
			}
			off := r.IntN(len(pool) - 160)
			ts := int64(1_577_836_800_000) + id*1000
			if _, err := msg.Exec(id, id%waChats+1, id%2, fmt.Sprintf("K%09d", id), 0, 0, ts, ts+100, typ, string(pool[off:off+40+r.IntN(120)]), id); err != nil {
				return fail(err)
			}
			if typ == 1 {
				if _, err := media.Exec(id, fmt.Sprintf("Media/WhatsApp Images/IMG-%09d.jpg", id), 12345, "image/jpeg", fmt.Sprintf("IMG-%09d.jpg", id)); err != nil {
					return fail(err)
				}
			}
		}
		_ = msg.Close()
		_ = media.Close()
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
	}
	if err := db.Close(); err != nil {
		return genInfo{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return genInfo{}, err
	}
	return genInfo{Bytes: st.Size(), Rows: n}, nil
}

// runGenWhatsApp is the whatsapp-gen phase: the 1,000,000-message store under MINUTIAE_SCALE_DIR/input.
func runGenWhatsApp(t *testing.T, e scaleEnv) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	gi, err := genWhatsApp(genPath(e, "msgstore-1m.db"), waMessages)
	if err != nil {
		t.Fatalf("gen msgstore-1m.db: %v", err)
	}
	t.Logf("gen msgstore-1m.db: %d bytes, %d messages in %s", gi.Bytes, gi.Rows, time.Since(start).Round(time.Second))
	b, _ := json.MarshalIndent(map[string]genInfo{"msgstore-1m.db": gi}, "", "  ")
	if err := os.WriteFile(genPath(e, "whatsapp-gen.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(t, e, phaseResult{Phase: "whatsapp-gen", Outcome: "generated", InputBytes: gi.Bytes, Rows: gi.Rows})
}

// whatsAppDecode is the whatsapp-decode-1m phase: the 1,000,000-message store parsed with MemBudget 1 GiB, counting what the
// parser emits. The parser is registered under a synthetic source hash: the measurement does not depend on the identity, and
// the phase registers the one parser directly rather than through the release registry (which holds the generated hash).
func whatsAppDecode(t *testing.T, e scaleEnv) {
	t.Helper()
	var gi genInfo
	b, err := os.ReadFile(genPath(e, "whatsapp-gen.json")) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatalf("run the whatsapp-gen phase first: %v", err)
	}
	m := map[string]genInfo{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	gi = m["msgstore-1m.db"]
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), "WHATSAPPDECODE1M")
	captureFile(t, c, waRemotePath, genPath(e, "msgstore-1m.db"))
	p := androidwhatsapp.Parser{}
	reg, err := artparse.Register(p, parsertest.FakeHash(androidwhatsapp.Name))
	if err != nil {
		t.Fatal(err)
	}
	if reg, err = reg.WithRecoveryHash(parsertest.FakeHash(androidwhatsapp.Name + ".recover")); err != nil {
		t.Fatal(err)
	}
	lim := parse.DefaultLimits()
	lim.MemBudget = waBudget
	opt := artparse.Options{Limits: lim}
	artparse.WithNewWriter(&opt, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
		return &countWriter{}, nil
	})
	h, err := artparse.New(c, []artparse.Registered{reg}, opt)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory()
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rss, ok := peakRSS()
	res := phaseResult{Phase: "whatsapp-decode-1m", MemBudget: waBudget, InputBytes: gi.Bytes, Rows: gi.Rows}
	fillResult(&res, sum, wall, rss, ok)
	res.Streamed = streamedOf(t, c)
	writeResult(t, e, res)
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(sum.Jobs))
	}
	if res.Outcome != artparse.OutcomeComplete {
		t.Errorf("outcome %s (%s), want complete", res.Outcome, res.Reason)
	}
	if res.Records < gi.Rows {
		t.Errorf("records = %d, want at least the %d messages written", res.Records, gi.Rows)
	}
}

// ---- iOS WhatsApp (plan 4I2 task 6) ----

const (
	waiosRemotePath = "/private/var/mobile/Containers/Shared/AppGroup/5A0B4C3D-0000-4000-8000-00000000A001/ChatStorage.sqlite"
	waiosDDL        = "../../tools/fixtures/schemas/whatsapp-ios/chatstorage.sql"
	waiosMessages   = 1_000_000
	waiosSessions   = 1000
	waiosMembers    = 100
	waiosBudget     = 1024 * scaleMiB // MemBudget of waios-decode-1m (plan 4I2 task 6)
	waiosSizeCap    = 2048 * scaleMiB // the database file plus the peak resident set must stay within 2 GiB
)

// genWhatsAppIOS writes a ChatStorage.sqlite through the engine from the committed DDL snapshot: n ZWAMESSAGE rows over
// waiosSessions chat sessions (the first waiosMembers of them groups, each with one ZWAGROUPMEMBER row), 10% of them image
// messages with a ZWAMEDIAITEM row, and the Core Data model that names the two entities the parser needs. It returns the
// message rows.
func genWhatsAppIOS(path string, n int64) (genInfo, error) {
	db, err := openGen(path)
	if err != nil {
		return genInfo{}, err
	}
	fail := func(err error) (genInfo, error) { _ = db.Close(); return genInfo{}, err }
	if err := execScript(db, waiosDDL); err != nil {
		return fail(err)
	}
	r, pool := rand.New(rand.NewPCG(5, 6)), bodyPool()
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	for _, z := range []struct {
		ent  int64
		name string
		max  int64
	}{{4, "WAChatSession", waiosSessions}, {9, "WAMessage", n}} {
		if _, err := tx.Exec("INSERT INTO Z_PRIMARYKEY (Z_ENT, Z_NAME, Z_SUPER, Z_MAX) VALUES (?,?,0,?)", z.ent, z.name, z.max); err != nil {
			return fail(err)
		}
	}
	for s := int64(1); s <= waiosSessions; s++ {
		typ, jid := int64(0), fmt.Sprintf("1555%07d@s.whatsapp.net", s)
		if s <= waiosMembers {
			typ, jid = 1, fmt.Sprintf("1203630000%08d@g.us", s)
		}
		if _, err := tx.Exec("INSERT INTO ZWACHATSESSION (Z_PK, Z_ENT, ZSESSIONTYPE, ZCONTACTJID, ZPARTNERNAME) VALUES (?,4,?,?,?)", s, typ, jid, fmt.Sprintf("chat %d", s)); err != nil {
			return fail(err)
		}
		if s <= waiosMembers {
			if _, err := tx.Exec("INSERT INTO ZWAGROUPMEMBER (Z_PK, Z_ENT, ZCHATSESSION, ZMEMBERJID) VALUES (?,5,?,?)", s, s, fmt.Sprintf("1555%07d@s.whatsapp.net", 5000+s)); err != nil {
				return fail(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	const batch = 20000
	for start := int64(1); start <= n; start += batch {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		msg, err := tx.Prepare("INSERT INTO ZWAMESSAGE (Z_PK, Z_ENT, ZCHATSESSION, ZISFROMME, ZMESSAGETYPE, ZMESSAGEDATE, ZGROUPMEMBER, ZMEDIAITEM, ZSTANZAID, ZTEXT) VALUES (?,9,?,?,?,?,?,?,?,?)")
		if err != nil {
			return fail(err)
		}
		media, err := tx.Prepare("INSERT INTO ZWAMEDIAITEM (Z_PK, Z_ENT, ZMESSAGE, ZFILESIZE, ZMEDIALOCALPATH, ZTITLE, ZVCARDSTRING) VALUES (?,6,?,12345,?,?,'image/jpeg')")
		if err != nil {
			return fail(err)
		}
		for id := start; id < start+batch && id <= n; id++ {
			session, from := id%waiosSessions+1, id%2
			typ, item, text := int64(0), any(nil), any(string(pool[r.IntN(len(pool)-160):][:40+r.IntN(120)]))
			if id%10 == 0 {
				typ, item, text = 1, id/10, nil
			}
			var member any
			if session <= waiosMembers && from == 0 {
				member = session
			}
			if _, err := msg.Exec(id, session, from, typ, float64(600_000_000+id), member, item, fmt.Sprintf("3EB0%09d", id), text); err != nil {
				return fail(err)
			}
			if typ == 1 {
				if _, err := media.Exec(item, id, fmt.Sprintf("Media/1555%07d@s.whatsapp.net/IMG-%09d.jpg", session, id), fmt.Sprintf("IMG-%09d", id)); err != nil {
					return fail(err)
				}
			}
		}
		_ = msg.Close()
		_ = media.Close()
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
	}
	if err := db.Close(); err != nil {
		return genInfo{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return genInfo{}, err
	}
	return genInfo{Bytes: st.Size(), Rows: n}, nil
}

// runGenWhatsAppIOS is the waios-gen phase: the 1,000,000-message store under MINUTIAE_SCALE_DIR/input. Under -short it skips.
func runGenWhatsAppIOS(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("waios-gen writes a database of several hundred MiB: skipped under -short")
	}
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	gi, err := genWhatsAppIOS(genPath(e, "chatstorage-1m.db"), waiosMessages)
	if err != nil {
		t.Fatalf("gen chatstorage-1m.db: %v", err)
	}
	t.Logf("gen chatstorage-1m.db: %d bytes, %d messages in %s", gi.Bytes, gi.Rows, time.Since(start).Round(time.Second))
	b, _ := json.MarshalIndent(map[string]genInfo{"chatstorage-1m.db": gi}, "", "  ")
	if err := os.WriteFile(genPath(e, "waios-gen.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(t, e, phaseResult{Phase: "waios-gen", Outcome: "generated", InputBytes: gi.Bytes, Rows: gi.Rows})
}

// captureExtract stores a file as extracted from an APFS image at fsPath, so that the host names it
// ios:HomeDomain/<fsPath below /private/var/mobile/>: an app group place, found by its signature.
func captureExtract(t *testing.T, c *evidence.Case, fsPath, src string) {
	t.Helper()
	f, err := os.Open(src) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "apfs", FSPath: fsPath}
	_, err = c.Capture("D1", "A1", "files"+fsPath, evidence.Source{Kind: "extract", RemotePath: fsPath, DeviceID: "D1", Derived: d}, func(w io.Writer) error {
		_, err := io.CopyBuffer(w, f, make([]byte, 1<<20))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// whatsAppIOSDecode is the waios-decode-1m phase: the 1,000,000-message store parsed with MemBudget 1 GiB, counting what the
// parser emits. The parser is registered under a synthetic source hash (it is not in the release registry yet); the phase
// fails when the database file plus the peak resident set exceed 2 GiB. Under -short it skips.
func whatsAppIOSDecode(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("waios-decode-1m parses a database of several hundred MiB with a 1 GiB budget: skipped under -short")
	}
	b, err := os.ReadFile(genPath(e, "waios-gen.json")) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatalf("run the waios-gen phase first: %v", err)
	}
	m := map[string]genInfo{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	gi := m["chatstorage-1m.db"]
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), "WAIOSDECODE1M")
	captureExtract(t, c, waiosRemotePath, genPath(e, "chatstorage-1m.db"))
	p := ioswhatsapp.Parser{}
	reg, err := artparse.Register(p, parsertest.FakeHash(ioswhatsapp.Name))
	if err != nil {
		t.Fatal(err)
	}
	if reg, err = reg.WithRecoveryHash(parsertest.FakeHash(ioswhatsapp.Name + ".recover")); err != nil {
		t.Fatal(err)
	}
	lim := parse.DefaultLimits()
	lim.MemBudget = waiosBudget
	opt := artparse.Options{Limits: lim}
	artparse.WithNewWriter(&opt, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
		return &countWriter{}, nil
	})
	h, err := artparse.New(c, []artparse.Registered{reg}, opt)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory()
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rss, ok := peakRSS()
	res := phaseResult{Phase: "waios-decode-1m", MemBudget: waiosBudget, InputBytes: gi.Bytes, Rows: gi.Rows, PeakRSSLimit: waiosSizeCap - gi.Bytes}
	fillResult(&res, sum, wall, rss, ok)
	res.Streamed = streamedOf(t, c)
	writeResult(t, e, res)
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(sum.Jobs))
	}
	if res.Outcome != artparse.OutcomeComplete {
		t.Errorf("outcome %s (%s), want complete", res.Outcome, res.Reason)
	}
	if res.Records < gi.Rows {
		t.Errorf("records = %d, want at least the %d messages written", res.Records, gi.Rows)
	}
	checkRSS(t, res) // the file plus the peak resident set within 2 GiB; skips where the platform cannot read it
}

// ---- Android calendar (plan 4I3 task 6, 4I3-P12) ----

const (
	calRemotePath = "/data/data/com.android.providers.calendar/databases/calendar.db"
	calDDL        = "../../tools/fixtures/schemas/calendar-android/601.sql"
	calEvents     = 500_000
	calAttendees  = 900_000 // below the join-index cap of 1,000,000 entries per column: the Attendees join stays decided (F13)
	calReminders  = 500_000
	calCalendars  = 10
	calBudget     = 1024 * scaleMiB // MemBudget of calendar-decode
	calSizeCap    = 2048 * scaleMiB // the database file plus the peak resident set must stay within 2 GiB
)

// calGen is what calendar-gen records: the file size and the rows of each table.
type calGen struct {
	Bytes int64            `json:"bytes"`
	Rows  map[string]int64 `json:"rows"`
}

// genRows inserts total rows through stmt in batches of 20,000, args giving the values of row id.
func genRows(db *sql.DB, total int64, stmt string, args func(id int64) []any) error {
	const batch = 20000
	for start := int64(1); start <= total; start += batch {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, err := tx.Prepare(stmt)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for id := start; id < start+batch && id <= total; id++ {
			if _, err := st.Exec(args(id)...); err != nil {
				_ = st.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = st.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// genCalendar writes a calendar.db (user version 601) through the engine from the committed 601 snapshot: calCalendars
// calendars; n events of one hour in Europe/Berlin, every 20th all-day and every 50th a weekly series with a duration;
// the attendees spread over the events in turn (event i gets attendee i and, past n, a second one); one reminder each.
func genCalendar(path string, n, attendees, reminders int64) (calGen, error) {
	db, err := openGen(path)
	if err != nil {
		return calGen{}, err
	}
	fail := func(err error) (calGen, error) { _ = db.Close(); return calGen{}, err }
	if err := execScript(db, calDDL); err != nil {
		return fail(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 601"); err != nil {
		return fail(err)
	}
	err = genRows(db, calCalendars, "INSERT INTO Calendars (_id, account_name, account_type, calendar_displayName, ownerAccount, deleted, visible, sync_events) VALUES (?,?,?,?,?,0,1,1)", func(c int64) []any {
		return []any{c, fmt.Sprintf("user%d@example.test", c), "com.example.sync", fmt.Sprintf("Calendar %d", c), fmt.Sprintf("user%d@example.test", c)}
	})
	if err != nil {
		return fail(err)
	}
	r, pool := rand.New(rand.NewPCG(8, 9)), bodyPool() //nolint:gosec // a fixed seed: the generated input is repeatable
	const day, hour = int64(86_400_000), int64(3_600_000)
	err = genRows(db, n, "INSERT INTO Events (_id, calendar_id, title, description, eventLocation, dtstart, dtend, duration, rrule, eventTimezone, allDay, deleted, lastSynced, dirty, eventStatus, hasAttendeeData) VALUES (?,?,?,?,?,?,?,?,?,?,?,0,0,0,1,1)", func(id int64) []any {
		off := r.IntN(len(pool) - 400)
		dtstart := int64(1_577_836_800_000) + id*hour
		var dtend, dur, rrule any = dtstart + hour, nil, nil
		allDay, zone := int64(0), "Europe/Berlin"
		switch {
		case id%20 == 0:
			allDay, zone = 1, "UTC"
			dtstart -= dtstart % day
			dtend = dtstart + day
		case id%50 == 1:
			dtend, dur, rrule = nil, "PT1H", "FREQ=WEEKLY;BYDAY=TU"
		}
		return []any{id, id%calCalendars + 1, string(pool[off : off+20+r.IntN(40)]), string(pool[off : off+r.IntN(300)]), string(pool[off+5 : off+25]), dtstart, dtend, dur, rrule, zone, allDay}
	})
	if err != nil {
		return fail(err)
	}
	err = genRows(db, attendees, "INSERT INTO Attendees (_id, event_id, attendeeName, attendeeEmail, attendeeStatus, attendeeRelationship, attendeeType) VALUES (?,?,?,?,1,1,1)", func(id int64) []any {
		return []any{id, (id-1)%n + 1, fmt.Sprintf("Attendee %d", id), fmt.Sprintf("a%d@example.test", id)}
	})
	if err != nil {
		return fail(err)
	}
	err = genRows(db, reminders, "INSERT INTO Reminders (_id, event_id, minutes, method) VALUES (?,?,?,1)", func(id int64) []any {
		return []any{id, (id-1)%n + 1, id % 60}
	})
	if err != nil {
		return fail(err)
	}
	if err := db.Close(); err != nil {
		return calGen{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return calGen{}, err
	}
	return calGen{Bytes: st.Size(), Rows: map[string]int64{"Events": n, "Attendees": attendees, "Reminders": reminders, "Calendars": calCalendars}}, nil
}

// runGenCalendar is the calendar-gen phase: the 500,000-event calendar.db under MINUTIAE_SCALE_DIR/input. Under -short it
// skips.
func runGenCalendar(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("calendar-gen writes a database of several hundred MiB: skipped under -short")
	}
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	g, err := genCalendar(genPath(e, "calendar-500k.db"), calEvents, calAttendees, calReminders)
	if err != nil {
		t.Fatalf("gen calendar-500k.db: %v", err)
	}
	t.Logf("gen calendar-500k.db: %d bytes, rows %v in %s", g.Bytes, g.Rows, time.Since(start).Round(time.Second))
	b, _ := json.MarshalIndent(g, "", "  ")
	if err := os.WriteFile(genPath(e, "calendar-gen.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(t, e, phaseResult{Phase: "calendar-gen", Outcome: "generated", InputBytes: g.Bytes, Rows: g.Rows["Events"], TableRows: g.Rows})
}

// joinCheckWriter is countWriter that also counts the records whose raw names an undecided join (F13: with the
// attendees below the join-index cap every join of the measurement is decided).
type joinCheckWriter struct {
	countWriter
	undecided int64
}

func (w *joinCheckWriter) Add(ctx context.Context, r records.Record) error {
	if raw, ok := r.Payload["raw"].(map[string]any); ok {
		if _, has := raw["join_undecided"]; has {
			w.undecided++
		}
	}
	return w.countWriter.Add(ctx, r)
}

// calendarDecode is the calendar-decode phase: the 500,000-event store parsed with MemBudget 1 GiB, counting what the
// parser emits and the records with an undecided join (there must be none). The parser is registered under a synthetic
// source hash (it is not in the release registry yet); the phase fails when the database file plus the peak resident set
// exceed 2 GiB. Under -short it skips.
func calendarDecode(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("calendar-decode parses a database of several hundred MiB with a 1 GiB budget: skipped under -short")
	}
	calendarDecodeRun(t, e)
}

// calendarDecodeRun is the body of calendar-decode on whatever store calendar-gen.json describes (TestScaleParseSmoke
// runs it on a small one, under -short too).
func calendarDecodeRun(t *testing.T, e scaleEnv) {
	t.Helper()
	b, err := os.ReadFile(genPath(e, "calendar-gen.json")) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatalf("run the calendar-gen phase first: %v", err)
	}
	var g calGen
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), "CALENDARDECODE")
	captureFile(t, c, calRemotePath, genPath(e, "calendar-500k.db"))
	p := androidcalendar.Parser{}
	reg, err := artparse.Register(p, parsertest.FakeHash(androidcalendar.Name))
	if err != nil {
		t.Fatal(err)
	}
	if reg, err = reg.WithRecoveryHash(parsertest.FakeHash(androidcalendar.Name + ".recover")); err != nil {
		t.Fatal(err)
	}
	lim := parse.DefaultLimits()
	lim.MemBudget = calBudget
	opt := artparse.Options{Limits: lim}
	w := &joinCheckWriter{}
	artparse.WithNewWriter(&opt, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
		return w, nil
	})
	h, err := artparse.New(c, []artparse.Registered{reg}, opt)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory()
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rss, ok := peakRSS()
	res := phaseResult{Phase: "calendar-decode", MemBudget: calBudget, InputBytes: g.Bytes, Rows: g.Rows["Events"], PeakRSSLimit: calSizeCap - g.Bytes, TableRows: g.Rows}
	fillResult(&res, sum, wall, rss, ok)
	res.Streamed = streamedOf(t, c)
	writeResult(t, e, res)
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(sum.Jobs))
	}
	if res.Outcome != artparse.OutcomeComplete {
		t.Errorf("outcome %s (%s), want complete", res.Outcome, res.Reason)
	}
	if res.Records != g.Rows["Events"] {
		t.Errorf("records = %d, want the %d events written (one record per Events row)", res.Records, g.Rows["Events"])
	}
	if w.undecided != 0 {
		t.Errorf("%d records name an undecided join: with %d attendees, below the join-index cap, every join is decided (F13)", w.undecided, g.Rows["Attendees"])
	}
	checkRSS(t, res) // the file plus the peak resident set within 2 GiB; skips where the platform cannot read it
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
	// the calendar phases on a small store, through the bodies without the -short skip of the real phases (review
	// M5): calendarDecodeRun reads what genCalendar wrote
	g, err := genCalendar(genPath(e, "calendar-500k.db"), 500, 900, 500)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := json.Marshal(g)
	if err := os.WriteFile(genPath(e, "calendar-gen.json"), cb, 0o600); err != nil {
		t.Fatal(err)
	}
	calendarDecodeRun(t, e)
	// the telegram phases on a small store, the same way
	tg, err := genTelegram(genPath(e, "telegram-1m.db"), 2000, 20, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := json.Marshal(tg)
	if err := os.WriteFile(genPath(e, "telegram-gen.json"), tb, 0o600); err != nil {
		t.Fatal(err)
	}
	telegramDecodeRun(t, e)
}

// ---- Android Telegram (plan 4I4 task 13, 4I4-P14) ----

const (
	tgRemotePath = "/data/data/org.telegram.messenger/files/cache4.db"
	tgDDL        = "../../tools/fixtures/schemas/telegram-android/v181.sql"
	tgMessages   = 1_000_000
	tgDialogs    = 1000 // half user dialogs (uid 1..500), half basic groups (uid -1..-500)
	tgUsers      = 10_000
	tgChats      = 1000
	tgBudget     = 1024 * scaleMiB // MemBudget of telegram-decode
	tgSizeCap    = 2048 * scaleMiB // the database file plus the peak resident set must stay within 2 GiB
)

// A local TL writer for the generated blobs (spec 6.5: little-endian ints, the short string form or 0xFE and three
// length bytes from 254 bytes on, zero padding to 4), with the field orders of constructors.tsv: TL_message
// (0x7600b9d3, TLRPC.java:60187), TL_peerUser (0x59511722), TL_peerChat (0x36c6019a), the vector (0x1cb5c415),
// TL_messageEntityBold (0xbd610bc9), TL_user (0xb1b8cc83) and TL_chat (0x41cbf256). It is independent of the
// parser's decoder.
func tgLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func tgLE64(b []byte, v int64) []byte {
	return tgLE32(tgLE32(b, uint32(uint64(v))), uint32(uint64(v)>>32))
}

func tgStr(b, s []byte) []byte {
	start := len(b)
	if len(s) < 254 {
		b = append(b, byte(len(s)))
	} else {
		b = append(b, 0xfe, byte(len(s)), byte(len(s)>>8), byte(len(s)>>16))
	}
	b = append(b, s...)
	for (len(b)-start)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// tgMessage is one TL_message blob: flags (from_id when from != 0, entities, out), flags2 0, id, [from_id], peer_id,
// date, the text, one bold entity over its first four bytes, then the app-local trailer (spec 8.8): one string, the
// attach path. A dialog uid below 0 is a basic group (TL_peerChat of -uid), else a user dialog (TL_peerUser).
func tgMessage(id int32, from, uid int64, out bool, date int32, text []byte) []byte {
	const bitOut, bitEntities, bitFromID = 1 << 1, 1 << 7, 1 << 8
	flags := uint32(bitEntities)
	if out {
		flags |= bitOut
	}
	if from != 0 {
		flags |= bitFromID
	}
	b := tgLE32(tgLE32(tgLE32(make([]byte, 0, 128+len(text)), 0x7600b9d3), flags), 0)
	b = tgLE32(b, uint32(id))
	if from != 0 {
		b = tgLE64(tgLE32(b, 0x59511722), from)
	}
	if uid < 0 {
		b = tgLE64(tgLE32(b, 0x36c6019a), -uid)
	} else {
		b = tgLE64(tgLE32(b, 0x59511722), uid)
	}
	b = tgLE32(b, uint32(date))
	b = tgStr(b, text)
	b = tgLE32(tgLE32(b, 0x1cb5c415), 1)
	b = tgLE32(tgLE32(tgLE32(b, 0xbd610bc9), 0), 4)
	return tgStr(b, []byte("/storage/emulated/0/Telegram/img.jpg"))
}

// tgUser is a TL_user blob with a first name and a phone; tgChat a TL_chat blob with a title.
func tgUser(id int64) []byte {
	b := tgLE32(tgLE32(tgLE32(nil, 0xb1b8cc83), 1<<1|1<<4), 0)
	b = tgLE64(b, id)
	b = tgStr(b, fmt.Appendf(nil, "User %d", id))
	return tgStr(b, fmt.Appendf(nil, "1555%07d", id))
}

func tgChat(id int64) []byte {
	b := tgLE64(tgLE32(tgLE32(nil, 0x41cbf256), 0), id)
	return tgStr(b, fmt.Appendf(nil, "Group %d", id))
}

// genTelegram writes a cache4.db (user version 181) through the engine from the committed v181 snapshot (4I4-P14):
// users rows 1..users, chats rows 1..chats, and n messages_v2 rows over dialogs dialogs in turn (the first half user
// dialogs with uid 1..dialogs/2, the rest basic groups with uid -1..-dialogs/2), each a TL_message text blob with an
// entity and a trailer, every 4th one outgoing; an incoming group message is from one of the users. It returns the
// rows of each table.
func genTelegram(path string, n, dialogs, users, chats int64) (calGen, error) {
	db, err := openGen(path)
	if err != nil {
		return calGen{}, err
	}
	fail := func(err error) (calGen, error) { _ = db.Close(); return calGen{}, err }
	if err := execScript(db, tgDDL); err != nil {
		return fail(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 181"); err != nil {
		return fail(err)
	}
	if err := genRows(db, users, "INSERT INTO users (uid, name, status, data) VALUES (?,?,?,?)", func(id int64) []any {
		return []any{id, fmt.Sprintf("user %d", id), int64(-100), tgUser(id)}
	}); err != nil {
		return fail(err)
	}
	if err := genRows(db, chats, "INSERT INTO chats (uid, name, data) VALUES (?,?,?)", func(id int64) []any {
		return []any{id, fmt.Sprintf("group %d", id), tgChat(id)}
	}); err != nil {
		return fail(err)
	}
	r, pool := rand.New(rand.NewPCG(14, 4)), bodyPool() //nolint:gosec // a fixed seed: the generated input is repeatable
	half := dialogs / 2
	err = genRows(db, n, "INSERT INTO messages_v2 (mid, uid, read_state, send_state, date, data, out, ttl, media, imp, mention, forwards, thread_reply_id, is_channel, reply_to_message_id, group_id, reply_to_story_id) VALUES (?,?,3,0,?,?,?,0,-1,0,0,0,0,0,0,0,0)", func(id int64) []any {
		d := (id - 1) % dialogs
		uid := d + 1
		if d >= half {
			uid = -(d - half + 1)
		}
		out := id%4 == 0
		var from int64
		switch {
		case out:
		case uid > 0:
			from = uid
		default:
			from = id%users + 1
		}
		date := int32(1_600_000_000 + id*30)
		off := r.IntN(len(pool) - 100)
		outCol := int64(0)
		if out {
			outCol = 1
		}
		return []any{id, uid, int64(date), tgMessage(int32(id), from, uid, out, date, pool[off:off+20+r.IntN(60)]), outCol}
	})
	if err != nil {
		return fail(err)
	}
	if err := db.Close(); err != nil {
		return calGen{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return calGen{}, err
	}
	return calGen{Bytes: st.Size(), Rows: map[string]int64{"messages_v2": n, "users": users, "chats": chats, "dialogs": dialogs}}, nil
}

// runGenTelegram is the telegram-gen phase: the 1,000,000-message cache4.db under MINUTIAE_SCALE_DIR/input. Under
// -short it skips.
func runGenTelegram(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("telegram-gen writes a database of several hundred MiB: skipped under -short")
	}
	if err := os.MkdirAll(filepath.Join(e.dir, "input"), 0o750); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	g, err := genTelegram(genPath(e, "telegram-1m.db"), tgMessages, tgDialogs, tgUsers, tgChats)
	if err != nil {
		t.Fatalf("gen telegram-1m.db: %v", err)
	}
	t.Logf("gen telegram-1m.db: %d bytes, rows %v in %s", g.Bytes, g.Rows, time.Since(start).Round(time.Second))
	b, _ := json.MarshalIndent(g, "", "  ")
	if err := os.WriteFile(genPath(e, "telegram-gen.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(t, e, phaseResult{Phase: "telegram-gen", Outcome: "generated", InputBytes: g.Bytes, Rows: g.Rows["messages_v2"], TableRows: g.Rows})
}

// stateCheckWriter is countWriter that also counts the records of each type, those whose raw names an undecided join
// and those with a budget state (raw.data_state or any raw.<col>_state "budget"; F8: every decode's charge is freed
// with its row, so a budget state would be a refused single decode).
type stateCheckWriter struct {
	countWriter
	undecided, budget int64
	types             map[string]int64
}

func (w *stateCheckWriter) Add(ctx context.Context, r records.Record) error {
	if w.types == nil {
		w.types = map[string]int64{}
	}
	w.types[r.Type]++
	if raw, ok := r.Payload["raw"].(map[string]any); ok {
		if _, has := raw["join_undecided"]; has {
			w.undecided++
		}
		for k, v := range raw {
			if strings.HasSuffix(k, "_state") && v == "budget" {
				w.budget++
				break
			}
		}
	}
	return w.countWriter.Add(ctx, r)
}

// telegramDecode is the telegram-decode phase: the 1,000,000-message store parsed with MemBudget 1 GiB, counting what
// the parser emits, the records with an undecided join and those with a budget state (there must be none of either).
// The parser is registered under a synthetic source hash (it is not in the release registry yet); the phase fails when
// the database file plus the peak resident set exceed 2 GiB. Under -short it skips.
func telegramDecode(t *testing.T, e scaleEnv) {
	t.Helper()
	if testing.Short() {
		t.Skip("telegram-decode parses a database of several hundred MiB with a 1 GiB budget: skipped under -short")
	}
	telegramDecodeRun(t, e)
}

// telegramDecodeRun is the body of telegram-decode on whatever store telegram-gen.json describes (TestScaleParseSmoke
// runs it on a small one, under -short too).
func telegramDecodeRun(t *testing.T, e scaleEnv) {
	t.Helper()
	b, err := os.ReadFile(genPath(e, "telegram-gen.json")) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatalf("run the telegram-gen phase first: %v", err)
	}
	var g calGen
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	c := newScaleCase(t, filepath.Join(e.dir, "cases"), "TELEGRAMDECODE")
	captureFile(t, c, tgRemotePath, genPath(e, "telegram-1m.db"))
	reg, err := artparse.Register(androidtelegram.Parser{}, parsertest.FakeHash(androidtelegram.Name))
	if err != nil {
		t.Fatal(err)
	}
	lim := parse.DefaultLimits()
	lim.MemBudget = tgBudget
	opt := artparse.Options{Limits: lim}
	w := &stateCheckWriter{}
	artparse.WithNewWriter(&opt, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
		return w, nil
	})
	h, err := artparse.New(c, []artparse.Registered{reg}, opt)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory() // hand back the garbage of the setup (generation, capture) before the measured run
	// the collector's memory limit as the CLI sets it (internal/cli/parse_recover.go) and runHost does: twice the budget
	prev := debug.SetMemoryLimit(min(debug.SetMemoryLimit(-1), 2*lim.MemBudget))
	defer debug.SetMemoryLimit(prev)
	stop := sampleHeap()
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	wall := time.Since(start)
	live, total := stop()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rss, ok := peakRSS()
	t.Logf("telegram-decode: peak live heap objects %d MiB, peak Go-mapped memory %d MiB (runtime/metrics, sampled every 50 ms)", live/scaleMiB, total/scaleMiB)
	res := phaseResult{Phase: "telegram-decode", MemBudget: tgBudget, InputBytes: g.Bytes, Rows: g.Rows["messages_v2"], PeakRSSLimit: tgSizeCap - g.Bytes, TableRows: g.Rows}
	fillResult(&res, sum, wall, rss, ok)
	res.Streamed = streamedOf(t, c)
	writeResult(t, e, res)
	t.Logf("telegram-decode: records by type %v, undecided joins %d, budget states %d", w.types, w.undecided, w.budget)
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(sum.Jobs))
	}
	if res.Outcome != artparse.OutcomeComplete {
		t.Errorf("outcome %s (%s), want complete", res.Outcome, res.Reason)
	}
	if w.types["message"] != g.Rows["messages_v2"] || w.types["contact"] != g.Rows["users"] || res.Records != g.Rows["messages_v2"]+g.Rows["users"] {
		t.Errorf("records %v (%d in all), want one message per messages_v2 row (%d) and one contact per users row (%d)", w.types, res.Records, g.Rows["messages_v2"], g.Rows["users"])
	}
	if w.undecided != 0 {
		t.Errorf("%d records name an undecided join: every dialog's users or chats row exists", w.undecided)
	}
	if w.budget != 0 {
		t.Errorf("%d records state budget: a decode's charge outlived its row (F8)", w.budget)
	}
	checkRSS(t, res) // the file plus the peak resident set within 2 GiB; skips where the platform cannot read it
}

// sampleHeap samples runtime/metrics every 50 ms until the returned stop is called, which gives the highest live heap
// (/memory/classes/heap/objects:bytes, the bytes of objects the last collection kept plus those allocated since) and the
// highest memory the Go runtime had mapped (/memory/classes/total:bytes) seen: what of a peak resident set is the Go
// heap, and how much of it is garbage the collector had not yet taken.
func sampleHeap() (stop func() (live, total int64)) {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
	done, out := make(chan struct{}), make(chan [2]int64)
	go func() {
		var peak [2]int64
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			metrics.Read(samples)
			for i, s := range samples {
				if s.Value.Kind() == metrics.KindUint64 {
					peak[i] = max(peak[i], int64(s.Value.Uint64()))
				}
			}
			select {
			case <-done:
				out <- peak
				return
			case <-tick.C:
			}
		}
	}()
	return func() (int64, int64) {
		close(done)
		p := <-out
		return p[0], p[1]
	}
}
