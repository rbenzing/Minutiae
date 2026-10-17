package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Residual of the OpenArtifact manifest cache: an edit that keeps the manifest's file, size and
// modification time is not seen by OpenArtifact. `case verify` never uses the cache, recomputes, and
// exits 4.
func TestCaseVerifyCatchesAManifestEditTheOpenCacheCannotSee(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	if f, _, err := c.OpenArtifact(a.ID); err != nil { // fills the cache
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
	path := filepath.Join(c.Dir, "manifest.jsonl")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// change one hex digit of the recorded SHA-256, in place
	old := []byte(`"sha256":"` + a.SHA256[:1])
	flip := "0"
	if a.SHA256[:1] == "0" {
		flip = "1"
	}
	edited := bytes.Replace(orig, old, []byte(`"sha256":"`+flip), 1)
	if len(edited) != len(orig) || bytes.Equal(edited, orig) {
		t.Fatal("the test edit must keep the size and change the bytes")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	// the cache (same size, same time) still serves the old record: this is the documented residual
	if f, rec, err := c.OpenArtifact(a.ID); err != nil || rec.SHA256 != a.SHA256 {
		t.Fatalf("the cache was expected to serve the stale record: %v %q", err, rec.SHA256)
	} else {
		_ = f.Close()
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "PROBLEM:") {
		t.Fatalf("case verify exit %d, want 4 with a problem:\n%s", code, out)
	}
}
