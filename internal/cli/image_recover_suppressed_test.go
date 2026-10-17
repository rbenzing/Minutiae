package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
)

// The dry run shows how many warnings the filesystem dropped while planning: its suppression note is one
// entry of FSWarnings however much it grew.
func TestRecoverListShowsWarningsSuppressedDuringPlanning(t *testing.T) {
	plan := &examine.RecoverPlan{FSWarnings: []string{"further warnings suppressed: 251 occurrences not recorded"}, FSWarningsSuppressed: 250}
	var b bytes.Buffer
	printRecoverList(&b, plan)
	if !strings.Contains(b.String(), "filesystem warnings suppressed while planning: 250") {
		t.Errorf("text summary lacks the suppressed count:\n%s", b.String())
	}
	var j map[string]any
	raw, err := json.Marshal(newJSONRecoverList(plan))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	if v, ok := j["fs_warnings_suppressed"]; !ok || v.(float64) != 250 {
		t.Errorf("json fs_warnings_suppressed = %v (present %v), want 250", v, ok)
	}
	var none bytes.Buffer
	printRecoverList(&none, &examine.RecoverPlan{})
	if strings.Contains(none.String(), "suppressed") {
		t.Errorf("a plan that dropped nothing mentions suppression:\n%s", none.String())
	}
}
