package examine

import (
	"errors"
	"maps"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Summary is the outcome of an extraction or an unallocated-space export.
type Summary struct {
	AnalysisID string
	Files      int   // finished (complete) artifacts of the requested content
	Bytes      int64 // bytes of those artifacts
	Skipped    int   // analysis.warning entries written
	// Artifacts lists every artifact the analysis wrote, runs sidecars and
	// incomplete ones included, in creation order.
	Artifacts []evidence.ManifestRecord
}

// analysis is one audited derived-artifact run. Its id doubles as the
// acquisition directory of the artifacts it writes.
type analysis struct {
	c        *evidence.Case
	deviceID string
	sum      Summary
}

// warn records an analysis.warning for path and counts it as skipped.
func (a *analysis) warn(path, reason string) error {
	a.sum.Skipped++
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "path": path, "reason": reason,
	})
	return err
}

// add keeps a written artifact record (when one exists) in the summary.
func (a *analysis) add(rec evidence.ManifestRecord) {
	if rec.ID != "" {
		a.sum.Artifacts = append(a.sum.Artifacts, rec)
	}
}

// runAnalysis brackets fn with analysis.start and analysis.end (or
// analysis.error) audit entries sharing one analysis id. details are added to
// the analysis.start entry (parent_id, partition, paths ...). The summary is
// returned even when fn fails, so partial artifacts stay discoverable.
func runAnalysis(c *evidence.Case, deviceID, op string, details map[string]any, fn func(a *analysis) error) (Summary, error) {
	a := &analysis{c: c, deviceID: deviceID, sum: Summary{AnalysisID: evidence.NewAcquisitionID(time.Now())}}
	d := maps.Clone(details)
	if d == nil {
		d = map[string]any{}
	}
	d["analysis_id"], d["op"] = a.sum.AnalysisID, op
	if _, err := c.Audit.Append("analysis.start", deviceID, d); err != nil {
		return a.sum, err
	}
	if err := fn(a); err != nil {
		_, aerr := c.Audit.Append("analysis.error", deviceID, map[string]any{
			"analysis_id": a.sum.AnalysisID, "error": err.Error(),
			"files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
		})
		return a.sum, errors.Join(err, aerr)
	}
	_, err := c.Audit.Append("analysis.end", deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
	})
	return a.sum, err
}
