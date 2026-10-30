package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

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
	cmd.AddCommand(newParseListCmd(d, opts), newParsePlanCmd(d, opts), newParseRunCmd(d, opts), newParseRecoverCmd(d, opts))
	return cmd
}

// registry returns the parsers of this build, or the injected ones.
func (d Deps) parserRegistry() ([]artparse.Registered, error) {
	if d.ParserRegistry != nil {
		return d.ParserRegistry()
	}
	return registerParsers(parsers.All(), parsers.HashOf, parsers.RecoveryHashOf)
}

// registerParsers registers each parser with its generated source hash and, when it claims tables, its
// generated recovery hash (the "<name>.recover" identity the typed recovery pass writes under).
func registerParsers(ps []parse.Parser, hashOf, recoveryHashOf func(parse.Parser) (string, bool)) ([]artparse.Registered, error) {
	var out []artparse.Registered
	for _, p := range ps {
		hash, ok := hashOf(p)
		if !ok {
			return nil, errors.New("a compiled-in parser has no generated source hash (run the parser hash generator)")
		}
		r, err := artparse.Register(p, hash)
		if err != nil {
			return nil, err
		}
		if len(r.Meta().Claims) > 0 {
			rh, ok := recoveryHashOf(p)
			if !ok {
				return nil, fmt.Errorf("compiled-in parser %s claims tables but has no generated recovery hash (run the parser hash generator)", r.Meta().Name)
			}
			if r, err = r.WithRecoveryHash(rh); err != nil {
				return nil, err
			}
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
	includeRecovered bool
}

func (f *parseSelFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.casePath, "case", "", "path to the case directory")
	fl.StringArrayVar(&f.parsers, "parser", nil, "only this parser, NAME or NAME@VERSION (repeatable)")
	fl.StringArrayVar(&f.artifacts, "artifact", nil, "only this artifact id as the primary input (repeatable)")
	fl.BoolVar(&f.includeSnapshots, "include-snapshots", false, "also parse artifacts extracted from filesystem snapshots")
	fl.BoolVar(&f.includeRecovered, "include-recovered", false, "also parse recovered and carved artifacts (their records are marked recovered, never live, and capped by how the artifact was recovered)")
}

// selection validates the parser references against the registry (before any case is opened).
func (f *parseSelFlags) selection(regs []artparse.Registered) (artparse.Selection, error) {
	sel := artparse.Selection{Artifacts: f.artifacts, IncludeSnapshots: f.includeSnapshots, IncludeRecovered: f.includeRecovered}
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
func openHost(casePath string, regs []artparse.Registered, lim parse.Limits, relaxed bool) (*evidence.Case, *artparse.Host, error) {
	c, err := openCase(casePath)
	if err != nil {
		return nil, nil, err
	}
	if err := c.RequireSchema(evidence.CurrentSchema); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	h, err := artparse.New(c, regs, artparse.Options{Limits: lim, RelaxedSync: relaxed})
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
	Scan       bool          `json:"scan"` // the row comes from --scan: an artifact no glob matched
	// Recovered is non-nil when a member of the bundle is recovered input; its confidence is null when the cap is unknown.
	Recovered *recoveredInputJSON `json:"recovered"`
}

type recoveredInputJSON struct {
	Class      string `json:"class"`
	Method     string `json:"method"`
	Confidence *int   `json:"confidence"`
	CapUnknown bool   `json:"cap_unknown"`
}

type unnamedJSON struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Reason     string `json:"reason"`
}

type countsJSON struct {
	Manifest          int             `json:"manifest"`
	NotParserInput    int             `json:"not_parser_input"`
	SnapshotExcluded  int             `json:"snapshot_excluded"`
	ByKind            map[string]int  `json:"by_kind"`
	MissingRole       map[string]int  `json:"missing_role"`
	Unnamed           map[string]int  `json:"unnamed"`
	UnnamedList       []unnamedJSON   `json:"unnamed_list"`
	RecoveredExcluded int             `json:"recovered_excluded"`
	ByStatus          map[string]int  `json:"by_status"`
	Scan              *scanCountsJSON `json:"scan"` // null without --scan
}

type scanCountsJSON struct {
	Considered      int    `json:"considered"`
	Probed          int    `json:"probed"`
	Candidates      int    `json:"candidates"`
	NotApplicable   int    `json:"not_applicable"`
	TooLarge        int    `json:"too_large"`
	Companions      int    `json:"companions"`
	NotProbed       int    `json:"not_probed"`
	PairsNotProbed  int    `json:"pairs_not_probed"`
	FindingsDropped int    `json:"findings_dropped"`
	Stopped         string `json:"stopped"`
}

type scanFindingJSON struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Parser     string `json:"parser"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
}

func scanCountsToJSON(s *artparse.ScanCounts) *scanCountsJSON {
	if s == nil {
		return nil
	}
	return &scanCountsJSON{
		Considered: s.Considered, Probed: s.Probed, Candidates: s.Candidates, NotApplicable: s.NotApplicable, TooLarge: s.TooLarge,
		Companions: s.Companions, NotProbed: s.NotProbed, PairsNotProbed: s.PairsNotProbed, FindingsDropped: s.FindingsDropped, Stopped: s.Stopped,
	}
}

// scanFindingsToJSON is null without --scan and an array (possibly empty) with it.
func scanFindingsToJSON(p artparse.Plan) []scanFindingJSON {
	if p.Scan == nil {
		return nil
	}
	out := []scanFindingJSON{}
	for _, f := range p.ScanFindings {
		out = append(out, scanFindingJSON{ArtifactID: f.ArtifactID, Path: f.Path, Parser: f.Parser, Status: f.Status, Reason: f.Reason})
	}
	return out
}

func nonNilMap(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

func planCountsJSON(c artparse.Counts, byStatus map[string]int) countsJSON {
	out := countsJSON{
		Manifest: c.Manifest, NotParserInput: c.NotParserInput, SnapshotExcluded: c.SnapshotExcluded, RecoveredExcluded: c.RecoveredExcluded,
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
		Integrity: r.Integrity, Incomplete: r.Incomplete, Scan: r.Scan,
	}
	if s := r.Job.Primary.Snapshot; s != nil {
		j.Snapshot = &snapshotJSON{Name: s.Name, Xid: s.Xid}
	}
	if ri := r.Recovered; ri != nil {
		j.Recovered = &recoveredInputJSON{Class: ri.Class, Method: ri.Method, CapUnknown: ri.CapUnknown}
		if !ri.CapUnknown {
			j.Recovered.Confidence = ri.Confidence // a cut or unreadable chain has no known bound: null, never a number
		}
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
		if j.Scan {
			logical += " [scan]"
		}
		if rj := j.Recovered; rj != nil {
			conf := "unknown"
			if rj.Confidence != nil {
				conf = strconv.Itoa(*rj.Confidence)
			}
			logical += " [recovered:" + printable(rj.Method) + " conf " + conf + "]"
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
	if c.RecoveredExcluded > 0 {
		fmt.Fprintf(w, "  %d recovered or carved artifacts are not discovered (use --include-recovered)\n", c.RecoveredExcluded)
	}
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
	writeScanText(w, p)
	writeBackupsText(w, p.Backups)
	return nil
}

// writeScanText prints the findings of a scan (the first 20) and its summary line.
func writeScanText(w io.Writer, p artparse.Plan) {
	s := p.Scan
	if s == nil {
		return
	}
	const maxListed = 20
	for i, f := range p.ScanFindings {
		if i == maxListed {
			fmt.Fprintf(w, "  ... %d more scan findings\n", len(p.ScanFindings)-maxListed)
			break
		}
		fmt.Fprintf(w, "  scan: %s %s %s: %s: %s\n", printable(f.ArtifactID), printable(f.Path), printable(f.Parser), escapeText(f.Status), escapeText(f.Reason))
	}
	if s.FindingsDropped > 0 {
		fmt.Fprintf(w, "  ... %d more scan findings were not kept\n", s.FindingsDropped)
	}
	fmt.Fprintf(w, "scan: %d considered, %d probed, %d candidates, %d artifact x parser probes not applicable, %d over 1 GiB, %d companions, %d not probed",
		s.Considered, s.Probed, s.Candidates, s.NotApplicable, s.TooLarge, s.Companions, s.NotProbed)
	if s.PairsNotProbed > 0 {
		fmt.Fprintf(w, ", %d artifact x parser probes of the abandoned candidate not run", s.PairsNotProbed)
	}
	if s.Stopped != "" {
		fmt.Fprintf(w, ", stopped: %s", escapeText(s.Stopped))
	}
	fmt.Fprintln(w)
}

func newParsePlanCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f parseSelFlags
	var scan bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show which parsers would run on which artifacts, without parsing anything",
		Long: "Discover the jobs of a selection and show the status each one would get, running every parser's Probe\n" +
			"in its sandbox. Nothing is parsed or stored and the audit log gains only the case.open entry.\n" +
			"iOS backups of the case are listed after the rows (BACKUP lines, JSON \"backups\"): whether each Manifest.db could name\n" +
			"its files, its encryption state and how many files got a logical path. An encrypted or unreadable backup is shown as\n" +
			"unparsed with its reason, never as empty; a Manifest.db that fails its hash check is an integrity failure (exit 4).\n" +
			"--scan also probes every selected parser on the eligible artifacts no glob matched (up to 1 GiB each, one read per artifact):\n" +
			"a database kept under another name or path is listed as a candidate (marked [scan]), never run without --artifact ID;\n" +
			"unsupported-schema, encrypted and failed probes are listed as scan findings, a file no parser recognises is only counted.",
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
			if scan && len(f.artifacts) > 0 {
				return usageErrorf("--scan cannot be combined with --artifact: a scan lists what no glob matched, --artifact names one artifact")
			}
			sel.Scan = scan
			fmt.Fprintln(d.Err, discoveryNote)
			c, h, err := openHost(f.casePath, regs, defaultParseLimits(), false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			plan, perr := h.Plan(cmd.Context(), sel)
			if perr != nil && len(plan.Rows) == 0 && len(plan.Backups) == 0 && plan.Scan == nil {
				return perr
			}
			var jerr error
			if opts.json {
				rows := []planRowJSON{}
				for _, r := range plan.Rows {
					rows = append(rows, planRowToJSON(r))
				}
				counts := planCountsJSON(plan.Counts, plan.ByStatus)
				counts.Scan = scanCountsToJSON(plan.Scan)
				jerr = writeJSON(d.Out, struct {
					Rows     []planRowJSON     `json:"rows"`
					Counts   countsJSON        `json:"counts"`
					Backups  []backupJSON      `json:"backups"`
					Findings []scanFindingJSON `json:"scan_findings"`
				}{rows, counts, backupsToJSON(plan.Backups), scanFindingsToJSON(plan)})
			} else {
				jerr = writePlanText(d.Out, plan)
			}
			return planOutcome(plan, perr, jerr)
		},
	}
	f.register(cmd)
	cmd.Flags().BoolVar(&scan, "scan", false, "also probe every parser on eligible artifacts no glob matched, up to 1 GiB; candidates are listed, never run without --artifact")
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
	backups int // backups not parsed (encrypted, unreadable or refused)
	stopped string
}

func (e partialRunError) Error() string {
	var parts []string
	if e.n > 0 {
		parts = append(parts, fmt.Sprintf("%d job(s) incomplete, unparsed or refused", e.n))
	}
	if e.backups > 0 {
		parts = append(parts, fmt.Sprintf("%d backup(s) not parsed (see the BACKUP lines)", e.backups))
	}
	if len(parts) == 0 {
		return "the run stopped early: " + e.stopped
	}
	msg := strings.Join(parts, "; ")
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
	writeBackupsText(w, sum.Backups)
	if n := notParsedBackups(sum.Backups); n > 0 {
		fmt.Fprintf(w, "  %d backup(s) not parsed\n", n)
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
	var fastSync bool
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
			lim, err := limitsFromFlags(cmd, timeout, memBudget)
			if err != nil {
				return err
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
			c, h, err := openHost(f.casePath, regs, lim, fastSync)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()

			defer limitMemory(lim)()

			ev := &runEvents{d: d, json: opts.json, stream: &jsonStream{w: d.Out, errw: d.Err}}
			onEvent := ev.onEvent
			sum, runErr := h.Run(cmd.Context(), artparse.RunOptions{Selection: sel, OnEvent: onEvent})
			return joinJSON(finishRun(d, opts, sum, runErr), ev.stream.finish())
		},
	}
	f.register(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&reparse, "reparse", false, "parse again the artifacts a parser already parsed (the new run supersedes the old one)")
	fl.BoolVar(&fastSync, "fast-sync", false, "faster ingest: fsync the database less often while records are written (a power loss can lose the last batches; the run is then reported incomplete with the lost batches named; the audit log is still fsynced before every batch)")
	fl.StringVar(&timeout, "timeout", "", "time limit for one parser job, 1s to 24h (default 1h)")
	fl.StringVar(&memBudget, "mem-budget", "", "memory budget of one parser job, 16MiB to 64GiB (default 1GiB)")
	fl.Int64Var(&maxRecords, "max-records", 0, "most records one job may emit, 1 to 1000000000 (default 10000000)")
	return cmd
}

// finishRun prints the summary and maps the outcome to the error that sets the exit code: integrity
// (exit 4) beats everything, then a run-level error, then a partial run (exit 1).
func finishRun(d Deps, opts *rootOptions, sum artparse.RunSummary, runErr error) error {
	return finishRunMode(d, opts, sum, runErr, false)
}

// finishRunMode is finishRun for a live run (typed false) or a typed recovery run (typed true).
func finishRunMode(d Deps, opts *rootOptions, sum artparse.RunSummary, runErr error, typed bool) error {
	jerr := writeRunSummaryMode(d, opts, sum, typed)
	return joinJSON(runOutcome(d, sum, runErr), jerr)
}

// writeRunSummaryMode prints the summary of a run (typed: a typed recovery run). A JSON summary that cannot be
// written safely is refused (stderr says so) and the refusal is returned, to be joined after the run's own outcome.
func writeRunSummaryMode(d Deps, opts *rootOptions, sum artparse.RunSummary, typed bool) error {
	if sum.ParseID == "" {
		return nil
	}
	if opts.json {
		var jobs []any
		for _, j := range sum.Jobs {
			if typed {
				jobs = append(jobs, recoverJobToJSON("", j))
			} else {
				jobs = append(jobs, jobToJSON("", j))
			}
		}
		if jobs == nil {
			jobs = []any{}
		}
		outcomes := map[string]int{}
		for _, j := range sum.Jobs {
			outcomes[j.Outcome]++
		}
		line := map[string]any{
			"event": "summary", "parse_id": sum.ParseID, "class": className(sum.Class()), "stopped": sum.Stopped,
			"cancelled": sum.Cancelled, "records": sum.Records, "outcomes": outcomes, "jobs": jobs, "backups": backupsToJSON(sum.Backups),
		}
		if typed {
			line["recovered_not_live"] = true
		}
		err := writeNDJSON(d.Out, line)
		if err != nil {
			fmt.Fprintf(d.Err, "summary line not written: %s\n", escapeText(err.Error()))
		}
		return err
	}
	if typed {
		writeRecoverSummaryText(d.Out, sum, opts.verbose)
		return nil
	}
	writeRunSummaryText(d.Out, sum, opts.verbose)
	return nil
}

// runOutcome maps the outcome of a run to the error that sets the exit code.
func runOutcome(d Deps, sum artparse.RunSummary, runErr error) error {
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
	integrity = append(integrity, backupIntegrity(sum.Backups)...)
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
	return partialRunError{n: n, backups: notParsedBackups(sum.Backups), stopped: sum.Stopped}
}

// planOutcome maps a plan to the error that sets the exit code: an integrity finding (a job or a backup's
// Manifest.db) is exit 4 and is never masked by a plan error such as a cancel; then the plan's own error.
func planOutcome(plan artparse.Plan, perr, jerr error) error {
	var bad []string
	for _, r := range plan.Rows {
		if r.Integrity {
			bad = append(bad, fmt.Sprintf("%s: %s", r.Job.Primary.Artifact.ID, r.Reason))
		}
	}
	bad = append(bad, backupIntegrity(plan.Backups)...)
	if len(bad) > 0 {
		return joinJSON(integrityRunError(len(bad), bad, perr), jerr)
	}
	return joinJSON(perr, jerr)
}

// backupIntegrity describes the backups whose Manifest.db failed its hash check.
func backupIntegrity(bs []artparse.BackupResolution) []string {
	var out []string
	for _, b := range bs {
		if b.Integrity {
			out = append(out, fmt.Sprintf("backup %s/%s manifest %s: %s", b.Acquisition, b.Dir, backupRefID(b.ManifestDB), b.Reason))
		}
	}
	return out
}

func backupRefID(r *artparse.ArtifactRef) string {
	if r == nil {
		return "none"
	}
	return r.ArtifactID
}

type artifactRefJSON struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
}

type backupJSON struct {
	Acquisition    string           `json:"acquisition"`
	Dir            string           `json:"dir"`
	Status         string           `json:"status"`
	Reason         string           `json:"reason"`
	Encryption     string           `json:"encryption"`
	ManifestDB     *artifactRefJSON `json:"manifest_db"`
	ManifestPlist  *artifactRefJSON `json:"manifest_plist"`
	Mapped         int              `json:"mapped"`
	Unmapped       int              `json:"unmapped"`
	FileIDMismatch int              `json:"fileid_mismatch"`
	Integrity      bool             `json:"integrity"`
}

func refJSON(r *artparse.ArtifactRef) *artifactRefJSON {
	if r == nil {
		return nil
	}
	return &artifactRefJSON{ArtifactID: r.ArtifactID, SHA256: r.SHA256}
}

// backupsToJSON is always a non-nil slice: no backups is [], never null.
func backupsToJSON(bs []artparse.BackupResolution) []backupJSON {
	out := []backupJSON{}
	for _, b := range bs {
		out = append(out, backupJSON{
			Acquisition: b.Acquisition, Dir: b.Dir, Status: b.Status, Reason: b.Reason, Encryption: b.Encryption,
			ManifestDB: refJSON(b.ManifestDB), ManifestPlist: refJSON(b.ManifestPlist), Mapped: b.Mapped, Unmapped: b.Unmapped,
			FileIDMismatch: b.FileIDMismatch, Integrity: b.Integrity,
		})
	}
	return out
}

// writeBackupsText prints one line per backup resolution. A manifest ref means the record was identified,
// not that it was verified (an incomplete or over-size one is named and its reason says so). A mapped
// backup with unknown encryption carries its reason as a note (4E-R37).
func writeBackupsText(w io.Writer, bs []artparse.BackupResolution) {
	for _, b := range bs {
		status := b.Status
		enc := "encryption=" + escapeText(b.Encryption)
		switch {
		case b.Status == artparse.BackupMapped && b.Encryption == "unknown":
			enc += " note=" + escapeText(b.Reason)
		case b.Reason != "":
			status += ":" + b.Reason
		}
		fmt.Fprintf(w, "BACKUP %s/%s  %s  %s  manifest=%s  mapped=%d  unmapped=%d  fileid_mismatch=%d\n",
			printable(b.Acquisition), printable(b.Dir), escapeText(status), enc, printable(backupRefID(b.ManifestDB)), b.Mapped, b.Unmapped, b.FileIDMismatch)
	}
}

// notParsedBackups counts the backups that are not mapped.
func notParsedBackups(bs []artparse.BackupResolution) int {
	n := 0
	for _, b := range bs {
		if b.Status != artparse.BackupMapped {
			n++
		}
	}
	return n
}
