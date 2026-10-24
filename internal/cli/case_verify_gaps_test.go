package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// An extract that carries an acquisition-gap note although its parent (a raw image) records no acquisition
// errors fails case verify with exit 4: the note is recomputed from the container, not trusted. The JSON report
// always carries gap_notes_recomputed.
func TestCaseVerifyForgedGapNoteExits4(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	code, out := run(t, e.d, "case", "verify", "--case", e.c, "--json")
	if code != 0 {
		t.Fatalf("a clean case must verify before the forgery: %d %s", code, out)
	}
	var clean map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &clean); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if string(clean["gap_notes_recomputed"]) != "0" {
		t.Errorf("gap_notes_recomputed = %s, want the key present with 0 (no extract exists)", clean["gap_notes_recomputed"])
	}
	c, err := evidence.Open(e.c)
	if err != nil {
		t.Fatal(err)
	}
	note := &evidence.AcquisitionGapNote{Bytes: 512, Ranges: 1, First: []evidence.Run{{Offset: 0, Length: 512}}}
	evidencetest.AddDerived(t, c, e.rec, make([]byte, 512), evidence.Source{Kind: "extract", Derived: &evidence.Derivation{
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, DeclaredSize: 512, AcquisitionGaps: note,
	}})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out = run(t, e.d, "case", "verify", "--case", e.c)
	if code != 4 || !strings.Contains(out, "acquisition gaps: the raw parent records no acquisition errors, but the artifact carries a gap note") {
		t.Fatalf("exit %d, want 4 with the acquisition-gap problem:\n%s", code, out)
	}
	code, out = run(t, e.d, "case", "verify", "--case", e.c, "--json")
	var rep struct {
		Recomputed int `json:"gap_notes_recomputed"`
	}
	if err := json.NewDecoder(strings.NewReader(string(jsonPart(out)))).Decode(&rep); err != nil || code != 4 || rep.Recomputed != 1 {
		t.Errorf("exit %d, gap_notes_recomputed %d (%v):\n%s", code, rep.Recomputed, err, out)
	}
}
