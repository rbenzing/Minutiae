//go:build scale

package records_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const (
	scaleCaseID   = "SCALE"
	scaleEpoch    = 1_577_836_800 // the workload starts at 2020-01-01T00:00:00Z
	cellBudget    = 120 * time.Second
	searchTimeout = 20 * time.Second
	superRecords  = 1000
)

var scaleTypes = []string{"message", "web_visit", "call", "location", "file", "event", "contact", "calendar_event", "note"}

type scaleState struct {
	t       *testing.T
	res     *scaleResult
	n       int64
	samples int
	dir     string
	c       *evidence.Case
	rd      *records.Reader
	gen     *recordstest.Scale
	ids     []string
	total   int64 // records the open case must hold
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, name string, def int64) int64 {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		t.Fatalf("%s=%q is not a non-negative integer", name, s)
	}
	return v
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func TestScale(t *testing.T) {
	label := os.Getenv("MINUTIAE_SCALE_LABEL")
	if label == "" {
		t.Skip("set MINUTIAE_SCALE_LABEL (baseline, after-pragmas, final, ...) to run the scale test; it is manual (see docs/testing.md)")
	}
	machine := os.Getenv("MINUTIAE_SCALE_MACHINE")
	if machine == "" {
		t.Fatal("set MINUTIAE_SCALE_MACHINE to the CPU, RAM and disk of this machine: a number without its machine is not a record")
	}
	dir := os.Getenv("MINUTIAE_SCALE_DIR")
	if dir == "" {
		t.Fatal("set MINUTIAE_SCALE_DIR to a directory under the session scratchpad: a scale case is large and never lives in the repository or the OS temp directory")
	}
	// The OS temp directory is listed before and after ingest, reindex and verify (ruling D46): the
	// listing proves something only when it is a directory no other process writes to, inside the
	// scale directory and outside both cases.
	if tmp := os.TempDir(); !pathWithin(dir, tmp) || pathWithin(filepath.Join(dir, "clean"), tmp) || pathWithin(filepath.Join(dir, "mut"), tmp) {
		t.Fatalf("os.TempDir() = %q: set TMP, TEMP and TMPDIR to a directory under MINUTIAE_SCALE_DIR (%q) but outside clean/ and mut/, so the before/after listing of the OS temp directory measures this run only", tmp, dir)
	}
	n := envInt(t, "MINUTIAE_SCALE_N", 10_000_000)
	phases := map[string]bool{}
	for _, p := range strings.Split(envOr("MINUTIAE_SCALE_PHASES", "ingest,queries,search,stats,untouched,verify"), ",") {
		phases[strings.TrimSpace(p)] = true
	}
	res := newScaleResult(label, gitCommit(), machine, n)
	defer func() { // partial results survive a failure
		if out := os.Getenv("MINUTIAE_SCALE_OUT"); out != "" {
			b, _ := json.MarshalIndent(res, "", "  ")
			_ = os.WriteFile(out, b, 0o600) //nolint:gosec // the result file path is chosen by the operator
		}
	}()
	st := &scaleState{t: t, res: res, n: n, samples: int(envInt(t, "MINUTIAE_SCALE_SAMPLES", 200)), dir: dir, total: n}

	clean := filepath.Join(dir, "clean", scaleCaseID)
	mutRoot := filepath.Join(dir, "mut")
	mut := filepath.Join(mutRoot, scaleCaseID)
	plan, err := planScalePhases(phases, pristineMarkerExists(mutRoot))
	if err != nil {
		t.Fatal(err)
	}
	mutating := plan.mutating
	if plan.copyOnly {
		copyCase(t, clean, mut)
		if err := writePristineMarker(mutRoot); err != nil {
			t.Fatal(err)
		}
		t.Logf("copied the clean case to %s", mut)
		return // the copy is its own run (it holds no case open and measures nothing)
	}
	if phases["ingest"] {
		endGuard := st.guardOSTemp("ingest")
		st.ingest(int(envInt(t, "MINUTIAE_SCALE_BATCH_ROWS", 0)))
		endGuard()
	} else {
		target := clean
		if mutating {
			target = mut
		}
		st.open(target)
	}
	defer func() { _ = st.c.Close() }()
	if mutating { // the copy is pristine until this run: a second mutating run is refused
		if err := consumePristineMarker(mutRoot); err != nil {
			t.Fatal(err)
		}
	}

	if phases["queries"] {
		st.listMatrix("clean")
	}
	if phases["search"] {
		st.searchMatrix()
	}
	if phases["probes"] {
		st.probeCosts()
	}
	if phases["stats"] {
		st.statsAndGet()
	}
	if phases["untouched"] {
		st.queriesLeaveCaseUntouched()
	}
	if phases["verify"] {
		st.verify("verify", st.total, true)
	}
	if phases["reindex"] {
		st.reindex()
	}
	if phases["supersede"] {
		st.supersede()
	}
	if phases["superqueries"] {
		st.listMatrix("super")
		st.superStats()
	}
	if phases["tamper"] {
		st.tamper()
	}
	if phases["tamper64"] {
		st.tamper64()
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil } //nolint:gosec // a scale case path chosen by the operator

// copyCase copies a case directory file by file (the lock file is not copied).
func copyCase(t *testing.T, from, to string) {
	t.Helper()
	if !exists(from) {
		t.Fatalf("no clean case at %s: run the ingest phase first", from)
	}
	if exists(to) {
		t.Fatalf("%s exists: delete the old copy first", to)
	}
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // copies the scale case this harness made
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750) //nolint:gosec // creates the copy of the scale case
		}
		if d.Name() == "case.lock" {
			return nil
		}
		in, err := os.Open(p) //nolint:gosec // copies the scale case this harness made
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // creates the copy of the scale case
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (st *scaleState) newReader() {
	rd, err := records.NewReader(st.c)
	if err != nil {
		st.t.Fatal(err)
	}
	st.rd = rd
}

func (st *scaleState) ingest(batchRows int) {
	t := st.t
	parent := filepath.Join(st.dir, "clean")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	c, err := evidence.Create(parent, evidence.CreateOptions{ID: scaleCaseID, Examiner: "Scale"})
	if err != nil {
		t.Fatal(err)
	}
	st.c = c
	st.ids = recordstest.AddScaleArtifacts(t, c, 40)
	st.gen = recordstest.NewScale(recordstest.ScaleConfig{Seed: 5, Artifacts: st.ids})
	w, err := records.NewWriter(c, records.Parser{Name: "scale-parser", Version: "1"}, records.WriterOptions{BatchRows: batchRows, RelaxedSync: os.Getenv("MINUTIAE_SCALE_RELAXED_SYNC") == "1"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := w.Start(ctx, records.StartOptions{Artifacts: st.ids}); err != nil {
		t.Fatal(err)
	}
	st.res.Values["start_s"] = time.Since(t0).Seconds()

	const blk = 5000
	blocks := make(chan []records.Record, 4)
	go func() {
		defer close(blocks)
		for from := int64(0); from < st.n; from += blk {
			blocks <- st.gen.Block(from, int(min(blk, st.n-from)))
		}
	}()
	var spent time.Duration
	var rowBytes int64
	for b := range blocks {
		for _, r := range b {
			rowBytes += recordstest.RowBytes(r) // outside the timer: bookkeeping, not ingest
		}
		t0 := time.Now()
		for _, r := range b {
			if err := w.Add(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		spent += time.Since(t0) // generator CPU is excluded: only Add (and the flushes it triggers) counts
	}
	t0 = time.Now()
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	spent += time.Since(t0)
	st.res.Values["ingest_rate"] = float64(st.n) / spent.Seconds()
	st.res.Values["ingest_s"] = spent.Seconds()
	fi, err := os.Stat(filepath.Join(c.Dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.res.Values["db_ratio"] = float64(fi.Size()) / float64(rowBytes+64*st.n)
	st.newReader()
}

// open opens an existing case and rebuilds the generator from its manifest (the first 40 artifacts
// named scale<i>.db are the workload's).
func (st *scaleState) open(path string) {
	t := st.t
	c, err := evidence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.c = c
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if strings.HasPrefix(filepath.Base(r.Path), "scale") && strings.HasSuffix(r.Path, ".db") && len(st.ids) < 40 {
			st.ids = append(st.ids, r.ID)
		}
	}
	if len(st.ids) != 40 {
		t.Fatalf("found %d workload artifacts in %s, want 40", len(st.ids), path)
	}
	st.gen = recordstest.NewScale(recordstest.ScaleConfig{Seed: 5, Artifacts: st.ids})
	st.newReader()
	got, _, err := st.rd.Count(ctx, records.Filter{IncludeSuperseded: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != st.n && got != st.n+superRecords {
		t.Fatalf("%s holds %d records, MINUTIAE_SCALE_N says %d", path, got, st.n)
	}
	st.total = got
}

type listCell struct {
	name    string
	indexed bool // hits a time-ordered index: part of the list_p95_ms target (spec: "any filter combination that hits an index")
	f       func(i int) records.Filter
}

func (st *scaleState) listCells() []listCell {
	rng := rand.New(rand.NewPCG(9, 9)) //nolint:gosec // a deterministic workload, not a secret
	const pool = 1000
	start := make([]int64, pool)
	for i := range start {
		start[i] = scaleEpoch + rng.Int64N(150_000_000)
	}
	window := func(i int, days int) (*time.Time, *time.Time) {
		from := time.Unix(start[i%pool], 0).UTC()
		to := from.Add(time.Duration(days) * 24 * time.Hour)
		return &from, &to
	}
	tp := func(i, k int) []string {
		out := make([]string, k)
		for j := range out {
			out[j] = scaleTypes[(i+3*j)%len(scaleTypes)] // k <= 3: distinct
		}
		return out
	}
	conf := 50
	return []listCell{
		{"none", true, func(int) records.Filter { return records.Filter{} }},
		{"type1", true, func(i int) records.Filter { return records.Filter{Types: tp(i, 1)} }},
		{"type2", true, func(i int) records.Filter { return records.Filter{Types: tp(i, 2)} }},
		{"type3", true, func(i int) records.Filter { return records.Filter{Types: tp(i, 3)} }},
		{"deleted", true, func(int) records.Filter { return records.Filter{Deleted: records.Only} }},
		{"time", true, func(i int) records.Filter { a, b := window(i, 7); return records.Filter{From: a, To: b} }},
		{"type+time", true, func(i int) records.Filter {
			a, b := window(i, 30)
			return records.Filter{Types: tp(i, 1), From: a, To: b}
		}},
		{"minconf+type", true, func(i int) records.Filter { return records.Filter{Types: tp(i, 1), MinConfidence: &conf} }},
		{"artifact_big", false, func(int) records.Filter { return records.Filter{ArtifactIDs: st.ids[:1]} }},
		{"artifact_tiny", false, func(int) records.Filter { return records.Filter{ArtifactIDs: []string{st.gen.TinyArtifact()}} }},
		{"artifacts3", false, func(int) records.Filter {
			return records.Filter{ArtifactIDs: []string{st.ids[0], st.ids[1], st.ids[5]}}
		}},
		{"parser", false, func(int) records.Filter { return records.Filter{Parsers: []records.ParserRef{{Name: "scale-parser"}}} }},
		{"path+artifact", false, func(int) records.Filter {
			return records.Filter{PathPrefix: "/data/12/", ArtifactIDs: st.ids[:1]}
		}},
		{"time+untimed", false, func(i int) records.Filter {
			a, b := window(i, 7)
			return records.Filter{From: a, To: b, IncludeUntimed: true}
		}},
	}
}

func (st *scaleState) listMatrix(tag string) {
	t := st.t
	worst := 0.0
	for _, c := range st.listCells() {
		if tag == "super" && c.name != "none" && c.name != "type1" && c.name != "deleted" {
			continue
		}
		ms, err := timeIt(st.samples, cellBudget, func(i int) error {
			_, err := st.rd.List(ctx, c.f(i), records.Page{Limit: 100})
			return err
		})
		if err != nil {
			t.Fatalf("list %s: %v", c.name, err)
		}
		key := "matrix.list." + tag + "." + c.name
		st.res.Values[key+".p50_ms"] = pct(ms, 0.5)
		st.res.Values[key+".p95_ms"] = pct(ms, 0.95)
		st.res.Values[key+".samples"] = float64(len(ms))
		if c.indexed {
			worst = max(worst, pct(ms, 0.95))
		}
		if qs, err := records.ListQueries(c.f(0), false, nil, 100, tag == "super"); err == nil && len(qs) > 0 {
			st.res.Plans[tag+"."+c.name] = explain(t, st.c, qs[0].SQL, qs[0].Args)
		}
	}
	if tag != "clean" {
		return
	}
	if st.total >= int64(30*records.MaxLimit)+1000 { // the deep cell needs 30 full pages before it
		cur := ""
		for p := 0; p < 30; p++ {
			r, err := st.rd.List(ctx, records.Filter{}, records.Page{Limit: records.MaxLimit, Cursor: cur})
			if err != nil {
				t.Fatal(err)
			}
			cur = r.NextCursor
		}
		ms, err := timeIt(st.samples, cellBudget, func(int) error {
			_, err := st.rd.List(ctx, records.Filter{}, records.Page{Limit: 100, Cursor: cur})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		st.res.Values["matrix.list.clean.deep.p95_ms"] = pct(ms, 0.95)
		worst = max(worst, pct(ms, 0.95))
	} else {
		t.Logf("deep cell skipped: %d records are too few for 30 pages of %d", st.total, records.MaxLimit)
	}
	st.res.Values["list_p95_ms"] = worst
}

func (st *scaleState) searchMatrix() {
	t := st.t
	compile := func(in string, sub bool) *records.TextQuery {
		q, err := records.CompileQuery(in, records.TextOptions{Substring: sub})
		if err != nil {
			t.Fatalf("compile %q: %v", in, err)
		}
		return q
	}
	common, mid, rare := st.gen.Word(20), st.gen.Word(2000), st.gen.Word(15000)
	qCommon, qMid, qRare := compile(common, false), compile(mid, false), compile(rare, false)
	qPrefix := compile(mid[:3]+"*", false)
	qSubCommon, qSubMid := compile(common[1:5], true), compile(mid[1:5], true)
	one := func(i int) []string { return []string{scaleTypes[i%len(scaleTypes)]} }
	win := func(i int) (*time.Time, *time.Time) {
		from := time.Unix(scaleEpoch+int64(i*977_113%150_000_000), 0).UTC()
		to := from.Add(30 * 24 * time.Hour)
		return &from, &to
	}
	cells := []struct {
		name, kind string
		f          func(i int) records.Filter
	}{
		{"word.common.type", "word", func(i int) records.Filter { return records.Filter{Types: one(i), Text: qCommon} }},
		{"word.mid.type", "word", func(i int) records.Filter { return records.Filter{Types: one(i), Text: qMid} }},
		{"word.rare.type", "word", func(i int) records.Filter { return records.Filter{Types: one(i), Text: qRare} }},
		{"word.mid.time", "word", func(i int) records.Filter { a, b := win(i); return records.Filter{From: a, To: b, Text: qMid} }},
		{"word.common.time", "word", func(i int) records.Filter { a, b := win(i); return records.Filter{From: a, To: b, Text: qCommon} }},
		{"prefix.mid.type", "word", func(i int) records.Filter { return records.Filter{Types: one(i), Text: qPrefix} }},
		{"substring.common", "substring", func(int) records.Filter { return records.Filter{Text: qSubCommon} }},
		{"substring.mid", "substring", func(int) records.Filter { return records.Filter{Text: qSubMid} }},
		{"substring.mid.type", "substring", func(i int) records.Filter { return records.Filter{Types: one(i), Text: qSubMid} }},
		// D98: a common word with a rare filter. The plan choice looks only at the number of full-text
		// hits, so these cells measure what that costs; they are not part of the two spec rows.
		{"selective.common.deleted", "selective", func(int) records.Filter { return records.Filter{Deleted: records.Only, Text: qCommon} }},
		{"selective.common.artifact_tiny", "selective", func(int) records.Filter {
			return records.Filter{ArtifactIDs: []string{st.gen.TinyArtifact()}, Text: qCommon}
		}},
		{"selective.common.typepair", "selective", func(int) records.Filter {
			return records.Filter{Types: []string{"calendar_event", "note"}, Text: qCommon}
		}},
		// S13: a path prefix is not served by an index, so the filter is never probed and keeps the hint.
		{"selective.common.pathprefix", "selective", func(int) records.Filter {
			return records.Filter{PathPrefix: "/data/7/", Text: qCommon}
		}},
	}
	word, sub := 0.0, 0.0
	for _, c := range cells {
		timeouts := 0
		ms, err := timeIt(st.samples, cellBudget, func(i int) error {
			qctx, cancel := context.WithTimeout(ctx, searchTimeout)
			defer cancel()
			_, err := st.rd.Search(qctx, c.f(i), records.Page{Limit: 100}, records.SearchOptions{})
			if errors.Is(err, records.ErrSearchTimeout) || errors.Is(err, context.DeadlineExceeded) {
				timeouts++ // a miss: the elapsed time is the timeout, and it is recorded
				return nil
			}
			return err
		})
		if err != nil {
			t.Fatalf("search %s: %v", c.name, err)
		}
		key := "matrix.search." + c.name
		p95 := pct(ms, 0.95)
		st.res.Values[key+".p95_ms"] = p95
		st.res.Values[key+".samples"] = float64(len(ms))
		st.res.Values[key+".timeouts"] = float64(timeouts)
		switch c.kind {
		case "word":
			word = max(word, p95)
		case "substring":
			sub = max(sub, p95)
		}
	}
	st.res.Values["search_word_p95_ms"] = word
	st.res.Values["search_substring_p95_ms"] = sub
}

// probeCosts times the bounded probes of a search page on the common, mid and prefix queries (14n/S6):
// the first call alone (cold) and the mean of probeReps further calls, in ms. The clock of the platform
// ticks in steps of several ms, so a single call reads as a multiple of the step and the mean is the
// figure to read. A tiny type filter and a deleted-only filter are measured beside the unfiltered
// query, because the filter probe only runs for an indexed filter.
func (st *scaleState) probeCosts() {
	t := st.t
	const probeReps = 20
	compile := func(in string) *records.TextQuery {
		q, err := records.CompileQuery(in, records.TextOptions{})
		if err != nil {
			t.Fatalf("compile %q: %v", in, err)
		}
		return q
	}
	common, mid := st.gen.Word(20), st.gen.Word(2000)
	cells := []struct {
		name string
		f    records.Filter
	}{
		{"common", records.Filter{Text: compile(common)}},
		{"mid", records.Filter{Text: compile(mid)}},
		{"prefix", records.Filter{Text: compile(mid[:3] + "*")}},
		{"common.type", records.Filter{Types: []string{"note"}, Text: compile(common)}},
		{"common.deleted", records.Filter{Deleted: records.Only, Text: compile(common)}},
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	for _, c := range cells {
		pc, err := st.rd.ScaleProbeCost(ctx, c.f, probeReps)
		if err != nil {
			t.Fatalf("probe cost %s: %v", c.name, err)
		}
		key := "probe." + c.name
		st.res.Values[key+".text4096_cold_ms"], st.res.Values[key+".text4096_mean_ms"] = ms(pc.TextSmallCold), ms(pc.TextSmallMean)
		st.res.Values[key+".text262145_cold_ms"], st.res.Values[key+".text262145_mean_ms"] = ms(pc.TextLargeCold), ms(pc.TextLargeMean)
		st.res.Values[key+".hits4096"], st.res.Values[key+".hits262145"] = float64(pc.HitsSmall), float64(pc.HitsLarge)
		if pc.FilterProbed {
			st.res.Values[key+".filter_cold_ms"], st.res.Values[key+".filter_mean_ms"] = ms(pc.FilterCold), ms(pc.FilterMean)
			tiny := 0.0
			if pc.Tiny {
				tiny = 1
			}
			st.res.Values[key+".filter_tiny"] = tiny
		}
	}
}

func (st *scaleState) statsAndGet() {
	t := st.t
	worst := func(f func() error) float64 {
		w := 0.0
		for i := 0; i < 3; i++ {
			t0 := time.Now()
			if err := f(); err != nil {
				t.Fatal(err)
			}
			w = max(w, time.Since(t0).Seconds())
		}
		return w
	}
	ov := worst(func() error { _, err := st.rd.Overview(ctx, records.Filter{}); return err })
	gr := worst(func() error { _, err := st.rd.Stats(ctx, records.Filter{}, "type"); return err })
	ix := worst(func() error { _, err := st.rd.IndexStatus(ctx); return err })
	st.res.Values["stats.overview_s"], st.res.Values["stats.group_s"], st.res.Values["stats.index_s"] = ov, gr, ix
	st.res.Values["stats_type_s"] = ov + gr + ix // what `records stats --by type` makes the examiner wait for
	rng := rand.New(rand.NewPCG(3, 3))           //nolint:gosec // a deterministic workload, not a secret
	ids := make([]int64, st.samples+3)
	for i := range ids {
		ids[i] = 1 + rng.Int64N(st.total)
	}
	ms, err := timeIt(st.samples, cellBudget, func(i int) error { _, err := st.rd.Get(ctx, ids[i]); return err })
	if err != nil {
		t.Fatal(err)
	}
	st.res.Values["get_p95_ms"] = pct(ms, 0.95)
}

func (st *scaleState) superStats() {
	t0 := time.Now()
	if _, err := st.rd.Overview(ctx, records.Filter{}); err != nil {
		st.t.Fatal(err)
	}
	if _, err := st.rd.Stats(ctx, records.Filter{}, "type"); err != nil {
		st.t.Fatal(err)
	}
	st.res.Values["stats.super_s"] = time.Since(t0).Seconds()
}

// hashTree hashes every file of the case (path and content), except the lock file.
func hashTree(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "case.lock" {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		_, _ = io.WriteString(h, filepath.ToSlash(rel)+"\x00")
		f, err := os.Open(p) //nolint:gosec // reads the scale case this harness made
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// queriesLeaveCaseUntouched is TestQueriesLeaveCaseUntouched at scale: a round of list, search, stats,
// overview and get leaves every byte of the case, and the audit log, as it was.
func (st *scaleState) queriesLeaveCaseUntouched() {
	t := st.t
	snap := func() (string, int) {
		entries, err := st.c.ReadAudit()
		if err != nil {
			t.Fatal(err)
		}
		return hashTree(t, st.c.Dir), len(entries)
	}
	h1, n1 := snap()
	q, err := records.CompileQuery(st.gen.Word(2000), records.TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { _, err := st.rd.List(ctx, records.Filter{}, records.Page{Limit: 100}); return err },
		func() error {
			_, err := st.rd.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 100}, records.SearchOptions{})
			return err
		},
		func() error { _, err := st.rd.Stats(ctx, records.Filter{}, "type"); return err },
		func() error { _, err := st.rd.Overview(ctx, records.Filter{}); return err },
		func() error { _, err := st.rd.Get(ctx, 1); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if h2, n2 := snap(); h1 != h2 || n1 != n2 {
		t.Fatalf("queries changed the case: tree hash equal %v, audit entries %d -> %d", h1 == h2, n1, n2)
	}
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// sampleTmp samples the size of <case>/tmp every 500 ms while verify runs; the maximum is a LOWER
// bound of the scratch space the P11 rebuild used (ruling D21).
func (st *scaleState) sampleTmp() (stop func() (tmp, scratch int64)) {
	var mu sync.Mutex
	var peak, scratchPeak int64
	done, fin := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(fin)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				n := dirSize(filepath.Join(st.c.Dir, "tmp"))
				var sc int64
				if ds, err := filepath.Glob(filepath.Join(st.c.Dir, "tmp", "verify-*")); err == nil {
					for _, d := range ds {
						sc += dirSize(d)
					}
				}
				mu.Lock()
				peak = max(peak, n)
				scratchPeak = max(scratchPeak, sc)
				mu.Unlock()
			}
		}
	}()
	return func() (int64, int64) {
		close(done)
		<-fin
		mu.Lock()
		defer mu.Unlock()
		return peak, scratchPeak
	}
}

// verify runs case verify and records its time. When mustBeOK it requires a clean report with exactly
// want records checked.
func (st *scaleState) verify(name string, want int64, mustBeOK bool) evidence.VerifyReport {
	t := st.t
	phases := map[string]float64{}
	st.c.SetVerifyPhaseHook(func(p string, d time.Duration) {
		phases[p] += d.Seconds()
		t.Logf("verify phase %s %.1fs", p, d.Seconds()) // a run that times out still shows its finished phases
	})
	defer st.c.SetVerifyPhaseHook(nil)
	stop := st.sampleTmp()
	endGuard := st.guardOSTemp(name)
	t0 := time.Now()
	rep, err := st.c.Verify()
	el := time.Since(t0).Seconds()
	endGuard()
	peak, scratch := stop()
	if err != nil {
		t.Fatal(err)
	}
	st.res.Values[name+"_s"] = el
	if name == "verify" {
		st.res.Values["verify_rate"] = float64(rep.RecordsChecked) / el
		if peak > 0 { // a sample of 0 is "not measured" (SQLite spill files need not be visible as directory entries), never a claim of 0 (ruling D21)
			st.res.Values["verify_tmp_bytes"] = float64(peak)
		} else {
			t.Log("verify_tmp_bytes: not measured (the sampler never saw a file under <case>/tmp)")
		}
		if scratch > 0 { // the same lower-bound reading, restricted to <case>/tmp/verify-* (ruling D46, read 2)
			st.res.Values["verify_scratch_bytes"] = float64(scratch)
		} else {
			t.Log("verify_scratch_bytes: not measured (no file under <case>/tmp/verify-* was visible to the sampler)")
		}
		for p, s := range phases {
			st.res.Phases[p] = s
		}
		st.res.Phases["other"] = el - topLevelPhaseSum(phases) // the checks no phase wraps (derived chain, cross checks, ...)
	}
	if mustBeOK {
		if !rep.OK() {
			t.Fatalf("%s: verify reported %d problems, first: %q", name, len(rep.Problems), rep.Problems[:min(20, len(rep.Problems))])
		}
		if int64(rep.RecordsChecked) != want {
			t.Fatalf("%s: verify checked %d records, want %d", name, rep.RecordsChecked, want)
		}
	}
	return rep
}

// supersede ingests a newer complete run of the same parser over one artifact, so the older run's
// records on it are hidden by default and every list, count and stats statement takes the supersession
// path. It runs on the copy only.
func (st *scaleState) supersede() {
	t := st.t
	w, err := records.NewWriter(st.c, records.Parser{Name: "scale-parser", Version: "2"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: st.ids[2:3]}); err != nil {
		t.Fatal(err)
	}
	for _, r := range st.gen.Block(st.n, superRecords) {
		r.ArtifactID = st.ids[2]
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	all, _, err := st.rd.Count(ctx, records.Filter{IncludeSuperseded: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	shown, _, err := st.rd.Count(ctx, records.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if all != st.n+superRecords || shown >= all {
		t.Fatalf("after the newer run: %d records in all (want %d), %d shown by default (must be fewer)", all, st.n+superRecords, shown)
	}
	st.total = all
	st.res.Values["super.hidden_records"] = float64(all - shown)
}

var batchIDRange = regexp.MustCompile(`ids (\d+)\.\.(\d+)`)

// tamper: on the copy, verify must first be clean with the supersession in place; then one record in
// the middle is altered in place and verify must name the batch whose id range holds it.
func (st *scaleState) tamper() {
	t := st.t
	st.verify("verify_super", st.total, true)
	id := st.total / 2
	recordstest.SetRecordSummary(t, st.c.Dir, id, "tampered in place")
	rep := st.verify("verify_tampered", st.total, false)
	if rep.OK() {
		t.Fatalf("verify did not notice record %d altered in place", id)
	}
	found := false
	for _, p := range rep.Problems {
		m := batchIDRange.FindStringSubmatch(p)
		if m == nil || !strings.Contains(p, "digest mismatch") {
			continue
		}
		lo, _ := strconv.ParseInt(m[1], 10, 64)
		hi, _ := strconv.ParseInt(m[2], 10, 64)
		found = found || (lo <= id && id <= hi)
	}
	if !found {
		t.Fatalf("no digest-mismatch problem names the id range holding record %d: %q", id, rep.Problems[:min(10, len(rep.Problems))])
	}
	st.res.Values["tamper_detected"] = 1
}

// guardOSTemp lists the OS temp directory now and returns the function that lists it again at the end
// of a phase (ruling D46, read 1). Any difference is an evidence-text leak: it is recorded
// (ostemp_added_<phase>) and fails the run, never reported as a number to accept. The peak resident
// memory of the process so far is recorded as peak_rss_<phase>_bytes (ruling D50); it is the
// high-water mark of the whole run, so a phase split into its own run reads as that phase's peak.
func (st *scaleState) guardOSTemp(phase string) func() {
	t := st.t
	dir := os.TempDir()
	before, err := snapshotTempDir(dir)
	if err != nil {
		t.Fatalf("listing the OS temp directory %s: %v", dir, err)
	}
	return func() {
		after, err := snapshotTempDir(dir)
		if err != nil {
			t.Fatalf("listing the OS temp directory %s: %v", dir, err)
		}
		added, removed := diffTempListings(before, after)
		st.res.Values["ostemp_added_"+phase] = float64(len(added))
		if peak, ok := peakRSS(); ok {
			st.res.Values["peak_rss_"+phase+"_bytes"] = float64(peak)
		} else {
			t.Logf("peak_rss_%s_bytes: not measured (no peak resident memory read on this platform)", phase)
		}
		if len(added) > 0 || len(removed) > 0 {
			t.Errorf("%s: the OS temp directory %s changed (added %q, removed %q): SQLite temporary data left the case", phase, dir, added, removed)
		}
	}
}

// reindex rebuilds both full-text indexes of the copy (the work of records reindex) under the OS
// temp guard and records its time. It runs on the copy only: it rewrites the index tables.
func (st *scaleState) reindex() {
	t := st.t
	endGuard := st.guardOSTemp("reindex")
	t0 := time.Now()
	res, err := st.c.ReindexText(ctx, evidence.ReindexOptions{})
	st.res.Values["reindex_s"] = time.Since(t0).Seconds()
	endGuard()
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != st.total {
		t.Fatalf("reindex saw %d records, want %d", res.Records, st.total)
	}
}

// tamper64 (D167): on a pristine copy, one record in each of 64 different audited batches is altered in
// place, so at least 64 batches of the full-text index differ from their records. Verify must report
// every one of them as a problem and the localization must stop at its bound with the not-localized
// line. The wall time of the verify, the problems and that line are recorded.
func (st *scaleState) tamper64() {
	t := st.t
	batches := envInt(t, "MINUTIAE_SCALE_TAMPER_BATCHES", 64) // 64 is the localization bound; more shows the not-localized line
	entries, err := st.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	audited := int64(0)
	for _, e := range entries {
		if e.Action == evidence.ActionBatch {
			audited++
		}
	}
	if audited < 1 {
		t.Fatalf("tamper64: the case has no audited batch (%d audit entries)", len(entries))
	}
	if batches < 1 || batches > audited {
		t.Fatalf("tamper64: MINUTIAE_SCALE_TAMPER_BATCHES=%d, want 1..%d (the audited batches of the case)", batches, audited)
	}
	step := st.total / batches
	if size := (st.total + audited - 1) / audited; step < size { // one altered record per batch needs a step of at least a batch
		t.Fatalf("tamper64: %d records apart is less than the batch size %d (%d records in %d audited batches)", step, size, st.total, audited)
	}
	var ids []int64
	for k := int64(0); k < batches; k++ {
		ids = append(ids, k*step+step/2+1)
	}
	for _, id := range ids {
		recordstest.SetRecordSummary(t, st.c.Dir, id, "tampered in place")
	}
	rep := st.verify("verify_tamper64", st.total, false)
	if rep.OK() {
		t.Fatalf("verify did not notice %d records altered in place", len(ids))
	}
	digest, notLocalized := 0, 0
	for _, p := range rep.Problems {
		if strings.Contains(p, "digest mismatch") {
			digest++
		}
		if strings.Contains(p, "not localized") {
			notLocalized++
			t.Logf("not-localized line: %.600s", p)
		}
	}
	t.Logf("tamper64: %d problems, %d digest-mismatch lines, %d not-localized lines", len(rep.Problems), digest, notLocalized)
	for _, p := range rep.Problems[:min(8, len(rep.Problems))] {
		t.Logf("tamper64 problem: %.300s", p)
	}
	st.res.Values["tamper64.batches"] = float64(batches)
	st.res.Values["tamper64.problems"] = float64(len(rep.Problems))
	st.res.Values["tamper64.digest_mismatch_lines"] = float64(digest)
	st.res.Values["tamper64.not_localized_lines"] = float64(notLocalized)
}
