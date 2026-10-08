package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// recoverFlags are the options shared by --list and a run.
type recoverFlags struct {
	list, all, allCandidates, keepUniform bool
	minConfidence                         int
	maxFiles, maxPlanRuns                 int
	maxBytes                              int64
}

func (f recoverFlags) validate(refs []string) error {
	if f.minConfidence < 0 || f.minConfidence > 100 {
		return usageErrorf("--min-confidence must be between 0 and 100, got %d", f.minConfidence)
	}
	if f.maxFiles < 0 {
		return usageErrorf("--max-files must not be negative, got %d", f.maxFiles)
	}
	if f.maxPlanRuns < 0 {
		return usageErrorf("--max-plan-runs must not be negative, got %d", f.maxPlanRuns)
	}
	if f.maxBytes < 0 {
		return usageErrorf("--max-bytes must not be negative, got %d", f.maxBytes)
	}
	if f.all && len(refs) > 0 {
		return usageErrorf("--all and explicit entries or directories are mutually exclusive")
	}
	if !f.list && !f.all && len(refs) == 0 {
		return usageErrorf("choose what to recover: --all, or one or more id:<FSID> / directory paths (--list shows what a run would write)")
	}
	for _, r := range refs {
		if r == "id:" || r == "" {
			return usageErrorf("malformed entry reference %q: expected id:<FSID> or a directory path", r)
		}
	}
	return nil
}

func newImageRecoverCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f recoverFlags
	cmd := &cobra.Command{
		Use:   "recover <ref> ( --list | --all | <id:FSID | dir path>... )",
		Short: "Recover the content of deleted filesystem entries from an image's free space into the case",
		Long: "Recover writes derived artifacts of kind recover, each with its map, confidence and the bytes that were not\n" +
			"captured; only bytes the filesystem reports free are copied. --list prints what a run would write and\n" +
			"writes nothing but the usual case.open audit entry.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return usageErrorf("%s: expected an image reference, got %d argument(s)", cmd.CommandPath(), len(args))
			}
			return nil
		},
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().BoolVar(&f.list, "list", false, "show what a run would write; writes nothing but the usual case.open audit entry")
	cmd.Flags().BoolVar(&f.all, "all", false, "recover every deleted entry")
	cmd.Flags().IntVar(&f.minConfidence, "min-confidence", 0, "skip candidates below this confidence (0..100)")
	cmd.Flags().BoolVar(&f.allCandidates, "all-candidates", false, "write every candidate of an entry, not only the best")
	cmd.Flags().BoolVar(&f.keepUniform, "keep-uniform", false, "write candidates whose bytes are all the same value")
	cmd.Flags().IntVar(&f.maxFiles, "max-files", 0, fmt.Sprintf("stop after this many artifacts (default %d; 0 uses the default)", examine.DefaultMaxFiles))
	cmd.Flags().Int64Var(&f.maxBytes, "max-bytes", 0, fmt.Sprintf("stop after this many bytes (default %d; 0 uses the default)", examine.DefaultMaxBytes))
	cmd.Flags().IntVar(&f.maxPlanRuns, "max-plan-runs", 0, "stop planning after this many runs over all candidates (default 524288, about 260 MiB of memory; more runs cost more memory; 0 uses the default)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		refs := args[1:]
		if err := f.validate(refs); err != nil {
			return err
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		o := examine.RecoverOptions{
			Partition: pidx, Refs: refs, All: f.all || (f.list && len(refs) == 0), MinConfidence: f.minConfidence,
			AllCandidates: f.allCandidates, KeepUniform: f.keepUniform, MaxFiles: f.maxFiles, MaxBytes: f.maxBytes, MaxPlanRuns: f.maxPlanRuns,
		}
		if f.list {
			plan, err := s.PlanRecovery(cmd.Context(), o)
			if err != nil {
				return withFSName(s, pidx, err)
			}
			if opts.json {
				return writeJSON(d.Out, newJSONRecoverList(plan))
			}
			printRecoverList(d.Out, plan)
			return nil
		}
		o.Progress = newProgress(d.Err, "recover")
		sum, err := s.Recover(cmd.Context(), o)
		fmt.Fprintln(d.Err)
		if sum.AnalysisID == "" {
			return withFSName(s, pidx, err)
		}
		if opts.json {
			return writeRecoverJSON(d.Out, sum, err)
		}
		printRecoverRun(d.Out, sum)
		return err
	}
	return cmd
}

// withFSName names the filesystem in an unsupported-recovery error.
func withFSName(s *examine.Session, pidx int, err error) error {
	if err == nil || !errors.Is(err, filesys.ErrNoRecovery) {
		return err
	}
	if fsys, _, ferr := s.FS(pidx); ferr == nil {
		return fmt.Errorf("%s: %w", fsys.Info().Type, err)
	}
	return err
}

// candidateLine is the one line printed for a candidate or a written artifact.
func candidateLine(method string, conf int, band string, size int64, path, id string) string {
	tag := fmt.Sprintf("conf %d", conf)
	if band != "" {
		tag += " " + escapeText(band)
	}
	return fmt.Sprintf("[recovered:%s %s] %d bytes  %s  id:%s", escapeText(method), tag, size, escapeText(path), escapeText(id))
}

func hasOverlap(assumptions []string) bool {
	return slices.ContainsFunc(assumptions, func(a string) bool { return strings.HasPrefix(a, "overlap=") })
}

func printRecoverList(w io.Writer, plan *examine.RecoverPlan) {
	written, bytes := 0, int64(0)
	_, uniform, _ := plan.Counters()
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			line := candidateLine(c.Method, c.Confidence, c.Band, c.Size, it.Path, it.ID)
			if why := c.PartialWhy(); why != "" {
				line += " [incomplete: " + escapeText(why) + "]"
			}
			if c.Encrypted {
				line += " [encrypted]"
			}
			if hasOverlap(c.Assumptions) {
				line += " [overlap]"
			}
			switch c.Skip {
			case "":
				line += " [would write]"
				written++
				bytes += c.Size
			case "uniform":
				line += " [uniform skipped]"
			default:
				line += " [not written: " + escapeText(c.Skip) + ": " + escapeText(c.SkipDetail()) + "]"
			}
			fmt.Fprintln(w, line)
		}
		if len(it.Candidates) > 0 {
			continue // a candidate's skip is already on its own line
		}
		for _, sk := range it.Skips {
			fmt.Fprintf(w, "[skipped: %s] %s  id:%s  %s\n", escapeText(sk.Reason), escapeText(firstNonEmpty(sk.Path, it.Path)), escapeText(firstNonEmpty(sk.ID, it.ID)), escapeText(sk.Detail))
		}
	}
	fmt.Fprintf(w, "would recover %d (%d bytes), uniform %d, skipped %d, considered %d\n", written, bytes, uniform, skippedTotal(plan.SkippedBy), plan.Considered)
	printLimit(w, plan.LimitReached, plan.NotProcessed)
	if n := len(plan.FSWarnings); n > 0 {
		fmt.Fprintf(w, "filesystem warnings: %d\n", n)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func skippedTotal(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func printLimit(w io.Writer, which string, notProcessed int) {
	if which != "" {
		fmt.Fprintf(w, "limit reached: %s (%d not processed)\n", escapeText(which), notProcessed)
	}
}

func printRecoverRun(w io.Writer, sum examine.RecoverSummary) {
	var bytes int64
	for _, a := range sum.Artifacts {
		d := a.Source.Derived
		if a.Source.Kind != evidence.KindRecover || d == nil || d.Recovery == nil {
			continue // runs sidecars are not recovered artifacts
		}
		r := d.Recovery
		conf := 0
		if r.Confidence != nil {
			conf = *r.Confidence
		}
		line := candidateLine(r.Method, conf, evidence.ConfidenceBand(conf), a.Size, d.FSPath, d.FSID)
		if a.Incomplete {
			line += " [incomplete: " + escapeText(a.Error) + "]"
		}
		if d.Encrypted {
			line += " [encrypted]"
		}
		if hasOverlap(r.Assumptions) {
			line += " [overlap]"
		}
		fmt.Fprintln(w, line)
		bytes += a.Size
	}
	fmt.Fprintf(w, "recovered %d (%d bytes), partial %d, uniform %d, skipped %d\n", sum.Recovered, bytes, sum.Partial, sum.Uniform, sum.Skipped)
	printSkipReasons(w, sum.Summary)
	printLimit(w, sum.LimitReached, sum.NotProcessed)
	printFSWarnings(w, sum.Summary)
}

// writeRecoverJSON writes the JSON of a run and returns the run's own error; a failed write is reported
// only when the run had no error, so an integrity error is never replaced by a write failure.
func writeRecoverJSON(w io.Writer, sum examine.RecoverSummary, err error) error {
	if jerr := writeJSON(w, newJSONRecover(sum)); jerr != nil && err == nil {
		return jerr
	}
	return err
}
