package examine

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// zeroImage is a reader of size bytes of zeros that counts what is read from it.
type zeroImage struct {
	size  int64
	bytes int64
	calls int
}

func (z *zeroImage) ReadAt(p []byte, off int64) (int, error) {
	z.calls++
	if off >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.size-off))
	clear(p[:n])
	z.bytes += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// C2: 10,000 candidates over one zero region cost one scan of it, and each uniform one frees its budget.
func TestUniformVerdictIsCachedPerRunList(t *testing.T) {
	const region = 1 << 20
	img := &zeroImage{size: 64 << 20}
	b, err := newBudget(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ad := newAdmission(b, img, false)
	for i := range 10_000 {
		c := &PlannedCandidate{Size: region, Runs: []evidence.Run{{Offset: 4096, Length: region}}}
		stop, err := ad.admit(context.Background(), c)
		if err != nil || stop != "" {
			t.Fatalf("candidate %d: stop %q err %v", i, stop, err)
		}
		if c.Skip != "uniform" || c.Content != "uniform" {
			t.Fatalf("candidate %d: skip %q content %q", i, c.Skip, c.Content)
		}
	}
	if img.bytes > region+uniformChunk {
		t.Errorf("read %d bytes for one region of %d (calls %d): the verdict is not cached", img.bytes, region, img.calls)
	}
	if b.files != 0 || b.bytes != 0 {
		t.Errorf("budget holds %d files %d bytes after uniform candidates: a uniform result must free its budget", b.files, b.bytes)
	}
}

// C2: with distinct run lists (no cache hit) the scanned bytes stop at four times the byte limit.
func TestUniformScanBytesAreCapped(t *testing.T) {
	const maxBytes, size = 1 << 20, 512 << 10
	img := &zeroImage{size: 64 << 20}
	b, err := newBudget(0, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	ad := newAdmission(b, img, false)
	var stopped string
	var limited int
	for i := range 40 {
		c := &PlannedCandidate{Size: size, Runs: []evidence.Run{{Offset: int64(i+1) * 1024, Length: size}}}
		stop, err := ad.admit(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if stop != "" {
			stopped = stop
			if c.Skip != "limit" {
				t.Errorf("refused candidate has skip %q, want limit", c.Skip)
			}
			limited++
			break
		}
	}
	if stopped != "max-scan-bytes" || limited != 1 {
		t.Fatalf("stop = %q after %d refusals, want max-scan-bytes", stopped, limited)
	}
	if img.bytes > 4*maxBytes {
		t.Errorf("scanned %d bytes, cap is %d", img.bytes, 4*maxBytes)
	}
	if img.bytes < 4*maxBytes-size {
		t.Errorf("scanned only %d bytes: the cap stopped the scan too early", img.bytes)
	}
}

// A candidate that is not uniform stays admitted and counted; a short read is an error, not uniform.
func TestAdmitKeepsNonUniformAndKeepUniformFlag(t *testing.T) {
	img := &zeroImage{size: 1 << 20}
	b, _ := newBudget(0, 0)
	keep := newAdmission(b, img, true)
	c := &PlannedCandidate{Size: 4096, Runs: []evidence.Run{{Offset: 0, Length: 4096}}}
	if stop, err := keep.admit(context.Background(), c); stop != "" || err != nil || c.Skip != "" || c.Content != "uniform" {
		t.Fatalf("keep-uniform: stop %q err %v skip %q content %q", stop, err, c.Skip, c.Content)
	}
	if b.files != 1 || b.bytes != 4096 {
		t.Errorf("budget %d files %d bytes, want the kept candidate counted", b.files, b.bytes)
	}
	short := &PlannedCandidate{Size: 4096, Runs: []evidence.Run{{Offset: 1<<20 - 100, Length: 4096}}}
	if _, err := keep.admit(context.Background(), short); err == nil {
		t.Error("a run past the end of the image must be an error, never uniform")
	}
}

// A negative size reaching admission is a bug upstream: an internal error, never a budget reason.
func TestAdmitNegativeSizeIsInternalError(t *testing.T) {
	b, _ := newBudget(0, 0)
	ad := newAdmission(b, &zeroImage{size: 1 << 20}, false)
	c := &PlannedCandidate{Size: -1, Runs: []evidence.Run{{Offset: 0, Length: 1}}}
	stop, err := ad.admit(context.Background(), c)
	if !errors.Is(err, errInvalidSize) || stop != "" || c.Skip != "" || b.files != 0 {
		t.Errorf("stop %q err %v skip %q files %d", stop, err, c.Skip, b.files)
	}
}
