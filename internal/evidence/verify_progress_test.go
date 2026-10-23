package evidence_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// makeAuditUnreadable makes the verify read of the audit log fail with an I/O error (not a bad line: a bad line
// still lets the rest of the log be read, and a case with one does not open): the log is closed and replaced
// by a directory of the same name. The verify then cannot audit itself, so it returns that error, after
// the phase hooks have run.
func makeAuditUnreadable(t *testing.T, c *evidence.Case) {
	t.Helper()
	p := filepath.Join(c.Dir, "audit.jsonl")
	if err := c.Audit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyObservedReportEqualsVerifyWith(t *testing.T) {
	c := ftsCase(t, 300)
	ctx := context.Background()
	compare := func(label string) {
		t.Helper()
		a, err := c.VerifyObserved(ctx, evidence.NewVerifyProgress())
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.VerifyWith(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if b.AuditEntries != a.AuditEntries+1 { // the first verify audited itself
			t.Fatalf("%s: audit entries %d then %d", label, a.AuditEntries, b.AuditEntries)
		}
		b.AuditEntries = a.AuditEntries
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: observed report differs:\n%+v\n%+v", label, a, b)
		}
	}
	compare("clean")
	recordstest.SetRecordSummary(t, c.Dir, 7, "altered")
	compare("altered")
}

// snapshotsAtPhaseEnds verifies c with an observer and returns the Snapshot taken inside the phase hook
// at the end of each phase.
func snapshotsAtPhaseEnds(t *testing.T, c *evidence.Case) map[string]evidence.VerifyProgressSnapshot {
	t.Helper()
	p := evidence.NewVerifyProgress()
	got := map[string]evidence.VerifyProgressSnapshot{}
	c.SetPhaseHook(func(name string, _ time.Duration) { got[name] = p.Snapshot() })
	if _, err := c.VerifyObserved(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestVerifyProgressCountersAtPhaseEnds(t *testing.T) {
	c := ftsCase(t, 300)
	recordstest.AddArtifact(t, c, "big.bin", bytes.Repeat([]byte{7}, 3*32768+1))
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var bytesTotal int64
	for _, r := range recs {
		bytesTotal += r.Size
	}
	if bytesTotal < 98305 {
		t.Fatalf("manifest holds %d bytes", bytesTotal)
	}
	got := snapshotsAtPhaseEnds(t, c)
	if s := got["artifacts"]; !s.Counted || s.Unit != "bytes" || s.Done != bytesTotal || s.Total != bytesTotal {
		t.Errorf("artifacts at its end = %+v, want bytes %d of %d", s, bytesTotal, bytesTotal)
	}
	if s := got["records.stream"]; !s.Counted || s.Unit != "records" || s.Done != 300 || s.Total != 300 {
		t.Errorf("records.stream at its end = %+v, want records 300 of 300", s)
	}
	if s, ok := got["fts.rebuild"]; ok {
		t.Errorf("fts.rebuild ended on a clean case (5D-14k removed it): %+v", s)
	}
	if s, ok := got["records.integrity_check"]; !ok || s.Counted {
		t.Errorf("records.integrity_check at its end = %+v (seen %v), want an uncounted phase", s, ok)
	}
	for name, s := range got {
		if s.Phase != name {
			t.Errorf("snapshot at the end of %q names phase %q", name, s.Phase)
		}
	}
}

func TestVerifyProgressTotalUnknownWithoutAudit(t *testing.T) {
	c := ftsCase(t, 300)
	makeAuditUnreadable(t, c)
	p := evidence.NewVerifyProgress()
	got := map[string]evidence.VerifyProgressSnapshot{}
	c.SetPhaseHook(func(name string, _ time.Duration) { got[name] = p.Snapshot() })
	_, _ = c.VerifyObserved(context.Background(), p) // its final audit entry cannot be written: the error is expected
	if s := got["records.stream"]; !s.Counted || s.Total != -1 || s.Done != 300 {
		t.Errorf("records.stream with an unreadable audit log = %+v, want total -1 and 300 done", s)
	}
}

func TestVerifyProgressHasOnlyReadMethods(t *testing.T) {
	ty := reflect.TypeOf(evidence.NewVerifyProgress())
	if ty.NumMethod() != 1 || ty.Method(0).Name != "Snapshot" {
		var names []string
		for i := range ty.NumMethod() {
			names = append(names, ty.Method(i).Name)
		}
		t.Fatalf("exported methods of *VerifyProgress = %v, want only Snapshot", names)
	}
}

func TestVerifyProgressSnapshotIsRaceFree(t *testing.T) {
	c := ftsCase(t, 300)
	p := evidence.NewVerifyProgress()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		var last uint64
		for {
			s := p.Snapshot()
			if s.Seq < last {
				t.Errorf("Seq went back from %d to %d", last, s.Seq)
			}
			last = s.Seq
			select {
			case <-stop:
				return
			default:
			}
		}
	})
	rep, err := c.VerifyObserved(context.Background(), p)
	close(stop)
	wg.Wait()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %q", err, rep.Problems)
	}
	if s := p.Snapshot(); s.Phase != "" {
		t.Fatalf("after the verify the observer still shows phase %q", s.Phase)
	}
}

// streamSnapshotAfterAudit appends one audit entry about the last batch of a 6000-record case (batches of
// 5000 and 1000), verifies with an observer and returns the snapshot at the end of records.stream.
func streamSnapshotAfterAudit(t *testing.T, action string, details func(ingest string, batchNo int) map[string]any) evidence.VerifyProgressSnapshot {
	t.Helper()
	c := ftsCase(t, 6000)
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var last evidence.BatchCommit
	batches := 0
	for _, e := range entries {
		if e.Action != evidence.ActionBatch {
			continue
		}
		b, err := evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		if err != nil {
			t.Fatal(err)
		}
		last, batches = b, batches+1
	}
	if batches != 2 || last.Count != 1000 {
		t.Fatalf("%d batches, last of %d records; want 2 and 1000", batches, last.Count)
	}
	if _, err := c.Audit.Append(action, "", details(last.IngestID, last.BatchNo)); err != nil {
		t.Fatal(err)
	}
	return snapshotsAtPhaseEnds(t, c)["records.stream"]
}

func TestVerifyProgressTotalSkipsAnErroredBatch(t *testing.T) {
	s := streamSnapshotAfterAudit(t, evidence.ActionBatchError, func(ingest string, no int) map[string]any {
		return map[string]any{"ingest_id": ingest, "batch_no": no, "error": "injected"}
	})
	if s.Total != 5000 {
		t.Errorf("records.stream total = %d, want 5000 (the errored batch of 1000 is not counted)", s.Total)
	}
}

func TestVerifyProgressTotalSkipsARecoveredBatch(t *testing.T) {
	s := streamSnapshotAfterAudit(t, evidence.ActionIngestRecover, func(ingest string, no int) map[string]any {
		return map[string]any{"ingest_id": ingest, "batch_nos": []int{no}}
	})
	if s.Total != 5000 {
		t.Errorf("records.stream total = %d, want 5000 (the recovered batch of 1000 is not counted)", s.Total)
	}
}

func TestVerifyProgressArtifactBytesEndAtTotalWithDuplicateManifestID(t *testing.T) {
	c := ftsCase(t, 300)
	path := filepath.Join(c.Dir, "manifest.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	dup := append(append([]byte{}, b...), append(lines[len(lines)-1], '\n')...)
	if err := os.WriteFile(path, dup, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Fatal(err)
	}
	s := snapshotsAtPhaseEnds(t, c)["artifacts"]
	if s.Total <= 0 || s.Done != s.Total {
		t.Errorf("artifacts at its end = %+v, want Done == Total > 0 (a duplicate id is hashed like any other record)", s)
	}
}
