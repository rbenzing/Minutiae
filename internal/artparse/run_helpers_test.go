package artparse_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// rx is a case with one acquisition ("D1"/"A1", Android) holding a three-line file per fake parser name.
type rx struct {
	t    *testing.T
	c    *evidence.Case
	recs map[string]evidence.ManifestRecord
}

const rxData = "alpha\nbeta\ngamma\n"

func newRx(t *testing.T, names ...string) *rx {
	t.Helper()
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	f := &rx{t: t, c: c, recs: map[string]evidence.ManifestRecord{}}
	for _, n := range names {
		f.recs[n] = putFile(t, c, "D1", "A1", fakePath(n), rxData)
	}
	return f
}

func rxRegWithHash(t testing.TB, p parse.Parser, hash string) artparse.Registered {
	t.Helper()
	r, err := artparse.Register(p, hash)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rxReg(t testing.TB, p parse.Parser) artparse.Registered {
	t.Helper()
	m := p.Meta()
	return rxRegWithHash(t, p, parsertest.FakeHash(m.Name+"@"+m.Version))
}

func wbp(name string) parsertest.WellBehaved {
	return parsertest.WellBehaved{Name: name, Version: "1.0.0"}
}

// host builds a host over the case with fast limits; mod may change them.
func (f *rx) host(mod func(*artparse.Options), ps ...parse.Parser) *artparse.Host {
	f.t.Helper()
	regs := make([]artparse.Registered, len(ps))
	for i, p := range ps {
		regs[i] = rxReg(f.t, p)
	}
	return hostWith(f.t, f.c, func(o *artparse.Options) {
		artparse.SkipLimitMinimums(o)
		if mod != nil {
			mod(o)
		}
	}, regs...)
}

// run runs the host and requires no run-level error.
func (f *rx) run(h *artparse.Host, sel artparse.Selection) artparse.RunSummary {
	f.t.Helper()
	sum, err := h.Run(context.Background(), artparse.RunOptions{Selection: sel})
	if err != nil {
		f.t.Fatalf("Run: %v", err)
	}
	return sum
}

// entries returns the audit entries of the given actions, in log order.
func (f *rx) entries(actions ...string) []evidence.AuditEntry {
	f.t.Helper()
	all, err := f.c.ReadAudit()
	if err != nil {
		f.t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		for _, a := range actions {
			if e.Action == a {
				out = append(out, e)
			}
		}
	}
	return out
}

func (f *rx) count(action string) int { return len(f.entries(action)) }

func decodeEntry[T any](t testing.TB, e evidence.AuditEntry) T {
	t.Helper()
	v, err := evidence.DecodeDetails[T](e.Details)
	if err != nil {
		t.Fatalf("audit entry %d (%s): %v", e.Seq, e.Action, err)
	}
	return v
}

func (f *rx) jobEnds() []artparse.JobEnd {
	f.t.Helper()
	var out []artparse.JobEnd
	for _, e := range f.entries(artparse.ActionParseJobEnd) {
		out = append(out, decodeEntry[artparse.JobEnd](f.t, e))
	}
	return out
}

func (f *rx) jobStarts() []artparse.JobStart {
	f.t.Helper()
	var out []artparse.JobStart
	for _, e := range f.entries(artparse.ActionParseJobStart) {
		out = append(out, decodeEntry[artparse.JobStart](f.t, e))
	}
	return out
}

// nRecords counts every stored record, superseded runs included.
func (f *rx) nRecords() int {
	f.t.Helper()
	rd, err := records.NewReader(f.c)
	if err != nil {
		f.t.Fatal(err)
	}
	n, _, err := rd.Count(context.Background(), records.Filter{IncludeSuperseded: true}, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return int(n)
}

// rows lists the stored records, superseded runs shown or hidden.
func (f *rx) rows(includeSuperseded bool) []records.Row {
	f.t.Helper()
	rd, err := records.NewReader(f.c)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := rd.List(context.Background(), records.Filter{IncludeSuperseded: includeSuperseded}, records.Page{Limit: records.MaxLimit})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Rows
}

func jobOf(t testing.TB, s artparse.RunSummary, parser string) artparse.JobResult {
	t.Helper()
	for _, j := range s.Jobs {
		if j.Parser == parser {
			return j
		}
	}
	t.Fatalf("no job of parser %q in %+v", parser, s.Jobs)
	return artparse.JobResult{}
}

func rxRecord(in *parse.Input, i int) records.Record {
	return records.Record{
		Type: message.Type, ArtifactID: in.Primary.ID, Locator: "text:line=" + strconv.Itoa(i),
		Summary: "s" + strconv.Itoa(i), Payload: recordstest.ValidPayload(message.Type),
	}
}

// rxKept is what a parser kept of the handles the host gave it.
type rxKept struct {
	mu  sync.Mutex // a parser goroutine abandoned by design writes while the test reads
	in  *parse.Input
	out parse.Emitter
}

func (k *rxKept) store(in *parse.Input, out parse.Emitter) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.in, k.out = in, out
}

func (k *rxKept) load() (*parse.Input, parse.Emitter) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.in, k.out
}

// assertSealed requires every kept handle to refuse: a goroutine a parser left behind can neither emit
// nor read.
func (k *rxKept) assertSealed(t *testing.T) {
	t.Helper()
	in, out := k.load()
	if in == nil || out == nil {
		t.Fatal("the parser kept nothing")
	}
	if err := out.Emit(context.Background(), rxRecord(in, 99)); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("late Emit = %v, want ErrSealed", err)
	}
	if err := out.Warn(context.Background(), "late", "late"); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("late Warn = %v, want ErrSealed", err)
	}
	var b [1]byte
	if _, err := in.Primary.R.ReadAt(b[:], 0); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("late ReadAt = %v, want ErrSealed", err)
	}
}

// rxParser emits n records, then runs after (when set). It keeps its handles in kept.
type rxParser struct {
	name  string
	n     int
	after func(ctx context.Context, in *parse.Input, out parse.Emitter) error
	kept  *rxKept
}

func (p *rxParser) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *rxParser) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	var b [4]byte
	_, _ = in.Primary.R.ReadAt(b[:], 0)
	return parse.Applicability{Status: parse.Applicable}, nil
}

func (p *rxParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if p.kept != nil {
		p.kept.store(in, out)
	}
	for i := 1; i <= p.n; i++ {
		if err := out.Emit(ctx, rxRecord(in, i)); err != nil {
			return err
		}
	}
	if p.after != nil {
		return p.after(ctx, in, out)
	}
	return nil
}

// rxWriter wraps the records writer of a job.
type rxWriter struct {
	artparse.IngestWriter
	adds       int
	afterAdd   func(adds int)
	abortErr   error
	startErr   error  // Start fails with it, the real writer is never started
	endErr     error  // End fails with it, the real writer is not ended
	flushErr   error  // Flush fails with it
	concluding func() // called when the host starts to conclude the ingest (Flush, End or Abort), i.e. after the job's seal
	aborted    func() // called after the real Abort has returned (its run row and audit entry are then stored)
}

func (w *rxWriter) conclude() {
	if w.concluding != nil {
		w.concluding()
	}
}

func (w *rxWriter) Start(ctx context.Context, so records.StartOptions) error {
	if w.startErr != nil {
		return w.startErr
	}
	return w.IngestWriter.Start(ctx, so)
}

func (w *rxWriter) End(ctx context.Context) (records.IngestResult, error) {
	w.conclude()
	if w.endErr != nil {
		return records.IngestResult{}, w.endErr
	}
	return w.IngestWriter.End(ctx)
}

func (w *rxWriter) Flush(ctx context.Context) error {
	w.conclude()
	if w.flushErr != nil {
		return w.flushErr
	}
	return w.IngestWriter.Flush(ctx)
}

func (w *rxWriter) Add(ctx context.Context, r records.Record) error {
	err := w.IngestWriter.Add(ctx, r)
	w.adds++
	if w.afterAdd != nil {
		w.afterAdd(w.adds)
	}
	return err
}

func (w *rxWriter) Abort(ctx context.Context, cause error) (records.IngestResult, error) {
	w.conclude()
	res, err := w.IngestWriter.Abort(ctx, cause)
	if w.aborted != nil {
		w.aborted()
	}
	if err != nil {
		return res, err
	}
	return res, w.abortErr // the real Abort concluded the ingest; the injected error is what the host must handle
}

func withWriter(wrap func(*rxWriter)) func(*artparse.Options) {
	return func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(c *evidence.Case, p records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, p, wo)
			if err != nil {
				return nil, err
			}
			rw := &rxWriter{IngestWriter: w}
			wrap(rw)
			return rw, nil
		})
	}
}

func withLimits(f func(*parse.Limits)) func(*artparse.Options) {
	return func(o *artparse.Options) { f(&o.Limits) }
}

func both(fs ...func(*artparse.Options)) func(*artparse.Options) {
	return func(o *artparse.Options) {
		for _, f := range fs {
			if f != nil {
				f(o)
			}
		}
	}
}

func shortTimeouts(o *artparse.Options) {
	o.Limits.Timeout = 150 * time.Millisecond
	o.Limits.GracePeriod = 30 * time.Millisecond
}
