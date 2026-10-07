package artparse_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// spy wraps the real records writer: it records what reaches it and can hold or fail an Add.
type spy struct {
	artparse.IngestWriter
	mu      sync.Mutex
	added   []records.Record
	warns   []string
	rejects []string
	addHook func(records.Record)
	addErr  error
	adds    atomic.Int32
}

func (s *spy) Add(ctx context.Context, r records.Record) error {
	s.adds.Add(1)
	if s.addHook != nil {
		s.addHook(r)
	}
	if s.addErr != nil {
		return s.addErr
	}
	s.mu.Lock()
	s.added = append(s.added, r)
	s.mu.Unlock()
	return s.IngestWriter.Add(ctx, r)
}

func (s *spy) Warn(ctx context.Context, path, reason string) error {
	s.mu.Lock()
	s.warns = append(s.warns, path)
	s.mu.Unlock()
	return s.IngestWriter.Warn(ctx, path, reason)
}

func (s *spy) Reject(ctx context.Context, path, reason string) error {
	s.mu.Lock()
	s.rejects = append(s.rejects, path)
	s.mu.Unlock()
	return s.IngestWriter.Reject(ctx, path, reason)
}

type env struct {
	c    *evidence.Case
	w    *records.Writer
	spy  *spy
	art  string // declared artifact id
	art2 string // in the manifest, NOT declared for the ingest
	pump chan func()
	reg  artparse.Registered
}

// newEnv starts a real ingest over one declared artifact; mod changes the limits and the artifact table entry.
func newEnv(t *testing.T, mod func(*parse.Limits, *parse.Artifact)) (*env, *artparse.Emitter) {
	t.Helper()
	c := newCase(t)
	a1 := recordstest.AddArtifact(t, c, "a1.db", []byte(strings.Repeat("x", 4096)))
	a2 := recordstest.AddArtifact(t, c, "a2.db", []byte("y"))
	reg := register(t, "emit", "1.0.0")
	w, err := records.NewWriter(c, reg.Identity(), records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background(), records.StartOptions{AnalysisID: "an-1", Artifacts: []string{a1.ID}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.Abort(context.Background(), errors.New("test over")) })
	lim := parse.DefaultLimits()
	art := parse.Artifact{ID: a1.ID, Platform: parse.PlatformAndroid, Logical: "android:/data/x"}
	if mod != nil {
		mod(&lim, &art)
	}
	v := &env{c: c, w: w, spy: &spy{IngestWriter: w}, art: a1.ID, art2: a2.ID, pump: make(chan func(), 4), reg: reg}
	e, err := artparse.NewEmitter(context.Background(), v.spy, reg, map[string]parse.Artifact{"db": art}, lim, v.pump, func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	return v, e
}

func (v *env) rec() records.Record {
	return records.Record{Type: message.Type, ArtifactID: v.art, Summary: "hello", Payload: recordstest.ValidPayload(message.Type)}
}

func (v *env) audit(t *testing.T, action string) []evidence.AuditEntry {
	t.Helper()
	all, err := v.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func ctx0() context.Context { return context.Background() }

func TestEmitterRefusesUndeclaredType(t *testing.T) {
	v, e := newEnv(t, nil)
	r := v.rec()
	r.Type = "call"
	r.Locator = "row:id=7"
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatalf("Emit = %v, want nil (a rejection is counted, not fatal)", err)
	}
	if len(v.spy.added) != 0 || v.spy.adds.Load() != 0 {
		t.Errorf("an undeclared type reached the writer's Add")
	}
	if len(v.spy.rejects) != 1 || v.spy.rejects[0] != "row:id=7" || len(v.spy.warns) != 0 {
		t.Errorf("rejects %v warns %v, want one Reject at the locator", v.spy.rejects, v.spy.warns)
	}
	ws := v.audit(t, evidence.ActionAnalysisWarning)
	if len(ws) != 1 || ws[0].Details["path"] != "row:id=7" || ws[0].Details[evidence.WarnKeyRejected] != true {
		t.Fatalf("audited warnings = %+v", ws)
	}
	if _, rej, _ := e.Counts(); rej != 1 {
		t.Errorf("emitter rejected = %d", rej)
	}
	res, err := v.w.End(ctx0())
	if err != nil || res.Rejected != 1 {
		t.Errorf("end entry rejected = %d (%v), want 1", res.Rejected, err)
	}
}

func TestEmitterWarnsOnRejectedRecord(t *testing.T) {
	v, e := newEnv(t, nil)
	r := v.rec()
	r.Summary = "bad\x00summary"
	r.SourcePath = "/data/x"
	r.Payload["secret"] = "TOP-SECRET-PAYLOAD-VALUE"
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatalf("Emit = %v, want nil", err)
	}
	// Add audits its own refusal; the emitter must not audit it a second time.
	if len(v.spy.warns) != 0 || len(v.spy.rejects) != 0 {
		t.Errorf("emitter called Warn %v / Reject %v for a refusal the writer already audited", v.spy.warns, v.spy.rejects)
	}
	ws := v.audit(t, evidence.ActionAnalysisWarning)
	if len(ws) != 1 || ws[0].Details["path"] != "/data/x" || ws[0].Details[evidence.WarnKeyRejected] != true {
		t.Fatalf("audited warnings = %+v", ws)
	}
	if s, _ := ws[0].Details["reason"].(string); strings.Contains(s, "TOP-SECRET") {
		t.Errorf("a payload value leaked into the warning: %q", s)
	}
	if _, rej, _ := e.Counts(); rej != 1 {
		t.Errorf("emitter rejected = %d, want 1", rej)
	}
	if res, _ := v.w.End(ctx0()); res.Rejected != 1 {
		t.Errorf("writer rejected = %d, want 1 (counted once)", res.Rejected)
	}
}

func TestEmitterAndWriterCountsAgree(t *testing.T) {
	v, e := newEnv(t, nil)
	good := v.rec()
	undeclaredType := v.rec()
	undeclaredType.Type = "contact"
	outside := v.rec()
	outside.ArtifactID = v.art2 // in the manifest, not part of the bundle or the ingest
	outOfRange := v.rec()
	outOfRange.Range = &records.Range{Offset: 1 << 40, Length: 5}
	for _, r := range []records.Record{good, undeclaredType, outside, outOfRange, good} {
		if err := e.Emit(ctx0(), r); err != nil {
			t.Fatalf("Emit = %v", err)
		}
	}
	acc, rej, _ := e.Counts()
	if acc != 2 || rej != 3 {
		t.Errorf("emitter accepted %d rejected %d, want 2 and 3", acc, rej)
	}
	res, err := v.w.End(ctx0())
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != rej {
		t.Errorf("end entry rejected = %d, emitter counted %d: a refusal must be counted once, not twice", res.Rejected, rej)
	}
	n := 0
	for _, w := range v.audit(t, evidence.ActionAnalysisWarning) {
		if w.Details[evidence.WarnKeyRejected] == true {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d rejected warnings audited, want 3", n)
	}
}

func TestJobRecordCap(t *testing.T) {
	v, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.MaxRecords = 10 })
	for i := 0; i < 10; i++ {
		if err := e.Emit(ctx0(), v.rec()); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, parse.ErrRecordCap) {
		t.Fatalf("11th Emit = %v, want ErrRecordCap", err)
	}
	if acc, _, _ := e.Counts(); acc != 10 || v.spy.adds.Load() != 10 {
		t.Errorf("accepted %d, writer Adds %d, want exactly 10", acc, v.spy.adds.Load())
	}
}

func TestJobRejectedCap(t *testing.T) {
	for name, bad := range map[string]func(*env) records.Record{
		"emitter-level": func(v *env) records.Record { r := v.rec(); r.Type = "call"; return r },
		"writer-level":  func(v *env) records.Record { r := v.rec(); r.Summary = "a\x00b"; return r },
	} {
		t.Run(name, func(t *testing.T) {
			v, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.MaxRejected = 3 })
			for i := 1; i <= 3; i++ {
				err := e.Emit(ctx0(), bad(v))
				if i < 3 && err != nil {
					t.Fatalf("rejection %d returned %v", i, err)
				}
				if i == 3 && !errors.Is(err, parse.ErrRejectedCap) {
					t.Fatalf("third rejection = %v, want ErrRejectedCap", err)
				}
			}
			if _, rej, _ := e.Counts(); rej != 3 {
				t.Errorf("rejected = %d, want 3", rej)
			}
		})
	}
}

func TestEmitterSealedAfterSeal(t *testing.T) {
	v, e := newEnv(t, nil)
	e.Seal()
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("Emit after seal = %v", err)
	}
	if err := e.Warn(ctx0(), "x", "y"); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("Warn after seal = %v", err)
	}
	e.Note("k", "v")
	e.Progress(5, 5)
	if v.spy.adds.Load() != 0 || len(v.spy.warns) != 0 || len(v.spy.rejects) != 0 {
		t.Error("something reached the writer after seal")
	}
	if len(e.Notes()) != 0 || len(v.pump) != 0 {
		t.Errorf("Note/Progress after seal were not no-ops: %v, pump %d", e.Notes(), len(v.pump))
	}
}

func TestEmitterLateCallsAfterAbandonRefused(t *testing.T) {
	v, e := newEnv(t, nil)
	release := make(chan struct{})
	late := make(chan error, 1)
	g := artparse.Guard(ctx0(), 20*time.Millisecond, 20*time.Millisecond, e.Seal, nil, func(context.Context) error {
		<-release // does not cooperate
		late <- e.Emit(ctx0(), v.rec())
		return nil
	})
	if !g.Abandoned {
		t.Fatalf("got %+v", g)
	}
	close(release)
	select {
	case err := <-late:
		if !errors.Is(err, parse.ErrSealed) {
			t.Errorf("late Emit = %v, want ErrSealed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late Emit never returned")
	}
	if acc, _, _ := e.Counts(); acc != 0 || v.spy.adds.Load() != 0 {
		t.Errorf("record count changed after abandonment: %d", acc)
	}
}

func TestEmitterSealWaitsForInFlightAdd(t *testing.T) {
	v, e := newEnv(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	v.spy.addHook = func(records.Record) {
		close(entered)
		<-release
	}
	emitted := make(chan error, 1)
	go func() { emitted <- e.Emit(ctx0(), v.rec()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the Add never started")
	}
	sealed := make(chan struct{})
	go func() { e.Seal(); close(sealed) }()
	select {
	case <-sealed:
		t.Fatal("seal returned while an Add was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	// the seal flag is already set: a new call is refused although the old Add is still running
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("Emit during seal = %v", err)
	}
	close(release)
	select {
	case <-sealed:
	case <-time.After(5 * time.Second):
		t.Fatal("seal did not return after the Add finished")
	}
	if err := <-emitted; err != nil {
		t.Errorf("in-flight Emit = %v", err)
	}
	if v.spy.adds.Load() != 1 {
		t.Errorf("Adds = %d, want 1 (none after seal)", v.spy.adds.Load())
	}
}

// blockedCtx is a parser-defined Context whose Err and Done block for ever.
type blockedCtx struct{ block chan struct{} }

func (blockedCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c blockedCtx) Done() <-chan struct{}     { <-c.block; return nil }
func (c blockedCtx) Err() error                { <-c.block; return nil }
func (blockedCtx) Value(any) any               { return nil }

func TestBlockingParserCtxCannotWedgeSealOrAbandon(t *testing.T) {
	v, e := newEnv(t, nil)
	bc := blockedCtx{block: make(chan struct{})}
	defer close(bc.block)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Emit(bc, v.rec()); err != nil {
			t.Errorf("Emit = %v", err)
		}
		if err := e.Warn(bc, "loc", "why"); err != nil {
			t.Errorf("Warn = %v", err)
		}
		g := artparse.Guard(ctx0(), 20*time.Millisecond, 20*time.Millisecond, e.Seal, nil, func(context.Context) error {
			select {} // never returns
		})
		if !g.Abandoned {
			t.Errorf("got %+v", g)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a parser's blocking Context wedged the host")
	}
}

func TestMutatingInputsCannotDefeatHostChecks(t *testing.T) {
	conf := 60
	v, _ := newEnv(t, nil)
	art := parse.Artifact{
		ID: v.art, Platform: parse.PlatformAndroid,
		Recovery: &parse.RecoveryInfo{Method: "carve-x", Confidence: &conf},
		Source:   parse.SourceInfo{Snapshot: &parse.SnapshotInfo{Name: "snap1", Xid: 261}},
	}
	arts := map[string]parse.Artifact{"db": art}
	em, err := artparse.NewEmitter(ctx0(), v.spy, v.reg, arts, parse.DefaultLimits(), v.pump, func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	// what a parser can do to the Input it was handed: clear Recovery, change the snapshot, drop the entry
	art.Recovery.Method, *art.Recovery.Confidence = "x", 1
	art.Recovery = nil
	art.Source.Snapshot.Xid = 9
	delete(arts, "db")
	if err := em.Emit(ctx0(), v.rec()); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) == 0 {
		t.Fatal("nothing reached the writer")
	}
	got := v.spy.added[len(v.spy.added)-1]
	if !got.Deleted || got.Recovery != "carve-x" || got.Confidence == nil || *got.Confidence != 60 {
		t.Errorf("recovered-ness was lost: %+v", got)
	}
	if s, _ := got.Payload["snapshot"].(map[string]any); s["xid"] != uint64(261) {
		t.Errorf("stamped snapshot = %v", got.Payload["snapshot"])
	}
	// Meta().Emits is copied once: appending to a copy the parser holds changes nothing
	m := v.reg.Meta()
	m.Emits = append(m.Emits, parse.Emit{Type: "call", PayloadVersion: 1})
	r := v.rec()
	r.Type = "call"
	if err := em.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if v.spy.adds.Load() != 1 {
		t.Error("an undeclared type got through after the parser edited its Emits copy")
	}
	// BudgetView.Free for far more than it allocated leaves the host accounting intact
	b := parse.NewBudget(100)
	view, other := b.View(), b.View()
	if err := view.Alloc(40); err != nil {
		t.Fatal(err)
	}
	if err := other.Alloc(50); err != nil {
		t.Fatal(err)
	}
	view.Free(1 << 40)
	if b.Used() != 50 {
		t.Errorf("Used = %d after an over-free, want 50", b.Used())
	}
	if err := other.Alloc(60); !errors.Is(err, parse.ErrBudget) {
		t.Errorf("Alloc beyond the limit = %v, want ErrBudget", err)
	}
}

func TestEmitterCopiesPayloadBeforeAdd(t *testing.T) {
	v, e := newEnv(t, nil)
	r := v.rec()
	nested := map[string]any{"k": "v"}
	list := []any{"a", "b"}
	r.Payload["extra"] = nested
	r.Payload["list"] = list
	v.spy.addHook = func(got records.Record) {
		// a parser goroutine mutating its payload while the writer reads the copy
		nested["k"] = "mutated"
		list[0] = "mutated"
		r.Payload["late"] = "late"
		time.Sleep(10 * time.Millisecond)
		if got.Payload["extra"].(map[string]any)["k"] != "v" || got.Payload["list"].([]any)[0] != "a" {
			t.Errorf("the copy handed to the writer changed under it: %v", got.Payload)
		}
	}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) == 0 {
		t.Fatal("nothing reached the writer")
	}
	stored := v.spy.added[0]
	if stored.Payload["extra"].(map[string]any)["k"] != "v" || stored.Payload["list"].([]any)[0] != "a" {
		t.Errorf("stored payload was affected: %v", stored.Payload)
	}
	if _, ok := stored.Payload["late"]; ok {
		t.Errorf("late top-level mutation reached the stored record: %v", stored.Payload)
	}
}

func TestEmitterRejectsRunawayPayload(t *testing.T) {
	v, e := newEnv(t, nil)
	cyc := map[string]any{}
	cyc["self"] = cyc
	r := v.rec()
	r.Payload["cycle"] = cyc
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatalf("Emit = %v, want nil (a counted rejection, no stack overflow)", err)
	}
	if v.spy.adds.Load() != 0 || len(v.spy.rejects) != 1 {
		t.Errorf("adds %d rejects %v", v.spy.adds.Load(), v.spy.rejects)
	}
}

func recoveredEnv(t *testing.T) (*env, *artparse.Emitter) {
	conf := 60
	return newEnv(t, func(_ *parse.Limits, a *parse.Artifact) {
		a.Recovery = &parse.RecoveryInfo{Class: "carved", Method: "carve-x", Confidence: &conf}
	})
}

func TestEmitterPropagatesRecoveredArtifact(t *testing.T) {
	v, e := recoveredEnv(t)
	hi, lo := 80, 30
	for _, c := range []struct {
		in   *int
		want int
	}{{&hi, 60}, {nil, 60}, {&lo, 30}} {
		r := v.rec()
		r.Confidence = c.in
		if err := e.Emit(ctx0(), r); err != nil {
			t.Fatal(err)
		}
		if len(v.spy.added) == 0 {
			t.Fatal("nothing reached the writer")
		}
		got := v.spy.added[len(v.spy.added)-1]
		if !got.Deleted || got.Recovery != "carve-x" || got.Confidence == nil || *got.Confidence != c.want {
			t.Errorf("record %+v, want deleted, carve-x, confidence %d", got, c.want)
		}
		rec, _ := got.Payload["recovery"].(map[string]any)
		if rec["artifact_method"] != "carve-x" || rec["artifact_confidence"] != 60 || rec["relation"] != "from-recovered-artifact" {
			t.Errorf("payload.recovery = %v", rec)
		}
		if _, ok := r.Payload["recovery"]; ok {
			t.Error("the caller's payload map was mutated")
		}
	}
}

func TestEmitterKeepsParserRecoveryRelation(t *testing.T) {
	v, e := recoveredEnv(t)
	r := v.rec()
	r.Payload["recovery"] = map[string]any{"relation": "superseded-version"}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) == 0 {
		t.Fatal("nothing reached the writer")
	}
	rec := v.spy.added[0].Payload["recovery"].(map[string]any)
	if rec["relation"] != "superseded-version" || rec["artifact_relation"] != "from-recovered-artifact" || rec["artifact_method"] != "carve-x" {
		t.Errorf("payload.recovery = %v", rec)
	}
}

func TestNewEmitterRejectsBadRecoveryInfo(t *testing.T) {
	v, _ := newEnv(t, nil)
	n101, nNeg := 101, -1
	for name, ri := range map[string]*parse.RecoveryInfo{
		"method with space and capital": {Method: "Carve X"},
		"empty method":                  {Method: ""},
		"confidence 101":                {Method: "carve-x", Confidence: &n101},
		"confidence -1":                 {Method: "carve-x", Confidence: &nNeg},
	} {
		art := parse.Artifact{ID: v.art, Platform: parse.PlatformAndroid, Recovery: ri}
		if _, err := artparse.NewEmitter(ctx0(), v.spy, v.reg, map[string]parse.Artifact{"db": art}, parse.DefaultLimits(), v.pump, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestEmitterStampsSnapshot(t *testing.T) {
	v, e := newEnv(t, func(_ *parse.Limits, a *parse.Artifact) {
		a.Source.Snapshot = &parse.SnapshotInfo{Name: "snap1", Xid: 261}
	})
	r := v.rec()
	r.Payload["snapshot"] = map[string]any{"name": "forged", "xid": uint64(1)}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) == 0 {
		t.Fatal("nothing reached the writer")
	}
	got := v.spy.added[0].Payload["snapshot"].(map[string]any)
	if got["name"] != "snap1" || got["xid"] != uint64(261) || len(got) != 2 {
		t.Errorf("payload.snapshot = %v", got)
	}
}

func TestEmitterRecordsOfLiveArtifactAreUntouched(t *testing.T) {
	v, e := newEnv(t, nil)
	r := v.rec()
	r.Locator = "row:id=1"
	r.Range = &records.Range{Offset: 1, Length: 2}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	if len(v.spy.added) == 0 {
		t.Fatal("nothing reached the writer")
	}
	if got := v.spy.added[0]; !reflect.DeepEqual(got, r) {
		t.Errorf("a live record was changed:\n got %+v\nwant %+v", got, r)
	}
}

func TestEmitterFatalErrorsPassThrough(t *testing.T) {
	v, e := newEnv(t, nil)
	boom := errors.New("database is locked")
	v.spy.addErr = boom
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, boom) {
		t.Fatalf("Emit = %v, want the writer's error", err)
	}
	if len(v.spy.warns)+len(v.spy.rejects) != 0 {
		t.Error("a writer failure was warned about")
	}
	if acc, rej, _ := e.Counts(); acc != 0 || rej != 0 {
		t.Errorf("counts %d/%d after a failure", acc, rej)
	}
}

func TestEmitterHostContextEndsCalls(t *testing.T) {
	v, _ := newEnv(t, nil)
	hctx, cancel := context.WithCancel(ctx0())
	e, err := artparse.NewEmitter(hctx, v.spy, v.reg, nil, parse.DefaultLimits(), v.pump, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := e.Emit(ctx0(), v.rec()); !errors.Is(err, context.Canceled) {
		t.Errorf("Emit = %v, want the host context's error", err)
	}
	if v.spy.adds.Load() != 0 {
		t.Error("Add called after the host context ended")
	}
}

func TestEmitterWarnForwardsAndCounts(t *testing.T) {
	v, e := newEnv(t, nil)
	if err := e.Warn(ctx0(), "row:id=1", "table damaged"); err != nil {
		t.Fatal(err)
	}
	if _, _, w := e.Counts(); w != 1 || len(v.spy.warns) != 1 {
		t.Errorf("warnings %d, forwarded %v", w, v.spy.warns)
	}
	if ws := v.audit(t, evidence.ActionAnalysisWarning); len(ws) != 1 || ws[0].Details["reason"] != "table damaged" {
		t.Errorf("audited %+v", ws)
	}
}

func TestEmitterNoteBounded(t *testing.T) {
	_, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.MaxNotes = 3 })
	e.Note("a", "first")
	e.Note("a", "second") // first write of a key wins
	e.Note(strings.Repeat("k", 100), strings.Repeat("v", 5000))
	e.Note("nul", "x\x00y")
	e.Note("dropped", "over the MaxNotes cap")
	n := e.Notes()
	if len(n) != 3 || n["a"] != "first" || n["dropped"] != "" {
		t.Fatalf("notes = %v", n)
	}
	for k, val := range n {
		if len(k) > 64 || len(val) > 1024 {
			t.Errorf("note %q is %d/%d bytes", k[:min(len(k), 10)], len(k), len(val))
		}
	}
	if n["nul"] != "x?y" {
		t.Errorf("nul note = %q", n["nul"])
	}
	n["a"] = "mutated" // notes() hands out a copy
	if e.Notes()["a"] != "first" {
		t.Error("notes() leaks its map")
	}
}

func TestEmitterProgressThrottled(t *testing.T) {
	v, _ := newEnv(t, nil)
	var got atomic.Int32
	var lastDone atomic.Int64
	pump := make(chan func(), 1)
	e, err := artparse.NewEmitter(ctx0(), v.spy, v.reg, nil, parse.DefaultLimits(), pump, func(done, _ int64) { got.Add(1); lastDone.Store(done) })
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i < 50; i++ {
		e.Progress(i, 100) // faster than 200 ms: only the first is forwarded
	}
	if len(pump) != 1 {
		t.Fatalf("queued %d closures, want 1", len(pump))
	}
	(<-pump)()
	if got.Load() != 1 || lastDone.Load() != 1 {
		t.Fatalf("forwarded %d (last %d)", got.Load(), lastDone.Load())
	}
	e.Progress(100, 100) // the final call is never throttled
	if len(pump) != 1 {
		t.Fatal("the final call was throttled")
	}
	(<-pump)()
	if lastDone.Load() != 100 {
		t.Errorf("last forwarded done = %d, want 100", lastDone.Load())
	}
	// a full pump never blocks the parser
	pump <- func() {}
	finished := make(chan struct{})
	go func() { e.Progress(100, 100); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Progress blocked on a full pump")
	}
}

func TestEmitterConcurrentCallsSerialised(t *testing.T) {
	v, e := newEnv(t, func(l *parse.Limits, _ *parse.Artifact) { l.MaxRecords = 100 })
	var wg sync.WaitGroup
	var capped atomic.Int32
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				switch err := e.Emit(ctx0(), v.rec()); {
				case err == nil:
				case errors.Is(err, parse.ErrRecordCap):
					capped.Add(1)
				default:
					t.Errorf("Emit = %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if acc, _, _ := e.Counts(); acc != 100 || v.spy.adds.Load() != 100 || capped.Load() != 100 {
		t.Errorf("accepted %d, Adds %d, capped %d; want 100, 100, 100", acc, v.spy.adds.Load(), capped.Load())
	}
	res, err := v.w.End(ctx0())
	if err != nil || res.Records != 100 {
		t.Errorf("writer holds %d records (%v), want 100", res.Records, err)
	}
}
