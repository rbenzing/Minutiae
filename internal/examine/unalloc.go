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
		runs, err := s.unallocatedRuns(a, fsys, part, o.Volume)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return a.warn(dir, "no unallocated space")
		}
		var total int64
		for _, r := range runs {
			total += r.Length // cannot overflow: each run ends inside the image
		}

		d := s.baseDerivation(part, fsType)
		sd := *d
		sidecar, err := s.Case.Capture(deviceID, a.sum.AnalysisID, dir+"/unallocated.runs.jsonl",
			evidence.Source{Kind: "runs", DeviceID: deviceID, Derived: &sd}, func(w io.Writer) error {
				var off int64
				return writeJSONLines(w, len(runs), func(i int) any {
					l := unallocLine{Offset: off, Length: runs[i].Length, ImageOffset: runs[i].Offset}
					off += runs[i].Length
					return l
				})
			})
		a.add(sidecar)
		if err != nil {
			return err
		}

		d.RunsArtifact = sidecar.ID
		var done int64
		bin, err := s.Case.Capture(deviceID, a.sum.AnalysisID, dir+"/unallocated.bin",
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
		a.add(bin)
		if err != nil {
			return err
		}
		a.sum.Files++
		a.sum.Bytes += bin.Size
		return nil
	})
}

// unallocatedRuns returns the image-relative runs to export, dropping (with
// one analysis.warning) any run the source reports that is empty, negative
// or outside its container, so the recorded provenance is never wrong.
func (s *Session) unallocatedRuns(a *analysis, fsys filesys.FileSystem, part volume.Partition, volumeMode bool) ([]evidence.Run, error) {
	var (
		out      []evidence.Run
		bad      int
		firstBad string
	)
	imageSize := s.Image.Size()
	check := func(r evidence.Run, limit, base int64) {
		off, ok1 := filesys.AddOK(r.Offset, base)
		end, ok2 := filesys.AddOK(off, r.Length)
		if r.Offset < 0 || r.Length <= 0 || !ok1 || !ok2 || end-base > limit || end > imageSize {
			if bad == 0 {
				firstBad = fmt.Sprintf("%d+%d", r.Offset, r.Length)
			}
			bad++
			return
		}
		out = append(out, evidence.Run{Offset: off, Length: r.Length})
	}
	if volumeMode {
		for _, r := range s.Table.Unallocated {
			check(evidence.Run{Offset: r.Offset, Length: r.Length}, imageSize, 0)
		}
	} else {
		raw, err := fsys.Unallocated()
		if err != nil {
			return nil, fmt.Errorf("list unallocated space of partition %d: %w", part.Index, err)
		}
		for _, r := range raw {
			check(evidence.Run{Offset: r.Offset, Length: r.Length}, part.Length, part.Start)
		}
	}
	if bad > 0 {
		if err := a.warn("", fmt.Sprintf("ignored %d unallocated run(s) outside the %s (first: %s)", bad, containerName(volumeMode), firstBad)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func containerName(volumeMode bool) string {
	if volumeMode {
		return "image"
	}
	return "partition or image"
}
