package evidence

import (
	"context"
	"testing"
	"time"
)

func TestVerifyProgressUpdatesOncePerChunk(t *testing.T) {
	c := indexedTextCase(t, 12000)
	p := NewVerifyProgress()
	var before, after uint64
	c.verifyPhaseHook = func(name string, _ time.Duration) {
		switch name {
		case "records.integrity_check":
			before = p.updates.Load()
		case "records.stream":
			after = p.updates.Load()
		}
	}
	// the case is not audited, so the report holds problems; only the counter is under test
	if _, err := c.VerifyObserved(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if after-before != 3 {
		t.Fatalf("records.stream updated the counter %d times, want 3 (chunks of 5000, 5000 and 2000)", after-before)
	}
}
