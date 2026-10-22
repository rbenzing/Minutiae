package evidence

import (
	"context"
	"math"
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

func TestProgressTotalsBecomeUnknownOnNegativeOrOverflow(t *testing.T) {
	if got := artifactBytesTotal([]ManifestRecord{{Size: 5}, {Size: 7}}); got != 12 {
		t.Errorf("plain sum = %d, want 12", got)
	}
	if got := artifactBytesTotal([]ManifestRecord{{Size: 5}, {Size: -1}, {Size: 7}}); got != -1 {
		t.Errorf("negative term = %d, want -1", got)
	}
	if got := artifactBytesTotal([]ManifestRecord{{Size: math.MaxInt64}, {Size: 1}}); got != -1 {
		t.Errorf("overflow = %d, want -1", got)
	}
	k1, k2 := batchKey{"i", 1}, batchKey{"i", 2}
	stored := map[batchKey]storedBatch{k1: {}, k2: {}}
	audit := &recordAudit{batchErr: map[batchKey]bool{}, recovered: map[batchKey]bool{}}
	mk := func(a, b int) []auditedBatch {
		return []auditedBatch{{BatchCommit: BatchCommit{IngestID: "i", BatchNo: 1, Count: a}}, {BatchCommit: BatchCommit{IngestID: "i", BatchNo: 2, Count: b}}}
	}
	if got := auditedRecordsTotal(mk(3, 4), stored, audit); got != 7 {
		t.Errorf("plain records = %d, want 7", got)
	}
	if got := auditedRecordsTotal(mk(3, -4), stored, audit); got != -1 {
		t.Errorf("negative count = %d, want -1", got)
	}
	if got := auditedRecordsTotal(mk(math.MaxInt, 1), stored, audit); got != -1 {
		t.Errorf("overflow = %d, want -1", got)
	}
}
