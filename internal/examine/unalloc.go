package examine

import (
	"context"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// UnallocOptions select which unallocated space ExportUnallocated copies.
type UnallocOptions struct {
	Partition int  // -1 = auto; ignored when Volume
	Volume    bool // export volume-level gaps instead of a filesystem's free space
	Progress  func(done, total int64)
}

// unallocLine is one line of unallocated.runs.jsonl: where a run sits in
// unallocated.bin and in the parent image.
type unallocLine struct {
	Offset      int64 `json:"offset"`
	Length      int64 `json:"length"`
	ImageOffset int64 `json:"image_offset"`
}

// ExportUnallocated concatenates unallocated space into one derived artifact,
// "unallocated.bin", described by a runs sidecar "unallocated.runs.jsonl"
// (written first). In filesystem mode the space is the filesystem's free
// space; in volume mode it is the part of the image covered by no partition
// and no table structure. Both are bracketed by analysis.* audit entries.
func (s *Session) ExportUnallocated(ctx context.Context, o UnallocOptions) (Summary, error) {
	if err := s.needTable(); err != nil {
		return Summary{}, err
	}
	var (
		fsys     filesys.FileSystem
		part     volume.Partition // zero value for volume mode: whole image
		fsType   string
		dir      = "volume"
		details  = map[string]any{"parent_id": s.Parent.ID, "volume": o.Volume}
		deviceID = s.Parent.Source.DeviceID
	)
	if !o.Volume {
		var err error
		if fsys, part, err = s.FS(o.Partition); err != nil {
			return Summary{}, err
		}
		fsType = fsys.Info().Type
		dir = fmt.Sprintf("p%d-%s", part.Index, fsType)
	}
	details["partition"] = part.Index

	return runAnalysis(s.Case, deviceID, "unalloc", details, func(a *analysis) error {
		if fsys != nil {
			if err := a.watchFS(fsys); err != nil {
				return err
			}
		}
		runs, err := s.unallocatedRuns(a, fsys, part, o.Volume)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return a.warn(dir, "no unallocated space")
		}
		var total int64
		for _, r := range runs {
			total += r.Length // the runs are disjoint and inside the image, so the sum is at most the image size
		}

		d := s.baseDerivation(part, fsType)
		sd := *d
		sidecar, fillErr, err := s.capture(a, dir+"/unallocated.runs.jsonl",
			evidence.Source{Kind: "runs", DeviceID: deviceID, Derived: &sd}, func(w io.Writer) error {
				var off int64
				return writeJSONLines(w, len(runs), func(i int) any {
					l := unallocLine{Offset: off, Length: runs[i].Length, ImageOffset: runs[i].Offset}
					off += runs[i].Length
					return l
				})
			})
		if err != nil {
			return err
		}
		if fillErr != nil { // generated in memory: only the case can fail here
			return &caseWriteError{fillErr}
		}

		d.RunsArtifact = sidecar.ID
		var done int64
		bin, fillErr, err := s.capture(a, dir+"/unallocated.bin",
			evidence.Source{Kind: "unallocated", DeviceID: deviceID, Derived: d}, func(w io.Writer) error {
				tw := &trackWriter{w: w}
				for _, r := range runs {
					n, err := io.Copy(tw, &ctxReader{ctx: ctx, r: io.NewSectionReader(s.Image, r.Offset, r.Length), onRead: func(n int) {
						done += int64(n)
						if o.Progress != nil {
							o.Progress(done, total)
						}
					}})
					switch {
					case tw.err != nil:
						return &caseWriteError{tw.err}
					case err != nil:
						return err
					case n != r.Length:
						return fmt.Errorf("image ended after %d of %d bytes of the run at %d: %w", n, r.Length, r.Offset, io.ErrUnexpectedEOF)
					}
				}
				return nil
			})
		if err != nil {
			return err
		}
		if fillErr != nil {
			return fillErr // the partial unallocated.bin is kept, flagged incomplete
		}
		a.sum.Files++
		a.sum.Bytes += bin.Size
		return nil
	})
}

// unallocatedRuns returns the image-relative runs to export: sorted, merged
// (a hostile filesystem cannot inflate the export with overlapping or
// duplicate runs) and each inside both its container (the partition, or the
// image in volume mode) and the image. Runs that are not are dropped, and one
// analysis.warning says how many, so the recorded provenance is never wrong.
func (s *Session) unallocatedRuns(a *analysis, fsys filesys.FileSystem, part volume.Partition, volumeMode bool) ([]evidence.Run, error) {
	out, note, err := s.unallocatedRunsNote(fsys, part, volumeMode)
	if err != nil {
		return nil, err
	}
	if note != "" {
		if err := a.warn("", note); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unallocatedRunsNote is unallocatedRuns without the audit: it returns the note (empty when every run
// was usable) instead of writing it, so a read-only plan can carry it and a run can audit it.
func (s *Session) unallocatedRunsNote(fsys filesys.FileSystem, part volume.Partition, volumeMode bool) ([]evidence.Run, string, error) {
	var (
		valid    []filesys.Run
		empty    int
		bad      int
		firstBad string
	)
	imageSize := s.Image.Size()
	check := func(r filesys.Run, limit, base int64) {
		if r.Length == 0 {
			empty++
			return
		}
		off, ok1 := filesys.AddOK(r.Offset, base)
		end, ok2 := filesys.AddOK(off, r.Length)
		if r.Offset < 0 || r.Length < 0 || !ok1 || !ok2 || end-base > limit || end > imageSize {
			if bad == 0 {
				firstBad = fmt.Sprintf("%d+%d", r.Offset, r.Length)
			}
			bad++
			return
		}
		valid = append(valid, filesys.Run{Offset: off, Length: r.Length})
	}
	if volumeMode {
		for _, r := range s.Table.Unallocated {
			check(filesys.Run{Offset: r.Offset, Length: r.Length}, imageSize, 0)
		}
	} else {
		raw, err := fsys.Unallocated()
		if err != nil {
			return nil, "", fmt.Errorf("list unallocated space of partition %d: %w", part.Index, err)
		}
		for _, r := range raw {
			check(r, part.Length, part.Start)
		}
	}
	var note string
	if bad > 0 || empty > 0 {
		note = fmt.Sprintf("ignored %d unallocated run(s) outside the %s or with a negative length (first: %s) and %d empty run(s)",
			bad, containerName(volumeMode), firstBad, empty)
		if bad == 0 {
			note = fmt.Sprintf("ignored %d empty unallocated run(s)", empty)
		}
	}
	merged := filesys.MergeRuns(valid)
	out := make([]evidence.Run, len(merged))
	for i, r := range merged {
		out[i] = evidence.Run{Offset: r.Offset, Length: r.Length}
	}
	return out, note, nil
}

func containerName(volumeMode bool) string {
	if volumeMode {
		return "image"
	}
	return "partition or image"
}
