package records

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestBatchAuditSeqNeverReadsAnUndecodableEntryAsAbsent: Get shows the audit seq of a record's batch; an
// entry of records.batch this build cannot decode (a huge layout, a newer layout, an unknown field) is an
// integrity error, never "no entry" (seq 0). The CLI path was already exit 4 through IngestGapsFrom, which
// decodes every batch entry after this call; this pins the function itself.
func TestBatchAuditSeqNeverReadsAnUndecodableEntryAsAbsent(t *testing.T) {
	good := evidence.BatchCommit{IngestID: "ing", BatchNo: 1, FirstID: 1, Count: 1}.Details()
	b := BatchInfo{IngestID: "ing", BatchNo: 1}
	if seq, err := batchAuditSeq([]evidence.AuditEntry{{Seq: 7, Action: evidence.ActionBatch, Details: good}}, b); err != nil || seq != 7 {
		t.Fatalf("good entry: seq %d, err %v", seq, err)
	}
	for name, mut := range map[string]func(map[string]any){
		"huge layout":  func(d map[string]any) { d["layout"] = json.Number("99999999999999999999") },
		"layout 2":     func(d map[string]any) { d["layout"] = json.Number("2") },
		"unknown key":  func(d map[string]any) { d["unexpected_field"] = "x" },
		"bad layout 0": func(d map[string]any) { d["layout"] = json.Number("0") },
	} {
		t.Run(name, func(t *testing.T) {
			bad := evidence.BatchCommit{IngestID: "other", BatchNo: 9, FirstID: 1, Count: 1}.Details()
			mut(bad)
			// the undecodable entry comes first, the matching good one after it
			seq, err := batchAuditSeq([]evidence.AuditEntry{
				{Seq: 3, Action: evidence.ActionBatch, Details: bad},
				{Seq: 7, Action: evidence.ActionBatch, Details: good},
			}, b)
			if !errors.Is(err, evidence.ErrIntegrity) || seq != 0 {
				t.Errorf("seq %d, err %v; want 0 and an integrity-class error", seq, err)
			}
		})
	}
}
