package evidence

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// cancelAfter is a reader that cancels the context once n bytes were read and counts what was read.
type cancelAfter struct {
	r      io.Reader
	n      int64
	read   int64
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.read += int64(k)
	if c.read >= c.n {
		c.cancel()
	}
	return k, err
}

// D113: the hash of a big artifact stops soon after the cancel, not at the end of the file.
func TestHashReaderContextStopsSoonAfterACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancelAfter{r: bytes.NewReader(make([]byte, 64<<20)), n: 1 << 20, cancel: cancel}
	start := time.Now()
	_, err := hashReaderContext(ctx, src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if src.read > 2<<20 || time.Since(start) > 30*time.Second {
		t.Fatalf("read %d bytes in %v after a cancel at 1 MiB", src.read, time.Since(start))
	}
	// a live context hashes the whole stream, exactly as HashFile does
	d, err := hashReaderContext(context.Background(), bytes.NewReader([]byte("abc")))
	if err != nil || d.Size != 3 {
		t.Fatalf("live hash: %+v %v", d, err)
	}
}

// D113: a verify whose context is dead before the artifact phase hashes nothing, and says it was cut.
func TestVerifyCancelledBeforeTheHashPhaseHashesNothing(t *testing.T) {
	c := indexedTextCase(t, 20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := c.VerifyWith(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ArtifactsChecked != 0 {
		t.Fatalf("ArtifactsChecked = %d after a cancel before the hash phase", rep.ArtifactsChecked)
	}
	if !slices.Contains(rep.Problems, verifyCancelled) {
		t.Fatalf("no %q in %q", verifyCancelled, rep.Problems)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "unmanifested") || strings.Contains(p, "not in the manifest") {
			t.Fatalf("a skipped hash must not make the artifact look unmanifested: %q", p)
		}
	}
}

// D114 and D116 at the evidence level: the problems of a cancelled run are those of the uncancelled run
// plus the single cancelled line, and nothing that looks like corruption is added by the cancel.
func TestVerifyCancelAddsOnlyTheCancelledLine(t *testing.T) {
	c := indexedTextCase(t, 12000)
	recs, err := c.Manifest()
	if err != nil || len(recs) == 0 {
		t.Fatalf("manifest: %v %d", err, len(recs))
	}
	path := filepath.Join(c.Dir, filepath.FromSlash(recs[0].Path))
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	base, err := c.Verify() // everything the uncancelled run finds
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "fts" {
			cancel()
		}
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, p := range rep.Problems {
		if !slices.Contains(base.Problems, p) {
			extra = append(extra, p)
		}
	}
	if len(extra) != 1 || extra[0] != verifyCancelled {
		t.Fatalf("a cancel added %q, want exactly [%q]", extra, verifyCancelled)
	}
	if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "hash mismatch") }) {
		t.Fatalf("the hash mismatch found before the cancel is missing: %q", rep.Problems)
	}
}
