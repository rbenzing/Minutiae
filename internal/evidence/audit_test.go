package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAudit(t *testing.T) (*AuditLog, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := CreateAuditLog(p, "tester", "test (none)")
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
	appendN(t, a, "case.create", "two", "three")
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
	appendN(t, a, "case.create")
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
	appendN(t, a, "case.create")
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
	appendN(t, a, "case.create", "two", "three")
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
	appendN(t, a, "case.create", "two", "three")
	rewrite(t, p, func(l [][]byte) [][]byte { return [][]byte{l[0], l[2]} })
	_, problems, _ := VerifyAuditLog(p)
	if !containsSubstr(problems, "seq") || !containsSubstr(problems, "prev_hash") {
		t.Fatalf("deletion not detected: %v", problems)
	}
}

func TestAuditVerifyDetectsReorder(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.create", "two", "three")
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

// Spec §6.2: hash = sha256(canonical JSON of the entry without "hash").
func TestAuditHashIsOverEntryWithoutHashField(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.create")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.TrimRight(b, "\n")
	i := bytes.LastIndex(line, []byte(`,"hash":"`))
	if i < 0 {
		t.Fatalf("no hash field in %s", line)
	}
	withoutHash := append(append([]byte{}, line[:i]...), '}')
	sum := sha256.Sum256(withoutHash)
	es, err := ReadAuditEntries(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum[:]); got != es[0].Hash {
		t.Fatalf("hash %s is not sha256 of %s (= %s)", es[0].Hash, withoutHash, got)
	}
}

func TestAuditInvalidUTF8IsNotFalseTamper(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.create")
	if _, err := a.Append("acquire.start", "dev\xff", map[string]any{"name": "x\xfey"}); err != nil {
		t.Fatal(err)
	}
	if _, problems, err := VerifyAuditLog(p); err != nil || len(problems) != 0 {
		t.Fatalf("problems=%v err=%v", problems, err)
	}
}

func TestOpenAuditLogCorruptLineIsIntegrityError(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.create", "two", "three")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	rewrite(t, p, func(l [][]byte) [][]byte {
		l[1] = l[1][:len(l[1])/2]
		return l
	})
	_, err := OpenAuditLog(p, "tester", "test (none)")
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "audit log unreadable at line 2 (edited or torn write)") {
		t.Fatalf("err = %v, want ErrIntegrity at line 2", err)
	}
}

func TestOpenAuditLogTornLastLineIsIntegrityError(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.create", "two")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	truncateTail(t, p, 20)
	_, err := OpenAuditLog(p, "tester", "test (none)")
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "audit log unreadable at line 2") {
		t.Fatalf("err = %v, want ErrIntegrity at line 2", err)
	}
}

func TestAuditVerifyFlagsEmptyLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, problems, err := VerifyAuditLog(p); err != nil || !containsSubstr(problems, "audit log is empty") {
		t.Fatalf("problems=%v err=%v", problems, err)
	}
}

func TestAuditVerifyRequiresCaseCreateFirst(t *testing.T) {
	a, p := newTestAudit(t)
	appendN(t, a, "case.open", "two")
	if _, problems, err := VerifyAuditLog(p); err != nil || !containsSubstr(problems, `line 1: action "case.open", want "case.create"`) {
		t.Fatalf("problems=%v err=%v", problems, err)
	}
}

func TestCreateAuditLogRefusesExisting(t *testing.T) {
	_, p := newTestAudit(t)
	if _, err := CreateAuditLog(p, "tester", "test (none)"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
}

func TestOpenAuditLogMissingIsIntegrityError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if _, err := OpenAuditLog(p, "tester", "test (none)"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenAuditLog created the file: %v", err)
	}
}

// truncateTail cuts the last n bytes off the file, simulating a torn write.
func truncateTail(t *testing.T, p string, n int64) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, fi.Size()-n); err != nil {
		t.Fatal(err)
	}
}
