package evidence

import (
	"context"
	"sync"
)

// ftsSelfTestRun is the fresh self-test the memo runs; a test replaces it to count runs or to force a result.
var ftsSelfTestRun = ftsSelfTest

// ftsSelfTestMemoState keeps the one result of the self-test of this process.
type ftsSelfTestMemoState struct {
	mu   sync.Mutex
	done bool
	ref  ftsReference
	err  error
}

var ftsSelfTestMemo ftsSelfTestMemoState

// reset forgets the result (tests only).
func (m *ftsSelfTestMemoState) reset() {
	m.mu.Lock()
	m.done, m.ref, m.err = false, ftsReference{}, nil
	m.mu.Unlock()
}

// ftsSelfTestCached is ftsSelfTest computed once per process, error included: the SQLite build and the
// replica tables cannot change within a process, so a failure stays a failure on every verify (the check
// fails closed each time). The memo is not used, and not filled, when
//   - the replica test seam is set (it changes the replicas the result depends on), or
//   - the run ended because ctx ended: a cancel says nothing about the SQLite, so it is never kept.
//
// The returned reference is shared and must be read only (compareConfig only reads it).
func ftsSelfTestCached(ctx context.Context) (ftsReference, error) {
	if ftsReplicaOverride != nil {
		return ftsSelfTestRun(ctx)
	}
	m := &ftsSelfTestMemo
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.ref, m.err
	}
	ref, err := ftsSelfTestRun(ctx)
	if err != nil && ctxCaused(ctx, err) {
		return ref, err
	}
	m.done, m.ref, m.err = true, ref, err
	return ref, err
}
