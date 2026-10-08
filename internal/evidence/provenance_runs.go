package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNoRuns is returned by CheckedRuns for an artifact that records no image runs: a non-derived
// artifact, or a derived one whose bytes were not copied from the image (decoded content, an empty
// file).
var ErrNoRuns = errors.New("evidence: artifact records no image runs")

func runsBad(format string, a ...any) error {
	return fmt.Errorf("%w: runs: %s", ErrIntegrity, fmt.Sprintf(format, a...))
}

// CheckedRuns returns the image runs of rec: the inline Derived.Runs, or the lines of its runs sidecar,
// which is PROVEN before it is believed. The sidecar must be in the manifest exactly once, be of kind
// runs derived from the same parent, be a regular file that OpenArtifactIn accepts (size equal to its
// record, no link on the path) and hash to the SHA-256 the manifest records, so a same-size swap of its
// bytes is caught. For an artifact of kind unallocated the sidecar is the export's run map and the result
// is its {image_offset, length} runs; the two sidecar formats are not interchangeable. Inline runs
// together with a sidecar are an integrity error. Every refusal of the sidecar wraps ErrIntegrity; an
// artifact with no runs is ErrNoRuns. ix is the manifest read once by the caller (NewManifestIndex);
// CheckedRuns never reads the manifest. The returned runs are not required to be disjoint.
func (c *Case) CheckedRuns(ix *ManifestIndex, rec ManifestRecord) ([]Run, error) {
	d := rec.Source.Derived
	switch {
	case d == nil:
		return nil, ErrNoRuns
	case len(d.Runs) > 0 && d.RunsArtifact != "":
		return nil, runsBad("artifact %q records both inline runs and a runs sidecar", rec.ID)
	case len(d.Runs) > 0:
		return append([]Run(nil), d.Runs...), nil
	case d.RunsArtifact == "":
		return nil, ErrNoRuns
	}
	if ix == nil {
		return nil, errors.New("evidence: CheckedRuns needs a manifest index (see NewManifestIndex)")
	}
	switch n := ix.count[d.RunsArtifact]; {
	case n == 0:
		return nil, runsBad("sidecar %q of artifact %q is not in the manifest", d.RunsArtifact, rec.ID)
	case n > 1:
		return nil, runsBad("sidecar id %q appears %d times in the manifest", d.RunsArtifact, n)
	}
	sc := ix.byID[d.RunsArtifact]
	if sc.Source.Kind != "runs" {
		return nil, runsBad("sidecar %q is of kind %q, not runs", sc.ID, sc.Source.Kind)
	}
	if sc.Source.Derived == nil || sc.Source.Derived.ParentID != d.ParentID {
		return nil, runsBad("sidecar %q is not derived from parent %q", sc.ID, d.ParentID)
	}
	f, _, err := c.OpenArtifactIn(ix, sc.ID)
	if err != nil {
		return nil, runsBad("sidecar %q: %s", sc.ID, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	tee := io.TeeReader(f, h)
	var runs []Run
	if rec.Source.Kind == "unallocated" {
		var m []UnallocRun
		if m, err = ReadUnallocRunMap(tee); err == nil {
			runs = UnallocRunsAsRuns(m)
		}
	} else {
		runs, err = readRunLines(tee)
	}
	if err != nil {
		if errors.Is(err, ErrIntegrity) {
			return nil, fmt.Errorf("sidecar %q: %w", sc.ID, err)
		}
		return nil, runsBad("sidecar %q: %s", sc.ID, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, fmt.Errorf("sidecar %q: %w", sc.ID, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sc.SHA256) {
		return nil, runsBad("sidecar %q hashes to %s, the manifest records %s", sc.ID, got, sc.SHA256)
	}
	return runs, nil
}
