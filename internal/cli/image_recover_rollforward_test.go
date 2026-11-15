package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func rfRecover(e *imgEnv, args ...string) (int, string, string) {
	return e.cli(append([]string{"image", "recover", "--case", e.c, e.ref, "--rollforward"}, args...)...)
}

func TestImageRecoverRollForward(t *testing.T) {
	e := rfEnv(t)
	code, out, errOut := rfRecover(e, "--all")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "post-checkpoint files (written after the last checkpoint: not deleted, not in the checkpoint view, never live):") {
		t.Errorf("header missing:\n%s", out)
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[recovered:") {
			n++
			if !strings.HasPrefix(l, "[recovered:f2fs-rollforward conf 60 medium]") || !strings.Contains(l, "[post-checkpoint]") {
				t.Errorf("line %q", l)
			}
		}
	}
	if n != 2 || !strings.Contains(out, "/rf/a.bin") || !strings.Contains(out, "recovered 2 ") || strings.Contains(out, "INCOMPLETE") {
		t.Errorf("%d artifact lines:\n%s", n, out)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
	recs := recoveredRecords(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	evidencetest.FlipArtifactByte(t, e.c, recs[0], 0)
	if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 4 {
		t.Errorf("a flipped byte: case verify exit %d, want 4\n%s", code, out)
	}
}

func TestImageRecoverRollForwardOneRef(t *testing.T) {
	e := rfEnv(t)
	l := e.list(t, "--rollforward", "--all")
	if len(l.Items) != 2 {
		t.Fatalf("%d items", len(l.Items))
	}
	code, out, errOut := rfRecover(e, "id:"+strings.TrimPrefix(l.Items[0].ID, "id:"))
	if code != 0 || strings.Count(out, "[recovered:") != 1 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if code, _, errOut := rfRecover(e, "id:rollforward:999999"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("unknown ref: exit %d %q", code, errOut)
	}
}

func TestImageRecoverRollForwardList(t *testing.T) {
	e := rfEnv(t)
	before := caseFiles(t, e.c)
	nAudit := len(auditActions(t, e.c))
	code, out, errOut := rfRecover(e, "--list")
	if code != 0 || strings.Count(out, "[post-checkpoint]") != 2 || !strings.Contains(out, "[would write]") ||
		!strings.Contains(out, "post-checkpoint files (written after the last checkpoint") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	after := caseFiles(t, e.c)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed", name)
		}
	}
	if len(after) != len(before) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("audit gained %v", acts)
	}
	l := e.list(t, "--rollforward")
	type shape struct {
		path string
		size int64
		conf int
	}
	var planned []shape
	for _, it := range l.Items {
		for _, c := range it.Candidates {
			if !c.WouldWrite {
				t.Errorf("%s not written", it.Path)
			}
			planned = append(planned, shape{it.Path, c.Size, c.Confidence})
		}
	}
	if c, _, errOut := rfRecover(e, "--all"); c != 0 {
		t.Fatalf("run: %d %s", c, errOut)
	}
	var ran []shape
	for _, r := range recoveredRecords(t, e.c) {
		rv := r.Source.Derived.Recovery
		ran = append(ran, shape{r.Source.Derived.FSPath, r.Size, *rv.Confidence})
	}
	key := func(a, b shape) int { return strings.Compare(a.path, b.path) }
	slices.SortFunc(planned, key)
	slices.SortFunc(ran, key)
	if !slices.Equal(planned, ran) || len(ran) != 2 {
		t.Errorf("the list %v differs from the run %v", planned, ran)
	}
}

func TestImageRecoverRollForwardListJSONNamesChain(t *testing.T) {
	e := rfEnv(t, cutChain)
	code, out, errOut := rfRecover(e, "--list", "--json")
	var j struct {
		Chain *struct {
			Complete bool   `json:"complete"`
			Stop     string `json:"stop"`
			Nodes    int    `json:"nodes"`
		} `json:"chain"`
	}
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || j.Chain == nil || j.Chain.Complete || !strings.HasPrefix(j.Chain.Stop, "footer@") {
		t.Fatalf("exit %d %v %+v\n%s%s", code, err, j, out, errOut)
	}
}

func TestImageRecoverRollForwardCutChainSaysIncomplete(t *testing.T) {
	e := rfEnv(t, cutChain)
	for _, args := range [][]string{{"--list"}, {"--all"}} {
		code, out, errOut := rfRecover(e, args...)
		if code != 0 || !strings.Contains(out, "INCOMPLETE: the chain is cut at footer@") || !strings.Contains(out, "later nodes are unknown") {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		if args[0] == "--all" && !strings.Contains(out, "conf 20") {
			t.Errorf("a cut chain's files are capped at 20:\n%s", out)
		}
	}
}

func TestImageRecoverRollForwardPathRefIsUsage(t *testing.T) {
	e := rfEnv(t)
	for _, ref := range []string{"/rf", "id:nid:5", "rf"} {
		if code, _, errOut := rfRecover(e, ref); code != 2 || !strings.Contains(errOut, "rollforward") {
			t.Errorf("ref %q: exit %d %q, want 2", ref, code, errOut)
		}
	}
	if code, _, _ := rfRecover(e); code != 2 {
		t.Errorf("no --all and no ref: exit %d, want 2", code)
	}
	if n := len(recoveredRecords(t, e.c)); n != 0 {
		t.Errorf("%d artifacts written by refused runs", n)
	}
}

// A deleted-entry run on the same image never shows a post-checkpoint marker.
func TestImageRecoverWithoutRollForwardHasNoMarker(t *testing.T) {
	e := rfEnv(t)
	_, out, _ := e.cli("image", "recover", "--case", e.c, e.ref, "--list")
	if strings.Contains(out, "[post-checkpoint]") || strings.Contains(out, "post-checkpoint files") {
		t.Errorf("a deleted-entry plan mentions post-checkpoint files:\n%s", out)
	}
}
