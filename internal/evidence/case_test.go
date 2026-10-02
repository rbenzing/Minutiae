package evidence

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func newTestCase(t *testing.T) *Case {
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
	for _, id := range []string{"", "..", "../x", "a/b", `a\b`, "a b", ".hidden"} {
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
