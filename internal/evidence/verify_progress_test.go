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
	if s := got["fts.rebuild"]; !s.Counted || s.Unit != "records" || s.Done != 300 || s.Total != 300 {
		t.Errorf("fts.rebuild at its end = %+v, want records 300 of 300", s)
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
