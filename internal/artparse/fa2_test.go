package artparse_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// purityProbe reports what a parser can see of the host: a deadline, a Seal method on its readers.
type purityProbe struct {
	parsertest.WellBehaved
	seen chan string
}

func (p purityProbe) look(ctx context.Context, where string, in *parse.Input) {
	_, hasDeadline := ctx.Deadline()
	_, canSeal := in.Primary.R.(interface{ Seal() })
	p.seen <- fmt.Sprintf("%s deadline=%v seal=%v", where, hasDeadline, canSeal)
}

func (p purityProbe) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	p.look(ctx, "probe", in)
	return p.WellBehaved.Probe(ctx, in)
}

func (p purityProbe) Parse(ctx context.Context, in *parse.Input, em parse.Emitter) error {
	p.look(ctx, "parse", in)
	return p.WellBehaved.Parse(ctx, in, em)
}

// The host owns every deadline and every seal: the context a parser gets reports none and its
// readers have no Seal method to reach by type assertion.
func TestParserSeesNoDeadlineAndNoSeal(t *testing.T) {
	f := newRx(t, "pure")
	p := purityProbe{WellBehaved: wbp("pure"), seen: make(chan string, 8)}
	sum := f.run(f.host(nil, p), artparse.Selection{})
	if j := jobOf(t, sum, "pure"); j.Outcome != artparse.OutcomeComplete {
		t.Fatalf("job %+v", j)
	}
	close(p.seen)
	var got []string
	for s := range p.seen {
		got = append(got, s)
	}
	want := []string{"probe deadline=false seal=false", "parse deadline=false seal=false"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parser saw %q, want %q", got, want)
	}
}
