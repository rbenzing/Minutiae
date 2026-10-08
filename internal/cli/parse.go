package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
)

// Everything a plan or a run prints that came from a case or a device (artifact ids and paths, logical
// paths, snapshot names, reasons, notes, panic values) reaches the terminal only through printable or
// escapeText. --json keeps the strings as given.

// parseLimitsHook lets a test adjust the limits of a run (the grace period of an abandonment test).
var parseLimitsHook func(*parse.Limits)

const discoveryNote = "note: discovery trusts the manifest's source fields; run `case verify` first to check them against the audit log"

func newParseCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("parse", "List, plan and run the artifact parsers")
	cmd.AddCommand(newParseListCmd(d, opts), newParsePlanCmd(d, opts), newParseRunCmd(d, opts))
	return cmd
}

// registry returns the parsers of this build, or the injected ones.
func (d Deps) parserRegistry() ([]artparse.Registered, error) {
	if d.ParserRegistry != nil {
		return d.ParserRegistry()
	}
	var out []artparse.Registered
	for _, p := range parsers.All() {
		hash, ok := parsers.HashOf(p)
		if !ok {
			return nil, errors.New("a compiled-in parser has no generated source hash (run the parser hash generator)")
		}
		r, err := artparse.Register(p, hash)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// list

type parserListJSON struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Title     string          `json:"title"`
	Hash      string          `json:"hash"`
	Platforms []string        `json:"platforms"`
	Emits     []emitJSON      `json:"emits"`
	Inputs    []inputSpecJSON `json:"inputs"`
}

type emitJSON struct {
	Type           string `json:"type"`
	PayloadVersion int    `json:"payload_version"`
}

type inputSpecJSON struct {
	Role       string   `json:"role"`
	Globs      []string `json:"globs"`
	Companions []string `json:"companions"`
	Required   bool     `json:"required"`
}

func listJSON(r artparse.Registered) parserListJSON {
	m, id := r.Meta(), r.Identity()
	j := parserListJSON{Name: m.Name, Version: m.Version, Title: m.Title, Hash: id.Hash, Platforms: append([]string{}, m.Platforms...), Emits: []emitJSON{}, Inputs: []inputSpecJSON{}}
	for _, e := range m.Emits {
		j.Emits = append(j.Emits, emitJSON{Type: e.Type, PayloadVersion: e.PayloadVersion})
	}
	for _, in := range m.Inputs {
		j.Inputs = append(j.Inputs, inputSpecJSON{Role: in.Role, Globs: append([]string{}, in.Globs...), Companions: append([]string{}, in.Companions...), Required: in.Required})
	}
	return j
}

func newParseListCmd(d Deps, opts *rootOptions) *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the parsers compiled into this build",
		Long:  "List the parsers compiled into this build: name, version, source hash, record types with their payload\nversions, platforms and input globs. Opens no case and appends nothing.",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if platform != "" && platform != parse.PlatformAndroid && platform != parse.PlatformIOS {
				return usageErrorf("--platform must be android or ios, got %s", printable(platform))
			}
			regs, err := d.parserRegistry()
			if err != nil {
				return err
			}
			var shown []artparse.Registered
			for _, r := range regs {
				if platform == "" || slices.Contains(r.Meta().Platforms, platform) {
					shown = append(shown, r)
				}
			}
			if opts.json {
				out := []parserListJSON{}
				for _, r := range shown {
					out = append(out, listJSON(r))
				}
				return writeJSON(d.Out, out)
			}
			tw := tabwriter.NewWriter(d.Out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tHASH\tRECORDS\tPLATFORMS\tINPUTS")
			for _, r := range shown {
				j := listJSON(r)
				var emits, ins []string
				for _, e := range j.Emits {
					emits = append(emits, fmt.Sprintf("%s@v%d", e.Type, e.PayloadVersion))
				}
				for _, in := range j.Inputs {
					ins = append(ins, in.Role+"="+strings.Join(in.Globs, "|"))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", printable(j.Name), printable(j.Version), printable(j.Hash),
					printable(strings.Join(emits, ",")), printable(strings.Join(j.Platforms, ",")), printable(strings.Join(ins, " ")))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(shown) == 0 {
				fmt.Fprintln(d.Out, "no parsers are compiled in")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "only parsers for this platform: android or ios")
	return cmd
}

// selection flags

type parseSelFlags struct {
	casePath         string
	parsers          []string
	artifacts        []string
	includeSnapshots bool
}

func (f *parseSelFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.casePath, "case", "", "path to the case directory")
	fl.StringArrayVar(&f.parsers, "parser", nil, "only this parser, NAME or NAME@VERSION (repeatable)")
	fl.StringArrayVar(&f.artifacts, "artifact", nil, "only this artifact id as the primary input (repeatable)")
	fl.BoolVar(&f.includeSnapshots, "include-snapshots", false, "also parse artifacts extracted from filesystem snapshots")
}

// selection validates the parser references against the registry (before any case is opened).
func (f *parseSelFlags) selection(regs []artparse.Registered) (artparse.Selection, error) {
	sel := artparse.Selection{Artifacts: f.artifacts, IncludeSnapshots: f.includeSnapshots}
	for _, p := range f.parsers {
		name, version, hasAt := strings.Cut(p, "@")
		if name == "" || (hasAt && version == "") {
			return sel, usageErrorf("--parser %s: want NAME or NAME@VERSION", printable(p))
		}
		var named, pinned bool
		for _, r := range regs {
			if m := r.Meta(); m.Name == name {
				named = true
				pinned = pinned || version == "" || m.Version == version
			}
		}
		switch {
		case !named:
			return sel, fmt.Errorf("%w: unknown parser %s", artparse.ErrSelection, printable(name))
		case !pinned:
			return sel, fmt.Errorf("%w: this build holds no parser %s@%s", artparse.ErrSelection, printable(name), printable(version))
		}
		sel.Parsers = append(sel.Parsers, artparse.ParserRef{Name: name, Version: version})
	}
	return sel, nil
}

// openHost opens the case (taking its lock), refuses a case that needs an upgrade and builds the host.
func openHost(casePath string, regs []artparse.Registered, lim parse.Limits) (*evidence.Case, *artparse.Host, error) {
	c, err := openCase(casePath)
	if err != nil {
		return nil, nil, err
	}
	if err := c.RequireSchema(evidence.CurrentSchema); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	h, err := artparse.New(c, regs, artparse.Options{Limits: lim})
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, h, nil
}

func defaultParseLimits() parse.Limits {
	lim := parse.DefaultLimits()
	if parseLimitsHook != nil {
		parseLimitsHook(&lim)
	}
	return lim
}

// plan

type bundleJSON struct {
	Role       string `json:"role"`
	ArtifactID string `json:"artifact_id"`
	Logical    string `json:"logical_path"`
}

type snapshotJSON struct {
	Name string `json:"name"`
	Xid  uint64 `json:"xid"`
}

type planRowJSON struct {
	Status     string        `json:"status"`
	Reason     string        `json:"reason"`
	Parser     string        `json:"parser"`
	Version    string        `json:"version"`
	ArtifactID string        `json:"artifact_id"`
	Logical    string        `json:"logical_path"`
	Platform   string        `json:"platform"`
	Namer      string        `json:"namer"`
	Snapshot   *snapshotJSON `json:"snapshot"`
	Bundle     []bundleJSON  `json:"bundle"`
	Integrity  bool          `json:"integrity"`
	Incomplete bool          `json:"incomplete"`
}

type unnamedJSON struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Reason     string `json:"reason"`
}

type countsJSON struct {
	Manifest         int            `json:"manifest"`
	NotParserInput   int            `json:"not_parser_input"`
	SnapshotExcluded int            `json:"snapshot_excluded"`
	ByKind           map[string]int `json:"by_kind"`
	MissingRole      map[string]int `json:"missing_role"`
	Unnamed          map[string]int `json:"unnamed"`
	UnnamedList      []unnamedJSON  `json:"unnamed_list"`
	ByStatus         map[string]int `json:"by_status"`
}

func nonNilMap(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

func planCountsJSON(c artparse.Counts, byStatus map[string]int) countsJSON {
	out := countsJSON{
		Manifest: c.Manifest, NotParserInput: c.NotParserInput, SnapshotExcluded: c.SnapshotExcluded,
		ByKind: nonNilMap(c.ByKind), MissingRole: nonNilMap(c.MissingRole), Unnamed: nonNilMap(c.Unnamed), ByStatus: nonNilMap(byStatus),
		UnnamedList: []unnamedJSON{},
	}
	for _, u := range c.UnnamedList {
		out.UnnamedList = append(out.UnnamedList, unnamedJSON{ArtifactID: u.ArtifactID, Path: u.Path, Reason: u.Reason})
	}
	return out
}

func memberRoles(j artparse.Job) []bundleJSON {
	out := []bundleJSON{{Role: "primary", ArtifactID: j.Primary.Artifact.ID, Logical: j.Primary.Logical}}
	roles := make([]string, 0, len(j.Others))
	for r := range j.Others {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		m := j.Others[r]
		out = append(out, bundleJSON{Role: r, ArtifactID: m.Artifact.ID, Logical: m.Logical})
	}
	return out
}

func planRowToJSON(r artparse.PlanRow) planRowJSON {
	id := r.Job.Parser.Identity()
	j := planRowJSON{
		Status: r.Status, Reason: r.Reason, Parser: id.Name, Version: id.Version, ArtifactID: r.Job.Primary.Artifact.ID,
		Logical: r.Job.Primary.Logical, Platform: r.Job.Primary.Platform, Namer: r.Job.Primary.Namer, Bundle: memberRoles(r.Job),
		Integrity: r.Integrity, Incomplete: r.Incomplete,
	}
	if s := r.Job.Primary.Snapshot; s != nil {
		j.Snapshot = &snapshotJSON{Name: s.Name, Xid: s.Xid}
	}
	return j
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writePlanText(w io.Writer, p artparse.Plan) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tPARSER\tARTIFACT\tLOGICAL PATH\tBUNDLE")
	for _, r := range p.Rows {
		j := planRowToJSON(r)
		status := j.Status
		if j.Reason != "" {
			status += ":" + j.Reason
		}
		var bundle []string
		for _, b := range j.Bundle[1:] {
			bundle = append(bundle, b.Role+"="+printable(b.ArtifactID))
		}
		logical := printable(j.Logical) + " (" + printable(j.Platform) + ", " + printable(j.Namer) + ")"
		if j.Snapshot != nil {
			logical += " [snapshot " + printable(j.Snapshot.Name) + " xid " + strconv.FormatUint(j.Snapshot.Xid, 10) + "]"
		}
		if j.Incomplete {
			logical += " [incomplete]"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", escapeText(status), printable(j.Parser+"@"+j.Version), printable(j.ArtifactID), logical, strings.Join(bundle, " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	c := planCountsJSON(p.Counts, p.ByStatus)
	fmt.Fprintf(w, "%d job(s)", len(p.Rows))
	for _, k := range sortedKeys(c.ByStatus) {
		fmt.Fprintf(w, ", %s %d", escapeText(k), c.ByStatus[k])
	}
	fmt.Fprintf(w, "; manifest records %d, not parser input %d, snapshot artifacts excluded %d\n", c.Manifest, c.NotParserInput, c.SnapshotExcluded)
	for _, k := range sortedKeys(c.MissingRole) {
		fmt.Fprintf(w, "  missing a required role: %s %d\n", printable(k), c.MissingRole[k])
	}
	for _, k := range sortedKeys(c.Unnamed) {
		fmt.Fprintf(w, "  no logical path (%s): %d\n", escapeText(k), c.Unnamed[k])
	}
	const maxListed = 20
	for i, u := range c.UnnamedList {
		if i == maxListed {
			fmt.Fprintf(w, "  ... %d more unnamed artifacts\n", len(c.UnnamedList)-maxListed)
			break
		}
		fmt.Fprintf(w, "  unnamed %s %s: %s\n", printable(u.ArtifactID), printable(u.Path), escapeText(u.Reason))
	}
	return nil
}

func newParsePlanCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f parseSelFlags
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show which parsers would run on which artifacts, without parsing anything",
		Long: "Discover the jobs of a selection and show the status each one would get, running every parser's Probe\n" +
			"in its sandbox. Nothing is parsed or stored and the audit log gains only the case.open entry.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			regs, err := d.parserRegistry()
			if err != nil {
				return err
			}
			sel, err := f.selection(regs)
			if err != nil {
				return err
			}
			fmt.Fprintln(d.Err, discoveryNote)
			c, h, err := openHost(f.casePath, regs, defaultParseLimits())
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			plan, perr := h.Plan(cmd.Context(), sel)
			if perr != nil && len(plan.Rows) == 0 {
				return perr
			}
			if opts.json {
				rows := []planRowJSON{}
				for _, r := range plan.Rows {
					rows = append(rows, planRowToJSON(r))
				}
				if err := writeJSON(d.Out, struct {
					Rows   []planRowJSON `json:"rows"`
					Counts countsJSON    `json:"counts"`
				}{rows, planCountsJSON(plan.Counts, plan.ByStatus)}); err != nil {
					return err
				}
			} else if err := writePlanText(d.Out, plan); err != nil {
				return err
			}
			if perr != nil {
				return perr
			}
			var bad []string
			for _, r := range plan.Rows {
				if r.Integrity {
					bad = append(bad, fmt.Sprintf("%s: %s", r.Job.Primary.Artifact.ID, r.Reason))
				}
			}
			if len(bad) > 0 {
				return integrityRunError(len(bad), bad, nil)
			}
			return nil
		},
	}
	f.register(cmd)
	return cmd
}

// run

var sizeRE = regexp.MustCompile(`^([0-9]{1,15}) ?(B|KiB|MiB|GiB)?$`)

// parseSize reads a size such as 512MiB, 1GiB or a plain number of bytes.
func parseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%s is not a size (for example 512MiB, 1GiB or a number of bytes)", printable(s))
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a size", printable(s))
	}
	var shift uint
	switch m[2] {
	case "KiB":
		shift = 10
	case "MiB":
		shift = 20
	case "GiB":
		shift = 30
	}
	if n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("%s is too large a size", printable(s))
	}
	return n << shift, nil
}

// partialRunError is a run in which some job did not complete (exit 1).
type partialRunError struct {
	n       int
	stopped string
}

func (e partialRunError) Error() string {
	if e.n == 0 {
		return "the run stopped early: " + e.stopped
	}
	msg := fmt.Sprintf("%d job(s) incomplete, unparsed or refused", e.n)
	if e.stopped != "" {
		msg += "; the run stopped early: " + e.stopped
	}
	return msg
}

// integrityRunError is the exit-4 error of a plan or run: one line, escaped.
func integrityRunError(n int, details []string, also error) error {
	const maxShown = 3
	shown := details
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	parts := make([]string, len(shown))
	for i, s := range shown {
		parts[i] = escapeText(s)
	}
	msg := fmt.Sprintf("%d integrity failure(s): %s", n, strings.Join(parts, "; "))
	if len(details) > maxShown {
		msg += fmt.Sprintf("; and %d more", len(details)-maxShown)
	}
	if also != nil {
		msg += "; the run also failed: " + escapeText(also.Error())
	}
	return fmt.Errorf("%s: %w", msg, evidence.ErrIntegrity)
}

type jobJSON struct {
	Event              string            `json:"event,omitempty"`
	Job                int               `json:"job"`
	Parser             string            `json:"parser"`
	Version            string            `json:"version"`
	Hash               string            `json:"hash"`
	ArtifactID         string            `json:"artifact_id"`
	Logical            string            `json:"logical_path"`
	Platform           string            `json:"platform"`
	Outcome            string            `json:"outcome"`
	Reason             string            `json:"reason"`
	IngestID           string            `json:"ingest_id"`
	Records            int64             `json:"records"`
	Rejected           int               `json:"rejected"`
	Warnings           int               `json:"warnings"`
	WarningsSuppressed int               `json:"warnings_suppressed"`
	DurationMS         int64             `json:"duration_ms"`
	Notes              map[string]string `json:"notes"`
	Panic              *panicJSON        `json:"panic"`
	Abandoned          bool              `json:"abandoned"`
	TimedOut           bool              `json:"timed_out"`
	Integrity          bool              `json:"integrity"`
}

type panicJSON struct {
	Value string `json:"value"`
	Stack string `json:"stack"`
}

func jobToJSON(event string, r artparse.JobResult) jobJSON {
	j := jobJSON{
		Event: event, Job: r.Job, Parser: r.Parser, Version: r.Version, Hash: r.Hash, ArtifactID: r.ArtifactID, Logical: r.Logical,
		Platform: r.Platform, Outcome: r.Outcome, Reason: r.Reason, IngestID: r.IngestID, Records: r.Records, Rejected: r.Rejected,
		Warnings: r.Warnings, WarningsSuppressed: r.WarningsSuppressed, DurationMS: r.Duration.Milliseconds(), Notes: r.Notes,
		Abandoned: r.Abandoned, TimedOut: r.TimedOut, Integrity: r.Integrity,
	}
	if j.Notes == nil {
		j.Notes = map[string]string{}
	}
	if r.Panic != nil {
		j.Panic = &panicJSON{Value: r.Panic.Value, Stack: r.Panic.Stack}
	}
	return j
}

func className(c artparse.Class) string {
	switch c {
	case artparse.ClassIntegrity:
		return "integrity"
	case artparse.ClassPartial:
		return "partial"
	}
	return "ok"
}

// thousands renders 48210 as 48,210.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func statusLine(r artparse.JobResult, total int) string {
	line := fmt.Sprintf("[%d/%d] %s %s %s ... %s records, %d warnings, %.1f s", r.Job, total, printable(r.Parser), printable(r.Version),
		printable(r.ArtifactID), thousands(r.Records), r.Warnings, r.Duration.Seconds())
	if r.Outcome != artparse.OutcomeComplete {
		line += " [" + r.Outcome
		if r.Reason != "" {
			line += ": " + escapeText(r.Reason)
		}
		line += "]"
	}
	return line
}

// failedJob reports whether a job counts for the "incomplete, unparsed or refused" exit line.
func failedJob(r artparse.JobResult) bool {
	switch r.Outcome {
	case artparse.OutcomeIncomplete, artparse.OutcomeUnparsed, artparse.OutcomeRefused, artparse.OutcomeNotRun:
		return true
	}
	return false
}

func writeRunSummaryText(w io.Writer, sum artparse.RunSummary, verbose bool) {
	counts := map[string]int{}
	for _, j := range sum.Jobs {
		counts[j.Outcome]++
	}
	fmt.Fprintf(w, "parse %s: %d job(s), %s records\n", printable(sum.ParseID), len(sum.Jobs), thousands(sum.Records))
	if len(counts) > 0 {
		var parts []string
		for _, k := range sortedKeys(counts) {
			parts = append(parts, fmt.Sprintf("%s %d", escapeText(k), counts[k]))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, ", "))
	}
	if sum.Stopped != "" {
		fmt.Fprintf(w, "  the run stopped early: %s\n", escapeText(sum.Stopped))
	}
	for _, j := range sum.Jobs {
		if j.Outcome == artparse.OutcomeComplete && j.Panic == nil && (!verbose || len(j.Notes) == 0) {
			continue
		}
		if j.Outcome == artparse.OutcomeSkipped && !verbose {
			continue
		}
		fmt.Fprintf(w, "  [%s] %s %s %s %s", escapeText(j.Outcome), printable(j.Parser), printable(j.Version), printable(j.ArtifactID), printable(j.Logical))
		if j.Reason != "" {
			fmt.Fprintf(w, ": %s", escapeText(j.Reason))
		}
		fmt.Fprintln(w)
		if j.Panic != nil {
			fmt.Fprintf(w, "    panic: %s\n", escapeText(j.Panic.Value))
		}
		if verbose {
			for _, k := range sortedNotes(j.Notes) {
				fmt.Fprintf(w, "    note %s: %s\n", escapeText(k), escapeText(j.Notes[k]))
			}
		}
	}
}

func sortedNotes(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newParseRunCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f parseSelFlags
	var reparse bool
	var timeout, memBudget string
	var maxRecords int64
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Parse artifacts into records",
		Long: "Parse the selected artifacts with the compiled-in parsers and store the records in the case. One status line\n" +
			"per job goes to stderr; the summary (or, with --json, one JSON event per line) goes to stdout. Exit 0 when every\n" +
			"job completed or was skipped, 1 when some job was incomplete, unparsed or refused (or the run was stopped),\n" +
			"2 for a usage error (a bad flag value, an unknown parser, a case that needs case upgrade or records reindex),\n" +
			"4 when an input did not match the evidence.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			lim := defaultParseLimits()
			if cmd.Flags().Changed("timeout") {
				dur, err := time.ParseDuration(timeout)
				if err != nil || dur < time.Second || dur > 24*time.Hour {
					return usageErrorf("--timeout %s: want a duration from 1s to 24h (for example 30m)", printable(timeout))
				}
				lim.Timeout = dur
			}
			if cmd.Flags().Changed("mem-budget") {
				n, err := parseSize(memBudget)
				if err != nil {
					return usageErrorf("--mem-budget: %v", err)
				}
				if n < 16<<20 || n > 64<<30 {
					return usageErrorf("--mem-budget %s: want a size from 16MiB to 64GiB", printable(memBudget))
				}
				lim.MemBudget = n
			}
			if cmd.Flags().Changed("max-records") {
				if maxRecords < 1 || maxRecords > 1_000_000_000 {
					return usageErrorf("--max-records %d: want a number from 1 to 1000000000", maxRecords)
				}
				lim.MaxRecords = maxRecords
			}
			regs, err := d.parserRegistry()
			if err != nil {
				return err
			}
			sel, err := f.selection(regs)
			if err != nil {
				return err
			}
			sel.Reparse = reparse
			fmt.Fprintln(d.Err, discoveryNote)
			c, h, err := openHost(f.casePath, regs, lim)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()

			// twice the budget, but never above a lower limit the operator set (GOMEMLIMIT)
			cur := debug.SetMemoryLimit(-1)
			prev := debug.SetMemoryLimit(min(cur, 2*lim.MemBudget))
			defer debug.SetMemoryLimit(prev)

			total := 0
			onEvent := func(e artparse.Event) {
				switch e.Kind {
				case "run.start":
					total = e.Jobs
					if opts.json {
						_ = writeNDJSON(d.Out, map[string]any{"event": "run.start", "parse_id": e.ParseID, "jobs": e.Jobs})
					}
				case "job.start":
					if opts.json {
						r := e.Result
						_ = writeNDJSON(d.Out, map[string]any{
							"event": "job.start", "job": e.Job, "parser": r.Parser, "version": r.Version, "artifact_id": r.ArtifactID,
							"logical_path": r.Logical, "platform": r.Platform,
						})
					}
				case "job.progress":
					if opts.json {
						_ = writeNDJSON(d.Out, map[string]any{"event": "job.progress", "job": e.Job, "done": e.Done, "total": e.Total})
					}
				case "job.end":
					fmt.Fprintln(d.Err, statusLine(*e.Result, total))
					if opts.json {
						_ = writeNDJSON(d.Out, jobToJSON("job.end", *e.Result))
					}
				}
			}
			sum, runErr := h.Run(cmd.Context(), artparse.RunOptions{Selection: sel, OnEvent: onEvent})
			return finishRun(d, opts, sum, runErr)
		},
	}
	f.register(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&reparse, "reparse", false, "parse again the artifacts a parser already parsed (the new run supersedes the old one)")
	fl.StringVar(&timeout, "timeout", "", "time limit for one parser job, 1s to 24h (default 1h)")
	fl.StringVar(&memBudget, "mem-budget", "", "memory budget of one parser job, 16MiB to 64GiB (default 1GiB)")
	fl.Int64Var(&maxRecords, "max-records", 0, "most records one job may emit, 1 to 1000000000 (default 10000000)")
	return cmd
}

func writeNDJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// finishRun prints the summary and maps the outcome to the error that sets the exit code: integrity
// (exit 4) beats everything, then a run-level error, then a partial run (exit 1).
func finishRun(d Deps, opts *rootOptions, sum artparse.RunSummary, runErr error) error {
	if sum.ParseID != "" {
		if opts.json {
			jobs := []jobJSON{}
			for _, j := range sum.Jobs {
				jobs = append(jobs, jobToJSON("", j))
			}
			outcomes := map[string]int{}
			for _, j := range sum.Jobs {
				outcomes[j.Outcome]++
			}
			_ = writeNDJSON(d.Out, map[string]any{
				"event": "summary", "parse_id": sum.ParseID, "class": className(sum.Class()), "stopped": sum.Stopped,
				"cancelled": sum.Cancelled, "records": sum.Records, "outcomes": outcomes, "jobs": jobs,
			})
		} else {
			writeRunSummaryText(d.Out, sum, opts.verbose)
		}
	}
	abandoned := sum.Stopped == artparse.StoppedAbandoned
	for _, j := range sum.Jobs {
		abandoned = abandoned || j.Abandoned
	}
	if abandoned {
		fmt.Fprintln(d.Err, "restart recommended: a parser did not stop; the run was ended")
	}
	var integrity []string
	for _, j := range sum.Jobs {
		if j.Integrity {
			integrity = append(integrity, fmt.Sprintf("job %d %s: %s", j.Job, j.ArtifactID, j.Reason))
		}
	}
	switch {
	case len(integrity) > 0:
		return integrityRunError(len(integrity), integrity, runErr)
	case runErr != nil:
		return runErr
	case sum.Class() == artparse.ClassOK:
		return nil
	}
	n := 0
	for _, j := range sum.Jobs {
		if failedJob(j) {
			n++
		}
	}
	return partialRunError{n: n, stopped: sum.Stopped}
}
