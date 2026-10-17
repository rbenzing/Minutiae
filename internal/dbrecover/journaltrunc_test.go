package dbrecover

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestAnalysisEndRecordsATruncatedJournal: a journal whose header declares more
// records than the file holds is audited as truncated, and the library's
// journal-truncated warning is in the run's audit log (LX-4 Task 2, L125).
func TestAnalysisEndRecordsATruncatedJournal(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	cut := append([]byte(nil), f.journal...)
	binary.BigEndian.PutUint32(cut[8:], 2) // two records declared, one held
	f.journal = cut
	db := addFile(t, c, "/data/x.db", f.db)
	addFile(t, c, "/data/x.db-journal", f.journal)
	mustRun(t, c, Inputs{DB: db.ID}, Options{})
	es := readAudit(t, c)
	end := lastEntry(t, es, "analysis.end").Details
	comp, _ := end["companions"].(map[string]any)
	cj, _ := comp["journal"].(map[string]any)
	if cj["hot"] != true || cj["truncated"] != true {
		t.Errorf("journal facts %v", cj)
	}
	found := false
	for _, i := range indexes(es, "analysis.warning") {
		if r, _ := es[i].Details["reason"].(string); strings.Contains(r, "[journal-truncated]") {
			found = true
		}
	}
	if !found {
		t.Error("the audit log holds no [journal-truncated] library warning")
	}
	// an intact journal is not marked
	e := newRunEnv(t)
	mustRun(t, e.c, e.in(), Options{})
	ok, _ := lastEntry(t, readAudit(t, e.c), "analysis.end").Details["companions"].(map[string]any)
	if cj, _ := ok["journal"].(map[string]any); cj["truncated"] != false {
		t.Errorf("intact journal facts %v", cj)
	}
}
