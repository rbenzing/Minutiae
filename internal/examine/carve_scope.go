package examine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// The scopes a carve reads (Recovery.Scope of the carved artifacts).
const (
	CarveUnallocated = "unallocated" // the free space of one filesystem
	CarveVolume      = "volume"      // the image bytes outside every partition and table structure
	CarveRaw         = "raw"         // a partition or the whole image, allocated bytes included
	CarveArtifact    = "artifact"    // one case artifact read as raw bytes
)

// DefaultCarveMinConfidence is the confidence floor below which an accepted hit is listed but not written.
const DefaultCarveMinConfidence = 40

// ErrInvalidCarveOption is a carve option outside its range or an unknown scope (a usage error).
var ErrInvalidCarveOption = errors.New("invalid carve option")

// CarveOptions select what a carve reads and what it keeps.
type CarveOptions struct {
	Partition         int    // -1 = auto (unallocated: the only recognised filesystem; raw: the whole image)
	Scope             string // "" = unallocated (an artifact session: artifact)
	Types             []string
	SigFile           []byte // examiner signature file bytes, nil = none
	Unaligned, Nested bool
	MinConfidence     int // 0..100
	MaxFiles          int
	MaxBytes          int64
	MaxFileSize       int64 // 0 = defaults; MaxFileSize lowers each type's MaxLen only
	Progress          func(done, total int64)
}

// carveScope is what a carve reads: runs of the session's image that no hit may leave, and how hits in them are
// aligned, classified and named.
type carveScope struct {
	Scope, Dir string      // Recovery.Scope; the directory below carved/
	Runs       []carve.Run // sorted, disjoint, inside the reader
	Align      int64
	Part       volume.Partition // scope unallocated
	FSType     string           // scope unallocated
	Alloc      *allocClassifier // scope raw
	Notes      []string         // audited as non-skip warnings
	ExportID   string           // the unallocated export this scope came from
	ArtifactID string           // scope artifact
	Bytes      int64
}

func carveOptionErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCarveOption, fmt.Sprintf(format, a...))
}

// check validates the numbers and the scope name before anything is read.
func (o CarveOptions) check() error {
	switch {
	case o.MaxFiles < 0, o.MaxBytes < 0, o.MaxFileSize < 0:
		return fmt.Errorf("%w: max files %d, max bytes %d, max file size %d (0 selects the default)", ErrInvalidLimit, o.MaxFiles, o.MaxBytes, o.MaxFileSize)
	case o.MinConfidence < 0 || o.MinConfidence > 100:
		return carveOptionErr("minimum confidence %d is outside 0..100", o.MinConfidence)
	case o.Partition < -1:
		return carveOptionErr("partition %d", o.Partition)
	}
	switch o.Scope {
	case "", CarveUnallocated, CarveVolume, CarveRaw, CarveArtifact:
		return nil
	}
	return carveOptionErr("unknown scope %q (want %s, %s, %s or %s)", o.Scope, CarveUnallocated, CarveVolume, CarveRaw, CarveArtifact)
}

// buildCarveScope resolves the scope of o over the session: 3E-R3 (runs), 3E-R4 (alignment) and 3E-R6
// (allocation state). It reads the table and the filesystems, never the carved bytes.
func (s *Session) buildCarveScope(o CarveOptions) (*carveScope, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	switch {
	case s.carveArtifact != nil:
		if o.Scope != "" && o.Scope != CarveArtifact {
			return nil, carveOptionErr("scope %q does not apply to artifact %s, which is read as raw bytes", o.Scope, s.carveArtifact.ID)
		}
		return s.artifactScope(o), nil
	case o.Scope == CarveArtifact:
		return nil, carveOptionErr("scope %s needs an artifact session (OpenArtifactScope)", CarveArtifact)
	case s.carveExport != nil:
		return s.exportScope(o)
	}
	switch o.Scope {
	case CarveVolume:
		return s.volumeScope(o)
	case CarveRaw:
		return s.rawScope(o)
	}
	return s.unallocatedScope(o)
}

// alignFor is the header unit of the scopes other than unallocated.
func alignFor(o CarveOptions) int64 {
	if o.Unaligned {
		return 1
	}
	return 512
}

func sumRuns(runs []carve.Run) int64 {
	var n int64
	for _, r := range runs {
		n += r.Length // disjoint and inside the image: the sum is at most its size
	}
	return n
}

func toCarveRuns(rs []evidence.Run) []carve.Run {
	out := make([]carve.Run, len(rs))
	for i, r := range rs {
		out[i] = carve.Run{Offset: r.Offset, Length: r.Length}
	}
	return out
}

func (s *Session) artifactScope(o CarveOptions) *carveScope {
	sc := &carveScope{Scope: CarveArtifact, Dir: "raw", Align: alignFor(o), ArtifactID: s.carveArtifact.ID}
	if n := s.Image.Size(); n > 0 {
		sc.Runs = []carve.Run{{Offset: 0, Length: n}}
		sc.Bytes = n
	}
	return sc
}

func (s *Session) volumeScope(o CarveOptions) (*carveScope, error) {
	if err := s.needTable(); err != nil {
		return nil, err
	}
	runs, note, err := s.unallocatedRunsNote(nil, volume.Partition{}, true)
	if err != nil {
		return nil, err
	}
	sc := &carveScope{Scope: CarveVolume, Dir: "volume", Align: alignFor(o)}
	sc.Runs = toCarveRuns(runs)
	sc.Bytes = sumRuns(sc.Runs)
	if note != "" {
		sc.Notes = append(sc.Notes, note)
	}
	return sc, nil
}

func (s *Session) rawScope(o CarveOptions) (*carveScope, error) {
	sc := &carveScope{Scope: CarveRaw, Dir: "raw", Align: alignFor(o)}
	size := s.Image.Size()
	if o.Partition < 0 && s.Table == nil {
		// an unreadable table does not keep the media from being carved: nothing says what is allocated
		sc.Alloc = newUnknownClassifier(size)
		if size > 0 {
			sc.Runs = []carve.Run{{Offset: 0, Length: size}}
		}
		note := "the partition table could not be read, so the allocation state of every carved object is unknown"
		if s.tableErr != nil {
			note = fmt.Sprintf("%s (%v)", note, s.tableErr)
		}
		sc.Notes = append(sc.Notes, note)
		sc.Bytes = sumRuns(sc.Runs)
		return sc, nil
	}
	a, err := s.newAllocClassifier()
	if err != nil {
		return nil, err
	}
	sc.Alloc = a
	if o.Partition >= 0 {
		p, ok := s.partitionByIndex(o.Partition)
		if !ok {
			return nil, fmt.Errorf("no partition %d (have %s)", o.Partition, indexList(s.partitions()))
		}
		for _, r := range a.regions {
			if r.Kind != regionTable && r.Partition == p.Index {
				sc.Runs = append(sc.Runs, carve.Run{Offset: r.Start, Length: r.End - r.Start})
			}
		}
		if len(sc.Runs) == 0 {
			sc.Notes = append(sc.Notes, fmt.Sprintf("partition %d lies outside the image: nothing to carve", p.Index))
		}
	} else {
		for _, r := range a.regions {
			sc.Runs = append(sc.Runs, carve.Run{Offset: r.Start, Length: r.End - r.Start})
		}
	}
	sc.Bytes = sumRuns(sc.Runs)
	return sc, nil
}

// unallocatedFS opens the filesystem a scope unallocated reads. A partition with no recognised filesystem is
// the existing error with the hint that raw can still carve it.
func (s *Session) unallocatedFS(index int) (filesys.FileSystem, volume.Partition, error) {
	fsys, part, err := s.FS(index)
	if err != nil {
		if errors.Is(err, filesys.ErrUnsupported) || strings.Contains(err.Error(), "recognized filesystem") {
			return nil, part, fmt.Errorf("%w; use --raw to carve a partition without a recognized filesystem", err)
		}
		return nil, part, err
	}
	return fsys, part, nil
}

// unallocatedAlign is the block size of the filesystem when it is a multiple of 512 in 512..64 MiB, else the
// sector grid and a note (3E-R4).
func unallocatedAlign(fsys filesys.FileSystem, o CarveOptions) (int64, []string) {
	if o.Unaligned {
		return 1, nil
	}
	bs := int64(fsys.Info().BlockSize)
	if bs >= 512 && bs <= 64<<20 && bs%512 == 0 {
		return bs, nil
	}
	return 512, []string{fmt.Sprintf("the filesystem reports block size %d, which is no multiple of 512 in 512..64 MiB: headers are tried every 512 bytes", bs)}
}

func (s *Session) unallocatedScope(o CarveOptions) (*carveScope, error) {
	fsys, part, err := s.unallocatedFS(o.Partition)
	if err != nil {
		return nil, err
	}
	runs, note, err := s.unallocatedRunsNote(fsys, part, false)
	if err != nil {
		return nil, err
	}
	fsType := fsys.Info().Type
	sc := &carveScope{Scope: CarveUnallocated, Dir: fmt.Sprintf("p%d-%s", part.Index, fsType), Part: part, FSType: fsType}
	var anote []string
	sc.Align, anote = unallocatedAlign(fsys, o)
	sc.Notes = append(sc.Notes, anote...)
	if note != "" {
		sc.Notes = append(sc.Notes, note)
	}
	sc.Runs = toCarveRuns(runs)
	sc.Bytes = sumRuns(sc.Runs)
	return sc, nil
}

// exportScope is the scope of an unallocated export: the runs it recorded, intersected with what is unallocated
// now (the media may be read through a reader whose view has changed). A difference is one note naming the
// bytes dropped (3E-R3).
func (s *Session) exportScope(o CarveOptions) (*carveScope, error) {
	exp := *s.carveExport
	d := exp.Source.Derived
	scope := CarveUnallocated
	if d.FSType == "" {
		scope = CarveVolume
	}
	if o.Scope != "" && o.Scope != scope {
		return nil, carveOptionErr("scope %q contradicts unallocated export %s, which is scope %s", o.Scope, exp.ID, scope)
	}
	if o.Partition >= 0 && o.Partition != d.Partition {
		return nil, carveOptionErr("partition %d contradicts unallocated export %s, which is partition %d", o.Partition, exp.ID, d.Partition)
	}
	recorded, err := s.Case.DerivedRuns(exp, nil)
	if err != nil {
		return nil, fmt.Errorf("unallocated export %s: %w", exp.ID, err)
	}
	var (
		sc   *carveScope
		now  []evidence.Run
		note string
	)
	if scope == CarveVolume {
		if err := s.needTable(); err != nil {
			return nil, err
		}
		now, note, err = s.unallocatedRunsNote(nil, volume.Partition{}, true)
		if err != nil {
			return nil, err
		}
		sc = &carveScope{Scope: CarveVolume, Dir: "volume", Align: alignFor(o)}
	} else {
		fsys, part, err := s.unallocatedFS(d.Partition)
		if err != nil {
			return nil, err
		}
		if part.Start != d.PartitionOffset {
			return nil, fmt.Errorf("partition %d starts at %d, unallocated export %s records %d", part.Index, part.Start, exp.ID, d.PartitionOffset)
		}
		if t := fsys.Info().Type; t != d.FSType {
			return nil, fmt.Errorf("the filesystem of partition %d is %q, unallocated export %s records %q", part.Index, t, exp.ID, d.FSType)
		}
		if now, note, err = s.unallocatedRunsNote(fsys, part, false); err != nil {
			return nil, err
		}
		sc = &carveScope{Scope: CarveUnallocated, Dir: fmt.Sprintf("p%d-%s", part.Index, d.FSType), Part: part, FSType: d.FSType}
		var anote []string
		sc.Align, anote = unallocatedAlign(fsys, o)
		sc.Notes = append(sc.Notes, anote...)
	}
	if note != "" {
		sc.Notes = append(sc.Notes, note)
	}
	sc.ExportID = exp.ID

	// the sanitised recorded runs (sorted, merged, valid), minus whatever is not unallocated now
	rec := newFreeMap(recorded, false).free
	cur := newFreeMap(now, false).free
	var recBytes int64
	for _, r := range rec {
		recBytes += r.Length // merged and disjoint
	}
	kept := intersectRuns(rec, cur)
	sc.Runs = toCarveRuns(kept)
	sc.Bytes = sumRuns(sc.Runs)
	if dropped := recBytes - sc.Bytes; dropped > 0 {
		sc.Notes = append(sc.Notes, fmt.Sprintf("%d byte(s) of unallocated export %s are no longer unallocated in the image and are not carved", dropped, exp.ID))
	}
	return sc, nil
}

// intersectRuns returns the parts of the sorted, disjoint runs a that lie inside the sorted, disjoint runs b.
func intersectRuns(a, b []evidence.Run) []evidence.Run {
	var out []evidence.Run
	for i, j := 0, 0; i < len(a) && j < len(b); {
		lo := max(a[i].Offset, b[j].Offset)
		ai, bj := a[i].Offset+a[i].Length, b[j].Offset+b[j].Length
		if hi := min(ai, bj); hi > lo {
			out = append(out, evidence.Run{Offset: lo, Length: hi - lo})
		}
		if ai <= bj {
			i++
		} else {
			j++
		}
	}
	return out
}
