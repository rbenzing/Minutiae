package sqlitefile_test

// Engine oracle for a truncated hot journal (LX-4 Task 2, L125): a journal whose
// header declares more records than the file holds is played back by the engine
// up to the records present. Live() presents the same state, and the scan says
// why it is short.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestTruncatedJournalLiveEqualsTheEngine(t *testing.T) {
	for _, ps := range []int{1024, 4096} {
		t.Run(fmt.Sprint("ps", ps), func(t *testing.T) {
			h := newHotFixture(t, ps)
			offs := h.records()
			if len(offs) < 8 {
				t.Fatalf("the journal holds only %d records", len(offs))
			}
			inSeg0 := 0
			for _, r := range offs {
				if r.seg == 0 {
					inSeg0++
				}
			}
			compared, refused := 0, 0
			for _, declared := range []uint32{uint32(inSeg0), 100000} {
				for _, keep := range []int{1, 3, min(6, inSeg0-1)} {
					if keep >= inSeg0 {
						continue
					}
					for _, partial := range []int{0, h.ps / 2} {
						jm := bytes.Clone(h.journal)
						binary.BigEndian.PutUint32(jm[8:], declared)
						jm = jm[:offs[keep].off+partial]
						label := fmt.Sprintf("declared %d, %d whole records and %d bytes", declared, keep, partial)
						switch compareJournalCopy(t, label, h.db, nil, jm) {
						case copyCompared:
							compared++
							d := openLibrary(t, h.db, nil, jm)
							j := d.Status().Journal
							if j == nil || !j.Truncated || j.Segments[0].DeclaredRecords != declared || int(j.Segments[0].Records) != keep {
								t.Errorf("%s: the scan does not name the truncation: %+v", label, j)
							}
						case copyEngineRefused:
							refused++
						}
					}
				}
			}
			t.Logf("%d truncated copies compared equal to the engine, %d unreadable by the engine", compared, refused)
			if compared == 0 {
				t.Error("no truncated copy could be compared: the test is vacuous")
			}
		})
	}
}
