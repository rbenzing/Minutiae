//go:build scale

package records

import (
	"context"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// ScaleProbeCost is the cost of each bounded probe a search page can run, measured one after the other
// in one read transaction whatever the plan would have chosen, so that the cost of the large text probe
// and of the filter probe is seen apart from the page that follows them. Cold is the first call alone
// (the clock of the platform may tick in steps of several ms, so a single call reads as a multiple of
// that step); Mean is the average of Reps further calls, which averages the tick out.
type ScaleProbeCost struct {
	Reps                         int
	TextSmallCold, TextSmallMean time.Duration
	TextLargeCold, TextLargeMean time.Duration
	FilterCold, FilterMean       time.Duration
	HitsSmall, HitsLarge         int
	Tiny, FilterProbed           bool
}

// ScaleProbeCost runs the probes for f (its text query is required), reps times each after the first.
func (r *Reader) ScaleProbeCost(ctx context.Context, f Filter, reps int) (ScaleProbeCost, error) {
	out := ScaleProbeCost{Reps: reps}
	timed := func(call func() error) (cold, mean time.Duration, err error) {
		t0 := time.Now()
		if err = call(); err != nil {
			return 0, 0, err
		}
		cold = time.Since(t0)
		t0 = time.Now()
		for i := 0; i < reps; i++ {
			if err = call(); err != nil {
				return 0, 0, err
			}
		}
		return cold, time.Since(t0) / time.Duration(reps), nil
	}
	err := r.readTx(ctx, true, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		if out.TextSmallCold, out.TextSmallMean, err = timed(func() (e error) {
			out.HitsSmall, e = r.probeText(ctx, h, f.Text, searchProbeLimit)
			return e
		}); err != nil {
			return err
		}
		if out.TextLargeCold, out.TextLargeMean, err = timed(func() (e error) {
			out.HitsLarge, e = r.probeText(ctx, h, f.Text, searchINLimit+1)
			return e
		}); err != nil {
			return err
		}
		if indexedFilter(f) {
			out.FilterProbed = true
			if out.FilterCold, out.FilterMean, err = timed(func() (e error) {
				out.Tiny, e = r.probeFilter(ctx, h, f, have)
				return e
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
