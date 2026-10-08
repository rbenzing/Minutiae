package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

var verifyRuns = []evidence.Run{{Offset: 4096, Length: 8192}, {Offset: 100000, Length: 5000}}

// recoveredCase is a case with a raw image parent and one recovered artifact built by build; the case is closed so the CLI can open it.
func recoveredCase(t *testing.T, build func(img []byte) evidencetest.RecoveredSpec) string {
	t.Helper()
	dir := newCLICase(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	img := recPat(7, 256<<10)
	parent := evidencetest.AddImage(t, c, img)
	evidencetest.AddRecovered(t, c, parent, img, build(img))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lastVerifyRun(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl")) //nolint:gosec // inside the test case
	if err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Action  string         `json:"action"`
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Action == "verify.run" {
			last = e.Details
		}
	}
	if last == nil {
		t.Fatal("no verify.run audit entry")
	}
	return last
}

func TestCaseVerifyRecoveredTamperExits4(t *testing.T) {
	dir := recoveredCase(t, func(img []byte) evidencetest.RecoveredSpec {
		data := slices.Concat(img[4096:4096+8192], img[100000:105000])
		data[9000] ^= 0x01
		return evidencetest.RecoveredSpec{Runs: verifyRuns, Data: data}
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "reproduce: byte") {
		t.Fatalf("exit %d, want 4 with a reproduce: byte problem:\n%s", code, out)
	}
	if ok, _ := lastVerifyRun(t, dir)["ok"].(bool); ok {
		t.Errorf("the verify.run audit entry says ok:true")
	}
}

func TestCaseVerifyRecoveredRunsOutsideImageExits4(t *testing.T) {
	dir := recoveredCase(t, func([]byte) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 256<<10 - 10, Length: 11}}, Data: make([]byte, 11)}
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "reproduce: run") {
		t.Fatalf("exit %d, want 4 with a reproduce: run problem:\n%s", code, out)
	}
}

func TestCaseVerifyRecoveredNamespaceExits4(t *testing.T) {
	dir := recoveredCase(t, func([]byte) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{Runs: verifyRuns, Path: "elsewhere/p1-mtfs/000001-a.bin"}
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "is outside the namespace") {
		t.Fatalf("exit %d, want 4 with the namespace problem:\n%s", code, out)
	}
}

// A case with no recovered artifact prints the summary line it always did.
func TestCaseVerifyCleanPlainCaseSummaryUnchanged(t *testing.T) {
	e := newImgEnv(t)
	code, out := run(t, e.d, "case", "verify", "--case", e.c)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	line := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	if !strings.HasPrefix(line, "OK: 1 artifacts, ") || !strings.HasSuffix(line, " audit entries, 0 problems") || strings.Contains(line, "recovered") {
		t.Errorf("summary line %q changed", line)
	}
}

func TestCaseVerifyJSONHasRecoveredCounts(t *testing.T) {
	dir := recoveredCase(t, func([]byte) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{Runs: verifyRuns}
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var rep map[string]any
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if rep["recovered_artifacts"] != float64(1) || rep["recovered_reproduced"] != float64(1) {
		t.Errorf("recovered counts %v / %v, want 1 / 1", rep["recovered_artifacts"], rep["recovered_reproduced"])
	}
}
