package examine

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// classifierImage is an MBR disk: sector 0 table, a gap, p1 an MTFS (a live and a freed node), a gap, p2 with
// no filesystem and a trailing gap.
type classifierImage struct {
	img        []byte
	p1Start    int64
	live, gone evidence.Run // image-relative runs of the live and the freed node
}

func buildClassifierImage(t *testing.T) classifierImage {
	t.Helper()
	nodes := []fstest.Node{
		{Path: "/live", Data: carveFill(1, 2*carveBS)},
		{Path: "/gone", Data: carveFill(2, 2*carveBS), Deleted: true, Freed: true},
	}
	fsimg := carveFS(nodes...)
	const p1LBA = 16
	p1Sectors := uint64(len(fsimg) / 512)
	p2LBA := uint64(p1LBA) + p1Sectors + 16
	p2Sectors := uint64(64)
	total := p2LBA + p2Sectors + 16
	img := volumetest.MBR(total, []volumetest.Part{
		{StartLBA: p1LBA, Sectors: p1Sectors, MBRType: 0x83},
		{StartLBA: p2LBA, Sectors: p2Sectors, MBRType: 0x83},
	})
	copy(img[p1LBA*512:], fsimg)
	copy(img[p2LBA*512:], carveFill(7, int(p2Sectors)*512)) // not a filesystem
	ci := classifierImage{img: img, p1Start: p1LBA * 512}
	toImage := func(rs []filesys.Run) evidence.Run {
		return evidence.Run{Offset: rs[0].Offset + ci.p1Start, Length: rs[0].Length}
	}
	ci.live = toImage(carveLayout(t, "/live", nodes...))
	ci.gone = toImage(carveLayout(t, "/gone", nodes...))
	return ci
}

func TestAllocClassifierRawStates(t *testing.T) {
	ci := buildClassifierImage(t)
	e := newCarveImageEnv(t, ci.img, nil)
	if e.s.Table.Scheme != "mbr" || len(e.s.Table.Partitions) != 2 {
		t.Fatalf("table %+v", e.s.Table)
	}
	p1, p2 := e.s.Table.Partitions[0], e.s.Table.Partitions[1]
	a, err := e.s.newAllocClassifier()
	if err != nil {
		t.Fatal(err)
	}
	only := func(name string, r evidence.Run, state string) allocRegion {
		t.Helper()
		sum, reg, ok := a.classify(r)
		if !ok {
			t.Fatalf("%s: run %+v crosses regions", name, r)
		}
		got := map[string]int64{"free": sum.Free, "allocated": sum.Allocated, "unknown": sum.Unknown}
		for k, v := range got {
			want := int64(0)
			if k == state {
				want = r.Length
			}
			if v != want {
				t.Errorf("%s: %s = %d, want %d (%+v)", name, k, v, want, sum)
			}
		}
		return reg
	}
	reg := only("live bytes", ci.live, "allocated")
	if reg.Partition != p1.Index || reg.PartOffset != p1.Start || reg.FSType != "mtfs" || reg.Kind != "fs" {
		t.Errorf("live region %+v", reg)
	}
	only("freed bytes", ci.gone, "free")
	reg = only("unrecognised partition", evidence.Run{Offset: p2.Start, Length: 4096}, "unknown")
	if reg.Partition != p2.Index || reg.PartOffset != p2.Start || reg.FSType != "" || reg.Kind != "nofs" {
		t.Errorf("unrecognised region %+v", reg)
	}
	reg = only("table gap", evidence.Run{Offset: 512, Length: 1024}, "free")
	if reg.Partition != 0 || reg.Kind != "table" {
		t.Errorf("gap region %+v", reg)
	}
	only("MBR sector", evidence.Run{Offset: 0, Length: 512}, "allocated")

	// a run across two regions is not classified
	if _, _, ok := a.classify(evidence.Run{Offset: p1.Start - 512, Length: 1024}); ok {
		t.Error("a run across the gap and p1 was classified")
	}
	for _, bad := range []evidence.Run{{Offset: -1, Length: 4}, {Offset: 0, Length: 0}, {Offset: 1 << 62, Length: 1 << 62}, {Offset: int64(len(ci.img)) - 4, Length: 64}} {
		if _, _, ok := a.classify(bad); ok {
			t.Errorf("run %+v classified", bad)
		}
	}

	// a filesystem warning after Unallocated(): the non-free bytes are unknown, never allocated (C5)
	w := newCarveImageEnv(t, ci.img, func(h *carveHook) { h.warnOnUnal = []string{"free space is partial"} })
	wa, err := w.s.newAllocClassifier()
	if err != nil {
		t.Fatal(err)
	}
	sum, _, ok := wa.classify(ci.live)
	if !ok || sum.Unknown != ci.live.Length || sum.Allocated != 0 || sum.Free != 0 {
		t.Errorf("live bytes with a filesystem warning: %+v, %v", sum, ok)
	}
	if sum, _, _ := wa.classify(ci.gone); sum.Free != ci.gone.Length {
		t.Errorf("freed bytes with a filesystem warning: %+v", sum)
	}
}

func TestAllocClassifierMixedRunCountsBytes(t *testing.T) {
	ci := buildClassifierImage(t)
	e := newCarveImageEnv(t, ci.img, nil)
	a, err := e.s.newAllocClassifier()
	if err != nil {
		t.Fatal(err)
	}
	// the freed node and the live node are adjacent data blocks: a run over the end of one and the start of the other
	lo, hi := ci.live, ci.gone
	if hi.Offset < lo.Offset {
		lo, hi = hi, lo
	}
	if lo.Offset+lo.Length != hi.Offset {
		t.Fatalf("the nodes are not adjacent in this build: %+v %+v", lo, hi)
	}
	sum, _, ok := a.classify(evidence.Run{Offset: hi.Offset - 512, Length: 1024})
	free := sum.Free
	alloc := sum.Allocated
	if !ok || free != 512 || alloc != 512 || sum.Unknown != 0 {
		t.Errorf("mixed run: %+v, %v", sum, ok)
	}
}
