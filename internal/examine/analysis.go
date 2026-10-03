package examine

import (
	"errors"
	"maps"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// Summary is the outcome of an extraction or an unallocated-space export.
type Summary struct {
	AnalysisID string
	Files      int   // finished (complete) artifacts of the requested content
	Bytes      int64 // bytes of those artifacts
	Skipped    int   // analysis.warning entries written (authoritative; Warnings may be shorter)
	// Warnings lists the first maxWarnings skips (path and reason), in order.
	Warnings []Warning
	// Artifacts lists every artifact the analysis wrote, runs sidecars and
	// incomplete ones included, in creation order.
	Artifacts []evidence.ManifestRecord
	// FSWarnings counts the filesystem warnings that appeared during the run
	// (each also an analysis.warning with source "filesystem"). Warnings the
	// filesystem already had at the start are audited but not counted.
	FSWarnings int
}

// Warning is one skipped path and why.
type Warning struct{ Path, Reason string }

// maxWarnings caps Summary.Warnings; the audit log has every warning.
const maxWarnings = 100

// analysis is one audited derived-artifact run. Its id doubles as the
// acquisition directory of the artifacts it writes.
type analysis struct {
	c        *evidence.Case
	deviceID string
	sum      Summary

	fsys  filesys.FileSystem // watched by watchFS, nil when none
	seen  map[string]bool    // filesystem warnings already in the audit log
	after string             // last path extracted, named by filesystem warnings written after it
}

// warn records an analysis.warning for path and counts it as skipped.
func (a *analysis) warn(path, reason string) error {
	a.sum.Skipped++
	if len(a.sum.Warnings) < maxWarnings {
		a.sum.Warnings = append(a.sum.Warnings, Warning{Path: path, Reason: reason})
	}
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "path": path, "reason": reason,
	})
	return err
}

// Filesystem warnings are de-duplicated by text (an anomaly that repeats is
// written once per analysis) and each reader caps its own Info().Warnings, so
// anomalies past that cap are invisible here. A warning is written after the
// artifact whose processing triggered it, not before: it is seen only once
// that work is done, and carries "after" (the last extracted path) when known.

// watchFS starts forwarding the warnings of fsys to the audit log. The ones it
// already has (from opening it, or from earlier work on it) are written once
// as analysis.warning with source "filesystem-open", so the derived artifacts'
// audit context records the state of the filesystem they came from; they are
// not counted in the summary. Later calls of syncFS write the new ones.
func (a *analysis) watchFS(fsys filesys.FileSystem) error {
	a.fsys, a.seen = fsys, map[string]bool{}
	for _, w := range fsys.Info().Warnings {
		if a.seen[w] {
			continue
		}
		a.seen[w] = true
		if err := a.fsWarning("filesystem-open", w); err != nil {
			return err
		}
	}
	return nil
}

// syncFS writes each filesystem warning that appeared since watchFS (or the
// previous syncFS) as analysis.warning with source "filesystem" and counts it
// in Summary.FSWarnings. They are not skips: Skipped and Warnings are untouched.
func (a *analysis) syncFS() error {
	if a.fsys == nil {
		return nil
	}
	for _, w := range a.fsys.Info().Warnings {
		if a.seen[w] {
			continue
		}
		a.seen[w] = true
		a.sum.FSWarnings++
		if err := a.fsWarning("filesystem", w); err != nil {
			return err
		}
	}
	return nil
}

func (a *analysis) fsWarning(source, text string) error {
	details := map[string]any{
		"analysis_id": a.sum.AnalysisID, "source": source, "warning": text,
		"path": "filesystem", "reason": text, // the shape of every other analysis.warning
	}
	if a.after != "" && source == "filesystem" {
		details["after"] = a.after
	}
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, details)
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
	err := fn(a)
	if serr := a.syncFS(); serr != nil { // warnings of work done before a failure are kept too
		err = errors.Join(err, serr)
	}
	if err != nil {
		_, aerr := c.Audit.Append("analysis.error", deviceID, map[string]any{
			"analysis_id": a.sum.AnalysisID, "error": err.Error(),
			"files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
		})
		return a.sum, errors.Join(err, aerr)
	}
	_, err = c.Audit.Append("analysis.end", deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
	})
	return a.sum, err
}
