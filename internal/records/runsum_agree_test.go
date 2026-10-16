package records_test

import (
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Get and case verify agree on a complete derived artifact whose runs do (or do not) add up to its size.
func TestGetRunSumAgreesWithVerify(t *testing.T) {
	for _, k := range []struct {
		name    string
		runs    []evidence.Run
		flagged bool
	}{
		{"exact", []evidence.Run{{Offset: 100, Length: 8}}, false},
		{"below the size", []evidence.Run{{Offset: 100, Length: 4}}, true},
		{"above the size", []evidence.Run{{Offset: 100, Length: 16}}, true},
		{"a hole counts as covered bytes", []evidence.Run{{Offset: -1, Length: 4}, {Offset: 100, Length: 4}}, false},
	} {
		t.Run(k.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
			f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: k.runs}), patterned(8))
			p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 2})
			rep := verifyReport(t, c)
			if got := hasProblemText(rep, "runs cover"); got != k.flagged {
				t.Fatalf("verify flags the runs = %v, want %v: %q", got, k.flagged, rep.Problems)
			}
			if got := problemKinds(p)[records.ProblemRunsInconsistent] == 1; got != k.flagged {
				t.Fatalf("Get flags the runs = %v, want %v: %+v", got, k.flagged, p.Problems)
			}
		})
	}
}

// RunsSum has the validity rule of TranslateRange: it refuses what TranslateRange calls invalid, and
// when both accept, it equals the artifact size exactly when TranslateRange does not say "runs cover".
func TestRunsSumAgreesWithTranslateRange(t *testing.T) {
	const size = 8
	for _, runs := range [][]evidence.Run{
		{{Offset: 0, Length: 8}},
		{{Offset: -1, Length: 4}, {Offset: 0, Length: 4}},
		{{Offset: 0, Length: 4}, {Offset: 100, Length: 4}},
		{{Offset: 0, Length: 4}},
		{{Offset: 0, Length: 16}},
		{{Offset: -2, Length: 4}},
		{{Offset: 0, Length: 0}},
		{{Offset: 0, Length: -1}},
		{{Offset: math.MaxInt64 - 1, Length: 5}},
		{{Offset: 0, Length: math.MaxInt64}, {Offset: 0, Length: 1}},
		{{Offset: -1, Length: math.MaxInt64}, {Offset: -1, Length: 8}},
		{{Offset: 0, Length: 4}, {Offset: 4, Length: 4}},
	} {
		sum, serr := evidence.RunsSum(runs)
		_, terr := records.TranslateRange(runs, size, false, records.Range{})
		invalid := terr != nil && strings.Contains(terr.Error(), "runs are invalid")
		if (serr != nil) != invalid {
			t.Errorf("%v: RunsSum error %v, TranslateRange error %v", runs, serr, terr)
			continue
		}
		if serr == nil {
			covers := terr != nil && strings.Contains(terr.Error(), "runs cover")
			if covers != (sum != size) {
				t.Errorf("%v: sum %d, TranslateRange error %v", runs, sum, terr)
			}
		}
	}
}
