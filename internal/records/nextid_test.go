package records_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// records_meta.next_id is an unaudited, mutable value. The writer must never
// trust it upward: raising it used to make the writer audit (fsynced, forever) a
// batch range that overflows, after which `case verify` failed permanently and
// every later Start was refused (final review I1).

const poisonedNextID = 9223372036854775800

func TestRaisedNextIDIsRefusedBeforeAnythingIsAudited(t *testing.T) {
	c, a := setup(t)
	recordstest.SetNextID(t, c, poisonedNextID)
	before := len(auditOf(t, c, ""))
	w := newWriter(t, c, testParser, records.WriterOptions{})
	err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
	if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "next_id") {
		t.Fatalf("Start with a raised next_id: %v", err)
	}
	if after := len(auditOf(t, c, "")); after != before {
		t.Fatalf("the refused Start audited %d entries", after-before)
	}
	// nothing was poisoned: with the counter put back the case works and verifies
	recordstest.SetNextID(t, c, 1)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 20, 1))
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("verify after the refusal: %q", rep.Problems)
	}
}

func TestStaleNextIDIsRefused(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 5, 1))
	recordstest.SetNextID(t, c, 3) // below max(id)+1
	before := len(auditOf(t, c, ""))
	w := newWriter(t, c, records.Parser{Name: "other", Version: "1"}, records.WriterOptions{})
	err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
	if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "max(id)+1") {
		t.Fatalf("Start with a stale next_id: %v", err)
	}
	if after := len(auditOf(t, c, "")); after != before {
		t.Fatalf("the refused Start audited %d entries", after-before)
	}
}

// TestRaisedNextIDMidIngestWritesNothingToTheAudit: the counter is raised while an
// ingest is running; the next batch is refused before its records.batch entry.
func TestRaisedNextIDMidIngestWritesNothingToTheAudit(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 10}, a.ID)
	add(t, w, recordstest.Records(a.ID, 10, 1)) // batch 1 is written
	recordstest.SetNextID(t, c, poisonedNextID)
	batches := len(auditOf(t, c, evidence.ActionBatch))
	var addErr error
	for _, r := range recordstest.Records(a.ID, 10, 2) {
		if addErr = w.Add(ctx, r); addErr != nil {
			break
		}
	}
	if !errors.Is(addErr, evidence.ErrIntegrity) || !strings.Contains(addErr.Error(), "next_id") {
		t.Fatalf("the batch after the counter was raised: %v", addErr)
	}
	if got := len(auditOf(t, c, evidence.ActionBatch)); got != batches {
		t.Fatalf("a records.batch entry was audited for the refused batch (%d -> %d)", batches, got)
	}
	if _, err := w.Abort(ctx, addErr); err != nil {
		t.Fatal(err)
	}
	recordstest.SetNextID(t, c, 11)
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("verify after the refused batch: %q", rep.Problems)
	}
}

func TestVerifyFlagsRaisedNextID(t *testing.T) {
	g := ingest30(t)
	recordstest.SetNextID(t, g.c, 100)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"records_meta next_id is 100 but the highest record id is 30"})
	if got := strconv.Itoa(rep.RecordsChecked); got != "30" {
		t.Errorf("RecordsChecked = %s", got)
	}
}

func TestVerifyFlagsExtraRecordsMetaKey(t *testing.T) {
	g := ingest30(t)
	recordstest.InjectMetaKey(t, g.c.Dir, "note", "x")
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{`records_meta holds the key "note", which is not part of the schema`})
}
