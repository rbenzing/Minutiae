package evidence

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAudit(t *testing.T) (*AuditLog, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenAuditLog(p, "tester", "test (none)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, p
}

func appendN(t *testing.T, a *AuditLog, actions ...string) {
	t.Helper()
	for _, act := range actions {
		if _, err := a.Append(act, "dev1", map[string]any{"k": act}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditAppendChainsHashes(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "one", "two", "three")
	es, err := ReadAuditEntries(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[0].PrevHash != GenesisHash || es[1].PrevHash != es[0].Hash || es[2].Seq != 3 {
		t.Fatalf("bad chain: %+v", es)
	}
	n, problems, err := VerifyAuditLog(p)
	if err != nil || n != 3 || len(problems) != 0 {
		t.Fatalf("verify: n=%d problems=%v err=%v", n, problems, err)
	}
}

func TestAuditReopenContinuesChain(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "one")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenAuditLog(p, "tester", "test (none)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	e, err := b.Append("two", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 2 {
		t.Fatalf("seq = %d, want 2", e.Seq)
	}
	if _, problems, _ := VerifyAuditLog(p); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

func TestAuditDetailsRoundTrip(t *testing.T) {
	a, p := newTestAudit(t)
	type src struct {
		Zeta  string `json:"zeta"`
		Alpha int64  `json:"alpha"`
	}
	_, err := a.Append("x", "", map[string]any{
		"big":    int64(1) << 62,
		"nested": map[string]any{"b": 1, "a": []string{"x", "y"}},
		"struct": src{Zeta: "z", Alpha: 7},
		"html":   "<a&b>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, problems, _ := VerifyAuditLog(p); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

func rewrite(t *testing.T, p string, f func(lines [][]byte) [][]byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	out := bytes.Join(f(lines), []byte("\n"))
	if err := os.WriteFile(p, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuditVerifyDetectsEdit(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "one", "two", "three")
	rewrite(t, p, func(l [][]byte) [][]byte {
		l[1] = bytes.Replace(l[1], []byte(`"action":"two"`), []byte(`"action":"TWO"`), 1)
		return l
	})
	_, problems, _ := VerifyAuditLog(p)
	if !containsSubstr(problems, "hash mismatch") {
		t.Fatalf("edit not detected: %v", problems)
	}
}

func TestAuditVerifyDetectsDeletedLine(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "one", "two", "three")
	rewrite(t, p, func(l [][]byte) [][]byte { return [][]byte{l[0], l[2]} })
	_, problems, _ := VerifyAuditLog(p)
	if !containsSubstr(problems, "seq") || !containsSubstr(problems, "prev_hash") {
		t.Fatalf("deletion not detected: %v", problems)
	}
}

func TestAuditVerifyDetectsReorder(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "one", "two", "three")
	rewrite(t, p, func(l [][]byte) [][]byte { return [][]byte{l[1], l[0], l[2]} })
	_, problems, _ := VerifyAuditLog(p)
	if len(problems) == 0 {
		t.Fatal("reorder not detected")
	}
}

func containsSubstr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
