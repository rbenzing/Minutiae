package parse

import (
	"fmt"
	"math"
	"time"
)

// Limits bounds one parser invocation. The host validates them (Validate)
// before any job runs; the bounds are the production minimums and maximums.
type Limits struct {
	// ProbeBytes is how much Probe may read in total.
	ProbeBytes int64
	// MemInputMax is the largest artifact held in memory (0 = always stream);
	// MemInputTotal is the total held in memory per job.
	MemInputMax, MemInputTotal int64
	// MemBudget is the size of the host-owned allocation budget.
	MemBudget int64
	// MaxRecords is the most records one job may emit.
	MaxRecords int64
	// MaxRejected, MaxNotes and MaxLookupOpens cap rejected records, notes
	// and Lookuper opens per job.
	MaxRejected, MaxNotes, MaxLookupOpens int
	// ProbeTimeout and Timeout bound Probe and Parse; GracePeriod is how long
	// a cancelled parser gets before it is abandoned.
	ProbeTimeout, Timeout, GracePeriod time.Duration
}

const (
	kiB = int64(1) << 10
	miB = int64(1) << 20
	giB = int64(1) << 30
)

// DefaultLimits returns the production limits.
func DefaultLimits() Limits {
	return Limits{
		ProbeBytes:     miB,
		MemInputMax:    256 * miB,
		MemInputTotal:  giB,
		MemBudget:      giB,
		MaxRecords:     10_000_000,
		MaxRejected:    1000,
		MaxNotes:       64,
		MaxLookupOpens: 1000,
		ProbeTimeout:   30 * time.Second,
		Timeout:        time.Hour,
		GracePeriod:    10 * time.Second,
	}
}

func checkRange(field string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fmt.Errorf("parse: Limits.%s = %d is outside %d..%d", field, v, lo, hi)
	}
	return nil
}

func checkDuration(field string, v, lo, hi time.Duration) error {
	if v < lo || v > hi {
		return fmt.Errorf("parse: Limits.%s = %v is outside %v..%v", field, v, lo, hi)
	}
	return nil
}

// Validate refuses limits outside the production bounds; the error names the
// field.
func (l Limits) Validate() error {
	for _, err := range []error{
		checkRange("ProbeBytes", l.ProbeBytes, 4*kiB, 64*miB),
		checkDuration("ProbeTimeout", l.ProbeTimeout, 100*time.Millisecond, 10*time.Minute),
		checkRange("MemInputMax", l.MemInputMax, 0, 4*giB),
		checkRange("MemInputTotal", l.MemInputTotal, max(l.MemInputMax, 0), math.MaxInt64),
		checkRange("MemBudget", l.MemBudget, 16*miB, 64*giB),
		checkRange("MaxRecords", l.MaxRecords, 1, 1_000_000_000),
		checkRange("MaxRejected", int64(l.MaxRejected), 1, 1_000_000),
		checkDuration("Timeout", l.Timeout, time.Second, 24*time.Hour),
		checkDuration("GracePeriod", l.GracePeriod, 10*time.Millisecond, 10*time.Minute),
		checkRange("MaxNotes", int64(l.MaxNotes), 1, 1024),
		checkRange("MaxLookupOpens", int64(l.MaxLookupOpens), 1, 100_000),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}
