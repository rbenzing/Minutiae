package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func cleanVerifyCase(t *testing.T) string {
	t.Helper()
	c := recordstest.NewCase(t)
	recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// D115: a verify cut short by a cancel, with no other problem, is a plain cancelled error (exit 1, as a
// cancelled parse run), not an integrity failure.
func TestCaseVerifyCleanCancelExits1(t *testing.T) {
	dir := cleanVerifyCase(t)
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
		t.Fatalf("clean case: exit %d:\n%s", code, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, errs := runSplit(ctx, t, Deps{}, "case", "verify", "--case", dir)
	if code != 1 || !strings.Contains(errs, "cancel") || strings.Contains(errs, "integrity") {
		t.Fatalf("exit %d, want 1 with a cancelled error:\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if !strings.Contains(out, "verification cancelled") || strings.Contains(out, "context canceled") {
		t.Fatalf("the report must say the verify was cancelled, once, without cancel noise:\n%s", out)
	}
}

// D115: any other problem next to a cancel is still an integrity failure (exit 4).
func TestCaseVerifyCancelWithAnotherProblemExits4(t *testing.T) {
	dir := cleanVerifyCase(t)
	if err := os.MkdirAll(filepath.Join(dir, "staging", "ACQ1", "U1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staging", "ACQ1", "U1", "Manifest.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, errs := runSplit(ctx, t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "leftover staging directory ACQ1") {
		t.Fatalf("exit %d, want 4 with the leftover-staging problem:\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
}
