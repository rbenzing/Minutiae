package evidence

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// stepCtx is a context whose Err is nil for the first n calls and context.Canceled after that, so a
// cancel can land at an exact point (between two reads of the same loop) without timing.
type stepCtx struct {
	context.Context
	calls atomic.Int64
	n     int64
}

func (s *stepCtx) Err() error {
	if s.calls.Add(1) > s.n {
		return context.Canceled
	}
	return nil
}

func hashCase(t *testing.T) (*Case, []ManifestRecord) {
	t.Helper()
	c, _ := caseWithArtifact(t)
	recs, err := c.Manifest()
	if err != nil || len(recs) == 0 {
		t.Fatalf("manifest: %v %d", err, len(recs))
	}
	return c, recs
}

// D118: the artifact phase itself marks the verify as cut, asserted on the flag so no later phase can
// stand in for it: before an artifact (the context is dead at the loop check) and in the middle of one
// (alive at the loop check, dead at the first read).
func TestHashPhaseMarksTheVerifyAsCut(t *testing.T) {
	c, recs := hashCase(t)
	t.Run("dead before the artifact", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rep := VerifyReport{Problems: []string{}, Notices: []string{}}
		c.hashArtifacts(ctx, &rep, recs, map[string]bool{}, map[string]bool{}, nil)
		if !rep.cancelCut || rep.ArtifactsChecked != 0 || len(rep.Problems) != 0 {
			t.Fatalf("cut=%v checked=%d problems=%q", rep.cancelCut, rep.ArtifactsChecked, rep.Problems)
		}
	})
	t.Run("dead in the middle of the artifact", func(t *testing.T) {
		rep := VerifyReport{Problems: []string{}, Notices: []string{}}
		c.hashArtifacts(&stepCtx{Context: context.Background(), n: 1}, &rep, recs, map[string]bool{}, map[string]bool{}, nil)
		if !rep.cancelCut || rep.ArtifactsChecked != 0 || len(rep.Problems) != 0 {
			t.Fatalf("cut=%v checked=%d problems=%q", rep.cancelCut, rep.ArtifactsChecked, rep.Problems)
		}
	})
	t.Run("alive", func(t *testing.T) {
		rep := VerifyReport{Problems: []string{}, Notices: []string{}}
		c.hashArtifacts(context.Background(), &rep, recs, map[string]bool{}, map[string]bool{}, nil)
		if rep.cancelCut || rep.ArtifactsChecked != len(recs) || len(rep.Problems) != 0 {
			t.Fatalf("cut=%v checked=%d problems=%q", rep.cancelCut, rep.ArtifactsChecked, rep.Problems)
		}
	})
}

// D119: an error that wraps context.Canceled is a cut only when the verify context really ended; from
// any other source it stays a problem.
func TestCancelCausedErrorsNeedADeadVerifyContext(t *testing.T) {
	foreign := fmt.Errorf("some other reader: %w", context.Canceled)
	live := context.Background()
	rep := VerifyReport{Problems: []string{}, Notices: []string{}}
	unreadable(live, &rep, "records", foreign)
	if rep.cancelCut || len(rep.Problems) != 1 {
		t.Fatalf("a foreign Canceled on a live verify: cut=%v problems=%q", rep.cancelCut, rep.Problems)
	}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	rep = VerifyReport{Problems: []string{}, Notices: []string{}}
	unreadable(dead, &rep, "records", foreign)
	if !rep.cancelCut || len(rep.Problems) != 0 {
		t.Fatalf("a Canceled on a dead verify: cut=%v problems=%q", rep.cancelCut, rep.Problems)
	}
	if ctxCaused(live, foreign) || !ctxCaused(dead, foreign) || ctxCaused(dead, errors.New("disk")) {
		t.Fatal("ctxCaused must need both a dead context and a context error")
	}
}
