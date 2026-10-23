package records_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type perfTarget struct {
	ID    string  `json:"id"`
	Op    string  `json:"op"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type perfRun struct {
	Label   string             `json:"label"`
	Commit  string             `json:"commit"`
	Machine string             `json:"machine"`
	Records int64              `json:"records"`
	Values  map[string]float64 `json:"values"`
	// Unfinished names the rows whose phase did not end (a timeout); such a row has no value and its
	// decision stays pending: an unfinished phase is never turned into a number.
	Unfinished []string `json:"unfinished,omitempty"`
}

type perfDoc struct {
	Schema    string            `json:"schema"`
	Targets   []perfTarget      `json:"targets"`
	Runs      []perfRun         `json:"runs"`
	Decisions map[string]string `json:"decisions"`
}

// perfRules: a decision is "apply" when ANY of its (run, row) pairs missed its target, "skip" when
// none did, and "pending" only while one of its runs does not exist yet (ruling D17: the ingest
// decision reads the 10M baseline as well as the 2M after-pragmas run; ruling D24, superseded by C2 of 5D-14m: the parallel-verify
// decision is keyed to the verify-10m-parallel run (the final 5D verify, after the parallel redesign of Tasks 15a to 15c; 5D-15d, which supersedes the verify-10m key of 5D-14m), never to final-post).
type perfRow struct{ run, id string }

var perfRules = []struct {
	decision string
	rows     []perfRow
}{
	{"stats_plan", []perfRow{{"baseline", "stats_type_s"}}},
	{"search_plan", []perfRow{{"baseline", "search_word_p95_ms"}, {"baseline", "search_substring_p95_ms"}}},
	{"open_artifact_cache", []perfRow{{"baseline", "open_artifact_ms"}}},
	{"ingest_hot_path", []perfRow{{"baseline", "ingest_rate"}, {"after-pragmas", "ingest_rate"}}},
	{"parallel_verify", []perfRow{{"verify-10m-parallel", "verify_rate"}}},
}

// knownRuns are the only run labels a record may hold. final-post is the whole final run repeated
// after Task 15, labelled with the code it measures. phases-2m is the 2M per-phase verify run of 5D-14c.
var knownRuns = map[string]bool{"baseline": true, "after-pragmas": true, "after-hot-path": true, "final": true, "final-post": true, "phases-2m": true, "verify-2m": true, "verify-10m": true, "verify-2m-parallel": true, "verify-10m-parallel": true, "search-stats-10m": true}

// specTargets are the bars of spec section 11, frozen here so an edit of the document cannot move one.
var specTargets = map[string]perfTarget{
	"ingest_rate":             {"ingest_rate", ">=", 40000, "records/s"},
	"list_p95_ms":             {"list_p95_ms", "<=", 50, "ms"},
	"search_word_p95_ms":      {"search_word_p95_ms", "<=", 300, "ms"},
	"search_substring_p95_ms": {"search_substring_p95_ms", "<=", 1000, "ms"},
	"stats_type_s":            {"stats_type_s", "<=", 3, "s"},
	"verify_rate":             {"verify_rate", ">=", 3700, "records/s"},
	"db_ratio":                {"db_ratio", "<=", 1.6, "x"},
	"open_artifact_ms":        {"open_artifact_ms", "<=", 1, "ms"},
}

var perfBlock = regexp.MustCompile("(?s)```json minutiae-perf-v1\n(.*?)\n```")

func loadPerf(t *testing.T) perfDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "performance.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := perfBlock.FindSubmatch(raw)
	if m == nil {
		t.Fatal("docs/performance.md has no ```json minutiae-perf-v1 block")
	}
	var d perfDoc
	if err := json.Unmarshal(m[1], &d); err != nil {
		t.Fatalf("performance block: %v", err)
	}
	return d
}

func (d perfDoc) missed(t *testing.T, r perfRun, id string) bool {
	t.Helper()
	for _, tg := range d.Targets {
		if tg.ID != id {
			continue
		}
		v, ok := r.Values[id]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("run %q has no usable value for %s", r.Label, id)
			return false
		}
		switch tg.Op {
		case ">=":
			return v < tg.Value
		case "<=":
			return v > tg.Value
		}
		t.Fatalf("target %s has operator %q", id, tg.Op)
	}
	t.Fatalf("no target %s", id)
	return false
}

func TestPerformanceRecordIsConsistent(t *testing.T) {
	d := loadPerf(t)
	if d.Schema != "minutiae-perf-v1" {
		t.Fatalf("schema %q", d.Schema)
	}
	if len(d.Targets) != len(specTargets) {
		t.Fatalf("%d targets, want %d", len(d.Targets), len(specTargets))
	}
	for _, tg := range d.Targets {
		if want, ok := specTargets[tg.ID]; !ok || tg != want {
			t.Errorf("target %+v differs from the spec bar %+v", tg, want)
		}
	}
	runs := map[string]perfRun{}
	for _, r := range d.Runs {
		if _, dup := runs[r.Label]; dup {
			t.Errorf("run label %q appears twice", r.Label)
		}
		runs[r.Label] = r
		if !knownRuns[r.Label] {
			t.Errorf("run label %q is not one of the known labels", r.Label)
		}
		if r.Machine == "" || r.Commit == "" || r.Records <= 0 {
			t.Errorf("run %q must record machine, commit and record count: %+v", r.Label, r)
		}
	}
	for _, label := range []string{"final", "final-post"} {
		if f, ok := runs[label]; ok {
			for id := range specTargets {
				if id == "open_artifact_ms" {
					continue
				}
				if slices.Contains(f.Unfinished, id) {
					if _, has := f.Values[id]; has {
						t.Errorf("the %s run lists %s as unfinished but records a value", label, id)
					}
					continue
				}
				if _, ok := f.Values[id]; !ok {
					t.Errorf("the %s run has no value for %s", label, id)
				}
			}
		}
	}
	if len(d.Decisions) != len(perfRules) {
		t.Fatalf("decisions has %d keys, want %d", len(d.Decisions), len(perfRules))
	}
	for _, rule := range perfRules {
		got, ok := d.Decisions[rule.decision]
		if !ok {
			t.Errorf("no decision %s", rule.decision)
			continue
		}
		want, missing := "skip", false
		for _, row := range rule.rows {
			run, have := runs[row.run]
			if !have {
				missing = true
				continue
			}
			if slices.Contains(run.Unfinished, row.id) {
				missing = true
				continue
			}
			if d.missed(t, run, row.id) {
				want = "apply"
			}
		}
		if missing && want == "skip" {
			want = "pending" // a run is not recorded yet and no recorded row has missed
		}
		if got != want && (want != "pending" || !strings.HasPrefix(got, "pending: ")) {
			t.Errorf("decision %s is %q, but the recorded numbers say %q", rule.decision, got, want)
		}
	}
}
