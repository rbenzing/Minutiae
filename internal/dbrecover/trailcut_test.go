package dbrecover

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestDBRecoverCutTrailIsUnknownCap (3J residual R1): a recovery trail that names
// a confidence and is then cut by a missing parent is an unknown cap, because a
// lower confidence may lie beyond the cut. Every row is capped at the unknown-cap
// ceiling, flagged and counted; the found confidence is not kept.
func TestDBRecoverCutTrailIsUnknownCap(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	conf := 80
	rec := recordstest.AddDerivedWith(t, c, "x.db", evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: "no-such-parent", ParentSHA256: strings.Repeat("0", 64), Partition: 1, FSType: "mtfs", FSPath: "/x.db", FSID: "dentry:1:1:1",
		Runs:     []evidence.Run{{Offset: 0, Length: int64(len(f.db))}},
		Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Algorithm: evidence.AlgorithmRecover, Confidence: &conf},
	}}, f.db)
	res, err := Run(t.Context(), c, Inputs{DB: rec.ID, NoSiblings: true}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Counts.Written == 0 {
		t.Fatal("no rows written")
	}
	if res.Counts.RecoveryCapUnknown != res.Counts.Written {
		t.Errorf("RecoveryCapUnknown %d, written %d", res.Counts.RecoveryCapUnknown, res.Counts.Written)
	}
	for _, r := range storedRecords(t, c) {
		if r.Confidence == nil || *r.Confidence > unknownCapConfidence {
			t.Errorf("record %d confidence %p exceeds the unknown cap %d", r.ID, r.Confidence, unknownCapConfidence)
		}
		if notes, _ := payloadOf(t, r)["notes"].([]any); !slices.Contains(notes, any("recovery-cap-unknown")) {
			t.Errorf("record %d lacks the recovery-cap-unknown note", r.ID)
		}
	}
}
