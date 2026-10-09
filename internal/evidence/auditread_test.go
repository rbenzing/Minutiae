package evidence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyAuditReadError pins the class of a ReadAudit failure without touching the file
// system, so it runs on every OS: a corrupt log is an integrity error (exit 4), anything else
// stays a plain error.
func TestClassifyAuditReadError(t *testing.T) {
	corrupt := fmt.Errorf("%w: audit line 3: bad", errAuditCorrupt)
	plain := errors.New("disk on fire")
	cases := []struct {
		name      string
		in        error
		integrity bool
	}{
		{"not exist", &fs.PathError{Op: "open", Path: "audit.jsonl", Err: fs.ErrNotExist}, false},
		{"corrupt line", corrupt, true},
		{"plain", plain, false},
	}
	for _, tc := range cases {
		got := classifyAuditReadError(tc.in)
		if got == nil {
			t.Fatalf("%s: nil", tc.name)
		}
		if errors.Is(got, ErrIntegrity) != tc.integrity {
			t.Errorf("%s: ErrIntegrity=%v, want %v (%v)", tc.name, errors.Is(got, ErrIntegrity), tc.integrity, got)
		}
		if !errors.Is(got, tc.in) {
			t.Errorf("%s: the cause is lost: %v", tc.name, got)
		}
	}
}

// TestReadAuditFileClasses runs ReadAudit against real files (no skip on Windows: a directory in
// place of the log is a plain I/O error everywhere).
func TestReadAuditFileClasses(t *testing.T) {
	dir := t.TempDir()
	c := &Case{Dir: dir}
	p := filepath.Join(dir, auditFile)
	if e, err := c.ReadAudit(); err != nil || len(e) != 0 {
		t.Fatalf("missing log: %v %v", e, err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if e, err := c.ReadAudit(); err != nil || len(e) != 0 {
		t.Fatalf("empty log: %v %v", e, err)
	}
	if err := os.WriteFile(p, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAudit(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt line: %v, want ErrIntegrity", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadAudit()
	if err == nil || errors.Is(err, ErrIntegrity) {
		t.Fatalf("directory as log: %v, want a plain I/O error", err)
	}
}
