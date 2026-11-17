package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestAcceptanceEndToEnd builds cmd/minutiae and runs every recovery command over the committed fixtures, exactly
// as `go run ./tools/recoveryaccept` does, except that the scratch parent is t.TempDir() (parseFlags, whose
// project-directory rule is for operators, is bypassed). A step whose exit differs from its pin fails the test
// (3K-R9): the table is never edited to match an observation.
func TestAcceptanceEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds the binary and runs every recovery command; runs in the full check")
	}
	root := moduleRoot(t)
	var out bytes.Buffer
	start := time.Now()
	code := accept(context.Background(), options{root: root, dir: t.TempDir()}, execRunner{}, &out)
	t.Logf("accept took %s:\n%s", time.Since(start).Round(time.Second), out.String())
	if code != 0 {
		t.Fatalf("accept exited %d", code)
	}
	if !bytes.Contains(out.Bytes(), []byte("ACCEPTANCE PASSED")) {
		t.Errorf("no ACCEPTANCE PASSED line")
	}
}
