package parsertest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// zoneParser embeds the host's time zone in its payload, the mistake the
// time.Local configuration of AssertDeterministic exists to catch.
type zoneParser struct{ WellBehaved }

func (p zoneParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	r := validRecord(in, 1)
	r.Body = time.Unix(1_700_000_000, 0).String()
	return out.Emit(ctx, r)
}

func TestParsersAreDeterministic(t *testing.T) {
	h, art, _ := bundle(t, t, "det", "one\ntwo\nthree\n")
	run := func(p parse.Parser) func() Result {
		return func() Result { return h.Run(p, h.Input(p, art, nil, false)) }
	}

	AssertDeterministic(t, run(WellBehaved{Name: "det", Version: "1.0.0"}))

	for name, p := range map[string]parse.Parser{
		"math/rand": Nondeterministic{Name: "det", Version: "1.0.0"},
		"host zone": zoneParser{WellBehaved{Name: "det", Version: "1.0.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{TB: t}
			AssertDeterministic(rec, run(p))
			if !rec.Failed() {
				t.Error("AssertDeterministic accepted a nondeterministic parser")
			}
		})
	}
}

func TestAssertDeterministicRunsSixteenTimesAndRestores(t *testing.T) {
	procs, local := runtime.GOMAXPROCS(0), time.Local
	var seenProcs []int
	var seenZones []string
	AssertDeterministic(t, func() Result {
		seenProcs = append(seenProcs, runtime.GOMAXPROCS(0))
		seenZones = append(seenZones, time.Local.String())
		return Result{}
	})
	if len(seenProcs) != DeterminismRuns || DeterminismRuns < 16 {
		t.Fatalf("newRun was called %d times, want DeterminismRuns = %d (at least 16)", len(seenProcs), DeterminismRuns)
	}
	for i := range seenProcs {
		wantProcs := procs
		if i%4 == 1 {
			wantProcs = 1
		}
		if seenProcs[i] != wantProcs {
			t.Errorf("run %d: GOMAXPROCS %d, want %d", i, seenProcs[i], wantProcs)
		}
		if (seenZones[i] != seenZones[0]) != (i%4 == 3) {
			t.Errorf("run %d: time.Local %q (default %q)", i, seenZones[i], seenZones[0])
		}
	}
	if runtime.GOMAXPROCS(0) != procs || time.Local != local {
		t.Error("GOMAXPROCS or time.Local was not restored")
	}

	// restored after a failure too
	rec := &recorder{TB: t}
	n := 0
	AssertDeterministic(rec, func() Result {
		n++
		return Result{Notes: map[string]string{"n": strings.Repeat("x", n)}, Records: []records.Record{{Type: "message", Summary: strings.Repeat("y", n)}}}
	})
	if !rec.Failed() || runtime.GOMAXPROCS(0) != procs || time.Local != local {
		t.Errorf("failed=%v procs=%d local restored=%v", rec.Failed(), runtime.GOMAXPROCS(0), time.Local == local)
	}
}

func TestCanonicalRecords(t *testing.T) {
	tm := records.Time{T: time.Date(2024, 1, 2, 3, 4, 5, 6000, time.UTC), Basis: records.BasisLocalOffset, OffsetMin: 60}
	r := records.Record{
		Type: "message", ArtifactID: "a1", Locator: "text:line=1", Range: &records.Range{Offset: 1, Length: 2},
		Time: &tm, Times: []records.NamedTime{{Kind: "sent", Time: tm}}, Summary: "s",
		Payload: map[string]any{"b": 1, "a": map[string]any{"z": true, "y": []any{"q"}}},
	}
	lines := CanonicalRecords([]records.Record{r, r})
	if len(lines) != 2 || lines[0] != lines[1] || strings.Contains(lines[0], "\n") {
		t.Fatalf("lines %q", lines)
	}
	if !strings.Contains(lines[0], `"a":{"y":["q"],"z":true},"b":1`) {
		t.Errorf("payload keys are not sorted: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"artifact_id":"a1"`) || strings.Index(lines[0], `"artifact_id"`) > strings.Index(lines[0], `"type"`) {
		t.Errorf("record keys are not sorted: %s", lines[0])
	}
	// a different range, time or payload gives a different line
	for name, mod := range map[string]func(*records.Record){
		"range":   func(r *records.Record) { r.Range = &records.Range{Offset: 1, Length: 3} },
		"time":    func(r *records.Record) { v := tm; v.T = v.T.Add(time.Microsecond); r.Time = &v },
		"basis":   func(r *records.Record) { v := tm; v.Basis = records.BasisUTC; r.Time = &v },
		"payload": func(r *records.Record) { r.Payload = map[string]any{"a": 1} },
		"deleted": func(r *records.Record) { r.Deleted = true },
	} {
		c := r
		mod(&c)
		if CanonicalRecords([]records.Record{c})[0] == lines[0] {
			t.Errorf("a change of %s does not show in the canonical line", name)
		}
	}
}

func detRecords(n int) []records.Record {
	var rs []records.Record
	for i := range n {
		rs = append(rs, records.Record{Type: "message", ArtifactID: "a1", Summary: strings.Repeat("s", i+1)})
	}
	return rs
}

// detRun returns a newRun whose n-th call (0-based) is changed by vary.
func detRun(vary func(call int, r *Result)) func() Result {
	call := -1
	return func() Result {
		call++
		r := Result{
			Records:  detRecords(3),
			Warnings: []Warning{{Locator: "text:line=1", Reason: "w1"}, {Locator: "text:line=2", Reason: "w2"}},
			Notes:    map[string]string{"lines": "3", "variant": "v1"},
			Progress: [][2]int64{{1, 3}, {2, 3}, {3, 3}},
		}
		vary(call, &r)
		return r
	}
}

func TestAssertDeterministicNeedsEveryRecordAndTheCountToAgree(t *testing.T) {
	rec := &recorder{TB: t}
	AssertDeterministic(rec, detRun(func(int, *Result) {}))
	if rec.Failed() {
		t.Fatalf("an identical result in every configuration failed: %v", rec.errs)
	}
	for call := 1; call <= 3; call++ {
		for name, vary := range map[string]func(*Result){
			"second record differs": func(r *Result) { r.Records[1].Summary = "other" },
			"last record differs":   func(r *Result) { r.Records[2].Summary = "other" },
			"fewer records":         func(r *Result) { r.Records = r.Records[:2] },
			"more records":          func(r *Result) { r.Records = append(r.Records, detRecords(4)[3]) },
			"no records":            func(r *Result) { r.Records = nil },
		} {
			rec := &recorder{TB: t}
			AssertDeterministic(rec, detRun(func(c int, r *Result) {
				if c == call {
					vary(r)
				}
			}))
			if !rec.Failed() {
				t.Errorf("call %d, %s: accepted", call, name)
			}
		}
	}
}

func TestAssertDeterministicComparesWarningsNotesErrAndFinalProgress(t *testing.T) {
	for name, vary := range map[string]func(*Result){
		"warning text":      func(r *Result) { r.Warnings[0].Reason = "other" },
		"warning locator":   func(r *Result) { r.Warnings[1].Locator = "text:line=9" },
		"warning order":     func(r *Result) { r.Warnings[0], r.Warnings[1] = r.Warnings[1], r.Warnings[0] },
		"warning count":     func(r *Result) { r.Warnings = r.Warnings[:1] },
		"note value":        func(r *Result) { r.Notes["variant"] = "v2" },
		"note key":          func(r *Result) { r.Notes["extra"] = "x" },
		"note missing":      func(r *Result) { delete(r.Notes, "variant") },
		"error set":         func(r *Result) { r.Err = errors.New("failed") },
		"final progress":    func(r *Result) { r.Progress[2] = [2]int64{3, 4} },
		"progress missing":  func(r *Result) { r.Progress = nil },
		"progress finished": func(r *Result) { r.Progress = r.Progress[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{TB: t}
			AssertDeterministic(rec, detRun(func(c int, r *Result) {
				if c == 2 {
					vary(r)
				}
			}))
			if !rec.Failed() {
				t.Error("accepted")
			}
		})
	}
	t.Run("error text", func(t *testing.T) {
		rec := &recorder{TB: t}
		AssertDeterministic(rec, detRun(func(c int, r *Result) { r.Err = fmt.Errorf("failed %d", c) }))
		if !rec.Failed() {
			t.Error("a different error text in each run was accepted")
		}
	})
	t.Run("same error", func(t *testing.T) {
		rec := &recorder{TB: t}
		AssertDeterministic(rec, detRun(func(_ int, r *Result) { r.Err = errors.New("failed") }))
		if rec.Failed() {
			t.Errorf("the same error every run failed: %v", rec.errs)
		}
	})
	t.Run("intermediate progress is timing, not output", func(t *testing.T) {
		rec := &recorder{TB: t}
		AssertDeterministic(rec, detRun(func(c int, r *Result) { r.Progress = [][2]int64{{int64(c + 1), 3}, {3, 3}} }))
		if rec.Failed() {
			t.Errorf("different intermediate progress failed: %v", rec.errs)
		}
	})
}

func TestCanonicalRecordsEveryFieldShowsInTheLine(t *testing.T) {
	tm := records.Time{T: time.Date(2024, 1, 2, 3, 4, 5, 6000, time.UTC), Basis: records.BasisLocalOffset, OffsetMin: 60}
	conf := 5
	base := records.Record{
		Type: "message", ArtifactID: "a1", SourcePath: "/d/x", Locator: "text:line=1", Range: &records.Range{Offset: 1, Length: 2},
		Time: &tm, TimeEnd: &tm, Times: []records.NamedTime{{Kind: "sent", Time: tm}},
		Deleted: true, Recovery: "carved", Confidence: &conf, Summary: "s", Body: "b", Payload: map[string]any{"a": 1},
	}
	want := CanonicalRecords([]records.Record{base})[0]
	later := func(f func(v *records.Time)) *records.Time { v := tm; f(&v); return &v }
	otherConf := 6
	for name, mod := range map[string]func(*records.Record){
		"type":           func(r *records.Record) { r.Type = "call" },
		"artifact_id":    func(r *records.Record) { r.ArtifactID = "a2" },
		"source_path":    func(r *records.Record) { r.SourcePath = "/d/y" },
		"locator":        func(r *records.Record) { r.Locator = "text:line=2" },
		"range offset":   func(r *records.Record) { r.Range = &records.Range{Offset: 2, Length: 2} },
		"range length":   func(r *records.Record) { r.Range = &records.Range{Offset: 1, Length: 3} },
		"range nil":      func(r *records.Record) { r.Range = nil },
		"time":           func(r *records.Record) { r.Time = later(func(v *records.Time) { v.T = v.T.Add(time.Microsecond) }) },
		"time basis":     func(r *records.Record) { r.Time = later(func(v *records.Time) { v.Basis = records.BasisUTC }) },
		"time offset":    func(r *records.Record) { r.Time = later(func(v *records.Time) { v.OffsetMin = 90 }) },
		"time nil":       func(r *records.Record) { r.Time = nil },
		"time_end":       func(r *records.Record) { r.TimeEnd = later(func(v *records.Time) { v.T = v.T.Add(time.Second) }) },
		"time_end basis": func(r *records.Record) { r.TimeEnd = later(func(v *records.Time) { v.Basis = records.BasisUTC }) },
		"time_end offset": func(r *records.Record) {
			r.TimeEnd = later(func(v *records.Time) { v.OffsetMin = 90 })
		},
		"time_end nil": func(r *records.Record) { r.TimeEnd = nil },
		"times kind":   func(r *records.Record) { r.Times = []records.NamedTime{{Kind: "read", Time: tm}} },
		"times time": func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "sent", Time: *later(func(v *records.Time) { v.T = v.T.Add(time.Hour) })}}
		},
		"times basis": func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "sent", Time: *later(func(v *records.Time) { v.Basis = records.BasisUTC })}}
		},
		"times offset": func(r *records.Record) {
			r.Times = []records.NamedTime{{Kind: "sent", Time: *later(func(v *records.Time) { v.OffsetMin = 90 })}}
		},
		"times extra":    func(r *records.Record) { r.Times = append(r.Times, records.NamedTime{Kind: "read", Time: tm}) },
		"times none":     func(r *records.Record) { r.Times = nil },
		"deleted":        func(r *records.Record) { r.Deleted = false },
		"recovery":       func(r *records.Record) { r.Recovery = "other" },
		"confidence":     func(r *records.Record) { r.Confidence = &otherConf },
		"confidence nil": func(r *records.Record) { r.Confidence = nil },
		"summary":        func(r *records.Record) { r.Summary = "t" },
		"body":           func(r *records.Record) { r.Body = "c" },
		"payload":        func(r *records.Record) { r.Payload = map[string]any{"a": 2} },
	} {
		c := base
		mod(&c)
		if got := CanonicalRecords([]records.Record{c})[0]; got == want {
			t.Errorf("a change of %s does not show in the canonical line", name)
		}
	}
	if CanonicalRecords([]records.Record{base})[0] != want {
		t.Error("the canonical line of the same record is not stable")
	}
}

// Go randomises map iteration, so one comparison catches a map-order bug only half the time; the
// repeated runs make a miss as unlikely as 2^-15. The fake is tried several times so that the test
// itself cannot be the flaky part.
func TestAssertDeterministicRejectsMapIterationOrder(t *testing.T) {
	h, art, _ := bundle(t, t, "mo", "one\ntwo\n")
	p := MapOrder{Name: "mo", Version: "1.0.0"}
	for attempt := 0; attempt < 5; attempt++ {
		rec := &recorder{TB: t}
		AssertDeterministic(rec, func() Result { return h.Run(p, h.Input(p, art, nil, false)) })
		if rec.Failed() {
			return
		}
	}
	t.Error("AssertDeterministic accepted a parser whose output follows map iteration order, five times in a row")
}

// One instance serves every run, so a parser that keeps state between jobs disagrees with itself.
func TestAssertDeterministicRejectsAStatefulParser(t *testing.T) {
	h, art, _ := bundle(t, t, "st", "one\ntwo\n")
	p := &Stateful{Name: "st", Version: "1.0.0"}
	rec := &recorder{TB: t}
	AssertDeterministic(rec, func() Result { return h.Run(p, h.Input(p, art, nil, false)) })
	if !rec.Failed() {
		t.Error("AssertDeterministic accepted a parser whose output depends on the jobs before it")
	}
}
