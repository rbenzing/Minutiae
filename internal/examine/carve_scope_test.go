package examine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func carveRunsOf(rs []evidence.Run) []carve.Run {
	out := make([]carve.Run, len(rs))
	for i, r := range rs {
		out[i] = carve.Run{Offset: r.Offset, Length: r.Length}
	}
	return out
}

func carveNodes() []fstest.Node {
	return []fstest.Node{
		{Path: "/a", Data: carveFill(1, 8192)},
		{Path: "/gone", Data: carveFill(2, 8192), Deleted: true, Freed: true},
	}
}

func TestCarveScopeUnallocatedRunsAndAlign(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	fsys, part, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := e.s.unallocatedRunsNote(fsys, part, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("test image has no free space")
	}
	if !slices.Equal(sc.Runs, carveRunsOf(want)) {
		t.Errorf("Runs = %v, want %v", sc.Runs, want)
	}
	if sc.Scope != CarveUnallocated || sc.Dir != "p1-mtfs" || sc.FSType != "mtfs" || sc.Part.Index != 1 {
		t.Errorf("Scope %q Dir %q FSType %q Part %d", sc.Scope, sc.Dir, sc.FSType, sc.Part.Index)
	}
	if sc.Align != int64(fsys.Info().BlockSize) || sc.Align != carveBS {
		t.Errorf("Align = %d, want the block size %d", sc.Align, carveBS)
	}
	var total int64
	for _, r := range want {
		total += r.Length
	}
	if sc.Bytes != total || len(sc.Notes) != 0 {
		t.Errorf("Bytes %d (want %d), Notes %v", sc.Bytes, total, sc.Notes)
	}

	// a reader whose block size is no multiple of 512 gives the sector grid and one note
	odd := newCarveImageEnv(t, carveDisk(carveFS(carveNodes()...)), func(h *carveHook) { h.blockSize = 1000 })
	sc, err = odd.s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Align != 512 || len(sc.Notes) != 1 || !strings.Contains(sc.Notes[0], "1000") {
		t.Errorf("block size 1000: Align %d, Notes %v", sc.Align, sc.Notes)
	}
}

func TestCarveScopeUnalignedOption(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	for _, scope := range []string{"", CarveVolume, CarveRaw} {
		sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1, Scope: scope, Unaligned: true})
		if err != nil {
			t.Fatalf("scope %q: %v", scope, err)
		}
		if sc.Align != 1 {
			t.Errorf("scope %q: Align = %d with Unaligned, want 1", scope, sc.Align)
		}
	}
}

func TestCarveScopeOptionsAreCheckedBeforeAnyRead(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	for name, o := range map[string]CarveOptions{
		"negative max size":                  {Partition: -1, MaxFileSize: -1},
		"negative max files":                 {Partition: -1, MaxFiles: -1},
		"negative max bytes":                 {Partition: -1, MaxBytes: -1},
		"confidence 101":                     {Partition: -1, MinConfidence: 101},
		"confidence -1":                      {Partition: -1, MinConfidence: -1},
		"unknown scope":                      {Partition: -1, Scope: "everything"},
		"artifact scope on an image session": {Partition: -1, Scope: CarveArtifact},
	} {
		if sc, err := e.s.buildCarveScope(o); err == nil || sc != nil {
			t.Errorf("%s: got %v, %v", name, sc, err)
		}
	}
	if _, err := e.s.buildCarveScope(CarveOptions{Partition: -1, MaxFileSize: -1}); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("negative max size: %v, want ErrInvalidLimit", err)
	}
}

func TestCarveScopeUnrecognisedFilesystemHintsRaw(t *testing.T) {
	e := newCarveImageEnv(t, carveDisk(carveFill(9, 8192)), nil)
	_, err := e.s.buildCarveScope(CarveOptions{Partition: 1})
	if err == nil || !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "use --raw") {
		t.Errorf("partition without a filesystem: %v", err)
	}
	if _, err := e.s.buildCarveScope(CarveOptions{Partition: -1}); err == nil || !strings.Contains(err.Error(), "use --raw") {
		t.Errorf("auto partition without a filesystem: %v", err)
	}
	// raw works there: the partition is one run
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: 1, Scope: CarveRaw})
	if err != nil || len(sc.Runs) != 1 {
		t.Errorf("raw: %v, %v", sc, err)
	}
}

func TestCarveScopeVolume(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveVolume})
	if err != nil {
		t.Fatal(err)
	}
	var want []carve.Run
	for _, r := range e.s.Table.Unallocated {
		want = append(want, carve.Run{Offset: r.Offset, Length: r.Length})
	}
	if len(want) == 0 || !slices.Equal(sc.Runs, want) {
		t.Errorf("Runs = %v, want %v", sc.Runs, want)
	}
	if sc.Scope != CarveVolume || sc.Dir != "volume" || sc.Align != 512 || sc.FSType != "" || sc.Part.Index != 0 {
		t.Errorf("Scope %q Dir %q Align %d FSType %q Part %d", sc.Scope, sc.Dir, sc.Align, sc.FSType, sc.Part.Index)
	}
}

func TestCarveScopeVolumeNeedsTable(t *testing.T) {
	c := carveNewCase(t)
	rec := carveImport(t, c, make([]byte, 1024))
	opts, _ := carveOpts(nil)
	s, err := OpenContainer(c, rec.ID, opts) // the table has not been read
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveVolume})
	if want := s.needTable(); err == nil || err.Error() != want.Error() {
		t.Errorf("volume without a table: %v, want %v", err, want)
	}
}

func TestCarveScopeRawWholeImageSplitsAtPartitions(t *testing.T) {
	img := carveDisk(carveFS(carveNodes()...), carveFS(carveNodes()...))
	e := newCarveImageEnv(t, img, nil)
	sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveRaw})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Scope != CarveRaw || sc.Dir != "raw" || sc.Align != 512 || sc.Alloc == nil {
		t.Fatalf("Scope %q Dir %q Align %d Alloc %v", sc.Scope, sc.Dir, sc.Align, sc.Alloc)
	}
	// sorted, disjoint, and together the whole image
	var pos int64
	for i, r := range sc.Runs {
		if r.Offset != pos || r.Length <= 0 {
			t.Fatalf("run %d = %+v, want it to start at %d", i, r, pos)
		}
		pos += r.Length
	}
	if pos != int64(len(img)) || sc.Bytes != pos {
		t.Fatalf("runs cover %d bytes (Bytes %d) of %d", pos, sc.Bytes, len(img))
	}
	has := func(off, n int64) bool { return slices.Contains(sc.Runs, carve.Run{Offset: off, Length: n}) }
	for _, p := range e.s.Table.Partitions {
		if !has(p.Start, p.Length) {
			t.Errorf("partition %d (%d+%d) is not one run", p.Index, p.Start, p.Length)
		}
	}
	for _, u := range e.s.Table.Unallocated {
		if !has(u.Offset, u.Length) {
			t.Errorf("table gap %+v is not one run (3E-R23)", u)
		}
	}
	// every run has exactly one state, and a gap is free while structure is allocated
	gap := func(r carve.Run) bool {
		for _, u := range e.s.Table.Unallocated {
			if u.Offset == r.Offset && u.Length == r.Length {
				return true
			}
		}
		return false
	}
	for _, r := range sc.Runs {
		sum, reg, ok := sc.Alloc.classify(evidence.Run{Offset: r.Offset, Length: r.Length})
		if !ok {
			t.Errorf("run %+v crosses regions", r)
			continue
		}
		states := 0
		if reg.Partition != 0 {
			continue // a filesystem partition mixes free and allocated bytes
		}
		for _, n := range []int64{sum.Free, sum.Allocated, sum.Unknown} {
			if n != 0 {
				states++
				if n != r.Length {
					t.Errorf("run %+v: state covers %d bytes (%+v)", r, n, sum)
				}
			}
		}
		if states != 1 {
			t.Errorf("run %+v: %d states (%+v)", r, states, sum)
		}
		if reg.Partition == 0 && gap(r) && sum.Free != r.Length {
			t.Errorf("table gap %+v classified %+v", r, sum)
		}
		if reg.Partition == 0 && !gap(r) && sum.Allocated != r.Length {
			t.Errorf("table structure %+v classified %+v", r, sum)
		}
	}

	// -p 2 raw is that partition alone
	p2 := e.s.Table.Partitions[1]
	one, err := e.s.buildCarveScope(CarveOptions{Partition: p2.Index, Scope: CarveRaw})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(one.Runs, []carve.Run{{Offset: p2.Start, Length: p2.Length}}) {
		t.Errorf("-p 2 raw runs = %v, want %d+%d", one.Runs, p2.Start, p2.Length)
	}
	if _, err := e.s.buildCarveScope(CarveOptions{Partition: 7, Scope: CarveRaw}); err == nil {
		t.Error("raw of a missing partition accepted")
	}

	// table scheme none: the whole image is one run
	bare := carveFS(carveNodes()...)
	n := newCarveImageEnv(t, bare, nil)
	if n.s.Table.Scheme != "none" {
		t.Fatalf("scheme %q, want none", n.s.Table.Scheme)
	}
	whole, err := n.s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveRaw})
	if err != nil || !slices.Equal(whole.Runs, []carve.Run{{Offset: 0, Length: int64(len(bare))}}) {
		t.Errorf("scheme none: %+v, %v", whole, err)
	}
}

// toyLen is the length (and minimum length) of the toy object: an examiner signature-file type with a header
// and a footer, no format builder (3E-R31).
const toyLen = 1024

func toySigFile() []byte {
	return []byte(fmt.Sprintf(`{"format":"%s","types":[{"name":"toy","ext":"toy","mime":"application/x-toy",`+
		`"headers":[{"offset":0,"hex":"%s"}],"footer":"%s","min_len":%d,"max_len":4096,"validate":"","align":"block"}]}`,
		carve.SigFileFormat, hex.EncodeToString([]byte("TOYHDR!\x00")), hex.EncodeToString([]byte("TOYFTR!\x00")), toyLen))
}

func toyObject() []byte {
	obj := carveFill(0x40, toyLen)
	copy(obj, "TOYHDR!\x00")
	copy(obj[toyLen-8:], "TOYFTR!\x00")
	return obj
}

func carveToyHits(t *testing.T, s *Session, sc *carveScope) []carve.Hit {
	t.Helper()
	set, err := carve.ParseSigFile(toySigFile(), carve.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	set, filter, err := set.Select([]string{"toy"})
	if err != nil {
		t.Fatal(err)
	}
	var hits []carve.Hit
	_, err = carve.Scan(context.Background(), s.Image, sc.Runs, set, carve.Options{Align: sc.Align, TypeFilter: filter},
		func(h carve.Hit) error { hits = append(hits, h); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestCarveRawHitNeverStraddlesTableGapJoint(t *testing.T) {
	// an MBR disk: the table sector 0, then the gap [512, 8192), then p1
	base := newCarveImageEnv(t, buildClassifierImage(t).img, nil)
	un := base.s.Table.Unallocated
	gap := un[0]
	if gap.Offset != 512 || gap.Length < 3*512 {
		t.Fatalf("unexpected test gap %+v", gap)
	}
	scan := func(t *testing.T, img []byte) (*carveEnv, *carveScope, []carve.Hit) {
		t.Helper()
		e := newCarveImageEnv(t, img, nil)
		if got := e.s.Table.Unallocated; len(got) != len(un) || got[0] != gap {
			t.Fatalf("the image changed the table gaps: %v, want %v (warnings %v)", got, un, e.s.Table.Warnings)
		}
		sc, err := e.s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveRaw, Unaligned: true})
		if err != nil {
			t.Fatal(err)
		}
		return e, sc, carveToyHits(t, e.s, sc)
	}

	// the header sits in the boot-code bytes of the table sector and the object runs on into the gap
	straddle := slices.Clone(base.img)
	carvetest.Place(straddle, 400, toyObject())
	copy(straddle[446:512], base.img[446:512]) // the partition table and signature stay, as part of the object body
	if _, _, hits := scan(t, straddle); len(hits) != 0 {
		t.Errorf("an object across the structure/gap joint gave hits %+v", hits)
	}

	inside := slices.Clone(base.img)
	carvetest.Place(inside, int(gap.Offset)+512, toyObject())
	_, sc, hits := scan(t, inside)
	if len(hits) != 1 || hits[0].Offset != gap.Offset+512 || hits[0].Length != toyLen {
		t.Fatalf("an object wholly inside the gap: hits %+v", hits)
	}
	sum, _, ok := sc.Alloc.classify(evidence.Run{Offset: hits[0].Offset, Length: hits[0].Length})
	if !ok || sum.Free != toyLen || sum.Allocated != 0 || sum.Unknown != 0 {
		t.Errorf("classify of the gap hit = %+v, %v; want free", sum, ok)
	}
}

func TestCarveScopeFromUnallocExportIntersects(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	sum, err := e.s.ExportUnallocated(context.Background(), UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	var export evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if a.Source.Kind == "unallocated" {
			export = a
		}
	}
	if export.ID == "" {
		t.Fatal("no unallocated export")
	}
	exportRuns, err := e.c.DerivedRuns(export, nil)
	if err != nil || len(exportRuns) == 0 {
		t.Fatalf("export runs %v, %v", exportRuns, err)
	}
	part := e.s.Table.Partitions[0]
	drop := evidence.Run{Offset: exportRuns[0].Offset, Length: carveBS} // an image-relative block that is allocated now
	opts, _ := carveOpts(func(h *carveHook) {
		h.dropFree = &filesys.Run{Offset: drop.Offset - part.Start, Length: drop.Length}
	})
	s, err := OpenArtifactScope(e.c, export.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Parent.ID != e.rec.ID {
		t.Fatalf("the export session is over %s, want the parent image %s", s.Parent.ID, e.rec.ID)
	}
	sc, err := s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	var want []carve.Run
	for _, r := range exportRuns {
		lo, hi := r.Offset, r.Offset+r.Length
		if hi <= drop.Offset || lo >= drop.Offset+drop.Length {
			want = append(want, carve.Run{Offset: lo, Length: r.Length})
			continue
		}
		if lo < drop.Offset {
			want = append(want, carve.Run{Offset: lo, Length: drop.Offset - lo})
		}
		if hi > drop.Offset+drop.Length {
			want = append(want, carve.Run{Offset: drop.Offset + drop.Length, Length: hi - drop.Offset - drop.Length})
		}
	}
	if !slices.Equal(sc.Runs, want) {
		t.Errorf("Runs = %v, want %v", sc.Runs, want)
	}
	if sc.ExportID != export.ID || sc.Scope != CarveUnallocated || sc.Dir != "p1-mtfs" || sc.ArtifactID != "" {
		t.Errorf("ExportID %q Scope %q Dir %q ArtifactID %q", sc.ExportID, sc.Scope, sc.Dir, sc.ArtifactID)
	}
	if len(sc.Notes) != 1 || !strings.Contains(sc.Notes[0], fmt.Sprint(carveBS)) {
		t.Errorf("Notes = %v, want one naming the %d dropped bytes", sc.Notes, carveBS)
	}

	// an explicit scope or partition that contradicts the export is refused
	if _, err := s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveRaw}); err == nil {
		t.Error("raw scope on an export session accepted")
	}
	if _, err := s.buildCarveScope(CarveOptions{Partition: 2}); err == nil {
		t.Error("another partition on an export session accepted")
	}
}

func TestOpenArtifactScopeReadsRaw(t *testing.T) {
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, make([]byte, 4096))
	data := append([]byte("EVF\x09\x0d\x0a\xff\x00"), carveFill(3, 1000)...)
	art := evidencetest.AddDerived(t, c, parent, data, evidence.Source{Kind: "extract", Derived: &evidence.Derivation{}})
	opts, _ := carveOpts(nil)
	s, err := OpenArtifactScope(c, art.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Image.Format() != "artifact" || s.Image.Size() != int64(len(data)) || s.Image.SectorSize() != 512 {
		t.Errorf("Format %q Size %d SectorSize %d", s.Image.Format(), s.Image.Size(), s.Image.SectorSize())
	}
	if len(s.Segments) != 1 || s.Segments[0].ID != art.ID || s.Parent.ID != art.ID {
		t.Errorf("Parent %s Segments %v", s.Parent.ID, s.Segments)
	}
	got := make([]byte, 8)
	if _, err := s.Image.ReadAt(got, 0); err != nil || string(got) != "EVF\x09\x0d\x0a\xff\x00" {
		t.Errorf("first bytes %q, %v", got, err)
	}
	sc, err := s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveArtifact})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sc.Runs, []carve.Run{{Offset: 0, Length: int64(len(data))}}) || sc.Scope != CarveArtifact ||
		sc.Dir != "raw" || sc.Align != 512 || sc.ArtifactID != art.ID || sc.ExportID != "" {
		t.Errorf("scope %+v", sc)
	}
	if auto, err := s.buildCarveScope(CarveOptions{Partition: -1}); err != nil || auto.Scope != CarveArtifact {
		t.Errorf("default scope of an artifact session: %+v, %v", auto, err)
	}
	for _, scope := range []string{CarveUnallocated, CarveVolume, CarveRaw} {
		if _, err := s.buildCarveScope(CarveOptions{Partition: -1, Scope: scope}); err == nil {
			t.Errorf("scope %q on an artifact session accepted", scope)
		}
	}

	_, wantErr := c.FindArtifact("no-such-artifact")
	if wantErr == nil {
		t.Fatal("FindArtifact found a missing id")
	}
	if _, err := OpenArtifactScope(c, "no-such-artifact", opts); err == nil || err.Error() != wantErr.Error() || !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Errorf("missing id: %v, want %v", err, wantErr)
	}
}
