package volume_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/volume"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

func FuzzRead(f *testing.F) {
	f.Add(volumetest.MBR(128, []volumetest.Part{
		{StartLBA: 8, Sectors: 16, MBRType: 0x83},
		{StartLBA: 40, Sectors: 10, MBRType: 0x83, Logical: true},
		{StartLBA: 60, Sectors: 10, MBRType: 0x07, Logical: true},
	}))
	f.Add(volumetest.GPT(512, 128, diskGUID, []volumetest.Part{
		{StartLBA: 40, Sectors: 20, TypeGUID: guidLinux, GUID: guidA, Name: "a"},
	}))
	f.Add(volumetest.GPT(4096, 16, diskGUID, []volumetest.Part{
		{StartLBA: 6, Sectors: 4, TypeGUID: guidEFI, GUID: guidB, Name: "b"},
	}))
	f.Add(make([]byte, 1024))
	// Seed with the first and last 64 KiB of each fixture, not the whole
	// multi-MiB images: the partition tables sit at those ends and the fuzzer
	// stays fast. TestVolumeFixturesMatchOracle keeps checking the full images.
	const seedEdge = 64 << 10
	for _, path := range fixtureImages(f) {
		img := gunzipFixture(f, path)
		f.Add(img[:min(len(img), seedEdge)])
		if len(img) > seedEdge {
			f.Add(img[len(img)-min(len(img), seedEdge):])
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		size := int64(len(b))
		tab, err := volume.Read(bytes.NewReader(b), size, 0)
		if err != nil {
			return
		}
		for _, p := range tab.Partitions {
			if p.Start < 0 || p.Length < 0 || p.Start+p.Length > size {
				t.Fatalf("partition out of image: %+v (size %d)", p, size)
			}
		}
		var prevEnd int64
		for _, u := range tab.Unallocated {
			if u.Length <= 0 || u.Offset < prevEnd || u.Offset+u.Length > size {
				t.Fatalf("bad unallocated run %+v after %d (size %d)", u, prevEnd, size)
			}
			prevEnd = u.Offset + u.Length
		}
	})
}
