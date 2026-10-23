package evidence

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// withSelfTestRun swaps the fresh self-test run for fn and empties the memo, restoring both afterwards.
func withSelfTestRun(t *testing.T, fn func(context.Context) (ftsReference, error)) {
	t.Helper()
	old := ftsSelfTestRun
	ftsSelfTestMemo.reset()
	t.Cleanup(func() { ftsSelfTestRun = old; ftsSelfTestMemo.reset() })
	ftsSelfTestRun = fn
}

// TestFTSSelfTestMemoRunsOncePerProcess: the SQLite build and the replica tables cannot change within a
// process, so the self-test result is computed once and reused.
func TestFTSSelfTestMemoRunsOncePerProcess(t *testing.T) {
	calls := 0
	withSelfTestRun(t, func(context.Context) (ftsReference, error) {
		calls++
		return ftsReference{config: map[string][]keyedRow{}}, nil
	})
	for range 3 {
		if _, err := ftsSelfTestCached(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("the self-test ran %d times for 3 verifies, want 1", calls)
	}
}

// TestFTSSelfTestMemoKeepsAFailure: a memoized failure is reported on every verify (fail closed), with its
// reference rows.
func TestFTSSelfTestMemoKeepsAFailure(t *testing.T) {
	calls := 0
	withSelfTestRun(t, func(context.Context) (ftsReference, error) {
		calls++
		return ftsReference{config: map[string][]keyedRow{"x": nil}}, fmt.Errorf("%w: test", errFTSReplica)
	})
	for i := range 3 {
		ref, err := ftsSelfTestCached(context.Background())
		if !errors.Is(err, errFTSReplica) {
			t.Fatalf("verify %d: err = %v, want errFTSReplica", i, err)
		}
		if ref.config == nil {
			t.Fatalf("verify %d: the reference rows were lost", i)
		}
	}
	if calls != 1 {
		t.Fatalf("the failing self-test ran %d times, want 1", calls)
	}
}

// TestVerifyMemoizedSelfTestFailureIsAProblemOnEveryVerify: the same case verified twice reports the PROBLEM
// both times.
func TestVerifyMemoizedSelfTestFailureIsAProblemOnEveryVerify(t *testing.T) {
	withSelfTestRun(t, func(context.Context) (ftsReference, error) {
		return ftsReference{}, fmt.Errorf("%w: test", errFTSReplica)
	})
	c := newTestCase(t)
	for i := range 2 {
		rep := mustVerify(t, c)
		if rep.OK() || countContaining(rep.Problems, "tokenizer replica does not match this SQLite") != 1 {
			t.Fatalf("verify %d: %q", i, rep.Problems)
		}
	}
}

// TestFTSSelfTestMemoNeverKeepsACancel: a cancelled context ends one verify, it says nothing about the SQLite.
func TestFTSSelfTestMemoNeverKeepsACancel(t *testing.T) {
	calls := 0
	withSelfTestRun(t, func(ctx context.Context) (ftsReference, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return ftsReference{}, err
		}
		return ftsReference{config: map[string][]keyedRow{}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ftsSelfTestCached(ctx); err == nil {
		t.Fatal("a cancelled context passed")
	}
	if _, err := ftsSelfTestCached(context.Background()); err != nil {
		t.Fatalf("a cancel was memoized: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestFTSSelfTestMemoIsBypassedByAReplicaOverride: the test seam changes the replica, so a result measured
// without it must not answer for it (and the reverse).
func TestFTSSelfTestMemoIsBypassedByAReplicaOverride(t *testing.T) {
	calls := 0
	withSelfTestRun(t, func(context.Context) (ftsReference, error) {
		calls++
		return ftsReference{config: map[string][]keyedRow{}}, nil
	})
	if _, err := ftsSelfTestCached(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := ftsReplicaOverride
	t.Cleanup(func() { ftsReplicaOverride = old })
	ftsReplicaOverride = func(string) func(string, func([]byte, int)) { return nil }
	for range 2 {
		if _, err := ftsSelfTestCached(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 memoized + 2 with the override)", calls)
	}
}
