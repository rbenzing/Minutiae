package exfat_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

const slackCS = 4096 // the builder's default cluster size

func slackAlloc(t *testing.T, f filesys.File) []filesys.Run {
	t.Helper()
	a, ok := filesys.AllocatedRunsOf(f)
	if !ok {
		t.Fatal("the file does not implement filesys.AllocatedRunner")
	}
	return a
}

// mergeAdjacent joins runs that are adjacent on disk, in order.
func mergeAdjacent(in []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range in {
		if k := len(out); k > 0 && out[k-1].Offset+out[k-1].Length == r.Offset {
			out[k-1].Length += r.Length
			continue
		}
		out = append(out, r)
	}
	return out
}

func TestExfatAllocatedRunsWholeClusters(t *testing.T) {
	cs := slackCS
	for _, chained := range []bool{false, true} {
		name := "nofatchain"
		if chained {
			name = "fatchain"
		}
		t.Run(name, func(t *testing.T) {
			files := []exfattest.File{
				{Path: "/one.bin", Data: pattern(1, 1), FatChain: chained},
				{Path: "/csm1.bin", Data: pattern(cs-1, 2), FatChain: chained},
				{Path: "/cs.bin", Data: pattern(cs, 3), FatChain: chained},
				{Path: "/csp1.bin", Data: pattern(cs+1, 4), FatChain: chained},
				{Path: "/three.bin", Data: pattern(3*cs+5, 5), FatChain: chained},
				{Path: "/empty.bin", FatChain: chained},
			}
			if chained {
				files = append(files, exfattest.File{Path: "/frag.bin", Data: pattern(5*cs+7, 6), Fragmented: true})
			}
			img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
			fsys := openImg(t, img)
			for _, f := range files {
				fl := openPrefixFile(t, fsys, f.Path)
				got := slackAlloc(t, fl)
				if len(f.Data) == 0 {
					if got != nil {
						t.Errorf("%s: a 0-byte file reports %v, want nil", f.Path, got)
					}
					continue
				}
				var want []filesys.Run
				for _, c := range lay.Find(f.Path, false).Clusters {
					want = append(want, filesys.Run{Offset: lay.ClusterOffset(c), Length: int64(cs)})
				}
				want = mergeAdjacent(want)
				if !slices.Equal(got, want) {
					t.Errorf("%s: AllocatedRuns = %v, want %v", f.Path, got, want)
				}
				if f.Fragmented && len(want) < 2 {
					t.Errorf("%s: the fragmented file is contiguous: the test lost its point", f.Path)
				}
				if len(got) > 0 {
					got[0] = filesys.Run{Offset: -1, Length: -1}
					if again := slackAlloc(t, fl); !slices.Equal(again, want) {
						t.Errorf("%s: a second call gave %v after the first answer was changed, want %v", f.Path, again, want)
					}
				}
			}
		})
	}
}

func TestExfatValidDataLengthIsTrailingHole(t *testing.T) {
	cs := slackCS
	valid := int64(cs + cs/2)
	data := pattern(3*cs, 7)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/v.bin", Data: data, ValidLength: &valid}})
	fsys := openImg(t, img)
	fl := openPrefixFile(t, fsys, "/v.bin")
	runs := fl.Runs()
	if len(runs) < 2 || runs[len(runs)-1].Offset != -1 {
		t.Fatalf("Runs = %v, want data runs then a hole", runs)
	}
	got := slackAlloc(t, fl)
	cl := lay.Find("/v.bin", false).Clusters
	want := []filesys.Run{{Offset: lay.ClusterOffset(cl[0]), Length: int64(2 * cs)}}
	if !slices.Equal(got, want) {
		t.Fatalf("AllocatedRuns = %v, want only the 2 clusters of the first 1.5: %v", got, want)
	}
	if _, outcome, reason := filesys.TailSlack(fl.Size(), runs, got, int64(cs), fsys.Info().Size); outcome != filesys.SlackTrailingHole {
		t.Errorf("TailSlack outcome %q (%s), want %q", outcome, reason, filesys.SlackTrailingHole)
	}
}

func TestExfatAllocatedRunsMatchOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "testdata/"), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			fsys := openImg(t, img)
			bs := int64(want.BlockSize)
			checked, withSlack := 0, 0
			for _, w := range want.Files {
				if w.Type != "file" || w.Size == 0 {
					continue
				}
				var exp []filesys.Run
				for _, c := range w.Clusters {
					exp = append(exp, filesys.Run{Offset: want.DataStart + (c[0]-2)*bs, Length: (c[1] - c[0] + 1) * bs})
				}
				exp = mergeAdjacent(exp)
				fl := openPrefixFile(t, fsys, w.Path)
				alloc := slackAlloc(t, fl)
				if got := mergeAdjacent(alloc); !slices.Equal(got, exp) {
					t.Errorf("%s: AllocatedRuns = %v, oracle clusters give %v", w.Path, got, exp)
				}
				_, outcome, reason := filesys.TailSlack(fl.Size(), fl.Runs(), alloc, bs, want.Size)
				wantOutcome := filesys.SlackFound
				if w.Size%bs == 0 {
					wantOutcome = filesys.SlackExactFit
				} else {
					withSlack++
				}
				if outcome != wantOutcome {
					t.Errorf("%s: TailSlack outcome %q (%s), want %q", w.Path, outcome, reason, wantOutcome)
				}
				checked++
			}
			if checked == 0 || withSlack == 0 {
				t.Errorf("the oracle gave %d files, %d with slack: the test lost its point", checked, withSlack)
			}
		})
	}
}

func TestExfatShortChainAllocatedRunsArePrefixUnits(t *testing.T) {
	data := pattern(3*slackCS-10, 3)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/f.bin", Data: data, FatChain: true}})
	cl := lay.Find("/f.bin", false).Clusters
	le32(img, int(lay.FatEntryOffset(cl[1])), 0xFFFFFFFF) // the chain ends after cluster 2
	fsys := openImg(t, img)
	fl := openPrefixFile(t, fsys, "/f.bin")
	got := slackAlloc(t, fl)
	want := []filesys.Run{{Offset: lay.ClusterOffset(cl[0]), Length: 2 * slackCS}}
	if !slices.Equal(mergeAdjacent(got), want) {
		t.Fatalf("AllocatedRuns = %v, want the two followed clusters %v", got, want)
	}
	if _, outcome, _ := filesys.TailSlack(fl.Size(), fl.Runs(), got, slackCS, fsys.Info().Size); outcome != filesys.SlackPrefix {
		t.Errorf("TailSlack outcome %q, want %q", outcome, filesys.SlackPrefix)
	}
}

func TestExfatAllocatedRunsContract(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(path, "testdata/"), ".img.gz"), func(t *testing.T) {
			img := gunzipFixture(t, path)
			fstest.AllocatedRunsContract(t, openImg(t, img))
			viaDetect, err := detect.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			if !filesys.ReportsSlack(viaDetect) {
				t.Error("the detect-opened filesystem does not report slack")
			}
		})
	}
	valid := int64(slackCS + 100)
	img := exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/a.bin", Data: pattern(1500, 1)},
		{Path: "/d", Dir: true},
		{Path: "/d/b.bin", Data: pattern(5*slackCS+3, 2), Fragmented: true},
		{Path: "/d/c.bin", Data: pattern(3*slackCS, 3), ValidLength: &valid},
	})
	fstest.AllocatedRunsContract(t, openImg(t, img))
}

func TestExfatAllocatedRunsContractSmallClusters(t *testing.T) {
	img := exfattest.Build(exfattest.Options{OneSectorClusters: true, ClusterCount: 512}, []exfattest.File{
		{Path: "/a.bin", Data: pattern(700, 1)},
		{Path: "/b.bin", Data: pattern(1025, 2), FatChain: true},
	})
	fstest.AllocatedRunsContract(t, openImg(t, img))
}
