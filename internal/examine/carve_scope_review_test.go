package examine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

func TestCarveScopeRawHintOnlyForNoFilesystem(t *testing.T) {
	// no partition holds a filesystem: the typed sentinel, with the hint
	none := newCarveImageEnv(t, carveDisk(carveFill(9, 8192)), nil)
	_, err := none.s.buildCarveScope(CarveOptions{Partition: -1})
	if !errors.Is(err, ErrNoRecognizedFilesystem) || !strings.Contains(err.Error(), "use --raw") {
		t.Errorf("auto, none recognised: %v", err)
	}
	// an explicit partition without one: ErrUnsupported, with the hint
	_, err = none.s.buildCarveScope(CarveOptions{Partition: 1})
	if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "use --raw") {
		t.Errorf("-p 1: %v", err)
	}
	// two recognised filesystems: the ambiguity error is not --raw advice
	two := newCarveImageEnv(t, carveDisk(carveFS(carveNodes()...), carveFS(carveNodes()...)), nil)
	_, err = two.s.buildCarveScope(CarveOptions{Partition: -1})
	if err == nil || strings.Contains(err.Error(), "--raw") || errors.Is(err, ErrNoRecognizedFilesystem) || !strings.Contains(err.Error(), "--partition") {
		t.Errorf("ambiguous partitions: %v", err)
	}
}

func TestOpenArtifactScopeExportIntegrity(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	opts, _ := carveOpts(nil)
	forge := func(d *evidence.Derivation) evidence.ManifestRecord {
		return evidencetest.AddDerived(t, e.c, e.rec, []byte{1}, evidence.Source{Kind: "unallocated", Derived: d})
	}
	p1 := e.s.Table.Partitions[0]

	// a parent hash that is not the image's
	bad := forge(&evidence.Derivation{ParentID: e.rec.ID, ParentSHA256: strings.Repeat("0", 64), Partition: 1, PartitionOffset: p1.Start, FSType: "mtfs"})
	if _, err := OpenArtifactScope(e.c, bad.ID, opts); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("parent SHA mismatch: %v", err)
	}
	// an unallocated export with no derivation is never read as raw bytes
	nod := evidencetest.AddDerived(t, e.c, e.rec, []byte{1}, evidence.Source{Kind: "unallocated"})
	if s, err := OpenArtifactScope(e.c, nod.ID, opts); !errors.Is(err, evidence.ErrIntegrity) {
		if s != nil {
			_ = s.Close()
		}
		t.Errorf("export without derivation: %v", err)
	}

	scope := func(d *evidence.Derivation) error {
		s, err := OpenArtifactScope(e.c, forge(d).ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		_, err = s.buildCarveScope(CarveOptions{Partition: -1})
		return err
	}
	if err := scope(&evidence.Derivation{Partition: 1, PartitionOffset: p1.Start + 512, FSType: "mtfs"}); err == nil || !strings.Contains(err.Error(), "starts at") {
		t.Errorf("partition offset mismatch: %v", err)
	}
	if err := scope(&evidence.Derivation{Partition: 1, PartitionOffset: p1.Start, FSType: "ext4"}); err == nil || !strings.Contains(err.Error(), "ext4") {
		t.Errorf("filesystem type mismatch: %v", err)
	}
}

func TestCarveScopeVolumeExport(t *testing.T) {
	e := newCarveEnv(t, carveNodes()...)
	sum, err := e.s.ExportUnallocated(context.Background(), UnallocOptions{Partition: -1, Volume: true})
	if err != nil {
		t.Fatal(err)
	}
	var export evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if a.Source.Kind == "unallocated" {
			export = a
		}
	}
	opts, _ := carveOpts(nil)
	s, err := OpenArtifactScope(e.c, export.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc, err := s.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	var want []carve.Run
	for _, r := range e.s.Table.Unallocated {
		want = append(want, carve.Run{Offset: r.Offset, Length: r.Length})
	}
	if !slices.Equal(sc.Runs, want) || sc.Scope != CarveVolume || sc.Dir != "volume" || sc.ExportID != export.ID || len(sc.Notes) != 0 || sc.Align != 512 {
		t.Errorf("volume export scope %+v, want runs %v", sc, want)
	}
	if _, err := s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveUnallocated}); err == nil {
		t.Error("scope unallocated on a volume export accepted")
	}
	// the "dropped" note: an export forged with a run that the table now calls structure
	forged := evidencetest.AddDerived(t, e.c, e.rec, []byte{1}, evidence.Source{Kind: "unallocated", Derived: &evidence.Derivation{
		Runs: []evidence.Run{{Offset: 0, Length: 512}, {Offset: want[0].Offset, Length: want[0].Length}},
	}})
	s2, err := OpenArtifactScope(e.c, forged.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	sc2, err := s2.buildCarveScope(CarveOptions{Partition: -1})
	if err != nil || !slices.Equal(sc2.Runs, want[:1]) || len(sc2.Notes) != 1 || !strings.Contains(sc2.Notes[0], "512 byte") {
		t.Errorf("forged export scope %+v, %v", sc2, err)
	}
}

func TestAllocClassifierHostileTable(t *testing.T) {
	ci := buildClassifierImage(t)
	e := newCarveImageEnv(t, ci.img, nil)
	size := int64(len(ci.img))
	p1, p2 := e.s.Table.Partitions[0], e.s.Table.Partitions[1]
	mk := func(i int, start, length int64) volume.Partition {
		return volume.Partition{Index: i, Start: start, Length: length}
	}
	e.s.Table.Partitions = []volume.Partition{
		p2, p1, // out of order
		mk(3, p1.Start, p1.Length), // duplicate of p1
		mk(4, p1.Start+4096, 4096), // nested in p1
		mk(5, size+4096, 4096),     // outside the image
		mk(6, size-512, 8192),      // runs past the image end
		mk(7, p2.Start+512, -5),    // negative length
		mk(8, 1<<62, 1<<62),        // wraps
		mk(9, p1.Start-1024, 2048), // overlaps the start of p1
	}
	a, err := e.s.newAllocClassifier()
	if err != nil {
		t.Fatal(err)
	}
	var pos int64
	for _, r := range a.regions {
		if r.Start != pos || r.End <= r.Start || r.End > size {
			t.Fatalf("regions are not a clean cover of the image at %d: %+v", pos, a.regions)
		}
		pos = r.End
	}
	if pos != size {
		t.Fatalf("regions end at %d of %d", pos, size)
	}
	// the same table gives the same regions
	b, _ := e.s.newAllocClassifier()
	if !slices.Equal(a.regions, b.regions) {
		t.Error("regions differ between two builds of one table")
	}
	owner := func(idx int) *allocRegion {
		for i := range a.regions {
			if a.regions[i].Partition == idx && a.regions[i].Kind != regionTable {
				return &a.regions[i]
			}
		}
		return nil
	}
	for _, gone := range []int{3, 4, 5, 7, 8} {
		if r := owner(gone); r != nil {
			t.Errorf("partition %d keeps region %+v, want none", gone, *r)
		}
	}
	if r := owner(9); r == nil || r.Start != p1.Start-1024 || r.End != p1.Start+1024 {
		t.Errorf("partition 9 region %+v: the earlier start wins the overlap", r)
	}
	if r := owner(1); r == nil || r.Start != p1.Start+1024 || r.End != p1.Start+p1.Length || r.Kind != regionFS {
		t.Errorf("partition 1 region %+v", r)
	}
	if r := owner(6); r == nil || r.End != size {
		t.Errorf("partition 6 region %+v, want it clipped to the image", r)
	}
	// nothing outside a recognised filesystem's free map is free
	for _, r := range a.regions {
		if r.Kind == regionNoFS {
			if sum, _, ok := a.classify(evidence.Run{Offset: r.Start, Length: r.End - r.Start}); !ok || sum.Free != 0 || sum.Unknown == 0 {
				t.Errorf("nofs region %+v classified %+v", r, sum)
			}
		}
	}
	// a partition with no bytes of its own says so
	for _, idx := range []int{3, 5} {
		sc, err := e.s.buildCarveScope(CarveOptions{Partition: idx, Scope: CarveRaw})
		if err != nil || len(sc.Runs) != 0 || len(sc.Notes) != 1 || !strings.Contains(sc.Notes[0], "no bytes of its own remain (outside the image or overlapped)") {
			t.Errorf("-p %d: %+v, %v", idx, sc, err)
		}
	}
}

func TestAllocClassifierBadFreeRunsAreUnknown(t *testing.T) {
	ci := buildClassifierImage(t)
	for name, extra := range map[string]filesys.Run{
		"outside the partition": {Offset: -5, Length: 10},
		"empty":                 {Offset: 0, Length: 0},
	} {
		e := newCarveImageEnv(t, ci.img, func(h *carveHook) { h.extraRuns = []filesys.Run{extra} })
		a, err := e.s.newAllocClassifier()
		if err != nil {
			t.Fatal(err)
		}
		sum, _, ok := a.classify(ci.live)
		if !ok || sum.Unknown != ci.live.Length || sum.Allocated != 0 || sum.Free != 0 {
			t.Errorf("%s: live bytes %+v, %v", name, sum, ok)
		}
		if sum, _, _ := a.classify(ci.gone); sum.Free != ci.gone.Length {
			t.Errorf("%s: freed bytes %+v", name, sum)
		}
	}
}

func TestCarveScopeRawWithoutTable(t *testing.T) {
	c := carveNewCase(t)
	img := make([]byte, 4096)
	rec := carveImport(t, c, img)
	opts, _ := carveOpts(nil)
	s, err := OpenContainer(c, rec.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.tableErr = errors.New("table is damaged")
	sc, err := s.buildCarveScope(CarveOptions{Partition: -1, Scope: CarveRaw})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sc.Runs, []carve.Run{{Offset: 0, Length: 4096}}) || sc.Bytes != 4096 || len(sc.Notes) != 1 ||
		!strings.Contains(sc.Notes[0], "could not be read") || !strings.Contains(sc.Notes[0], "table is damaged") {
		t.Errorf("scope %+v", sc)
	}
	sum, reg, ok := sc.Alloc.classify(evidence.Run{Offset: 0, Length: 4096})
	if !ok || sum.Unknown != 4096 || sum.Free != 0 || sum.Allocated != 0 || reg.Kind != regionNoFS {
		t.Errorf("classify %+v %+v %v", sum, reg, ok)
	}
	if _, err := s.buildCarveScope(CarveOptions{Partition: 1, Scope: CarveRaw}); err == nil {
		t.Error("raw -p 1 without a table accepted")
	}
}
