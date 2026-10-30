package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Everything the sqlite commands print that came from a case (artifact ids and paths, sibling
// texts, table names, reasons, method names) reaches the terminal only through escapeText.
// --json keeps the strings as given. Recovered rows are never shown as live: the summary counts
// them per recovery method with their confidence, and `records list` marks them [recovered:...].

func newSQLiteCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("sqlite", "Recover deleted rows from SQLite artifacts")
	cmd.AddCommand(newSQLiteRecoverCmd(d, opts))
	return cmd
}

type sqliteRecoverFlags struct {
	casePath             string
	wal, journal         string
	noSiblings           bool
	methods              string
	minConfidence        int
	excludeTables        []string
	maxRecords, maxBytes int64
	maxRowBytes          int64
	maxBodyBytes         int64
	maxFitEquality       int64
	timeout              time.Duration
	allowReingest        bool
	list                 bool
}

func (f *sqliteRecoverFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.casePath, "case", "", "path to the case directory")
	fl.StringVar(&f.wal, "wal", "", "artifact id of the write-ahead log to use instead of the one found by name")
	fl.StringVar(&f.journal, "journal", "", "artifact id of the rollback journal to use instead of the one found by name")
	fl.BoolVar(&f.noSiblings, "no-siblings", false, "do not look for a WAL or a journal of the database")
	fl.StringVar(&f.methods, "methods", "", "only these recovery methods, comma separated (default: all)")
	fl.IntVar(&f.minConfidence, "min-confidence", 0, "do not write rows below this confidence (0 to 100)")
	fl.StringArrayVar(&f.excludeTables, "exclude-table", nil, "do not write rows of this table (repeatable)")
	fl.Int64Var(&f.maxRecords, "max-records", 0, "stop after this many records (0 = default)")
	fl.Int64Var(&f.maxBytes, "max-bytes", 0, "stop after this many bytes of records (0 = default)")
	fl.Int64Var(&f.maxRowBytes, "max-row-bytes", 0, "largest payload of one record (0 = default)")
	fl.Int64Var(&f.maxBodyBytes, "max-body-bytes", 0, "largest body of one record (0 = default)")
	fl.Int64Var(&f.maxFitEquality, "max-fit-equality-rows", 0, "live rows the equals-live-row annotation of fit-named rows may read in all (0 = default)")
	fl.DurationVar(&f.timeout, "timeout", 0, "stop the run after this long, for example 30m (0 = no limit)")
	fl.BoolVar(&f.allowReingest, "allow-reingest", false, "run again although these inputs were already recovered")
	fl.BoolVar(&f.list, "list", false, "assess only: print what a run would do and write nothing")
}

func (f *sqliteRecoverFlags) inputs(artifact string) dbrecover.Inputs {
	return dbrecover.Inputs{DB: artifact, WAL: f.wal, Journal: f.journal, NoSiblings: f.noSiblings}
}

func (f *sqliteRecoverFlags) options() (dbrecover.Options, error) {
	o := dbrecover.Options{
		MinConfidence: f.minConfidence, MaxRecords: f.maxRecords, MaxBytes: f.maxBytes,
		MaxPayloadBytes: f.maxRowBytes, MaxBodyBytes: f.maxBodyBytes, MaxFitEqualityRows: f.maxFitEquality, AllowReingest: f.allowReingest,
	}
	if f.methods != "" {
		for _, m := range strings.Split(f.methods, ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				return o, usageErrorf("--methods %s: an empty method name", printable(f.methods))
			}
			o.Methods = append(o.Methods, m)
		}
	}
	for _, t := range f.excludeTables {
		if t == "" {
			return o, usageErrorf("--exclude-table: an empty table name")
		}
		o.ExcludeTables = append(o.ExcludeTables, dbrecover.ExcludedTable{Table: t, Parser: "examiner"})
	}
	if f.timeout < 0 {
		return o, usageErrorf("--timeout %s is negative", f.timeout)
	}
	return o, nil
}

func newSQLiteRecoverCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f sqliteRecoverFlags
	cmd := &cobra.Command{
		Use:   "recover <artifact id or path>",
		Short: "Recover deleted rows from a SQLite database artifact",
		Long: "Recover the deleted rows of a SQLite database artifact, and of its WAL and rollback journal, and store them\n" +
			"as db_row records (recovered, never live) with their confidence. The WAL and the journal are found by the\n" +
			"manifest identity of the database (for a file of an iOS backup, from that backup's Manifest.db), or named\n" +
			"with --wal and --journal. With --list nothing is written: the\n" +
			"command prints what a run would do. Exit 0 on success (a narrowed run included, its narrowing is printed),\n" +
			"1 on an error, 2 for a usage error or a case that needs case upgrade or records reindex, 4 when an input no\n" +
			"longer matches the evidence.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := f.options()
			if err != nil {
				return err
			}
			c, err := openCase(f.casePath)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if err := c.RequireSchema(evidence.CurrentSchema); err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if f.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, f.timeout)
				defer cancel()
			}
			// a database that is a file of an iOS backup gets the companions its Manifest.db names (4h-R8); the generic
			// and the typed run read the same bundle
			in, bsib, err := pairBackupSiblings(ctx, d, c, f.inputs(args[0]))
			if err != nil {
				return err
			}
			if f.list {
				as, err := dbrecover.Plan(ctx, c, in, o)
				if err != nil {
					return err
				}
				claims, err := listClaims(ctx, d, c, in)
				if err != nil {
					return err
				}
				return printSQLiteAssessment(d.Out, c, as, opts.json, claims, bsib)
			}
			// the typed pass first (4G-R5); it honors a claim only for a job that concluded
			tr, typedExcl, typedErr := composeTyped(ctx, d, c, in, f.allowReingest)
			tr.backup = bsib
			if tr.Stop {
				return errors.Join(typedErr, printTypedOnly(d.Out, tr, opts.json))
			}
			o.ExcludeTables = append(typedExcl, o.ExcludeTables...)
			o.KeepClaimed, o.TypedNotHonored = tr.Keep, tr.Link
			res, err := dbrecover.Run(ctx, c, in, o)
			// the typed pass's exit contribution is joined, never replaces the generic run's (integrity wins in ExitCode)
			err = errors.Join(typedErr, err)
			if len(res.Inputs) > 0 {
				// the summary of a run that stopped is printed too: what was written stays in the case
				return finishSQLiteRunTyped(d.Out, c, res, err, opts.json, tr)
			}
			// the generic run did not start (a refusal): the typed pass's report is still printed
			return errors.Join(err, printTypedOnly(d.Out, tr, opts.json))
		},
	}
	f.register(cmd)
	_ = cmd.MarkFlagRequired("case")
	return cmd
}

// JSON shapes

type sqliteInputJSON struct {
	Role            string         `json:"role"`
	ArtifactID      string         `json:"artifact_id"`
	SHA256          string         `json:"sha256"`
	Size            int64          `json:"size"`
	Recovered       bool           `json:"recovered"`
	RecoveredMethod string         `json:"recovered_method,omitempty"`
	ConfidenceCap   *int           `json:"confidence_cap"`
	Incomplete      bool           `json:"incomplete"`
	Facts           map[string]any `json:"facts"`
}

type sqliteSiblingJSON struct {
	Status           string   `json:"status"`
	ArtifactID       string   `json:"artifact_id,omitempty"`
	Unused           string   `json:"unused,omitempty"`
	Candidates       []string `json:"candidates,omitempty"`
	SearchedBy       string   `json:"searched_by,omitempty"`
	OtherAcquisition []string `json:"other_acquisition,omitempty"`
	Text             string   `json:"text"`
}

type sqliteMethodJSON struct {
	Method        string `json:"method"`
	Rows          int64  `json:"rows"`
	ConfidenceMin *int   `json:"confidence_min"`
	ConfidenceMax *int   `json:"confidence_max"`
	Band          string `json:"band,omitempty"`
}

type sqliteCountsJSON struct {
	Written             int64              `json:"written"`
	Methods             []sqliteMethodJSON `json:"methods"`
	RelationUnknown     int64              `json:"relation_unknown"`
	FitOnly             int64              `json:"fit_only"`
	DuplicateOfLive     int64              `json:"duplicate_of_live"`
	SkippedByReason     map[string]int64   `json:"skipped_by_reason"`
	Narrowed            []string           `json:"narrowed"`
	LimitsHit           []string           `json:"limits_hit"`
	FreelistPages       int64              `json:"freelist_pages"`
	FreelistPagesZero   int64              `json:"freelist_pages_zeroed"`
	SlackUnparsedBytes  int64              `json:"slack_unparsed_bytes"`
	LibraryWarnings     int                `json:"library_warnings"`
	RangeOutside        int64              `json:"range_outside"`
	KeptUnmappedClaimed int64              `json:"kept_unmapped_claimed"`
}

type sqliteIngestJSON struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Records  int64  `json:"records"`
	Batches  int    `json:"batches"`
	Rejected int    `json:"rejected"`
}

type sqliteReportJSON struct {
	Assessment bool                         `json:"assessment"`
	Note       string                       `json:"note,omitempty"`
	AnalysisID string                       `json:"analysis_id,omitempty"`
	RulesHash  string                       `json:"rules_hash"`
	Inputs     []sqliteInputJSON            `json:"inputs"`
	Siblings   map[string]sqliteSiblingJSON `json:"siblings"`
	Counts     sqliteCountsJSON             `json:"counts"`
	Ingest     *sqliteIngestJSON            `json:"ingest,omitempty"`
	Claims     []claimedJSON                `json:"claims,omitempty"`
	Typed      *typedJSON                   `json:"typed"`
	// BackupSiblings: null unless the database is a file of an iOS backup whose Manifest.db was asked for its companions.
	BackupSiblings *backupSiblingsJSON `json:"backup_siblings"`
	Incomplete     bool                `json:"incomplete,omitempty"`
	Stopped        string              `json:"stopped,omitempty"`
	NotStarted     string              `json:"not_started,omitempty"`
}

func siblingJSON(role string, r dbrecover.SiblingResult) sqliteSiblingJSON {
	return sqliteSiblingJSON{
		Status: string(r.Status), ArtifactID: r.ArtifactID, Unused: r.Unused, Candidates: r.Candidates,
		SearchedBy: r.SearchedBy, OtherAcquisition: r.OtherAcquisition, Text: r.Text(role),
	}
}

func methodRows(c dbrecover.Counts) []sqliteMethodJSON {
	names := dbrecover.Methods()
	for m := range c.ByMethod {
		if !slices.Contains(names, m) {
			names = append(names, m)
		}
	}
	out := []sqliteMethodJSON{}
	for _, m := range names {
		row := sqliteMethodJSON{Method: m, Rows: c.ByMethod[m]}
		if r, ok := c.ConfidenceRange[m]; ok {
			lo, hi := r.Min, r.Max
			row.ConfidenceMin, row.ConfidenceMax = &lo, &hi
			row.Band = bandRange(lo, hi)
		}
		out = append(out, row)
	}
	return out
}

func bandRange(lo, hi int) string {
	a, b := sqlitefile.Band(lo), sqlitefile.Band(hi)
	if a == b {
		return a
	}
	return a + " to " + b
}

func countsJSON2(c dbrecover.Counts) sqliteCountsJSON {
	return sqliteCountsJSON{
		Written: c.Written, Methods: methodRows(c), RelationUnknown: c.RelationUnknown, FitOnly: c.FitOnly,
		DuplicateOfLive: c.DuplicateOfLive, SkippedByReason: nonNilCounts(c.SkippedByReason),
		Narrowed: nonNilStrings(c.Narrowed), LimitsHit: nonNilStrings(c.LimitsHit),
		FreelistPages: c.FreePages, FreelistPagesZero: c.FreePagesZeroed, SlackUnparsedBytes: c.SlackUnparsedBytes,
		LibraryWarnings: c.LibraryWarnings, RangeOutside: c.RangeOutside, KeptUnmappedClaimed: c.KeptUnmappedClaimed,
	}
}

func nonNilCounts(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// recoveredTrails maps each input artifact to the recovery it comes from, read from the manifest.
func recoveredTrails(c *evidence.Case, inputs []dbrecover.InputInfo) (map[string]string, error) {
	man, err := c.Manifest()
	if err != nil {
		return nil, err
	}
	byID := map[string]evidence.ManifestRecord{}
	for _, r := range man {
		byID[r.ID] = r
	}
	out := map[string]string{}
	for _, in := range inputs {
		if tr, ok := evidence.RecoveryTrailOf(byID, in.ArtifactID); ok {
			m := ""
			if tr.Recovery != nil {
				m = tr.Recovery.Method
			}
			out[in.ArtifactID] = m
		}
	}
	return out, nil
}

func inputsJSON(inputs []dbrecover.InputInfo, trails map[string]string) []sqliteInputJSON {
	out := []sqliteInputJSON{}
	for _, in := range inputs {
		out = append(out, sqliteInputJSON{
			Role: in.Role, ArtifactID: in.ArtifactID, SHA256: in.SHA256, Size: in.Size, Recovered: in.Recovered,
			RecoveredMethod: trails[in.ArtifactID], ConfidenceCap: in.Cap, Incomplete: in.Incomplete, Facts: in.Facts,
		})
	}
	return out
}

func printSQLiteAssessment(w io.Writer, c *evidence.Case, as dbrecover.Assessment, asJSON bool, claims []artparse.Claimed, bsib *backupSiblingsJSON) error {
	trails, err := recoveredTrails(c, as.Inputs)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(w, sqliteReportJSON{
			Assessment: true, Note: as.Note, RulesHash: as.RulesHash, Inputs: inputsJSON(as.Inputs, trails),
			Siblings: map[string]sqliteSiblingJSON{"wal": siblingJSON("wal", as.Siblings.WAL), "journal": siblingJSON("journal", as.Siblings.Journal)},
			Counts:   countsJSON2(as.Counts), Claims: claimsJSON(claims), BackupSiblings: bsib,
		})
	}
	fmt.Fprintln(w, escapeText(as.Note))
	printBackupSiblings(w, bsib)
	printClaims(w, claims)
	printSQLiteBody(w, as.Inputs, trails, as.Siblings, as.Counts)
	return nil
}

// finishSQLiteRun prints the summary of a run that resolved its inputs, then returns the run's error joined
// with any print error: an integrity failure found while printing is never masked by the run's own error (J96).
func finishSQLiteRun(w io.Writer, c *evidence.Case, res dbrecover.Result, runErr error, asJSON bool) error {
	return finishSQLiteRunTyped(w, c, res, runErr, asJSON, typedResult{})
}

// finishSQLiteRunTyped is finishSQLiteRun for a run composed with a typed pass.
func finishSQLiteRunTyped(w io.Writer, c *evidence.Case, res dbrecover.Result, runErr error, asJSON bool, tr typedResult) error {
	return errors.Join(runErr, printSQLiteResult(w, c, res, runErr, asJSON, tr))
}

// printSQLiteResult prints the summary of a run. A run that failed says so on stdout (J95): "not started: <reason>"
// when the writer never began, "stopped: <reason>" and "incomplete" when it did, so a reader of stdout alone cannot
// take the summary for a complete run.
func printSQLiteResult(w io.Writer, c *evidence.Case, res dbrecover.Result, runErr error, asJSON bool, tr typedResult) error {
	trails, err := recoveredTrails(c, res.Inputs)
	if err != nil {
		return err
	}
	notStarted := runErr != nil && !res.WriterStarted
	stopped := runErr != nil && !notStarted
	if asJSON {
		rep := sqliteReportJSON{
			AnalysisID: res.AnalysisID, RulesHash: res.RulesHash, Inputs: inputsJSON(res.Inputs, trails),
			Siblings:   map[string]sqliteSiblingJSON{"wal": siblingJSON("wal", res.Siblings.WAL), "journal": siblingJSON("journal", res.Siblings.Journal)},
			Counts:     countsJSON2(res.Counts),
			Incomplete: stopped || res.Ingest.Outcome == "incomplete",
			Typed:      tr.json(), BackupSiblings: tr.backup,
		}
		if notStarted {
			rep.NotStarted = runErr.Error()
		} else {
			rep.Ingest = &sqliteIngestJSON{
				ID: res.Ingest.IngestID, Outcome: res.Ingest.Outcome, Records: res.Ingest.Records,
				Batches: res.Ingest.Batches, Rejected: res.Ingest.Rejected,
			}
		}
		if stopped {
			rep.Stopped = runErr.Error()
		}
		return writeJSON(w, rep)
	}
	printTypedText(w, tr)
	switch {
	case notStarted:
		fmt.Fprintf(w, "not started: %s\n", escapeText(runErr.Error()))
	case stopped:
		fmt.Fprintf(w, "stopped: %s\n", escapeText(runErr.Error()))
		fmt.Fprintln(w, "incomplete: what follows is what was written before the run stopped")
	}
	printSQLiteBody(w, res.Inputs, trails, res.Siblings, res.Counts)
	if !stopped && res.Ingest.Outcome == "incomplete" {
		// a run that lost or left out rows (a filter, a limit, a refusal by the writer, a cap) is not a full recovery
		reasons := append(slices.Clone(res.Counts.Narrowed), res.Counts.LimitsHit...)
		if len(reasons) == 0 {
			reasons = []string{"the ingest was concluded incomplete"}
		}
		fmt.Fprintf(w, "incomplete: %s\n", escapeText(strings.Join(reasons, ", ")))
	}
	switch {
	case res.Ingest.IngestID != "":
		fmt.Fprintf(w, "ingest %s %s\n", escapeText(res.Ingest.IngestID), escapeText(res.Ingest.Outcome))
	case !notStarted:
		fmt.Fprintln(w, "ingest none")
	}
	fmt.Fprintf(w, "analysis %s\n", escapeText(res.AnalysisID))
	return nil
}

// printSQLiteBody prints what a run and an assessment share, so that the two agree line for line.
func printSQLiteBody(w io.Writer, inputs []dbrecover.InputInfo, trails map[string]string, sib dbrecover.SiblingReport, c dbrecover.Counts) {
	fmt.Fprintln(w, "inputs:")
	for _, in := range inputs {
		line := fmt.Sprintf("  %s: artifact %s  %d bytes  sha256 %s", escapeText(in.Role), escapeText(in.ArtifactID), in.Size, escapeText(in.SHA256))
		if m, ok := trails[in.ArtifactID]; ok {
			if m == "" {
				m = "unknown"
			}
			conf := "conf unknown"
			if in.Cap != nil {
				conf = fmt.Sprintf("conf %d", *in.Cap)
			}
			line += fmt.Sprintf("  [recovered:%s %s]", escapeText(m), conf)
		}
		if in.Incomplete {
			line += "  [incomplete]"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "siblings:")
	fmt.Fprintf(w, "  %s\n", escapeText(sib.WAL.Text("wal")))
	fmt.Fprintf(w, "  %s\n", escapeText(sib.Journal.Text("journal")))
	fmt.Fprintln(w, "recovered rows by method:")
	for _, m := range methodRows(c) {
		if m.ConfidenceMin == nil {
			fmt.Fprintf(w, "  %s: rows %d\n", escapeText(m.Method), m.Rows)
			continue
		}
		fmt.Fprintf(w, "  %s: rows %d, conf %d-%d, band %s\n", escapeText(m.Method), m.Rows, *m.ConfidenceMin, *m.ConfidenceMax, escapeText(m.Band))
	}
	fmt.Fprintf(w, "rows written: %d\n", c.Written)
	fmt.Fprintf(w, "relation unknown: %d, fit-only table identity: %d\n", c.RelationUnknown, c.FitOnly)
	if c.KeptUnmappedClaimed > 0 {
		fmt.Fprintf(w, "claimed rows the typed pass could not map, kept as db_row: %d\n", c.KeptUnmappedClaimed)
	}
	if len(c.SkippedByReason) > 0 {
		fmt.Fprintln(w, "skipped by reason:")
		for _, r := range slices.Sorted(maps.Keys(c.SkippedByReason)) {
			fmt.Fprintf(w, "  %s: %d\n", escapeText(r), c.SkippedByReason[r])
		}
	}
	if len(c.Narrowed) > 0 {
		fmt.Fprintf(w, "narrowed by: %s (a narrowed run is not a full recovery and never supersedes one)\n", escapeText(strings.Join(c.Narrowed, ", ")))
	}
	if len(c.LimitsHit) > 0 {
		fmt.Fprintf(w, "limits reached: %s\n", escapeText(strings.Join(c.LimitsHit, ", ")))
	}
	fmt.Fprintf(w, "secure_delete evidence: %d of %d freelist pages are all zero\n", c.FreePagesZeroed, c.FreePages)
}
