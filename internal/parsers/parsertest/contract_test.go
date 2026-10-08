package parsertest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// fnParser is a WellBehaved whose Parse is replaced by f.
type fnParser struct {
	WellBehaved
	f func(ctx context.Context, in *parse.Input, out parse.Emitter) error
}

func (p fnParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return p.f(ctx, in, out)
}

func newFn(name string, f func(ctx context.Context, in *parse.Input, out parse.Emitter) error) fnParser {
	return fnParser{WellBehaved{Name: name, Version: "1.0.0"}, f}
}

func joined(rec *recorder) string { return strings.Join(rec.errs, "\n") }

func TestStrictReportsAndRecordsAParserError(t *testing.T) {
	p := newFn("perr", func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") })

	rec := &recorder{TB: t}
	h, art, _ := bundle(t, rec, "perr", "x\n")
	res := h.Run(p, h.Input(p, art, nil, false))
	if !rec.Failed() || !strings.Contains(joined(rec), "parse failed") {
		t.Errorf("strict run did not fail on the parser's error: %v", rec.errs)
	}
	if res.Err == nil || res.Err.Error() != "parse failed" {
		t.Errorf("Result.Err = %v", res.Err)
	}

	h, art, _ = bundle(t, t, "perr", "x\n")
	res = h.RunLenient(p, h.Input(p, art, nil, false))
	if res.Err == nil || res.Err.Error() != "parse failed" {
		t.Errorf("lenient Result.Err = %v", res.Err)
	}
}

func TestStrictStopsAtTheFirstFailure(t *testing.T) {
	invalid := func(in *parse.Input, n int) records.Record {
		r := validRecord(in, n)
		r.Summary = "a\x00b"
		return r
	}
	t.Run("a refused record, the parser ignores the error", func(t *testing.T) {
		var errs []error
		p := newFn("stop", func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
			for i := 1; i <= 3; i++ {
				errs = append(errs, out.Emit(ctx, invalid(in, i)))
			}
			errs = append(errs, out.Warn(ctx, "text:line=1", "w"), out.Emit(ctx, validRecord(in, 9)))
			return nil
		})
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "stop", "x\n")
		res := h.Run(p, h.Input(p, art, nil, false))
		if len(rec.errs) != 1 {
			t.Errorf("want exactly one failure, got %d: %v", len(rec.errs), rec.errs)
		}
		for i, err := range errs {
			if err == nil {
				t.Errorf("call %d after the first failure returned nil: the parser was not told to stop", i)
			}
		}
		if len(res.Records) != 0 || len(res.Warnings) != 0 {
			t.Errorf("records %d, warnings %v after the stop", len(res.Records), res.Warnings)
		}
	})
	t.Run("a warning, the parser ignores the error", func(t *testing.T) {
		var errs []error
		p := newFn("stop", func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
			errs = append(errs, out.Warn(ctx, "text:line=1", "w1"), out.Warn(ctx, "text:line=2", "w2"),
				out.Emit(ctx, validRecord(in, 1)))
			return nil
		})
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "stop", "x\n")
		res := h.Run(p, h.Input(p, art, nil, false))
		if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "text:line=1") {
			t.Errorf("want exactly one failure naming the first warning, got %v", rec.errs)
		}
		if errs[0] == nil || errs[1] == nil || errs[2] == nil || len(res.Records) != 0 {
			t.Errorf("calls after the failure: %v, records %d", errs, len(res.Records))
		}
	})
}

func TestResultRecordsAreDeepCopies(t *testing.T) {
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	p := newFn("copy", func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		conf := 40
		r := validRecord(in, 1)
		r.Range = &records.Range{Offset: 0, Length: 2}
		r.Time = &records.Time{T: when}
		r.TimeEnd = &records.Time{T: when.Add(time.Hour)}
		r.Times = []records.NamedTime{{Kind: "sent", Time: records.Time{T: when}}}
		r.Deleted, r.Recovery, r.Confidence = true, "carved", &conf
		if err := out.Emit(ctx, r); err != nil {
			return err
		}
		// change everything the parser still holds
		r.Range.Length = 99
		r.Time.T = when.Add(time.Hour)
		r.TimeEnd.T = when
		r.Times[0].Kind = "changed"
		*r.Confidence = 1
		return nil
	})
	h, art, _ := bundle(t, t, "copy", "x\n")
	res := h.RunLenient(p, h.Input(p, art, nil, false))
	if len(res.Records) != 1 || len(res.Warnings) != 0 {
		t.Fatalf("records %d, warnings %v", len(res.Records), res.Warnings)
	}
	r := res.Records[0]
	if r.Range.Length != 2 || !r.Time.T.Equal(when) || !r.TimeEnd.T.Equal(when.Add(time.Hour)) ||
		r.Times[0].Kind != "sent" || *r.Confidence != 40 {
		t.Errorf("the result changed after Emit: range %+v time %v end %v times %+v confidence %d",
			*r.Range, r.Time.T, r.TimeEnd.T, r.Times, *r.Confidence)
	}
}

func TestEmitterIsSealedWhenRunReturns(t *testing.T) {
	var em parse.Emitter
	p := newFn("late", func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		em = out
		return out.Emit(ctx, validRecord(in, 1))
	})
	rec := &recorder{TB: t}
	h, art, _ := bundle(t, rec, "late", "x\n")
	in := h.Input(p, art, nil, false)
	res := h.Run(p, in)
	if res.Late() != 0 || len(res.Records) != 1 {
		t.Fatalf("late %d, records %d before any late call", res.Late(), len(res.Records))
	}
	notes, progress := len(res.Notes), len(res.Progress)

	// late calls from another goroutine, as an abandoned parser makes them
	errs := make(chan [2]error)
	go func() {
		a := em.Emit(context.Background(), validRecord(in, 2))
		b := em.Warn(context.Background(), "text:line=2", "late warning")
		em.Note("late", "x")
		em.Progress(9, 9)
		errs <- [2]error{a, b}
	}()
	got := <-errs
	if !errors.Is(got[0], parse.ErrSealed) || !errors.Is(got[1], parse.ErrSealed) {
		t.Errorf("late Emit and Warn returned %v and %v, want ErrSealed", got[0], got[1])
	}
	if res.Late() != 4 {
		t.Errorf("Result.LateCalls = %d, want 4 (Emit, Warn, Note, Progress)", res.Late())
	}
	if rec.Failed() {
		t.Errorf("a late call failed the test: %v", rec.errs)
	}
	if _, ok := res.Notes["late"]; ok || len(res.Notes) != notes || len(res.Progress) != progress || len(res.Records) != 1 || len(res.Warnings) != 0 {
		t.Errorf("a late call changed the returned result: %+v", res)
	}
	var fresh Result
	if fresh.Late() != 0 {
		t.Error("a zero Result has late calls")
	}

	// the final check: a call that slips in after the seal fails a strict run
	rec2 := &recorder{TB: t}
	h2, art2, _ := bundle(t, rec2, "late", "x\n")
	h2.afterSeal = func() { em.Note("late", "between the seal and the final check") }
	h2.Run(p, h2.Input(p, art2, nil, false))
	if !rec2.Failed() || !strings.Contains(joined(rec2), "after Run returned") {
		t.Errorf("a late call before the final check did not fail the strict run: %v", rec2.errs)
	}

	// and the exported check reports the count
	rec3 := &recorder{TB: t}
	res.AssertNoLateCalls(rec3)
	if !rec3.Failed() || !strings.Contains(joined(rec3), "4") {
		t.Errorf("AssertNoLateCalls did not report the count: %v", rec3.errs)
	}
	rec4 := &recorder{TB: t}
	fresh.AssertNoLateCalls(rec4)
	if rec4.Failed() {
		t.Errorf("AssertNoLateCalls failed a result with no late calls: %v", rec4.errs)
	}
}

func TestStrictEnforcesTheContractRules(t *testing.T) {
	t.Run("record type not in Meta.Emits", func(t *testing.T) {
		p := Liar{Name: "liar", Version: "1.0.0", Mode: LieUndeclaredType}
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "liar", "x\n")
		res := h.Run(p, h.Input(p, art, nil, false))
		if !strings.Contains(joined(rec), parse.ErrUndeclaredType.Error()) || len(res.Records) != 0 {
			t.Errorf("strict: failures %v, records %d", rec.errs, len(res.Records))
		}
		h, art, _ = bundle(t, t, "liar", "x\n")
		res = h.RunLenient(p, h.Input(p, art, nil, false))
		if len(res.Records) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, parse.ErrUndeclaredType.Error()) {
			t.Errorf("lenient: records %d, warnings %v", len(res.Records), res.Warnings)
		}
	})
	t.Run("artifact of a platform the parser does not declare", func(t *testing.T) {
		p := Liar{Name: "liar", Version: "1.0.0", Mode: LieForeignPlatform}
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "liar", "x\n")
		res := h.Run(p, h.Input(p, art, nil, false))
		if !strings.Contains(joined(rec), parse.ErrForeignPlatform.Error()) || len(res.Records) != 0 {
			t.Errorf("strict: failures %v, records %d", rec.errs, len(res.Records))
		}
		h, art, _ = bundle(t, t, "liar", "x\n")
		res = h.RunLenient(p, h.Input(p, art, nil, false))
		if len(res.Records) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, parse.ErrForeignPlatform.Error()) {
			t.Errorf("lenient: records %d, warnings %v", len(res.Records), res.Warnings)
		}
	})
	t.Run("an input whose bytes changed during the run", func(t *testing.T) {
		data := []byte("abc\n")
		p := newFn("mutdata", func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
			data[0] = 'Z' // what a parser holding a writable view of the case file could do
			return out.Emit(ctx, validRecord(in, 1))
		})
		rec := &recorder{TB: t}
		h := NewHarness(rec)
		m := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/mutdata.dat", data)
		res := h.Run(p, h.Input(p, h.Artifact(m, data), nil, false))
		if !strings.Contains(joined(rec), "changed during the run") || !strings.Contains(joined(rec), m.ID) {
			t.Errorf("strict: failures %v", rec.errs)
		}
		var ic *parse.InputChangedError
		if len(res.Violations) != 1 || !errors.As(res.Violations[0], &ic) || ic.ArtifactID != m.ID || ic.Want != m.SHA256 {
			t.Errorf("violations %v", res.Violations)
		}
		// lenient: collected, not failed
		data = []byte("abc\n")
		h = NewHarness(t)
		m = AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/mutdata.dat", data)
		res = h.RunLenient(p, h.Input(p, h.Artifact(m, data), nil, false))
		if len(res.Violations) != 1 || !errors.Is(res.Violations[0], parse.ErrInputChanged) {
			t.Errorf("lenient violations %v", res.Violations)
		}
	})
	t.Run("an unchanged input is not a violation", func(t *testing.T) {
		h, art, _ := bundle(t, t, "ok", "abc\n")
		p := WellBehaved{Name: "ok", Version: "1.0.0"}
		if res := h.Run(p, h.Input(p, art, nil, false)); len(res.Violations) != 0 {
			t.Errorf("violations %v", res.Violations)
		}
	})
	t.Run("too many warnings", func(t *testing.T) {
		var calls int
		var last error
		p := newFn("spam", func(ctx context.Context, _ *parse.Input, out parse.Emitter) error {
			for range parse.MaxWarnings + 5 {
				if last = out.Warn(ctx, "text:line=1", "w"); last != nil {
					return nil
				}
				calls++
			}
			return nil
		})
		h, art, _ := bundle(t, t, "spam", "x\n")
		h.warnSink = func(context.Context, string, string) error { return nil } // 10,000 audited warnings would take minutes
		res := h.RunLenient(p, h.Input(p, art, nil, false))
		if calls != parse.MaxWarnings || last == nil || len(res.Warnings) != parse.MaxWarnings {
			t.Errorf("accepted %d warnings (kept %d), stop error %v", calls, len(res.Warnings), last)
		}
		if len(res.Violations) != 1 || !errors.Is(res.Violations[0], parse.ErrWarningCap) {
			t.Errorf("violations %v", res.Violations)
		}
		// exactly the cap is allowed
		p = newFn("spam", func(ctx context.Context, _ *parse.Input, out parse.Emitter) error {
			for range parse.MaxWarnings {
				if err := out.Warn(ctx, "text:line=1", "w"); err != nil {
					return err
				}
			}
			return nil
		})
		h, art, _ = bundle(t, t, "spam", "x\n")
		h.warnSink = func(context.Context, string, string) error { return nil }
		if res := h.RunLenient(p, h.Input(p, art, nil, false)); len(res.Violations) != 0 || res.Err != nil {
			t.Errorf("exactly the cap: violations %v, err %v", res.Violations, res.Err)
		}
	})
	t.Run("a refused record fails a strict run", func(t *testing.T) {
		p := Invalid{Name: "inv", Version: "1.0.0", Mode: InvalidNulInSummary, N: 1}
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "inv", "x\n")
		h.Run(p, h.Input(p, art, nil, false))
		if !strings.Contains(joined(rec), parse.ErrRefusedRecords.Error()) {
			t.Errorf("failures %v do not use the shared refusal check", rec.errs)
		}
	})
}

func TestAssertHonoursCancel(t *testing.T) {
	t.Run("a cooperative parser", func(t *testing.T) {
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "coop", "x\n")
		p := Slow{Name: "coop", Version: "1.0.0", Cooperative: true}
		res := h.AssertHonoursCancel(p, h.Input(p, art, nil, false), 5*time.Second)
		if rec.Failed() || !errors.Is(res.Err, context.Canceled) {
			t.Errorf("failures %v, err %v", rec.errs, res.Err)
		}
	})
	t.Run("a parser that ignores the context", func(t *testing.T) {
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "deaf", "x\n")
		p := Slow{Name: "deaf", Version: "1.0.0", Release: make(chan struct{}), Done: make(chan struct{}), Late: &LateResults{}}
		h.AssertHonoursCancel(p, h.Input(p, art, nil, false), 100*time.Millisecond)
		close(p.Release)
		<-p.Done
		if !rec.Failed() || !strings.Contains(joined(rec), "did not return") {
			t.Errorf("failures %v", rec.errs)
		}
	})
	t.Run("a parser that returns another error", func(t *testing.T) {
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "other", "x\n")
		p := newFn("other", func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("not the context") })
		h.AssertHonoursCancel(p, h.Input(p, art, nil, false), 5*time.Second)
		if !rec.Failed() || !strings.Contains(joined(rec), "not the context") {
			t.Errorf("failures %v", rec.errs)
		}
	})
	t.Run("a parser that finishes without a check", func(t *testing.T) {
		rec := &recorder{TB: t}
		h, art, _ := bundle(t, rec, "quick", "x\n")
		p := WellBehaved{Name: "quick", Version: "1.0.0"}
		h.AssertHonoursCancel(p, h.Input(p, art, nil, false), 5*time.Second)
		if rec.Failed() {
			t.Errorf("a parser that completes is not a failure: %v", rec.errs)
		}
	})
}

func TestInputReadersOfEveryRoleAreSealedAndLimited(t *testing.T) {
	lim := parse.DefaultLimits()
	h, art, _ := bundle(t, t, "well", "a\n")
	big := strings.Repeat("w", int(lim.ProbeBytes)+4096)
	m2 := AddAndroidFile(t, h.Case, "acq1", "/data/parsertest/well.dat-wal", []byte(big))
	wal := h.Artifact(m2, []byte(big))
	if wal.Platform != parse.PlatformAndroid {
		t.Errorf("Artifact platform %q", wal.Platform)
	}
	if _, ok := wal.R.(interface{ Seal() }); ok {
		t.Errorf("Artifact reader %T exposes Seal to the parser", wal.R)
	}
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	in := h.Input(p, art, map[string]parse.Artifact{"wal": wal}, true)
	r := in.Artifacts["wal"].R
	if _, ok := r.(interface{ Seal() }); ok {
		t.Fatalf("a non-primary reader %T exposes Seal to the parser", r)
	}
	buf := make([]byte, len(big))
	if n, err := r.ReadAt(buf, 0); !errors.Is(err, parse.ErrProbeLimit) || int64(n) != lim.ProbeBytes {
		t.Errorf("a non-primary read under Probe: n=%d err=%v, want the probe limit", n, err)
	}
	in = h.Input(p, art, map[string]parse.Artifact{"wal": wal}, false)
	other := in.Artifacts["wal"].R
	h.RunLenient(p, in)
	if _, err := other.ReadAt(buf[:8], 0); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("a non-primary reader after Run: %v, want ErrSealed", err)
	}
}

func TestHarnessRejectsAForeignInput(t *testing.T) {
	rec := &recorder{TB: t}
	h, art, _ := bundle(t, rec, "well", "a\n")
	p := WellBehaved{Name: "well", Version: "1.0.0"}
	res := h.Run(p, &parse.Input{Primary: art})
	if !rec.Failed() || res.Err == nil {
		t.Errorf("a foreign Input was accepted: failures %v, err %v", rec.errs, res.Err)
	}
}
