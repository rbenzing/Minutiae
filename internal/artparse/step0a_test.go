package artparse_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// A parser goroutine that leaves through runtime.Goexit makes recover() return nil: that is not a
// clean return and must never look like one.
func TestGuardGoexitIsAnErrorNotACleanReturn(t *testing.T) {
	g := artparse.Guard(context.Background(), time.Second, time.Second, func() {}, noPump(), func(context.Context) error {
		runtime.Goexit()
		return nil
	})
	if g.Err == nil || g.Panic != nil || g.Abandoned || g.TimedOut || g.Cancelled {
		t.Fatalf("got %+v, want an Err and nothing else", g)
	}
	if !strings.Contains(g.Err.Error(), "exited without returning") {
		t.Errorf("Err = %v", g.Err)
	}
}

// fn answers the end of its context while the guard loop is busy elsewhere, so the loop sees "done" and
// "ended" ready together; the flags must then come from the fix-up block alone.
func TestGuardFlagsFromFnAnsweringTheEndOfItsContext(t *testing.T) {
	for i := 0; i < 30; i++ {
		pump := make(chan func(), 1)
		pump <- func() { time.Sleep(60 * time.Millisecond) } // the loop is busy until fn has returned
		g := artparse.Guard(context.Background(), 10*time.Millisecond, time.Second, func() {}, pump, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !g.TimedOut || g.Cancelled || g.Abandoned {
			t.Fatalf("iteration %d: timeout answered by fn: got %+v", i, g)
		}
	}
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		pump := make(chan func(), 1)
		pump <- func() { cancel(); time.Sleep(60 * time.Millisecond) }
		g := artparse.Guard(ctx, time.Hour, time.Second, func() {}, pump, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		cancel()
		if !g.Cancelled || g.TimedOut || g.Abandoned {
			t.Fatalf("iteration %d: cancel answered by fn: got %+v", i, g)
		}
	}
}

func TestEmitterCopiesEveryPointerFieldBeforeAdd(t *testing.T) {
	v, e := newEnv(t, nil)
	now := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	conf := 50
	r := v.rec()
	r.Range = &records.Range{Offset: 1, Length: 2}
	r.Time = &records.Time{T: now}
	r.TimeEnd = &records.Time{T: now.Add(time.Hour)}
	r.Times = []records.NamedTime{{Kind: "created", Time: records.Time{T: now}}}
	r.Deleted, r.Recovery, r.Confidence = true, "carve", &conf
	v.spy.addHook = func(records.Record) { // the parser goroutine edits what it still holds
		r.Range.Offset, r.Range.Length = 9, 9
		r.Time.T = now.Add(24 * time.Hour)
		r.TimeEnd.T = now.Add(48 * time.Hour)
		r.Times[0].Kind = "mutated"
		conf = 1
	}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) != 1 {
		t.Fatalf("the writer got %d records", len(v.spy.added))
	}
	got := v.spy.added[0]
	if *got.Range != (records.Range{Offset: 1, Length: 2}) {
		t.Errorf("Range = %+v", *got.Range)
	}
	if !got.Time.T.Equal(now) || !got.TimeEnd.T.Equal(now.Add(time.Hour)) {
		t.Errorf("Time %v / TimeEnd %v follow the parser's edits", got.Time.T, got.TimeEnd.T)
	}
	if got.Times[0].Kind != "created" {
		t.Errorf("Times[0] = %+v", got.Times[0])
	}
	if got.Confidence == nil || *got.Confidence != 50 {
		t.Errorf("Confidence = %v", got.Confidence)
	}
}

// A refused payload must not use up the MaxRecords quota.
func TestEmitterRefusedPayloadDoesNotConsumeTheRecordQuota(t *testing.T) {
	v, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.MaxRecords = 2 })
	deep := v.rec()
	var m any = "leaf"
	for i := 0; i < 200; i++ {
		m = map[string]any{"n": m}
	}
	deep.Payload["raw"] = m
	for i := 0; i < 5; i++ {
		if err := e.Emit(ctx0(), deep); err != nil {
			t.Fatalf("refusal %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := e.Emit(ctx0(), v.rec()); err != nil {
			t.Fatalf("good record %d = %v: refused payloads ate the quota", i, err)
		}
	}
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, parse.ErrRecordCap) {
		t.Fatalf("third good record = %v, want ErrRecordCap", err)
	}
}

// The writer must see a live context while the job's context is being cancelled (an accepted record
// is audited even then).
func TestEmitterWriterContextSurvivesHostCancellation(t *testing.T) {
	v, _ := newEnv(t, nil)
	hctx, cancel := context.WithCancel(ctx0())
	defer cancel()
	e, err := artparse.NewEmitter(hctx, v.spy, v.reg, map[string]parse.Artifact{"db": {ID: v.art, Platform: parse.PlatformAndroid}}, parse.DefaultLimits(), v.pump, nil)
	if err != nil {
		t.Fatal(err)
	}
	v.spy.addHook = func(records.Record) { cancel() } // Ctrl-C while the Add is in flight
	if err := e.Emit(ctx0(), v.rec()); err != nil {
		t.Fatal(err)
	}
	v.spy.mu.Lock()
	defer v.spy.mu.Unlock()
	if v.spy.ctxErr != nil {
		t.Errorf("the writer's context ended with the host's: %v", v.spy.ctxErr)
	}
}

func TestEmitterProgressThrottleForwardsOnePerInterval(t *testing.T) {
	v, _ := newEnv(t, nil)
	pump := make(chan func(), 64)
	e, err := artparse.NewEmitter(ctx0(), v.spy, v.reg, nil, parse.DefaultLimits(), pump, func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 40; i++ {
		e.Progress(i, 1000)
	}
	if len(pump) != 1 {
		t.Fatalf("%d progress closures queued within one interval, want 1", len(pump))
	}
}

func TestEmitterPayloadBoundsAreRefusedByTheEmitter(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		v, e := newEnv(t, nil)
		r := v.rec()
		var m any = "leaf"
		for i := 0; i < 70; i++ { // a plain chain: no cycle, only depth
			m = map[string]any{"n": m}
		}
		r.Payload["raw"] = m
		if err := e.Emit(ctx0(), r); err != nil {
			t.Fatal(err)
		}
		if v.spy.adds.Load() != 0 || len(v.spy.rejects) != 1 {
			t.Errorf("adds %d rejects %v: the emitter itself must refuse a too deep payload", v.spy.adds.Load(), v.spy.rejects)
		}
	})
	t.Run("shared sub-tree wider than the node cap", func(t *testing.T) {
		v, e := newEnv(t, nil)
		r := v.rec()
		inner := make([]any, 2100)
		for i := range inner {
			inner[i] = "x"
		}
		outer := make([]any, 2100)
		for i := range outer {
			outer[i] = inner // one shared slice, 2100 times: 4.4 million values to copy
		}
		r.Payload["raw"] = outer
		if err := e.Emit(ctx0(), r); err != nil {
			t.Fatal(err)
		}
		if v.spy.adds.Load() != 0 || len(v.spy.rejects) != 1 {
			t.Errorf("adds %d rejects %v: the emitter itself must refuse the shared sub-tree", v.spy.adds.Load(), v.spy.rejects)
		}
	})
}

func TestNewEmitterRecoveryMethodLengthBoundary(t *testing.T) {
	long := strings.Repeat("a", 32)
	for method, wantErr := range map[string]bool{long: false, long + "a": true, "a": true, "ab": false} {
		v, _ := newEnv(t, nil)
		_, err := artparse.NewEmitter(ctx0(), v.spy, v.reg, map[string]parse.Artifact{"db": {ID: v.art, Platform: parse.PlatformAndroid, Recovery: &parse.RecoveryInfo{Method: method}}}, parse.DefaultLimits(), v.pump, nil)
		if (err != nil) != wantErr {
			t.Errorf("method of %d chars: err = %v, want error: %v", len(method), err, wantErr)
		}
	}
}

func TestEmitterWarnCleansLocatorAndReason(t *testing.T) {
	v, e := newEnv(t, nil)
	if err := e.Warn(ctx0(), "row:\x00id=1", "bad\x00reason"); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.warns) != 1 || strings.ContainsRune(v.spy.warns[0], 0) {
		t.Errorf("the writer was handed an uncleaned locator: %q", v.spy.warns)
	}
}

// The emitter itself checks the platform of a record's artifact (the writer does not know it).
func TestEmitterRefusesRecordOfForeignPlatformArtifact(t *testing.T) {
	v, e := newEnv(t, func(_ *parse.Limits, a *parse.Artifact) { a.Platform = parse.PlatformIOS })
	if err := e.Emit(ctx0(), v.rec()); err != nil {
		t.Fatal(err)
	}
	if v.spy.adds.Load() != 0 || len(v.spy.rejects) != 1 {
		t.Errorf("adds %d rejects %v: a record for an iOS artifact of an Android parser reached the writer", v.spy.adds.Load(), v.spy.rejects)
	}
}

// seal waits for an in-flight writer call for at most the grace period; past it the host abandons
// the job (an Add stuck in the writer must not hang the run).
func TestEmitterSealGivesUpOnAStuckAdd(t *testing.T) {
	v, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.GracePeriod = 50 * time.Millisecond })
	entered, release := make(chan struct{}), make(chan struct{})
	v.spy.addHook = func(records.Record) {
		close(entered)
		<-release
	}
	emitted := make(chan error, 1)
	go func() { emitted <- e.Emit(ctx0(), v.rec()) }()
	<-entered
	start := time.Now()
	sealed := make(chan struct{})
	go func() { e.Seal(); close(sealed) }()
	select {
	case <-sealed:
	case <-time.After(5 * time.Second):
		t.Fatal("seal waited for the stuck Add instead of giving up after the grace period")
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("seal returned after %v, before the grace period", d)
	}
	if !e.StuckInWriter() {
		t.Error("seal gave up but does not say so")
	}
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("Emit after the seal = %v", err)
	}
	close(release)
	<-emitted
}

func TestEmitterNotStuckWhenAddsFinish(t *testing.T) {
	v, e := newEnv(t, nil)
	if err := e.Emit(ctx0(), v.rec()); err != nil {
		t.Fatal(err)
	}
	e.Seal()
	if e.StuckInWriter() {
		t.Error("stuck although nothing was in flight")
	}
}
