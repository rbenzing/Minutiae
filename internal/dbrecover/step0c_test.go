package dbrecover

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestInputRewrittenInPlaceBeforeEndAbortsWithIntegrityError is the variant of the
// replaced-input test that runs everywhere (J64): the input keeps its path and
// its size and gets other bytes, between the last flush and the fresh hash.
func TestInputRewrittenInPlaceBeforeEndAbortsWithIntegrityError(t *testing.T) {
	e := newRunEnv(t)
	path := diskPath(e.c, e.wal)
	repl := slices.Clone(e.f.wal)
	repl[len(repl)-1] ^= 0xFF
	if len(repl) != len(e.f.wal) || slices.Equal(repl, e.f.wal) {
		t.Fatal("the replacement must have the same size and other bytes")
	}
	rewrote := false
	defer SetAfterFlushHookForTest(func() {
		if err := os.WriteFile(path, repl, 0o600); err != nil {
			t.Error(err)
			return
		}
		rewrote = true
	})()
	_, err := Run(context.Background(), e.c, e.in(), Options{})
	if !rewrote {
		t.Fatal("the hook did not rewrite the input")
	}
	var ch *InputChangedError
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.As(err, &ch) || ch.ArtifactID != e.wal.ID {
		t.Fatalf("err %v, want an InputChangedError naming the WAL %s", err, e.wal.ID)
	}
	if n := count(readAudit(t, e.c), evidence.ActionIngestEnd); n != 0 {
		t.Errorf("%d records.ingest.end after a rewritten input", n)
	}
}

// failRecorder is a testing.TB whose Fatalf records the failure and stops the
// goroutine, so a test can ask whether a checker fails.
type failRecorder struct {
	testing.TB
	failed bool
}

func (f *failRecorder) Helper() {}
func (f *failRecorder) Fatalf(string, ...any) {
	f.failed = true
	panic(f)
}

func hostileFails(t *testing.T, c *evidence.Case, err error, expectInternal bool) (failed bool) {
	t.Helper()
	f := &failRecorder{TB: t}
	defer func() {
		if r := recover(); r != nil && r != f {
			panic(r)
		}
		failed = f.failed
	}()
	hostileOKExpecting(f, c, err, expectInternal)
	return false
}

// TestHostileOKFailsOnAnyInternalError pins J71: an ErrInternal that is not a
// PanicError is a bug on hostile input unless the case expects it.
func TestHostileOKFailsOnAnyInternalError(t *testing.T) {
	e := newRunEnv(t)
	internal := errors.Join(errors.New("x"), ErrInternal)
	if !hostileFails(t, e.c, internal, false) {
		t.Error("an ErrInternal passed hostileOK")
	}
	if hostileFails(t, e.c, internal, true) {
		t.Error("an expected ErrInternal failed hostileOK")
	}
	if hostileFails(t, e.c, nil, false) {
		t.Error("a nil error failed hostileOK")
	}
}

// TestPanicBeforeAnalysisStartIsAuditedWithTheDevice pins J70: a panic in resolve
// after the database is known writes analysis.error with its device and parent.
func TestPanicBeforeAnalysisStartIsAuditedWithTheDevice(t *testing.T) {
	e := newRunEnv(t)
	defer SetResolveHookForTest(func() { panic("resolve exploded") })()
	_, err := Run(context.Background(), e.c, e.in(), Options{})
	var pe *PanicError
	if !errors.Is(err, ErrInternal) || !errors.As(err, &pe) {
		t.Fatalf("err %v, want a PanicError", err)
	}
	es := readAudit(t, e.c)
	if count(es, "analysis.start") != 0 || count(es, "analysis.error") != 1 {
		t.Fatalf("analysis.start %d analysis.error %d", count(es, "analysis.start"), count(es, "analysis.error"))
	}
	ent := lastEntry(t, es, "analysis.error")
	if got, _ := ent.Details["parent_id"].(string); got != e.db.ID {
		t.Errorf("parent_id %q, want %q", got, e.db.ID)
	}
	if ent.DeviceID != e.db.Source.DeviceID || ent.DeviceID == "" {
		t.Errorf("device %q, want %q", ent.DeviceID, e.db.Source.DeviceID)
	}
	if !strings.Contains(ent.Details["error"].(string), "resolve exploded") {
		t.Errorf("error %v", ent.Details["error"])
	}
}
