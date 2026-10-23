package evidence

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestVerifyIntegrityCheckRunsBesideTheStream (5D-15c): P10 holds the verify open until the record stream has read its
// first chunk, which only happens when the two run at once. The status snapshot taken there names the stream as the
// phase and P10 as running beside it.
func TestVerifyIntegrityCheckRunsBesideTheStream(t *testing.T) {
	c := indexedTextCase(t, 12000) // three chunks of the stream
	started := make(chan struct{})
	var once sync.Once
	timedOut := false
	c.verifyIntegrityHook = func() {
		select {
		case <-started:
		case <-time.After(20 * time.Second):
			timedOut = true // the serial order: the stream cannot start before P10 is over
		}
	}
	p := NewVerifyProgress()
	var snap VerifyProgressSnapshot
	if _, err := c.verifyRun(context.Background(), func(table string, _ int) {
		if table == "records" {
			once.Do(func() {
				snap = p.Snapshot()
				close(started)
			})
		}
	}, p, nil); err != nil {
		t.Fatal(err)
	}
	if timedOut {
		t.Fatal("P10 and the record stream did not run at the same time")
	}
	if snap.Phase != "records.stream" || !slices.Equal(snap.Also, []string{"records.integrity_check"}) {
		t.Fatalf("snapshot beside the stream = %+v, want phase records.stream and also records.integrity_check", snap)
	}
}

// TestVerifyPhaseHookCallsNeverOverlap: concurrent phases end on different goroutines, but the hook (and the
// snapshot it takes) is never entered twice at once, and a snapshot taken inside the hook names the phase that ended.
func TestVerifyPhaseHookCallsNeverOverlap(t *testing.T) {
	c := indexedTextCase(t, 300)
	p := NewVerifyProgress()
	var inFlight, maxInFlight atomic.Int32
	var mismatched []string
	c.verifyPhaseHook = func(name string, _ time.Duration) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		if s := p.Snapshot(); s.Phase != name && name != VerifyOverlapPhase { // the overlap span is a hook event only
			mismatched = append(mismatched, name+" -> "+s.Phase)
		}
		time.Sleep(2 * time.Millisecond) // long enough for the other goroutine to reach its own hook
	}
	if _, err := c.VerifyObserved(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("%d phase hook calls at once, want 1", got)
	}
	if len(mismatched) != 0 {
		t.Fatalf("snapshots inside the hook that name another phase: %q", mismatched)
	}
}

// TestVerifyTablesAreDecodedTogether (5D-15c): the hook of the first decode phase to end (the second has nothing left to see) waits until the other
// table's decode phase is on the stack too; that happens only when the two tables are decoded at the same time (the
// frame of the table that ends is kept until its hook returns, so the order of the two does not matter).
func TestVerifyTablesAreDecodedTogether(t *testing.T) {
	c := indexedTextCase(t, 300)
	p := NewVerifyProgress()
	const word, sub = "fts.index/records_fts/decode", "fts.index/records_fts_sub/decode"
	missed := map[string]bool{}
	checked := false // the hooks are serialised
	c.verifyPhaseHook = func(name string, _ time.Duration) {
		other := map[string]string{word: sub, sub: word}[name]
		if other == "" || checked {
			return // only the first to end can see the other still on the stack
		}
		checked = true
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if slices.Contains(p.Snapshot().Also, other) {
				return
			}
		}
		missed[name] = true // the serial order: the other table starts only after this phase is over
	}
	if _, err := c.VerifyObserved(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(missed) != 0 {
		t.Fatalf("a decode phase ended without the other table's decode running beside it: %v", missed)
	}
}
