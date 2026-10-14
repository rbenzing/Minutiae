package artparse

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func readyConclusion(abortErr error) <-chan concludedIngest {
	ch := make(chan concludedIngest, 1)
	ch <- concludedIngest{res: records.IngestResult{}, abortErr: abortErr}
	return ch
}

// A conclusion that has finished is never reported pending, even when the join deadline has passed.
func TestJoinNeverReportsAFinishedConclusionPending(t *testing.T) {
	old := concludeJoinTimeout
	concludeJoinTimeout = 0 // the deadline has passed before the first receive
	defer func() { concludeJoinTimeout = old }()
	for i := 0; i < 300; i++ {
		r := &run{pending: []<-chan concludedIngest{readyConclusion(nil), readyConclusion(nil)}}
		if err := r.joinConclusions(); err != nil {
			t.Fatalf("iteration %d: %v: a finished conclusion was reported pending", i, err)
		}
	}
}

// A join-time abort failure keeps the original stop reason and is returned next to a pending conclusion.
func TestJoinAbortFailureKeepsTheStopReason(t *testing.T) {
	abortErr := errors.New("abort broke")
	r := &run{pending: []<-chan concludedIngest{readyConclusion(abortErr)}}
	r.sum.Stopped = StoppedAbandoned
	err := r.joinConclusions()
	if !errors.Is(err, abortErr) || r.sum.Stopped != StoppedAbandoned {
		t.Fatalf("err %v stopped %q: want the abort failure and the original stop reason", err, r.sum.Stopped)
	}

	old := concludeJoinTimeout
	concludeJoinTimeout = 0
	defer func() { concludeJoinTimeout = old }()
	r = &run{pending: []<-chan concludedIngest{readyConclusion(abortErr), make(chan concludedIngest)}}
	r.sum.Stopped = StoppedCancelled
	err = r.joinConclusions()
	if !errors.Is(err, abortErr) || !errors.Is(err, ErrConclusionPending) || r.sum.Stopped != StoppedCancelled {
		t.Fatalf("err %v stopped %q: want both errors and the original stop reason", err, r.sum.Stopped)
	}
}
