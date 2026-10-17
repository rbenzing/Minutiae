package sqlitedb_test

import (
	"encoding/binary"
	"testing"
)

// TestOpenReportsATruncatedJournal: a hot journal whose header declares more
// records than the file holds is named on the opened DB (LX-4 Task 2, L125).
func TestOpenReportsATruncatedJournal(t *testing.T) {
	b := companionBase(t)
	snap := b.Snapshot()
	companionRewrite(b, "one")
	j := companionJournal(b, snap, 0xabc, 0)
	jb := j.Bytes()
	if len(jb) < 512+2*(4+512+4) {
		t.Fatalf("the journal holds fewer than two records (%d bytes)", len(jb))
	}
	binary.BigEndian.PutUint32(jb[8:], 1_000_000) // far more records than the file holds
	d := openBytes(t, b.Bytes(), nil, jb, bigBudget())
	found := false
	for _, w := range d.Warnings() {
		if w.Code == "journal-truncated" {
			found = true
		}
	}
	if !found {
		t.Errorf("no journal-truncated warning among %v", d.Warnings())
	}
	if ji := d.Info().Journal; ji == nil || !ji.Truncated {
		t.Errorf("Info().Journal = %+v, want Truncated", ji)
	}
	// an intact journal is not reported
	d2 := openBytes(t, b.Bytes(), nil, j.Bytes(), bigBudget())
	for _, w := range d2.Warnings() {
		if w.Code == "journal-truncated" {
			t.Errorf("intact journal reported truncated: %v", w)
		}
	}
}
