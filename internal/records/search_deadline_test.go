package records_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// lateCtx is a context whose deadline has passed while its timer has not fired (Err is still nil),
// the state that made `--timeout 1ns` succeed on some runs.
type lateCtx struct{ context.Context }

func (lateCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// expiringCtx is a context whose deadline the test expires by hand, at a point it chooses.
type expiringCtx struct {
	context.Context
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newExpiringCtx() *expiringCtx {
	return &expiringCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringCtx) Done() <-chan struct{} { return c.done }

func (c *expiringCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *expiringCtx) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = context.DeadlineExceeded
		close(c.done)
	}
}

// textCalls are every Reader call that carries a text query.
func textCalls(t *testing.T, r *records.Reader) map[string]func(context.Context) error {
	t.Helper()
	plain := records.Filter{Text: mustCompile(t, "common", records.TextOptions{})}
	ranked := records.Filter{Text: mustCompile(t, "common", records.TextOptions{Rank: true})}
	return map[string]func(context.Context) error{
		"search": func(ctx context.Context) error {
			_, err := r.Search(ctx, plain, records.Page{Limit: 50}, records.SearchOptions{})
			return err
		},
		"search with snippets": func(ctx context.Context) error {
			_, err := r.Search(ctx, plain, records.Page{Limit: 50}, records.SearchOptions{Snippets: true})
			return err
		},
		"rank": func(ctx context.Context) error {
			_, err := r.Search(ctx, ranked, records.Page{Limit: 50}, records.SearchOptions{})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := r.List(ctx, plain, records.Page{Limit: 50})
			return err
		},
		"count": func(ctx context.Context) error {
			_, _, err := r.Count(ctx, plain, 0)
			return err
		},
		"stats": func(ctx context.Context) error {
			_, err := r.Stats(ctx, plain, "type")
			return err
		},
		"overview": func(ctx context.Context) error {
			_, err := r.Overview(ctx, plain)
			return err
		},
		"term hits": func(ctx context.Context) error {
			_, err := r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "common"}}, 5)
			return err
		},
	}
}

func commonCorpus(t *testing.T, n int) *records.Reader {
	t.Helper()
	c, art := setup(t)
	recs := recordstest.Records(art.ID, n, 3)
	for i := range recs {
		recs[i].Summary = fmt.Sprintf("common token %d", i)
		recs[i].Body = "common words in the body of the record"
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return newReader(t, c)
}

func requireTimeout(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, records.ErrSearchTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%s: %v, want ErrSearchTimeout wrapping DeadlineExceeded", name, err)
	}
}

// TestExpiredDeadlineIsAlwaysATimeout (R58): a deadline that has passed is ErrSearchTimeout for every
// call that carries text, whether or not the context's timer has fired yet, and never "sometimes hits".
func TestExpiredDeadlineIsAlwaysATimeout(t *testing.T) {
	r := commonCorpus(t, 50)
	for name, call := range textCalls(t, r) {
		t.Run(name, func(t *testing.T) {
			for range 20 {
				requireTimeout(t, name+" (timer not fired)", call(lateCtx{context.Background()}))
			}
			dctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Hour))
			defer cancel()
			requireTimeout(t, name+" (timer fired)", call(dctx))
		})
	}
}

// TestDeadlineExpiringBetweenStatementsIsATimeout (R58): the deadline passes right before a MATCH
// statement (the seam), and the call is a timeout whatever the statements that follow would answer.
func TestDeadlineExpiringBetweenStatementsIsATimeout(t *testing.T) {
	r := commonCorpus(t, 50)
	for name, call := range textCalls(t, r) {
		t.Run(name, func(t *testing.T) {
			ec := newExpiringCtx()
			r.SetBeforeQuery(ec.expire)
			defer r.SetBeforeQuery(nil)
			requireTimeout(t, name, call(ec))
		})
	}
}

// TestSearchDeadlineExpiresMidStatement (R53, R59): the deadline passes after the statement has
// started and before its rows are read (the seam fires there, no sleep), and the error is
// ErrSearchTimeout wrapping DeadlineExceeded, whatever the driver reports for the interrupt. The
// mapping of the driver's own interrupt error is pinned by TestMapTimeout.
func TestSearchDeadlineExpiresMidStatement(t *testing.T) {
	r := commonCorpus(t, 3000)
	calls := textCalls(t, r)
	for _, name := range []string{"rank", "search", "list"} {
		t.Run(name, func(t *testing.T) {
			ec := newExpiringCtx()
			started := 0
			r.SetAfterStart(func() { started++; ec.expire() })
			defer r.SetAfterStart(nil)
			requireTimeout(t, name, calls[name](ec))
			if started == 0 {
				t.Fatal("the seam never fired: the statement was not running when the deadline passed")
			}
		})
	}
}

// TestMapTimeout (R59): an expired deadline maps to ErrSearchTimeout whether the error is the
// context's own or the driver's interrupt error; a cancellation, a live deadline and any other error
// are not touched.
func TestMapTimeout(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	live := context.Background()
	interrupted := errors.New("sqlite: interrupted (9)")
	other := errors.New("disk I/O error")

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		timeout bool
	}{
		{"nil", expired, nil, false},
		{"deadline error", live, context.DeadlineExceeded, true},
		{"interrupt with expired deadline", expired, interrupted, true},
		{"interrupt with live context", live, interrupted, false},
		{"interrupt with cancelled context", canceled, interrupted, false},
		{"other error with expired deadline", expired, other, false},
		{"cancellation", expired, context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := records.MapTimeout(tc.ctx, tc.err)
			if is := errors.Is(got, records.ErrSearchTimeout); is != tc.timeout {
				t.Fatalf("MapTimeout(%v) = %v: ErrSearchTimeout %v, want %v", tc.err, got, is, tc.timeout)
			}
			if tc.timeout && !errors.Is(got, context.DeadlineExceeded) {
				t.Errorf("%v does not wrap DeadlineExceeded", got)
			}
			if !tc.timeout && got != tc.err {
				t.Errorf("error changed: %v -> %v", tc.err, got)
			}
		})
	}
}
