package fat_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// slackBuilderOptions are the three FAT types with a cluster of 1024 bytes.
var slackBuilderOptions = []fattest.Options{
	{Type: 12, SecPerClus: 2},
	{Type: 16, SecPerClus: 2},
	{Type: 32, SecPerClus: 2},
}

func patternBytes(n int) []byte { return bytes.Repeat([]byte{0xA5}, n) }

// mergeRuns joins runs that are consecutive and adjacent on disk, in order.
func mergeRuns(in []filesys.Run) []filesys.Run {
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

func openBuilt(t *testing.T, img []byte) *fat.FS {
	t.Helper()
	fsys, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func openPath(t *testing.T, fsys *fat.FS, path string) filesys.File {
	t.Helper()
	live, _ := walkAll(t, fsys)
	e, ok := live[path]
	if !ok {
		t.Fatalf("no live entry %s", path)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return f
}

func allocOf(t *testing.T, f filesys.File) []filesys.Run {
	t.Helper()
	a, ok := filesys.AllocatedRunsOf(f)
	if !ok {
		t.Fatal("the file does not implement filesys.AllocatedRunner")
	}
	return a
}

func TestFATAllocatedRunsWholeClusters(t *testing.T) {
	for _, o := range slackBuilderOptions {
		t.Run(fmt.Sprintf("fat%d", o.Type), func(t *testing.T) {
			const cs = 1024
			files := []fattest.File{
				{Path: "/one.bin", Data: patternBytes(1)},
				{Path: "/csm1.bin", Data: patternBytes(cs - 1)},
				{Path: "/cs.bin", Data: patternBytes(cs)},
				{Path: "/csp1.bin", Data: patternBytes(cs + 1)},
				{Path: "/three.bin", Data: patternBytes(3*cs + 5)},
				{Path: "/frag.bin", Data: patternBytes(5*cs + 7), Fragmented: true},
				{Path: "/other.bin", Data: patternBytes(6 * cs)},
				{Path: "/empty.bin"},
			}
			img, built := fattest.BuildLayout(o, files)
			fsys := openBuilt(t, img)
			g := built.Geometry
			for _, f := range files {
				fl := openPath(t, fsys, f.Path)
				got := allocOf(t, fl)
				if len(f.Data) == 0 {
					if got != nil {
						t.Errorf("%s: a 0-byte file reports %v, want nil", f.Path, got)
					}
					continue
				}
				var want []filesys.Run
				for _, c := range built.Find(f.Path, false).Clusters {
					want = append(want, filesys.Run{Offset: g.ClusterOffset(c), Length: cs})
				}
				want = mergeRuns(want)
				if !slices.Equal(got, want) {
					t.Errorf("%s: AllocatedRuns = %v, want %v", f.Path, got, want)
				}
				if f.Fragmented && len(want) < 2 {
					t.Errorf("%s: the fragmented file is contiguous: the test lost its point", f.Path)
				}
				if len(got) > 0 {
					got[0] = filesys.Run{Offset: -1, Length: -1}
					if again := allocOf(t, fl); !slices.Equal(again, want) {
						t.Errorf("%s: a second call gave %v after the first answer was changed, want %v", f.Path, again, want)
					}
				}
			}
		})
	}
}

func TestFATAllocatedRunsMatchOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "testdata/"), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			fsys := openBuilt(t, img)
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
				exp = mergeRuns(exp)
				f := openPath(t, fsys, w.Path)
				alloc := allocOf(t, f)
				if got := mergeRuns(alloc); !slices.Equal(got, exp) {
					t.Errorf("%s: AllocatedRuns = %v, oracle clusters give %v", w.Path, got, exp)
				}
				_, outcome, reason := filesys.TailSlack(f.Size(), f.Runs(), alloc, bs, want.Size)
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

func TestFATShortChainAllocatedRunsArePrefixUnits(t *testing.T) {
	const cs = 1024
	o := fattest.Options{Type: 16, SecPerClus: 2}
	img, built := fattest.BuildLayout(o, []fattest.File{{Path: "/f.bin", Data: patternBytes(3*cs - 10)}})
	loc := built.Find("/f.bin", false)
	if len(loc.Clusters) != 3 {
		t.Fatalf("the file has %d clusters, want 3", len(loc.Clusters))
	}
	fattest.SetFATEntry(img, built.Geometry, loc.Clusters[0], 0xFFFF) // the chain ends after cluster 1
	fsys := openBuilt(t, img)
	f := openPath(t, fsys, "/f.bin")
	got := allocOf(t, f)
	want := []filesys.Run{{Offset: built.Geometry.ClusterOffset(loc.Clusters[0]), Length: cs}}
	if !slices.Equal(got, want) {
		t.Fatalf("AllocatedRuns = %v, want the one whole cluster %v", got, want)
	}
	if _, outcome, _ := filesys.TailSlack(f.Size(), f.Runs(), got, cs, fsys.Info().Size); outcome != filesys.SlackPrefix {
		t.Errorf("TailSlack outcome %q, want %q", outcome, filesys.SlackPrefix)
	}
}

func TestFATAllocatedRunsContract(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(path, "testdata/"), ".img.gz"), func(t *testing.T) {
			img := gunzipFixture(t, path)
			fstest.AllocatedRunsContract(t, openBuilt(t, img))
			viaDetect, err := detect.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			if !filesys.ReportsSlack(viaDetect) {
				t.Error("the detect-opened filesystem does not report slack")
			}
		})
	}
	for _, o := range slackBuilderOptions {
		img := fattest.Build(o, []fattest.File{
			{Path: "/a.bin", Data: patternBytes(1500)},
			{Path: "/d", Dir: true},
			{Path: "/d/b.bin", Data: patternBytes(5000), Fragmented: true},
			{Path: "/d/c.bin", Data: patternBytes(3000)},
		})
		fstest.AllocatedRunsContract(t, openBuilt(t, img))
	}
}
