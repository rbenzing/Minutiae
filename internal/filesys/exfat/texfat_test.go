package exfat_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

const texCS = 4096 // the builder's default cluster size

func texData() []byte { return bytes.Repeat([]byte("TexFAT data "), 1024) } // 12288 bytes: 3 clusters

func texOpen(t *testing.T, img []byte) *exfat.FS {
	t.Helper()
	f, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func texWarnings(f *exfat.FS, sub string) []string {
	var out []string
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, sub) {
			out = append(out, w)
		}
	}
	return out
}

func texFeature(f *exfat.FS) string {
	for _, ft := range f.Info().Features {
		if strings.HasPrefix(ft, "TexFAT") {
			return ft
		}
	}
	return ""
}

// texImage builds a two-FAT image holding one FatChain file whose chain in the
// FIRST FAT is cut after its first cluster while the second FAT holds all of it.
func texImage(active int) ([]byte, *exfattest.Layout) {
	img, lay := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, ActiveFat: active},
		[]exfattest.File{{Path: "/f.bin", Data: texData(), FatChain: true}})
	e := lay.Find("/f.bin", false)
	binary.LittleEndian.PutUint32(img[lay.FatEntryOffset(e.Clusters[0]):], 0xFFFFFFFF)
	return img, lay
}

func texRunsLen(fl filesys.File) int64 {
	var n int64
	for _, r := range fl.Runs() {
		n += r.Length
	}
	return n
}

func texOpenFile(t *testing.T, f *exfat.FS, path string) filesys.File {
	t.Helper()
	e, err := f.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	return fl
}

func texFreeBytes(t *testing.T, f *exfat.FS) (int64, []filesys.Run) {
	t.Helper()
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range runs {
		total += r.Length
	}
	return total, runs
}

func TestExfatTexFATSecondActiveIsRead(t *testing.T) {
	img, lay := texImage(1)
	free := lay.FreeClusters()
	// Bitmap 2 marks one cluster used that bitmap 1 calls free.
	bit := int(free[0] - 2)
	img[lay.ClusterOffset(lay.SecondBitmapClusters[0])+int64(bit/8)] |= 1 << (bit % 8)

	f := texOpen(t, img)
	if f.ActiveFat() != 1 {
		t.Fatalf("ActiveFat = %d, want 1", f.ActiveFat())
	}
	fl := texOpenFile(t, f, "/f.bin")
	got := make([]byte, fl.Size())
	if _, err := fl.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, texData()) {
		t.Errorf("content differs: the second FAT was not followed")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Errorf("CheckRuns: %v", err)
	}
	total, runs := texFreeBytes(t, f)
	hit := lay.ClusterOffset(free[0])
	for _, r := range runs {
		if r.Offset <= hit && hit < r.Offset+r.Length {
			t.Errorf("cluster %d is used in the active bitmap but reported free (runs %+v)", free[0], runs)
		}
	}
	if want := int64(len(free)-1) * texCS; total != want {
		t.Errorf("free bytes = %d, want %d (bitmap 2)", total, want)
	}
}

func TestExfatTexFATFirstActiveReadsFirst(t *testing.T) {
	img, lay := texImage(0)
	f := texOpen(t, img)
	if f.ActiveFat() != 0 {
		t.Fatalf("ActiveFat = %d, want 0", f.ActiveFat())
	}
	// The first FAT (active) ends the chain after one cluster: a one-cluster prefix.
	if got := texRunsLen(texOpenFile(t, f, "/f.bin")); got != texCS {
		t.Errorf("Runs cover %d bytes, want %d (the first FAT's chain)", got, texCS)
	}
	if total, _ := texFreeBytes(t, f); total != int64(len(lay.FreeClusters()))*texCS {
		t.Errorf("free bytes = %d, want %d", total, int64(len(lay.FreeClusters()))*texCS)
	}
}

func TestExfatTexFATMissingActiveBitmapReportsNoFreeSpace(t *testing.T) {
	img, _ := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, ActiveFat: 1, SecondBitmapMissing: true}, nil)
	f := texOpen(t, img)
	if total, runs := texFreeBytes(t, f); total != 0 {
		t.Errorf("Unallocated = %+v, want none (never the first bitmap's)", runs)
	}
	if w := texWarnings(f, "no Allocation Bitmap entry for the active bitmap (BitmapFlags 1)"); len(w) != 1 {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

func TestExfatActiveFatWithOneFATWarns(t *testing.T) {
	files := []exfattest.File{{Path: "/f.bin", Data: texData(), FatChain: true}}
	plain, _ := exfattest.BuildLayout(exfattest.Options{}, files)
	img, _ := exfattest.BuildLayout(exfattest.Options{ActiveFat: 1}, files)
	f := texOpen(t, img)
	if f.ActiveFat() != 0 {
		t.Errorf("ActiveFat = %d, want 0 with one FAT", f.ActiveFat())
	}
	if w := texWarnings(f, "ActiveFat is 1 but NumberOfFats is 1: the only FAT and bitmap are read"); len(w) != 1 {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
	if texFeature(f) != "" {
		t.Errorf("a one-FAT volume reports the feature %q", texFeature(f))
	}
	fl := texOpenFile(t, f, "/f.bin")
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Errorf("CheckRuns: %v", err)
	}
	_, got := texFreeBytes(t, f)
	_, want := texFreeBytes(t, texOpen(t, plain))
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Unallocated = %+v, want %+v", got, want)
	}
}

func TestExfatTexFATDifferingCopiesWarn(t *testing.T) {
	files := []exfattest.File{{Path: "/f.bin", Data: texData(), FatChain: true}}
	clean, _ := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, ActiveFat: 1}, files)
	cf := texOpen(t, clean)
	texFreeBytes(t, cf)
	if w := texWarnings(cf, "TexFAT: the inactive"); len(w) != 0 {
		t.Errorf("equal copies warn: %v", w)
	}

	img, lay := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, ActiveFat: 1}, files)
	fatByte := lay.FatOffset + 40 // an entry of the inactive (first) FAT
	img[fatByte] ^= 0x01
	bmByte := int64(3)
	img[lay.ClusterOffset(lay.BitmapClusters[0])+bmByte] ^= 0x80
	f := texOpen(t, img)
	texFreeBytes(t, f)
	wantFat := fatByte - lay.FatOffset
	if w := texWarnings(f, fmt.Sprintf("the inactive FAT differs from the active one (first difference at byte %d)", wantFat)); len(w) != 1 {
		t.Errorf("FAT warning missing for byte %d: %v", wantFat, f.Info().Warnings)
	}
	if w := texWarnings(f, fmt.Sprintf("the inactive Allocation Bitmap differs from the active one (first difference at byte %d)", bmByte)); len(w) != 1 {
		t.Errorf("bitmap warning missing for byte %d: %v", bmByte, f.Info().Warnings)
	}
}

func TestExfatTexFATFeatureReported(t *testing.T) {
	for active, want := range map[int]string{0: "TexFAT: 2 FATs, active FAT 1", 1: "TexFAT: 2 FATs, active FAT 2"} {
		img, _ := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, ActiveFat: active}, nil)
		if got := texFeature(texOpen(t, img)); got != want {
			t.Errorf("ActiveFat %d: feature %q, want %q", active, got, want)
		}
	}
}

func TestExfatTexFATCompareIsBounded(t *testing.T) {
	const cc = 16384
	img, lay := exfattest.BuildLayout(exfattest.Options{NumberOfFats: 2, OneSectorClusters: true, ClusterCount: cc}, nil)
	// The inactive FAT (the first) differs only in the entry of the last cluster.
	lastEntry := lay.FatEntryOffset(cc + 1)
	img[lastEntry] ^= 0xFF
	// The inactive bitmap (the first) differs only in its last byte.
	bmLast := int64(cc/8 - 1)
	img[lay.ClusterOffset(lay.BitmapClusters[0])+bmLast] ^= 0x01
	wantFat := fmt.Sprintf("(first difference at byte %d)", lastEntry-lay.FatOffset)
	wantBm := fmt.Sprintf("(first difference at byte %d)", bmLast)

	check := func(t *testing.T, f *exfat.FS) {
		t.Helper()
		texFreeBytes(t, f)
		if w := texWarnings(f, "the inactive FAT differs"); len(w) != 1 || !strings.Contains(w[0], wantFat) {
			t.Errorf("FAT warning = %v, want %s", w, wantFat)
		}
		if w := texWarnings(f, "the inactive Allocation Bitmap differs"); len(w) != 1 || !strings.Contains(w[0], wantBm) {
			t.Errorf("bitmap warning = %v, want %s", w, wantBm)
		}
	}
	t.Run("default-chunk", func(t *testing.T) {
		f := texOpen(t, img)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		check(t, f)
		runtime.ReadMemStats(&after)
		if grown := after.TotalAlloc - before.TotalAlloc; grown >= 2_228_224 {
			t.Errorf("Unallocated allocated %d bytes, want < 2228224", grown)
		}
	})
	t.Run("chunk-4096", func(t *testing.T) {
		f := texOpen(t, img)
		f.SetBitmapChunk(4096)
		check(t, f)
	})
}
