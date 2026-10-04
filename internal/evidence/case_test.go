package evidence

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestCase(t testing.TB) *Case {
	t.Helper()
	c, err := Create(t.TempDir(), CreateOptions{ID: "CASE01", Examiner: "Examiner A"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCreateWritesLayout(t *testing.T) {
	c := newTestCase(t)
	for _, f := range []string{caseFile, auditFile, dbFile, artifactsDir} {
		if _, err := os.Stat(filepath.Join(c.Dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	es, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil || len(es) != 1 || es[0].Action != "case.create" {
		t.Fatalf("audit = %+v, %v", es, err)
	}
	if c.Meta.ToolVersion == "" || c.Meta.HostOS == "" || c.Meta.Created == "" {
		t.Fatalf("meta incomplete: %+v", c.Meta)
	}
}

func TestCreateRefusesNonEmptyDir(t *testing.T) {
	parent := t.TempDir()
	c, err := Create(parent, CreateOptions{ID: "C1", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	before, _ := os.ReadFile(filepath.Join(parent, "C1", caseFile))
	_, err = Create(parent, CreateOptions{ID: "C1", Examiner: "Other"})
	if !errors.Is(err, ErrCaseExists) {
		t.Fatalf("err = %v, want ErrCaseExists", err)
	}
	after, _ := os.ReadFile(filepath.Join(parent, "C1", caseFile))
	if string(before) != string(after) {
		t.Fatal("case.json was modified")
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	for _, id := range []string{
		"", "..", "../x", "a/b", `a\b`, "a b", ".hidden",
		"case.", "case-1.", "CON", "con", "Nul", "aux.txt", "PRN.case.1", "COM1", "com9.x", "LPT1", "lpt9",
	} {
		if _, err := Create(t.TempDir(), CreateOptions{ID: id, Examiner: "E"}); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if _, err := Create(t.TempDir(), CreateOptions{ID: "ok", Examiner: "  "}); err == nil {
		t.Error("blank examiner accepted")
	}
}

func TestOpenAppendsCaseOpen(t *testing.T) {
	c := newTestCase(t)
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if c2.Meta.ID != "CASE01" {
		t.Fatalf("meta = %+v", c2.Meta)
	}
	es, _ := ReadAuditEntries(filepath.Join(dir, auditFile))
	if len(es) != 2 || es[1].Action != "case.open" {
		t.Fatalf("audit = %+v", es)
	}
}

func TestOpenNonCaseFails(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenMissingAuditOrDBIsIntegrityError(t *testing.T) {
	for _, name := range []string{auditFile, dbFile} {
		t.Run(name, func(t *testing.T) {
			c := newTestCase(t)
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("err = %v, want ErrIntegrity", err)
			}
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s was recreated: %v", name, err)
			}
		})
	}
}

func TestCaseLockExcludesSecondOpen(t *testing.T) {
	c := newTestCase(t)
	if _, err := os.Stat(filepath.Join(c.Dir, lockFile)); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	if _, err := Open(c.Dir); !errors.Is(err, ErrCaseInUse) {
		t.Fatalf("second Open while first is open: err = %v, want ErrCaseInUse", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(c.Dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if _, err := Open(c.Dir); !errors.Is(err, ErrCaseInUse) {
		t.Fatalf("Open while reopened case is open: err = %v, want ErrCaseInUse", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCorruptDBIsIntegrityError: an artifacts.db that is not a SQLite
// database, or is damaged, fails closed as an integrity error (CLI exit 4) and
// is never recreated or rewritten.
func TestOpenCorruptDBIsIntegrityError(t *testing.T) {
	garbage := func(t *testing.T, p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(strings.Repeat("this is not a sqlite database ", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	truncate := func(frac func(int64) int64) func(*testing.T, string) {
		return func(t *testing.T, p string) {
			t.Helper()
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(p, frac(fi.Size())); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := map[string]func(*testing.T, string){
		"garbage":             garbage,
		"truncated-to-header": truncate(func(int64) int64 { return 100 }),
		"truncated-mid-page":  truncate(func(int64) int64 { return 5000 }),
		"truncated-half":      truncate(func(n int64) int64 { return n / 2 }),
		"truncated-by-a-page": truncate(func(n int64) int64 { return n - 4096 }),
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestCase(t)
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, dbFile)
			damage(t, p)
			before, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Open(dir)
			if !errors.Is(err, ErrIntegrity) {
				t.Fatalf("err = %v, want ErrIntegrity", err)
			}
			after, _ := os.ReadFile(p)
			if !bytes.Equal(before, after) {
				t.Fatal("artifacts.db was modified or recreated")
			}
			// the case lock must have been released
			if _, err := Open(dir); errors.Is(err, ErrCaseInUse) {
				t.Fatal("case lock still held after a failed Open")
			}
		})
	}
}

// TestOpenDBPermissionErrorIsNotIntegrity: an ordinary I/O error stays a plain
// error (only structural damage is an integrity failure).
func TestOpenDBPermissionErrorIsNotIntegrity(t *testing.T) {
	if err := classifyDBError(os.ErrPermission); errors.Is(err, ErrIntegrity) {
		t.Fatalf("permission error classified as integrity: %v", err)
	}
	if err := classifyDBError(errors.New("unable to open database file")); errors.Is(err, ErrIntegrity) {
		t.Fatalf("open failure classified as integrity: %v", err)
	}
	if err := classifyDBError(errors.New("database disk image is malformed")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("malformed not classified as integrity: %v", err)
	}
	if err := classifyDBError(errors.New("file is not a database")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("not-a-database not classified as integrity: %v", err)
	}
}
