package dbrecover

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func flipByteAt(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt([]byte{0xAA}, off); err != nil {
		t.Error(err)
	}
}

func TestChangedUnusedCompanionNeverReachesIngestEnd(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	db := addFile(t, c, "/data/x.db", f.db)
	w := addFile(t, c, "/data/x.db-wal", bytes.Repeat([]byte{0x5a}, 32+1024+24))
	defer SetAfterFlushHookForTest(func() { flipByteAt(t, diskPath(c, w), 40) })()
	res, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
	var ch *InputChangedError
	if !errors.As(err, &ch) || !errors.Is(err, evidence.ErrIntegrity) || ch.ArtifactID != w.ID {
		t.Fatalf("err %v", err)
	}
	es := readAudit(t, c)
	if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) != 1 {
		t.Errorf("ingest.end %d ingest.error %d", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError))
	}
	if res.Ingest.Outcome != "incomplete" {
		t.Errorf("outcome %q", res.Ingest.Outcome)
	}
}

func TestHookTimeChangeOfTheDatabaseGivesNoIngestEnd(t *testing.T) {
	e := newRunEnv(t)
	defer SetAfterFlushHookForTest(func() { flipByteAt(t, diskPath(e.c, e.db), 3000) })()
	_, err := Run(context.Background(), e.c, e.in(), Options{})
	if !errors.Is(err, ErrInputChanged) {
		t.Fatalf("err %v", err)
	}
	if n := count(readAudit(t, e.c), evidence.ActionIngestEnd); n != 0 {
		t.Errorf("%d ingest.end after a hook-time change", n)
	}
}

func TestUnusedCompanionDoesNotLowerCapOrMarkMixed(t *testing.T) {
	cap40 := 40
	mk := func(role string, rec, inc bool, cp *int, unused string) *sealedInput {
		return &sealedInput{Ref: inputRef{Role: role, ArtifactID: role, Recovered: rec, Incomplete: inc, Cap: cp, Unused: unused}}
	}
	s := &sealedSet{DB: *mk("db", false, false, nil, ""), WAL: mk("wal", true, true, &cap40, "header invalid")}
	if s.cap() != nil || s.mixed() || s.incomplete() {
		t.Errorf("an unused companion counted: cap %v mixed %v incomplete %v", s.cap(), s.mixed(), s.incomplete())
	}
	s.WAL.Ref.Unused = ""
	if s.cap() == nil || *s.cap() != 40 || !s.mixed() || !s.incomplete() {
		t.Errorf("a used companion did not count: cap %v mixed %v incomplete %v", s.cap(), s.mixed(), s.incomplete())
	}
}
