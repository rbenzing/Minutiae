package exfat_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// stale builds a deleted, fragmented, FAT-chained file of n clusters and returns the reader, the image,
// the layout and the file entry (the builder keeps its chain in the FAT).
func stale(t *testing.T, n int) (*exfat.FS, []byte, *exfattest.Layout, exfattest.EntryLoc) {
	t.Helper()
	data := bytes.Repeat([]byte("stale chain "), n*4096/12+1)[:n*4096-3]
	files := []exfattest.File{{Path: "/F.BIN", Data: data, FatChain: true, Fragmented: true, Deleted: true}}
	img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 512}, files)
	return reopen(t, img), img, l, l.Find("/F.BIN", true)
}

func recoverOne(t *testing.T, fsys *exfat.FS, loc exfattest.EntryLoc) (filesys.Candidate, error) {
	t.Helper()
	cs, err := fsys.Recoverable(recID(loc))
	if err != nil || len(cs) != 1 {
		return filesys.Candidate{}, fmt.Errorf("candidates %d, err %v", len(cs), err)
	}
	return cs[0], nil
}

func reopen(t *testing.T, img []byte) *exfat.FS {
	t.Helper()
	fsys, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// mergeRuns merges consecutive cluster numbers into runs; trim is cut from the end of the last run.
func mergeRuns(l *exfattest.Layout, cl []uint32, trim int64) []filesys.Run {
	var runs []filesys.Run
	cs := int64(l.ClusterSize)
	for i := 0; i < len(cl); {
		j := i + 1
		for j < len(cl) && cl[j] == cl[j-1]+1 {
			j++
		}
		runs = append(runs, filesys.Run{Offset: l.ClusterOffset(cl[i]), Length: int64(j-i) * cs})
		i = j
	}
	if len(runs) > 0 {
		runs[len(runs)-1].Length -= trim
	}
	return runs
}

func total(runs []filesys.Run) (n int64) {
	for _, r := range runs {
		n += r.Length
	}
	return n
}

func stopOf(as []string) string {
	for _, a := range as {
		if v, ok := strings.CutPrefix(a, "stale-chain-stop="); ok {
			return v
		}
	}
	return ""
}

func TestExfatStaleChainComplete(t *testing.T) {
	fsys, img, l, loc := stale(t, 6)
	c, err := recoverOne(t, fsys, loc)
	if err != nil {
		t.Fatal(err)
	}
	if c.Method != "exfat-fatchain" || c.Size != 6*4096-3 {
		t.Fatalf("method %q size %d", c.Method, c.Size)
	}
	want := mergeRuns(l, loc.Clusters, 3)
	if !slices.Equal(c.Runs, want) {
		t.Fatalf("runs %v, want %v", c.Runs, want)
	}
	var got []byte
	for _, r := range c.Runs {
		got = append(got, img[r.Offset:r.Offset+r.Length]...)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("stale chain "), 6*4096/12)[:6*4096-3]) {
		t.Error("bytes at the runs differ from the data")
	}
	if stopOf(c.Assumptions) != "" || !slices.Contains(c.Assumptions, "fat-chain-unverified-against-reuse") || !slices.Contains(c.Basis, "fat-chain=stale") {
		t.Errorf("basis %v assumptions %v", c.Basis, c.Assumptions)
	}
	if _, err := filesys.CheckCandidate(c, int64(len(img))); err != nil {
		t.Error(err)
	}
}

func TestExfatStaleChainFirstEntryFreeIsContiguous(t *testing.T) {
	_, img, l, loc := stale(t, 6)
	exfattest.SetFATEntry(img, l, loc.Clusters[0], 0)
	c, err := recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if c.Method != "exfat-contiguous" || len(c.Runs) != 1 || c.Runs[0].Offset != l.ClusterOffset(loc.Clusters[0]) || !slices.Contains(c.Assumptions, "contiguous-allocation") {
		t.Errorf("candidate %+v", c)
	}
}

func TestExfatStaleChainHostile(t *testing.T) {
	const eoc, bad = 0xFFFFFFFF, 0xFFFFFFF7
	for _, tc := range []struct {
		name string
		set  func(cl []uint32) (c, v uint32)
		keep int    // clusters of the prefix
		stop string // stop kind
		at   int    // index of the cluster the stop concerns
	}{
		{"loop", func(cl []uint32) (uint32, uint32) { return cl[2], cl[1] }, 3, "loop", 2},
		{"range", func(cl []uint32) (uint32, uint32) { return cl[2], 0x7FFFFFF0 }, 3, "range", 2},
		{"bad", func(cl []uint32) (uint32, uint32) { return cl[2], bad }, 2, "bad", 2},
		{"early end", func(cl []uint32) (uint32, uint32) { return cl[1], eoc }, 2, "end", 1},
		{"free entry", func(cl []uint32) (uint32, uint32) { return cl[2], 0 }, 2, "free-entry", 2},
		{"first entry end", func(cl []uint32) (uint32, uint32) { return cl[0], eoc }, 1, "end", 0},
		{"first entry bad", func(cl []uint32) (uint32, uint32) { return cl[0], bad }, 0, "bad", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, img, l, loc := stale(t, 6)
			cl := loc.Clusters
			c0, v := tc.set(cl)
			exfattest.SetFATEntry(img, l, c0, v)
			c, err := recoverOne(t, reopen(t, img), loc)
			if err != nil {
				t.Fatal(err)
			}
			if c.Method != "exfat-fatchain" || c.Size != 6*4096-3 {
				t.Fatalf("method %q size %d", c.Method, c.Size)
			}
			if want := mergeRuns(l, cl[:tc.keep], 0); !slices.Equal(c.Runs, want) {
				t.Errorf("runs %v, want %v", c.Runs, want)
			}
			if total(c.Runs) >= c.Size {
				t.Errorf("runs cover %d of %d bytes: not a prefix", total(c.Runs), c.Size)
			}
			if got, want := stopOf(c.Assumptions), fmt.Sprintf("%s@%d", tc.stop, cl[tc.at]); got != want {
				t.Errorf("stop %q, want %q (%v)", got, want, c.Assumptions)
			}
			if !slices.Contains(c.Assumptions, fmt.Sprintf("map-covers-clusters=%d-of-6", tc.keep)) {
				t.Errorf("assumptions %v lack map-covers-clusters=%d-of-6", c.Assumptions, tc.keep)
			}
			if _, err := filesys.CheckCandidate(c, int64(len(img))); err != nil {
				t.Error(err)
			}
			// no run touches a cluster after the stop
			for _, r := range c.Runs {
				for _, after := range cl[tc.keep:] {
					if o := l.ClusterOffset(after); r.Offset <= o && o < r.Offset+r.Length {
						t.Errorf("run %v touches cluster %d after the stop", r, after)
					}
				}
			}
		})
	}
}

func TestExfatStaleChainNeverFallsBackToContiguous(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 3*4096-1)
	img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 512}, []exfattest.File{{Path: "/F.BIN", Data: data, FatChain: true, Deleted: true}})
	loc := l.Find("/F.BIN", true)
	cl := loc.Clusters
	if cl[1] != cl[0]+1 || cl[2] != cl[1]+1 {
		t.Fatalf("clusters %v are not contiguous", cl)
	}
	exfattest.SetFATEntry(img, l, cl[0], 0xFFFFFFFF)
	c, err := recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if c.Method != "exfat-fatchain" || !slices.Equal(c.Runs, []filesys.Run{{Offset: l.ClusterOffset(cl[0]), Length: 4096}}) {
		t.Errorf("method %q runs %v: the clusters after the stop must not be claimed", c.Method, c.Runs)
	}
}

func TestExfatStaleChainNeverWalksMoreThanNeed(t *testing.T) {
	_, img, l, loc := stale(t, 40)
	cl := loc.Clusters
	exfattest.PatchStream(img, loc, exfattest.StreamPatch{DataLen: ptr(uint64(2*4096 - 3)), Valid: ptr(uint64(2*4096 - 3))})
	exfattest.SetFATEntry(img, l, cl[1], 0xFFFFFFF7)
	exfattest.SetFATEntry(img, l, cl[2], cl[0])
	exfattest.SetFATEntry(img, l, cl[3], 0)
	c, err := recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := mergeRuns(l, cl[:2], 3); c.Method != "exfat-fatchain" || c.Size != 2*4096-3 || !slices.Equal(c.Runs, want) || stopOf(c.Assumptions) != "" {
		t.Errorf("need 2: method %q size %d runs %v (want %v) assumptions %v", c.Method, c.Size, c.Runs, want, c.Assumptions)
	}
	exfattest.PatchStream(img, loc, exfattest.StreamPatch{DataLen: ptr(uint64(4096 - 3)), Valid: ptr(uint64(4096 - 3))})
	exfattest.SetFATEntry(img, l, cl[0], 0xFFFFFFF7)
	c, err = recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := mergeRuns(l, cl[:1], 3); c.Method != "exfat-fatchain" || !slices.Equal(c.Runs, want) || stopOf(c.Assumptions) != "" {
		t.Errorf("need 1: method %q runs %v (want %v) assumptions %v", c.Method, c.Runs, want, c.Assumptions)
	}
}

func TestExfatStaleChainRangeBoundary(t *testing.T) {
	_, img, l, loc := stale(t, 3)
	cl := loc.Clusters
	last := l.ClusterCount + 1
	exfattest.SetFATEntry(img, l, cl[1], last)
	c, err := recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := mergeRuns(l, []uint32{cl[0], cl[1], last}, 3); !slices.Equal(c.Runs, want) || stopOf(c.Assumptions) != "" {
		t.Errorf("last heap cluster: runs %v (want %v) assumptions %v", c.Runs, want, c.Assumptions)
	}
	exfattest.SetFATEntry(img, l, cl[1], last+1)
	c, err = recoverOne(t, reopen(t, img), loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := mergeRuns(l, cl[:2], 0); !slices.Equal(c.Runs, want) || stopOf(c.Assumptions) != fmt.Sprintf("range@%d", cl[1]) {
		t.Errorf("past the heap: runs %v (want %v) assumptions %v", c.Runs, want, c.Assumptions)
	}
}
