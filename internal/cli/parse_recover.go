package cli

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
)

// limitsFromFlags applies --timeout and --mem-budget to the default limits; a bad value is a usage error
// (exit 2), reported before any case is opened.
func limitsFromFlags(cmd *cobra.Command, timeout, memBudget string) (parse.Limits, error) {
	lim := defaultParseLimits()
	if cmd.Flags().Changed("timeout") {
		dur, err := time.ParseDuration(timeout)
		if err != nil || dur < time.Second || dur > 24*time.Hour {
			return lim, usageErrorf("--timeout %s: want a duration from 1s to 24h (for example 30m)", printable(timeout))
		}
		lim.Timeout = dur
	}
	if cmd.Flags().Changed("mem-budget") {
		n, err := parseSize(memBudget)
		if err != nil {
			return lim, usageErrorf("--mem-budget: %v", err)
		}
		if n < 16<<20 || n > 64<<30 {
			return lim, usageErrorf("--mem-budget %s: want a size from 16MiB to 64GiB", printable(memBudget))
		}
		lim.MemBudget = n
	}
	return lim, nil
}

// limitMemory sets the process memory limit to twice the job budget, but never above a lower limit the
// operator set (GOMEMLIMIT); the returned function restores the previous limit.
func limitMemory(lim parse.Limits) func() {
	cur := debug.SetMemoryLimit(-1)
	prev := debug.SetMemoryLimit(min(cur, 2*lim.MemBudget))
	return func() { debug.SetMemoryLimit(prev) }
}

// runEvents turns the host's events into the status lines (stderr) and, with --json, the NDJSON events
// (stdout) of parse run and parse recover.
type runEvents struct {
	d       Deps
	json    bool
	recover bool
	total   int
	stream  *jsonStream
}

func (re *runEvents) onEvent(e artparse.Event) {
	switch e.Kind {
	case "run.start":
		re.total = e.Jobs
		if re.json {
			re.stream.ndjson(map[string]any{"event": "run.start", "parse_id": e.ParseID, "jobs": e.Jobs})
		}
	case "job.start":
		if re.json {
			r := e.Result
			re.stream.ndjson(map[string]any{
				"event": "job.start", "job": e.Job, "parser": r.Parser, "version": r.Version, "artifact_id": r.ArtifactID,
				"logical_path": r.Logical, "platform": r.Platform,
			})
		}
	case "job.progress":
		if re.json {
			re.stream.ndjson(map[string]any{"event": "job.progress", "job": e.Job, "done": e.Done, "total": e.Total})
		}
	case "job.end":
		if re.recover {
			fmt.Fprintln(re.d.Err, recoverStatusLine(*e.Result, re.total))
			if re.json {
				re.stream.ndjson(recoverJobToJSON("job.end", *e.Result))
			}
			return
		}
		fmt.Fprintln(re.d.Err, statusLine(*e.Result, re.total))
		if re.json {
			re.stream.ndjson(jobToJSON("job.end", *e.Result))
		}
	}
}

// recoverJobJSON is a typed recovery job: the fields of a parse run job plus what only recovery has. The
// counts are always present (zero is a count, not an absence); skipped_invalid and unmapped_overflow make
// a job that did not cover its claims visible.
type recoverJobJSON struct {
	jobJSON
	Claims             []string `json:"claims"`
	LiveParser         string   `json:"live_parser"`
	UnmappedRecovered  int64    `json:"unmapped_recovered"`
	SkippedInvalid     int64    `json:"skipped_invalid"`
	UnmappedOverflow   bool     `json:"unmapped_overflow"`
	BelowMinConfidence int64    `json:"below_min_confidence"`
	Narrowed           []string `json:"narrowed"`
}

func recoverJobToJSON(event string, r artparse.JobResult) recoverJobJSON {
	j := recoverJobJSON{
		jobJSON: jobToJSON(event, r), Claims: append([]string{}, r.Claims...), LiveParser: r.LiveParser,
		UnmappedRecovered: r.Unmapped, SkippedInvalid: r.SkippedInvalid, UnmappedOverflow: r.UnmappedOverflow,
		BelowMinConfidence: r.BelowMinConfidence, Narrowed: append([]string{}, r.Narrowed...),
	}
	return j
}

// recoverStatusLine is the stderr line of one typed recovery job.
func recoverStatusLine(r artparse.JobResult, total int) string {
	line := fmt.Sprintf("[%d/%d] %s %s %s ... %s records, %d unmapped, %d warnings, %.1f s", r.Job, total, printable(r.Parser), printable(r.Version),
		printable(r.ArtifactID), thousands(r.Records), r.Unmapped, r.Warnings, r.Duration.Seconds())
	if r.Outcome != artparse.OutcomeComplete {
		line += " [" + r.Outcome
		if r.Reason != "" {
			line += ": " + escapeText(r.Reason)
		}
		line += "]"
	}
	return line
}

// writeRecoverSummaryText is the summary of parse recover. It says in every case that the records are
// recovered, and names every way a job fell short of covering its claims.
func writeRecoverSummaryText(w io.Writer, sum artparse.RunSummary, verbose bool) {
	counts := map[string]int{}
	var unmapped int64
	for _, j := range sum.Jobs {
		counts[j.Outcome]++
		unmapped += j.Unmapped
	}
	fmt.Fprintf(w, "parse recover %s: %d job(s), %s records, %s unmapped\n", printable(sum.ParseID), len(sum.Jobs), thousands(sum.Records), thousands(unmapped))
	fmt.Fprintln(w, "  recovered records are not live evidence: they are marked deleted and recovered, and their confidence is capped by how the data was recovered")
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
		quiet := j.Outcome == artparse.OutcomeComplete && j.Panic == nil && j.Unmapped == 0 && !j.UnmappedOverflow && j.SkippedInvalid == 0 &&
			len(j.Narrowed) == 0 && (!verbose || len(j.Notes) == 0)
		if quiet || (j.Outcome == artparse.OutcomeSkipped && !verbose) {
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
		if j.Unmapped > 0 {
			fmt.Fprintf(w, "    %s claimed recovered row(s) could not be mapped to a record (each has a warning in the audit log)\n", thousands(j.Unmapped))
		}
		if j.UnmappedOverflow {
			fmt.Fprintln(w, "    warning: more unmapped rows than can be tracked: this job's claims are not honored by sqlite recover")
		}
		if len(j.Narrowed) > 0 {
			fmt.Fprintf(w, "    narrowed (%s): %s record(s) were below the minimum confidence and dropped; this run never replaces a full recovery\n",
				escapeText(strings.Join(j.Narrowed, ", ")), thousands(j.BelowMinConfidence))
		}
		if verbose {
			for _, k := range sortedNotes(j.Notes) {
				fmt.Fprintf(w, "    note %s: %s\n", escapeText(k), escapeText(j.Notes[k]))
			}
		}
	}
}

func newParseRecoverCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f parseSelFlags
	var reparse bool
	var minConfidence int
	var timeout, memBudget string
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Type the recovered rows of the tables a parser claims (deleted messages, calls, contacts)",
		Long: "Run the typed recovery pass of every parser that claims tables: rows recovered from a database's freelist,\n" +
			"old WAL frames or rolled-back journal become typed records (a deleted SMS is a message marked deleted and\n" +
			"recovered) under the identity NAME.recover, never superseding the live parse. Recovered records are not live\n" +
			"evidence: their confidence is capped by how the row, and the artifact it came from, were recovered. A row\n" +
			"that cannot become a valid record is a counted warning, never dropped silently. One status line per job\n" +
			"goes to stderr; the summary (or, with --json, one JSON event per line) goes to stdout. Exit 0 when every\n" +
			"job completed or was skipped, 1 when some job was incomplete, unparsed or refused or the run was narrowed by\n" +
			"--min-confidence, 2 for a usage error (a bad flag, an unknown parser or one that claims no tables),\n" +
			"4 when an input did not match the evidence.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			lim, err := limitsFromFlags(cmd, timeout, memBudget)
			if err != nil {
				return err
			}
			if minConfidence < 0 || minConfidence > 100 {
				return usageErrorf("--min-confidence %d: want a number from 0 to 100", minConfidence)
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
			c, h, err := openHost(f.casePath, regs, lim, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			defer limitMemory(lim)()

			ev := &runEvents{d: d, json: opts.json, recover: true, stream: &jsonStream{w: d.Out, errw: d.Err}}
			sum, runErr := h.RecoverTyped(cmd.Context(), artparse.RecoverOptions{Selection: sel, MinConfidence: minConfidence, OnEvent: ev.onEvent})
			return joinJSON(finishRunMode(d, opts, sum, runErr, true), ev.stream.finish())
		},
	}
	f.register(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&reparse, "reparse", false, "recover again what an earlier typed recovery already covered (the new run supersedes only that earlier recovery)")
	fl.IntVar(&minConfidence, "min-confidence", 0, "drop recovered records below this confidence, 0 to 100 (0 keeps all); a narrowed run is reported incomplete")
	fl.StringVar(&timeout, "timeout", "", "time limit for one recovery job, 1s to 24h (default 1h)")
	fl.StringVar(&memBudget, "mem-budget", "", "memory budget of one recovery job, 16MiB to 64GiB (default 1GiB)")
	return cmd
}
