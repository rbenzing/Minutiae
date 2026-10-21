package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeRunner returns scripted results and records the order of the calls.
type fakeRunner struct {
	results map[string]RunResult
	err     map[string]error
	ran     []string
	tick    func() // called by every Run: lets a test move the campaign clock
}

func (f *fakeRunner) Run(_ context.Context, it Item, _ Env) (RunResult, error) {
	name := it.Pkg + "." + it.Name
	f.ran = append(f.ran, name)
	if f.tick != nil {
		f.tick()
	}
	if e := f.err[name]; e != nil {
		return RunResult{}, e
	}
	r := f.results[name]
	r.Target = it.Target
	if r.Outcome == "" {
		r.Outcome = OK
	}
	return r, nil
}

func planOf(n int) Plan {
	var items []Item
	for i := range n {
		items = append(items, Item{Target: Target{"internal/p", "Fuzz" + string(rune('A'+i))}, Fuzztime: time.Minute, Minimize: 10 * time.Second})
	}
	return Plan{Items: items, Options: DefaultOptions("all")}
}

func TestCampaignFailsOnAnyNonOKTarget(t *testing.T) {
	for _, bad := range []Outcome{Failed, Stalled, NoProgress, Timeout, BuildFailed, Skipped} {
		r := &fakeRunner{results: map[string]RunResult{"internal/p.FuzzB": {Outcome: bad}}}
		res, code := Campaign(context.Background(), planOf(3), r, Env{}, time.Now().Add(24*time.Hour))
		if code != 1 || len(res) != 3 {
			t.Errorf("%s: exit %d with %d results, want 1 and 3", bad, code, len(res))
		}
	}
	r := &fakeRunner{}
	res, code := Campaign(context.Background(), planOf(3), r, Env{}, time.Now().Add(24*time.Hour))
	if code != 0 || len(res) != 3 {
		t.Errorf("all ok: exit %d with %d results, want 0 and 3", code, len(res))
	}
	if !reflect.DeepEqual(r.ran, []string{"internal/p.FuzzA", "internal/p.FuzzB", "internal/p.FuzzC"}) {
		t.Errorf("targets ran in the order %v", r.ran)
	}
}

func TestCampaignTurnsARunnerErrorIntoAFailedTarget(t *testing.T) {
	r := &fakeRunner{err: map[string]error{"internal/p.FuzzB": errors.New("cannot start go")}}
	res, code := Campaign(context.Background(), planOf(3), r, Env{}, time.Now().Add(24*time.Hour))
	if len(res) != 3 {
		t.Fatalf("%d results, want 3", len(res))
	}
	if code != 1 || res[1].Outcome != Failed || !strings.Contains(res[1].Note, "cannot start go") {
		t.Errorf("exit %d, result %+v; want 1 and failed with the error text", code, res[1])
	}
	if len(r.ran) != 3 {
		t.Errorf("a runner error stopped the campaign: ran %v", r.ran)
	}
}

func TestCampaignDeadlineSkipsAndFails(t *testing.T) {
	// Each target needs fuzztime 1m + minimize 10s + 11m = 12m10s; the deadline leaves room
	// for the first only.
	clock := time.Now()
	old := nowFunc
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = old })
	r := &fakeRunner{tick: func() { clock = clock.Add(12*time.Minute + 10*time.Second) }}
	deadline := clock.Add(13 * time.Minute)
	res, code := Campaign(context.Background(), planOf(3), r, Env{}, deadline)
	if len(res) != 3 {
		t.Fatalf("%d results, want 3", len(res))
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if len(r.ran) != 1 {
		t.Errorf("ran %v, want exactly the first target", r.ran)
	}
	for _, i := range []int{1, 2} {
		if res[i].Outcome != Skipped || !strings.Contains(res[i].Note, "campaign deadline") {
			t.Errorf("result %d = %+v, want skipped with the note", i, res[i])
		}
	}
	out := Summary(res)
	for _, name := range []string{"FuzzB", "FuzzC"} {
		if !strings.Contains(out, name) {
			t.Errorf("the summary does not list the skipped target %s", name)
		}
	}
}

func TestSummaryNamesEveryTargetOnce(t *testing.T) {
	rs := []RunResult{
		{Target: Target{"internal/a", "FuzzOne"}, Outcome: OK, Execs: 10},
		{Target: Target{"internal/a", "FuzzTwo"}, Outcome: Stalled, Execs: 5, Note: "counter frozen"},
		{Target: Target{"internal/b", "FuzzThree"}, Outcome: Failed, Note: "failing input"},
	}
	out := Summary(rs)
	for _, r := range rs {
		if n := strings.Count(out, r.Target.Name); n != 1 {
			t.Errorf("%s appears %d times in the summary:\n%s", r.Target.Name, n, out)
		}
	}
	for _, want := range []string{"ok", "stalled", "failed", "counter frozen"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
}

func TestSummaryIsTerminalSafe(t *testing.T) {
	rs := []RunResult{{
		Target:  Target{"internal/a", "FuzzOne"},
		Outcome: Failed,
		Note:    "bad \x1b[31mred\x1b[0m and \u202egnirts and \x07bell and \u0085next",
	}}
	out := Summary(rs)
	for _, bad := range []string{"\x1b", "\u202e", "\x07", "\u0085"} {
		if strings.Contains(out, bad) {
			t.Errorf("the summary carries the control %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "red") {
		t.Errorf("the escaped note lost its text: %q", out)
	}
}

func TestResultsJSONRoundTrips(t *testing.T) {
	rs := []RunResult{
		{Target: Target{"internal/a", "FuzzOne"}, Outcome: OK, Execs: 10, NewInteresting: 2, Elapsed: 3 * time.Second, LogPath: "/out/logs/x.log"},
		{Target: Target{"internal/b", "FuzzTwo"}, Outcome: Failed, Findings: []string{"/out/findings/a"}, LogDroppedLines: 4, Note: "n"},
	}
	b, err := ResultsJSON(rs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseResultsJSON(b)
	if err != nil || !reflect.DeepEqual(got, rs) {
		t.Errorf("round trip = %+v, %v; want %+v", got, err, rs)
	}
	if _, err := ParseResultsJSON([]byte("{not json")); err == nil {
		t.Errorf("garbage was accepted")
	}
}
