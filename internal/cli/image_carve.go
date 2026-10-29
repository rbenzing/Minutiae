package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// carveFlags are the options of image carve.
type carveFlags struct {
	artifact                string
	volume, raw, list       bool
	unaligned, nested       bool
	types                   []string
	sigFile                 string
	minConfidence, maxFiles int
	maxBytes, maxFileSize   int64
	partitionSet            bool
}

func (f carveFlags) validate(refs []string) error {
	switch {
	case f.artifact != "" && len(refs) > 0:
		return usageErrorf("an image reference and --artifact are mutually exclusive")
	case f.artifact == "" && len(refs) == 0:
		return usageErrorf("expected an image reference or --artifact <id>")
	case f.volume && (f.partitionSet || f.raw):
		return usageErrorf("--volume cannot be combined with -p/--partition or --raw")
	case f.artifact != "" && (f.partitionSet || f.volume || f.raw):
		return usageErrorf("--artifact cannot be combined with -p/--partition, --volume or --raw (an artifact is read as raw bytes)")
	case f.minConfidence < 0 || f.minConfidence > 100:
		return usageErrorf("--min-confidence must be between 0 and 100, got %d", f.minConfidence)
	case f.maxFiles < 0:
		return usageErrorf("--max-files must not be negative, got %d", f.maxFiles)
	case f.maxBytes < 0:
		return usageErrorf("--max-bytes must not be negative, got %d", f.maxBytes)
	case f.maxFileSize < 0:
		return usageErrorf("--max-file-size must not be negative, got %d", f.maxFileSize)
	}
	return nil
}

// readCarveSigFile reads the examiner signature file through a limit, so a huge file is refused, not loaded.
func readCarveSigFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the examiner names the file
	if err != nil {
		return nil, usageErrorf("--sig-file: %v", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, carve.MaxSigFileBytes+1))
	if err != nil {
		return nil, usageErrorf("--sig-file %s: %v", path, err)
	}
	if len(b) > carve.MaxSigFileBytes {
		return nil, usageErrorf("--sig-file %s is larger than %d bytes", path, carve.MaxSigFileBytes)
	}
	return b, nil
}

// checkCarveTypes refuses a signature file or a type name the carve package does not know, before the case is
// opened, naming the valid type names.
func checkCarveTypes(types []string, sig []byte) error {
	set := carve.Builtin()
	if sig != nil {
		var err error
		if set, err = carve.ParseSigFile(sig, set); err != nil {
			return usageErrorf("--sig-file: %v", err)
		}
	}
	if _, _, err := set.Select(types); err != nil {
		var valid []string
		for _, t := range set.Types {
			valid = append(valid, t.Name)
		}
		for fam, names := range carve.RefinedTypes() {
			if slices.ContainsFunc(set.Types, func(t carve.Type) bool { return t.Name == fam }) {
				valid = append(valid, names...)
			}
		}
		slices.Sort(valid)
		return usageErrorf("--types: %v (valid names: %s)", err, strings.Join(slices.Compact(valid), ", "))
	}
	return nil
}

func newImageCarveCmd(d Deps, opts *rootOptions) *cobra.Command {
	var f carveFlags
	cmd := &cobra.Command{
		Use:   "carve [<ref>] ( -p N | --volume | --raw | --artifact ID ) [--types a,b] [--sig-file F] [--list]",
		Short: "Carve whole objects by signature out of unallocated space, volume gaps, raw bytes or a case artifact",
		Long: "Carve scans a scope for the signatures of known file types and writes each validated object as a derived\n" +
			"artifact of kind carve (class carved, never live evidence). The default scope is the free space of the\n" +
			"filesystem; --volume is the image outside every partition, --raw a partition or the whole image, and\n" +
			"--artifact the bytes of one case artifact (an unallocated export keeps its run boundaries). --list prints what\n" +
			"a run would write and writes nothing but the usual case.open audit entry.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageErrorf("%s: expected at most one image reference, got %d argument(s)", cmd.CommandPath(), len(args))
			}
			return nil
		},
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem; with --raw, the whole image)")
	cmd.Flags().StringVar(&f.artifact, "artifact", "", "carve the bytes of this case artifact instead of an image")
	cmd.Flags().BoolVar(&f.volume, "volume", false, "carve the image bytes outside every partition and table structure")
	cmd.Flags().BoolVar(&f.raw, "raw", false, "carve a partition (-p) or the whole image, allocated bytes included")
	cmd.Flags().StringSliceVar(&f.types, "types", nil, "carve only these types (family or refined names, comma separated; default all)")
	cmd.Flags().StringVar(&f.sigFile, "sig-file", "", fmt.Sprintf("examiner signature file adding types (JSON, at most %d bytes)", carve.MaxSigFileBytes))
	cmd.Flags().BoolVar(&f.unaligned, "unaligned", false, "try headers at every byte, not only at block or sector starts")
	cmd.Flags().BoolVar(&f.nested, "nested", false, "also report objects found inside another carved object")
	cmd.Flags().IntVar(&f.minConfidence, "min-confidence", examine.DefaultCarveMinConfidence, "skip hits below this confidence (0..100)")
	cmd.Flags().BoolVar(&f.list, "list", false, "show what a run would write; writes nothing but the usual case.open audit entry")
	cmd.Flags().IntVar(&f.maxFiles, "max-files", 0, fmt.Sprintf("stop after this many artifacts (default %d; 0 uses the default)", examine.DefaultMaxFiles))
	cmd.Flags().Int64Var(&f.maxBytes, "max-bytes", 0, fmt.Sprintf("stop after this many bytes (default %d; 0 uses the default)", examine.DefaultMaxBytes))
	cmd.Flags().Int64Var(&f.maxFileSize, "max-file-size", 0, "lower every type's maximum object size to this many bytes (0 keeps the type limits)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		f.partitionSet = cmd.Flags().Changed("partition")
		if err := f.validate(args); err != nil {
			return err
		}
		var sig []byte
		if f.sigFile != "" {
			if sig, err = readCarveSigFile(f.sigFile); err != nil {
				return err
			}
		}
		if err := checkCarveTypes(f.types, sig); err != nil {
			return err
		}
		o := examine.CarveOptions{
			Partition: pidx, Types: f.types, SigFile: sig, Unaligned: f.unaligned, Nested: f.nested,
			MinConfidence: f.minConfidence, MaxFiles: f.maxFiles, MaxBytes: f.maxBytes, MaxFileSize: f.maxFileSize,
		}
		switch {
		case f.volume:
			o.Scope = examine.CarveVolume
		case f.raw:
			o.Scope = examine.CarveRaw
		}
		s, closeAll, err := openCarveSession(d, *casePath, args, f.artifact)
		if err != nil {
			return err
		}
		defer closeAll()
		if f.list {
			plan, err := s.PlanCarve(cmd.Context(), o)
			if err != nil {
				return carveSessionErr(err, o.Scope, f.artifact != "")
			}
			err = carvePartialFacts(plan.Stats.UnreadableBytes, 0, plan.LimitReached, plan.Stats.TypeCapped)
			if opts.json {
				return joinJSON(err, writeJSON(d.Out, newJSONCarveList(plan)))
			}
			printCarveList(d.Out, plan)
			return err
		}
		o.Progress = newProgress(d.Err, "carve")
		sum, err := s.Carve(cmd.Context(), o)
		fmt.Fprintln(d.Err)
		if sum.AnalysisID == "" {
			return carveSessionErr(err, o.Scope, f.artifact != "")
		}
		err = errors.Join(err, carveIncomplete(sum))
		if opts.json {
			return joinJSON(err, writeJSON(d.Out, newJSONCarve(sum)))
		}
		printCarveRun(d.Out, sum)
		return err
	}
	return cmd
}

// openCarveSession opens the case and either the image ref or the artifact id as the scope to carve.
func openCarveSession(d Deps, casePath string, refs []string, artifact string) (*examine.Session, func(), error) {
	if artifact == "" {
		return openImageSession(d, casePath, refs[0])
	}
	c, err := openCase(casePath)
	if err != nil {
		return nil, nil, err
	}
	s, err := examine.OpenArtifactScope(c, artifact, examine.Options{Drivers: d.FSDrivers})
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	warnIncomplete(d, s)
	return s, func() { _ = s.Close(); _ = c.Close() }, nil
}

// carveSessionErr adds the hint a failed default scope needs: a partition without a filesystem can still be carved raw.
func carveSessionErr(err error, scope string, artifact bool) error {
	if err == nil || scope != "" || artifact {
		return err
	}
	if errors.Is(err, examine.ErrNoRecognizedFilesystem) || errors.Is(err, filesys.ErrUnsupported) {
		return fmt.Errorf("%w (carve the bytes without a filesystem with --raw, or --volume for the space outside the partitions)", err)
	}
	return err
}

// carvePartial is a run that finished but did not cover everything it was asked to: the work is written and
// printed, and the exit is 1 (never 0, and never hiding an integrity failure, which ExitCode tests first).
type carvePartial struct{ reasons []string }

func (p *carvePartial) Error() string { return "carve incomplete: " + strings.Join(p.reasons, "; ") }

// carveIncomplete says why a finished run is not clean. Summary.Skipped does not count these facts.
func carveIncomplete(s examine.CarveSummary) error {
	return carvePartialFacts(s.Stats.UnreadableBytes, s.UncopiedBytes, s.LimitReached, s.Stats.TypeCapped)
}

func carvePartialFacts(unreadable, uncopied int64, limit string, capped []string) error {
	var why []string
	if unreadable > 0 {
		why = append(why, fmt.Sprintf("%d byte(s) of the scope were unreadable", unreadable))
	}
	if uncopied > 0 {
		why = append(why, fmt.Sprintf("%d byte(s) of accepted hits were not copied", uncopied))
	}
	if limit != "" {
		why = append(why, "limit reached: "+escapeText(limit))
	}
	if len(capped) > 0 {
		why = append(why, "type(s) capped: "+escapeText(strings.Join(capped, ", ")))
	}
	if len(why) == 0 {
		return nil
	}
	return &carvePartial{reasons: why}
}

func outcomeTotals(m map[carve.Outcome]int) string {
	return fmt.Sprintf("validated %d footer %d bounded %d", m[carve.Validated], m[carve.Footer], m[carve.Bounded])
}

func rejectedTotal(m map[string]int) int { return skippedTotal(m) }

func hitLine(h examine.CarveHit) string {
	line := strings.TrimSuffix(candidateLine(h.Method, h.Confidence, evidence.ConfidenceBand(h.Confidence), h.Length, h.Name, ""), "  id:")
	line += fmt.Sprintf(" at 0x%x", h.Offset)
	if h.Alloc.Allocated > 0 {
		line += fmt.Sprintf(" [allocated %d]", h.Alloc.Allocated)
	}
	if h.NestedIn >= 0 {
		line += " [nested]"
	}
	return line
}

func printCarveList(w io.Writer, p *examine.CarvePlan) {
	written, bytes := 0, int64(0)
	for _, h := range p.Hits {
		line := hitLine(h)
		if h.Skip == "" {
			line += " [would write]"
			written++
			bytes += h.Length
		} else {
			line += " [not written: " + escapeText(h.Skip) + "]"
		}
		fmt.Fprintln(w, line)
	}
	if p.HitsNotListed > 0 {
		fmt.Fprintf(w, "%d further hits not listed\n", p.HitsNotListed)
	}
	fmt.Fprintf(w, "scope %s: %d run(s), %d bytes, aligned to %d\n", escapeText(p.Scope), p.ScopeRuns, p.ScopeBytes, p.Align)
	fmt.Fprintf(w, "would carve %d (%d bytes); hits %s; rejected %d; skipped %d\n", written, bytes, outcomeTotals(p.Stats.Accepted), rejectedTotal(p.Stats.Rejected), skippedTotal(p.SkippedBy))
	printCarveTail(w, p.Stats, p.LimitReached, p.NotScannedBytes, 0)
	for _, n := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", escapeText(n))
	}
}

// printCarveTail prints the facts that make a run partial and the counters a clean run still reports.
func printCarveTail(w io.Writer, st carve.Stats, limit string, notScanned, uncopied int64) {
	if limit != "" {
		fmt.Fprintf(w, "limit reached: %s (%d byte(s) of the scope were not scanned)\n", escapeText(limit), notScanned)
	}
	if st.UnreadableBytes > 0 {
		fmt.Fprintf(w, "unreadable: %d byte(s) of the scope could not be read and were not scanned\n", st.UnreadableBytes)
	}
	if uncopied > 0 {
		fmt.Fprintf(w, "not copied: %d byte(s) of accepted hits could not be read (partial artifacts are flagged incomplete)\n", uncopied)
	}
	if len(st.TypeCapped) > 0 {
		fmt.Fprintf(w, "types capped at %d hits (later hits of these types were not matched): %s\n", carve.DefaultMaxHitsPerType, escapeText(strings.Join(st.TypeCapped, ", ")))
	}
	if st.NestedDepthSkipped > 0 {
		fmt.Fprintf(w, "nested objects deeper than the nesting limit were not reported: %d\n", st.NestedDepthSkipped)
	}
	if n := skippedTotal(st.Rejected); n > 0 {
		keys := make([]string, 0, len(st.Rejected))
		for k := range st.Rejected {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", escapeText(k), st.Rejected[k]))
		}
		fmt.Fprintf(w, "rejected headers by reason: %s\n", strings.Join(parts, ", "))
	}
}

func printCarveRun(w io.Writer, sum examine.CarveSummary) {
	var bytes int64
	for _, a := range sum.Artifacts {
		d := a.Source.Derived
		if a.Source.Kind != evidence.KindCarve || d == nil || d.Recovery == nil || len(d.Runs) == 0 {
			continue
		}
		r := d.Recovery
		conf := 0
		if r.Confidence != nil {
			conf = *r.Confidence
		}
		line := candidateLine(r.Method, conf, evidence.ConfidenceBand(conf), a.Size, a.Path, a.ID)
		line += fmt.Sprintf(" at 0x%x", d.Runs[0].Offset)
		if a.Incomplete {
			line += " [incomplete: " + escapeText(a.Error) + "]"
		}
		if r.Alloc.Allocated > 0 {
			line += fmt.Sprintf(" [allocated %d]", r.Alloc.Allocated)
		}
		if slices.ContainsFunc(r.Assumptions, func(x string) bool { return strings.HasPrefix(x, "nested-in=") }) {
			line += " [nested]"
		}
		fmt.Fprintln(w, line)
		bytes += a.Size
	}
	fmt.Fprintf(w, "carved %d (%d bytes), partial %d; hits %s; rejected %d; skipped %d\n",
		sum.Carved, bytes, sum.Partial, outcomeTotals(sum.Stats.Accepted), rejectedTotal(sum.Stats.Rejected), skippedTotal(sum.SkippedBy))
	printCarveTail(w, sum.Stats, sum.LimitReached, sum.NotScannedBytes, sum.UncopiedBytes)
	printCarveNotes(w, sum.Notes, sum.NotesTotal)
	for i, wn := range sum.Warnings {
		if i == maxSkipReasons {
			break
		}
		if wn.Path == "" {
			continue // a note: printed by printCarveNotes
		}
		fmt.Fprintf(w, "  skipped %s: %s\n", escapeText(wn.Path), escapeText(wn.Reason))
	}
	if sum.Skipped > maxSkipReasons {
		fmt.Fprintln(w, "  ... see analysis.warning entries in audit.jsonl")
	}
	printFSWarnings(w, sum.Summary)
	fmt.Fprintln(w, "recovered data is not live evidence: carved objects are contiguous guesses from signatures, with confidence capped by how they were found")
}

// maxCarveNotesShown is how many notes a run prints; the rest are in the audit log.
const maxCarveNotesShown = 20

func printCarveNotes(w io.Writer, notes []string, total int) {
	for i, n := range notes {
		if i == maxCarveNotesShown {
			break
		}
		fmt.Fprintf(w, "note: %s\n", escapeText(n))
	}
	if more := total - min(len(notes), maxCarveNotesShown); more > 0 {
		fmt.Fprintf(w, "note: %d more; see the audit log (analysis.warning entries)\n", more)
	}
}
