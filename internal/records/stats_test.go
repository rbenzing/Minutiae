package records_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

type group struct {
	count    int64
	min, max int // hours; -1 = none
}

func tally(key func(fx) string, keep func(fx) bool) map[string]group {
	out := map[string]group{}
	for _, r := range fxRows {
		if !keep(r) {
			continue
		}
		k := key(r)
		g, ok := out[k]
		if !ok {
			g = group{min: -1, max: -1}
		}
		g.count++
		if r.hour >= 0 {
			if g.min < 0 || r.hour < g.min {
				g.min = r.hour
			}
			if r.hour > g.max {
				g.max = r.hour
			}
		}
		out[k] = g
	}
	return out
}

func checkStats(t *testing.T, got []records.StatRow, want map[string]group) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d groups %v, want %d %v", len(got), got, len(want), want)
	}
	keys := make([]string, len(got))
	for i, g := range got {
		keys[i] = g.Key
		w, ok := want[g.Key]
		if !ok || g.Count != w.count {
			t.Errorf("group %q: count %d, want %+v", g.Key, g.Count, w)
			continue
		}
		var wmin, wmax *int64
		if w.min >= 0 {
			wmin, wmax = ptr(hourTime(w.min).UnixMicro()), ptr(hourTime(w.max).UnixMicro())
		}
		if !reflect.DeepEqual(g.TSMin, wmin) || !reflect.DeepEqual(g.TSMax, wmax) {
			t.Errorf("group %q: ts range %v..%v, want %v..%v", g.Key, deref(g.TSMin), deref(g.TSMax), deref(wmin), deref(wmax))
		}
	}
	if !slices.IsSorted(keys) {
		t.Errorf("keys not in order: %v", keys)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestStatsByKeys(t *testing.T) {
	f := newFixture(t)
	all := func(fx) bool { return true }
	parserKey := func(r fx) string { return fxParsers[r.parser].Name + "/" + fxParsers[r.parser].Version }
	for _, tc := range []struct {
		by  string
		key func(fx) string
	}{
		{"type", func(r fx) string { return r.typ }},
		{"parser", parserKey},
		{"artifact", func(r fx) string { return f.arts[r.art].ID }},
		{"deleted", func(r fx) string {
			switch {
			case r.recover != "":
				return "recovered"
			case r.deleted:
				return "deleted"
			}
			return "live"
		}},
		{"run", func(r fx) string { return f.ingest[r.parser] }},
	} {
		t.Run(tc.by, func(t *testing.T) {
			got, err := f.r.Stats(ctx, records.Filter{}, tc.by)
			if err != nil {
				t.Fatal(err)
			}
			checkStats(t, got, tally(tc.key, all))
		})
	}
	// a filter narrows the groups; an all-untimed group has no range
	got, err := f.r.Stats(ctx, records.Filter{Types: []string{"web_visit", "call"}, Deleted: records.Only}, "type")
	if err != nil {
		t.Fatal(err)
	}
	checkStats(t, got, tally(func(r fx) string { return r.typ }, func(r fx) bool {
		return (r.typ == "web_visit" || r.typ == "call") && r.deleted
	}))
	if len(got) != 1 || got[0].TSMin != nil || got[0].TSMax != nil || got[0].Count != 1 {
		t.Fatalf("untimed group: %+v", got)
	}
	for _, by := range []string{"", "bogus", "Type", "type; DROP TABLE records", "ts"} {
		if _, err := f.r.Stats(ctx, records.Filter{}, by); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("by %q: %v, want ErrInvalidFilter", by, err)
		}
	}
	// no records, no groups
	got, err = f.r.Stats(ctx, records.Filter{Types: []string{"nope"}}, "type")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
}

func TestOverview(t *testing.T) {
	f := newFixture(t)
	ov, err := f.r.Overview(ctx, records.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := hourTime(1).UnixMicro(), hourTime(6).UnixMicro()
	want := records.Overview{
		Records: 12, Deleted: 4, Recovered: 2, Untimed: 3, SupersededRecords: 0,
		TSMin: &lo, TSMax: &hi, Runs: map[string]int64{"complete": 3},
	}
	if !reflect.DeepEqual(ov, want) {
		t.Fatalf("overview %+v\nwant     %+v", ov, want)
	}
	// narrowed by a filter (the run counts stay case-wide)
	ov, err = f.r.Overview(ctx, records.Filter{Types: []string{"message"}})
	if err != nil {
		t.Fatal(err)
	}
	lo, hi = hourTime(1).UnixMicro(), hourTime(3).UnixMicro()
	want = records.Overview{Records: 4, TSMin: &lo, TSMax: &hi, Runs: map[string]int64{"complete": 3}}
	if !reflect.DeepEqual(ov, want) {
		t.Fatalf("message overview %+v\nwant %+v", ov, want)
	}
	// nothing selected: zero counts and no range
	ov, err = f.r.Overview(ctx, records.Filter{Types: []string{"nope"}})
	if err != nil || ov.Records != 0 || ov.TSMin != nil || ov.TSMax != nil || ov.Runs["complete"] != 3 {
		t.Fatalf("empty overview %+v, %v", ov, err)
	}
	// runs by outcome, including an incomplete one
	tr := newTwoRuns(t, false)
	ov, err = tr.r.Overview(ctx, records.Filter{})
	if err != nil || !reflect.DeepEqual(ov.Runs, map[string]int64{"complete": 1, "incomplete": 1}) || ov.Records != 7 || ov.SupersededRecords != 0 {
		t.Fatalf("two runs, one incomplete: %+v, %v", ov, err)
	}
}

// snapshot hashes every file of the case directory except the lock.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "case.lock" {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a test hashing the files of its own temp case
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQueriesLeaveCaseUntouched(t *testing.T) {
	f := newFixture(t)
	dir := f.c.Dir
	before := snapshot(t, dir)
	for _, name := range []string{"artifacts.db", "manifest.jsonl", "audit.jsonl"} {
		if before[name] == "" {
			t.Fatalf("snapshot lacks %s: %v", name, before)
		}
	}
	entries := len(auditOf(t, f.c, ""))

	for i := 0; i < 3; i++ {
		if _, err := f.r.List(ctx, records.Filter{}, records.Page{Limit: 5}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.List(ctx, records.Filter{Deleted: records.Only}, records.Page{Limit: 2, Desc: true}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.r.Count(ctx, records.Filter{}, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Get(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Get(ctx, 999); !errors.Is(err, records.ErrNotFound) {
			t.Fatal(err)
		}
		for _, by := range []string{"type", "parser", "artifact", "deleted", "run"} {
			if _, err := f.r.Stats(ctx, records.Filter{}, by); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.r.Overview(ctx, records.Filter{}); err != nil {
			t.Fatal(err)
		}
		// the full-text readers write nothing either, and add no audit entry
		tq, err := records.CompileQuery("r0*", records.TextOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := f.r.Search(ctx, records.Filter{Text: tq}, records.Page{Limit: 5}, records.SearchOptions{Snippets: true}); err != nil || len(res.Hits) == 0 {
			t.Fatalf("Search: %d hits, %v", len(res.Hits), err)
		}
		rq, err := records.CompileQuery("r0*", records.TextOptions{Rank: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Search(ctx, records.Filter{Text: rq}, records.Page{Limit: 5}, records.SearchOptions{}); err != nil {
			t.Fatal(err)
		}
		if th, err := f.r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "r01"}, {Text: "r01", Substring: true}}, 5); err != nil || th[0].Count != 1 {
			t.Fatalf("TermHits: %+v, %v", th, err)
		}
		if _, err := f.r.IndexStatus(ctx); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.r.Count(ctx, records.Filter{Text: tq}, 0); err != nil {
			t.Fatal(err)
		}
		// refused input changes nothing either
		_, _ = f.r.List(ctx, records.Filter{}, records.Page{Cursor: "junk"})
	}
	after := snapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		for k, v := range after {
			if before[k] != v {
				t.Errorf("%s changed", k)
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok {
				t.Errorf("%s vanished", k)
			}
		}
		t.Fatal("the case changed")
	}
	if got := len(auditOf(t, f.c, "")); got != entries {
		t.Fatalf("audit log grew from %d to %d entries", entries, got)
	}
}
