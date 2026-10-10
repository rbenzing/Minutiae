package dbrecover

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Op and Algorithm are the audited names of this analysis.
const (
	Op        = "sqlite-recover"
	Algorithm = "minutiae-sqlite-recover/1"
)

// analysis is the audit bracket of one run: analysis.start, then analysis.end
// or analysis.error, mirroring examine.runAnalysis's action and key names.
type analysis struct {
	c        *evidence.Case
	deviceID string
	id       string
	parentID string
}

func (a *analysis) base() map[string]any {
	d := map[string]any{"analysis_id": a.id, "op": Op}
	if a.parentID != "" {
		d["parent_id"] = a.parentID
	}
	return d
}

func (a *analysis) start(details map[string]any) error {
	d := a.base()
	maps.Copy(d, details)
	_, err := a.c.Audit.Append("analysis.start", a.deviceID, d)
	return err
}

// finish appends analysis.end (cause nil) or analysis.error. The error entry
// carries the same details, so a refused or failed run still says what was
// examined. It returns cause joined with an audit failure.
func (a *analysis) finish(cause error, details map[string]any) error {
	d := a.base()
	maps.Copy(d, details)
	if cause == nil {
		_, err := a.c.Audit.Append("analysis.end", a.deviceID, d)
		return err
	}
	d["error"] = cause.Error()
	d["reason"] = reasonToken(cause)
	_, err := a.c.Audit.Append("analysis.error", a.deviceID, d)
	return errors.Join(cause, err)
}

func reasonToken(err error) string {
	// an input that changed is named first: no other error hides an integrity failure
	if errors.Is(err, ErrInputChanged) {
		return "input-changed"
	}
	var re *RefusalError
	if errors.As(err, &re) {
		return re.Reason
	}
	if errors.Is(err, ErrAmbiguousSibling) {
		return ReasonAmbiguousSibling
	}
	return "error"
}

func inputDetails(s *sealedSet) []any {
	var out []any
	for _, in := range s.inputs() {
		r := in.Ref
		var capv any
		if r.Cap != nil {
			capv = int64(*r.Cap)
		}
		out = append(out, map[string]any{
			"role": r.Role, "id": r.ArtifactID, "sha256": r.SHA256, "size": r.Size,
			"recovered": r.Recovered, "incomplete": r.Incomplete, "cap": capv,
		})
	}
	return out
}

func siblingDetails(r SiblingReport) map[string]any {
	one := func(x SiblingResult) map[string]any {
		d := map[string]any{"status": string(x.Status), "searched_by": x.SearchedBy, "explicit_choice": x.Status == SiblingExplicit}
		if x.ArtifactID != "" {
			d["artifact_id"] = x.ArtifactID
		}
		if len(x.Candidates) > 0 {
			d["candidates"] = toAny(x.Candidates)
		}
		if len(x.OtherAcquisition) > 0 {
			d["other_acquisition"] = toAny(x.OtherAcquisition)
		}
		if x.Unused != "" {
			d["unused"] = x.Unused
		}
		return d
	}
	return map[string]any{"wal": one(r.WAL), "journal": one(r.Journal)}
}

func optionDetails(o Options, narrowed []string) map[string]any {
	d := map[string]any{
		"min_confidence": int64(o.MinConfidence), "max_records": o.MaxRecords, "max_bytes": o.MaxBytes,
		"max_payload_bytes": o.MaxPayloadBytes, "max_body_bytes": o.MaxBodyBytes,
		"allow_reingest": o.AllowReingest, "batch_rows": int64(o.BatchRows),
		"narrowed": toAny(narrowed),
	}
	if len(o.Methods) > 0 {
		d["methods"] = toAny(o.Methods)
	}
	return d
}

func excludedDetails(x []ExcludedTable) []any {
	out := []any{}
	for _, e := range x {
		out = append(out, map[string]any{"table": e.Table, "parser": e.Parser, "version": e.Version})
	}
	return out
}

func startDetails(set *sealedSet, rep SiblingReport, o Options, narrowed []string) map[string]any {
	return map[string]any{
		"inputs": inputDetails(set), "siblings": siblingDetails(rep),
		"options": optionDetails(o, narrowed), "excluded_tables": excludedDetails(o.ExcludeTables),
		"algorithm": Algorithm, "rules_hash": RulesHash(),
		"parser": map[string]any{"name": ParserName, "version": version, "hash": RulesHash()},
	}
}

func sortedStrings(s []string) []any {
	c := slices.Clone(s)
	slices.Sort(c)
	return toAny(c)
}

func int64Map(m map[string]int64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func countsDetails(c Counts) map[string]any {
	ranges := map[string]any{}
	for m, r := range c.ConfidenceRange {
		ranges[m] = map[string]any{"min": int64(r.Min), "max": int64(r.Max)}
	}
	return map[string]any{
		"written": c.Written, "by_method": int64Map(c.ByMethod), "skipped_by_reason": int64Map(c.SkippedByReason),
		"duplicate_of_live": c.DuplicateOfLive, "cells_rejected": c.CellsRejected, "interior_skipped": c.InteriorSkipped,
		"relation_unknown": c.RelationUnknown, "fit_only": c.FitOnly, "freelist_pages": c.FreePages,
		"freelist_pages_zeroed": c.FreePagesZeroed, "pages_by_origin": int64Map(c.PagesByOrigin),
		"slack_copies_of_live": c.SlackCopiesOfLive, "slack_unparsed_bytes": c.SlackUnparsedBytes, "slack_dropped_crossed": c.SlackDroppedCrossed,
		"slack_dropped_overlap": c.SlackDroppedOverlap, "slack_cross_page_equal": c.SlackCrossPageEqual,
		"slack_work_capped_pages": c.SlackWorkCapped, "slack_row_capped": c.SlackRowCapped, "fit_equality_capped": c.FitEqualityCapped,
		"recovery_cap_unknown": c.RecoveryCapUnknown,
		"limits_hit":           sortedStrings(c.LimitsHit), "narrowed": toAny(c.Narrowed), "library_warnings": int64(c.LibraryWarnings),
		"confidence_range": ranges, "range_outside": c.RangeOutside,
	}
}

// warningText is the path and reason a library warning is audited under.
func warningText(w sqlitefile.Warning) (path, reason string) {
	if w.File != 0 {
		path = fileName(w.File)
		if w.Page > 0 {
			path += fmt.Sprintf(" page %d", w.Page)
		}
	}
	return path, fmt.Sprintf("library warning [%s]: %s (offset %d)", w.Code, w.Msg, w.Offset)
}
