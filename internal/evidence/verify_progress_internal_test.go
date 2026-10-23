package evidence

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"
)

func TestVerifyProgressUpdatesOncePerChunk(t *testing.T) {
	c := indexedTextCase(t, 12000)
	c.verifySerial = true // P10 would otherwise end at any moment of the stream: the count between the two ends is that of the serial order
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

// Concurrent phases are siblings on the stack: the snapshot names the one that started last (or the one whose hook is
// running) and lists the others, except the phases that contain it.
func TestSnapshotNamesConcurrentPhases(t *testing.T) {
	p := NewVerifyProgress()
	p10 := p.begin("records.integrity_check", false, 0, "")
	parent := p.begin("fts.index", false, 0, "")
	t0 := p.begin("fts.index/records_fts/decode", true, 10, "pages")
	t1 := p.begin("fts.index/records_fts_sub/decode", true, 10, "pages")
	s := p.Snapshot()
	if s.Phase != "fts.index/records_fts_sub/decode" || !slices.Equal(s.Also, []string{"records.integrity_check", "fts.index/records_fts/decode"}) {
		t.Fatalf("snapshot = %+v, want the last started phase and the other two, not the parent fts.index", s)
	}
	p.setFocus(t0)
	s = p.Snapshot()
	if s.Phase != "fts.index/records_fts/decode" || !slices.Equal(s.Also, []string{"records.integrity_check", "fts.index/records_fts_sub/decode"}) {
		t.Fatalf("snapshot with the focus on table 0 = %+v", s)
	}
	p.setFocus(nil)
	p.end(p10) // not the last started: the others stay
	p.end(t1)
	s = p.Snapshot()
	if s.Phase != "fts.index/records_fts/decode" || len(s.Also) != 0 {
		t.Fatalf("snapshot after two ends = %+v, want table 0 alone (its parent contains it)", s)
	}
	p.end(t0)
	p.end(parent)
	if s := p.Snapshot(); s.Phase != "" || s.Also != nil {
		t.Fatalf("snapshot with nothing running = %+v", s)
	}
}
