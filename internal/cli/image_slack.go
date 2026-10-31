package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
)

// jsonSlack is the JSON of an image slack run (strings are the raw ones; artifacts are manifest records).
// Volume figures are what was written: 0 when the volume part was not reached.
type jsonSlack struct {
	AnalysisID          string                    `json:"analysis_id"`
	Partition           int                       `json:"partition"`
	FSType              string                    `json:"fs_type"`
	Unit                int64                     `json:"unit"`
	FilesExamined       int                       `json:"files_examined"`
	FileSegments        int                       `json:"file_segments"`
	FileBytes           int64                     `json:"file_bytes"`
	UniformSegments     int                       `json:"uniform_segments"`
	UniformBytes        int64                     `json:"uniform_bytes"`
	VolumeSegments      int                       `json:"volume_segments"`
	VolumeBytes         int64                     `json:"volume_bytes"`
	VolumeUniformChunks int                       `json:"volume_uniform_chunks"`
	VolumeUniformBytes  int64                     `json:"volume_uniform_bytes"`
	SkippedBy           map[string]int            `json:"skipped_by"`
	VolumeSkippedBy     map[string]int            `json:"volume_skipped_by"`
	VolumeClippedBytes  int64                     `json:"volume_clipped_bytes"`
	Skipped             int                       `json:"skipped"`
	Warnings            []jsonSlackWarning        `json:"warnings"`
	WalkErrors          int                       `json:"walk_errors"`
	LimitReached        string                    `json:"limit_reached,omitempty"`
	Incomplete          bool                      `json:"incomplete"`
	FSWarnings          int                       `json:"fs_warnings"`
	Artifacts           []evidence.ManifestRecord `json:"artifacts"`
}

// jsonSlackWarning is one plan warning: Path is "" for a warning about the run (the volume part), raw as in the audit log.
type jsonSlackWarning struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func newJSONSlack(s examine.SlackSummary) jsonSlack {
	j := jsonSlack{
		AnalysisID: s.AnalysisID, Partition: s.Partition, FSType: s.FSType, Unit: s.Unit, FilesExamined: s.FilesExamined,
		FileSegments: s.FileSegments, FileBytes: s.FileBytes, UniformSegments: s.UniformSegments, UniformBytes: s.UniformBytes,
		VolumeSegments: s.VolumeSegments, VolumeBytes: s.VolumeBytes, VolumeUniformChunks: s.VolumeUniformChunks,
		VolumeUniformBytes: s.VolumeUniformBytes, SkippedBy: s.SkippedBy, WalkErrors: s.WalkErrors, LimitReached: s.LimitReached,
		Incomplete: s.Incomplete, FSWarnings: s.FSWarnings, Artifacts: s.Artifacts,
		VolumeSkippedBy: s.VolumeSkippedBy, VolumeClippedBytes: s.VolumeClipped, Skipped: s.Skipped, Warnings: []jsonSlackWarning{},
	}
	if j.SkippedBy == nil {
		j.SkippedBy = map[string]int{}
	}
	if j.VolumeSkippedBy == nil {
		j.VolumeSkippedBy = map[string]int{}
	}
	for _, w := range s.Warnings {
		j.Warnings = append(j.Warnings, jsonSlackWarning{Path: w.Path, Reason: w.Reason})
	}
	if j.Artifacts == nil {
		j.Artifacts = []evidence.ManifestRecord{}
	}
	return j
}

func newImageSlackCmd(d Deps, opts *rootOptions) *cobra.Command {
	var files, volume, includeZero bool
	var maxBytes int64
	cmd := &cobra.Command{
		Use:   "slack <ref> [-p N] [--files|--volume] [--include-zero]",
		Short: "Export file slack and volume slack of a partition's filesystem as labelled residue",
		Long: "Slack writes derived artifacts of kind slack: the bytes after the end of each live file in its last allocation\n" +
			"unit (file slack) and the partition bytes after the end of the filesystem (volume slack). They are residue of\n" +
			"earlier content, never live data. Uniform segments are counted, not exported, unless --include-zero.",
		Args: exactArgs(1),
	}
	casePath := caseFlag(cmd)
	vp := verifyParentFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().BoolVar(&files, "files", false, "export only file slack")
	cmd.Flags().BoolVar(&volume, "volume", false, "export only volume slack")
	cmd.Flags().BoolVar(&includeZero, "include-zero", false, "also export segments whose bytes are all the same value")
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0, fmt.Sprintf("stop after this many bytes (default %d; 0 uses the default)", examine.DefaultMaxBytes))
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		if files && volume {
			return usageErrorf("--files and --volume are mutually exclusive")
		}
		if maxBytes < 0 {
			return usageErrorf("--max-bytes must not be negative, got %d", maxBytes)
		}
		s, closeAll, err := openImageSessionWith(d, *casePath, args[0], openOpts{ctx: cmd.Context(), verifyParent: *vp})
		if err != nil {
			return err
		}
		defer closeAll()
		prog, endProg := newLineProgress(d.Err, "slack")
		sum, err := s.ExportSlack(cmd.Context(), examine.SlackOptions{
			Partition: pidx, Files: files, Volume: volume, IncludeZero: includeZero, MaxBytes: maxBytes, Progress: prog,
		})
		endProg()
		if sum.AnalysisID == "" {
			return err
		}
		if opts.json {
			return joinJSON(err, writeJSON(d.Out, newJSONSlack(sum)))
		}
		printSlackRun(d.Out, sum, files || !volume, volume || !files)
		return err
	}
	return cmd
}

func printSlackRun(w io.Writer, sum examine.SlackSummary, doFiles, doVol bool) {
	fmt.Fprintf(w, "slack of partition %d (%s, unit %d bytes): %d live files examined\n", sum.Partition, escapeText(sum.FSType), sum.Unit, sum.FilesExamined)
	if doFiles {
		fmt.Fprintf(w, "file slack: %d segment(s) exported (%s); %d uniform segment(s) counted, not exported (%s)\n",
			sum.FileSegments, humanBytes(sum.FileBytes), sum.UniformSegments, humanBytes(sum.UniformBytes))
	}
	var parts []string
	for _, r := range slices.Sorted(maps.Keys(sum.SkippedBy)) {
		if n := sum.SkippedBy[r]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", escapeText(r), n))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "skipped: %s\n", strings.Join(parts, ", "))
	}
	if doVol {
		fmt.Fprintf(w, "volume slack: %d segment(s) exported (%s); %d uniform chunk(s) counted, not exported (%s)\n",
			sum.VolumeSegments, humanBytes(sum.VolumeBytes), sum.VolumeUniformChunks, humanBytes(sum.VolumeUniformBytes))
	}
	if doVol {
		var vparts []string
		for _, r := range slices.Sorted(maps.Keys(sum.VolumeSkippedBy)) {
			if n := sum.VolumeSkippedBy[r]; n > 0 {
				vparts = append(vparts, fmt.Sprintf("%s %d", escapeText(r), n))
			}
		}
		if len(vparts) > 0 {
			fmt.Fprintf(w, "volume skipped: %s\n", strings.Join(vparts, ", "))
		}
		if sum.VolumeClipped > 0 {
			fmt.Fprintf(w, "volume slack clipped: %s of the partition lie past the end of the image\n", humanBytes(sum.VolumeClipped))
		}
	}
	firstIncomplete := ""
	for _, a := range sum.Artifacts {
		d := a.Source.Derived
		if a.Source.Kind != evidence.KindSlack || d == nil || d.Recovery == nil {
			continue // runs sidecars are not slack artifacts
		}
		r := d.Recovery
		conf := 0
		if r.Confidence != nil {
			conf = *r.Confidence
		}
		line := candidateLine(r.Method, conf, evidence.ConfidenceBand(conf), a.Size, a.Path, a.ID)
		if a.Incomplete {
			line += " [incomplete]"
			if firstIncomplete == "" {
				firstIncomplete = a.Error
			}
		}
		fmt.Fprintln(w, line)
	}
	switch {
	case sum.WalkErrors > 0:
		noun := "directory"
		if sum.WalkErrors > 1 {
			noun = "directories"
		}
		fmt.Fprintf(w, "INCOMPLETE: %d %s not read to the end; slack of the files in them is unknown\n", sum.WalkErrors, noun)
	case sum.Incomplete:
		fmt.Fprintf(w, "INCOMPLETE: %s\n", escapeText(firstIncomplete))
	}
	if sum.LimitReached != "" {
		fmt.Fprintf(w, "limit reached: %s\n", escapeText(sum.LimitReached))
	}
	for i, wn := range sum.Warnings {
		if i == maxSkipReasons {
			break
		}
		if wn.Path == "" {
			fmt.Fprintf(w, "  warning: %s\n", escapeText(wn.Reason))
			continue
		}
		fmt.Fprintf(w, "  warning %s: %s\n", escapeText(wn.Path), escapeText(wn.Reason))
	}
	if sum.Skipped > maxSkipReasons {
		fmt.Fprintln(w, "  ... see analysis.warning entries in audit.jsonl")
	}
	printFSWarnings(w, sum.Summary)
}
