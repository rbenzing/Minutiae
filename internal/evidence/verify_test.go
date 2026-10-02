package evidence

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func caseWithArtifact(t *testing.T) (*Case, ManifestRecord) {
	t.Helper()
	c := newTestCase(t)
	rec, err := c.Capture("dev1", "acq1", "a.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "evidence")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func mustVerify(t *testing.T, c *Case) VerifyReport {
	t.Helper()
	r, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifyCleanCase(t *testing.T) {
	c, _ := caseWithArtifact(t)
	r := mustVerify(t, c)
	if !r.OK() || r.ArtifactsChecked != 1 || r.AuditEntries == 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsModifiedArtifact(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)), []byte("evidencE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); r.OK() || !containsSubstr(r.Problems, "hash mismatch") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsDeletedArtifact(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); r.OK() {
		t.Fatal("deleted artifact not detected")
	}
}

func TestVerifyDetectsUnmanifestedFile(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, artifactsDir, "planted.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !containsSubstr(r.Problems, "unmanifested") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsManifestLineRemoved(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, manifestFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !containsSubstr(r.Problems, "not in manifest") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsAuditTamper(t *testing.T) {
	c, _ := caseWithArtifact(t)
	p := filepath.Join(c.Dir, auditFile)
	rewrite(t, p, func(l [][]byte) [][]byte { return l[1:] })
	if r := mustVerify(t, c); r.OK() {
		t.Fatal("audit tamper not detected")
	}
}

func TestVerifyAppendsVerifyRun(t *testing.T) {
	c, _ := caseWithArtifact(t)
	mustVerify(t, c)
	es, _ := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if es[len(es)-1].Action != "verify.run" {
		t.Fatalf("last action = %s", es[len(es)-1].Action)
	}
}
