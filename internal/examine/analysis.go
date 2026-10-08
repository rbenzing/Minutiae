package examine

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
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
	known int                // len(Info().Warnings) at the last watchFS/syncFS
	after string             // last path extracted, named by filesystem warnings written after it

	warnEntries int // analysis.warning entries asked for (written or suppressed)
	suppressed  int // of those, how many were not written

	extra map[string]any // setEnd: extra details of analysis.end and analysis.error
}

// warn records an analysis.warning for path and counts it as skipped.
func (a *analysis) warn(path, reason string) error { return a.warnWith(path, reason, nil) }

// warnWith is warn plus extra audit keys (an entry id, a detail text). The base keys (analysis_id, path,
// reason) always win over extra.
func (a *analysis) warnWith(path, reason string, extra map[string]any) error {
	a.sum.Skipped++
	if len(a.sum.Warnings) < maxWarnings {
		a.sum.Warnings = append(a.sum.Warnings, Warning{Path: path, Reason: reason})
	}
	d := maps.Clone(extra)
	if d == nil {
		d = map[string]any{}
	}
	d["analysis_id"], d["path"], d["reason"] = a.sum.AnalysisID, path, reason
	return a.appendWarning(d)
}

// appendWarning writes one analysis.warning entry unless the analysis already wrote warningEntryCap of
// them: the first one past the cap writes a single "further warnings suppressed" entry and the rest are
// only counted (analysis.end carries warnings_suppressed). The counts of the summary stay exact.
func (a *analysis) appendWarning(d map[string]any) error {
	a.warnEntries++
	if a.warnEntries > warningEntryCap {
		a.suppressed++
		if a.warnEntries > warningEntryCap+1 {
			return nil
		}
		d = map[string]any{
			"analysis_id": a.sum.AnalysisID, "path": "", "reason": "further warnings suppressed",
			"warning": fmt.Sprintf("more than %d warnings: the rest are counted in analysis.end (warnings_suppressed), not written one by one", warningEntryCap),
		}
	}
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, d)
	return err
}

// setEnd adds a detail to the analysis.end (or analysis.error) entry. The keys the entry always carries
// (analysis_id, files, bytes, skipped, and error for analysis.error) are never overridden.
func (a *analysis) setEnd(key string, v any) {
	if a.extra == nil {
		a.extra = map[string]any{}
	}
	a.extra[key] = v
}

// endDetails is the base details of analysis.end/error with the extras beneath them.
func (a *analysis) endDetails(base map[string]any) map[string]any {
	d := maps.Clone(a.extra)
	if d == nil {
		d = map[string]any{}
	}
	maps.Copy(d, base)
	if a.suppressed > 0 {
		d["warnings_suppressed"] = a.suppressed
	}
	return d
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
	warnings := fsys.Info().Warnings
	a.known = len(warnings)
	for _, w := range warnings {
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
	// Filesystem warnings only grow, so an unchanged count means nothing new:
	// skip the per-warning work (it runs after every extracted file).
	warnings := a.fsys.Info().Warnings
	if len(warnings) == a.known {
		return nil
	}
	for _, w := range warnings {
		if a.seen[w] {
			continue
		}
		a.seen[w] = true
		a.sum.FSWarnings++
		if err := a.fsWarning("filesystem", w); err != nil {
			return err
		}
	}
	a.known = len(warnings)
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
	return a.appendWarning(details)
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
		_, aerr := c.Audit.Append("analysis.error", deviceID, a.endDetails(map[string]any{
			"analysis_id": a.sum.AnalysisID, "error": err.Error(),
			"files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
		}))
		return a.sum, errors.Join(err, aerr)
	}
	_, err = c.Audit.Append("analysis.end", deviceID, a.endDetails(map[string]any{
		"analysis_id": a.sum.AnalysisID, "files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
	}))
	return a.sum, err
}

// snapshotCount is the number of usable snapshots a synthetic .snapshots
// directory reports (attribute "snapshots"); 0 when it is absent or unparsable.
func snapshotCount(e filesys.Entry) int {
	for _, kv := range e.Attrs {
		if kv.Key == "snapshots" {
			n, err := strconv.Atoi(kv.Value)
			if err != nil || n < 0 {
				return 0
			}
			return n
		}
	}
	return 0
}

// snapshotsSkippedDetails is the audit entry for a recursive operation that did
// not descend into the .snapshots directory p of a volume with n snapshots; it
// is nil when there are none.
func snapshotsSkippedDetails(p string, e filesys.Entry) map[string]any {
	n := snapshotCount(e)
	if n == 0 {
		return nil
	}
	reason := fmt.Sprintf("the volume has %d snapshot(s) that are present and not included in this recursive operation; use --snapshot (image ls) or address a path below %s", n, p)
	return map[string]any{"source": "examine", "path": p, "reason": reason, "warning": reason, "snapshots": n}
}

// noteSnapshotsSkipped writes one analysis.warning (source "examine") for the
// .snapshots directory e at p when its volume has snapshots. It is a note, not a
// skipped file: Summary.Skipped is unchanged.
func (a *analysis) noteSnapshotsSkipped(p string, e filesys.Entry) error {
	d := snapshotsSkippedDetails(p, e)
	if d == nil {
		return nil
	}
	d["analysis_id"] = a.sum.AnalysisID
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, d)
	return err
}

// warningEntryCap is how many analysis.warning entries one analysis writes before it writes one
// "further warnings suppressed" entry and only counts the rest (as records.Writer.Warn does).
var warningEntryCap = 10_000
