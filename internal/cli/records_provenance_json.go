package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// JSON mapping of a records.Provenance for `records show --json`. Strings are raw (JSON escapes
// them); only the text form goes through printable.

type provJSON struct {
	Status             string            `json:"status"`
	ReachedRoot        bool              `json:"reached_root"`
	Chain              []provHopJSON     `json:"chain"`
	Recovery           *provRecoveryJSON `json:"recovery"`
	Offset             provOffsetJSON    `json:"offset"`
	Notes              []string          `json:"notes"`
	Problems           []provProblemJSON `json:"problems"`
	ProblemsSuppressed int               `json:"problems_suppressed"`
}

type provHopJSON struct {
	Index    int                     `json:"index"`
	Artifact evidence.ManifestRecord `json:"artifact"`
	Link     string                  `json:"link"`
	Audit    provAuditJSON           `json:"audit"`
	Segments provSegmentsJSON        `json:"segments"`
}

type provAuditJSON struct {
	State  string `json:"state"`
	Seq    int64  `json:"seq"`
	Detail string `json:"detail"`
}

type provSegmentsJSON struct {
	Total  int               `json:"total"`
	Bad    int               `json:"bad"`
	Listed []provSegmentJSON `json:"listed"`
}

type provSegmentJSON struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
}

type provRecoveryJSON struct {
	ArtifactID    string                `json:"artifact_id"`
	Hops          int                   `json:"hops"`
	Recovery      evidence.Recovery     `json:"recovery"`
	MinConfidence *int                  `json:"min_confidence"`
	DeclaredRuns  *provDeclaredRunsJSON `json:"declared_runs"`
}

type provDeclaredRunsJSON struct {
	ArtifactID string `json:"artifact_id"`
	State      string `json:"state"`
	Detail     string `json:"detail"`
}

type provOffsetJSON struct {
	State       string              `json:"state"`
	Reason      string              `json:"reason"`
	Coordinates string              `json:"coordinates"`
	Segments    int                 `json:"segments"`
	Hops        []provOffsetHopJSON `json:"hops"`
	Image       []provExtentJSON    `json:"image"`
}

type provOffsetHopJSON struct {
	From           string           `json:"from"`
	To             string           `json:"to"`
	Runs           string           `json:"runs"`
	Total          int              `json:"total"`
	Truncated      bool             `json:"truncated"`
	RunsExceedSize bool             `json:"runs_exceed_size"`
	Extents        []provExtentJSON `json:"extents"`
}

type provExtentJSON struct {
	ArtifactOffset int64 `json:"artifact_offset"`
	Length         int64 `json:"length"`
	ImageOffset    int64 `json:"image_offset"`
	Hole           bool  `json:"hole"`
}

type provProblemJSON struct {
	Kind   string `json:"kind"`
	Hop    int    `json:"hop"`
	Detail string `json:"detail"`
}

func provExtentsJSON(es []records.ImageExtent) []provExtentJSON {
	out := make([]provExtentJSON, len(es))
	for i, e := range es {
		out[i] = provExtentJSON{ArtifactOffset: e.ArtifactOffset, Length: e.Length, ImageOffset: e.ImageOffset, Hole: e.Hole}
	}
	return out
}

// provIsRecovered is the one rule for "this record is recovered data", used by the text status
// line and the JSON status: a recovery description along the chain, or a recovered row.
func provIsRecovered(p records.Provenance, row records.Row) bool {
	return p.Recovery != nil || row.Recovered
}

const (
	statusLive      = "live"
	statusRecovered = "recovered"
	statusUnknown   = "unknown"
)

// provStatusKind is the one status rule of the text and the JSON: recovered when the chain or the
// row says so; else unknown when the chain could not be resolved (a missing, ambiguous, cyclic or
// too-deep parent hides what it derives from); else live.
func provStatusKind(p records.Provenance, row records.Row) string {
	switch {
	case provIsRecovered(p, row):
		return statusRecovered
	case !p.ReachedRoot:
		return statusUnknown
	}
	return statusLive
}

func provenanceJSON(p records.Provenance, row records.Row) provJSON {
	status := provStatusKind(p, row)
	j := provJSON{
		Status: status, ReachedRoot: p.ReachedRoot,
		Chain: make([]provHopJSON, len(p.Chain)),
		Offset: provOffsetJSON{
			State: string(p.Offset.State), Reason: p.Offset.Reason, Coordinates: p.Offset.Coordinates, Segments: p.Offset.Segments,
			Hops: make([]provOffsetHopJSON, len(p.Offset.Hops)), Image: provExtentsJSON(p.Offset.Image),
		},
		Notes: append([]string{}, p.Notes...), Problems: make([]provProblemJSON, len(p.Problems)), ProblemsSuppressed: p.ProblemsSuppressed,
	}
	for i, h := range p.Chain {
		hj := provHopJSON{
			Index: i, Artifact: h.Artifact, Link: string(h.Link),
			Audit:    provAuditJSON{State: string(h.Audit.State), Seq: h.Audit.Seq, Detail: h.Audit.Detail},
			Segments: provSegmentsJSON{Total: h.SegmentsTotal, Bad: h.SegmentsBad, Listed: make([]provSegmentJSON, len(h.Segments))},
		}
		for k, s := range h.Segments {
			hj.Segments.Listed[k] = provSegmentJSON{ID: s.ID, SHA256: s.SHA256, State: s.State}
		}
		j.Chain[i] = hj
	}
	if v := p.Recovery; v != nil {
		rj := &provRecoveryJSON{ArtifactID: v.ArtifactID, Hops: v.Hops, Recovery: v.Recovery, MinConfidence: v.MinConfidence}
		if dr := v.DeclaredRuns; dr != nil {
			rj.DeclaredRuns = &provDeclaredRunsJSON{ArtifactID: dr.ArtifactID, State: dr.State, Detail: dr.Detail}
		}
		j.Recovery = rj
	}
	for i, h := range p.Offset.Hops {
		j.Offset.Hops[i] = provOffsetHopJSON{
			From: h.From, To: h.To, Runs: h.Runs, Total: h.Total, Truncated: h.Truncated,
			RunsExceedSize: h.RunsExceedSize, Extents: provExtentsJSON(h.Extents),
		}
	}
	for i, pr := range p.Problems {
		j.Problems[i] = provProblemJSON{Kind: string(pr.Kind), Hop: pr.Hop, Detail: pr.Detail}
	}
	return j
}

// provenanceFailure is the error `records show` returns AFTER it printed the whole provenance when
// the chain is damaged: an integrity error (exit 4) that counts the listed and the suppressed
// problems. A chain with notes only is not a failure.
func provenanceFailure(p records.Provenance, caseDir string) error {
	if p.OK() {
		return nil
	}
	return fmt.Errorf("%w: provenance check found %d problem(s); run: minutiae case verify --case %s",
		evidence.ErrIntegrity, len(p.Problems)+p.ProblemsSuppressed, shellQuote(escapeText(caseDir)))
}

// joinIntegrity returns the print error alone when the chain is sound, and the integrity failure
// joined with it when it is not: a failed print never masks an integrity failure (exit stays 4).
func joinIntegrity(integrity, printErr error) error {
	if integrity == nil {
		return printErr
	}
	return errors.Join(integrity, printErr)
}

// shellQuote makes s one shell word: unchanged when it only holds safe characters, otherwise in
// single quotes with each embedded single quote written as quote-backslash-quote-quote.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, unsafeForShell) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafeForShell(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("_-./:@%+=,", r)
}
