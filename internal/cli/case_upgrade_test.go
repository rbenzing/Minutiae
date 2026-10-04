package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestCaseUpgradeCLI(t *testing.T) {
	c := recordstest.NewV1Case(t)

	code, out := run(t, Deps{}, "case", "info", "--case", c)
	if code != 0 || !strings.Contains(out, "Schema:") || !strings.Contains(out, "v1") || !strings.Contains(out, "case upgrade") {
		t.Fatalf("info of a v1 case: %d %s", code, out)
	}

	code, out = run(t, Deps{}, "case", "upgrade", "--case", c)
	if code != 0 || !strings.Contains(out, "v1") || !strings.Contains(out, "v3") {
		t.Fatalf("upgrade: %d %s", code, out)
	}
	code, out = run(t, Deps{}, "case", "upgrade", "--case", c)
	if code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("second upgrade: %d %s", code, out)
	}

	code, out = run(t, Deps{Err: &bytes.Buffer{}}, "case", "upgrade", "--case", c, "--json")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil {
		t.Fatalf("upgrade --json: %d %q %v", code, out, err)
	}
	if res["from"] != float64(3) || res["to"] != float64(3) || res["upgraded"] != false || res["records_to_index"] != float64(0) {
		t.Fatalf("json = %v", res)
	}

	code, out = run(t, Deps{Err: &bytes.Buffer{}}, "case", "info", "--case", c, "--json")
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); code != 0 || err != nil || info["schema_version"] != float64(3) {
		t.Fatalf("info json: %d %q %v", code, out, err)
	}
	if code, out = run(t, Deps{}, "case", "info", "--case", c); code != 0 || !strings.Contains(out, "Schema:") || !strings.Contains(out, "v3") {
		t.Fatalf("info text: %d %s", code, out)
	}
	if code, out = run(t, Deps{}, "case", "verify", "--case", c); code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("verify after upgrade: %d %s", code, out)
	}
}

func TestCaseUpgradeJSONReportsUpgrade(t *testing.T) {
	c := recordstest.NewV1Case(t)
	code, outS := run(t, Deps{Err: &bytes.Buffer{}}, "case", "upgrade", "--case", c, "--json")
	if code != 0 {
		t.Fatalf("upgrade --json: %d %s", code, outS)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(outS), &res); err != nil {
		t.Fatalf("json %q: %v", outS, err)
	}
	if res["from"] != float64(1) || res["to"] != float64(3) || res["upgraded"] != true || res["records_to_index"] != float64(0) {
		t.Fatalf("json = %v", res)
	}
}

func TestCaseUpgradeBlockedExit1(t *testing.T) {
	c := recordstest.NewV1CaseWithRecord(t)
	dbPath := filepath.Join(c, "artifacts.db")
	before := sha256Of(t, dbPath)

	code, out := run(t, Deps{}, "case", "upgrade", "--case", c)
	if code != ExitError || !strings.Contains(out, "blocked") {
		t.Fatalf("blocked upgrade: %d %s", code, out)
	}
	if sha256Of(t, dbPath) != before {
		t.Fatal("a blocked upgrade changed artifacts.db")
	}
	es, err := evidence.ReadAuditEntries(filepath.Join(c, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Action)
	}
	if !containsAll(actions, "case.upgrade", "case.upgrade.error") {
		t.Fatalf("audit actions = %v", actions)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", c); code != 0 {
		t.Fatalf("verify after a blocked upgrade: %d %s", code, out)
	}
}

func TestCaseUpgradeRefusedWhenInUse(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	held, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	code, out := run(t, Deps{}, "case", "upgrade", "--case", dir)
	if code != ExitError || !strings.Contains(out, "in use") {
		t.Fatalf("upgrade of a case in use: %d %s", code, out)
	}
	if v, err := held.SchemaVersion(); err != nil || v != 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestCaseVerifyPrintsNotices(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// announce an upgrade that is never concluded
	if _, err := c.Audit.Append("case.upgrade", "", map[string]any{"from": 1, "to": 2}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 0 || !strings.Contains(out, "NOTICE: ") || !strings.Contains(out, "OK") {
		t.Fatalf("verify: %d %s", code, out)
	}
}

func TestNeedsUpgradeExitsUsage(t *testing.T) {
	err := errors.Join(evidence.ErrNeedsUpgrade, errors.New("x"))
	if ExitCode(err) != ExitUsage {
		t.Fatalf("ExitCode(ErrNeedsUpgrade) = %d, want %d", ExitCode(err), ExitUsage)
	}
}

func sha256Of(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestCaseVerifyEscapesProblemText: a problem that carries text from the case (here
// a manifest path with an escape sequence and a bidi override) is printed
// escaped, never raw; the exit code is still 4.
func TestCaseVerifyEscapesProblemText(t *testing.T) {
	rc := recDataset(t, false)
	recordstest.SetManifestPath(t, rc.dir, rc.art.ID, "artifacts/x\x1b[31mRED"+string(rune(0x202e))+"evil.db")
	code, out := run(t, Deps{}, "case", "verify", "--case", rc.dir)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM: ") {
		t.Fatalf("verify: %d %s", code, out)
	}
	noRawNonPrintable(t, "case verify output", out)
	if !strings.Contains(out, `\x1b`) || !strings.Contains(out, string(rune(0x5c))+"u202e") {
		t.Errorf("the escapes are not visible in the output:\n%s", out)
	}
}

// TestCaseUpgradePrintsReindexHint: a v2 case with records upgrades to "index not built" and the
// command says how many records are not searchable and which command builds the index; a case with
// no records has a current index at once and gets no hint.
func TestCaseUpgradePrintsReindexHint(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		dir := recordstest.CopyV2Case(t)
		code, out := run(t, Deps{}, "case", "upgrade", "--case", dir)
		if code != 0 || !strings.Contains(out, "v2 -> v3") {
			t.Fatalf("upgrade: %d %s", code, out)
		}
		for _, want := range []string{"12 records", "not searchable", "minutiae records reindex --case " + dir} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q lacks %q", out, want)
			}
		}
		// the hint is not repeated as news by a no-op upgrade, but the state is still said
		code, out = run(t, Deps{}, "case", "upgrade", "--case", dir)
		if code != 0 || !strings.Contains(out, "already") || !strings.Contains(out, "records reindex --case "+dir) {
			t.Fatalf("second upgrade: %d %s", code, out)
		}
	})
	t.Run("json", func(t *testing.T) {
		dir := recordstest.CopyV2Case(t)
		code, out := run(t, Deps{Err: &bytes.Buffer{}}, "case", "upgrade", "--case", dir, "--json")
		var res map[string]any
		if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil {
			t.Fatalf("upgrade --json: %d %q %v", code, out, err)
		}
		if res["from"] != float64(2) || res["to"] != float64(3) || res["upgraded"] != true || res["records_to_index"] != float64(12) {
			t.Fatalf("json = %v", res)
		}
	})
	t.Run("no records, no hint", func(t *testing.T) {
		dir := recordstest.NewV1Case(t)
		code, out := run(t, Deps{}, "case", "upgrade", "--case", dir)
		if code != 0 || strings.Contains(out, "reindex") || strings.Contains(out, "searchable") {
			t.Fatalf("upgrade of an empty case: %d %s", code, out)
		}
	})
}
