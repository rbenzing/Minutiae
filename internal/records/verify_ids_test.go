package records_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestVerifyFlagsRecordsWithNonPositiveIDs (review C1): a record whose id is 0,
// negative or at the integer limits is read, counted and reported; the keyset
// scan must start below every possible id.
func TestVerifyFlagsRecordsWithNonPositiveIDs(t *testing.T) {
	for _, id := range []int64{0, -1, -5, math.MinInt64, math.MaxInt64} {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			g := ingest30(t)
			bid := recordstest.BatchID(t, g.c, g.res.IngestID, 3)
			recordstest.InjectRecord(t, g.c.Dir, id, bid, g.art.ID, "forged")
			rep := mustVerify(t, g.c)
			if rep.RecordsChecked != 31 {
				t.Errorf("RecordsChecked = %d, want 31: the forged row was never read", rep.RecordsChecked)
			}
			want := []string{"outside every batch range"}
			if id <= 0 {
				want = append(want, "id is not positive")
			}
			expectProblems(t, rep, want, "next_id")
			if !strings.Contains(strings.Join(rep.Problems, "\n"), "record "+strconv.FormatInt(id, 10)+" ") &&
				!strings.Contains(strings.Join(rep.Problems, "\n"), "record "+strconv.FormatInt(id, 10)+":") {
				t.Errorf("no problem names record %d: %q", id, rep.Problems)
			}
		})
	}
}

// TestVerifyFlagsRecordTimesWithNonPositiveIDs: a record_times row of record 0 or
// a negative id is read and reported as an orphan with a non-positive id.
func TestVerifyFlagsRecordTimesWithNonPositiveIDs(t *testing.T) {
	for _, id := range []int64{0, -3, math.MinInt64} {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			g := ingest30(t)
			recordstest.InjectRecordTime(t, g.c.Dir, id, "created", 5)
			rep := mustVerify(t, g.c)
			expectProblems(t, rep, []string{"has no record", "record_id is not positive"})
		})
	}
}

// TestVerifyDetectsAlteredBatchCreated (review I2): the creation time of a batch
// row is committed by the records.batch audit entry, so a changed one is caught.
func TestVerifyDetectsAlteredBatchCreated(t *testing.T) {
	g := ingest30(t)
	if rep := mustVerify(t, g.c); !rep.OK() {
		t.Fatalf("not clean before the change: %q", rep.Problems)
	}
	recordstest.SetBatchColumn(t, g.c.Dir, g.res.IngestID, 2, "created", "2001-01-01T00:00:00Z")
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"differs from the audit log", "created"})
}

// TestVerifyDetectsBatchRowDeletedKeepingRecords: the batch row is gone but its
// records remain, pointing at a batch id that no longer exists.
func TestVerifyDetectsBatchRowDeletedKeepingRecords(t *testing.T) {
	g := ingest30(t)
	bid := recordstest.BatchID(t, g.c, g.res.IngestID, 2)
	recordstest.DeleteBatchRowOnly(t, g.c.Dir, g.res.IngestID, 2)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"batch row missing", "batch id " + strconv.FormatInt(bid, 10) + " does not exist"})
}
