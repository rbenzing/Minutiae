package parsertest

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// testEmitter is a parse.Emitter for tests that call a fake's Parse directly.
type testEmitter struct {
	emitErr error
	recs    []records.Record
	warns   int
}

func (e *testEmitter) Emit(_ context.Context, r records.Record) error {
	if e.emitErr != nil {
		return e.emitErr
	}
	e.recs = append(e.recs, r)
	return nil
}
func (e *testEmitter) Warn(context.Context, string, string) error { e.warns++; return nil }
func (e *testEmitter) Note(string, string)                        {}
func (e *testEmitter) Progress(int64, int64)                      {}

func allFakes() []parse.Parser {
	return []parse.Parser{
		WellBehaved{Name: "a", Version: "1.0.0"},
		Panicking{Name: "b", Version: "1.0.0"},
		Slow{Name: "c", Version: "1.0.0"},
		BlockingCtxParser{Name: "d", Version: "1.0.0"},
		&Mutator{Name: "e", Version: "1.0.0"},
		Liar{Name: "f", Version: "1.0.0"},
		Liar{Name: "g", Version: "1.0.0", Mode: LieForeignPlatform},
		Invalid{Name: "h", Version: "1.0.0"},
		Hog{Name: "i", Version: "1.0.0"},
		Spammer{Name: "j", Version: "1.0.0"},
		ProbeOnly{Name: "k", Version: "1.0.0"},
		Nondeterministic{Name: "l", Version: "1.0.0"},
	}
}

func TestFakeParsersMetaIsValid(t *testing.T) {
	for _, p := range allFakes() {
		m := p.Meta()
		if err := parse.ValidateMeta(m); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
		if m.Inputs[0].Globs[0] != "android:**/parsertest/"+m.Name+".dat" && m.Platforms[0] == parse.PlatformAndroid {
			t.Errorf("%s: primary glob %q", m.Name, m.Inputs[0].Globs[0])
		}
		for _, e := range m.Emits {
			typ, ok := records.LookupType(e.Type)
			switch {
			case !ok:
				t.Errorf("%s declares %q, which is not registered", m.Name, e.Type)
			case typ.PayloadVersion != e.PayloadVersion:
				t.Errorf("%s declares %q v%d, registered v%d", m.Name, e.Type, e.PayloadVersion, typ.PayloadVersion)
			case typ.Validate == nil:
				t.Errorf("%s declares %q, which has no validator", m.Name, e.Type)
			}
		}
	}
	if h := FakeHash("x"); len(h) != 76 || !strings.HasPrefix(h, "src1:sha256:") {
		t.Errorf("FakeHash %q", h)
	}
}

func TestFakeProbesAreApplicable(t *testing.T) {
	h, art, _ := bundle(t, t, "a", "x\n")
	for _, p := range []parse.Parser{WellBehaved{Name: "a", Version: "1.0.0"}, Spammer{Name: "a", Version: "1.0.0"}, Hog{Name: "a", Version: "1.0.0"}} {
		a, err := p.Probe(context.Background(), h.Input(p, art, nil, true))
		if err != nil || a.Status != parse.Applicable {
			t.Errorf("%T: %v %v", p, a, err)
		}
	}
	po := ProbeOnly{Name: "a", Version: "1.0.0", Status: parse.UnsupportedSchema, Reason: "r", Err: errors.New("e")}
	a, err := po.Probe(context.Background(), h.Input(po, art, nil, true))
	if a != (parse.Applicability{Status: parse.UnsupportedSchema, Reason: "r"}) || err == nil || err.Error() != "e" {
		t.Errorf("ProbeOnly: %v %v", a, err)
	}
	if res := h.RunLenient(po, h.Input(po, art, nil, false)); len(res.Records) != 0 || len(res.Warnings) != 0 {
		t.Errorf("ProbeOnly parsed something: %+v", res)
	}
}

// catch runs f and returns what it panicked with (nil when it did not).
func catch(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestPanickingFake(t *testing.T) {
	h, art, _ := bundle(t, t, "pan", "x\n")
	ctx := context.Background()
	for name, val := range map[string]any{
		"string": "boom", "error": errors.New("boom"), "runtime error": RuntimePanicValue(), "Error panics": ErrorPanics{},
	} {
		t.Run(name, func(t *testing.T) {
			p := Panicking{Name: "pan", Version: "1.0.0", Where: PanicInProbe, Value: val}
			if got := catch(func() { _, _ = p.Probe(ctx, h.Input(p, art, nil, true)) }); got != val {
				t.Errorf("probe panicked with %v, want %v", got, val)
			}
			p.Where = PanicInParse
			if got := catch(func() { _ = p.Parse(ctx, h.Input(p, art, nil, false), &testEmitter{}) }); got != val {
				t.Errorf("parse panicked with %v, want %v", got, val)
			}
		})
	}
	if _, ok := RuntimePanicValue().(runtime.Error); !ok {
		t.Errorf("RuntimePanicValue is %T, want a runtime.Error", RuntimePanicValue())
	}
	if catch(func() { _ = ErrorPanics{}.Error() }) == nil {
		t.Error("ErrorPanics.Error did not panic")
	}

	p := Panicking{Name: "pan", Version: "1.0.0", Where: PanicAfterEmits, Value: "late", After: 2}
	em := &testEmitter{}
	if got := catch(func() { _ = p.Parse(ctx, h.Input(p, art, nil, false), em) }); got != "late" || len(em.recs) != 2 {
		t.Errorf("after emits: panic %v, %d records", got, len(em.recs))
	}
}

func TestSlowFakeFinishesAfterRelease(t *testing.T) {
	h, art, _ := bundle(t, t, "slow", "x\n")
	s := Slow{Name: "slow", Version: "1.0.0", Release: make(chan struct{}), Done: make(chan struct{}), Late: &LateResults{}}
	in := h.Input(s, art, nil, false)
	em := &testEmitter{emitErr: errors.New("abandoned")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	returned := make(chan error, 1)
	go func() { returned <- s.Parse(ctx, in, em) }()

	select {
	case err := <-returned:
		t.Fatalf("a non-cooperative parser returned on a cancelled context: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	in.Primary.R.(*parse.SealedReaderAt).Seal() // what the host does when it abandons the job
	close(s.Release)
	select {
	case <-s.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("Done was not closed after Release")
	}
	if s.Late.EmitErr == nil || s.Late.EmitErr.Error() != "abandoned" || !errors.Is(s.Late.ReadErr, parse.ErrSealed) {
		t.Errorf("late results: emit %v, read %v", s.Late.EmitErr, s.Late.ReadErr)
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Parse did not return after Release")
	}
}

func TestSlowCooperativeStopsOnContext(t *testing.T) {
	h, art, _ := bundle(t, t, "slow", "x\n")
	s := Slow{Name: "slow", Version: "1.0.0", Cooperative: true}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- s.Parse(ctx, h.Input(s, art, nil, false), &testEmitter{}) }()
	select {
	case err := <-returned:
		t.Fatalf("returned before the context ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop on cancel")
	}
}

func TestBlockingCtxParserCompletes(t *testing.T) {
	h, art, _ := bundle(t, t, "blk", "x\n")
	p := BlockingCtxParser{Name: "blk", Version: "1.0.0"}
	done := make(chan Result, 1)
	go func() { done <- h.RunLenient(p, h.Input(p, art, nil, false)) }()
	select {
	case res := <-done:
		if len(res.Records) != 1 || len(res.Warnings) != 1 {
			t.Errorf("records %d, warnings %v", len(res.Records), res.Warnings)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the harness blocked on the parser's context")
	}
}

func TestMutatorMutatesWhatItWasGiven(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		h, art, _ := bundle(t, t, "mut", "x\n")
		art.Recovery = &parse.RecoveryInfo{Class: "carved", Method: "m"}
		art.Source.Snapshot = &parse.SnapshotInfo{Name: "s", Xid: 1}
		p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutateInput}
		in := h.Input(p, art, nil, false)
		h.RunLenient(p, in)
		if in.Primary.SHA256 == art.SHA256 || in.Primary.Recovery != nil && in.Primary.Recovery.Class == "carved" {
			t.Errorf("the mutator did not change its input copy: %+v", in.Primary)
		}
		if art.Recovery == nil || art.Recovery.Class != "carved" || art.Source.Snapshot.Xid != 1 {
			t.Errorf("the original artifact was changed: %+v", art)
		}
	})
	t.Run("meta", func(t *testing.T) {
		p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutateMeta}
		m := p.Meta() // the copy the host keeps
		before := m.Clone()
		h, art, _ := bundle(t, t, "mut", "x\n")
		h.RunLenient(p, h.Input(p, art, nil, false))
		if m.Inputs[0].Globs[0] == before.Inputs[0].Globs[0] || m.Platforms[0] == before.Platforms[0] {
			t.Errorf("the mutator left the Meta it returned alone: %+v", m)
		}
	})
	t.Run("budget", func(t *testing.T) {
		b := parse.NewBudget(1 << 30)
		if err := b.Alloc(100); err != nil {
			t.Fatal(err)
		}
		p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutateBudget}
		if err := p.Parse(context.Background(), &parse.Input{Budget: b.View()}, &testEmitter{}); err != nil {
			t.Fatal(err)
		}
		if b.Used() != 100 {
			t.Errorf("budget in use %d after the mutator over-freed, want the host's 100", b.Used())
		}
	})
	t.Run("payload after emit", func(t *testing.T) {
		h, art, _ := bundle(t, t, "mut", "x\n")
		p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutatePayloadAfterEmit}
		res := h.RunLenient(p, h.Input(p, art, nil, false))
		if len(res.Records) != 1 || len(res.Warnings) != 0 {
			t.Fatalf("records %d, warnings %v", len(res.Records), res.Warnings)
		}
		raw, _ := res.Records[0].Payload["raw"].(map[string]any)
		if raw["k"] != "orig" || res.Records[0].Payload["kind"] != "text" {
			t.Errorf("the result changed after Emit: %v", res.Records[0].Payload)
		}
	})
}

func TestLiarFakes(t *testing.T) {
	p := Liar{Name: "liar", Version: "1.0.0", Mode: LieUndeclaredType}
	em := &testEmitter{}
	h, art, _ := bundle(t, t, "liar", "x\n")
	if err := p.Parse(context.Background(), h.Input(p, art, nil, false), em); err != nil {
		t.Fatal(err)
	}
	if len(em.recs) != 1 || em.recs[0].Type != "call" {
		t.Fatalf("undeclared type: %+v", em.recs)
	}
	for _, e := range p.Meta().Emits {
		if e.Type == "call" {
			t.Error("the undeclared type is declared")
		}
	}
	fp := Liar{Name: "liar", Version: "1.0.0", Mode: LieForeignPlatform}
	if m := fp.Meta(); m.Platforms[0] != parse.PlatformIOS || !strings.HasPrefix(m.Inputs[0].Globs[0], "ios:") {
		t.Errorf("foreign platform meta: %+v", m)
	}
	em = &testEmitter{}
	if err := fp.Parse(context.Background(), h.Input(p, art, nil, false), em); err != nil || len(em.recs) != 1 || em.recs[0].Type != "message" {
		t.Errorf("foreign platform record: %v, %+v", err, em.recs)
	}
}

func TestHogSpammerAndNondeterministic(t *testing.T) {
	h, art, _ := bundle(t, t, "hog", "x\n")
	hog := Hog{Name: "hog", Version: "1.0.0", Alloc: parse.DefaultLimits().MemBudget + 1}
	if err := hog.Parse(context.Background(), h.Input(hog, art, nil, false), &testEmitter{}); !errors.Is(err, parse.ErrBudget) {
		t.Errorf("Hog: %v, want ErrBudget", err)
	}
	ok := Hog{Name: "hog", Version: "1.0.0", Alloc: 1 << 20}
	if err := ok.Parse(context.Background(), h.Input(ok, art, nil, false), &testEmitter{}); err != nil {
		t.Errorf("a small Hog failed: %v", err)
	}

	sp := Spammer{Name: "hog", Version: "1.0.0", N: 25}
	if res := h.Run(sp, h.Input(sp, art, nil, false)); len(res.Records) != 25 {
		t.Errorf("Spammer emitted %d", len(res.Records))
	}

	nd := Nondeterministic{Name: "hog", Version: "1.0.0"}
	a := CanonicalRecords(h.Run(nd, h.Input(nd, art, nil, false)).Records)
	b := CanonicalRecords(h.Run(nd, h.Input(nd, art, nil, false)).Records)
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Errorf("Nondeterministic gave equal output twice: %v %v", a, b)
	}
}

// ctxProbe is an emitter that calls Err and Done on the context the parser
// handed it, each under a timeout, and records whether they returned.
type ctxProbe struct {
	testEmitter
	errReturned, doneReturned bool
}

func (e *ctxProbe) Emit(ctx context.Context, r records.Record) error {
	e.errReturned = returnsWithin(func() { _ = ctx.Err() })
	e.doneReturned = returnsWithin(func() { _ = ctx.Done() })
	return e.testEmitter.Emit(ctx, r)
}

func returnsWithin(f func()) bool {
	done := make(chan struct{})
	go func() { f(); close(done) }() // a blocked call leaks this goroutine: the fake never unblocks it
	select {
	case <-done:
		return true
	case <-time.After(100 * time.Millisecond):
		return false
	}
}

func TestBlockingCtxParserContextReallyBlocks(t *testing.T) {
	h, art, _ := bundle(t, t, "blk", "x\n")
	p := BlockingCtxParser{Name: "blk", Version: "1.0.0"}
	em := &ctxProbe{}
	if err := p.Parse(context.Background(), h.Input(p, art, nil, false), em); err != nil {
		t.Fatal(err)
	}
	if em.errReturned {
		t.Error("ctx.Err() returned: the fake's context does not block")
	}
	if em.doneReturned {
		t.Error("ctx.Done() returned: the fake's context does not block")
	}
	if len(em.recs) != 1 || em.warns != 1 {
		t.Errorf("records %d, warnings %d", len(em.recs), em.warns)
	}
}

func TestMutatorInputModeChangesEveryFieldItDocuments(t *testing.T) {
	conf := 7
	rec := &parse.RecoveryInfo{Class: "carved", Method: "m", Confidence: &conf}
	snap := &parse.SnapshotInfo{Name: "s", Xid: 1}
	mkArt := func() parse.Artifact {
		return parse.Artifact{ID: "a", SHA256: "orig", Recovery: rec, Source: parse.SourceInfo{Kind: "file", Snapshot: snap}}
	}
	// the pointers are shared on purpose: a mutator that writes through them is seen here
	pRec2, pSnap2 := &parse.RecoveryInfo{Class: "carved"}, &parse.SnapshotInfo{Name: "s", Xid: 1}
	other := mkArt()
	other.Recovery, other.Source.Snapshot = pRec2, pSnap2
	in := &parse.Input{Primary: mkArt(), Artifacts: map[string]parse.Artifact{"primary": mkArt(), "wal": other}}
	p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutateInput}
	if err := p.Parse(context.Background(), in, &testEmitter{}); err != nil {
		t.Fatal(err)
	}
	if in.Primary.SHA256 != "mutated" || in.Primary.Recovery != nil || in.Primary.Source.Kind != "mutated" {
		t.Errorf("Primary: %+v", in.Primary)
	}
	if snap.Xid != 99 {
		t.Errorf("the primary's snapshot was not rewritten in place: %+v", *snap)
	}
	a := in.Artifacts["wal"]
	if a.SHA256 != "mutated" || a.Recovery != nil || a.Source.Kind != "mutated" {
		t.Errorf("artifact: %+v", a)
	}
	if pRec2.Class != "mutated" {
		t.Errorf("the artifact's recovery was not rewritten in place: %+v", *pRec2)
	}
	if pSnap2.Name != "mutated" || pSnap2.Xid != 99 {
		t.Errorf("the artifact's snapshot was not rewritten in place: %+v", *pSnap2)
	}
}

func TestMutatorMetaModeChangesEveryTarget(t *testing.T) {
	p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutateMeta}
	m := p.Meta() // the copy the host keeps
	before := m.Clone()
	h, art, _ := bundle(t, t, "mut", "x\n")
	if err := p.Parse(context.Background(), h.Input(p, art, nil, false), &testEmitter{}); err != nil {
		t.Fatal(err)
	}
	if len(m.Emits) != len(before.Emits) || m.Emits[0] == before.Emits[0] {
		t.Errorf("Emits of the Meta the host holds: %+v, was %+v", m.Emits, before.Emits)
	}
	if m.Platforms[0] == before.Platforms[0] {
		t.Errorf("Platforms of the Meta the host holds: %v", m.Platforms)
	}
	if m.Inputs[0].Globs[0] == before.Inputs[0].Globs[0] {
		t.Errorf("Globs of the Meta the host holds: %v", m.Inputs[0].Globs)
	}
}

func TestMutatorPayloadAfterEmitChangesWhatItKept(t *testing.T) {
	h, art, _ := bundle(t, t, "mut", "x\n")
	p := &Mutator{Name: "mut", Version: "1.0.0", Mode: MutatePayloadAfterEmit}
	em := &testEmitter{}
	if err := p.Parse(context.Background(), h.Input(p, art, nil, false), em); err != nil {
		t.Fatal(err)
	}
	if len(em.recs) != 1 {
		t.Fatalf("records %d", len(em.recs))
	}
	pl := em.recs[0].Payload
	raw, _ := pl["raw"].(map[string]any)
	if raw["k"] != "mutated" || pl["kind"] != "mutated" {
		t.Errorf("the maps the parser kept were not changed after Emit: %v", pl)
	}
}

func TestWellBehavedLinesCap(t *testing.T) {
	h, art, _ := bundle(t, t, "cap", "a\nb\nc\nd\ne\n")
	p := WellBehaved{Name: "cap", Version: "1.0.0", Lines: 2}
	res := h.Run(p, h.Input(p, art, nil, false))
	if len(res.Records) != 2 || res.Notes["lines"] != "2" {
		t.Errorf("records %d, notes %v, want 2 lines", len(res.Records), res.Notes)
	}
	if n := len(res.Progress); n == 0 || res.Progress[n-1] != [2]int64{2, 2} {
		t.Errorf("progress %v", res.Progress)
	}
	all := WellBehaved{Name: "cap", Version: "1.0.0"}
	if res := h.Run(all, h.Input(all, art, nil, false)); len(res.Records) != 5 {
		t.Errorf("no cap: %d records, want 5", len(res.Records))
	}
}

func TestFakeMetaDeclaresTheWalCompanion(t *testing.T) {
	m := FakeMeta("x", "1.0.0")
	if got := m.Inputs[0].Companions; len(got) != 1 || got[0] != "-wal" {
		t.Errorf("companions %v, want [-wal]", got)
	}
}
