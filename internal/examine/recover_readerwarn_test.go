package examine_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// readerWarnEntries returns the audited reader-warning texts and the number of suppression entries.
func readerWarnEntries(t *testing.T, e *recEnv) (texts []string, suppressed int) {
	t.Helper()
	for _, w := range auditByAction(t, e.c, "analysis.warning") {
		switch w.Details["code"] {
		case "reader-warning":
			if w.Details["source"] != "filesystem-recover" {
				t.Errorf("reader warning source %v", w.Details["source"])
			}
			r, _ := w.Details["reason"].(string)
			texts = append(texts, r)
		case "reader-warnings-suppressed":
			suppressed++
		}
	}
	return texts, suppressed
}

func TestCandidateWarningsReachAuditAndSummary(t *testing.T) {
	e := newRecEnv(t, delNode("/w.bin", pat(8, bs), fstest.RecoverMap{Method: mtfsRecM, Warnings: []string{"stale name \x1b[31m", "second"}}))
	sum := e.recover(t, recoverAllOpts())
	texts, _ := readerWarnEntries(t, e)
	if len(texts) != 2 {
		t.Fatalf("%d reader-warning entries, want 2: %v", len(texts), texts)
	}
	if strings.ContainsRune(texts[0], 0x1b) || !strings.Contains(texts[0], "stale name") {
		t.Errorf("first warning %q: the escape byte must be removed", texts[0])
	}
	if sum.ReaderWarnings != 2 || sum.Skipped != 0 {
		t.Errorf("ReaderWarnings %d Skipped %d, want 2 and 0", sum.ReaderWarnings, sum.Skipped)
	}
	ends := auditByAction(t, e.c, "analysis.end")
	if got := ends[len(ends)-1].Details["reader_warnings"]; fmt.Sprint(got) != "2" {
		t.Errorf("analysis.end reader_warnings = %v, want 2", got)
	}
	if len(recovered(t, e.c)) != 1 {
		t.Error("the artifact was not written")
	}
}

func TestCandidateWarningsAreBounded(t *testing.T) {
	ws := make([]string, 40)
	for i := range ws {
		ws[i] = fmt.Sprintf("%02d", i) + strings.Repeat("x", 5000)
	}
	e := newRecEnv(t, delNode("/w.bin", pat(9, bs), fstest.RecoverMap{Method: mtfsRecM, Warnings: ws}))
	sum := e.recover(t, recoverAllOpts())
	texts, sup := readerWarnEntries(t, e)
	if len(texts) != 16 || sup != 1 {
		t.Fatalf("%d reader-warning entries and %d suppression entries, want 16 and 1", len(texts), sup)
	}
	for _, x := range texts {
		if len(x) > 256 {
			t.Errorf("a warning text has %d bytes, want at most 256", len(x))
		}
	}
	if sum.ReaderWarnings != 40 {
		t.Errorf("ReaderWarnings %d, want 40", sum.ReaderWarnings)
	}
}
