package artparse_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// gateFlush holds the Flush of the real writer until gate is closed.
type gateFlush struct {
	artparse.IngestWriter
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (w *gateFlush) Flush(ctx context.Context) error {
	w.once.Do(func() { close(w.entered) })
	<-w.gate
	return w.IngestWriter.Flush(ctx)
}

// TestAbandonedConclusionSurvivesTheCallersCancel: the caller's context is cancelled while the
// abandoned job's Flush is still in flight, long after the grace period. The records and the run row
// must still be stored, and they must be stored by the time Run returns.
func TestAbandonedConclusionSurvivesTheCallersCancel(t *testing.T) {
	f := newRx(t, "one", "two")
	entered, gate := make(chan struct{}), make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	two := &rxParser{name: "two", n: 3, after: func(context.Context, *parse.Input, parse.Emitter) error { <-hold; return nil }}
	h := f.host(both(shortTimeouts, func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(c *evidence.Case, p records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, p, wo)
			if err != nil {
				return nil, err
			}
			return &gateFlush{IngestWriter: w, entered: entered, gate: gate}, nil
		})
	}), two)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-entered:
		case <-time.After(time.Minute):
		}
		time.Sleep(150 * time.Millisecond) // well past the 30ms grace period: Run would be returning
		cancel()
		time.Sleep(50 * time.Millisecond)
		close(gate)
	}()
	sum, err := h.Run(ctx, artparse.RunOptions{})
	cancel()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := jobOf(t, sum, "two")
	if !j.Abandoned || j.Outcome != artparse.OutcomeIncomplete {
		t.Fatalf("job %+v", j)
	}
	if n := f.nRecords(); n != 3 {
		t.Errorf("%d records stored after Run returned, want the 3 the abandoned job accepted", n)
	}
	if f.count("records.ingest.error") != 1 {
		t.Errorf("%d ingest.error entries: the abandoned job's run row and audit entry must be stored by the time Run returns", f.count("records.ingest.error"))
	}
}

// TestRunReportsAPendingConclusionOfAStuckWriter: a stuck Flush never makes Run hang; Run returns the
// typed error within the bound and the job is incomplete.
func TestRunReportsAPendingConclusionOfAStuckWriter(t *testing.T) {
	defer artparse.SetConcludeJoinTimeout(300 * time.Millisecond)()
	f := newRx(t, "two")
	entered, gate := make(chan struct{}), make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold); close(gate) })
	two := &rxParser{name: "two", n: 2, after: func(context.Context, *parse.Input, parse.Emitter) error { <-hold; return nil }}
	h := f.host(both(shortTimeouts, func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(c *evidence.Case, p records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, p, wo)
			if err != nil {
				return nil, err
			}
			return &gateFlush{IngestWriter: w, entered: entered, gate: gate}, nil
		})
	}), two)
	start := time.Now()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if !errors.Is(err, artparse.ErrConclusionPending) {
		t.Fatalf("Run error %v, want ErrConclusionPending", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v: the join must be bounded", d)
	}
	if j := jobOf(t, sum, "two"); !j.Abandoned || j.Outcome != artparse.OutcomeIncomplete {
		t.Errorf("job %+v, want abandoned and incomplete", j)
	}
}
