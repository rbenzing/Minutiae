package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/evidence"
)

// The composition of sqlite recover (4G-R5): resolve the inputs, run the typed pass of the parsers that claim
// tables of this database, honor a claim only for a typed job that concluded, then let the generic run record
// everything else. A row ends exactly once: a typed record, a db_row, or a counted surfaced skip.

// typedUnmappedCap lowers the unmapped-locator bound of the typed pass (a test seam; 0 = the default).
var typedUnmappedCap int

type honoredClaim struct {
	Table   string `json:"table"`
	Parser  string `json:"parser"`
	Version string `json:"version"`
}

type notHonoredClaim struct {
	Table  string `json:"table"`
	Parser string `json:"parser"`
	Reason string `json:"reason"`
	// AlsoRecorded: the typed job wrote records (kept, flagged incomplete) for this table, so the generic
	// run's rows for it are a second recording of the same history (4G-R26).
	AlsoRecorded bool `json:"also_recorded_as_incomplete_typed_run"`
}

type typedJobJSON struct {
	Parser            string   `json:"parser"`
	Version           string   `json:"version"`
	ArtifactID        string   `json:"artifact_id"`
	Outcome           string   `json:"outcome"`
	Reason            string   `json:"reason"`
	Records           int64    `json:"records"`
	UnmappedRecovered int64    `json:"unmapped_recovered"`
	SkippedInvalid    int64    `json:"skipped_invalid"`
	UnmappedOverflow  bool     `json:"unmapped_overflow"`
	Claims            []string `json:"claims"`
}

// typedJSON is the "typed" key of the sqlite recover report: null when no claiming parser matched the database.
type typedJSON struct {
	AnalysisID string            `json:"analysis_id"`
	Jobs       []typedJobJSON    `json:"jobs"`
	Honored    []honoredClaim    `json:"honored"`
	NotHonored []notHonoredClaim `json:"not_honored"`
	Warnings   []string          `json:"warnings"`
	// Undecided: a typed job could not decide its claims (the parser could not parse the database now), so
	// the generic run was not started.
	Undecided bool `json:"claims_undecided"`
}

// typedResult is what the typed pass came to, and what the generic run is told about it.
type typedResult struct {
	Matched    bool // some claiming parser matched this database
	Summary    artparse.RunSummary
	Honored    []honoredClaim
	NotHonored []notHonoredClaim
	Warnings   []string
	Keep       [][16]byte
	Link       []dbrecover.NotHonoredClaim
	Undecided  bool
	// Stop: the generic run must not start (integrity failure, a cancelled or failed run, undecided claims).
	Stop bool
}

// errClaimsUndecided is the exit-1 error of a database whose claiming parser could not parse it.
var errClaimsUndecided = errors.New("claims undecided: a parser that claims tables of this database could not parse it, so the generic recovery was not run as if the tables were unclaimed")

func (tr typedResult) json() *typedJSON {
	if !tr.Matched {
		return nil
	}
	t := &typedJSON{
		AnalysisID: tr.Summary.ParseID, Jobs: []typedJobJSON{}, Honored: append([]honoredClaim{}, tr.Honored...),
		NotHonored: append([]notHonoredClaim{}, tr.NotHonored...), Warnings: append([]string{}, tr.Warnings...), Undecided: tr.Undecided,
	}
	for _, j := range tr.Summary.Jobs {
		t.Jobs = append(t.Jobs, typedJobJSON{
			Parser: j.Parser, Version: j.Version, ArtifactID: j.ArtifactID, Outcome: j.Outcome, Reason: j.Reason, Records: j.Records,
			UnmappedRecovered: j.Unmapped, SkippedInvalid: j.SkippedInvalid, UnmappedOverflow: j.UnmappedOverflow,
			Claims: append([]string{}, j.Claims...),
		})
	}
	return t
}

// hasClaims says whether any registered parser claims tables.
func hasClaims(regs []artparse.Registered) bool {
	return slices.ContainsFunc(regs, func(r artparse.Registered) bool { return len(r.Meta().Claims) > 0 })
}

// composeTyped resolves the inputs, runs the typed pass and decides which claims the generic run excludes.
// The returned error is the typed pass's contribution to the exit code (integrity first, a job that is
// incomplete or refused, a stopped run); it is joined with the generic run's by the caller. With no claiming
// parser registered it does nothing (the result is not Matched).
func composeTyped(ctx context.Context, d Deps, c *evidence.Case, in dbrecover.Inputs, allowReingest bool) (typedResult, []dbrecover.ExcludedTable, error) {
	var tr typedResult
	regs, err := d.parserRegistry()
	if err != nil {
		return tr, nil, err
	}
	if !hasClaims(regs) {
		return tr, nil, nil
	}
	res, err := dbrecover.ResolveSiblings(c, in)
	if err != nil {
		tr.Stop = true
		return tr, nil, err
	}
	lim := defaultParseLimits()
	defer limitMemory(lim)()
	h, err := artparse.New(c, regs, artparse.Options{Limits: lim, UnmappedLocatorCap: typedUnmappedCap})
	if err != nil {
		tr.Stop = true
		return tr, nil, err
	}
	sum, runErr := h.RecoverTyped(ctx, artparse.RecoverOptions{
		Selection:        artparse.Selection{Reparse: allowReingest},
		OnlyPrimary:      res.DB,
		ExpectCompanions: map[string]string{"wal": res.WAL, "journal": res.Journal},
	})
	tr.Summary = sum
	tr.Matched = len(sum.Jobs) > 0
	// an integrity failure, a cancel or any run-level failure ends the command here: the typed state is not
	// known, so no claim is honored and the generic run does not start
	terr := typedExit(d, sum, runErr)
	if runErr != nil || sum.Class() == artparse.ClassIntegrity || sum.Stopped != "" || sum.Cancelled {
		tr.Stop = true
		return tr, nil, terr
	}
	if !tr.Matched {
		return tr, nil, nil
	}
	excl := tr.decide()
	if tr.Undecided {
		tr.Stop = true
		return tr, nil, errors.Join(terr, errClaimsUndecided)
	}
	return tr, excl, terr
}

// typedExit is the exit contribution of the typed pass (4G-R6): a job that is incomplete or refused gives 1; an
// unparsed or skipped job does not raise the exit code by itself (it is reported, honors nothing, and the
// generic run covers its tables); integrity wins.
func typedExit(d Deps, sum artparse.RunSummary, runErr error) error {
	f := sum
	f.Jobs = nil
	for _, j := range sum.Jobs {
		if j.Outcome == artparse.OutcomeUnparsed || j.Outcome == artparse.OutcomeSkipped {
			if j.Integrity {
				f.Jobs = append(f.Jobs, j)
			}
			continue
		}
		f.Jobs = append(f.Jobs, j)
	}
	return runOutcome(d, f, runErr)
}

const (
	reasonAlreadyParsed = "already parsed"
	reasonStaleBundle   = "stale bundle"
)

// decide fills Honored, NotHonored, Keep and Link from the jobs and returns the exclusions of the generic run.
func (tr *typedResult) decide() []dbrecover.ExcludedTable {
	var excl []dbrecover.ExcludedTable
	seen := map[[16]byte]struct{}{}
	for _, j := range tr.Summary.Jobs {
		why, honor := honorable(j)
		if honor && len(j.UnmappedLocators) > 0 {
			var fresh [][16]byte
			for _, dg := range j.UnmappedLocators {
				if _, dup := seen[dg]; !dup {
					fresh = append(fresh, dg)
				}
			}
			if len(tr.Keep)+len(fresh) > dbrecover.MaxKeepClaimed {
				honor, why = false, "the unmapped rows of the typed passes exceed the limit the generic run can keep"
			} else {
				for _, dg := range fresh {
					seen[dg] = struct{}{}
				}
				tr.Keep = append(tr.Keep, fresh...)
			}
		}
		if j.Outcome == artparse.OutcomeUnparsed && !strings.HasPrefix(j.Reason, reasonStaleBundle) {
			tr.Undecided = true
		}
		tables := append([]string{}, j.Claims...)
		sort.Strings(tables)
		if honor {
			if j.Outcome == artparse.OutcomeSkipped {
				tr.Warnings = append(tr.Warnings, fmt.Sprintf("%s %s: the claims are honored from an earlier complete typed recovery of this bundle; rows it could not map were warnings then and are not recorded as db_row now (use --allow-reingest to redo both)", j.Parser, j.Version))
			}
			for _, t := range tables {
				excl = append(excl, dbrecover.ExcludedTable{Table: t, Parser: j.Parser, Version: j.Version, Typed: true, TypedAnalysisID: tr.Summary.ParseID})
				tr.Honored = append(tr.Honored, honoredClaim{Table: t, Parser: j.Parser, Version: j.Version})
			}
			continue
		}
		if j.Outcome == artparse.OutcomeSkipped && !strings.HasPrefix(j.Reason, "bundle-differs") {
			// the parser does not apply to this database (or its table is absent): there is no claim to
			// honor or to lose, the generic run records whatever the database holds
			continue
		}
		if j.UnmappedOverflow {
			tr.Warnings = append(tr.Warnings, fmt.Sprintf("%s %s: more unmapped rows than can be tracked: none of its claims is honored, the generic run records the whole of its tables", j.Parser, j.Version))
		}
		if j.SkippedInvalid > 0 {
			tr.Warnings = append(tr.Warnings, fmt.Sprintf("%s %s: %d claimed recovered row(s) were skipped for invalid facts; the job is incomplete and its claims are not honored", j.Parser, j.Version, j.SkippedInvalid))
		}
		also := j.Records > 0
		for _, t := range tables {
			tr.NotHonored = append(tr.NotHonored, notHonoredClaim{Table: t, Parser: j.Parser, Reason: why, AlsoRecorded: also})
		}
		tr.Link = append(tr.Link, dbrecover.NotHonoredClaim{Parser: j.Parser, Version: j.Version, Tables: tables, TypedAnalysisID: tr.Summary.ParseID, Reason: why})
	}
	sort.Slice(tr.Keep, func(a, b int) bool { return string(tr.Keep[a][:]) < string(tr.Keep[b][:]) })
	return excl
}

// honorable says whether a typed job's claims are honored (4G-R5 step 3): it concluded complete with every
// unmapped row tracked, or it was skipped because a complete run of the same recover identity covers the bundle.
// Otherwise it returns why not.
func honorable(j artparse.JobResult) (string, bool) {
	switch {
	case j.UnmappedOverflow:
		return "more unmapped rows than can be tracked", false
	case j.SkippedInvalid > 0:
		return fmt.Sprintf("%d claimed row(s) skipped for invalid facts", j.SkippedInvalid), false
	case j.Outcome == artparse.OutcomeComplete:
		return "", true
	case j.Outcome == artparse.OutcomeSkipped && strings.HasPrefix(j.Reason, reasonAlreadyParsed):
		return "", true
	}
	why := j.Outcome
	if j.Reason != "" {
		why += ": " + j.Reason
	}
	return why, false
}

// printTypedText writes the typed block of the text report: one line per typed job, the claimed tables, and
// what was not honored.
func printTypedText(w io.Writer, tr typedResult) {
	if !tr.Matched {
		return
	}
	for _, j := range tr.Summary.Jobs {
		line := fmt.Sprintf("typed recovery: %s %s %s: %d records, %d unmapped", escapeText(j.Parser), escapeText(j.Version), escapeText(j.Outcome), j.Records, j.Unmapped)
		if j.Reason != "" {
			line += " (" + escapeText(j.Reason) + ")"
		}
		fmt.Fprintln(w, line)
	}
	if len(tr.Honored) > 0 {
		tables := make([]string, len(tr.Honored))
		for i, h := range tr.Honored {
			tables[i] = escapeText(h.Table)
		}
		fmt.Fprintf(w, "claimed tables (typed): %s\n", strings.Join(tables, ", "))
	} else {
		fmt.Fprintln(w, "claimed tables (typed): none honored")
	}
	for _, n := range tr.NotHonored {
		line := fmt.Sprintf("claim not honored: table %s of %s (%s): the generic run records it", escapeText(n.Table), escapeText(n.Parser), escapeText(n.Reason))
		if n.AlsoRecorded {
			line += "; also recorded as an incomplete typed run"
		}
		fmt.Fprintln(w, line)
	}
	for _, x := range tr.Warnings {
		fmt.Fprintf(w, "warning: %s\n", escapeText(x))
	}
	if tr.Undecided {
		fmt.Fprintln(w, "claims undecided: a parser that claims tables of this database could not parse it; the generic recovery was not run")
	}
}

// printClaims is the --list output: which parser claims which tables of the database, and nothing written.
func printClaims(w io.Writer, claims []artparse.Claimed) {
	type key struct{ parser, version string }
	by := map[key][]artparse.Claimed{}
	var keys []key
	for _, cl := range claims {
		k := key{cl.RecoverParser, cl.Version}
		if _, ok := by[k]; !ok {
			keys = append(keys, k)
		}
		by[k] = append(by[k], cl)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].parser != keys[b].parser {
			return keys[a].parser < keys[b].parser
		}
		return keys[a].version < keys[b].version
	})
	for _, k := range keys {
		var tables []string
		reason := ""
		for _, cl := range by[k] {
			tables = append(tables, escapeText(cl.Table))
			if cl.Unparsed {
				reason = cl.Reason
			}
		}
		line := fmt.Sprintf("claimed by %s %s: %s", escapeText(k.parser), escapeText(k.version), strings.Join(tables, ", "))
		if reason != "" {
			line += fmt.Sprintf(" [claims undecided: the parser cannot parse this artifact now: %s]", escapeText(reason))
		}
		fmt.Fprintln(w, line)
	}
}

type claimedJSON struct {
	Table       string `json:"table"`
	Parser      string `json:"parser"`
	Version     string `json:"version"`
	Undecided   bool   `json:"claims_undecided,omitempty"`
	Reason      string `json:"reason,omitempty"`
	LiveParser  string `json:"live_parser"`
	RecoverHash string `json:"recover_hash"`
}

func claimsJSON(claims []artparse.Claimed) []claimedJSON {
	out := []claimedJSON{}
	for _, cl := range claims {
		out = append(out, claimedJSON{
			Table: cl.Table, Parser: cl.RecoverParser, Version: cl.Version, Undecided: cl.Unparsed, Reason: cl.Reason,
			LiveParser: cl.Parser, RecoverHash: cl.RecoverHash,
		})
	}
	return out
}

// listClaims reads which tables of the database registered parsers claim (--list: nothing is written).
func listClaims(ctx context.Context, d Deps, c *evidence.Case, in dbrecover.Inputs) ([]artparse.Claimed, error) {
	regs, err := d.parserRegistry()
	if err != nil {
		return nil, err
	}
	if !hasClaims(regs) {
		return nil, nil
	}
	res, err := dbrecover.ResolveSiblings(c, in)
	if err != nil {
		return nil, err
	}
	h, err := artparse.New(c, regs, artparse.Options{Limits: defaultParseLimits()})
	if err != nil {
		return nil, err
	}
	return h.ClaimedTables(ctx, res.DB)
}

// printTypedOnly reports the typed pass when the generic run did not produce a report of its own (it was not
// started, or was refused): text lines, or a JSON report that holds the typed object.
func printTypedOnly(w io.Writer, tr typedResult, asJSON bool) error {
	if !tr.Matched {
		return nil
	}
	if asJSON {
		return writeJSON(w, sqliteReportJSON{
			Inputs: []sqliteInputJSON{}, Siblings: map[string]sqliteSiblingJSON{}, Counts: countsJSON2(dbrecover.Counts{}),
			NotStarted: "the generic recovery was not run", Typed: tr.json(),
		})
	}
	printTypedText(w, tr)
	return nil
}
