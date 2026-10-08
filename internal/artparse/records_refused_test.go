package artparse_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// A job whose writer refused records is incomplete (reason records-refused), never complete: a complete
// run supersedes older complete runs of the same parser, so a run that lost records must not hide them.
func TestRefusedRecordsMakeTheJobIncomplete(t *testing.T) {
	f := newRx(t, "inv")
	sum := f.run(f.host(nil, parsertest.Invalid{Name: "inv", Version: "1.0.0", Mode: parsertest.InvalidNulInSummary, N: 2}), artparse.Selection{})
	j := jobOf(t, sum, "inv")
	if j.Outcome != artparse.OutcomeIncomplete || !strings.Contains(j.Reason, "records-refused") || j.Rejected != 2 {
		t.Fatalf("job %+v, want incomplete, records-refused, 2 rejected", j)
	}
	if sum.Class() != artparse.ClassPartial {
		t.Errorf("class %v, want partial", sum.Class())
	}
	end := f.jobEnds()[0]
	if end.Outcome != artparse.OutcomeIncomplete || end.Rejected != 2 || !strings.Contains(end.Reason, "records-refused") {
		t.Errorf("job.end %+v", end)
	}
}

func TestRefusedRecordsRunDoesNotSupersedeAnOlderCompleteRun(t *testing.T) {
	f := newRx(t, "p")
	f.run(f.host(nil, wbp("p")), artparse.Selection{})
	sum := f.run(f.host(nil, parsertest.Invalid{Name: "p", Version: "2.0.0", Mode: parsertest.InvalidNulInSummary, N: 1}), artparse.Selection{})
	if j := jobOf(t, sum, "p"); j.Outcome != artparse.OutcomeIncomplete {
		t.Fatalf("job %+v", j)
	}
	v1 := 0
	for _, r := range f.rows(false) {
		if r.Parser.Version == "1.0.0" {
			v1++
		}
	}
	if v1 != 3 {
		t.Errorf("%d rows of the complete v1.0.0 are visible, want 3: a run that lost records never hides them", v1)
	}
}

func TestJobEndCarriesTheWarningCount(t *testing.T) {
	f := newRx(t, "w")
	putFile(t, f.c, "D1", "A1", fakePath("blank"), "x\n\n\ny\n") // two blank lines: two warnings
	sum := f.run(f.host(nil, wbp("blank")), artparse.Selection{})
	j := jobOf(t, sum, "blank")
	end := f.jobEnds()[j.Job-1]
	if end.Warnings != 2 || j.Warnings != 2 {
		t.Errorf("job.end warnings %d, result %d, want 2", end.Warnings, j.Warnings)
	}
}
